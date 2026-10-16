package repo_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
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
// an exported copy passes git fsck, and git add and git commit rebuild
// every Commit with the same SHA.
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
			{{Kind: repo.OpAddLFS, Path: "model.safetensors", OID: oid, Size: int64(len(weights))}, add("empty.txt", ""), add("pytorch_model.bin", "tracked by .gitattributes")},
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
		replayWithPorcelain(t, dir, "refs/heads/main")
	})
}

// replayWithPorcelain rebuilds every Commit on ref with git add and git
// commit in a fresh repository, from the files, author, committer and
// message of the exported Commit, and checks that git produces the same
// SHA. It checks tree and Commit encoding against git itself, independently
// of the plumbing that both the service and the fake use.
func replayWithPorcelain(t *testing.T, exported, ref string) {
	t.Helper()
	work := filepath.Join(t.TempDir(), "replay")
	gitCmd(t, "", "init", "--quiet", "--initial-branch=main", work)
	for _, sha := range strings.Fields(gitCmd(t, exported, "rev-list", "--reverse", ref)) {
		entries, err := os.ReadDir(work)
		require.NoError(t, err)
		for _, en := range entries {
			if en.Name() != ".git" {
				require.NoError(t, os.RemoveAll(filepath.Join(work, en.Name())))
			}
		}
		for _, line := range strings.Split(gitCmd(t, exported, "ls-tree", "-r", "-z", "--format=%(objectmode) %(path)", sha), "\x00") {
			if line == "" {
				continue
			}
			mode, path, _ := strings.Cut(line, " ")
			require.Equal(t, "100644", mode, path)
			out, err := gitRun(exported, nil, "cat-file", "blob", sha+":"+path)
			require.NoError(t, err)
			p := filepath.Join(work, filepath.FromSlash(path))
			require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
			require.NoError(t, os.WriteFile(p, []byte(out), 0o644))
		}
		raw, err := gitRun(exported, nil, "cat-file", "commit", sha)
		require.NoError(t, err)
		header, msg, ok := strings.Cut(raw, "\n\n")
		require.True(t, ok)
		env := []string{}
		for _, h := range strings.Split(header, "\n") {
			kind, rest, _ := strings.Cut(h, " ")
			if kind != "author" && kind != "committer" {
				continue
			}
			who, when, _ := strings.Cut(rest, "> ")
			name, email, _ := strings.Cut(who, " <")
			prefix := "GIT_" + strings.ToUpper(kind) + "_"
			env = append(env, prefix+"NAME="+name, prefix+"EMAIL="+email, prefix+"DATE=@"+when)
		}
		require.Len(t, env, 6)
		msgFile := filepath.Join(t.TempDir(), "msg")
		require.NoError(t, os.WriteFile(msgFile, []byte(msg), 0o600))
		gitCmd(t, work, "add", "-A")
		cmd := exec.Command("git", "commit", "--quiet", "--allow-empty", "--no-verify", "--cleanup=verbatim", "-F", msgFile)
		cmd.Dir = work
		cmd.Env = append(os.Environ(), append([]string{"GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_CONFIG_NOSYSTEM=1"}, env...)...)
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, string(out))
		require.Equal(t, sha, gitCmd(t, work, "rev-parse", "HEAD"), "git commit gives a different SHA for %s", sha)
	}
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

		// CompareFile diffs across Commits, as pull requests need.
		cf, err := e.svc.CompareFile(e.ctx, id, first.SHA, second.SHA, "z.txt")
		require.NoError(t, err)
		require.Equal(t, fd.Patch, cf.Patch, "across one Commit it equals the Commit's diff")
		_, err = e.svc.CompareFile(e.ctx, id, first.SHA, second.SHA, "a/d.txt")
		require.ErrorIs(t, err, repo.ErrEntryNotFound)
		_, err = e.svc.CompareFile(e.ctx, id, model.CommitSHA(strings.Repeat("0", 40)), second.SHA, "z.txt")
		require.ErrorIs(t, err, repo.ErrRevisionNotFound)
		_, err = e.svc.CompareFile(e.ctx, model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "nope"}, first.SHA, second.SHA, "z.txt")
		require.ErrorIs(t, err, repo.ErrNotFound)

		// Index and scan Jobs for every new branch head, and repo.updated events.
		var kinds []jobs.Kind
		js, _, err := e.jobs.List(e.ctx, jobs.Filter{})
		require.NoError(t, err)
		created, err := e.svc.Get(e.ctx, id)
		require.NoError(t, err)
		for _, j := range js {
			kinds = append(kinds, j.Kind)
			if j.Kind == jobs.KindIndex || j.Kind == jobs.KindScan {
				require.Equal(t, created.UUID, j.Repo, "index and scan Jobs are scoped to their Repo")
			}
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

func TestOnlyHangarCommitsToConvertRefs(t *testing.T) {
	forEach(t, func(t *testing.T, e *env) {
		alice := e.addUser("alice")
		id := model.RepoID{Type: model.RepoTypeDataset, Namespace: "alice", Name: "data"}
		e.create(repo.CreateParams{ID: id, Actor: alice})
		head := e.commit(id, alice, add("train.csv", "a,b\n1,2\n"))
		_, err := e.svc.Commit(e.ctx, repo.CommitParams{Repo: id, Revision: model.RefParquet, Summary: "spoof", Actor: alice, CreateFrom: head.SHA,
			Operations: []repo.Operation{add("default/train/0000.parquet", "not parquet")}})
		var pe *repo.PolicyError
		require.ErrorAs(t, err, &pe, "users cannot write the Parquet export")
		require.Contains(t, pe.Error(), "refs/convert/")
		res, err := e.svc.Commit(e.ctx, repo.CommitParams{Repo: id, Revision: model.RefParquet, Summary: "Update Parquet export", Actor: model.System(),
			CreateFrom: head.SHA, Operations: []repo.Operation{add("default/train/0000.parquet", "export")}})
		require.NoError(t, err, "Hangar's own export commits")
		require.False(t, res.NoOp)
	})
}

// TestCommitBatchesOperationsInOrder checks the batched index updates of a
// Commit: operations still apply in order, a refused file is still named,
// and paths that need quoting for git are deleted correctly. It also checks
// the batched blob probes behind Diff.
func TestCommitBatchesOperationsInOrder(t *testing.T) {
	forEach(t, func(t *testing.T, e *env) {
		alice := e.addUser("alice")
		id := model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "batch"}
		e.create(repo.CreateParams{ID: id, Actor: alice})
		quoted, newline := `"quoted".txt`, "line\nbreak.txt"
		e.commit(id, alice, add(quoted, "q"), add(newline, "n"), add("dir/keep.txt", "k"), add("gone.txt", "g"))

		// An add, a delete of the same path, and an add after a delete.
		res := e.commit(id, alice, add("temp.txt", "t"), repo.Operation{Kind: repo.OpDelete, Path: "temp.txt"},
			repo.Operation{Kind: repo.OpDelete, Path: quoted}, repo.Operation{Kind: repo.OpDelete, Path: newline},
			repo.Operation{Kind: repo.OpDelete, Path: "gone.txt"}, add("gone.txt", "back"))
		root, err := e.svc.Tree(e.ctx, id, res.SHA, "", repo.TreeOpts{})
		require.NoError(t, err)
		require.Equal(t, []string{"dir", ".gitattributes", "gone.txt"}, paths(root))
		require.Equal(t, "back", e.read(id, "main", "gone.txt"))
		gitCmd(t, e.export(id), "fsck", "--strict", "--no-dangling")

		// A file that would replace a folder fails in the middle of a batch
		// and is named, and nothing is committed.
		_, err = e.svc.Commit(e.ctx, repo.CommitParams{Repo: id, Summary: "s", Actor: alice,
			Operations: []repo.Operation{add("a.txt", "a"), add("dir", "x"), add("b.txt", "b")}})
		require.ErrorIs(t, err, repo.ErrInvalid)
		require.ErrorContains(t, err, "cannot add dir")
		head, err := e.svc.Resolve(e.ctx, id, "main")
		require.NoError(t, err)
		require.Equal(t, res.SHA, head)
		_, err = e.svc.Commit(e.ctx, repo.CommitParams{Repo: id, Summary: "s", Actor: alice,
			Operations: []repo.Operation{add("c.txt", "c"), {Kind: repo.OpCopy, Path: "dir", SrcPath: "gone.txt"}}})
		require.ErrorIs(t, err, repo.ErrInvalid)
		require.ErrorContains(t, err, "cannot copy to dir")

		// Diff reports sizes, LFS pointers and binary files of both sides,
		// including blobs too large to probe in the batch.
		large := strings.Repeat("l", 2<<20)
		next := e.commit(id, alice, add("bin.dat", "a\x00b"), add("large.txt", large), add("w.bin", "lfs content"),
			add("dir/keep.txt", "kk"))
		changes, err := e.svc.Diff(e.ctx, id, next.SHA)
		require.NoError(t, err)
		got := map[string]repo.FileChange{}
		for _, c := range changes {
			got[c.Path] = c
		}
		require.True(t, got["bin.dat"].Binary)
		require.Equal(t, int64(3), got["bin.dat"].NewSize)
		require.False(t, got["large.txt"].Binary)
		require.Equal(t, int64(len(large)), got["large.txt"].NewSize)
		require.True(t, got["w.bin"].LFS)
		require.Equal(t, int64(len("lfs content")), got["w.bin"].NewSize)
		require.Equal(t, repo.ChangeModified, got["dir/keep.txt"].Status)
		require.Equal(t, int64(1), got["dir/keep.txt"].OldSize)
		require.Equal(t, int64(2), got["dir/keep.txt"].NewSize)
		fd, err := e.svc.FileDiff(e.ctx, id, next.SHA, "dir/keep.txt")
		require.NoError(t, err)
		require.Contains(t, fd.Patch, "+kk")

		// The first Commit diffs against the empty tree.
		first, err := e.svc.Log(e.ctx, id, "main", repo.LogOpts{})
		require.NoError(t, err)
		initial, err := e.svc.Diff(e.ctx, id, first[len(first)-1].SHA)
		require.NoError(t, err)
		require.Len(t, initial, 1)
		require.Equal(t, repo.ChangeAdded, initial[0].Status)
		require.Empty(t, initial[0].OldOID)
	})
}

// TestOpenFileStreamsLargeGitBlobs covers regular files too large for the
// blob cache: they stream from git, ranges skip to their start, and Close
// releases the repository's objects.
func TestOpenFileStreamsLargeGitBlobs(t *testing.T) {
	forEach(t, func(t *testing.T, e *env) {
		alice := e.addUser("alice")
		id := model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "large"}
		e.create(repo.CreateParams{ID: id, Actor: alice})
		var sb strings.Builder
		for i := 0; sb.Len() < 100<<10; i++ {
			fmt.Fprintf(&sb, "line %06d\n", i)
		}
		content := sb.String()
		res := e.commit(id, alice, add("big.txt", content))

		rc, en, err := e.svc.OpenFile(e.ctx, id, res.SHA, "big.txt", nil)
		require.NoError(t, err)
		require.Nil(t, en.LFS)
		require.Equal(t, int64(len(content)), en.Size)
		b, err := io.ReadAll(rc)
		require.NoError(t, err)
		require.NoError(t, rc.Close())
		require.Equal(t, content, string(b))

		rc, _, err = e.svc.OpenFile(e.ctx, id, res.SHA, "big.txt", &blob.Range{Start: 77000, End: 77010})
		require.NoError(t, err)
		b, err = io.ReadAll(rc)
		require.NoError(t, err)
		require.NoError(t, rc.Close())
		require.Equal(t, content[77000:77011], string(b))

		_, _, err = e.svc.OpenFile(e.ctx, id, res.SHA, "big.txt", &blob.Range{Start: int64(len(content)) + 1, End: -1})
		require.ErrorIs(t, err, blob.ErrInvalidRange)
	})
}
