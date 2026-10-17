package repo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/uptrace/bun"

	"github.com/jamesagarside/hangar/internal/blob"
	"github.com/jamesagarside/hangar/internal/db"
	"github.com/jamesagarside/hangar/internal/gitstore"
	"github.com/jamesagarside/hangar/internal/model"
)

// emptyTree is the SHA-1 of git's empty tree.
const emptyTree = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

// maxTextDiff is the size limit for text diffs (REPO-3).
const maxTextDiff = 1 << 20

// Resolve implements Service. Full SHAs must name a Commit in the Repo;
// otherwise branches win over tags, and full ref names are looked up as
// given. Annotated tags are peeled to their Commit.
func (s *Store) Resolve(ctx context.Context, id model.RepoID, revision string) (model.CommitSHA, error) {
	row, err := s.lookup(ctx, s.db, id)
	if err != nil {
		return "", err
	}
	return s.resolve(ctx, s.db, row, revision)
}

func (s *Store) resolve(ctx context.Context, idb bun.IDB, row *repoRow, revision string) (model.CommitSHA, error) {
	if revision == "" {
		revision = row.DefaultBranch
	}
	if model.IsFullSHA(revision) {
		sha := model.CommitSHA(strings.ToLower(revision))
		if ok, err := s.commitKnown(ctx, row.ID, sha); err != nil {
			return "", err
		} else if ok {
			return sha, nil
		}
		return "", fmt.Errorf("%w: %s", ErrRevisionNotFound, revision)
	}
	candidates := []string{model.RefHeads + revision, model.RefTags + revision}
	if strings.HasPrefix(revision, "refs/") {
		candidates = []string{revision}
	}
	for _, c := range candidates {
		if model.ValidateRefName(c) != nil {
			continue
		}
		target, ok, err := gitstore.LookupRef(ctx, idb, row.ID, c)
		if err != nil {
			return "", err
		}
		if ok {
			return s.peel(ctx, row.ID, target)
		}
	}
	return "", fmt.Errorf("%w: %s", ErrRevisionNotFound, revision)
}

// commitKnown reports whether sha is a Commit of the Repo, from the
// commits cache or the objects.
func (s *Store) commitKnown(ctx context.Context, repoID uuid.UUID, sha model.CommitSHA) (bool, error) {
	ok, err := s.db.NewSelect().Model((*db.Commit)(nil)).Where("repo_id = ?", repoID).Where("sha = ?", string(sha)).Exists(ctx)
	if err != nil || ok {
		return ok, err
	}
	objs, err := s.git.Acquire(ctx, repoID)
	if err != nil {
		return false, err
	}
	defer objs.Release()
	return s.isCommit(ctx, objs, string(sha)), nil
}

// peel turns a ref target into a Commit, following annotated tags.
func (s *Store) peel(ctx context.Context, repoID uuid.UUID, target model.CommitSHA) (model.CommitSHA, error) {
	out, err := s.peelMany(ctx, repoID, []model.CommitSHA{target})
	if err != nil {
		return "", err
	}
	return out[target], nil
}

func (s *Store) peelMany(ctx context.Context, repoID uuid.UUID, targets []model.CommitSHA) (map[model.CommitSHA]model.CommitSHA, error) {
	out := map[model.CommitSHA]model.CommitSHA{}
	var unknown []string
	if len(targets) > 0 {
		strs := make([]string, 0, len(targets))
		for _, t := range targets {
			strs = append(strs, string(t))
		}
		var known []string
		for start := 0; start < len(strs); start += 500 {
			var part []string
			if err := s.db.NewSelect().Model((*db.Commit)(nil)).Column("sha").Where("repo_id = ?", repoID).
				Where("sha IN (?)", bun.List(strs[start:min(start+500, len(strs))])).Scan(ctx, &part); err != nil {
				return nil, err
			}
			known = append(known, part...)
		}
		isKnown := map[string]bool{}
		for _, k := range known {
			isKnown[k] = true
		}
		for _, t := range strs {
			if isKnown[t] {
				out[model.CommitSHA(t)] = model.CommitSHA(t)
			} else {
				unknown = append(unknown, t)
			}
		}
	}
	if len(unknown) == 0 {
		return out, nil
	}
	objs, err := s.git.Acquire(ctx, repoID)
	if err != nil {
		return nil, err
	}
	defer objs.Release()
	args := []string{"rev-parse"}
	for _, u := range unknown {
		args = append(args, u+"^{commit}")
	}
	res, err := s.git.Git().Output(ctx, objs.Dir, args...)
	if err != nil {
		return nil, fmt.Errorf("repo: peel refs: %w", err)
	}
	lines := strings.Fields(res)
	if len(lines) != len(unknown) {
		return nil, fmt.Errorf("repo: peel refs: got %d results for %d refs", len(lines), len(unknown))
	}
	for i, u := range unknown {
		out[model.CommitSHA(u)] = model.CommitSHA(lines[i])
	}
	return out, nil
}

// pathOrder orders tree rows directories first, then by path in byte order.
func (s *Store) pathOrder() string {
	if s.db.IsSQLite() {
		return "CASE WHEN type = 'directory' THEN 0 ELSE 1 END, path"
	}
	return `CASE WHEN type = 'directory' THEN 0 ELSE 1 END, path COLLATE "C"`
}

func (s *Store) pathGreater() string {
	if s.db.IsSQLite() {
		return "path > ?"
	}
	return `path COLLATE "C" > ?`
}

func (s *Store) treeRow(ctx context.Context, repoID uuid.UUID, commit model.CommitSHA, p string) (*db.TreeEntry, error) {
	row := new(db.TreeEntry)
	err := s.db.NewSelect().Model(row).Where("repo_id = ?", repoID).Where("commit_sha = ?", string(commit)).Where("path = ?", p).Scan(ctx)
	if db.IsNotFound(err) {
		return nil, fmt.Errorf("%w: %s", ErrEntryNotFound, p)
	}
	if err != nil {
		return nil, fmt.Errorf("repo: read tree cache: %w", err)
	}
	return row, nil
}

func toEntry(r *db.TreeEntry) model.Entry {
	e := model.Entry{Path: r.Path, Type: model.EntryType(r.Type), Size: r.Size, OID: r.GitOID, Present: true}
	if r.LFSOID != nil {
		e.LFS = &model.LFSInfo{OID: *r.LFSOID}
		if r.LFSSize != nil {
			e.LFS.Size = *r.LFSSize
		}
		if r.PointerSize != nil {
			e.LFS.PointerSize = *r.PointerSize
		}
	}
	return e
}

// decorate sets presence, and the last Commit when expand is set, on rows
// read from the tree cache.
func (s *Store) decorate(ctx context.Context, repoID uuid.UUID, commit model.CommitSHA, rows []db.TreeEntry, expand bool) ([]model.Entry, error) {
	out := make([]model.Entry, len(rows))
	var oids []string
	for i := range rows {
		out[i] = toEntry(&rows[i])
		if out[i].LFS != nil {
			oids = append(oids, out[i].LFS.OID)
		}
	}
	if len(oids) > 0 {
		present, err := s.presence(ctx, repoID, oids)
		if err != nil {
			return nil, err
		}
		for i := range out {
			if out[i].LFS != nil {
				out[i].Present = present[out[i].LFS.OID]
			}
		}
	}
	if expand {
		if err := s.expand(ctx, repoID, commit, rows, out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// presence reports which LFS objects are in the Arsenal for a Repo: stored
// and verified, and not marked absent for a Partial mirror.
func (s *Store) presence(ctx context.Context, repoID uuid.UUID, oids []string) (map[string]bool, error) {
	stored, err := s.lfs.StatMany(ctx, oids)
	if err != nil {
		return nil, err
	}
	var absent []string
	for start := 0; start < len(oids); start += 500 {
		var part []string
		if err := s.db.NewSelect().Model((*db.RepoLFS)(nil)).Column("oid").Where("repo_id = ?", repoID).
			Where("oid IN (?)", bun.List(oids[start:min(start+500, len(oids))])).Where("present = ?", false).Scan(ctx, &part); err != nil {
			return nil, err
		}
		absent = append(absent, part...)
	}
	out := make(map[string]bool, len(oids))
	for _, o := range oids {
		_, ok := stored[o]
		out[o] = ok
	}
	for _, o := range absent {
		out[o] = false
	}
	return out, nil
}

// expand fills LastCommit, computing and saving values the cache lacks.
func (s *Store) expand(ctx context.Context, repoID uuid.UUID, commit model.CommitSHA, rows []db.TreeEntry, out []model.Entry) error {
	var missing []string
	shas := map[string]bool{}
	for _, r := range rows {
		if r.LastCommit == nil {
			if r.Path != rootPath {
				missing = append(missing, r.Path)
			}
		} else {
			shas[*r.LastCommit] = true
		}
	}
	computed := map[string]model.CommitSummary{}
	if len(missing) > 0 {
		objs, err := s.git.Acquire(ctx, repoID)
		if err != nil {
			return err
		}
		computed, err = lastCommits(ctx, s.git.Git(), objs.Dir, commit, missing)
		objs.Release()
		if err != nil {
			return fmt.Errorf("repo: find last commits: %w", err)
		}
		for p, c := range computed {
			if _, err := s.db.NewUpdate().Model((*db.TreeEntry)(nil)).Set("last_commit = ?", string(c.SHA)).
				Where("repo_id = ?", repoID).Where("commit_sha = ?", string(commit)).Where("path = ?", p).Exec(ctx); err != nil {
				return err
			}
		}
	}
	summaries, err := s.summaries(ctx, repoID, shas)
	if err != nil {
		return err
	}
	for i, r := range rows {
		if r.LastCommit != nil {
			if c, ok := summaries[*r.LastCommit]; ok {
				cc := c
				out[i].LastCommit = &cc
			}
		} else if c, ok := computed[r.Path]; ok {
			cc := c
			out[i].LastCommit = &cc
		}
	}
	return nil
}

// summaries returns the title and date of Commits, from the commits cache,
// adding any that are missing.
func (s *Store) summaries(ctx context.Context, repoID uuid.UUID, shas map[string]bool) (map[string]model.CommitSummary, error) {
	out := map[string]model.CommitSummary{}
	if len(shas) == 0 {
		return out, nil
	}
	list := make([]string, 0, len(shas))
	for k := range shas {
		list = append(list, k)
	}
	var rows []db.Commit
	for start := 0; start < len(list); start += 500 {
		var part []db.Commit
		if err := s.db.NewSelect().Model(&part).Column("sha", "title", "committed_at").Where("repo_id = ?", repoID).
			Where("sha IN (?)", bun.List(list[start:min(start+500, len(list))])).Scan(ctx); err != nil {
			return nil, err
		}
		rows = append(rows, part...)
	}
	for _, r := range rows {
		out[r.SHA] = model.CommitSummary{SHA: model.CommitSHA(r.SHA), Title: r.Title, Date: r.CommittedAt.UTC()}
	}
	var missing []string
	for _, k := range list {
		if _, ok := out[k]; !ok {
			missing = append(missing, k)
		}
	}
	if len(missing) == 0 {
		return out, nil
	}
	objs, err := s.git.Acquire(ctx, repoID)
	if err != nil {
		return nil, err
	}
	defer objs.Release()
	res, err := s.git.Git().Run(ctx, objs.Dir, nil, nil, append([]string{"log", "--no-walk=unsorted", logFormat}, missing...)...)
	if err != nil {
		return nil, err
	}
	commits := parseLog(res)
	if err := s.insertCommits(ctx, repoID, commits); err != nil {
		return nil, err
	}
	for _, c := range commits {
		out[string(c.SHA)] = model.CommitSummary{SHA: c.SHA, Title: c.Title, Date: c.Committer.When}
	}
	return out, nil
}

// Tree implements Service. Directories come before files, each in byte
// order of their paths, as on the Hub.
func (s *Store) Tree(ctx context.Context, id model.RepoID, commit model.CommitSHA, p string, opts TreeOpts) ([]model.Entry, error) {
	row, err := s.lookup(ctx, s.db, id)
	if err != nil {
		return nil, err
	}
	clean, err := model.CleanRepoPath(p)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrEntryNotFound, err)
	}
	if err := s.ensureTree(ctx, row.ID, commit); err != nil {
		return nil, err
	}
	dir, err := s.treeRow(ctx, row.ID, commit, clean)
	if err != nil {
		return nil, err
	}
	if dir.Type != string(model.EntryDirectory) {
		return nil, fmt.Errorf("%w: %s is a file", ErrEntryNotFound, clean)
	}
	q := s.db.NewSelect().Model((*db.TreeEntry)(nil)).Where("repo_id = ?", row.ID).Where("commit_sha = ?", string(commit)).
		Where("path <> ?", rootPath)
	switch {
	case !opts.Recursive:
		q = q.Where("parent = ?", clean)
	case clean != "":
		q = q.Where("substr(path, 1, ?) = ?", len(clean)+1, clean+"/")
	}
	if opts.After != "" {
		afterType := string(model.EntryFile)
		if a, err := s.treeRow(ctx, row.ID, commit, opts.After); err == nil {
			afterType = a.Type
		}
		if afterType == string(model.EntryDirectory) {
			q = q.WhereGroup(" AND ", func(q *bun.SelectQuery) *bun.SelectQuery {
				return q.Where("type = 'directory' AND "+s.pathGreater(), opts.After).WhereOr("type = 'file'")
			})
		} else {
			q = q.Where("type = 'file'").Where(s.pathGreater(), opts.After)
		}
	}
	q = q.OrderExpr(s.pathOrder())
	if opts.Limit > 0 {
		q = q.Limit(int64(opts.Limit))
	}
	var rows []db.TreeEntry
	if err := q.Scan(ctx, &rows); err != nil {
		return nil, fmt.Errorf("repo: list tree: %w", err)
	}
	return s.decorate(ctx, row.ID, commit, rows, opts.Expand)
}

// Entry implements Service.
func (s *Store) Entry(ctx context.Context, id model.RepoID, commit model.CommitSHA, p string) (model.Entry, error) {
	row, err := s.lookup(ctx, s.db, id)
	if err != nil {
		return model.Entry{}, err
	}
	return s.entry(ctx, row.ID, commit, p)
}

func (s *Store) entry(ctx context.Context, repoID uuid.UUID, commit model.CommitSHA, p string) (model.Entry, error) {
	clean, err := model.CleanRepoPath(p)
	if err != nil {
		return model.Entry{}, fmt.Errorf("%w: %w", ErrEntryNotFound, err)
	}
	// Look the path up first: the tree cache of a Commit is filled in one
	// transaction, so a row means it is complete, and the hot path of
	// resolve costs one query instead of two. A miss is a path that does
	// not exist or a cache not filled yet, so fill it and look again.
	r, err := s.treeRow(ctx, repoID, commit, clean)
	if errors.Is(err, ErrEntryNotFound) {
		if err := s.ensureTree(ctx, repoID, commit); err != nil {
			return model.Entry{}, err
		}
		r, err = s.treeRow(ctx, repoID, commit, clean)
	}
	if err != nil {
		return model.Entry{}, err
	}
	out, err := s.decorate(ctx, repoID, commit, []db.TreeEntry{*r}, false)
	if err != nil {
		return model.Entry{}, err
	}
	return out[0], nil
}

// Entries implements Service.
func (s *Store) Entries(ctx context.Context, id model.RepoID, commit model.CommitSHA, paths []string, expand bool) ([]model.Entry, error) {
	row, err := s.lookup(ctx, s.db, id)
	if err != nil {
		return nil, err
	}
	if err := s.ensureTree(ctx, row.ID, commit); err != nil {
		return nil, err
	}
	var clean []string
	seen := map[string]bool{}
	for _, p := range paths {
		c, err := model.CleanRepoPath(p)
		if err != nil || seen[c] {
			continue
		}
		seen[c] = true
		clean = append(clean, c)
	}
	var rows []db.TreeEntry
	for start := 0; start < len(clean); start += 500 {
		var part []db.TreeEntry
		if err := s.db.NewSelect().Model(&part).Where("repo_id = ?", row.ID).Where("commit_sha = ?", string(commit)).
			Where("path IN (?)", bun.List(clean[start:min(start+500, len(clean))])).Scan(ctx); err != nil {
			return nil, fmt.Errorf("repo: read tree cache: %w", err)
		}
		rows = append(rows, part...)
	}
	// Answer in the order the paths were asked for.
	order := map[string]int{}
	for i, c := range clean {
		order[c] = i
	}
	sortRows(rows, func(a, b db.TreeEntry) bool { return order[a.Path] < order[b.Path] })
	return s.decorate(ctx, row.ID, commit, rows, expand)
}

// OpenFile implements Service.
func (s *Store) OpenFile(ctx context.Context, id model.RepoID, commit model.CommitSHA, p string, rng *blob.Range) (io.ReadCloser, model.Entry, error) {
	row, err := s.lookup(ctx, s.db, id)
	if err != nil {
		return nil, model.Entry{}, err
	}
	return s.openFile(ctx, row.ID, commit, p, rng)
}

func (s *Store) openFile(ctx context.Context, repoID uuid.UUID, commit model.CommitSHA, p string, rng *blob.Range) (io.ReadCloser, model.Entry, error) {
	e, err := s.entry(ctx, repoID, commit, p)
	if err != nil {
		return nil, model.Entry{}, err
	}
	if e.Type != model.EntryFile {
		return nil, model.Entry{}, fmt.Errorf("%w: %s is a directory", ErrEntryNotFound, p)
	}
	if e.LFS != nil {
		if !e.Present {
			return nil, e, fmt.Errorf("%w: %s", ErrNotPresent, p)
		}
		rc, _, err := s.lfs.Open(ctx, e.LFS.OID, rng)
		if errors.Is(err, ErrNotFound) {
			return nil, e, fmt.Errorf("%w: %s", ErrNotPresent, p)
		}
		return rc, e, err
	}
	var start, length int64 = 0, e.Size
	if rng != nil {
		a, b, err := rng.Resolve(e.Size)
		if err != nil {
			return nil, e, err
		}
		start, length = a, b-a+1
	}
	if e.Size <= maxCachedBlob {
		data, err := s.smallBlob(ctx, repoID, e.OID)
		if err != nil {
			return nil, model.Entry{}, err
		}
		if int64(len(data)) < start+length {
			return nil, model.Entry{}, fmt.Errorf("repo: git blob %s is %d bytes, not %d", e.OID, len(data), e.Size)
		}
		return io.NopCloser(bytes.NewReader(data[start : start+length])), e, nil
	}
	objs, err := s.git.Acquire(ctx, repoID)
	if err != nil {
		return nil, model.Entry{}, err
	}
	rc, err := s.git.Git().OpenBlob(ctx, objs.Dir, e.OID)
	if err != nil {
		objs.Release()
		return nil, model.Entry{}, err
	}
	if start > 0 {
		if _, err := io.CopyN(io.Discard, rc, start); err != nil {
			_ = rc.Close()
			objs.Release()
			return nil, model.Entry{}, err
		}
	}
	return &releasingReader{Reader: io.LimitReader(rc, length), closer: rc, release: objs.Release}, e, nil
}

type releasingReader struct {
	io.Reader
	closer  io.Closer
	release func()
}

func (r *releasingReader) Close() error {
	err := r.closer.Close()
	r.release()
	return err
}

// Log implements Service.
func (s *Store) Log(ctx context.Context, id model.RepoID, revision string, opts LogOpts) ([]CommitInfo, error) {
	row, err := s.lookup(ctx, s.db, id)
	if err != nil {
		return nil, err
	}
	sha, err := s.resolve(ctx, s.db, row, revision)
	if err != nil {
		return nil, err
	}
	args := []string{"--literal-pathspecs", "log", logFormat, "--skip=" + strconv.Itoa(max(opts.Skip, 0))}
	if opts.Limit > 0 {
		args = append(args, "--max-count="+strconv.Itoa(opts.Limit))
	}
	args = append(args, string(sha))
	if opts.Path != "" {
		clean, err := model.CleanRepoPath(opts.Path)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
		}
		if clean != "" {
			args = append(args, "--", clean)
		}
	}
	objs, err := s.git.Acquire(ctx, row.ID)
	if err != nil {
		return nil, err
	}
	defer objs.Release()
	out, err := s.git.Git().Run(ctx, objs.Dir, nil, nil, args...)
	if err != nil {
		return nil, err
	}
	return parseLog(out), nil
}

// GetCommit implements Service.
func (s *Store) GetCommit(ctx context.Context, id model.RepoID, sha model.CommitSHA) (*CommitInfo, error) {
	row, err := s.lookup(ctx, s.db, id)
	if err != nil {
		return nil, err
	}
	return s.getCommit(ctx, row.ID, sha)
}

func (s *Store) getCommit(ctx context.Context, repoID uuid.UUID, sha model.CommitSHA) (*CommitInfo, error) {
	if !model.IsFullSHA(string(sha)) {
		return nil, fmt.Errorf("%w: %s", ErrRevisionNotFound, sha)
	}
	c := new(db.Commit)
	err := s.db.NewSelect().Model(c).Where("repo_id = ?", repoID).Where("sha = ?", string(sha)).Scan(ctx)
	if err == nil {
		return fromCommitRow(c), nil
	}
	if !db.IsNotFound(err) {
		return nil, err
	}
	objs, err := s.git.Acquire(ctx, repoID)
	if err != nil {
		return nil, err
	}
	defer objs.Release()
	if !s.isCommit(ctx, objs, string(sha)) {
		return nil, fmt.Errorf("%w: %s", ErrRevisionNotFound, sha)
	}
	out, err := s.git.Git().Run(ctx, objs.Dir, nil, nil, "log", "-1", logFormat, string(sha))
	if err != nil {
		return nil, err
	}
	commits := parseLog(out)
	if len(commits) != 1 {
		return nil, fmt.Errorf("%w: %s", ErrRevisionNotFound, sha)
	}
	if err := s.insertCommits(ctx, repoID, commits); err != nil {
		s.log.WarnContext(ctx, "fill commit cache", "error", err.Error())
	}
	return &commits[0], nil
}

// binaryProbe is how much of a blob is checked for a NUL byte, as git does.
const binaryProbe = 8000

// blobMeta is what a diff shows about one side of a changed file.
type blobMeta struct {
	size        int64
	lfs, binary bool
}

// blobMetas returns the size, and whether it is an LFS pointer or binary,
// of each blob, with two git processes however many blobs there are: one
// for sizes and one that reads the start of every blob up to maxTextDiff
// bytes. Larger blobs, which only exist outside LFS, are probed one at a
// time. Missing objects are left out.
func (s *Store) blobMetas(ctx context.Context, dir string, oids []string) (map[string]blobMeta, error) {
	out := map[string]blobMeta{}
	seen := map[string]bool{}
	var list []string
	for _, o := range oids {
		if o == "" || strings.Trim(o, "0") == "" || seen[o] {
			continue
		}
		seen[o] = true
		list = append(list, o)
	}
	if len(list) == 0 {
		return out, nil
	}
	g := s.git.Git()
	infos, err := g.BatchCheck(ctx, dir, list)
	if err != nil {
		return nil, err
	}
	probe := func(oid string, size int64, head []byte) {
		if size <= model.MaxLFSPointerSize {
			if p, err := model.ParseLFSPointer(head); err == nil {
				out[oid] = blobMeta{size: p.Size, lfs: true}
				return
			}
		}
		out[oid] = blobMeta{size: size, binary: bytes.IndexByte(head, 0) >= 0}
	}
	var small []string
	var large []gitstore.ObjectInfo
	for _, i := range infos {
		switch {
		case i.Missing:
		case i.Size <= maxTextDiff:
			small = append(small, i.OID)
		default:
			large = append(large, i)
		}
	}
	err = g.BatchReadHead(ctx, dir, small, binaryProbe, func(info gitstore.ObjectInfo, head []byte) error {
		if !info.Missing {
			probe(info.OID, info.Size, head)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, i := range large {
		out[i.OID] = blobMeta{size: i.Size}
		rc, err := g.OpenBlob(ctx, dir, i.OID)
		if err != nil {
			continue
		}
		head := make([]byte, binaryProbe)
		n, _ := io.ReadFull(rc, head)
		_ = rc.Close()
		probe(i.OID, i.Size, head[:n])
	}
	return out, nil
}

// Diff implements Service.
func (s *Store) Diff(ctx context.Context, id model.RepoID, sha model.CommitSHA) ([]FileChange, error) {
	row, err := s.lookup(ctx, s.db, id)
	if err != nil {
		return nil, err
	}
	objs, err := s.git.Acquire(ctx, row.ID)
	if err != nil {
		return nil, err
	}
	defer objs.Release()
	changes, _, err := s.diff(ctx, row.ID, objs, sha)
	return changes, err
}

// diff lists the files a Commit changed against its first parent, and
// returns that parent (the empty tree for a root Commit).
func (s *Store) diff(ctx context.Context, repoID uuid.UUID, objs *gitstore.Objects, sha model.CommitSHA) ([]FileChange, string, error) {
	_, parents, err := s.commitHeader(ctx, repoID, objs, sha)
	if err != nil {
		return nil, "", err
	}
	parent := emptyTree
	if len(parents) > 0 {
		parent = parents[0]
	}
	changes, err := s.diffBetween(ctx, objs, parent, string(sha))
	if err != nil {
		return nil, "", err
	}
	return changes, parent, nil
}

// diffBetween summarises the files that differ from tree-ish from to
// tree-ish to. It starts the same number of git processes however many
// files changed.
func (s *Store) diffBetween(ctx context.Context, objs *gitstore.Objects, from, to string) ([]FileChange, error) {
	out, err := s.git.Git().Run(ctx, objs.Dir, nil, nil, "diff-tree", "-r", "-z", "-M", "--no-commit-id", "--raw", from, to)
	if err != nil {
		return nil, err
	}
	fields := strings.Split(strings.TrimRight(string(out), "\x00"), "\x00")
	changes := []FileChange{}
	for i := 0; i < len(fields); i++ {
		meta := fields[i]
		if !strings.HasPrefix(meta, ":") {
			continue
		}
		m := strings.Fields(strings.TrimPrefix(meta, ":"))
		if len(m) < 5 || i+1 >= len(fields) {
			continue
		}
		c := FileChange{OldOID: m[2], NewOID: m[3]}
		status := m[4]
		i++
		c.Path = fields[i]
		switch status[0] {
		case 'A':
			c.Status = ChangeAdded
		case 'D':
			c.Status = ChangeDeleted
		case 'R':
			c.Status = ChangeRenamed
			c.OldPath = c.Path
			if i+1 < len(fields) {
				i++
				c.Path = fields[i]
			}
		default:
			c.Status = ChangeModified
		}
		if strings.Trim(c.OldOID, "0") == "" {
			c.OldOID = ""
		}
		if strings.Trim(c.NewOID, "0") == "" {
			c.NewOID = ""
		}
		changes = append(changes, c)
	}
	oids := make([]string, 0, 2*len(changes))
	for _, c := range changes {
		oids = append(oids, c.OldOID, c.NewOID)
	}
	metas, err := s.blobMetas(ctx, objs.Dir, oids)
	if err != nil {
		return nil, err
	}
	for i := range changes {
		c := &changes[i]
		o, n := metas[c.OldOID], metas[c.NewOID]
		c.OldSize, c.NewSize = o.size, n.size
		c.LFS = o.lfs || n.lfs
		c.Binary = o.binary || n.binary
	}
	return changes, nil
}

// FileDiff implements Service.
func (s *Store) FileDiff(ctx context.Context, id model.RepoID, sha model.CommitSHA, p string) (*FileDiff, error) {
	row, err := s.lookup(ctx, s.db, id)
	if err != nil {
		return nil, err
	}
	objs, err := s.git.Acquire(ctx, row.ID)
	if err != nil {
		return nil, err
	}
	defer objs.Release()
	changes, parent, err := s.diff(ctx, row.ID, objs, sha)
	if err != nil {
		return nil, err
	}
	return s.fileDiff(ctx, objs, changes, parent, string(sha), p)
}

// CompareFile implements Service.
func (s *Store) CompareFile(ctx context.Context, id model.RepoID, base, head model.CommitSHA, p string) (*FileDiff, error) {
	row, err := s.lookup(ctx, s.db, id)
	if err != nil {
		return nil, err
	}
	objs, err := s.git.Acquire(ctx, row.ID)
	if err != nil {
		return nil, err
	}
	defer objs.Release()
	for _, sha := range []model.CommitSHA{base, head} {
		if _, _, err := s.commitHeader(ctx, row.ID, objs, sha); err != nil {
			return nil, err
		}
	}
	changes, err := s.diffBetween(ctx, objs, string(base), string(head))
	if err != nil {
		return nil, err
	}
	return s.fileDiff(ctx, objs, changes, string(base), string(head), p)
}

// fileDiff is the diff of path p, one of changes, from tree-ish from to
// tree-ish to.
func (s *Store) fileDiff(ctx context.Context, objs *gitstore.Objects, changes []FileChange, from, to, p string) (*FileDiff, error) {
	for _, c := range changes {
		if c.Path != p {
			continue
		}
		fd := &FileDiff{FileChange: c}
		if !c.LFS && !c.Binary && c.OldSize < maxTextDiff && c.NewSize < maxTextDiff {
			args := []string{"--literal-pathspecs", "diff", "-M", from, to, "--", p}
			if c.OldPath != "" {
				args = append(args, c.OldPath)
			}
			out, err := s.git.Git().Run(ctx, objs.Dir, nil, nil, args...)
			if err != nil {
				return nil, err
			}
			fd.Patch = string(out)
		}
		return fd, nil
	}
	return nil, fmt.Errorf("%w: %s did not change in %s", ErrEntryNotFound, p, to)
}

// Refs implements Service.
func (s *Store) Refs(ctx context.Context, id model.RepoID) (*Refs, error) {
	row, err := s.lookup(ctx, s.db, id)
	if err != nil {
		return nil, err
	}
	raw, err := gitstore.Refs(ctx, s.db, row.ID)
	if err != nil {
		return nil, err
	}
	targets := make([]model.CommitSHA, 0, len(raw))
	for _, r := range raw {
		targets = append(targets, r.Target)
	}
	peeled, err := s.peelMany(ctx, row.ID, targets)
	if err != nil {
		return nil, err
	}
	refs := &Refs{Branches: []Ref{}, Tags: []Ref{}, Converts: []Ref{}, PullRequests: []Ref{}, Other: []Ref{}}
	for _, r := range raw {
		ref := Ref{Name: r.Name, Target: peeled[r.Target]}
		switch {
		case strings.HasPrefix(r.Name, model.RefHeads):
			refs.Branches = append(refs.Branches, ref)
		case strings.HasPrefix(r.Name, model.RefTags):
			refs.Tags = append(refs.Tags, ref)
		case strings.HasPrefix(r.Name, model.RefConvert):
			refs.Converts = append(refs.Converts, ref)
		case strings.HasPrefix(r.Name, model.RefPR):
			refs.PullRequests = append(refs.PullRequests, ref)
		default:
			refs.Other = append(refs.Other, ref)
		}
	}
	return refs, nil
}

// OpenView implements Service.
func (s *Store) OpenView(ctx context.Context, id model.RepoID) (*GitView, error) {
	row, err := s.lookup(ctx, s.db, id)
	if err != nil {
		return nil, err
	}
	v, err := s.git.OpenView(ctx, row.ID, row.HeadRef)
	if err != nil {
		return nil, err
	}
	return NewGitView(v.Dir, v.Close), nil
}

// LFSRefs implements Service.
func (s *Store) LFSRefs(ctx context.Context, id model.RepoID, oids []string) (map[string]LFSRef, error) {
	row, err := s.lookup(ctx, s.db, id)
	if err != nil {
		return nil, err
	}
	out := map[string]LFSRef{}
	var rows []db.RepoLFS
	for start := 0; start < len(oids); start += 500 {
		var part []db.RepoLFS
		if err := s.db.NewSelect().Model(&part).Where("repo_id = ?", row.ID).
			Where("oid IN (?)", bun.List(oids[start:min(start+500, len(oids))])).Scan(ctx); err != nil {
			return nil, err
		}
		rows = append(rows, part...)
	}
	if len(rows) == 0 {
		return out, nil
	}
	list := make([]string, 0, len(rows))
	for _, r := range rows {
		list = append(list, r.OID)
	}
	present, err := s.presence(ctx, row.ID, list)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		out[r.OID] = LFSRef{OID: r.OID, Size: r.Size, Present: present[r.OID]}
	}
	return out, nil
}

func sortRows(rows []db.TreeEntry, less func(a, b db.TreeEntry) bool) {
	for i := 1; i < len(rows); i++ {
		for j := i; j > 0 && less(rows[j], rows[j-1]); j-- {
			rows[j], rows[j-1] = rows[j-1], rows[j]
		}
	}
}
