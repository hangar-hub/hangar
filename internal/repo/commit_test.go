package repo_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/blob"
	"github.com/jamesagarside/hangar/internal/jobs"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/repo"
	"github.com/jamesagarside/hangar/internal/repo/repotest"
	"github.com/jamesagarside/hangar/internal/webhook"
)

// TestCommitsAreByteIdenticalToGit is W2's main definition of done: the
// same operations through the Repo service and through git itself (the
// repotest fake drives git plumbing directly) give the same Commit SHAs,
// and an exported copy passes git fsck.
func TestCommitsAreByteIdenticalToGit(t *testing.T) {
	forEach(t, func(t *testing.T, e *env) {
		alice := e.addUser("alice")
		fake := repotest.NewFake(t)
		fake.SetClock(e.clock.Now)
		id := model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "tiny"}
		params := repo.CreateParams{ID: id, Visibility: model.VisibilityPublic, Actor: alice, InitialReadme: true, Licence: "apache-2.0", Description: "A tiny model."}
		e.create(params)
		_, err := fake.Create(e.ctx, params)
		require.NoError(t, err)

		weights := []byte("not really weights")
		oid := e.putLFS(weights)
		_, err = fake.LFS.Put(e.ctx, weights)
		require.NoError(t, err)

		steps := [][]repo.Operation{
			{add("config.json", `{"model_type":"bert"}`), add("nested/dir/notes.txt", "hello\n")},
			{{Kind: repo.OpAddLFS, Path: "model.safetensors", OID: oid, Size: int64(len(weights))}, add("empty.txt", "")},
			{{Kind: repo.OpCopy, Path: "nested/copy.txt", SrcPath: "nested/dir/notes.txt"}, {Kind: repo.OpDelete, Path: "config.json"}},
			{{Kind: repo.OpDeleteFolder, Path: "nested/dir"}, add("README.md", "# Replaced\n")},
		}
		for i, ops := range steps {
			e.clock.Add(61 * time.Second)
			p := repo.CommitParams{Repo: id, Summary: fmt.Sprintf("step %d", i), Description: "details", Operations: ops, Actor: alice}
			got, err := e.svc.Commit(e.ctx, cloneOps(p))
			require.NoError(t, err)
			want, err := fake.Commit(e.ctx, cloneOps(p))
			require.NoError(t, err)
			require.Equal(t, want.SHA, got.SHA, "step %d", i)
			require.Equal(t, want.Parent, got.Parent)
		}

		dir := e.export(id)
		gitCmd(t, dir, "fsck", "--strict", "--no-dangling")
		head, err := e.svc.Resolve(e.ctx, id, "main")
		require.NoError(t, err)
		require.Equal(t, string(head), gitCmd(t, dir, "rev-parse", "refs/heads/main"))
		require.Equal(t, "# Replaced\n", gitCmd(t, dir, "cat-file", "blob", "main:README.md")+"\n")
		ptr := gitCmd(t, dir, "cat-file", "blob", "main:model.safetensors") + "\n"
		require.Equal(t, string(model.LFSPointer{OID: oid, Size: int64(len(weights))}.Bytes()), ptr)
		fakeDir, err := fake.Dir(id)
		require.NoError(t, err)
		require.Equal(t, gitCmd(t, fakeDir, "cat-file", "-p", string(head)), gitCmd(t, dir, "cat-file", "-p", string(head)))
		require.Equal(t, gitCmd(t, fakeDir, "ls-tree", "-r", string(head)), gitCmd(t, dir, "ls-tree", "-r", string(head)))
	})
}

// cloneOps gives each run its own content readers.
func cloneOps(p repo.CommitParams) repo.CommitParams {
	ops := make([]repo.Operation, len(p.Operations))
	for i, op := range p.Operations {
		if op.Content != nil {
			if sr, ok := op.Content.(*strings.Reader); ok {
				b, _ := io.ReadAll(io.NewSectionReader(sr, 0, sr.Size()))
				op.Content = strings.NewReader(string(b))
			}
		}
		ops[i] = op
	}
	p.Operations = ops
	return p
}

func TestCommitReadsAndCaches(t *testing.T) {
	forEach(t, func(t *testing.T, e *env) {
		alice := e.addUser("alice")
		id := model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "files"}
		e.create(repo.CreateParams{ID: id, Actor: alice})
		first := e.commit(id, alice, add("a/b/c.txt", "c"), add("a/d.txt", "d"), add("z.txt", "z"), add("b.bin", "tiny but lfs"))
		require.False(t, first.NoOp)

		// .gitattributes from the initial Commit tracks *.bin with LFS.
		e2, err := e.svc.Entry(e.ctx, id, first.SHA, "b.bin")
		require.NoError(t, err)
		require.NotNil(t, e2.LFS)
		require.Equal(t, sha256Hex([]byte("tiny but lfs")), e2.LFS.OID)
		require.Equal(t, int64(len("tiny but lfs")), e2.Size)
		require.True(t, e2.Present)
		require.Equal(t, "tiny but lfs", e.read(id, "main", "b.bin"))

		root, err := e.svc.Tree(e.ctx, id, first.SHA, "", repo.TreeOpts{})
		require.NoError(t, err)
		require.Equal(t, []string{"a", ".gitattributes", "b.bin", "z.txt"}, paths(root))
		rec, err := e.svc.Tree(e.ctx, id, first.SHA, "a", repo.TreeOpts{Recursive: true})
		require.NoError(t, err)
		require.Equal(t, []string{"a/b", "a/b/c.txt", "a/d.txt"}, paths(rec))
		page, err := e.svc.Tree(e.ctx, id, first.SHA, "", repo.TreeOpts{After: "a", Limit: 2})
		require.NoError(t, err)
		require.Equal(t, []string{".gitattributes", "b.bin"}, paths(page))
		page, err = e.svc.Tree(e.ctx, id, first.SHA, "", repo.TreeOpts{After: "b.bin", Limit: 2})
		require.NoError(t, err)
		require.Equal(t, []string{"z.txt"}, paths(page))
		_, err = e.svc.Tree(e.ctx, id, first.SHA, "z.txt", repo.TreeOpts{})
		require.ErrorIs(t, err, repo.ErrEntryNotFound)
		_, err = e.svc.Tree(e.ctx, id, first.SHA, "missing", repo.TreeOpts{})
		require.ErrorIs(t, err, repo.ErrEntryNotFound)

		second := e.commit(id, alice, add("z.txt", "zz"))
		expanded, err := e.svc.Tree(e.ctx, id, second.SHA, "", repo.TreeOpts{Expand: true})
		require.NoError(t, err)
		last := map[string]model.CommitSHA{}
		for _, en := range expanded {
			require.NotNil(t, en.LastCommit, en.Path)
			last[en.Path] = en.LastCommit.SHA
		}
		require.Equal(t, second.SHA, last["z.txt"])
		require.Equal(t, first.SHA, last["a"])
		require.Equal(t, first.SHA, last["b.bin"])

		ents, err := e.svc.Entries(e.ctx, id, second.SHA, []string{"z.txt", "missing", "a/b", "../x"}, true)
		require.NoError(t, err)
		require.Equal(t, []string{"z.txt", "a/b"}, paths(ents))
		require.Equal(t, model.EntryDirectory, ents[1].Type)
		require.Equal(t, first.SHA, ents[1].LastCommit.SHA)

		// Ranges of regular files.
		rc, en, err := e.svc.OpenFile(e.ctx, id, first.SHA, "a/d.txt", nil)
		require.NoError(t, err)
		require.Equal(t, model.GitBlobID([]byte("d")), en.OID)
		rc.Close()
		e.commit(id, alice, add("long.txt", "0123456789"))
		head, _ := e.svc.Resolve(e.ctx, id, "")
		rc, _, err = e.svc.OpenFile(e.ctx, id, head, "long.txt", &blob.Range{Start: 3, End: 5})
		require.NoError(t, err)
		b, _ := io.ReadAll(rc)
		rc.Close()
		require.Equal(t, "345", string(b))
		_, _, err = e.svc.OpenFile(e.ctx, id, head, "a", nil)
		require.ErrorIs(t, err, repo.ErrEntryNotFound)

		// History.
		log, err := e.svc.Log(e.ctx, id, "main", repo.LogOpts{})
		require.NoError(t, err)
		require.Len(t, log, 4)
		require.Equal(t, "initial commit", log[3].Title)
		log, err = e.svc.Log(e.ctx, id, "main", repo.LogOpts{Path: "z.txt"})
		require.NoError(t, err)
		require.Equal(t, []model.CommitSHA{second.SHA, first.SHA}, shas(log))
		log, err = e.svc.Log(e.ctx, id, "main", repo.LogOpts{Skip: 1, Limit: 1})
		require.NoError(t, err)
		require.Equal(t, []model.CommitSHA{second.SHA}, shas(log))
		c, err := e.svc.GetCommit(e.ctx, id, second.SHA)
		require.NoError(t, err)
		require.Equal(t, "update", c.Title)
		require.Equal(t, "Alice", c.Author.Name)
		require.Equal(t, []model.CommitSHA{first.SHA}, c.Parents)

		diff, err := e.svc.Diff(e.ctx, id, first.SHA)
		require.NoError(t, err)
		require.Len(t, diff, 4)
		for _, ch := range diff {
			require.Equal(t, repo.ChangeAdded, ch.Status)
			if ch.Path == "b.bin" {
				require.True(t, ch.LFS)
				require.Equal(t, int64(12), ch.NewSize)
			}
		}
		fd, err := e.svc.FileDiff(e.ctx, id, second.SHA, "z.txt")
		require.NoError(t, err)
		require.Equal(t, repo.ChangeModified, fd.Status)
		require.Contains(t, fd.Patch, "-z\n")
		require.Contains(t, fd.Patch, "+zz\n")
		_, err = e.svc.FileDiff(e.ctx, id, second.SHA, "a/d.txt")
		require.ErrorIs(t, err, repo.ErrEntryNotFound)

		// Index and scan Jobs for every new branch head, and repo.updated events.
		var kinds []jobs.Kind
		js, _, err := e.jobs.List(e.ctx, jobs.Filter{})
		require.NoError(t, err)
		for _, j := range js {
			kinds = append(kinds, j.Kind)
		}
		require.Contains(t, kinds, jobs.KindIndex)
		require.Contains(t, kinds, jobs.KindScan)
		require.Contains(t, e.hooks.Names(), webhook.RepoUpdated)
	})
}

func paths(es []model.Entry) []string {
	out := []string{}
	for _, e := range es {
		out = append(out, e.Path)
	}
	return out
}

func shas(cs []repo.CommitInfo) []model.CommitSHA {
	out := []model.CommitSHA{}
	for _, c := range cs {
		out = append(out, c.SHA)
	}
	return out
}

func TestCommitOperationsAndErrors(t *testing.T) {
	forEach(t, func(t *testing.T, e *env) {
		alice := e.addUser("alice")
		id := model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "ops"}
		e.create(repo.CreateParams{ID: id, Actor: alice})
		base := e.commit(id, alice, add("dir/a.txt", "a"), add("dir/b.txt", "b"))

		// No-op Commits make no Commit.
		res, err := e.svc.Commit(e.ctx, repo.CommitParams{Repo: id, Summary: "same", Operations: []repo.Operation{add("dir/a.txt", "a")}, Actor: alice})
		require.NoError(t, err)
		require.True(t, res.NoOp)
		require.Equal(t, base.SHA, res.SHA)

		cases := map[string]struct {
			p   repo.CommitParams
			err error
		}{
			"delete missing":     {repo.CommitParams{Operations: []repo.Operation{{Kind: repo.OpDelete, Path: "nope"}}}, repo.ErrEntryNotFound},
			"delete folder file": {repo.CommitParams{Operations: []repo.Operation{{Kind: repo.OpDelete, Path: "dir"}}}, repo.ErrEntryNotFound},
			"folder missing":     {repo.CommitParams{Operations: []repo.Operation{{Kind: repo.OpDeleteFolder, Path: "nope"}}}, repo.ErrEntryNotFound},
			"copy missing":       {repo.CommitParams{Operations: []repo.Operation{{Kind: repo.OpCopy, Path: "x", SrcPath: "nope"}}}, repo.ErrEntryNotFound},
			"file over dir":      {repo.CommitParams{Operations: []repo.Operation{add("dir", "x")}}, repo.ErrInvalid},
			"lfs missing":        {repo.CommitParams{Operations: []repo.Operation{{Kind: repo.OpAddLFS, Path: "w.bin", OID: strings.Repeat("a", 64), Size: 3}}}, repo.ErrLFSObjectMissing},
			"no branch":          {repo.CommitParams{Revision: "nope", Operations: []repo.Operation{add("x", "x")}}, repo.ErrRevisionNotFound},
			"tag ref":            {repo.CommitParams{Revision: "refs/tags/v1", Operations: []repo.Operation{add("x", "x")}}, repo.ErrInvalid},
			"stale parent":       {repo.CommitParams{ParentCommit: "0000000", Operations: []repo.Operation{add("x", "x")}}, repo.ErrParentMismatch},
			"too big":            {repo.CommitParams{Operations: []repo.Operation{add("big.txt", strings.Repeat("x", 11<<20))}}, repo.ErrInvalid},
		}
		for name, c := range cases {
			p := c.p
			p.Repo, p.Summary, p.Actor = id, "s", alice
			_, err := e.svc.Commit(e.ctx, p)
			require.ErrorIs(t, err, c.err, name)
		}
		_, err = e.svc.Commit(e.ctx, repo.CommitParams{Repo: model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "none"}, Summary: "s", Operations: []repo.Operation{add("x", "x")}})
		require.ErrorIs(t, err, repo.ErrNotFound)

		// Parent prefix matches; pull request refs are created from a Commit.
		res, err = e.svc.Commit(e.ctx, repo.CommitParams{Repo: id, Summary: "s", ParentCommit: base.SHA[:10], Operations: []repo.Operation{add("p.txt", "p")}, Actor: alice})
		require.NoError(t, err)
		pr, err := e.svc.Commit(e.ctx, repo.CommitParams{Repo: id, Revision: "refs/pr/1", CreateFrom: base.SHA, Summary: "pr", Operations: []repo.Operation{add("pr.txt", "pr")}, Actor: alice})
		require.NoError(t, err)
		require.Equal(t, base.SHA, pr.Parent)
		require.Equal(t, "refs/pr/1", pr.Ref)
		got, err := e.svc.Resolve(e.ctx, id, "refs/pr/1")
		require.NoError(t, err)
		require.Equal(t, pr.SHA, got)
		require.Equal(t, "p", e.read(id, res.SHA.String(), "p.txt"))
	})
}

// TestRefCASUnderConcurrency checks that concurrent ref updates from the
// same old value let exactly one win, and that concurrent Commits with the
// same parent Commit lose cleanly, while Commits without one retry.
func TestRefCASUnderConcurrency(t *testing.T) {
	forEach(t, func(t *testing.T, e *env) {
		alice := e.addUser("alice")
		id := model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "race"}
		e.create(repo.CreateParams{ID: id, Actor: alice})
		base := e.commit(id, alice, add("base.txt", "base"))
		var targets []model.CommitSHA
		for i := range 6 {
			res, err := e.svc.Commit(e.ctx, repo.CommitParams{Repo: id, Revision: fmt.Sprintf("refs/pr/%d", i+1), CreateFrom: base.SHA,
				Summary: "s", Operations: []repo.Operation{add(fmt.Sprintf("f%d.txt", i), "x")}, Actor: alice})
			require.NoError(t, err)
			targets = append(targets, res.SHA)
		}

		var wg sync.WaitGroup
		errs := make([]error, len(targets))
		for i, tgt := range targets {
			wg.Go(func() {
				errs[i] = e.svc.UpdateRefs(context.Background(), id, []repo.RefUpdate{{Name: "refs/heads/main", Old: base.SHA, New: tgt}})
			})
		}
		wg.Wait()
		won := 0
		for _, err := range errs {
			if err == nil {
				won++
			} else {
				require.ErrorIs(t, err, repo.ErrRefConflict)
			}
		}
		require.Equal(t, 1, won)

		head, err := e.svc.Resolve(e.ctx, id, "main")
		require.NoError(t, err)
		errs = make([]error, 5)
		for i := range errs {
			wg.Go(func() {
				_, errs[i] = e.svc.Commit(context.Background(), repo.CommitParams{Repo: id, Summary: "p", ParentCommit: head,
					Operations: []repo.Operation{add(fmt.Sprintf("parent%d.txt", i), "x")}, Actor: alice})
			})
		}
		wg.Wait()
		won = 0
		for _, err := range errs {
			if err == nil {
				won++
			} else {
				require.True(t, errors.Is(err, repo.ErrParentMismatch), "%v", err)
			}
		}
		require.Equal(t, 1, won)

		results := make([]*repo.CommitResult, 3)
		errs = make([]error, 3)
		for i := range results {
			wg.Go(func() {
				results[i], errs[i] = e.svc.Commit(context.Background(), repo.CommitParams{Repo: id, Summary: "retry",
					Operations: []repo.Operation{add(fmt.Sprintf("retry%d.txt", i), "x")}, Actor: alice})
			})
		}
		wg.Wait()
		head, err = e.svc.Resolve(e.ctx, id, "main")
		require.NoError(t, err)
		for i, err := range errs {
			if err != nil {
				require.ErrorIs(t, err, repo.ErrRefConflict)
				continue
			}
			_, err = e.svc.Entry(e.ctx, id, head, fmt.Sprintf("retry%d.txt", i))
			require.NoError(t, err, "a successful Commit's file is on the branch")
		}
		gitCmd(t, e.export(id), "fsck", "--strict", "--no-dangling")
	})
}
