package repo_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"

	"github.com/jamesagarside/hangar/internal/db"
	"github.com/jamesagarside/hangar/internal/gitstore"
	"github.com/jamesagarside/hangar/internal/gitstore/gitstoretest"
	"github.com/jamesagarside/hangar/internal/jobs"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/repo"
)

// mktree writes a tree object of entries ("<mode> <type> <oid>\t<name>"
// lines, as git mktree reads them) without checking the names, as a
// hostile client or Upstream would.
func (w *workdir) mktree(entries ...string) string {
	w.t.Helper()
	var raw bytes.Buffer
	for _, e := range entries {
		meta, name, _ := strings.Cut(e, "\t")
		f := strings.Fields(meta)
		mode := f[0]
		if mode == "040000" {
			mode = "40000"
		}
		oid, err := hex.DecodeString(f[2])
		require.NoError(w.t, err)
		raw.WriteString(mode + " " + name + "\x00")
		raw.Write(oid)
	}
	out, err := gitRun(w.dir, strings.NewReader(raw.String()), "hash-object", "-t", "tree", "-w", "--literally", "--stdin")
	require.NoError(w.t, err)
	return strings.TrimSpace(out)
}

func (w *workdir) blob(content string) string {
	w.t.Helper()
	out, err := gitRun(w.dir, strings.NewReader(content), "hash-object", "-w", "--stdin")
	require.NoError(w.t, err)
	return strings.TrimSpace(out)
}

func (w *workdir) commitTree(tree string, parent model.CommitSHA) model.CommitSHA {
	w.t.Helper()
	out, err := gitRun(w.dir, strings.NewReader("hostile\n"), "commit-tree", tree, "-p", string(parent))
	require.NoError(w.t, err)
	return model.CommitSHA(strings.TrimSpace(out))
}

// fanOut returns a tree that names the previous level's tree 16 times,
// levels deep, over one file: 16^levels files from levels+1 small trees.
func (w *workdir) fanOut(levels int) string {
	w.t.Helper()
	cur := w.mktree("100644 blob " + w.blob("x\n") + "\tf")
	for range levels {
		ents := make([]string, 16)
		for i := range ents {
			ents[i] = fmt.Sprintf("040000 tree %s\td%x", cur, i)
		}
		cur = w.mktree(ents...)
	}
	return cur
}

// TestApplyPackRefusesTreeBombs checks the Commit limits on pushes and
// Pulls. Before them, a 666-byte pack whose Commit expanded to 2,167,056
// paths was accepted, and listing it took 45 seconds and 1.3 GiB of heap
// to fill the tree cache; two more levels would not have fitted in memory.
func TestApplyPackRefusesTreeBombs(t *testing.T) {
	forEach(t, func(t *testing.T, e *env) {
		alice := e.addUser("alice")
		for _, source := range []repo.PackSource{repo.SourcePush, repo.SourceMirror} {
			id := model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "bombs-" + string(source)}
			e.create(repo.CreateParams{ID: id, Actor: alice})
			base, err := e.svc.Resolve(e.ctx, id, "main")
			require.NoError(t, err)
			w := e.clone(id)
			apply := func(tip model.CommitSHA) error {
				return e.svc.ApplyPack(e.ctx, repo.ApplyPackParams{Repo: id, Source: source, Actor: alice, Pack: bytes.NewReader(w.pack(tip, base)),
					Updates: []repo.RefUpdate{{Name: "refs/heads/main", Old: base, New: tip}}})
			}
			var pe *repo.PolicyError

			// 16^7 = 268 million files in a pack of under 1 KiB.
			bomb := w.commitTree(w.fanOut(7), base)
			require.Less(t, len(w.pack(bomb, base)), 1<<10)
			start := time.Now()
			err = apply(bomb)
			require.ErrorAs(t, err, &pe, source)
			require.Contains(t, pe.Message, "has more than 1000000 files and folders")
			require.Less(t, time.Since(start), 30*time.Second, "the check reads each tree once")

			// Paths longer than model.MaxPathLength: 41 folders of 100 bytes and a file.
			deep := w.mktree("100644 blob " + w.blob("x") + "\tf")
			for range 41 {
				deep = w.mktree("040000 tree " + deep + "\t" + strings.Repeat("d", 100))
			}
			err = apply(w.commitTree(deep, base))
			require.ErrorAs(t, err, &pe, source)
			require.Contains(t, pe.Message, "has a path of 4142 bytes, over the limit of 4096")

			// Names that escape a download folder or reach a client's .git
			// folder. git index-pack --strict refuses some of these on push
			// on its own; the check refuses them all, for Pulls too.
			file := "100644 blob " + w.blob("evil") + "\t"
			for _, name := range []string{"..", ".GIT", "git~1", ".g\u200cit", `..\..\x`, "a\\b"} {
				tree := w.mktree(file + name)
				if strings.EqualFold(name, ".git") || name == "git~1" || strings.Contains(name, "\u200c") {
					tree = w.mktree("040000 tree " + w.mktree(file+"config") + "\t" + name)
				}
				err = apply(w.commitTree(tree, base))
				require.Error(t, err, "%s: %q", source, name)
				if source == repo.SourceMirror || strings.Contains(name, "\\") {
					require.ErrorAs(t, err, &pe, "%s: %q", source, name)
					require.Contains(t, pe.Message, "which a Repo cannot hold", "%s: %q", source, name)
				}
			}

			got, err := e.svc.Resolve(e.ctx, id, "main")
			require.NoError(t, err)
			require.Equal(t, base, got, "%s: nothing moved", source)

			// A Commit within the limits is accepted.
			ok := w.commitTree(w.fanOut(2), base)
			require.NoError(t, apply(ok), source)
			entries, err := e.svc.Tree(e.ctx, id, ok, "", repo.TreeOpts{Recursive: true})
			require.NoError(t, err)
			require.Len(t, entries, 16*16+16+16*16)
		}
	})
}

// TestApplyPackPathLimitIsConfigurable checks Options.MaxTreePaths, and
// that reading a Commit stored before a lower limit fails rather than
// listing it.
func TestApplyPackPathLimitIsConfigurable(t *testing.T) {
	forEach(t, func(t *testing.T, e *env) {
		alice := e.addUser("alice")
		id := model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "limited"}
		e.create(repo.CreateParams{ID: id, Actor: alice})
		base, err := e.svc.Resolve(e.ctx, id, "main")
		require.NoError(t, err)
		w := e.clone(id)
		big := w.commitTree(w.fanOut(2), base) // 16 + 256 + 256 paths
		require.NoError(t, e.svc.ApplyPack(e.ctx, repo.ApplyPackParams{Repo: id, Source: repo.SourcePush, Actor: alice,
			Pack: bytes.NewReader(w.pack(big, base)), Updates: []repo.RefUpdate{{Name: "refs/heads/big", New: big}}}))

		// As if the Commit had been stored before the limit: not cached.
		_, err = e.d.NewDelete().Model((*db.TreeEntry)(nil)).Where("commit_sha = ?", string(big)).Exec(e.ctx)
		require.NoError(t, err)
		opts := e.opts
		opts.MaxTreePaths = 100
		small, err := repo.NewStore(opts)
		require.NoError(t, err)
		_, err = small.Tree(e.ctx, id, big, "", repo.TreeOpts{Recursive: true})
		require.ErrorIs(t, err, repo.ErrInvalid)
		require.ErrorContains(t, err, "more than 100 files and folders")

		other := w.commitTree(w.fanOut(2), big)
		err = small.ApplyPack(e.ctx, repo.ApplyPackParams{Repo: id, Source: repo.SourcePush, Actor: alice,
			Pack: bytes.NewReader(w.pack(other, big)), Updates: []repo.RefUpdate{{Name: "refs/heads/big", Old: big, New: other}}})
		var pe *repo.PolicyError
		require.ErrorAs(t, err, &pe)
		require.Contains(t, pe.Message, "has more than 100 files and folders")
	})
}

// TestApplyPackRefusesDeltaBombs checks that git index-pack never gets a
// pack whose deltas rebuild objects over the limit, for Pulls as well as
// pushes, since it rebuilds them in memory.
func TestApplyPackRefusesDeltaBombs(t *testing.T) {
	forEach(t, func(t *testing.T, e *env) {
		alice := e.addUser("alice")
		for _, source := range []repo.PackSource{repo.SourcePush, repo.SourceMirror} {
			id := model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "delta-" + string(source)}
			e.create(repo.CreateParams{ID: id, Actor: alice})
			pack := gitstoretest.Pack(gitstoretest.Blob(make([]byte, 0xffffff)), gitstoretest.CopyDelta(0, 0xffffff, 64))
			err := e.svc.ApplyPack(e.ctx, repo.ApplyPackParams{Repo: id, Source: source, Actor: alice, Pack: bytes.NewReader(pack),
				Updates: []repo.RefUpdate{{Name: "refs/heads/x", New: model.CommitSHA(strings.Repeat("1", 40))}}})
			var pe *repo.PolicyError
			require.ErrorAs(t, err, &pe, source)
			require.Contains(t, pe.Message, "a delta that rebuilds an object of 1024 MiB, over the limit of 64 MiB", source)
		}
	})
}

// storeLegacyCommit stores a Commit's objects (those not reachable from
// have) and creates ref at it without repo.ApplyPack, as a Commit stored
// before checkTrees existed would be.
func (e *env) storeLegacyCommit(id model.RepoID, w *workdir, ref string, tip, have model.CommitSHA) {
	e.t.Helper()
	uid := mustUUID(e, id)
	st, err := e.git.NewStaging(e.ctx, uid)
	require.NoError(e.t, err)
	defer st.Close()
	sha, err := st.IndexPack(e.ctx, bytes.NewReader(w.pack(tip, have)), false)
	require.NoError(e.t, err)
	_, err = st.StorePack(e.ctx, sha)
	require.NoError(e.t, err)
	require.NoError(e.t, e.d.InTx(e.ctx, func(ctx context.Context, tx bun.Tx) error {
		if err := gitstore.UpdateRefs(ctx, tx, uid, []gitstore.RefUpdate{{Name: ref, New: tip}}); err != nil {
			return err
		}
		return gitstore.MarkLive(ctx, tx, uid, sha)
	}))
}

// TestZipExportRefusesUnsafePaths checks that a zip export of a Commit
// stored before pushes and Pulls were checked never writes an entry that
// extracts outside its folder. Before the check, "../evil.txt" became the
// entry "evil.txt" one level above the Repo's folder (path.Join cleaned
// "zipme/../evil.txt").
func TestZipExportRefusesUnsafePaths(t *testing.T) {
	forEach(t, func(t *testing.T, e *env) {
		require.NoError(t, e.svc.RegisterJobs(e.reg, e.lfs, repo.JobOptions{Blobs: e.blobs}))
		alice := e.addUser("alice")
		id := model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "zipme"}
		e.create(repo.CreateParams{ID: id, Actor: alice})
		base, err := e.svc.Resolve(e.ctx, id, "main")
		require.NoError(t, err)
		w := e.clone(id)
		for _, name := range []string{"..", `..\..\x`} {
			inner := w.mktree("100644 blob " + w.blob("evil") + "\tevil.txt")
			tip := w.commitTree(w.mktree("040000 tree "+inner+"\t"+name), base)
			ref := "refs/heads/legacy"
			e.storeLegacyCommit(id, w, ref, tip, base)

			jid, err := e.jobs.Enqueue(e.ctx, jobs.NewJob{Kind: jobs.KindZipExport, Payload: repo.ZipJob{RepoUUID: mustUUID(e, id), Commit: tip}})
			require.NoError(t, err)
			e.jobs.RunPending(e.ctx)
			j, err := e.jobs.Get(e.ctx, jid)
			require.NoError(t, err)
			require.Equal(t, jobs.StatusFailed, j.Status, name)
			require.Contains(t, j.Error, "cannot be extracted safely", name)

			require.NoError(t, e.d.InTx(e.ctx, func(ctx context.Context, tx bun.Tx) error {
				return gitstore.UpdateRefs(ctx, tx, mustUUID(e, id), []gitstore.RefUpdate{{Name: ref, Old: tip}})
			}))
		}
	})
}
