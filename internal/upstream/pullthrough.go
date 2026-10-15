package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/jamesagarside/hangar/internal/httpx"
	"github.com/jamesagarside/hangar/internal/jobs"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/repo"
)

// ErrPullPending means a Pull-through has not finished planning in time;
// the Hub API answers 503 with Retry-After (hfapi.ErrPullPending).
var ErrPullPending = errors.New("the pull has not finished planning yet")

// PullThroughPath is where the signed git-lfs Pull-through URLs are
// served, outside the native API router and authentication.
const PullThroughPath = "/api/hangar/v1/pull-through/"

// pullThroughURLTTL bounds the life of a signed Pull-through URL.
const pullThroughURLTTL = time.Hour

// PullThrough starts Pulls on demand (PULL-10). It implements
// hfapi.PullThrough (Repo, File) and gitserver.PullThrough (DownloadURL).
type PullThrough struct{ s *Service }

// PullThrough returns the Pull-through entry points.
func (s *Service) PullThrough() *PullThrough { return &PullThrough{s: s} }

// allowed reports whether Pull-through applies to the upstream Repo for p.
func (pt *PullThrough) allowed(ctx context.Context, p model.Principal, upID model.RepoID) (bool, error) {
	st, err := pt.s.opts.Settings(ctx)
	if err != nil {
		return false, err
	}
	if !st.PullThroughEnabled {
		return false, nil
	}
	if p.IsAnonymous() && st.PullThroughMinRole != "anonymous" {
		return false, nil
	}
	for _, g := range st.PullThroughAllow {
		if MatchGlob(g, upID.String()) || MatchGlob(g, upID.Path()) {
			return true, nil
		}
	}
	return false, nil
}

// Repo starts or joins a history-only Pull of a Repo that is not in the
// Arsenal and waits for planning (at most PullThroughWait). It returns
// repo.ErrNotFound when Pull-through does not apply and ErrPullPending
// when planning takes longer.
func (pt *PullThrough) Repo(ctx context.Context, p model.Principal, id model.RepoID) (*repo.Repo, error) {
	s := pt.s
	ups, err := s.opts.Registry.List(ctx, true)
	if err != nil {
		return nil, err
	}
	for i := range ups {
		up := &ups[i]
		upID, ok := up.UpstreamID(id)
		if !ok || upID.Validate() != nil {
			continue
		}
		if ok, err := pt.allowed(ctx, p, upID); err != nil {
			return nil, err
		} else if !ok {
			continue
		}
		pl := pullPayload{UpstreamID: up.ID, Upstream: up.Name, UpstreamRepo: upID.String(), Type: id.Type, Repo: up.LocalID(upID).String(),
			Selection: Selection{None: true}, RequestedBy: p.UserID, PullThrough: true}
		jobID, err := s.enqueuePull(ctx, pl, uuid.Nil, jobs.PriorityHigh)
		if err != nil {
			return nil, err
		}
		r, err := pt.waitPlanned(ctx, jobID, up.LocalID(upID))
		if errors.Is(err, errPullThroughMiss) {
			continue
		}
		return r, err
	}
	return nil, fmt.Errorf("%w: %s", repo.ErrNotFound, id.Path())
}

var errPullThroughMiss = errors.New("not on this upstream")

func (pt *PullThrough) waitPlanned(ctx context.Context, jobID jobs.JobID, local model.RepoID) (*repo.Repo, error) {
	s := pt.s
	ctx, cancel := context.WithTimeout(ctx, s.opts.PullThroughWait)
	defer cancel()
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		j, err := s.opts.Queue.Get(ctx, jobID)
		if err != nil && ctx.Err() == nil {
			return nil, err
		}
		if j != nil {
			var st pullState
			_ = json.Unmarshal(j.Result, &st)
			switch {
			case st.Phase == phaseFailed && st.Reason == reasonUpstreamNotFound:
				return nil, errPullThroughMiss
			case st.Phase == phaseFailed && st.Reason == reasonQuotaExceeded:
				// The Job's error, the message for users, is set once
				// the handler returns.
				if j.Status == jobs.StatusFailed {
					return nil, QuotaExceeded(errors.New(j.Error))
				}
			case st.Phase == phaseFailed:
				return nil, fmt.Errorf("%w: %s cannot be pulled through: %s", repo.ErrNotFound, local.Path(), j.Error)
			case st.Phase != "" || j.Status == jobs.StatusSucceeded:
				return s.opts.Repos.Get(ctx, local)
			}
		}
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return nil, ErrPullPending
			}
			return nil, ctx.Err()
		case <-tick.C:
		}
	}
}

// File streams a file that a Partial mirror does not hold while it
// arrives from the Upstream. Concurrent requests in this process share
// one transfer; when another replica is transferring the object, the
// reader waits for it to finish and then streams it. It returns
// repo.ErrNotPresent when Pull-through does not apply.
func (pt *PullThrough) File(ctx context.Context, p model.Principal, r *repo.Repo, commit model.CommitSHA, e model.Entry) (io.ReadCloser, error) {
	if e.LFS == nil {
		return nil, repo.ErrNotPresent
	}
	m, up, err := pt.s.Mirror(ctx, r)
	if errors.Is(err, ErrNotMirror) || (err == nil && (m.DetachedAt != nil || !up.Enabled)) {
		return nil, repo.ErrNotPresent
	}
	if err != nil {
		return nil, err
	}
	if ok, err := pt.allowed(ctx, p, m.UpstreamRepo); err != nil || !ok {
		if err != nil {
			return nil, err
		}
		return nil, repo.ErrNotPresent
	}
	return pt.stream(ctx, r, m, up, commit, e.Path, e.LFS.OID, e.LFS.Size, p.UserID)
}

// PullFiles starts, or joins, a Pull of paths at commit into a Partial
// mirror, for clients that retry until the files arrive (the Ollama
// registry). The caller is the request's Principal. It returns
// repo.ErrNotPresent when Pull-through does not apply: the Repo is not an
// attached Mirror of an enabled Upstream, Pull-through is off, or the
// allowlist or minimum role leaves the caller out.
func (pt *PullThrough) PullFiles(ctx context.Context, r *repo.Repo, commit model.CommitSHA, paths []string) error {
	p := httpx.Principal(ctx)
	m, up, err := pt.s.Mirror(ctx, r)
	if errors.Is(err, ErrNotMirror) || (err == nil && (m.DetachedAt != nil || !up.Enabled)) {
		return repo.ErrNotPresent
	}
	if err != nil {
		return err
	}
	if ok, err := pt.allowed(ctx, p, m.UpstreamRepo); err != nil {
		return err
	} else if !ok {
		return repo.ErrNotPresent
	}
	pl := pullPayload{UpstreamID: up.ID, Upstream: up.Name, UpstreamRepo: m.UpstreamRepo.String(), Type: r.ID.Type, Repo: r.ID.String(),
		Revision: string(commit), Selection: Selection{Paths: slices.Clone(paths)}, RequestedBy: p.UserID, PullThrough: true}
	_, err = pt.s.enqueuePull(ctx, pl, uuid.Nil, jobs.PriorityHigh)
	return err
}

func (pt *PullThrough) stream(ctx context.Context, r *repo.Repo, m *Mirror, up *Upstream, commit model.CommitSHA, path, oid string, size int64, user uuid.UUID) (io.ReadCloser, error) {
	s := pt.s
	if done, err := s.markIfStored(ctx, r.ID, r.NamespaceID, oid, size); err != nil {
		return nil, err
	} else if done {
		rc, _, err := s.opts.LFS.Open(ctx, oid, nil)
		return rc, err
	}
	if t := s.hub.lookup(oid); t != nil {
		if t.spooled() {
			return t.reader(ctx)
		}
		return pt.afterTransfer(ctx, r, oid, size, t), nil
	}
	// A new transfer stores the object for the Mirror's Namespace.
	if err := s.checkQuota(ctx, r.NamespaceID, map[string]int64{oid: size}, 0); err != nil {
		return nil, err
	}
	fp := filePayload{UpstreamID: up.ID, Upstream: up.Name, UpstreamRepo: m.UpstreamRepo.String(), Type: r.ID.Type, Repo: r.ID.String(),
		RepoUUID: r.UUID, Commit: commit, Path: path, OID: oid, Size: size, RequestedBy: user, Now: true}
	jobID, err := s.opts.Queue.Enqueue(ctx, jobs.NewJob{Kind: jobs.KindPullFile, DedupeKey: oid, Payload: fp, Priority: jobs.PriorityHigh, Repo: r.UUID,
		RunAfter: s.opts.Now().Add(time.Minute), CreatedBy: user})
	if err != nil {
		return nil, err
	}
	if j, err := s.opts.Queue.Get(ctx, jobID); err == nil && j.Status == jobs.StatusRunning && s.hub.lookup(oid) == nil {
		// Another replica is transferring it.
		return pt.afterTransfer(ctx, r, oid, size, nil), nil
	}
	t, started, err := s.hub.begin(oid, size, true)
	if err != nil {
		return nil, err
	}
	if !started {
		if t.spooled() {
			return t.reader(ctx)
		}
		return pt.afterTransfer(ctx, r, oid, size, t), nil
	}
	rd, err := t.reader(ctx)
	if err != nil {
		t.finish(err)
		return nil, err
	}
	cred, err := s.credentials(ctx, up, user)
	if err != nil {
		_ = rd.Close()
		t.finish(err)
		return nil, err
	}
	src := fileSource{up: up, cred: cred, id: m.UpstreamRepo, commit: commit, path: path, oid: oid, size: size}
	go pt.transfer(context.WithoutCancel(ctx), r, src, t)
	return rd, nil
}

// transfer runs one spooled Pull-through transfer.
func (pt *PullThrough) transfer(ctx context.Context, r *repo.Repo, src fileSource, t *transfer) {
	s := pt.s
	err := func() error {
		rc, err := s.open(ctx, src, 0)
		if err != nil {
			return err
		}
		defer func() { _ = rc.Close() }()
		body := io.TeeReader(s.meter(ctx, rc, src.up.Name, nil), t)
		if _, err := s.opts.LFS.Ingest(ctx, src.oid, src.size, body); err != nil {
			return err
		}
		return s.opts.Repos.SetLFSPresence(ctx, r.ID, []repo.LFSRef{{OID: src.oid, Size: src.size, Present: true}})
	}()
	if err != nil {
		s.log.WarnContext(ctx, "pull-through transfer failed", "repo", r.ID.Path(), "path", src.path, "error", err.Error())
		s.countFile(ctx, src.up.Name, "failed")
	} else {
		s.countFile(ctx, src.up.Name, "ok")
		_, _ = s.mirrors.update(ctx, r.UUID, r.ID.Type, func(m *Mirror) error {
			if !m.Selection.All() && !slices.Contains(m.Selection.Paths, src.path) {
				m.Selection = m.Selection.Union(Selection{Paths: []string{src.path}})
			}
			return nil
		})
	}
	t.finish(err)
}

// afterTransfer returns a reader that waits for the object to be stored
// (by a transfer in this process, or by polling for another replica's)
// and then streams it from the Blob store.
func (pt *PullThrough) afterTransfer(ctx context.Context, r *repo.Repo, oid string, size int64, t *transfer) io.ReadCloser {
	return &lazyReader{open: func() (io.ReadCloser, error) {
		if t != nil {
			if err := t.wait(ctx); err != nil {
				return nil, err
			}
		}
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			done, err := pt.s.markIfStored(ctx, r.ID, r.NamespaceID, oid, size)
			if err != nil {
				return nil, err
			}
			if done {
				rc, _, err := pt.s.opts.LFS.Open(ctx, oid, nil)
				return rc, err
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-tick.C:
			}
		}
	}}
}

type lazyReader struct {
	open func() (io.ReadCloser, error)
	rc   io.ReadCloser
	err  error
}

func (l *lazyReader) Read(p []byte) (int, error) {
	if l.rc == nil && l.err == nil {
		l.rc, l.err = l.open()
	}
	if l.err != nil {
		return 0, l.err
	}
	return l.rc.Read(p)
}

func (l *lazyReader) Close() error {
	if l.rc != nil {
		return l.rc.Close()
	}
	return nil
}

// DownloadURL returns a signed Hangar URL that starts the transfer of an
// LFS object a Partial mirror references and streams it as it arrives,
// for git-lfs batch downloads (04-git.md "Batch").
func (pt *PullThrough) DownloadURL(ctx context.Context, p model.Principal, r *repo.Repo, oid string, size int64) (string, bool, error) {
	s := pt.s
	if s.opts.Signer == nil || s.opts.PublicURL == "" || !model.IsSHA256(oid) {
		return "", false, nil
	}
	m, up, err := s.Mirror(ctx, r)
	if errors.Is(err, ErrNotMirror) || (err == nil && (m.DetachedAt != nil || !up.Enabled)) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if ok, err := pt.allowed(ctx, p, m.UpstreamRepo); err != nil || !ok {
		return "", false, err
	}
	raw := strings.TrimRight(s.opts.PublicURL, "/") + PullThroughPath + r.UUID.String() + "/" + oid + "?size=" + strconv.FormatInt(size, 10)
	signed, err := s.opts.Signer.Sign(raw, pullThroughScope(r.UUID, oid), pullThroughURLTTL)
	if err != nil {
		return "", false, err
	}
	return signed, true, nil
}

func pullThroughScope(repoUUID uuid.UUID, oid string) string {
	return "pull-through:" + repoUUID.String() + ":" + oid
}

// Handler serves the signed URLs DownloadURL returns. Mount it at
// PullThroughPath, outside authentication: the URL authorises itself.
func (pt *PullThrough) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			httpx.WriteError(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "Use GET.")
			return
		}
		rest, ok := strings.CutPrefix(r.URL.Path, PullThroughPath)
		repoPart, oid, ok2 := strings.Cut(rest, "/")
		repoUUID, err := uuid.Parse(repoPart)
		if !ok || !ok2 || err != nil || !model.IsSHA256(oid) {
			httpx.WriteError(w, r, http.StatusNotFound, "not_found", "Not found.")
			return
		}
		if pt.s.opts.Signer == nil {
			httpx.WriteError(w, r, http.StatusNotFound, "not_found", "Not found.")
			return
		}
		scope, err := pt.s.opts.Signer.Verify(r.URL)
		if err != nil || scope != pullThroughScope(repoUUID, oid) {
			httpx.WriteError(w, r, http.StatusForbidden, "forbidden", "The link is invalid or has expired. Ask git-lfs for a new one.")
			return
		}
		size, err := strconv.ParseInt(r.URL.Query().Get("size"), 10, 64)
		if err != nil || size < 0 {
			httpx.WriteError(w, r, http.StatusBadRequest, "bad_request", "The size is missing.")
			return
		}
		ctx := r.Context()
		rp, err := pt.s.opts.Repos.GetByUUID(ctx, repoUUID)
		if err != nil || rp.DeletedAt != nil {
			httpx.WriteError(w, r, http.StatusNotFound, "not_found", "Not found.")
			return
		}
		m, up, err := pt.s.Mirror(ctx, rp)
		if err != nil || m.DetachedAt != nil || !up.Enabled {
			httpx.WriteError(w, r, http.StatusNotFound, "not_found", "The Repo is no longer a Mirror.")
			return
		}
		commit, path, err := pt.findObject(ctx, rp, oid)
		if err != nil {
			httpx.WriteError(w, r, http.StatusNotFound, "not_found", "No branch of the Repo references this object.")
			return
		}
		// A link issued before an admin turned on blocking is checked
		// again, as git-lfs signed links are.
		blocked, err := pt.s.opts.Downloads.CheckObjects(ctx, rp.UUID, rp.ID, []string{oid})
		if err != nil {
			pt.s.log.WarnContext(ctx, "pull-through download check", "repo", rp.ID.Path(), "error", err.Error())
			httpx.WriteError(w, r, http.StatusInternalServerError, "internal", "The download could not be checked. Try again.")
			return
		}
		if err := blocked[oid]; err != nil {
			httpx.WriteError(w, r, http.StatusForbidden, "forbidden", err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		rc, err := pt.stream(ctx, rp, m, up, commit, path, oid, size, uuid.Nil)
		if errors.Is(err, ErrQuotaExceeded) {
			w.Header().Del("Content-Length")
			httpx.WriteError(w, r, http.StatusForbidden, reasonQuotaExceeded, err.Error())
			return
		}
		if err != nil {
			pt.s.log.WarnContext(ctx, "pull-through download", "repo", rp.ID.Path(), "error", err.Error())
			w.Header().Del("Content-Length")
			httpx.WriteError(w, r, http.StatusBadGateway, "upstream_error", "The Upstream could not provide the file.")
			return
		}
		defer func() { _ = rc.Close() }()
		w.WriteHeader(http.StatusOK)
		if _, err := io.Copy(w, io.LimitReader(rc, size)); err != nil && !errors.Is(err, context.Canceled) {
			pt.s.log.WarnContext(ctx, "pull-through download interrupted", "repo", rp.ID.Path(), "error", err.Error())
		}
	})
}

// findObject returns a Commit and path that reference oid, searching the
// tip of each branch, default branch first.
func (pt *PullThrough) findObject(ctx context.Context, r *repo.Repo, oid string) (model.CommitSHA, string, error) {
	refs, err := pt.s.opts.Repos.Refs(ctx, r.ID)
	if err != nil {
		return "", "", err
	}
	branches := slices.Clone(refs.Branches)
	slices.SortStableFunc(branches, func(a, b repo.Ref) int {
		switch {
		case a.Name == model.BranchRef(r.DefaultBranch):
			return -1
		case b.Name == model.BranchRef(r.DefaultBranch):
			return 1
		}
		return 0
	})
	for _, b := range branches {
		entries, err := pt.s.opts.Repos.Tree(ctx, r.ID, b.Target, "", repo.TreeOpts{Recursive: true})
		if err != nil {
			return "", "", err
		}
		for _, e := range entries {
			if e.LFS != nil && e.LFS.OID == oid {
				return b.Target, e.Path, nil
			}
		}
	}
	return "", "", repo.ErrEntryNotFound
}
