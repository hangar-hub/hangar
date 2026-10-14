package repo

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"

	"github.com/jamesagarside/hangar/internal/db"
	"github.com/jamesagarside/hangar/internal/gitstore"
	"github.com/jamesagarside/hangar/internal/model"
)

// The tree_entries cache holds every path of a Commit, so the Hub API's hot
// paths (tree listings, paths-info, resolve) never read git objects. A row
// with the empty path is the root and is written last in the same
// transaction, so its presence means the Commit is fully cached.
//
// last_commit is filled when the cache is built: paths whose object is the
// same as in the first parent inherit the parent's value when the parent is
// cached, paths that changed get the Commit itself, and the rest are left
// empty and computed on first use.

const rootPath = ""

// lsEntry is one line of `git ls-tree -l -z`.
type lsEntry struct {
	mode, typ, oid, path string
	size                 int64
}

// lsTree runs `git ls-tree -l -z` in dir and parses the result.
func lsTree(ctx context.Context, g gitstore.Git, dir string, args ...string) ([]lsEntry, error) {
	out, err := g.Run(ctx, dir, nil, nil, append([]string{"ls-tree", "-l", "-z"}, args...)...)
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
		f := strings.Fields(meta)
		if len(f) != 4 {
			return nil, fmt.Errorf("unexpected ls-tree output %q", rec)
		}
		e := lsEntry{mode: f[0], typ: f[1], oid: f[2], path: p}
		if f[3] != "-" {
			e.size, _ = strconv.ParseInt(f[3], 10, 64)
		}
		entries = append(entries, e)
	}
	return entries, nil
}

func parentDir(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[:i]
	}
	return ""
}

// isCommit reports whether sha is a Commit in the Repo's objects.
func (s *Store) isCommit(ctx context.Context, objs *gitstore.Objects, sha string) bool {
	if !model.IsFullSHA(sha) {
		return false
	}
	info, err := s.git.Git().BatchCheck(ctx, objs.Dir, []string{sha})
	return err == nil && len(info) == 1 && !info[0].Missing && info[0].Type == "commit"
}

func (s *Store) treeCached(ctx context.Context, idb bun.IDB, repoID uuid.UUID, commit model.CommitSHA) (bool, error) {
	return idb.NewSelect().Model((*db.TreeEntry)(nil)).Where("repo_id = ?", repoID).Where("commit_sha = ?", string(commit)).
		Where("path = ?", rootPath).Exists(ctx)
}

// ensureTree builds the tree_entries cache of a Commit if it is missing.
// ErrRevisionNotFound if the Commit is not in the Repo.
func (s *Store) ensureTree(ctx context.Context, repoID uuid.UUID, commit model.CommitSHA) error {
	if !model.IsFullSHA(string(commit)) {
		return fmt.Errorf("%w: %s", ErrRevisionNotFound, commit)
	}
	ok, err := s.treeCached(ctx, s.db, repoID, commit)
	if err != nil {
		return fmt.Errorf("repo: read tree cache: %w", err)
	}
	if ok {
		return nil
	}
	_, err, _ = s.fills.Do(repoID.String()+":"+string(commit), func() (any, error) {
		return nil, s.fillTree(context.WithoutCancel(ctx), repoID, commit)
	})
	return err
}

func (s *Store) fillTree(ctx context.Context, repoID uuid.UUID, commit model.CommitSHA) error {
	if ok, err := s.treeCached(ctx, s.db, repoID, commit); err != nil || ok {
		return err
	}
	objs, err := s.git.Acquire(ctx, repoID)
	if err != nil {
		return err
	}
	defer objs.Release()
	g := s.git.Git()
	if !s.isCommit(ctx, objs, string(commit)) {
		return fmt.Errorf("%w: %s", ErrRevisionNotFound, commit)
	}
	rootTree, err := g.Output(ctx, objs.Dir, "rev-parse", string(commit)+"^{tree}")
	if err != nil {
		return err
	}
	entries, err := lsTree(ctx, g, objs.Dir, "-r", "-t", string(commit))
	if err != nil {
		return err
	}
	pointers, err := readPointers(ctx, g, objs.Dir, entries)
	if err != nil {
		return err
	}
	// Last Commits: inherit from the first parent where nothing changed.
	parent, _ := g.Output(ctx, objs.Dir, "rev-parse", "--verify", "--quiet", string(commit)+"^1")
	var parentOIDs map[string]string
	var parentLast map[string]string
	if parent != "" {
		pe, err := lsTree(ctx, g, objs.Dir, "-r", "-t", parent)
		if err != nil {
			return err
		}
		parentOIDs = make(map[string]string, len(pe))
		for _, e := range pe {
			parentOIDs[e.path] = e.oid
		}
		if ok, _ := s.treeCached(ctx, s.db, repoID, model.CommitSHA(parent)); ok {
			var rows []db.TreeEntry
			if err := s.db.NewSelect().Model(&rows).Column("path", "last_commit").Where("repo_id = ?", repoID).
				Where("commit_sha = ?", parent).Scan(ctx); err != nil {
				return err
			}
			parentLast = make(map[string]string, len(rows))
			for _, r := range rows {
				if r.LastCommit != nil {
					parentLast[r.Path] = *r.LastCommit
				}
			}
		}
	}
	now := db.Now()
	rows := make([]db.TreeEntry, 0, len(entries)+1)
	for _, e := range entries {
		row := db.TreeEntry{RepoID: repoID, Commit: string(commit), Path: e.path, Parent: parentDir(e.path), GitOID: e.oid,
			Timestamps: db.Timestamps{CreatedAt: now, UpdatedAt: now}}
		if e.typ == "tree" {
			row.Type = string(model.EntryDirectory)
		} else {
			row.Type = string(model.EntryFile)
			row.Size = e.size
			if p, ok := pointers[e.oid]; ok {
				oid, lsize, psize := p.OID, p.Size, p.PointerSize()
				row.LFSOID, row.LFSSize, row.PointerSize = &oid, &lsize, &psize
				row.Size = p.Size
			}
		}
		switch {
		case parent == "":
			c := string(commit)
			row.LastCommit = &c
		case parentOIDs[e.path] != e.oid:
			c := string(commit)
			row.LastCommit = &c
		default:
			if lc, ok := parentLast[e.path]; ok {
				row.LastCommit = &lc
			}
		}
		rows = append(rows, row)
	}
	rootRow := db.TreeEntry{RepoID: repoID, Commit: string(commit), Path: rootPath, Parent: rootPath, Type: string(model.EntryDirectory),
		GitOID: rootTree, Timestamps: db.Timestamps{CreatedAt: now, UpdatedAt: now}}
	return s.db.InTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		for start := 0; start < len(rows); start += 500 {
			chunk := rows[start:min(start+500, len(rows))]
			if _, err := tx.NewInsert().Model(&chunk).On("CONFLICT DO NOTHING").Exec(ctx); err != nil {
				return fmt.Errorf("repo: fill tree cache: %w", err)
			}
		}
		_, err := tx.NewInsert().Model(&rootRow).On("CONFLICT DO NOTHING").Exec(ctx)
		return err
	})
}

// readPointers reads every blob small enough to be an LFS pointer and
// returns the pointers by blob OID.
func readPointers(ctx context.Context, g gitstore.Git, dir string, entries []lsEntry) (map[string]model.LFSPointer, error) {
	seen := map[string]bool{}
	var small []string
	for _, e := range entries {
		if e.typ == "blob" && e.size > 0 && e.size <= model.MaxLFSPointerSize && !seen[e.oid] {
			seen[e.oid] = true
			small = append(small, e.oid)
		}
	}
	out := map[string]model.LFSPointer{}
	err := g.BatchRead(ctx, dir, small, func(info gitstore.ObjectInfo, content []byte) error {
		if info.Missing {
			return nil
		}
		if p, err := model.ParseLFSPointer(content); err == nil {
			out[info.OID] = p
		}
		return nil
	})
	return out, err
}

// lastCommits finds, for each path, the newest Commit reachable from
// commit that changed it, with one `git log` walk that stops as soon as
// every path is found.
func lastCommits(ctx context.Context, g gitstore.Git, dir string, commit model.CommitSHA, paths []string) (map[string]model.CommitSummary, error) {
	out := map[string]model.CommitSummary{}
	if len(paths) == 0 {
		return out, nil
	}
	ctx, cancel := g.WithTimeout(ctx)
	defer cancel()
	args := append([]string{"--literal-pathspecs", "log", "--format=%x1e%H%x1f%ct%x1f%s", "--name-only", "-z", "--no-renames", string(commit), "--"}, paths...)
	cmd := g.Command(ctx, dir, nil, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	pending := map[string]bool{}
	for _, p := range paths {
		pending[p] = true
	}
	br := bufio.NewReader(stdout)
	var cur model.CommitSummary
	readErr := func() error {
		for len(pending) > 0 {
			tok, err := br.ReadString(0)
			if errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil {
				return err
			}
			tok = strings.TrimSuffix(tok, "\x00")
			if i := strings.IndexByte(tok, 0x1e); i >= 0 {
				f := strings.SplitN(tok[i+1:], "\x1f", 3)
				if len(f) != 3 {
					return fmt.Errorf("unexpected log output %q", tok)
				}
				secs, _ := strconv.ParseInt(f[1], 10, 64)
				cur = model.CommitSummary{SHA: model.CommitSHA(f[0]), Title: f[2], Date: time.Unix(secs, 0).UTC()}
				continue
			}
			name := strings.TrimPrefix(tok, "\n")
			if name == "" || cur.SHA == "" {
				continue
			}
			for p := range pending {
				if name == p || strings.HasPrefix(name, p+"/") {
					out[p] = cur
					delete(pending, p)
				}
			}
		}
		return nil
	}()
	_ = cmd.Process.Kill()
	_, _ = io.Copy(io.Discard, stdout)
	_ = cmd.Wait()
	if readErr != nil {
		return nil, readErr
	}
	return out, nil
}

// commitRow converts parsed commit information into a commits row.
func commitRow(repoID uuid.UUID, c CommitInfo) db.Commit {
	parents := make([]string, 0, len(c.Parents))
	for _, p := range c.Parents {
		parents = append(parents, string(p))
	}
	return db.Commit{RepoID: repoID, SHA: string(c.SHA), Tree: c.Tree, Parents: parents,
		Author:    db.GitSignature{Name: c.Author.Name, Email: c.Author.Email, When: c.Author.When.UTC()},
		Committer: db.GitSignature{Name: c.Committer.Name, Email: c.Committer.Email, When: c.Committer.When.UTC()},
		Title:     c.Title, Message: c.Message, AuthoredAt: c.Author.When.UTC(), CommittedAt: c.Committer.When.UTC()}
}

func fromCommitRow(r *db.Commit) *CommitInfo {
	c := &CommitInfo{SHA: model.CommitSHA(r.SHA), Tree: r.Tree, Title: r.Title, Message: r.Message,
		Author:    Signature{Name: r.Author.Name, Email: r.Author.Email, When: r.AuthoredAt.UTC()},
		Committer: Signature{Name: r.Committer.Name, Email: r.Committer.Email, When: r.CommittedAt.UTC()},
		Parents:   []model.CommitSHA{}}
	for _, p := range r.Parents {
		c.Parents = append(c.Parents, model.CommitSHA(p))
	}
	return c
}

// maxCommitFill bounds how many Commits one ref move adds to the commits
// cache; older Commits are added when first read.
const maxCommitFill = 20000

// fillCommits adds the Commits reachable from tips, and not from
// excludes, to the commits cache.
func (s *Store) fillCommits(ctx context.Context, repoID uuid.UUID, dir string, tips, excludes []string) error {
	if len(tips) == 0 {
		return nil
	}
	args := []string{"log", logFormat, "--max-count=" + strconv.Itoa(maxCommitFill)}
	args = append(args, tips...)
	if len(excludes) > 0 {
		args = append(args, "--not")
		args = append(args, excludes...)
	}
	out, err := s.git.Git().Run(ctx, dir, nil, nil, args...)
	if err != nil {
		return err
	}
	commits := parseLog(out)
	return s.insertCommits(ctx, repoID, commits)
}

func (s *Store) insertCommits(ctx context.Context, repoID uuid.UUID, commits []CommitInfo) error {
	now := db.Now()
	for start := 0; start < len(commits); start += 500 {
		chunk := commits[start:min(start+500, len(commits))]
		rows := make([]db.Commit, 0, len(chunk))
		for _, c := range chunk {
			r := commitRow(repoID, c)
			r.CreatedAt, r.UpdatedAt = now, now
			rows = append(rows, r)
		}
		if _, err := s.db.NewInsert().Model(&rows).On("CONFLICT DO NOTHING").Exec(ctx); err != nil {
			return fmt.Errorf("repo: fill commit cache: %w", err)
		}
	}
	return nil
}

const logFormat = "--format=%H%x1f%P%x1f%T%x1f%an%x1f%ae%x1f%at%x1f%cn%x1f%ce%x1f%ct%x1f%B%x1e"

// parseLog parses `git log` output in logFormat.
func parseLog(out []byte) []CommitInfo {
	var commits []CommitInfo
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
		parents := []model.CommitSHA{}
		for _, p := range strings.Fields(f[1]) {
			parents = append(parents, model.CommitSHA(p))
		}
		msg := strings.TrimRight(f[9], "\n")
		title, _, _ := strings.Cut(msg, "\n")
		commits = append(commits, CommitInfo{
			SHA: model.CommitSHA(f[0]), Parents: parents, Tree: f[2],
			Author:    Signature{Name: f[3], Email: f[4], When: ts(f[5])},
			Committer: Signature{Name: f[6], Email: f[7], When: ts(f[8])},
			Title:     title, Message: msg,
		})
	}
	return commits
}
