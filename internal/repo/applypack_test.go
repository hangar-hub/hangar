package repo_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/db"
	"github.com/jamesagarside/hangar/internal/gitstore"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/repo"
)

// workdir is a git clone of a Repo where tests make Commits with git and
// build the packs a client would push.
type workdir struct {
	t   *testing.T
	dir string
}

func (e *env) clone(id model.RepoID) *workdir {
	e.t.Helper()
	v, err := e.svc.OpenView(e.ctx, id)
	require.NoError(e.t, err)
	defer v.Close()
	dir := filepath.Join(e.t.TempDir(), "work")
	gitCmd(e.t, "", "clone", "--quiet", v.Dir, dir)
	return &workdir{t: e.t, dir: dir}
}

func (w *workdir) write(path, content string) {
	w.t.Helper()
	p := filepath.Join(w.dir, path)
	require.NoError(w.t, os.MkdirAll(filepath.Dir(p), 0o755))
	require.NoError(w.t, os.WriteFile(p, []byte(content), 0o600))
}

func (w *workdir) commit(msg string) model.CommitSHA {
	w.t.Helper()
	gitCmd(w.t, w.dir, "add", "-A")
	gitCmd(w.t, w.dir, "commit", "-q", "-m", msg)
	return model.CommitSHA(gitCmd(w.t, w.dir, "rev-parse", "HEAD"))
}

// pack returns the objects reachable from tip and not from have, as a
// client sends them in a push.
func (w *workdir) pack(tip model.CommitSHA, have ...model.CommitSHA) []byte {
	w.t.Helper()
	in := string(tip) + "\n"
	for _, h := range have {
		in += "^" + string(h) + "\n"
	}
	// Only stdout: git warns on stderr about paths it cannot check out.
	cmd := exec.Command("git", "pack-objects", "--revs", "--stdout", "-q")
	cmd.Dir = w.dir
	cmd.Stdin = strings.NewReader(in)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.Output()
	require.NoError(w.t, err)
	return out
}

func TestApplyPackPushPolicy(t *testing.T) {
	forEach(t, func(t *testing.T, e *env) {
		alice := e.addUser("alice")
		id := model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "pushed"}
		e.create(repo.CreateParams{ID: id, Actor: alice})
		base, err := e.svc.Resolve(e.ctx, id, "main")
		require.NoError(t, err)
		w := e.clone(id)

		push := func(updates []repo.RefUpdate, pack []byte, force bool) error {
			var r *bytes.Reader
			if pack != nil {
				r = bytes.NewReader(pack)
			}
			p := repo.ApplyPackParams{Repo: id, Source: repo.SourcePush, Actor: alice, Updates: updates, AllowForce: force}
			if r != nil {
				p.Pack = r
			}
			return e.svc.ApplyPack(e.ctx, p)
		}

		// A fast-forward push.
		w.write("notes/a.txt", "a\n")
		one := w.commit("one")
		require.NoError(t, push([]repo.RefUpdate{{Name: "refs/heads/main", Old: base, New: one}}, w.pack(one, base), false))
		require.Equal(t, "a\n", e.read(id, "main", "notes/a.txt"))
		ci, err := e.svc.GetCommit(e.ctx, id, one)
		require.NoError(t, err)
		require.Equal(t, "one", ci.Title)

		// Pushing a branch and a tag at once, with no new objects.
		require.NoError(t, push([]repo.RefUpdate{{Name: "refs/heads/dev", New: one}, {Name: "refs/tags/v1", New: base}}, nil, false))
		refs, err := e.svc.Refs(e.ctx, id)
		require.NoError(t, err)
		require.Len(t, refs.Branches, 2)
		require.Len(t, refs.Tags, 1)

		var pe *repo.PolicyError
		// Force-pushing the default branch needs AllowForce.
		gitCmd(t, w.dir, "reset", "-q", "--hard", string(base))
		w.write("other.txt", "other")
		forced := w.commit("forced")
		err = push([]repo.RefUpdate{{Name: "refs/heads/main", Old: one, New: forced}}, w.pack(forced, base), false)
		require.ErrorAs(t, err, &pe)
		require.Contains(t, pe.Message, "force-pushing")
		require.NoError(t, push([]repo.RefUpdate{{Name: "refs/heads/dev", Old: one, New: forced}}, w.pack(forced, base), false), "other branches may be rewritten")
		require.NoError(t, push([]repo.RefUpdate{{Name: "refs/heads/main", Old: one, New: forced}}, w.pack(forced, base), true))

		for name, u := range map[string]repo.RefUpdate{
			"pr ref":         {Name: "refs/pr/1", New: forced},
			"delete default": {Name: "refs/heads/main", Old: forced},
			"bad name":       {Name: "refs/heads/a..b", New: forced},
		} {
			err := push([]repo.RefUpdate{u}, nil, false)
			require.ErrorAs(t, err, &pe, name)
		}
		err = push([]repo.RefUpdate{{Name: "refs/heads/ghost", New: model.CommitSHA(strings.Repeat("c", 40))}}, nil, false)
		require.ErrorAs(t, err, &pe)
		require.Contains(t, pe.Message, "missing")
		require.NoError(t, push([]repo.RefUpdate{{Name: "refs/heads/dev", Old: forced}}, nil, false), "branches other than the default can be deleted")

		// Corrupt packs are refused.
		err = push([]repo.RefUpdate{{Name: "refs/heads/x", New: forced}}, []byte("PACK not really"), false)
		require.ErrorIs(t, err, repo.ErrInvalid)

		// A stale old value is a conflict, and nothing moves.
		w.write("late.txt", "late")
		late := w.commit("late")
		err = push([]repo.RefUpdate{{Name: "refs/heads/main", Old: base, New: late}, {Name: "refs/heads/new", New: late}}, w.pack(late, forced), false)
		require.ErrorIs(t, err, repo.ErrRefConflict)
		_, err = e.svc.Resolve(e.ctx, id, "new")
		require.ErrorIs(t, err, repo.ErrRevisionNotFound)
		var orphaned int64
		orphaned, err = e.d.NewSelect().Model((*db.Pack)(nil)).Where("state = ?", gitstore.PackOrphaned).Count(e.ctx)
		require.NoError(t, err)
		require.Equal(t, int64(1), orphaned)

		gitCmd(t, e.export(id), "fsck", "--strict", "--no-dangling")
	})
}

func TestApplyPackBlobAndLFSPolicy(t *testing.T) {
	forEach(t, func(t *testing.T, e *env) {
		alice := e.addUser("alice")
		id := model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "lfs"}
		e.create(repo.CreateParams{ID: id, Actor: alice})
		base, err := e.svc.Resolve(e.ctx, id, "main")
		require.NoError(t, err)
		w := e.clone(id)
		var pe *repo.PolicyError

		w.write("big.txt", strings.Repeat("x", 4096))
		big := w.commit("big")
		err = e.svc.ApplyPack(e.ctx, repo.ApplyPackParams{Repo: id, Source: repo.SourcePush, Pack: bytes.NewReader(w.pack(big, base)),
			Updates: []repo.RefUpdate{{Name: "refs/heads/main", Old: base, New: big}}})
		require.ErrorAs(t, err, &pe)
		require.Contains(t, pe.Message, "git lfs track")

		gitCmd(t, w.dir, "reset", "-q", "--hard", string(base))
		weights := []byte("weights over lfs")
		ptr := model.LFSPointer{OID: sha256Hex(weights), Size: int64(len(weights))}
		w.write("model.bin", string(ptr.Bytes()))
		withPtr := w.commit("pointer")
		pack := w.pack(withPtr, base)
		push := func() error {
			return e.svc.ApplyPack(e.ctx, repo.ApplyPackParams{Repo: id, Source: repo.SourcePush, Pack: bytes.NewReader(pack),
				Updates: []repo.RefUpdate{{Name: "refs/heads/main", Old: base, New: withPtr}}})
		}
		err = push()
		require.ErrorAs(t, err, &pe)
		require.Contains(t, pe.Message, "git lfs push")

		// After a presigned upload that was never verified, the push
		// verifies the object itself.
		require.NoError(t, e.blobs.Put(e.ctx, model.LFSObjectKey(ptr.OID), bytes.NewReader(weights), ptr.Size))
		require.NoError(t, push())
		require.Equal(t, string(weights), e.read(id, "main", "model.bin"))
		refs, err := e.svc.LFSRefs(e.ctx, id, []string{ptr.OID})
		require.NoError(t, err)
		require.True(t, refs[ptr.OID].Present)

		// Packs over git.max_pack_size are refused.
		w.write("more.txt", strings.Repeat("y", 900))
		more := w.commit("more")
		err = e.svc.ApplyPack(e.ctx, repo.ApplyPackParams{Repo: id, Source: repo.SourcePush, Pack: bytes.NewReader(w.pack(more, withPtr)),
			Updates: []repo.RefUpdate{{Name: "refs/heads/main", Old: withPtr, New: more}}})
		require.NoError(t, err, "under both limits")
		w.write("more2.txt", strings.Repeat("z", 1000)+"different")
		more2 := w.commit("more2")
		e2, err := repo.NewStore(withOpts(e.opts, func(o *repo.Options) { o.MaxPackSize = 64 }))
		require.NoError(t, err)
		err = e2.ApplyPack(e.ctx, repo.ApplyPackParams{Repo: id, Source: repo.SourcePush, Pack: bytes.NewReader(w.pack(more2, more)),
			Updates: []repo.RefUpdate{{Name: "refs/heads/main", Old: more, New: more2}}})
		require.ErrorAs(t, err, &pe)
		require.Contains(t, pe.Message, "larger than")
	}, func(o *repo.Options) { o.MaxBlobSize = 2000 })
}

func withOpts(o repo.Options, f func(*repo.Options)) repo.Options {
	f(&o)
	return o
}

func TestApplyPackMirrorAndConcurrentPushes(t *testing.T) {
	forEach(t, func(t *testing.T, e *env) {
		alice := e.addUser("alice")
		src := model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "upstream"}
		e.create(repo.CreateParams{ID: src, Actor: alice})
		w := e.clone(src)
		missing := []byte("q8 weights")
		ptr := model.LFSPointer{OID: sha256Hex(missing), Size: int64(len(missing))}
		w.write("m-Q8.gguf", string(ptr.Bytes()))
		tip := w.commit("add quant")

		// A Mirror stores the upstream pack as is, with the same SHAs.
		mid := model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "mirror"}
		e.create(repo.CreateParams{ID: mid, Actor: alice, Mirror: true})
		err := e.svc.ApplyPack(e.ctx, repo.ApplyPackParams{Repo: mid, Source: repo.SourcePush, Pack: bytes.NewReader(w.pack(tip)),
			Updates: []repo.RefUpdate{{Name: "refs/heads/main", New: tip}}})
		require.ErrorIs(t, err, repo.ErrReadOnly)
		require.NoError(t, e.svc.ApplyPack(e.ctx, repo.ApplyPackParams{Repo: mid, Source: repo.SourceMirror, Actor: model.System(),
			Pack: bytes.NewReader(w.pack(tip)), Updates: []repo.RefUpdate{{Name: "refs/heads/main", New: tip}, {Name: "refs/pr/1", New: tip}}}))
		got, err := e.svc.Resolve(e.ctx, mid, "main")
		require.NoError(t, err)
		require.Equal(t, tip, got)
		en, err := e.svc.Entry(e.ctx, mid, tip, "m-Q8.gguf")
		require.NoError(t, err)
		require.False(t, en.Present)
		refs, err := e.svc.LFSRefs(e.ctx, mid, []string{ptr.OID})
		require.NoError(t, err)
		require.Equal(t, repo.LFSRef{OID: ptr.OID, Size: ptr.Size, Present: false}, refs[ptr.OID])

		// Two pushes race from the same old value: exactly one wins.
		base, err := e.svc.Resolve(e.ctx, src, "main")
		require.NoError(t, err)
		packs := make([][]byte, 2)
		tips := make([]model.CommitSHA, 2)
		for i := range packs {
			gitCmd(t, w.dir, "reset", "-q", "--hard", string(base))
			w.write("race.txt", strings.Repeat("r", i+1))
			tips[i] = w.commit("race")
			packs[i] = w.pack(tips[i], base)
		}
		errs := make([]error, 2)
		var wg sync.WaitGroup
		for i := range packs {
			wg.Go(func() {
				errs[i] = e.svc.ApplyPack(context.Background(), repo.ApplyPackParams{Repo: src, Source: repo.SourcePush, Actor: alice,
					Pack: bytes.NewReader(packs[i]), Updates: []repo.RefUpdate{{Name: "refs/heads/main", Old: base, New: tips[i]}}})
			})
		}
		wg.Wait()
		ok := 0
		for _, err := range errs {
			if err == nil {
				ok++
			} else {
				require.True(t, errors.Is(err, repo.ErrRefConflict), "%v", err)
			}
		}
		require.Equal(t, 1, ok)
		gitCmd(t, e.export(src), "fsck", "--strict", "--no-dangling")
		require.NoError(t, e.svc.ApplyPack(e.ctx, repo.ApplyPackParams{Repo: src}), "no updates is a no-op")
	})
}

// TestCachesRebuildFromGit checks that the tree and Commit caches are only
// caches: with their rows gone, or on a replica with an empty pack cache,
// every read gives the same answer from git.
func TestCachesRebuildFromGit(t *testing.T) {
	forEach(t, func(t *testing.T, e *env) {
		alice := e.addUser("alice")
		src := model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "src"}
		e.create(repo.CreateParams{ID: src, Actor: alice})
		base, err := e.svc.Resolve(e.ctx, src, "main")
		require.NoError(t, err)
		w := e.clone(src)
		w.write("a.txt", "a")
		w.write("dir/b.txt", "b")
		one := w.commit("one")
		w.write("a.txt", "a2")
		two := w.commit("two")
		w.write("c.txt", "c")
		three := w.commit("three")

		// A Mirror sync brings three Commits at once, so only the tip is cached.
		mid := model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "mirror"}
		e.create(repo.CreateParams{ID: mid, Actor: alice, Mirror: true})
		require.NoError(t, e.svc.ApplyPack(e.ctx, repo.ApplyPackParams{Repo: mid, Source: repo.SourceMirror, Pack: bytes.NewReader(w.pack(three)),
			Updates: []repo.RefUpdate{{Name: "refs/heads/main", New: three}}}))
		want := map[string]model.CommitSHA{".gitattributes": base, "a.txt": two, "dir": one, "c.txt": three}
		check := func(svc *repo.Store) {
			t.Helper()
			es, err := svc.Tree(e.ctx, mid, three, "", repo.TreeOpts{Expand: true})
			require.NoError(t, err)
			require.Len(t, es, len(want))
			for _, en := range es {
				require.NotNil(t, en.LastCommit, en.Path)
				require.Equal(t, want[en.Path], en.LastCommit.SHA, en.Path)
			}
			ci, err := svc.GetCommit(e.ctx, mid, two)
			require.NoError(t, err)
			require.Equal(t, "two", ci.Title)
			sub, err := svc.Entries(e.ctx, mid, three, []string{"dir/b.txt"}, true)
			require.NoError(t, err)
			require.Equal(t, one, sub[0].LastCommit.SHA)
		}
		check(e.svc)
		check(e.svc) // now from the saved values

		for _, m := range []any{(*db.TreeEntry)(nil), (*db.Commit)(nil)} {
			_, err := e.d.NewDelete().Model(m).Where("1 = 1").Exec(e.ctx)
			require.NoError(t, err)
		}
		gs2, err := gitstore.New(gitstore.Options{DB: e.d, Blobs: e.blobs, CacheDir: t.TempDir()})
		require.NoError(t, err)
		fresh, err := repo.NewStore(withOpts(e.opts, func(o *repo.Options) { o.Git = gs2 }))
		require.NoError(t, err)
		check(fresh)
		_, err = fresh.GetCommit(e.ctx, mid, model.CommitSHA(strings.Repeat("d", 40)))
		require.ErrorIs(t, err, repo.ErrRevisionNotFound)
		_, err = fresh.GetCommit(e.ctx, mid, "short")
		require.ErrorIs(t, err, repo.ErrRevisionNotFound)
		_, err = fresh.Tree(e.ctx, mid, model.CommitSHA(strings.Repeat("d", 40)), "", repo.TreeOpts{})
		require.ErrorIs(t, err, repo.ErrRevisionNotFound)
		_, err = fresh.Diff(e.ctx, mid, model.CommitSHA(strings.Repeat("d", 40)))
		require.ErrorIs(t, err, repo.ErrRevisionNotFound)
		diff, err := fresh.Diff(e.ctx, mid, two)
		require.NoError(t, err)
		require.Equal(t, []repo.FileChange{{Path: "a.txt", Status: repo.ChangeModified, OldOID: model.GitBlobID([]byte("a")),
			NewOID: model.GitBlobID([]byte("a2")), OldSize: 1, NewSize: 2}}, diff)
	})
}
