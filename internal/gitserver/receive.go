package gitserver

import (
	"context"
	"crypto/sha1" //nolint:gosec // git pack indexes are SHA-1 by definition.
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-git/go-billy/v6"
	"github.com/go-git/go-billy/v6/osfs"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/cache"
	"github.com/go-git/go-git/v6/plumbing/format/idxfile"
	"github.com/go-git/go-git/v6/plumbing/protocol/packp"
	"github.com/go-git/go-git/v6/plumbing/storer"
	"github.com/go-git/go-git/v6/plumbing/transport"
	"github.com/go-git/go-git/v6/storage/filesystem"
	"github.com/go-git/go-git/v6/storage/memory"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"

	"github.com/jamesagarside/hangar/internal/auth"
	"github.com/jamesagarside/hangar/internal/httpx"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/repo"
	"github.com/jamesagarside/hangar/internal/telemetry"
)

// Push outcomes, the values of the `outcome` attribute of hangar.git.push.
const (
	outcomeOK       = "ok"
	outcomeRejected = "rejected"
	outcomeConflict = "conflict"
	outcomeError    = "error"
)

// advertiseReceivePack answers `info/refs?service=git-receive-pack` with
// the Repo's branches and tags.
func (s *Server) advertiseReceivePack(w http.ResponseWriter, r *http.Request, rec *repo.Repo) {
	ctx, span := s.deps.Tracer.Start(r.Context(), "git.advertise_refs")
	span.SetAttributes(attribute.String("git.service", serviceReceivePack))
	defer span.End()
	refs, err := s.snapshot(ctx, rec)
	if err != nil {
		span.SetStatus(codes.Error, "read refs")
		s.gitFailure(w, r, err)
		return
	}
	st := memory.NewStorage()
	st.ReferenceStorage = refs.pushRefs()
	h := w.Header()
	noCache(h)
	h.Set("Content-Type", "application/x-git-receive-pack-advertisement")
	out := &lazyHeaderWriter{w: w}
	err = transport.ReceivePack(ctx, st, nil, nopWriteCloser{out}, &transport.ReceivePackRequest{
		GitProtocol:   gitProtocol(r),
		AdvertiseRefs: true,
		StatelessRPC:  true,
	})
	if err != nil && !out.started {
		s.gitFailure(w, r, fmt.Errorf("advertise refs: %w", err))
	}
}

// snapshot reads a Repo's refs through a view.
func (s *Server) snapshot(ctx context.Context, rec *repo.Repo) (*viewRefs, error) {
	view, err := s.deps.Repos.OpenView(ctx, rec.ID)
	if err != nil {
		return nil, fmt.Errorf("open view: %w", err)
	}
	defer closeView(s, ctx, view)
	return readViewRefs(view.Dir)
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

// errPackTooLarge is returned by reads past git.max_pack_size.
type errPackTooLarge struct{ limit int64 }

func (e errPackTooLarge) Error() string {
	return "the push is larger than the " + formatSize(e.limit) + " limit (git.max_pack_size). Push fewer commits at a time, or move large files to Git LFS"
}

// limitReader fails once more than max bytes have been read.
type limitReader struct {
	r   io.Reader
	n   int64
	max int64
}

func (l *limitReader) Read(p []byte) (int, error) {
	if l.max > 0 && l.n > l.max {
		return 0, errPackTooLarge{l.max}
	}
	n, err := l.r.Read(p)
	l.n += int64(n)
	if l.max > 0 && l.n > l.max {
		return n, errPackTooLarge{l.max}
	}
	return n, err
}

// receivePack runs a push: go-git's receive-pack unpacks into a quarantine
// and the pre-receive hook applies the result through repo.Service.ApplyPack.
func (s *Server) receivePack(w http.ResponseWriter, r *http.Request, rp repoPath) {
	rec, err := s.lookup(r.Context(), rp.ID, auth.ActionWrite)
	if err != nil {
		s.gitFailure(w, r, err)
		return
	}
	if !checkRequestType(r, serviceReceivePack) {
		s.gitError(w, r, http.StatusUnsupportedMediaType, "Expected Content-Type application/x-git-receive-pack-request.")
		return
	}
	if s.cfg.MaxPackSize > 0 && r.ContentLength > s.cfg.MaxPackSize {
		s.countPush(r.Context(), outcomeRejected)
		s.gitError(w, r, http.StatusRequestEntityTooLarge, "Push refused: "+errPackTooLarge{s.cfg.MaxPackSize}.Error()+".")
		return
	}
	body, err := requestBody(r)
	if err != nil {
		s.gitError(w, r, http.StatusBadRequest, "The request body could not be decoded.")
		return
	}
	defer body.Close()

	ctx, span := s.deps.Tracer.Start(r.Context(), "git.receive_pack")
	defer span.End()
	start := time.Now()
	defer s.recordPackDuration(ctx, "receive-pack", start)

	view, err := s.deps.Repos.OpenView(ctx, rec.ID)
	if err != nil {
		span.SetStatus(codes.Error, "open view")
		s.gitFailure(w, r, fmt.Errorf("open view: %w", err))
		return
	}
	defer closeView(s, ctx, view)
	refs, err := readViewRefs(view.Dir)
	if err != nil {
		s.gitFailure(w, r, err)
		return
	}
	q, err := s.newQuarantine(view.Dir)
	if err != nil {
		s.gitFailure(w, r, err)
		return
	}
	defer q.remove(s, ctx)

	st := &pushStorer{Storage: q.storage(), memoryRefs: &memoryRefs{refs: refs.pushRefs()}}
	defer func() { _ = st.Close() }()
	hook := &pushHook{s: s, rec: rec, q: q, st: st, principal: httpx.Principal(ctx), outcome: outcomeOK}

	h := w.Header()
	noCache(h)
	h.Set("Content-Type", "application/x-git-receive-pack-result")
	out := &lazyHeaderWriter{w: w}
	err = transport.ReceivePack(ctx, st, io.NopCloser(&limitReader{r: body, max: s.cfg.MaxPackSize}), nopWriteCloser{out}, &transport.ReceivePackRequest{
		GitProtocol:  gitProtocol(r),
		StatelessRPC: true,
		Hooks:        transport.ReceivePackHooks{PreReceive: hook.preReceive},
	})
	outcome := hook.outcome
	if err != nil && outcome == outcomeOK {
		outcome = outcomeRejected
		var tooLarge errPackTooLarge
		if !errors.As(err, &tooLarge) && !hook.ran {
			outcome = outcomeError
		}
	}
	s.countPush(ctx, outcome)
	span.SetAttributes(attribute.String("git.push.outcome", outcome))
	if err != nil {
		if outcome == outcomeError {
			span.SetStatus(codes.Error, "receive-pack")
			s.log.WarnContext(ctx, "git receive-pack failed", "error", err)
		}
		if !out.started {
			s.gitError(w, r, http.StatusBadRequest, "The push request could not be read.")
		}
	}
}

func (s *Server) countPush(ctx context.Context, outcome string) {
	if s.deps.Instruments == nil {
		return
	}
	s.deps.Instruments.GitPush.Add(ctx, 1, metric.WithAttributes(attribute.String(telemetry.MetricOutcome, outcome)))
}

// quarantine is a private bare repository that receives a push's objects.
// Its alternates point at the view, so existing objects resolve, and
// nothing in the view changes.
type quarantine struct {
	dir string
}

// newQuarantine creates a quarantine whose alternates list the view's
// object directory and, flattened, every alternate the view itself has.
func (s *Server) newQuarantine(viewDir string) (*quarantine, error) {
	dir, err := s.tempDir("hangar-push-")
	if err != nil {
		return nil, fmt.Errorf("create push quarantine: %w", err)
	}
	q := &quarantine{dir: dir}
	for _, d := range []string{"objects/pack", "objects/info", "refs"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o700); err != nil {
			_ = os.RemoveAll(dir)
			return nil, fmt.Errorf("create push quarantine: %w", err)
		}
	}
	viewObjects := absPath(filepath.Join(viewDir, "objects"))
	alternates := []string{viewObjects}
	if b, err := os.ReadFile(filepath.Join(viewObjects, "info", "alternates")); err == nil { //nolint:gosec // the view directory comes from repo.Service, not the client.
		for _, line := range strings.Split(string(b), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			if !filepath.IsAbs(line) {
				line = filepath.Join(viewObjects, line)
			}
			alternates = append(alternates, absPath(line))
		}
	}
	//nolint:gosec // dir is a private temporary directory Hangar just created.
	if err := os.WriteFile(filepath.Join(dir, "objects", "info", "alternates"), []byte(strings.Join(alternates, "\n")+"\n"), 0o600); err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("create push quarantine: %w", err)
	}
	return q, nil
}

func (q *quarantine) storage() *filesystem.Storage {
	return filesystem.NewStorageWithOptions(osfs.New(q.dir), cache.NewObjectLRUDefault(), filesystem.Options{
		ExclusiveAccess: true,
		AlternatesFS:    absoluteFS{osfs.New(string(filepath.Separator))},
	})
}

// absoluteFS resolves the absolute paths in a quarantine's alternates file.
// osfs refuses absolute paths under its root, and go-git passes them through
// unchanged when the root is "/", so Stat and Chroot use the OS directly.
type absoluteFS struct{ billy.Filesystem }

func (absoluteFS) Root() string { return "" }

func (absoluteFS) Stat(path string) (os.FileInfo, error) { return os.Stat(path) }

func (absoluteFS) Chroot(path string) (billy.Filesystem, error) { return osfs.New(path), nil }

func (q *quarantine) remove(s *Server, ctx context.Context) {
	if err := os.RemoveAll(q.dir); err != nil {
		s.log.WarnContext(ctx, "remove push quarantine", "error", err)
	}
}

// pack returns the path of the pack go-git wrote, or "" when the push sent
// no objects.
func (q *quarantine) pack() (string, error) {
	matches, err := filepath.Glob(filepath.Join(q.dir, "objects", "pack", "pack-*.pack"))
	if err != nil {
		return "", err
	}
	switch len(matches) {
	case 0:
		return "", nil
	case 1:
		return matches[0], nil
	default:
		return "", fmt.Errorf("push quarantine holds %d packs, expected one", len(matches))
	}
}

// packObjects lists the objects in a pack from its index.
func packObjects(packPath string) ([]plumbing.Hash, error) {
	f, err := os.Open(strings.TrimSuffix(packPath, ".pack") + ".idx")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	idx := idxfile.NewMemoryIndex(sha1.Size)
	if err := idxfile.NewDecoder(f, sha1.New()).Decode(idx); err != nil { //nolint:gosec // git pack indexes are SHA-1.
		return nil, fmt.Errorf("read pack index: %w", err)
	}
	it, err := idx.Entries()
	if err != nil {
		return nil, err
	}
	defer it.Close()
	var out []plumbing.Hash
	for {
		e, err := it.Next()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		out = append(out, e.Hash)
	}
}

// pushStorer is the storer receive-pack works on: objects in the
// quarantine (and, through alternates, the view) and refs in memory.
type pushStorer struct {
	*filesystem.Storage
	*memoryRefs
}

// HasEncodedObject works around go-git v6.0.0-beta.1, whose filesystem
// storer reports a missing loose object as ErrObjectNotFound before it
// looks in alternates, so objects that exist only in the view are reported
// missing. EncodedObject does search alternates.
func (p *pushStorer) HasEncodedObject(h plumbing.Hash) error {
	err := p.Storage.HasEncodedObject(h)
	if errors.Is(err, plumbing.ErrObjectNotFound) {
		if _, err := p.EncodedObject(plumbing.AnyObject, h); err == nil {
			return nil
		}
	}
	return err
}

// Ref methods come from memoryRefs, never from the quarantine on disk.

func (p *pushStorer) SetReference(r *plumbing.Reference) error { return p.memoryRefs.SetReference(r) }

func (p *pushStorer) CheckAndSetReference(newRef, old *plumbing.Reference) error {
	return p.memoryRefs.CheckAndSetReference(newRef, old)
}

func (p *pushStorer) Reference(n plumbing.ReferenceName) (*plumbing.Reference, error) {
	return p.memoryRefs.Reference(n)
}

func (p *pushStorer) IterReferences() (storer.ReferenceIter, error) {
	return p.memoryRefs.IterReferences()
}

func (p *pushStorer) RemoveReference(n plumbing.ReferenceName) error {
	return p.memoryRefs.RemoveReference(n)
}

func (p *pushStorer) CountLooseRefs() (int, error) { return p.memoryRefs.CountLooseRefs() }

func (p *pushStorer) PackRefs() error { return p.memoryRefs.PackRefs() }

// pushHook is the pre-receive hook of one push.
type pushHook struct {
	s         *Server
	rec       *repo.Repo
	q         *quarantine
	st        *pushStorer
	principal model.Principal
	outcome   string
	ran       bool
}

// refusal is a push refusal shown to the client as the reason of every ref.
type refusal struct {
	outcome string
	message string
}

func (e *refusal) Error() string { return e.message }

func reject(format string, args ...any) error {
	return &refusal{outcome: outcomeRejected, message: fmt.Sprintf(format, args...)}
}

// oneLine makes a message safe for a report-status line.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// preReceive checks the push policy (docs/spec/04-git.md "Push") and then
// applies the push through repo.Service.ApplyPack. An error refuses every
// ref with its message.
func (h *pushHook) preReceive(ctx context.Context, info *transport.PreReceiveInfo) error {
	h.ran = true
	ctx, span := h.s.deps.Tracer.Start(ctx, "git.pre_receive")
	defer span.End()
	err := h.check(ctx, info.Commands)
	if err != nil {
		var rf *refusal
		if errors.As(err, &rf) {
			h.outcome = rf.outcome
			return errors.New(oneLine(rf.message))
		}
		h.outcome = outcomeError
		span.SetStatus(codes.Error, "pre-receive")
		h.s.log.ErrorContext(ctx, "git push failed", "error", err)
		return errors.New("internal server error, the request ID is " + httpx.RequestID(ctx))
	}
	return nil
}

func (h *pushHook) check(ctx context.Context, cmds []*packp.Command) error {
	if h.rec.Mirror {
		return reject("%s is a Mirror and is read-only. Detach it from its Upstream to push to it", h.rec.ID.Path())
	}
	updates := make([]repo.RefUpdate, 0, len(cmds))
	names := make([]plumbing.ReferenceName, 0, len(cmds))
	olds := make([]plumbing.Hash, 0, len(cmds))
	defaultRef := model.BranchRef(h.rec.DefaultBranch)
	for _, c := range cmds {
		name := c.Name.String()
		if err := validatePushRef(c.Name); err != nil {
			return err
		}
		if c.Action() == packp.Delete && name == defaultRef {
			return reject("%s is the default branch and cannot be deleted. Change the default branch in the Repo settings first", name)
		}
		if c.Action() != packp.Delete {
			if err := h.st.HasEncodedObject(c.New); err != nil {
				return reject("missing necessary objects for %s", name)
			}
		}
		updates = append(updates, repo.RefUpdate{Name: name, Old: sha(c.Old), New: sha(c.New)})
		names = append(names, c.Name)
		olds = append(olds, c.Old)
	}

	packPath, err := h.q.pack()
	if err != nil {
		return err
	}
	var packSize int64
	if packPath != "" {
		fi, err := os.Stat(packPath)
		if err != nil {
			return err
		}
		packSize = fi.Size()
		objs, err := packObjects(packPath)
		if err != nil {
			return err
		}
		if len(objs) == 0 {
			packPath = ""
		} else if err := h.checkObjects(ctx, objs, cmds); err != nil {
			return err
		}
	}
	if h.s.deps.Quota != nil && packSize > 0 {
		if err := h.s.deps.Quota.CheckQuota(ctx, h.rec.NamespaceID, packSize); err != nil {
			return reject("%s", err.Error())
		}
	}

	params := repo.ApplyPackParams{
		Repo:       h.rec.ID,
		Source:     repo.SourcePush,
		Actor:      h.principal,
		Updates:    updates,
		AllowForce: h.s.cfg.AllowForcePush || h.s.canForcePush(ctx, h.rec),
	}
	if packPath != "" {
		f, err := os.Open(packPath) //nolint:gosec // a pack go-git wrote into the push quarantine.
		if err != nil {
			return err
		}
		defer f.Close()
		params.Pack = f
	}
	ctx, span := h.s.deps.Tracer.Start(ctx, "git.update_refs")
	err = h.s.deps.Repos.ApplyPack(ctx, params)
	span.End()
	if err != nil {
		return applyError(err)
	}
	h.st.reset(names, olds)
	return nil
}

func sha(h plumbing.Hash) model.CommitSHA {
	if h.IsZero() {
		return model.ZeroSHA
	}
	return model.CommitSHA(h.String())
}

// validatePushRef applies Hangar's ref rules and git's ref name format.
func validatePushRef(n plumbing.ReferenceName) error {
	name := n.String()
	if strings.HasPrefix(name, model.RefPR) {
		return reject("%s: pull request refs can only be written by opening or updating a pull request", name)
	}
	if !pushable(name) {
		return reject("%s: only branches (refs/heads/*) and tags (refs/tags/*) can be pushed", name)
	}
	if err := model.ValidateRefName(name); err != nil || n.Validate() != nil || !n.IsSafe() {
		return reject("%s: invalid ref name", name)
	}
	return nil
}

// applyError maps an ApplyPack error to a refusal.
func applyError(err error) error {
	var pe *repo.PolicyError
	switch {
	case errors.As(err, &pe):
		return &refusal{outcome: outcomeRejected, message: pe.Error()}
	case errors.Is(err, repo.ErrRefConflict):
		return &refusal{outcome: outcomeConflict, message: repo.ErrRefConflict.Error()}
	case errors.Is(err, repo.ErrReadOnly), errors.Is(err, repo.ErrProtectedRef),
		errors.Is(err, repo.ErrLFSObjectMissing), errors.Is(err, repo.ErrInvalid),
		errors.Is(err, repo.ErrRevisionNotFound):
		return &refusal{outcome: outcomeRejected, message: err.Error()}
	}
	return err
}
