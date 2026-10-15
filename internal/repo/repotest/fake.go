// Package repotest provides fakes of repo.Service and repo.LFSStore for
// other packages' tests. The Service fake keeps each Repo in a real bare git
// repository and drives the git binary, so Commits, trees and SHAs are the
// ones git itself produces and a GitView can be served by git upload-pack.
package repotest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jamesagarside/hangar/internal/blob"
	"github.com/jamesagarside/hangar/internal/blob/blobtest"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/repo"
)

// emptyTree is the SHA-1 of git's empty tree.
const emptyTree = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

// maxTextDiff is the size limit for text diffs (REPO-3).
const maxTextDiff = 1 << 20

// Fake is a repo.Service for tests. It is safe for concurrent use; calls
// are serialised. List applies the Viewer, Type, Namespaces, Search, Mirror
// and Tags filters and sorts by name, created or updated time; it returns
// an error for metadata filters, which need the index.
type Fake struct {
	// LFS stores LFS objects. Commits with OpAddLFS need the object in it,
	// and OpAdd stores paths that .gitattributes tracks with LFS in it.
	LFS *FakeLFS

	root string
	git  git

	mu        sync.Mutex
	repos     map[uuid.UUID]*fakeRepo
	names     map[string]uuid.UUID
	redirects map[string]uuid.UUID
	// namespaces maps lower-case Namespace names to IDs (MapNamespace).
	namespaces map[string]uuid.UUID
	now        func() time.Time
}

type fakeRepo struct {
	repo.Repo
	dir string
	lfs map[string]repo.LFSRef
}

var _ repo.Service = (*Fake)(nil)

// NewFake returns an empty Fake with its git data in a test temporary
// directory and an in-memory LFS store.
func NewFake(t testing.TB) *Fake {
	t.Helper()
	root := t.TempDir()
	home := filepath.Join(root, ".home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatalf("create git home: %v", err)
	}
	return &Fake{
		LFS:        NewFakeLFS(blobtest.NewFake()),
		root:       root,
		git:        git{home: home},
		repos:      map[uuid.UUID]*fakeRepo{},
		names:      map[string]uuid.UUID{},
		redirects:  map[string]uuid.UUID{},
		namespaces: map[string]uuid.UUID{},
		now:        func() time.Time { return time.Now().UTC().Truncate(time.Second) },
	}
}

// MapNamespace records a Namespace's ID, so Repos created in or transferred
// to it get that NamespaceID, as the real service does for Organisation
// Namespaces. Without a mapping, Create uses the actor's personal Namespace.
func (f *Fake) MapNamespace(name string, id uuid.UUID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.namespaces[strings.ToLower(name)] = id
}

func (f *Fake) namespaceID(name string, fallback uuid.UUID) uuid.UUID {
	if id, ok := f.namespaces[strings.ToLower(name)]; ok {
		return id
	}
	return fallback
}

// SetClock makes the fake use now for Commit times and timestamps.
func (f *Fake) SetClock(now func() time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = now
}

// Dir returns the bare git directory of a Repo, for assertions with git.
func (f *Fake) Dir(id model.RepoID) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, err := f.lookup(id)
	if err != nil {
		return "", err
	}
	return r.dir, nil
}

// SetMirror marks a Repo as a Mirror, or detaches it.
func (f *Fake) SetMirror(id model.RepoID, mirror bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, err := f.lookup(id)
	if err != nil {
		return err
	}
	r.Mirror = mirror
	return nil
}

func nameKey(t model.RepoType, ns, name string) string {
	return string(t) + ":" + strings.ToLower(ns) + "/" + strings.ToLower(name)
}

func keyOf(id model.RepoID) string { return nameKey(id.Type, id.Namespace, id.Name) }

func (f *Fake) lookup(id model.RepoID) (*fakeRepo, error) {
	if u, ok := f.names[keyOf(id)]; ok {
		return f.repos[u], nil
	}
	if u, ok := f.redirects[keyOf(id)]; ok {
		if r := f.repos[u]; r != nil && r.DeletedAt == nil {
			return r, nil
		}
	}
	return nil, fmt.Errorf("%w: %s", repo.ErrNotFound, id.Path())
}

func copyRepo(r *fakeRepo) *repo.Repo {
	out := r.Repo
	out.Tags = slices.Clone(r.Tags)
	return &out
}

func signature(p model.Principal, when time.Time) repo.Signature {
	name := p.DisplayName
	if name == "" {
		name = p.Username
	}
	if name == "" {
		name = "Hangar"
	}
	email := p.Email
	if email == "" {
		user := p.Username
		if user == "" {
			user = model.SystemUsername
		}
		email = user + "@users.noreply.hangar"
	}
	return repo.Signature{Name: name, Email: email, When: when}
}

func sigEnv(author repo.Signature) []string {
	date := strconv.FormatInt(author.When.Unix(), 10) + " +0000"
	return []string{
		"GIT_AUTHOR_NAME=" + author.Name, "GIT_AUTHOR_EMAIL=" + author.Email, "GIT_AUTHOR_DATE=" + date,
		"GIT_COMMITTER_NAME=" + author.Name, "GIT_COMMITTER_EMAIL=" + author.Email, "GIT_COMMITTER_DATE=" + date,
	}
}

// Create implements repo.Service.
func (f *Fake) Create(ctx context.Context, p repo.CreateParams) (*repo.Repo, error) {
	if err := p.ID.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %w", repo.ErrInvalid, err)
	}
	vis := p.Visibility
	if vis == "" {
		vis = model.VisibilityPublic
	}
	if !vis.Valid() {
		return nil, fmt.Errorf("%w: visibility %q", repo.ErrInvalid, vis)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.names[keyOf(p.ID)]; ok {
		return nil, fmt.Errorf("%w: %s", repo.ErrExists, p.ID.Path())
	}
	now := f.now()
	r := &fakeRepo{
		Repo: repo.Repo{
			UUID: uuid.Must(uuid.NewV7()), ID: p.ID, NamespaceID: f.namespaceID(p.ID.Namespace, p.Actor.NamespaceID), Visibility: vis,
			DefaultBranch: model.DefaultBranch, HeadRef: model.DefaultHeadRef, Description: p.Description,
			Tags: []string{}, Licence: p.Licence, Mirror: p.Mirror, CreatedBy: p.Actor.UserID,
			CreatedAt: now, UpdatedAt: now, LastModified: now,
		},
		lfs: map[string]repo.LFSRef{},
	}
	r.dir = filepath.Join(f.root, r.UUID.String()+".git")
	if _, err := f.git.run(ctx, f.root, nil, nil, "init", "--bare", "-q", "--initial-branch="+model.DefaultBranch, r.dir); err != nil {
		return nil, err
	}
	f.repos[r.UUID] = r
	f.names[keyOf(p.ID)] = r.UUID
	if !p.Mirror {
		ops := []repo.Operation{{Kind: repo.OpAdd, Path: ".gitattributes", Content: bytes.NewReader(repo.DefaultGitAttributes(p.ID.Type))}}
		if p.InitialReadme {
			readme := "---\nlicense: " + orDefault(p.Licence, "other") + "\n---\n\n# " + p.ID.Name + "\n\n" + p.Description + "\n"
			ops = append(ops, repo.Operation{Kind: repo.OpAdd, Path: "README.md", Content: strings.NewReader(readme)})
		}
		if _, err := f.commitLocked(ctx, r, repo.CommitParams{
			Repo: p.ID, Revision: model.DefaultBranch, Summary: "initial commit", Operations: ops, Actor: p.Actor, AllowMirror: true,
		}); err != nil {
			return nil, err
		}
	}
	return copyRepo(r), nil
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// Get implements repo.Service.
func (f *Fake) Get(_ context.Context, id model.RepoID) (*repo.Repo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, err := f.lookup(id)
	if err != nil {
		return nil, err
	}
	return copyRepo(r), nil
}

// GetByUUID implements repo.Service.
func (f *Fake) GetByUUID(_ context.Context, id uuid.UUID) (*repo.Repo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.repos[id]
	if !ok {
		return nil, fmt.Errorf("%w: %s", repo.ErrNotFound, id)
	}
	return copyRepo(r), nil
}

// List implements repo.Service.
func (f *Fake) List(_ context.Context, opts repo.ListOptions) (*repo.ListResult, error) {
	if len(opts.Formats)+len(opts.Tasks)+len(opts.Libraries)+len(opts.Quants)+len(opts.Licences)+
		len(opts.Modalities)+len(opts.Languages)+len(opts.SizeCategories) > 0 ||
		opts.ParamsMin+opts.ParamsMax+opts.ContextMin+opts.SizeMax+opts.RowsMin+opts.RowsMax > 0 || opts.IncludeFacets {
		return nil, errors.New("repotest: metadata filters and facets need the index; the fake does not support them")
	}
	f.mu.Lock()
	var out []*repo.Repo
	for _, r := range f.repos {
		if r.DeletedAt != nil || !visibleTo(&r.Repo, opts.Viewer) {
			continue
		}
		if opts.Type != "" && r.ID.Type != opts.Type {
			continue
		}
		if len(opts.Namespaces) > 0 && !slices.ContainsFunc(opts.Namespaces, func(ns string) bool { return strings.EqualFold(ns, r.ID.Namespace) }) {
			continue
		}
		if opts.Search != "" && !strings.Contains(strings.ToLower(r.ID.String()), strings.ToLower(opts.Search)) {
			continue
		}
		if opts.Mirror != nil && r.Mirror != *opts.Mirror {
			continue
		}
		if !hasAll(r.Tags, opts.Tags) {
			continue
		}
		out = append(out, copyRepo(r))
	}
	f.mu.Unlock()
	less := func(a, b *repo.Repo) bool { return strings.ToLower(a.ID.String()) < strings.ToLower(b.ID.String()) }
	switch opts.Sort {
	case "", "name":
	case "created":
		less = func(a, b *repo.Repo) bool { return a.CreatedAt.Before(b.CreatedAt) }
	case "updated":
		less = func(a, b *repo.Repo) bool { return a.LastModified.Before(b.LastModified) }
	default:
		return nil, fmt.Errorf("repotest: sort %q needs the index; the fake supports name, created and updated", opts.Sort)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if opts.Desc {
			return less(out[j], out[i])
		}
		return less(out[i], out[j])
	})
	offset := 0
	if opts.Cursor != "" {
		n, err := strconv.Atoi(opts.Cursor)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("%w: bad cursor", repo.ErrInvalid)
		}
		offset = n
	}
	res := &repo.ListResult{}
	if offset < len(out) {
		out = out[offset:]
	} else {
		out = nil
	}
	if opts.Limit > 0 && len(out) > opts.Limit {
		out = out[:opts.Limit]
		res.NextCursor = strconv.Itoa(offset + opts.Limit)
	}
	res.Repos = out
	return res, nil
}

func hasAll(have, want []string) bool {
	for _, w := range want {
		if !slices.Contains(have, w) {
			return false
		}
	}
	return true
}

// visibleTo is the fake's simplified read rule: public Repos for anyone,
// internal Repos for signed-in users, private Repos for admins and the
// owner Namespace's user.
func visibleTo(r *repo.Repo, p model.Principal) bool {
	switch r.Visibility {
	case model.VisibilityPublic:
		return true
	case model.VisibilityInternal:
		return !p.IsAnonymous()
	default:
		return p.IsAdmin || (!p.IsAnonymous() && strings.EqualFold(p.Username, r.ID.Namespace))
	}
}

func (f *Fake) revParse(ctx context.Context, r *fakeRepo, rev string) (model.CommitSHA, bool) {
	out, err := f.git.out(ctx, r.dir, "rev-parse", "--verify", "--quiet", rev+"^{commit}")
	if err != nil || out == "" {
		return "", false
	}
	return model.CommitSHA(out), true
}

// resolve turns a Revision into a Commit: full SHAs, then branches, then
// tags, then full ref names.
func (f *Fake) resolve(ctx context.Context, r *fakeRepo, revision string) (model.CommitSHA, error) {
	if revision == "" {
		revision = r.DefaultBranch
	}
	if model.IsFullSHA(revision) {
		sha := model.CommitSHA(strings.ToLower(revision))
		if t, err := f.git.out(ctx, r.dir, "cat-file", "-t", string(sha)); err == nil && t == "commit" {
			return sha, nil
		}
		return "", fmt.Errorf("%w: %s", repo.ErrRevisionNotFound, revision)
	}
	candidates := []string{model.RefHeads + revision, model.RefTags + revision}
	if strings.HasPrefix(revision, "refs/") {
		candidates = []string{revision}
	}
	for _, c := range candidates {
		if model.ValidateRefName(c) != nil {
			continue
		}
		if sha, ok := f.revParse(ctx, r, c); ok {
			return sha, nil
		}
	}
	return "", fmt.Errorf("%w: %s", repo.ErrRevisionNotFound, revision)
}

// Resolve implements repo.Service.
func (f *Fake) Resolve(ctx context.Context, id model.RepoID, revision string) (model.CommitSHA, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, err := f.lookup(id)
	if err != nil {
		return "", err
	}
	return f.resolve(ctx, r, revision)
}

type lsEntry struct {
	mode, typ, oid, path string
	size                 int64
}

// lsTree runs `git ls-tree -l -z` and parses the result.
func (f *Fake) lsTree(ctx context.Context, r *fakeRepo, args ...string) ([]lsEntry, error) {
	out, err := f.git.run(ctx, r.dir, nil, nil, append([]string{"ls-tree", "-l", "-z"}, args...)...)
	if err != nil {
		return nil, err
	}
	var entries []lsEntry
	for _, rec := range bytes.Split(out, []byte{0}) {
		if len(rec) == 0 {
			continue
		}
		meta, p, ok := strings.Cut(string(rec), "\t")
		if !ok {
			return nil, fmt.Errorf("unexpected ls-tree output %q", rec)
		}
		fields := strings.Fields(meta)
		if len(fields) != 4 {
			return nil, fmt.Errorf("unexpected ls-tree output %q", rec)
		}
		e := lsEntry{mode: fields[0], typ: fields[1], oid: fields[2], path: p}
		if fields[3] != "-" {
			e.size, _ = strconv.ParseInt(fields[3], 10, 64)
		}
		entries = append(entries, e)
	}
	return entries, nil
}

func (f *Fake) toEntry(ctx context.Context, r *fakeRepo, commit model.CommitSHA, e lsEntry, expand bool) (model.Entry, error) {
	out := model.Entry{Path: e.path, OID: e.oid, Present: true}
	switch e.typ {
	case "tree":
		out.Type = model.EntryDirectory
	case "blob":
		out.Type = model.EntryFile
		out.Size = e.size
		if e.size <= model.MaxLFSPointerSize {
			content, err := f.git.run(ctx, r.dir, nil, nil, "cat-file", "blob", e.oid)
			if err != nil {
				return model.Entry{}, err
			}
			if p, err := model.ParseLFSPointer(content); err == nil {
				out.LFS = &model.LFSInfo{OID: p.OID, Size: p.Size, PointerSize: len(content)}
				out.Size = p.Size
				out.Present = f.present(ctx, r, p.OID)
			}
		}
	default:
		// Submodules (commit entries) are listed as files of size 0.
		out.Type = model.EntryFile
	}
	if expand {
		lc, err := f.lastCommit(ctx, r, commit, e.path)
		if err != nil {
			return model.Entry{}, err
		}
		out.LastCommit = lc
	}
	return out, nil
}

func (f *Fake) present(ctx context.Context, r *fakeRepo, oid string) bool {
	if ref, ok := r.lfs[oid]; ok && !ref.Present {
		return false
	}
	_, err := f.LFS.Stat(ctx, oid)
	return err == nil
}

func (f *Fake) lastCommit(ctx context.Context, r *fakeRepo, commit model.CommitSHA, p string) (*model.CommitSummary, error) {
	args := []string{"log", "-1", "--format=%H%x00%s%x00%ct", string(commit)}
	if p != "" {
		args = append(args, "--", p)
	}
	out, err := f.git.out(ctx, r.dir, args...)
	if err != nil {
		return nil, err
	}
	parts := strings.SplitN(out, "\x00", 3)
	if len(parts) != 3 {
		return nil, nil
	}
	secs, _ := strconv.ParseInt(parts[2], 10, 64)
	return &model.CommitSummary{SHA: model.CommitSHA(parts[0]), Title: parts[1], Date: time.Unix(secs, 0).UTC()}, nil
}

func (f *Fake) checkCommit(ctx context.Context, r *fakeRepo, commit model.CommitSHA) error {
	if t, err := f.git.out(ctx, r.dir, "cat-file", "-t", string(commit)); err != nil || t != "commit" {
		return fmt.Errorf("%w: %s", repo.ErrRevisionNotFound, commit)
	}
	return nil
}

// entryLocked returns one entry; path "" is the root directory.
func (f *Fake) entryLocked(ctx context.Context, r *fakeRepo, commit model.CommitSHA, p string, expand bool) (model.Entry, error) {
	clean, err := model.CleanRepoPath(p)
	if err != nil {
		return model.Entry{}, fmt.Errorf("%w: %w", repo.ErrEntryNotFound, err)
	}
	if err := f.checkCommit(ctx, r, commit); err != nil {
		return model.Entry{}, err
	}
	if clean == "" {
		tree, err := f.git.out(ctx, r.dir, "rev-parse", string(commit)+"^{tree}")
		if err != nil {
			return model.Entry{}, err
		}
		return model.Entry{Path: "", Type: model.EntryDirectory, OID: tree, Present: true}, nil
	}
	entries, err := f.lsTree(ctx, r, string(commit), "--", clean)
	if err != nil {
		return model.Entry{}, err
	}
	for _, e := range entries {
		if e.path == clean {
			return f.toEntry(ctx, r, commit, e, expand)
		}
	}
	return model.Entry{}, fmt.Errorf("%w: %s", repo.ErrEntryNotFound, clean)
}

// Tree implements repo.Service. Directories are listed before files, as on the Hub.
func (f *Fake) Tree(ctx context.Context, id model.RepoID, commit model.CommitSHA, p string, opts repo.TreeOpts) ([]model.Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, err := f.lookup(id)
	if err != nil {
		return nil, err
	}
	dir, err := f.entryLocked(ctx, r, commit, p, false)
	if err != nil {
		return nil, err
	}
	if dir.Type != model.EntryDirectory {
		return nil, fmt.Errorf("%w: %s is a file", repo.ErrEntryNotFound, p)
	}
	args := []string{string(commit)}
	if opts.Recursive {
		args = []string{"-r", "-t", string(commit)}
	}
	if dir.Path != "" {
		args = append(args, "--", dir.Path+"/")
	}
	raw, err := f.lsTree(ctx, r, args...)
	if err != nil {
		return nil, err
	}
	entries := make([]model.Entry, 0, len(raw))
	for _, e := range raw {
		ent, err := f.toEntry(ctx, r, commit, e, opts.Expand)
		if err != nil {
			return nil, err
		}
		entries = append(entries, ent)
	}
	sort.SliceStable(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		if a.Type != b.Type {
			return a.Type == model.EntryDirectory
		}
		return a.Path < b.Path
	})
	if opts.After != "" {
		for i, e := range entries {
			if e.Path == opts.After {
				entries = entries[i+1:]
				break
			}
		}
	}
	if opts.Limit > 0 && len(entries) > opts.Limit {
		entries = entries[:opts.Limit]
	}
	return entries, nil
}

// Entry implements repo.Service.
func (f *Fake) Entry(ctx context.Context, id model.RepoID, commit model.CommitSHA, p string) (model.Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, err := f.lookup(id)
	if err != nil {
		return model.Entry{}, err
	}
	return f.entryLocked(ctx, r, commit, p, false)
}

// Entries implements repo.Service.
func (f *Fake) Entries(ctx context.Context, id model.RepoID, commit model.CommitSHA, paths []string, expand bool) ([]model.Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, err := f.lookup(id)
	if err != nil {
		return nil, err
	}
	if err := f.checkCommit(ctx, r, commit); err != nil {
		return nil, err
	}
	var out []model.Entry
	for _, p := range paths {
		e, err := f.entryLocked(ctx, r, commit, p, expand)
		if errors.Is(err, repo.ErrEntryNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, nil
}

// OpenFile implements repo.Service.
func (f *Fake) OpenFile(ctx context.Context, id model.RepoID, commit model.CommitSHA, p string, rng *blob.Range) (io.ReadCloser, model.Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, err := f.lookup(id)
	if err != nil {
		return nil, model.Entry{}, err
	}
	e, err := f.entryLocked(ctx, r, commit, p, false)
	if err != nil {
		return nil, model.Entry{}, err
	}
	if e.Type != model.EntryFile {
		return nil, model.Entry{}, fmt.Errorf("%w: %s is a directory", repo.ErrEntryNotFound, p)
	}
	if e.LFS != nil {
		if !e.Present {
			return nil, e, fmt.Errorf("%w: %s", repo.ErrNotPresent, p)
		}
		rc, _, err := f.LFS.Open(ctx, e.LFS.OID, rng)
		return rc, e, err
	}
	content, err := f.git.run(ctx, r.dir, nil, nil, "cat-file", "blob", e.OID)
	if err != nil {
		return nil, model.Entry{}, err
	}
	if rng != nil {
		start, end, err := rng.Resolve(int64(len(content)))
		if err != nil {
			return nil, e, err
		}
		content = content[start : end+1]
	}
	return io.NopCloser(bytes.NewReader(content)), e, nil
}

// Commit implements repo.Service.
func (f *Fake) Commit(ctx context.Context, p repo.CommitParams) (*repo.CommitResult, error) {
	if err := repo.ValidateCommitParams(p); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	r, err := f.lookup(p.Repo)
	if err != nil {
		return nil, err
	}
	return f.commitLocked(ctx, r, p)
}

func (f *Fake) commitLocked(ctx context.Context, r *fakeRepo, p repo.CommitParams) (*repo.CommitResult, error) {
	if r.Mirror && !p.AllowMirror {
		return nil, repo.ErrReadOnly
	}
	revision := p.Revision
	if revision == "" {
		revision = r.DefaultBranch
	}
	ref := model.BranchRef(revision)
	if err := model.ValidateRefName(ref); err != nil {
		return nil, fmt.Errorf("%w: %w", repo.ErrInvalid, err)
	}
	tip, exists := f.revParse(ctx, r, ref)
	parent := tip
	switch {
	case exists:
	case p.CreateFrom != "":
		parent = p.CreateFrom
	case revision == r.DefaultBranch && f.isEmpty(ctx, r):
		parent = ""
	default:
		return nil, fmt.Errorf("%w: %s", repo.ErrRevisionNotFound, revision)
	}
	if p.ParentCommit != "" && exists && !strings.HasPrefix(string(tip), strings.ToLower(string(p.ParentCommit))) {
		return nil, fmt.Errorf("%w: %s is at %s", repo.ErrParentMismatch, revision, tip)
	}

	idx := filepath.Join(f.root, "index-"+uuid.NewString())
	defer os.Remove(idx)
	env := []string{"GIT_INDEX_FILE=" + idx}
	run := func(stdin io.Reader, args ...string) ([]byte, error) {
		return f.git.run(ctx, r.dir, stdin, env, args...)
	}
	if parent != "" {
		if _, err := run(nil, "read-tree", string(parent)); err != nil {
			return nil, err
		}
	} else if _, err := run(nil, "read-tree", "--empty"); err != nil {
		return nil, err
	}
	addBlob := func(path string, content io.Reader) error {
		oid, err := run(content, "hash-object", "-w", "--stdin")
		if err != nil {
			return err
		}
		_, err = run(nil, "update-index", "--add", "--cacheinfo", "100644,"+strings.TrimSpace(string(oid))+","+path)
		return err
	}
	// Paths that .gitattributes at the parent tracks with LFS (the Hub's
	// defaults without one) are stored as LFS objects, as the real service
	// does.
	attrs := repo.DefaultGitAttributes(r.ID.Type)
	if parent != "" {
		if out, err := run(nil, "cat-file", "blob", string(parent)+":.gitattributes"); err == nil {
			attrs = out
		}
	}
	rules := repo.ParseLFSRules(attrs)
	var lfsRefs []repo.LFSRef
	for _, op := range p.Operations {
		clean, _ := model.CleanRepoPath(op.Path)
		switch op.Kind {
		case repo.OpAdd:
			content := op.Content
			if rules.IsLFS(clean) {
				b, err := io.ReadAll(op.Content)
				if err != nil {
					return nil, err
				}
				obj, err := f.LFS.Put(ctx, b)
				if err != nil {
					return nil, err
				}
				lfsRefs = append(lfsRefs, repo.LFSRef{OID: obj.OID, Size: obj.Size, Present: true})
				content = bytes.NewReader(model.LFSPointer{OID: obj.OID, Size: obj.Size}.Bytes())
			}
			if err := addBlob(clean, content); err != nil {
				return nil, err
			}
		case repo.OpAddLFS:
			obj, err := f.LFS.Stat(ctx, op.OID)
			size := op.Size
			switch {
			case err == nil && obj.Verified:
				if size == 0 {
					size = obj.Size
				}
				if size != obj.Size {
					return nil, fmt.Errorf("%w: %s has size %d, not %d", repo.ErrInvalid, op.OID, obj.Size, size)
				}
				lfsRefs = append(lfsRefs, repo.LFSRef{OID: op.OID, Size: size, Present: true})
			case r.Mirror && p.AllowMirror && size > 0:
				lfsRefs = append(lfsRefs, repo.LFSRef{OID: op.OID, Size: size, Present: false})
			default:
				return nil, fmt.Errorf("%w: %s (%s)", repo.ErrLFSObjectMissing, op.OID, op.Path)
			}
			ptr := model.LFSPointer{OID: op.OID, Size: size}
			if err := addBlob(clean, bytes.NewReader(ptr.Bytes())); err != nil {
				return nil, err
			}
		case repo.OpDelete:
			if _, err := run(nil, "ls-files", "--error-unmatch", "--", clean); err != nil {
				return nil, fmt.Errorf("%w: A file with this name doesn't exist: %s", repo.ErrEntryNotFound, clean)
			}
			if err := removeFromIndex(run, []string{clean}); err != nil {
				return nil, err
			}
		case repo.OpDeleteFolder:
			args := []string{"ls-files", "-z"}
			if clean != "" {
				args = append(args, "--", clean+"/")
			}
			out, err := run(nil, args...)
			if err != nil {
				return nil, err
			}
			files := strings.Split(strings.TrimRight(string(out), "\x00"), "\x00")
			if len(out) == 0 {
				return nil, fmt.Errorf("%w: folder %s does not exist", repo.ErrEntryNotFound, clean)
			}
			if err := removeFromIndex(run, files); err != nil {
				return nil, err
			}
		case repo.OpCopy:
			src := parent
			if op.SrcRevision != "" {
				s, err := f.resolve(ctx, r, op.SrcRevision)
				if err != nil {
					return nil, err
				}
				src = s
			}
			if src == "" {
				return nil, fmt.Errorf("%w: copy source %s does not exist", repo.ErrEntryNotFound, op.SrcPath)
			}
			srcPath, _ := model.CleanRepoPath(op.SrcPath)
			ents, err := f.lsTree(ctx, r, string(src), "--", srcPath)
			if err != nil {
				return nil, err
			}
			if len(ents) != 1 || ents[0].typ != "blob" || ents[0].path != srcPath {
				return nil, fmt.Errorf("%w: copy source %s does not exist", repo.ErrEntryNotFound, op.SrcPath)
			}
			if _, err := run(nil, "update-index", "--add", "--cacheinfo", ents[0].mode+","+ents[0].oid+","+clean); err != nil {
				return nil, err
			}
		}
	}
	treeOut, err := run(nil, "write-tree")
	if err != nil {
		return nil, err
	}
	tree := strings.TrimSpace(string(treeOut))
	if parent != "" {
		parentTree, err := f.git.out(ctx, r.dir, "rev-parse", string(parent)+"^{tree}")
		if err != nil {
			return nil, err
		}
		if parentTree == tree && exists {
			return &repo.CommitResult{SHA: parent, Parent: parent, Ref: ref, NoOp: true}, nil
		}
	}
	when := p.Time
	if when.IsZero() {
		when = f.now()
	}
	msg := p.Summary
	if p.Description != "" {
		msg += "\n\n" + p.Description
	}
	args := []string{"commit-tree", tree, "-F", "-"}
	if parent != "" {
		args = []string{"commit-tree", tree, "-p", string(parent), "-F", "-"}
	}
	out, err := f.git.run(ctx, r.dir, strings.NewReader(msg), sigEnv(signature(p.Actor, when)), args...)
	if err != nil {
		return nil, err
	}
	sha := model.CommitSHA(strings.TrimSpace(string(out)))
	old := tip
	if !exists {
		old = model.ZeroSHA
	}
	if err := f.updateRefsLocked(ctx, r, []repo.RefUpdate{{Name: ref, Old: old, New: sha}}); err != nil {
		return nil, err
	}
	for _, l := range lfsRefs {
		r.lfs[l.OID] = l
	}
	r.LastModified = when
	r.UpdatedAt = when
	return &repo.CommitResult{SHA: sha, Parent: parent, Ref: ref}, nil
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

func (f *Fake) isEmpty(ctx context.Context, r *fakeRepo) bool {
	out, err := f.git.out(ctx, r.dir, "for-each-ref", "--count=1")
	return err == nil && out == ""
}

const logFormat = "--format=%H%x1f%P%x1f%T%x1f%an%x1f%ae%x1f%at%x1f%cn%x1f%ce%x1f%ct%x1f%B%x1e"

func parseLog(out []byte) []repo.CommitInfo {
	var commits []repo.CommitInfo
	for _, rec := range bytes.Split(out, []byte{0x1e}) {
		s := strings.TrimLeft(string(rec), "\n")
		if s == "" {
			continue
		}
		f := strings.SplitN(s, "\x1f", 10)
		if len(f) != 10 {
			continue
		}
		ts := func(s string) time.Time {
			n, _ := strconv.ParseInt(s, 10, 64)
			return time.Unix(n, 0).UTC()
		}
		var parents []model.CommitSHA
		for _, p := range strings.Fields(f[1]) {
			parents = append(parents, model.CommitSHA(p))
		}
		msg := strings.TrimRight(f[9], "\n")
		title, _, _ := strings.Cut(msg, "\n")
		commits = append(commits, repo.CommitInfo{
			SHA: model.CommitSHA(f[0]), Parents: parents, Tree: f[2],
			Author:    repo.Signature{Name: f[3], Email: f[4], When: ts(f[5])},
			Committer: repo.Signature{Name: f[6], Email: f[7], When: ts(f[8])},
			Title:     title, Message: msg,
		})
	}
	return commits
}

// Log implements repo.Service.
func (f *Fake) Log(ctx context.Context, id model.RepoID, revision string, opts repo.LogOpts) ([]repo.CommitInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, err := f.lookup(id)
	if err != nil {
		return nil, err
	}
	sha, err := f.resolve(ctx, r, revision)
	if err != nil {
		return nil, err
	}
	args := []string{"log", logFormat, "--skip=" + strconv.Itoa(opts.Skip)}
	if opts.Limit > 0 {
		args = append(args, "--max-count="+strconv.Itoa(opts.Limit))
	}
	args = append(args, string(sha))
	if opts.Path != "" {
		args = append(args, "--", opts.Path)
	}
	out, err := f.git.run(ctx, r.dir, nil, nil, args...)
	if err != nil {
		return nil, err
	}
	return parseLog(out), nil
}

// GetCommit implements repo.Service.
func (f *Fake) GetCommit(ctx context.Context, id model.RepoID, sha model.CommitSHA) (*repo.CommitInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, err := f.lookup(id)
	if err != nil {
		return nil, err
	}
	if err := f.checkCommit(ctx, r, sha); err != nil {
		return nil, err
	}
	out, err := f.git.run(ctx, r.dir, nil, nil, "log", "-1", logFormat, string(sha))
	if err != nil {
		return nil, err
	}
	commits := parseLog(out)
	if len(commits) != 1 {
		return nil, fmt.Errorf("%w: %s", repo.ErrRevisionNotFound, sha)
	}
	return &commits[0], nil
}

func (f *Fake) firstParent(ctx context.Context, r *fakeRepo, sha model.CommitSHA) string {
	if p, err := f.git.out(ctx, r.dir, "rev-parse", "--verify", "--quiet", string(sha)+"^1"); err == nil && p != "" {
		return p
	}
	return emptyTree
}

func (f *Fake) blobInfo(ctx context.Context, r *fakeRepo, oid string) (size int64, lfs, binary bool, content []byte) {
	if oid == "" || strings.Trim(oid, "0") == "" {
		return 0, false, false, nil
	}
	b, err := f.git.run(ctx, r.dir, nil, nil, "cat-file", "blob", oid)
	if err != nil {
		return 0, false, false, nil
	}
	if p, err := model.ParseLFSPointer(b); err == nil {
		return p.Size, true, false, nil
	}
	probe := b
	if len(probe) > 8000 {
		probe = probe[:8000]
	}
	return int64(len(b)), false, bytes.IndexByte(probe, 0) >= 0, b
}

// Diff implements repo.Service.
func (f *Fake) Diff(ctx context.Context, id model.RepoID, sha model.CommitSHA) ([]repo.FileChange, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, err := f.lookup(id)
	if err != nil {
		return nil, err
	}
	return f.diffLocked(ctx, r, sha)
}

func (f *Fake) diffLocked(ctx context.Context, r *fakeRepo, sha model.CommitSHA) ([]repo.FileChange, error) {
	if err := f.checkCommit(ctx, r, sha); err != nil {
		return nil, err
	}
	return f.diffBetweenLocked(ctx, r, f.firstParent(ctx, r, sha), string(sha))
}

func (f *Fake) diffBetweenLocked(ctx context.Context, r *fakeRepo, from, to string) ([]repo.FileChange, error) {
	out, err := f.git.run(ctx, r.dir, nil, nil, "diff-tree", "-r", "-z", "-M", "--no-commit-id", "--raw", from, to)
	if err != nil {
		return nil, err
	}
	fields := strings.Split(strings.TrimRight(string(out), "\x00"), "\x00")
	var changes []repo.FileChange
	for i := 0; i < len(fields); i++ {
		meta := fields[i]
		if !strings.HasPrefix(meta, ":") {
			continue
		}
		m := strings.Fields(strings.TrimPrefix(meta, ":"))
		if len(m) < 5 {
			continue
		}
		c := repo.FileChange{OldOID: m[2], NewOID: m[3]}
		status := m[4]
		i++
		c.Path = fields[i]
		switch status[0] {
		case 'A':
			c.Status = repo.ChangeAdded
		case 'D':
			c.Status = repo.ChangeDeleted
		case 'R':
			c.Status = repo.ChangeRenamed
			c.OldPath = c.Path
			i++
			c.Path = fields[i]
		default:
			c.Status = repo.ChangeModified
		}
		var oldLFS, newLFS, oldBin, newBin bool
		c.OldSize, oldLFS, oldBin, _ = f.blobInfo(ctx, r, c.OldOID)
		c.NewSize, newLFS, newBin, _ = f.blobInfo(ctx, r, c.NewOID)
		c.LFS = oldLFS || newLFS
		c.Binary = oldBin || newBin
		if strings.Trim(c.OldOID, "0") == "" {
			c.OldOID = ""
		}
		if strings.Trim(c.NewOID, "0") == "" {
			c.NewOID = ""
		}
		changes = append(changes, c)
	}
	return changes, nil
}

// FileDiff implements repo.Service.
func (f *Fake) FileDiff(ctx context.Context, id model.RepoID, sha model.CommitSHA, p string) (*repo.FileDiff, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, err := f.lookup(id)
	if err != nil {
		return nil, err
	}
	if err := f.checkCommit(ctx, r, sha); err != nil {
		return nil, err
	}
	return f.fileDiffLocked(ctx, r, f.firstParent(ctx, r, sha), string(sha), p)
}

// CompareFile implements repo.Service.
func (f *Fake) CompareFile(ctx context.Context, id model.RepoID, base, head model.CommitSHA, p string) (*repo.FileDiff, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, err := f.lookup(id)
	if err != nil {
		return nil, err
	}
	for _, sha := range []model.CommitSHA{base, head} {
		if err := f.checkCommit(ctx, r, sha); err != nil {
			return nil, err
		}
	}
	return f.fileDiffLocked(ctx, r, string(base), string(head), p)
}

func (f *Fake) fileDiffLocked(ctx context.Context, r *fakeRepo, from, to, p string) (*repo.FileDiff, error) {
	changes, err := f.diffBetweenLocked(ctx, r, from, to)
	if err != nil {
		return nil, err
	}
	for _, c := range changes {
		if c.Path != p {
			continue
		}
		fd := &repo.FileDiff{FileChange: c}
		if !c.LFS && !c.Binary && c.OldSize < maxTextDiff && c.NewSize < maxTextDiff {
			args := []string{"diff", "-M", from, to, "--", p}
			if c.OldPath != "" {
				args = append(args, c.OldPath)
			}
			out, err := f.git.run(ctx, r.dir, nil, nil, args...)
			if err != nil {
				return nil, err
			}
			fd.Patch = string(out)
		}
		return fd, nil
	}
	return nil, fmt.Errorf("%w: %s did not change in %s", repo.ErrEntryNotFound, p, to)
}

// Refs implements repo.Service.
func (f *Fake) Refs(ctx context.Context, id model.RepoID) (*repo.Refs, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, err := f.lookup(id)
	if err != nil {
		return nil, err
	}
	return f.refsLocked(ctx, r)
}

func (f *Fake) refsLocked(ctx context.Context, r *fakeRepo) (*repo.Refs, error) {
	out, err := f.git.out(ctx, r.dir, "for-each-ref", "--format=%(refname)%00%(objectname)%00%(*objectname)")
	if err != nil {
		return nil, err
	}
	refs := &repo.Refs{Branches: []repo.Ref{}, Tags: []repo.Ref{}, Converts: []repo.Ref{}, PullRequests: []repo.Ref{}, Other: []repo.Ref{}}
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		parts := strings.Split(line, "\x00")
		target := parts[1]
		if len(parts) > 2 && parts[2] != "" {
			target = parts[2]
		}
		ref := repo.Ref{Name: parts[0], Target: model.CommitSHA(target)}
		switch {
		case strings.HasPrefix(ref.Name, model.RefHeads):
			refs.Branches = append(refs.Branches, ref)
		case strings.HasPrefix(ref.Name, model.RefTags):
			refs.Tags = append(refs.Tags, ref)
		case strings.HasPrefix(ref.Name, model.RefConvert):
			refs.Converts = append(refs.Converts, ref)
		case strings.HasPrefix(ref.Name, model.RefPR):
			refs.PullRequests = append(refs.PullRequests, ref)
		default:
			refs.Other = append(refs.Other, ref)
		}
	}
	return refs, nil
}

// UpdateRefs implements repo.Service with `git update-ref --stdin`, which
// applies every update or none.
func (f *Fake) UpdateRefs(ctx context.Context, id model.RepoID, updates []repo.RefUpdate) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, err := f.lookup(id)
	if err != nil {
		return err
	}
	return f.updateRefsLocked(ctx, r, updates)
}

func (f *Fake) updateRefsLocked(ctx context.Context, r *fakeRepo, updates []repo.RefUpdate) error {
	var b strings.Builder
	b.WriteString("start\n")
	for _, u := range updates {
		if err := model.ValidateRefName(u.Name); err != nil {
			return fmt.Errorf("%w: %w", repo.ErrInvalid, err)
		}
		switch {
		case u.Old.IsZero() && u.New.IsZero():
			return fmt.Errorf("%w: update of %s has neither old nor new value", repo.ErrInvalid, u.Name)
		case u.Old.IsZero():
			fmt.Fprintf(&b, "create %s %s\n", u.Name, u.New)
		case u.New.IsZero():
			fmt.Fprintf(&b, "delete %s %s\n", u.Name, u.Old)
		default:
			fmt.Fprintf(&b, "update %s %s %s\n", u.Name, u.New, u.Old)
		}
	}
	b.WriteString("prepare\ncommit\n")
	if _, err := f.git.run(ctx, r.dir, strings.NewReader(b.String()), nil, "update-ref", "--stdin"); err != nil {
		var ge *gitError
		if errors.As(err, &ge) && (strings.Contains(ge.stderr, "but expected") || strings.Contains(ge.stderr, "reference already exists") || strings.Contains(ge.stderr, "unable to resolve reference") || strings.Contains(ge.stderr, "cannot lock ref")) {
			return fmt.Errorf("%w: %s", repo.ErrRefConflict, ge.stderr)
		}
		return err
	}
	return nil
}

// CreateBranch implements repo.Service.
func (f *Fake) CreateBranch(ctx context.Context, id model.RepoID, branch, from string, actor model.Principal) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, err := f.lookup(id)
	if err != nil {
		return err
	}
	if r.Mirror {
		return repo.ErrReadOnly
	}
	ref := model.RefHeads + branch
	if _, ok := f.revParse(ctx, r, ref); ok {
		return fmt.Errorf("%w: %s", repo.ErrRefExists, branch)
	}
	sha, err := f.resolve(ctx, r, from)
	if err != nil {
		return err
	}
	return f.updateRefsLocked(ctx, r, []repo.RefUpdate{{Name: ref, New: sha}})
}

// DeleteBranch implements repo.Service.
func (f *Fake) DeleteBranch(ctx context.Context, id model.RepoID, branch string, actor model.Principal) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, err := f.lookup(id)
	if err != nil {
		return err
	}
	if r.Mirror {
		return repo.ErrReadOnly
	}
	if branch == r.DefaultBranch {
		return fmt.Errorf("%w: the default branch cannot be deleted", repo.ErrProtectedRef)
	}
	ref := model.RefHeads + branch
	sha, ok := f.revParse(ctx, r, ref)
	if !ok {
		return fmt.Errorf("%w: %s", repo.ErrRevisionNotFound, branch)
	}
	return f.updateRefsLocked(ctx, r, []repo.RefUpdate{{Name: ref, Old: sha}})
}

// CreateTag implements repo.Service.
func (f *Fake) CreateTag(ctx context.Context, p repo.TagParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, err := f.lookup(p.Repo)
	if err != nil {
		return err
	}
	if r.Mirror {
		return repo.ErrReadOnly
	}
	ref := model.RefTags + p.Tag
	if err := model.ValidateRefName(ref); err != nil {
		return fmt.Errorf("%w: %w", repo.ErrInvalid, err)
	}
	if _, err := f.git.out(ctx, r.dir, "rev-parse", "--verify", "--quiet", ref); err == nil {
		return fmt.Errorf("%w: %s", repo.ErrRefExists, p.Tag)
	}
	sha, err := f.resolve(ctx, r, p.Revision)
	if err != nil {
		return err
	}
	if p.Message == "" {
		return f.updateRefsLocked(ctx, r, []repo.RefUpdate{{Name: ref, New: sha}})
	}
	_, err = f.git.run(ctx, r.dir, nil, sigEnv(signature(p.Actor, f.now())), "tag", "-a", "-m", p.Message, p.Tag, string(sha))
	return err
}

// DeleteTag implements repo.Service.
func (f *Fake) DeleteTag(ctx context.Context, id model.RepoID, tag string, actor model.Principal) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, err := f.lookup(id)
	if err != nil {
		return err
	}
	if r.Mirror {
		return repo.ErrReadOnly
	}
	ref := model.RefTags + tag
	target, err := f.git.out(ctx, r.dir, "rev-parse", "--verify", "--quiet", ref)
	if err != nil || target == "" {
		return fmt.Errorf("%w: %s", repo.ErrRevisionNotFound, tag)
	}
	return f.updateRefsLocked(ctx, r, []repo.RefUpdate{{Name: ref, Old: model.CommitSHA(target)}})
}

// ApplyPack implements repo.Service. It indexes the pack into the Repo with
// `git index-pack --fix-thin`, checks the push policy, and then applies the
// ref updates atomically.
func (f *Fake) ApplyPack(ctx context.Context, p repo.ApplyPackParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, err := f.lookup(p.Repo)
	if err != nil {
		return err
	}
	if p.Source == repo.SourcePush && r.Mirror {
		return repo.ErrReadOnly
	}
	if p.Pack != nil {
		if _, err := f.git.run(ctx, r.dir, p.Pack, nil, "index-pack", "--stdin", "--fix-thin"); err != nil {
			return fmt.Errorf("%w: unpack failed: %w", repo.ErrInvalid, err)
		}
	}
	if p.Source == repo.SourcePush {
		for _, u := range p.Updates {
			if !strings.HasPrefix(u.Name, model.RefHeads) && !strings.HasPrefix(u.Name, model.RefTags) {
				return &repo.PolicyError{Ref: u.Name, Message: "only refs/heads/* and refs/tags/* can be pushed"}
			}
			if u.Name == model.BranchRef(r.DefaultBranch) {
				if u.New.IsZero() {
					return &repo.PolicyError{Ref: u.Name, Message: "the default branch cannot be deleted"}
				}
				if !u.Old.IsZero() && !p.AllowForce {
					if _, err := f.git.run(ctx, r.dir, nil, nil, "merge-base", "--is-ancestor", string(u.Old), string(u.New)); err != nil {
						return &repo.PolicyError{Ref: u.Name, Message: "force-pushing the default branch is not allowed"}
					}
				}
			}
		}
	}
	if err := f.updateRefsLocked(ctx, r, p.Updates); err != nil {
		return err
	}
	r.LastModified = f.now()
	return nil
}

// OpenView implements repo.Service. The fake's Repos are already bare
// repositories, so the view is the Repo itself and Close does nothing.
func (f *Fake) OpenView(_ context.Context, id model.RepoID) (*repo.GitView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, err := f.lookup(id)
	if err != nil {
		return nil, err
	}
	return repo.NewGitView(r.dir, func() error { return nil }), nil
}

func (f *Fake) move(id model.RepoID, to model.RepoID) (*repo.Repo, error) {
	if err := to.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %w", repo.ErrInvalid, err)
	}
	r, err := f.lookup(id)
	if err != nil {
		return nil, err
	}
	if other, ok := f.names[keyOf(to)]; ok && other != r.UUID {
		return nil, fmt.Errorf("%w: %s", repo.ErrExists, to.Path())
	}
	delete(f.names, keyOf(r.ID))
	f.redirects[keyOf(r.ID)] = r.UUID
	delete(f.redirects, keyOf(to))
	r.ID = to
	r.NamespaceID = f.namespaceID(to.Namespace, r.NamespaceID)
	r.UpdatedAt = f.now()
	f.names[keyOf(to)] = r.UUID
	return copyRepo(r), nil
}

// Rename implements repo.Service.
func (f *Fake) Rename(_ context.Context, id model.RepoID, newName string, _ model.Principal) (*repo.Repo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, err := f.lookup(id)
	if err != nil {
		return nil, err
	}
	return f.move(r.ID, model.RepoID{Type: r.ID.Type, Namespace: r.ID.Namespace, Name: newName})
}

// Transfer implements repo.Service.
func (f *Fake) Transfer(_ context.Context, id model.RepoID, newNamespace string, _ model.Principal) (*repo.Repo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, err := f.lookup(id)
	if err != nil {
		return nil, err
	}
	return f.move(r.ID, model.RepoID{Type: r.ID.Type, Namespace: newNamespace, Name: r.ID.Name})
}

// Delete implements repo.Service.
func (f *Fake) Delete(_ context.Context, id model.RepoID, _ model.Principal) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, err := f.lookup(id)
	if err != nil {
		return err
	}
	now := f.now()
	r.DeletedAt = &now
	delete(f.names, keyOf(r.ID))
	return nil
}

// Restore implements repo.Service.
func (f *Fake) Restore(_ context.Context, id model.RepoID, _ model.Principal) (*repo.Repo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var found *fakeRepo
	for _, r := range f.repos {
		if r.DeletedAt != nil && keyOf(r.ID) == keyOf(id) && (found == nil || r.DeletedAt.After(*found.DeletedAt)) {
			found = r
		}
	}
	if found == nil {
		return nil, fmt.Errorf("%w: no deleted repo %s", repo.ErrNotFound, id.Path())
	}
	if _, taken := f.names[keyOf(id)]; taken {
		return nil, fmt.Errorf("%w: %s was created again after it was deleted", repo.ErrExists, id.Path())
	}
	found.DeletedAt = nil
	f.names[keyOf(found.ID)] = found.UUID
	return copyRepo(found), nil
}

// Purge implements repo.Service.
func (f *Fake) Purge(_ context.Context, id uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.repos[id]
	if !ok {
		return fmt.Errorf("%w: %s", repo.ErrNotFound, id)
	}
	if r.DeletedAt == nil {
		return fmt.Errorf("%w: delete the repo before purging it", repo.ErrInvalid)
	}
	delete(f.repos, id)
	for k, v := range f.redirects {
		if v == id {
			delete(f.redirects, k)
		}
	}
	return os.RemoveAll(r.dir)
}

// UpdateSettings implements repo.Service.
func (f *Fake) UpdateSettings(ctx context.Context, id model.RepoID, u repo.SettingsUpdate, _ model.Principal) (*repo.Repo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, err := f.lookup(id)
	if err != nil {
		return nil, err
	}
	if u.Visibility != nil && !u.Visibility.Valid() {
		return nil, fmt.Errorf("%w: visibility %q", repo.ErrInvalid, *u.Visibility)
	}
	if u.DefaultBranch != nil {
		if _, ok := f.revParse(ctx, r, model.RefHeads+*u.DefaultBranch); !ok {
			return nil, fmt.Errorf("%w: branch %s", repo.ErrRevisionNotFound, *u.DefaultBranch)
		}
	}
	if u.Description != nil {
		r.Description = *u.Description
	}
	if u.Visibility != nil {
		r.Visibility = *u.Visibility
	}
	if u.DefaultBranch != nil {
		r.DefaultBranch = *u.DefaultBranch
		r.HeadRef = model.RefHeads + *u.DefaultBranch
		if _, err := f.git.run(ctx, r.dir, nil, nil, "symbolic-ref", "HEAD", r.HeadRef); err != nil {
			return nil, err
		}
	}
	if u.Tags != nil {
		r.Tags = slices.Clone(*u.Tags)
	}
	if u.Licence != nil {
		r.Licence = *u.Licence
	}
	if u.Gating != nil {
		if g := u.Gating.Mode; g != "" && g != "auto" && g != "manual" {
			return nil, fmt.Errorf("%w: gating mode %q", repo.ErrInvalid, g)
		}
		r.Gating = *u.Gating
	}
	r.UpdatedAt = f.now()
	return copyRepo(r), nil
}

// SetMirrorState implements repo.Service.
func (f *Fake) SetMirrorState(_ context.Context, id model.RepoID, mirror bool) error {
	return f.SetMirror(id, mirror)
}

// CountByNamespace implements repo.Service.
func (f *Fake) CountByNamespace(_ context.Context, namespaceID uuid.UUID, viewer model.Principal) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.repos {
		if r.DeletedAt == nil && r.NamespaceID == namespaceID && visibleTo(&r.Repo, viewer) {
			n++
		}
	}
	return n, nil
}

// SetLFSPresence implements repo.Service.
func (f *Fake) SetLFSPresence(_ context.Context, id model.RepoID, refs []repo.LFSRef) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, err := f.lookup(id)
	if err != nil {
		return err
	}
	for _, l := range refs {
		if !model.IsSHA256(l.OID) {
			return fmt.Errorf("%w: oid %q", repo.ErrInvalid, l.OID)
		}
		r.lfs[l.OID] = l
	}
	return nil
}

// LFSRefs implements repo.Service. The fake records references when a
// Commit adds an LFS file or SetLFSPresence is called; objects referenced
// by packs from ApplyPack are found by scanning every ref's tree.
func (f *Fake) LFSRefs(ctx context.Context, id model.RepoID, oids []string) (map[string]repo.LFSRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, err := f.lookup(id)
	if err != nil {
		return nil, err
	}
	refs, err := f.refsLocked(ctx, r)
	if err != nil {
		return nil, err
	}
	known := map[string]repo.LFSRef{}
	for oid, ref := range r.lfs {
		known[oid] = ref
	}
	for _, group := range [][]repo.Ref{refs.Branches, refs.Tags, refs.PullRequests, refs.Converts} {
		for _, ref := range group {
			entries, err := f.lsTree(ctx, r, "-r", string(ref.Target))
			if err != nil {
				return nil, err
			}
			for _, e := range entries {
				if e.typ != "blob" || e.size > model.MaxLFSPointerSize {
					continue
				}
				content, err := f.git.run(ctx, r.dir, nil, nil, "cat-file", "blob", e.oid)
				if err != nil {
					return nil, err
				}
				if p, err := model.ParseLFSPointer(content); err == nil {
					if _, ok := known[p.OID]; !ok {
						known[p.OID] = repo.LFSRef{OID: p.OID, Size: p.Size, Present: f.present(ctx, r, p.OID)}
					}
				}
			}
		}
	}
	out := map[string]repo.LFSRef{}
	for _, oid := range oids {
		if ref, ok := known[oid]; ok {
			ref.Present = ref.Present && f.present(ctx, r, oid)
			out[oid] = ref
		}
	}
	return out, nil
}
