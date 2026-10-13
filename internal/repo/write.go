package repo

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"

	"github.com/jamesagarside/hangar/internal/db"
	"github.com/jamesagarside/hangar/internal/gitstore"
	"github.com/jamesagarside/hangar/internal/jobs"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/webhook"
)

// CommitJob is the payload of the index and scan Jobs that run for every
// Commit that becomes the head of a branch (07-indexing-and-scanning.md).
type CommitJob struct {
	RepoUUID uuid.UUID       `json:"repo_uuid"`
	Repo     string          `json:"repo"`
	Type     model.RepoType  `json:"type"`
	Commit   model.CommitSHA `json:"commit"`
}

// RepoJob is the payload of Jobs about a whole Repo: git.compact and repo.purge.
type RepoJob struct {
	RepoUUID uuid.UUID `json:"repo_uuid"`
}

// maxCommitAttempts bounds retries of a Commit without a parent Commit
// when the branch moves underneath it (04-git.md "Server-side Commits").
const maxCommitAttempts = 4

// applyParams describes a set of ref updates and the pack, if any, that
// holds their new objects.
type applyParams struct {
	row     *repoRow
	updates []gitstore.RefUpdate
	st      *gitstore.Staging
	pack    string
	lfs     []LFSRef
	actor   model.Principal
}

// apply stores the pack, then in one transaction moves every ref with
// compare-and-swap, marks the pack live and records LFS references. On a
// conflict the pack is marked orphaned and ErrRefConflict is returned.
func (s *Store) apply(ctx context.Context, p applyParams) error {
	if p.pack != "" {
		if _, err := p.st.StorePack(ctx, p.pack); err != nil {
			return err
		}
	}
	now := s.now()
	err := s.db.InTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		if err := gitstore.UpdateRefs(ctx, tx, p.row.ID, p.updates); err != nil {
			return err
		}
		if p.pack != "" {
			if err := gitstore.MarkLive(ctx, tx, p.row.ID, p.pack); err != nil {
				return err
			}
		}
		if err := upsertLFS(ctx, tx, p.row.ID, p.lfs, false); err != nil {
			return err
		}
		return s.touchRepo(ctx, tx, p.row.ID, now)
	})
	if err != nil {
		if p.pack != "" {
			if merr := s.git.MarkOrphaned(context.WithoutCancel(ctx), p.row.ID, p.pack); merr != nil {
				s.log.WarnContext(ctx, "mark pack orphaned", "error", merr.Error())
			}
		}
		if errors.Is(err, gitstore.ErrRefConflict) {
			return fmt.Errorf("%w: %w", ErrRefConflict, err)
		}
		if errors.Is(err, gitstore.ErrInvalidUpdate) {
			return fmt.Errorf("%w: %w", ErrInvalid, err)
		}
		return err
	}
	s.afterRefMove(ctx, p.row, p.updates, p.actor)
	return nil
}

// upsertLFS records LFS references. With overwrite, presence is set as
// given; otherwise an object already marked present stays present.
func upsertLFS(ctx context.Context, tx bun.IDB, repoID uuid.UUID, refs []LFSRef, overwrite bool) error {
	if len(refs) == 0 {
		return nil
	}
	byOID := map[string]LFSRef{}
	for _, r := range refs {
		if prev, ok := byOID[r.OID]; ok && prev.Present {
			r.Present = true
		}
		byOID[r.OID] = r
	}
	now := db.Now()
	rows := make([]db.RepoLFS, 0, len(byOID))
	oids := make([]string, 0, len(byOID))
	for _, r := range byOID {
		rows = append(rows, db.RepoLFS{RepoID: repoID, OID: r.OID, Size: r.Size, Present: r.Present, Timestamps: db.Timestamps{CreatedAt: now, UpdatedAt: now}})
		oids = append(oids, r.OID)
	}
	presentExpr := "present = repo_lfs.present OR EXCLUDED.present"
	if overwrite {
		presentExpr = "present = EXCLUDED.present"
	}
	for start := 0; start < len(rows); start += 500 {
		chunk := rows[start:min(start+500, len(rows))]
		if _, err := tx.NewInsert().Model(&chunk).On("CONFLICT (repo_id, oid) DO UPDATE").Set(presentExpr).
			Set("size = CASE WHEN EXCLUDED.size > 0 THEN EXCLUDED.size ELSE repo_lfs.size END").Set("updated_at = EXCLUDED.updated_at").Exec(ctx); err != nil {
			return fmt.Errorf("repo: record lfs references: %w", err)
		}
	}
	return Touch(ctx, tx, oids, now)
}

// touchRepo updates last_modified_at and the size columns.
func (s *Store) touchRepo(ctx context.Context, tx bun.IDB, repoID uuid.UUID, now time.Time) error {
	_, err := tx.NewUpdate().Model((*db.Repo)(nil)).
		Set("last_modified_at = ?", now).Set("updated_at = ?", now).
		Set("lfs_bytes = (SELECT COALESCE(SUM(size), 0) FROM repo_lfs WHERE repo_id = ? AND present = ?)", repoID, true).
		Set("size_bytes = (SELECT COALESCE(SUM(size), 0) FROM packs WHERE repo_id = ? AND state = ?) + (SELECT COALESCE(SUM(size), 0) FROM repo_lfs WHERE repo_id = ? AND present = ?)",
			repoID, gitstore.PackLive, repoID, true).
		Where("id = ?", repoID).Exec(ctx)
	return err
}

// afterRefMove fills the caches, enqueues index, scan and compaction Jobs,
// and emits repo.updated. Failures are logged: readers rebuild caches on
// demand, and the ref move has already happened.
func (s *Store) afterRefMove(ctx context.Context, row *repoRow, updates []gitstore.RefUpdate, actor model.Principal) {
	ctx = context.WithoutCancel(ctx)
	var tips, excludes []string
	changed := make([]map[string]any, 0, len(updates))
	for _, u := range updates {
		changed = append(changed, map[string]any{"ref": u.Name, "old": string(u.Old), "new": string(u.New)})
		if !u.New.IsZero() {
			tips = append(tips, string(u.New))
		}
		if !u.Old.IsZero() {
			excludes = append(excludes, string(u.Old))
		}
	}
	if len(tips) > 0 {
		if objs, err := s.git.Acquire(ctx, row.ID); err == nil {
			if err := s.fillCommits(ctx, row.ID, objs.Dir, tips, excludes); err != nil {
				s.log.WarnContext(ctx, "fill commit cache", "repo.uuid", row.ID.String(), "error", err.Error())
			}
			objs.Release()
		}
	}
	r := toRepo(row)
	for _, u := range updates {
		if u.New.IsZero() || !strings.HasPrefix(u.Name, model.RefHeads) {
			continue
		}
		if err := s.ensureTree(ctx, row.ID, u.New); err != nil {
			s.log.WarnContext(ctx, "fill tree cache", "repo.uuid", row.ID.String(), "error", err.Error())
		}
		payload := CommitJob{RepoUUID: row.ID, Repo: r.ID.String(), Type: r.ID.Type, Commit: u.New}
		key := row.ID.String() + ":" + string(u.New)
		s.enqueue(ctx, jobs.NewJob{Kind: jobs.KindIndex, DedupeKey: key, Payload: payload})
		s.enqueue(ctx, jobs.NewJob{Kind: jobs.KindScan, DedupeKey: key, Payload: payload})
	}
	if n, err := s.db.NewSelect().Model((*db.Pack)(nil)).Where("repo_id = ?", row.ID).Where("state = ?", gitstore.PackLive).Count(ctx); err == nil && n > int64(s.opts.CompactThreshold) {
		s.enqueue(ctx, jobs.NewJob{Kind: jobs.KindGitCompact, DedupeKey: row.ID.String(), Payload: RepoJob{RepoUUID: row.ID}, Priority: jobs.PriorityLow})
	}
	s.emit(ctx, webhook.RepoUpdated, actor, r, map[string]any{"refs": changed})
}

// preparedOp is an operation whose content is already written into the
// staging repository, so it can be replayed on a newer branch tip.
type preparedOp struct {
	kind    OpKind
	path    string
	oid     string // blob for adds
	srcPath string
	srcRev  string
}

// prepare writes the content of every add into the staging repository,
// storing paths that .gitattributes tracks with LFS as LFS objects.
func (s *Store) prepare(ctx context.Context, row *repoRow, st *gitstore.Staging, parent model.CommitSHA, p CommitParams) ([]preparedOp, []LFSRef, int64, error) {
	rules, err := s.lfsRules(ctx, st, row, parent)
	if err != nil {
		return nil, nil, 0, err
	}
	var (
		ops   []preparedOp
		refs  []LFSRef
		added int64
	)
	for _, op := range p.Operations {
		clean, _ := model.CleanRepoPath(op.Path)
		po := preparedOp{kind: op.Kind, path: clean, srcRev: op.SrcRevision}
		switch op.Kind {
		case OpAdd:
			content := op.Content
			if rules.IsLFS(clean) {
				ptr, err := s.ingestContent(ctx, op.Content)
				if err != nil {
					return nil, nil, 0, err
				}
				refs = append(refs, LFSRef{OID: ptr.OID, Size: ptr.Size, Present: true})
				added += ptr.Size
				content = bytes.NewReader(ptr.Bytes())
			}
			counter := &countingReader{r: content}
			oid, err := st.Run(ctx, counter, nil, "hash-object", "-w", "--stdin")
			if err != nil {
				return nil, nil, 0, err
			}
			if !rules.IsLFS(clean) {
				if counter.n > s.opts.MaxBlobSize {
					return nil, nil, 0, fmt.Errorf("%w: %s is %d bytes, larger than the %d byte limit for files outside LFS; track it with git lfs track or upload it as an LFS file", ErrInvalid, clean, counter.n, s.opts.MaxBlobSize)
				}
				added += counter.n
			}
			po.oid = strings.TrimSpace(string(oid))
		case OpAddLFS:
			ref, err := s.lfsOp(ctx, row, op, p.AllowMirror)
			if err != nil {
				return nil, nil, 0, fmt.Errorf("%w (%s)", err, clean)
			}
			refs = append(refs, ref)
			if ref.Present {
				added += ref.Size
			}
			ptr := model.LFSPointer{OID: ref.OID, Size: ref.Size}
			oid, err := st.Run(ctx, bytes.NewReader(ptr.Bytes()), nil, "hash-object", "-w", "--stdin")
			if err != nil {
				return nil, nil, 0, err
			}
			po.oid = strings.TrimSpace(string(oid))
			po.kind = OpAdd
		case OpCopy:
			po.srcPath, _ = model.CleanRepoPath(op.SrcPath)
		}
		ops = append(ops, po)
	}
	return ops, refs, added, nil
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// lfsOp checks the object of an OpAddLFS. An object written through a
// presigned PUT and not yet verified is verified now. Mirrors written by
// Pulls may reference objects that are not present yet.
func (s *Store) lfsOp(ctx context.Context, row *repoRow, op Operation, allowMirror bool) (LFSRef, error) {
	obj, err := s.lfs.Stat(ctx, op.OID)
	if errors.Is(err, ErrNotFound) && op.Size > 0 {
		obj, err = s.lfs.Verify(ctx, op.OID, op.Size)
	}
	switch {
	case err == nil:
		size := op.Size
		if size == 0 {
			size = obj.Size
		}
		if size != obj.Size {
			return LFSRef{}, fmt.Errorf("%w: %s has size %d, not %d", ErrInvalid, op.OID, obj.Size, size)
		}
		return LFSRef{OID: op.OID, Size: size, Present: true}, nil
	case toRepo(row).Mirror && allowMirror && op.Size > 0 && (errors.Is(err, ErrNotFound) || errors.Is(err, ErrChecksumMismatch)):
		return LFSRef{OID: op.OID, Size: op.Size, Present: false}, nil
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrChecksumMismatch):
		return LFSRef{}, fmt.Errorf("%w: %s", ErrLFSObjectMissing, op.OID)
	default:
		return LFSRef{}, err
	}
}

// ingestContent spools content to a temporary file while hashing it, then
// stores it as an LFS object, and returns its pointer.
func (s *Store) ingestContent(ctx context.Context, r io.Reader) (model.LFSPointer, error) {
	f, err := os.CreateTemp("", "hangar-lfs-*")
	if err != nil {
		return model.LFSPointer{}, err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), r)
	if err != nil {
		return model.LFSPointer{}, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return model.LFSPointer{}, err
	}
	ptr := model.LFSPointer{OID: hex.EncodeToString(h.Sum(nil)), Size: n}
	if _, err := s.lfs.Ingest(ctx, ptr.OID, ptr.Size, f); err != nil {
		return model.LFSPointer{}, err
	}
	return ptr, nil
}

// lfsRules reads .gitattributes at the parent Commit, falling back to the
// Hub's defaults for the Repo type.
func (s *Store) lfsRules(ctx context.Context, st *gitstore.Staging, row *repoRow, parent model.CommitSHA) (*LFSRules, error) {
	if parent != "" {
		out, err := st.Run(ctx, nil, nil, "cat-file", "blob", string(parent)+":.gitattributes")
		if err == nil {
			return ParseLFSRules(out), nil
		}
	}
	return ParseLFSRules(DefaultGitAttributes(model.RepoType(row.Type))), nil
}

// buildTree applies prepared operations to parent's tree in a temporary
// index and writes the new tree.
func (s *Store) buildTree(ctx context.Context, row *repoRow, st *gitstore.Staging, parent model.CommitSHA, ops []preparedOp) (string, error) {
	idx := filepath.Join(st.Dir, "index-"+uuid.NewString())
	defer os.Remove(idx)
	env := []string{"GIT_INDEX_FILE=" + idx, "GIT_LITERAL_PATHSPECS=1"}
	run := func(stdin io.Reader, args ...string) ([]byte, error) { return st.Run(ctx, stdin, env, args...) }
	if parent != "" {
		if _, err := run(nil, "read-tree", string(parent)); err != nil {
			return "", err
		}
	} else if _, err := run(nil, "read-tree", "--empty"); err != nil {
		return "", err
	}
	for _, op := range ops {
		switch op.kind {
		case OpAdd:
			if _, err := run(nil, "update-index", "--add", "--cacheinfo", "100644,"+op.oid+","+op.path); err != nil {
				return "", fmt.Errorf("%w: cannot add %s: %s", ErrInvalid, op.path, gitMessage(err))
			}
		case OpDelete:
			if _, err := run(nil, "ls-files", "--error-unmatch", "--", op.path); err != nil {
				return "", fmt.Errorf("%w: A file with this name doesn't exist: %s", ErrEntryNotFound, op.path)
			}
			out, err := run(nil, "ls-files", "-z", "--", op.path)
			if err != nil {
				return "", err
			}
			if !containsName(out, op.path) {
				return "", fmt.Errorf("%w: %s is a folder; delete it as a folder", ErrEntryNotFound, op.path)
			}
			if err := removeFromIndex(run, []string{op.path}); err != nil {
				return "", err
			}
		case OpDeleteFolder:
			args := []string{"ls-files", "-z"}
			if op.path != "" {
				args = append(args, "--", op.path+"/")
			}
			out, err := run(nil, args...)
			if err != nil {
				return "", err
			}
			if len(out) == 0 {
				return "", fmt.Errorf("%w: folder %s does not exist", ErrEntryNotFound, op.path)
			}
			files := strings.Split(strings.TrimRight(string(out), "\x00"), "\x00")
			if err := removeFromIndex(run, files); err != nil {
				return "", err
			}
		case OpCopy:
			src := parent
			if op.srcRev != "" {
				sha, err := s.resolve(ctx, s.db, row, op.srcRev)
				if err != nil {
					return "", err
				}
				src = sha
			}
			if src == "" {
				return "", fmt.Errorf("%w: copy source %s does not exist", ErrEntryNotFound, op.srcPath)
			}
			ents, err := lsTree(ctx, s.git.Git(), st.Dir, string(src), "--", op.srcPath)
			if err != nil {
				return "", err
			}
			if len(ents) != 1 || ents[0].typ != "blob" || ents[0].path != op.srcPath {
				return "", fmt.Errorf("%w: copy source %s does not exist", ErrEntryNotFound, op.srcPath)
			}
			if _, err := run(nil, "update-index", "--add", "--cacheinfo", ents[0].mode+","+ents[0].oid+","+op.path); err != nil {
				return "", fmt.Errorf("%w: cannot copy to %s: %s", ErrInvalid, op.path, gitMessage(err))
			}
		}
	}
	out, err := run(nil, "write-tree")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func containsName(out []byte, name string) bool {
	for _, f := range strings.Split(strings.TrimRight(string(out), "\x00"), "\x00") {
		if f == name {
			return true
		}
	}
	return false
}

func gitMessage(err error) string {
	var ge *gitstore.Error
	if errors.As(err, &ge) {
		return ge.Stderr
	}
	return err.Error()
}

// removeFromIndex drops paths from the index. --index-info with mode 0
// works in a bare repository, unlike --force-remove.
func removeFromIndex(run func(io.Reader, ...string) ([]byte, error), paths []string) error {
	var b strings.Builder
	for _, p := range paths {
		b.WriteString("0 0000000000000000000000000000000000000000\t" + p + "\n")
	}
	_, err := run(strings.NewReader(b.String()), "update-index", "--index-info")
	return err
}

func sigEnv(sig Signature) []string {
	date := strconv.FormatInt(sig.When.Unix(), 10) + " +0000"
	return []string{
		"GIT_AUTHOR_NAME=" + sig.Name, "GIT_AUTHOR_EMAIL=" + sig.Email, "GIT_AUTHOR_DATE=" + date,
		"GIT_COMMITTER_NAME=" + sig.Name, "GIT_COMMITTER_EMAIL=" + sig.Email, "GIT_COMMITTER_DATE=" + date,
	}
}

// commitRef returns the full ref a Commit writes to. Commits may write to
// branches, pull request refs and refs/convert/*.
func commitRef(revision string) (string, error) {
	ref := model.BranchRef(revision)
	if err := model.ValidateRefName(ref); err != nil {
		return "", fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if !strings.HasPrefix(ref, model.RefHeads) && !strings.HasPrefix(ref, model.RefPR) && !strings.HasPrefix(ref, model.RefConvert) {
		return "", fmt.Errorf("%w: commits can only be made to branches, refs/pr/* and refs/convert/*, not %s", ErrInvalid, ref)
	}
	return ref, nil
}

// Commit implements Service (04-git.md "Server-side Commits"). The new
// objects are written with git into a staging repository, so they are
// byte-identical to what git produces, packed on their own and stored
// before the ref moves with compare-and-swap.
func (s *Store) Commit(ctx context.Context, p CommitParams) (*CommitResult, error) {
	if err := ValidateCommitParams(p); err != nil {
		return nil, err
	}
	row, err := s.lookup(ctx, s.db, p.Repo)
	if err != nil {
		return nil, err
	}
	return s.commit(ctx, row, p)
}

func (s *Store) commit(ctx context.Context, row *repoRow, p CommitParams) (*CommitResult, error) {
	if toRepo(row).Mirror && !p.AllowMirror {
		return nil, ErrReadOnly
	}
	revision := p.Revision
	if revision == "" {
		revision = row.DefaultBranch
	}
	ref, err := commitRef(revision)
	if err != nil {
		return nil, err
	}
	st, err := s.git.NewStaging(ctx, row.ID)
	if err != nil {
		return nil, err
	}
	defer st.Close()

	tip, exists, err := gitstore.LookupRef(ctx, s.db, row.ID, ref)
	if err != nil {
		return nil, err
	}
	parent, err := s.commitParent(ctx, row, ref, tip, exists, p)
	if err != nil {
		return nil, err
	}
	ops, lfsRefs, added, err := s.prepare(ctx, row, st, parent, p)
	if err != nil {
		return nil, err
	}
	if s.opts.Quota != nil && added > 0 {
		if err := s.opts.Quota.CheckQuota(ctx, row.NamespaceID, added); err != nil {
			return nil, err
		}
	}
	when := p.Time
	if when.IsZero() {
		when = s.now()
	}
	sig := signature(p.Actor, when)
	msg := p.Summary
	if p.Description != "" {
		msg += "\n\n" + p.Description
	}
	for attempt := 1; ; attempt++ {
		tree, err := s.buildTree(ctx, row, st, parent, ops)
		if err != nil {
			return nil, err
		}
		if parent != "" && exists {
			parentTree, err := st.Run(ctx, nil, nil, "rev-parse", string(parent)+"^{tree}")
			if err != nil {
				return nil, err
			}
			if strings.TrimSpace(string(parentTree)) == tree {
				return &CommitResult{SHA: parent, Parent: parent, Ref: ref, NoOp: true}, nil
			}
		}
		args := []string{"commit-tree", tree, "-F", "-"}
		if parent != "" {
			args = []string{"commit-tree", tree, "-p", string(parent), "-F", "-"}
		}
		out, err := st.Run(ctx, strings.NewReader(msg), sigEnv(sig), args...)
		if err != nil {
			return nil, err
		}
		sha := model.CommitSHA(strings.TrimSpace(string(out)))
		pack, _, err := st.PackLoose(ctx)
		if err != nil {
			return nil, err
		}
		old := tip
		if !exists {
			old = ""
		}
		err = s.apply(ctx, applyParams{row: row, updates: []gitstore.RefUpdate{{Name: ref, Old: old, New: sha}}, st: st, pack: pack, lfs: lfsRefs, actor: p.Actor})
		if err == nil {
			return &CommitResult{SHA: sha, Parent: parent, Ref: ref}, nil
		}
		if !errors.Is(err, ErrRefConflict) {
			return nil, err
		}
		if p.ParentCommit != "" {
			return nil, fmt.Errorf("%w: %s moved while the commit was made", ErrParentMismatch, revision)
		}
		if attempt >= maxCommitAttempts {
			return nil, err
		}
		// Retry on the new tip if no path in this Commit changed meanwhile.
		newTip, newExists, lerr := gitstore.LookupRef(ctx, s.db, row.ID, ref)
		if lerr != nil {
			return nil, lerr
		}
		if !newExists || !exists {
			return nil, err
		}
		if conflict, cerr := s.pathsChanged(ctx, st, row.ID, tip, newTip, ops); cerr != nil || conflict {
			return nil, err
		}
		tip, parent = newTip, newTip
	}
}

// commitParent decides the parent of a Commit: the branch tip, CreateFrom
// for a new ref, or none for the first Commit of an empty Repo.
func (s *Store) commitParent(ctx context.Context, row *repoRow, ref string, tip model.CommitSHA, exists bool, p CommitParams) (model.CommitSHA, error) {
	switch {
	case exists:
		if p.ParentCommit != "" && !strings.HasPrefix(string(tip), strings.ToLower(string(p.ParentCommit))) {
			return "", fmt.Errorf("%w: %s is at %s", ErrParentMismatch, model.ShortRefName(ref), tip)
		}
		return tip, nil
	case p.CreateFrom != "":
		sha, err := s.resolve(ctx, s.db, row, string(p.CreateFrom))
		if err != nil {
			return "", err
		}
		return sha, nil
	case ref == model.BranchRef(row.DefaultBranch):
		refs, err := gitstore.Refs(ctx, s.db, row.ID)
		if err != nil {
			return "", err
		}
		if len(refs) == 0 {
			return "", nil
		}
	}
	return "", fmt.Errorf("%w: %s", ErrRevisionNotFound, model.ShortRefName(ref))
}

// pathsChanged reports whether any path of ops differs between two Commits.
func (s *Store) pathsChanged(ctx context.Context, st *gitstore.Staging, repoID uuid.UUID, from, to model.CommitSHA, ops []preparedOp) (bool, error) {
	// The new tip's objects may be in packs this replica has not fetched.
	objs, err := s.git.Acquire(ctx, repoID)
	if err != nil {
		return true, err
	}
	defer objs.Release()
	args := []string{"--literal-pathspecs", "diff-tree", "-r", "--name-only", "--no-commit-id", string(from), string(to), "--"}
	for _, op := range ops {
		if op.path == "" {
			return true, nil
		}
		args = append(args, op.path)
	}
	out, err := st.Run(ctx, nil, nil, args...)
	if err != nil {
		return true, err
	}
	return len(bytes.TrimSpace(out)) > 0, nil
}

// UpdateRefs implements Service. Every new target must be an object in the
// Repo.
func (s *Store) UpdateRefs(ctx context.Context, id model.RepoID, updates []RefUpdate) error {
	row, err := s.lookup(ctx, s.db, id)
	if err != nil {
		return err
	}
	return s.updateRefs(ctx, row, updates, model.System())
}

func (s *Store) updateRefs(ctx context.Context, row *repoRow, updates []RefUpdate, actor model.Principal) error {
	if len(updates) == 0 {
		return nil
	}
	gus := make([]gitstore.RefUpdate, 0, len(updates))
	var news []string
	for _, u := range updates {
		gus = append(gus, gitstore.RefUpdate{Name: u.Name, Old: u.Old, New: u.New})
		if !u.New.IsZero() {
			if !model.IsFullSHA(string(u.New)) {
				return fmt.Errorf("%w: %q is not a full SHA", ErrInvalid, u.New)
			}
			news = append(news, string(u.New))
		}
	}
	if len(news) > 0 {
		objs, err := s.git.Acquire(ctx, row.ID)
		if err != nil {
			return err
		}
		info, err := s.git.Git().BatchCheck(ctx, objs.Dir, news)
		objs.Release()
		if err != nil {
			return err
		}
		for _, i := range info {
			if i.Missing || (i.Type != "commit" && i.Type != "tag") {
				return fmt.Errorf("%w: %s", ErrRevisionNotFound, i.OID)
			}
		}
	}
	return s.apply(ctx, applyParams{row: row, updates: gus, actor: actor})
}

func (s *Store) writableRow(ctx context.Context, id model.RepoID) (*repoRow, error) {
	row, err := s.lookup(ctx, s.db, id)
	if err != nil {
		return nil, err
	}
	if toRepo(row).Mirror {
		return nil, ErrReadOnly
	}
	return row, nil
}

// CreateBranch implements Service.
func (s *Store) CreateBranch(ctx context.Context, id model.RepoID, branch, from string, actor model.Principal) error {
	row, err := s.writableRow(ctx, id)
	if err != nil {
		return err
	}
	ref := model.RefHeads + branch
	if err := model.ValidateRefName(ref); err != nil || strings.HasPrefix(branch, "refs/") {
		return fmt.Errorf("%w: invalid branch name %q", ErrInvalid, branch)
	}
	if _, ok, err := gitstore.LookupRef(ctx, s.db, row.ID, ref); err != nil {
		return err
	} else if ok {
		return fmt.Errorf("%w: %s", ErrRefExists, branch)
	}
	sha, err := s.resolve(ctx, s.db, row, from)
	if err != nil {
		return err
	}
	err = s.updateRefs(ctx, row, []RefUpdate{{Name: ref, New: sha}}, actor)
	if errors.Is(err, ErrRefConflict) {
		return fmt.Errorf("%w: %s", ErrRefExists, branch)
	}
	return err
}

// DeleteBranch implements Service.
func (s *Store) DeleteBranch(ctx context.Context, id model.RepoID, branch string, actor model.Principal) error {
	row, err := s.writableRow(ctx, id)
	if err != nil {
		return err
	}
	if branch == row.DefaultBranch {
		return fmt.Errorf("%w: the default branch cannot be deleted", ErrProtectedRef)
	}
	ref := model.RefHeads + branch
	sha, ok, err := gitstore.LookupRef(ctx, s.db, row.ID, ref)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: %s", ErrRevisionNotFound, branch)
	}
	return s.apply(ctx, applyParams{row: row, updates: []gitstore.RefUpdate{{Name: ref, Old: sha}}, actor: actor})
}

// CreateTag implements Service. A Message makes an annotated tag, written
// with git like a Commit.
func (s *Store) CreateTag(ctx context.Context, p TagParams) error {
	row, err := s.writableRow(ctx, p.Repo)
	if err != nil {
		return err
	}
	ref := model.RefTags + p.Tag
	if err := model.ValidateRefName(ref); err != nil || strings.HasPrefix(p.Tag, "refs/") {
		return fmt.Errorf("%w: invalid tag name %q", ErrInvalid, p.Tag)
	}
	if _, ok, err := gitstore.LookupRef(ctx, s.db, row.ID, ref); err != nil {
		return err
	} else if ok {
		return fmt.Errorf("%w: %s", ErrRefExists, p.Tag)
	}
	sha, err := s.resolve(ctx, s.db, row, p.Revision)
	if err != nil {
		return err
	}
	if p.Message == "" {
		err := s.updateRefs(ctx, row, []RefUpdate{{Name: ref, New: sha}}, p.Actor)
		if errors.Is(err, ErrRefConflict) {
			return fmt.Errorf("%w: %s", ErrRefExists, p.Tag)
		}
		return err
	}
	st, err := s.git.NewStaging(ctx, row.ID)
	if err != nil {
		return err
	}
	defer st.Close()
	sig := signature(p.Actor, s.now())
	if _, err := st.Run(ctx, nil, sigEnv(sig), "tag", "-a", "-m", p.Message, "--", p.Tag, string(sha)); err != nil {
		return fmt.Errorf("%w: %s", ErrInvalid, gitMessage(err))
	}
	tagOID, err := st.Run(ctx, nil, nil, "rev-parse", ref)
	if err != nil {
		return err
	}
	pack, _, err := st.PackLoose(ctx)
	if err != nil {
		return err
	}
	err = s.apply(ctx, applyParams{row: row, updates: []gitstore.RefUpdate{{Name: ref, New: model.CommitSHA(strings.TrimSpace(string(tagOID)))}}, st: st, pack: pack, actor: p.Actor})
	if errors.Is(err, ErrRefConflict) {
		return fmt.Errorf("%w: %s", ErrRefExists, p.Tag)
	}
	return err
}

// DeleteTag implements Service.
func (s *Store) DeleteTag(ctx context.Context, id model.RepoID, tag string, actor model.Principal) error {
	row, err := s.writableRow(ctx, id)
	if err != nil {
		return err
	}
	ref := model.RefTags + tag
	target, ok, err := gitstore.LookupRef(ctx, s.db, row.ID, ref)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: %s", ErrRevisionNotFound, tag)
	}
	return s.apply(ctx, applyParams{row: row, updates: []gitstore.RefUpdate{{Name: ref, Old: target}}, actor: actor})
}
