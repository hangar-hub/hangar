package scan_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/db/dbtest"
	"github.com/hangar-hub/hangar/internal/index/indextest"
	"github.com/hangar-hub/hangar/internal/model"
	"github.com/hangar-hub/hangar/internal/repo/repotest"
	"github.com/hangar-hub/hangar/internal/scan"
	"github.com/hangar-hub/hangar/internal/scan/scantest"
)

// TestCurrentCarriesResultsForUnchangedFiles checks that, while a new
// Commit's scan Job is pending, readers and the download gate see the
// nearest scanned ancestor's result for each file whose blob is unchanged,
// and no result (pending) for each file that changed.
func TestCurrentCarriesResultsForUnchangedFiles(t *testing.T) {
	d := dbtest.SQLite(t)
	ctx := t.Context()
	fake := repotest.NewFake(t)
	id := model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "carry"}
	r := indextest.CreateRepo(t, fake, id, model.VisibilityPublic)
	indextest.InsertRepoRow(t, d, r)
	scanned := indextest.Commit(t, fake, id, map[string][]byte{
		"evil.pkl":           scantest.OSSystemProtocol0(),
		"model.ckpt":         scantest.TorchStateDict(),
		"changed.pkl":        scantest.TorchStateDict(),
		"openvino/model.bin": scantest.SubprocessInst(),
		"README.md":          []byte("# hi"),
	})
	s := scan.New(scan.Options{DB: d, Repos: fake, LFS: fake.LFS})
	_, err := s.ScanCommit(ctx, r.UUID, id, scanned)
	require.NoError(t, err)

	// The next Commit changes one pickle, adds another and adds an
	// OpenVINO graph beside the .bin, which once made the scanner skip the
	// .bin as raw weights (phase 3 finding 6a). Its scan has not run.
	next := indextest.Commit(t, fake, id, map[string][]byte{
		"changed.pkl":        scantest.SubprocessInst(),
		"new.pkl":            scantest.TorchStateDict(),
		"openvino/model.xml": []byte("<net/>"),
	})
	_, err = s.Results(ctx, r.UUID, next)
	require.ErrorIs(t, err, scan.ErrNotScanned)

	v, err := scan.Current(ctx, s.Results, fake, r.UUID, id, next, nil)
	require.NoError(t, err)
	require.False(t, v.Scanned)
	require.Equal(t, scanned, v.From)
	evil, ok := v.File("evil.pkl")
	require.True(t, ok, "an unchanged pickle keeps its result")
	require.Equal(t, scan.StatusUnsafe, evil.Status)
	require.NotEmpty(t, evil.Findings)
	ckpt, ok := v.File("model.ckpt")
	require.True(t, ok)
	require.Equal(t, scan.StatusSafe, ckpt.Status)
	require.Equal(t, scan.StatusUnsafe, v.Status)
	bin, ok := v.File("openvino/model.bin")
	require.True(t, ok, "an .xml beside a pickle does not change what the pickle is")
	require.Equal(t, scan.StatusUnsafe, bin.Status)
	for _, p := range []string{"changed.pkl", "new.pkl"} {
		_, ok := v.File(p)
		require.False(t, ok, "%s changed, so its old result does not apply", p)
		require.True(t, v.Pending(p), p)
	}
	require.False(t, v.Pending("README.md"), "the scan does not read README.md")
	require.False(t, v.Pending("evil.pkl"))

	// paths limits the View to the files asked for.
	v, err = scan.Current(ctx, s.Results, fake, r.UUID, id, next, []string{"model.ckpt", "new.pkl"})
	require.NoError(t, err)
	require.Len(t, v.Files, 1)
	require.Equal(t, "model.ckpt", v.Files[0].Path)
	require.Equal(t, scan.StatusSafe, v.Status, "the status covers only the files asked for")

	// The download gate agrees: an unchanged unsafe file is refused as
	// unsafe, an unchanged safe one downloads, a changed one fails closed.
	g := s.NewGate(func(context.Context) (bool, error) { return true, nil })
	require.Equal(t, scan.BlockUnsafe, blockedReason(t, g.CheckFile(ctx, r.UUID, id, next, "evil.pkl")))
	require.NoError(t, g.CheckFile(ctx, r.UUID, id, next, "model.ckpt"))
	require.Equal(t, scan.BlockNotScanned, blockedReason(t, g.CheckFile(ctx, r.UUID, id, next, "changed.pkl")))
	require.Equal(t, scan.BlockNotScanned, blockedReason(t, g.CheckFile(ctx, r.UUID, id, next, "new.pkl")))
	require.Equal(t, scan.BlockUnsafe, blockedReason(t, g.CheckFile(ctx, r.UUID, id, next, "openvino/model.bin")))
	require.Error(t, g.CheckTree(ctx, r.UUID, id, next, ""))

	// Carrying looks past ancestors that are not scanned either.
	third := indextest.Commit(t, fake, id, map[string][]byte{"notes.md": []byte("notes")})
	v, err = scan.Current(ctx, s.Results, fake, r.UUID, id, third, []string{"evil.pkl", "changed.pkl"})
	require.NoError(t, err)
	require.Equal(t, scanned, v.From)
	evil, ok = v.File("evil.pkl")
	require.True(t, ok)
	require.Equal(t, scan.StatusUnsafe, evil.Status)
	require.True(t, v.Pending("changed.pkl"))

	// Once the Commit is scanned, its own results replace the carried ones.
	_, err = s.ScanCommit(ctx, r.UUID, id, next)
	require.NoError(t, err)
	v, err = scan.Current(ctx, s.Results, fake, r.UUID, id, next, nil)
	require.NoError(t, err)
	require.True(t, v.Scanned)
	require.Equal(t, next, v.From)
	changed, ok := v.File("changed.pkl")
	require.True(t, ok)
	require.Equal(t, scan.StatusUnsafe, changed.Status)
	bin, ok = v.File("openvino/model.bin")
	require.True(t, ok, "the scanner reads a .bin by its content, whatever is beside it")
	require.Equal(t, scan.StatusUnsafe, bin.Status)
	require.Equal(t, scan.BlockUnsafe, blockedReason(t, g.CheckFile(ctx, r.UUID, id, next, "openvino/model.bin")))
	require.False(t, v.Pending("openvino/model.bin"), "a scanned Commit has nothing pending")
}

// TestCurrentWithoutScannedAncestor checks that nothing is carried when no
// ancestor within reach is scanned, so every file the scan reads is
// pending. The gate then takes the result of the same file in a scanned
// Commit, and fails closed when there is none.
func TestCurrentWithoutScannedAncestor(t *testing.T) {
	d := dbtest.SQLite(t)
	ctx := t.Context()
	fake := repotest.NewFake(t)
	id := model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "deep"}
	r := indextest.CreateRepo(t, fake, id, model.VisibilityPublic)
	indextest.InsertRepoRow(t, d, r)
	first := indextest.Commit(t, fake, id, map[string][]byte{"evil.pkl": scantest.OSSystemProtocol0()})
	s := scan.New(scan.Options{DB: d, Repos: fake})

	v, err := scan.Current(ctx, s.Results, fake, r.UUID, id, first, nil)
	require.NoError(t, err)
	require.False(t, v.Scanned)
	require.Empty(t, v.From)
	require.Empty(t, v.Files)
	require.True(t, v.Pending("evil.pkl"))

	v, err = scan.Current(ctx, s.Results, nil, r.UUID, id, first, nil)
	require.NoError(t, err, "without a repo.Service nothing is carried")
	require.True(t, v.Pending("evil.pkl"))

	// A scanned ancestor further back than the walk reaches is not used.
	_, err = s.ScanCommit(ctx, r.UUID, id, first)
	require.NoError(t, err)
	head := first
	for i := range 17 {
		head = indextest.Commit(t, fake, id, map[string][]byte{"n.txt": fmt.Appendf(nil, "%d", i)})
	}
	v, err = scan.Current(ctx, s.Results, fake, r.UUID, id, head, nil)
	require.NoError(t, err)
	require.Empty(t, v.From)
	require.True(t, v.Pending("evil.pkl"))
	// Without carried results the gate fails closed, and the same file in
	// a scanned Commit (Gate.Scanned) decides it when one is stored.
	block := func(context.Context) (bool, error) { return true, nil }
	alone := &scan.Gate{Block: block, Results: s.Results, Repos: fake}
	require.Equal(t, scan.BlockNotScanned, blockedReason(t, alone.CheckFile(ctx, r.UUID, id, head, "evil.pkl")))
	g := s.NewGate(block)
	require.Equal(t, scan.BlockUnsafe, blockedReason(t, g.CheckFile(ctx, r.UUID, id, head, "evil.pkl")))
}
