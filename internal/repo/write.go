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
// Commit that becomes the head of a branch, and of the scan Jobs for tag
// and pull request tips (07-indexing-and-scanning.md).
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
		// Branches are indexed and scanned. Tags and pull request refs are
		// scanned too, because scan.block_unsafe_downloads refuses files
		// that can run code until their Commit has a scan result.
		head := strings.HasPrefix(u.Name, model.RefHeads)
		if u.New.IsZero() || (!head && !strings.HasPrefix(u.Name, model.RefTags) && !strings.HasPrefix(u.Name, model.RefPR)) {
			continue
		}
		commit := u.New
		if !head {
			// An annotated tag names a tag object; scan its Commit.
			peeled, err := s.peel(ctx, row.ID, u.New)
			if err != nil {
				s.log.WarnContext(ctx, "peel ref", "repo.uuid", row.ID.String(), "ref", u.Name, "error", err.Error())
				continue
			}
			commit = peeled
		}
		if err := s.ensureTree(ctx, row.ID, commit); err != nil {
			s.log.WarnContext(ctx, "fill tree cache", "repo.uuid", row.ID.String(), "error", err.Error())
		}
		payload := CommitJob{RepoUUID: row.ID, Repo: r.ID.String(), Type: r.ID.Type, Commit: commit}
		key := row.ID.String() + ":" + string(commit)
		if head {
			s.enqueue(ctx, jobs.NewJob{Kind: jobs.KindIndex, DedupeKey: key, Payload: payload, Repo: row.ID})
		}
		s.enqueue(ctx, jobs.NewJob{Kind: jobs.KindScan, DedupeKey: key, Payload: payload, Repo: row.ID})
	}
	if n, err := s.db.NewSelect().Model((*db.Pack)(nil)).Where("repo_id = ?", row.ID).Where("state = ?", gitstore.PackLive).Count(ctx); err == nil && n > int64(s.opts.CompactThreshold) {
		s.enqueue(ctx, jobs.NewJob{Kind: jobs.KindGitCompact, DedupeKey: row.ID.String(), Payload: RepoJob{RepoUUID: row.ID}, Priority: jobs.PriorityLow, Repo: row.ID})
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
// storing paths that .gitattributes tracks with LFS as LFS objects. The
// content of every add is spooled to a file and hashed with one git
// process, however many files the Commit has.
func (s *Store) prepare(ctx context.Context, row *repoRow, st *gitstore.Staging, parent model.CommitSHA, p CommitParams) ([]preparedOp, []LFSRef, int64, error) {
	rules, err := s.lfsRules(ctx, st, row, parent)
	if err != nil {
		return nil, nil, 0, err
	}
	spool, err := os.MkdirTemp(st.Dir, "spool-")
	if err != nil {
		return nil, nil, 0, err
	}
	defer os.RemoveAll(spool)
	var (
		ops     []preparedOp
		refs    []LFSRef
		added   int64
		files   []string
		sizes   []int64
		hashFor []int
	)
	spoolBlob := func(r io.Reader) (int64, error) {
		name := filepath.Join(spool, strconv.Itoa(len(files)))
		n, err := writeSpool(name, r)
		if err != nil {
			return n, err
		}
		files = append(files, name)
		sizes = append(sizes, n)
		hashFor = append(hashFor, len(ops))
		return n, nil
	}
	for _, op := range p.Operations {
		clean, _ := model.CleanRepoPath(op.Path)
		po := preparedOp{kind: op.Kind, path: clean, srcRev: op.SrcRevision}
		switch op.Kind {
		case OpAdd:
			content := op.Content
			lfs := rules.IsLFS(clean)
			if lfs {
				ptr, err := s.ingestContent(ctx, op.Content)
				if err != nil {
					return nil, nil, 0, err
				}
				refs = append(refs, LFSRef{OID: ptr.OID, Size: ptr.Size, Present: true})
				added += ptr.Size
				content = bytes.NewReader(ptr.Bytes())
			}
			n, err := spoolBlob(content)
			if err != nil {
				return nil, nil, 0, err
			}
			if !lfs {
				if n > s.opts.MaxBlobSize {
					return nil, nil, 0, fmt.Errorf("%w: %s is %d bytes, larger than the %d byte limit for files outside LFS; track it with git lfs track or upload it as an LFS file", ErrInvalid, clean, n, s.opts.MaxBlobSize)
				}
				added += n
			}
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
			if _, err := spoolBlob(bytes.NewReader(ptr.Bytes())); err != nil {
				return nil, nil, 0, err
			}
			po.kind = OpAdd
		case OpCopy:
			po.srcPath, _ = model.CleanRepoPath(op.SrcPath)
		}
		ops = append(ops, po)
	}
	if len(files) > 0 {
		// --no-filters hashes the bytes as they are, like --stdin does.
		out, err := st.Run(ctx, strings.NewReader(strings.Join(files, "\n")+"\n"), nil, "hash-object", "-w", "--no-filters", "--stdin-paths")
		if err != nil {
			return nil, nil, 0, err
		}
		oids := strings.Fields(string(out))
		if len(oids) != len(files) {
			return nil, nil, 0, fmt.Errorf("repo: hash-object returned %d object IDs for %d files", len(oids), len(files))
		}
		for i, oid := range oids {
			ops[hashFor[i]].oid = oid
			// Small files are read back soon: by the tree cache, the
			// next Commit's .gitattributes and the web UI.
			if sizes[i] <= maxCachedBlob {
				if data, err := os.ReadFile(files[i]); err == nil {
					s.blobs.add(oid, data)
				}
			}
		}
	}
	return ops, refs, added, nil
}

// writeSpool copies r into a new file and returns the number of bytes.
func writeSpool(name string, r io.Reader) (int64, error) {
	f, err := os.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // a file in a staging directory Hangar created.
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(f, r)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return n, err
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
		obj, err = s.verifyLFS(ctx, op.OID, op.Size)
	}
	switch {
	case errors.Is(err, errVerifyWait):
		return LFSRef{}, fmt.Errorf("%w: %w", ErrLFSObjectMissing, err)
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

// errVerifyWait reports that an LFS object was still being verified when
// lfs.verify_wait ran out.
var errVerifyWait = errors.New("lfs object is still being verified")

// verifyLFS verifies an LFS object that was written through a presigned URL
// and is not recorded yet, waiting at most VerifyWait (lfs.verify_wait).
// The verification itself carries on when the wait runs out, and concurrent
// writers on this process share it, so a retried Commit finds the object
// verified instead of hashing it again.
func (s *Store) verifyLFS(ctx context.Context, oid string, size int64) (LFSObject, error) {
	ch := s.verifies.DoChan(oid+":"+strconv.FormatInt(size, 10), func() (any, error) {
		return s.lfs.Verify(context.WithoutCancel(ctx), oid, size)
	})
	timer := time.NewTimer(s.opts.VerifyWait)
	defer timer.Stop()
	select {
	case res := <-ch:
		if res.Err != nil {
			return LFSObject{}, res.Err
		}
		return res.Val.(LFSObject), nil
	case <-timer.C:
		return LFSObject{}, fmt.Errorf("%w: %s (%d bytes) was not verified within %s; try again later", errVerifyWait, oid, size, s.opts.VerifyWait)
	case <-ctx.Done():
		return LFSObject{}, ctx.Err()
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
		data, ok, err := s.gitattributes(ctx, st, row.ID, parent)
		if err != nil {
			return nil, err
		}
		if ok {
			return ParseLFSRules(data), nil
		}
	}
	return ParseLFSRules(DefaultGitAttributes(model.RepoType(row.Type))), nil
}

// gitattributes returns the content of .gitattributes at a Commit, and
// false when there is no such file. When the Commit is in the tree cache,
// which it is for every branch tip, the blob comes from the blob cache, so
// a Commit usually starts no git process for it.
func (s *Store) gitattributes(ctx context.Context, st *gitstore.Staging, repoID uuid.UUID, commit model.CommitSHA) ([]byte, bool, error) {
	cached, err := s.treeCached(ctx, s.db, repoID, commit)
	if err != nil {
		return nil, false, fmt.Errorf("repo: read tree cache: %w", err)
	}
	if cached {
		r, err := s.treeRow(ctx, repoID, commit, gitattributesPath)
		switch {
		case errors.Is(err, ErrEntryNotFound):
			return nil, false, nil
		case err != nil:
			return nil, false, err
		case r.Type != string(model.EntryFile):
			return nil, false, nil
		case r.LFSOID == nil && r.Size <= maxCachedBlob:
			data, err := s.smallBlob(ctx, repoID, r.GitOID)
			if err != nil {
				return nil, false, err
			}
			return data, true, nil
		}
	}
	out, err := st.Run(ctx, nil, nil, "cat-file", "blob", string(commit)+":"+gitattributesPath)
	if err != nil {
		var ge *gitstore.Error
		if errors.As(err, &ge) && ctx.Err() == nil {
			// No .gitattributes, or not a file.
			return nil, false, nil
		}
		return nil, false, err
	}
	return out, true, nil
}

const gitattributesPath = ".gitattributes"

// buildTree applies prepared operations to parent's tree in a temporary
// index and writes the new tree. Runs of adds and copies go to one
// update-index process; a delete first writes what is pending, because it
// looks at the index.
func (s *Store) buildTree(ctx context.Context, row *repoRow, st *gitstore.Staging, parent model.CommitSHA, ops []preparedOp) (string, error) {
	idx := filepath.Join(st.Dir, "index-"+uuid.NewString())
	defer os.Remove(idx)
	env := []string{"GIT_INDEX_FILE=" + idx, "GIT_LITERAL_PATHSPECS=1"}
	run := func(stdin io.Reader, args ...string) ([]byte, error) { return st.Run(ctx, stdin, env, args...) }
	// A missing index file is an empty index, so a first Commit needs no
	// read-tree.
	if parent != "" {
		if _, err := run(nil, "read-tree", string(parent)); err != nil {
			return "", err
		}
	}
	var pending []cacheInfo
	flush := func() error {
		err := addToIndex(ctx, run, pending)
		pending = pending[:0]
		return err
	}
	for _, op := range ops {
		switch op.kind {
		case OpAdd:
			pending = append(pending, cacheInfo{mode: "100644", oid: op.oid, path: op.path, verb: "add"})
		case OpDelete:
			if err := flush(); err != nil {
				return "", err
			}
			out, err := run(nil, "ls-files", "-z", "--", op.path)
			if err != nil {
				return "", err
			}
			if len(out) == 0 {
				return "", fmt.Errorf("%w: A file with this name doesn't exist: %s", ErrEntryNotFound, op.path)
			}
			if !containsName(out, op.path) {
				return "", fmt.Errorf("%w: %s is a folder; delete it as a folder", ErrEntryNotFound, op.path)
			}
			if err := removeFromIndex(run, []string{op.path}); err != nil {
				return "", err
			}
		case OpDeleteFolder:
			if err := flush(); err != nil {
				return "", err
			}
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
			pending = append(pending, cacheInfo{mode: ents[0].mode, oid: ents[0].oid, path: op.path, verb: "copy to"})
		}
	}
	if err := flush(); err != nil {
		return "", err
	}
	out, err := run(nil, "write-tree")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// cacheInfo is one file to put in the index.
type cacheInfo struct {
	mode, oid, path string
	// verb names the operation in errors: "add" or "copy to".
	verb string
}

// maxCacheInfoArgs bounds the bytes of --cacheinfo arguments given to one
// update-index process, well below every platform's argument limit.
const maxCacheInfoArgs = 64 << 10

// addToIndex adds files with `update-index --add --cacheinfo`, several per
// process. --cacheinfo, unlike --index-info, refuses a file that would
// replace a folder or the other way round, as the Hub does. git writes
// nothing when one of them fails, so the failing batch is replayed one
// file at a time to name the file in the error.
func addToIndex(ctx context.Context, run func(io.Reader, ...string) ([]byte, error), files []cacheInfo) error {
	for len(files) > 0 {
		args := []string{"update-index", "--add"}
		size, n := 0, 0
		for n < len(files) && (n == 0 || size < maxCacheInfoArgs) {
			arg := files[n].mode + "," + files[n].oid + "," + files[n].path
			args = append(args, "--cacheinfo", arg)
			size += len(arg)
			n++
		}
		if _, err := run(nil, args...); err != nil {
			if ctx.Err() != nil {
				return err
			}
			for _, f := range files[:n] {
				if _, err := run(nil, "update-index", "--add", "--cacheinfo", f.mode+","+f.oid+","+f.path); err != nil {
					if ctx.Err() != nil {
						return err
					}
					return fmt.Errorf("%w: cannot %s %s: %s", ErrInvalid, f.verb, f.path, gitMessage(err))
				}
			}
		}
		files = files[n:]
	}
	return nil
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
// works in a bare repository, unlike --force-remove. Paths are quoted, so
// a name that starts with a double quote or holds a newline is read as is.
func removeFromIndex(run func(io.Reader, ...string) ([]byte, error), paths []string) error {
	var b strings.Builder
	for _, p := range paths {
		b.WriteString("0 0000000000000000000000000000000000000000\t" + quoteCPath(p) + "\n")
	}
	_, err := run(strings.NewReader(b.String()), "update-index", "--index-info")
	return err
}

// quoteCPath quotes a path the way git's C-style unquoting reads it back.
func quoteCPath(p string) string {
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(p); i++ {
		c := p[i]
		switch {
		case c == '"' || c == '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		case c < 0x20 || c == 0x7f:
			fmt.Fprintf(&b, "\\%03o", c)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
	return b.String()
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
	if strings.HasPrefix(ref, model.RefConvert) && p.Actor.Kind != model.PrincipalSystem {
		// Only Hangar writes Parquet exports, as on the Hub.
		return nil, &PolicyError{Ref: ref, Message: "refs/convert/* is written by Hangar's Parquet export and cannot be committed to"}
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
			parentTree, err := s.commitTree(ctx, st, row.ID, parent)
			if err != nil {
				return nil, err
			}
			if parentTree == tree {
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
		// The paths added were checked one by one; this bounds the whole
		// Commit, as for pushes. Names already in the parent are not
		// checked again, so a Repo that holds one from before the check
		// still takes Commits.
		if err := s.checkTrees(ctx, st.Dir, []string{string(sha)}, false); err != nil {
			return nil, err
		}
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

// commitTree returns the tree of a Commit, from the commits cache when it
// has the Commit.
func (s *Store) commitTree(ctx context.Context, st *gitstore.Staging, repoID uuid.UUID, sha model.CommitSHA) (string, error) {
	var tree string
	err := s.db.NewSelect().Model((*db.Commit)(nil)).Column("tree").Where("repo_id = ?", repoID).Where("sha = ?", string(sha)).Scan(ctx, &tree)
	if err == nil && tree != "" {
		return tree, nil
	}
	if err != nil && !db.IsNotFound(err) {
		return "", err
	}
	out, err := st.Run(ctx, nil, nil, "rev-parse", string(sha)+"^{tree}")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
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
