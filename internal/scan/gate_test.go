package scan_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/db/dbtest"
	"github.com/jamesagarside/hangar/internal/index/indextest"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/repo"
	"github.com/jamesagarside/hangar/internal/repo/repotest"
	"github.com/jamesagarside/hangar/internal/scan"
	"github.com/jamesagarside/hangar/internal/scan/scantest"
)

func blockedReason(t *testing.T, err error) scan.BlockReason {
	t.Helper()
	var be *scan.BlockedError
	require.ErrorAs(t, err, &be)
	return be.Reason
}

func TestGateRefusesUnsafeAndUnscannedFilesThatRunCode(t *testing.T) {
	d := dbtest.SQLite(t)
	ctx := t.Context()
	fake := repotest.NewFake(t)
	id := model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "pickles"}
	r := indextest.CreateRepo(t, fake, id, model.VisibilityPublic)
	indextest.InsertRepoRow(t, d, r)
	sha := indextest.Commit(t, fake, id, map[string][]byte{
		"evil.pkl":            scantest.OSSystemProtocol0(),
		"model.ckpt":          scantest.TorchStateDict(),
		"openvino/model.bin":  []byte("raw weights"),
		"openvino/model.xml":  []byte("<net/>"),
		"README.md":           []byte("# hi"),
		"weights.safetensors": indextest.Safetensors(t, nil),
	})
	s := scan.New(scan.Options{DB: d, Repos: fake, LFS: fake.LFS})
	block := false
	g := s.NewGate(func(context.Context) (bool, error) { return block, nil })

	// Off: nothing is checked, scanned or not.
	require.NoError(t, g.CheckFile(ctx, r.UUID, id, sha, "evil.pkl"))
	require.NoError(t, g.CheckTree(ctx, r.UUID, id, sha, ""))
	blocked, err := g.CheckObjects(ctx, r.UUID, id, []string{"x"})
	require.NoError(t, err)
	require.Empty(t, blocked)

	// On, before the scan: files that can run code fail closed, others
	// download.
	block = true
	require.Equal(t, scan.BlockNotScanned, blockedReason(t, g.CheckFile(ctx, r.UUID, id, sha, "evil.pkl")))
	require.Equal(t, scan.BlockNotScanned, blockedReason(t, g.CheckFile(ctx, r.UUID, id, sha, "model.ckpt")))
	require.NoError(t, g.CheckFile(ctx, r.UUID, id, sha, "README.md"))
	require.NoError(t, g.CheckFile(ctx, r.UUID, id, sha, "weights.safetensors"))
	require.Equal(t, scan.BlockNotScanned, blockedReason(t, g.CheckTree(ctx, r.UUID, id, sha, "")))
	entry := func(p string) model.Entry {
		e, err := fake.Entry(ctx, id, sha, p)
		require.NoError(t, err)
		return e
	}
	evil, ckpt, st := entry("evil.pkl").LFS.OID, entry("model.ckpt").LFS.OID, entry("weights.safetensors").LFS.OID
	blocked, err = g.CheckObjects(ctx, r.UUID, id, []string{evil, ckpt, st})
	require.NoError(t, err)
	require.Len(t, blocked, 2, "an unscanned branch head refuses its objects that can run code")
	require.Equal(t, scan.BlockNotScanned, blockedReason(t, blocked[evil]))

	// After the scan only the unsafe file is refused, by path and by OID.
	_, err = s.ScanCommit(ctx, r.UUID, id, sha)
	require.NoError(t, err)
	err = g.CheckFile(ctx, r.UUID, id, sha, "evil.pkl")
	require.Equal(t, scan.BlockUnsafe, blockedReason(t, err))
	require.True(t, scan.IsBlocked(err))
	require.Contains(t, err.Error(), "evil.pkl is marked unsafe")
	require.NoError(t, g.CheckFile(ctx, r.UUID, id, sha, "model.ckpt"))
	require.NoError(t, g.CheckFile(ctx, r.UUID, id, sha, "openvino/model.bin"), "OpenVINO weights are not pickles")
	require.Equal(t, scan.BlockUnsafe, blockedReason(t, g.CheckTree(ctx, r.UUID, id, sha, "")))
	require.NoError(t, g.CheckTree(ctx, r.UUID, id, sha, "openvino"), "a folder without the unsafe file exports")
	blocked, err = g.CheckObjects(ctx, r.UUID, id, []string{evil, ckpt, st})
	require.NoError(t, err)
	require.Len(t, blocked, 1)
	require.Equal(t, scan.BlockUnsafe, blockedReason(t, blocked[evil]))

	// A later Commit that removes the file still refuses the object: an
	// older Commit with stored results references it.
	sha2, err := fake.Commit(ctx, deleteOp(id, "evil.pkl"))
	require.NoError(t, err)
	_, err = s.ScanCommit(ctx, r.UUID, id, sha2.SHA)
	require.NoError(t, err)
	blocked, err = g.CheckObjects(ctx, r.UUID, id, []string{evil})
	require.NoError(t, err)
	require.Contains(t, blocked, evil)
	flagged, err := s.Flagged(ctx, r.UUID)
	require.NoError(t, err)
	require.Equal(t, []model.CommitSHA{sha}, flagged)

	// A failing setting read fails the check rather than allowing it.
	failing := s.NewGate(func(context.Context) (bool, error) { return false, errors.New("settings unavailable") })
	require.Error(t, failing.CheckFile(ctx, r.UUID, id, sha, "README.md"))
	var nilGate *scan.Gate
	require.NoError(t, nilGate.CheckFile(ctx, uuid.New(), id, sha, "evil.pkl"), "no gate never blocks")
}

func TestBlockedErrorMessages(t *testing.T) {
	require.Equal(t, "w.pkl is marked unsafe by Hangar's security scan: loading it can run code. An admin blocks unsafe downloads on this Instance.",
		(&scan.BlockedError{Path: "w.pkl", Reason: scan.BlockUnsafe}).Error())
	require.Contains(t, (&scan.BlockedError{Path: "w.pkl", Reason: scan.BlockNotScanned}).Error(), "w.pkl has not been scanned")
	require.Contains(t, (&scan.BlockedError{Reason: scan.BlockUnsafe}).Error(), "This file is marked unsafe")
	require.False(t, scan.IsBlocked(errors.New("other")))
}

func deleteOp(id model.RepoID, p string) repo.CommitParams {
	return repo.CommitParams{Repo: id, Revision: model.DefaultBranch, Summary: "remove " + p, Actor: indextest.Owner(id.Namespace),
		Operations: []repo.Operation{{Kind: repo.OpDelete, Path: p}}}
}
