package scan_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/db"
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

// gateFixture is a Repo with blocking on and helpers to read its files.
type gateFixture struct {
	t    *testing.T
	fake *repotest.Fake
	s    *scan.Service
	g    *scan.Gate
	r    *repo.Repo
	id   model.RepoID
}

func newGateFixture(t *testing.T, d *db.DB, name string) *gateFixture {
	t.Helper()
	fake := repotest.NewFake(t)
	id := model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: name}
	r := indextest.CreateRepo(t, fake, id, model.VisibilityPublic)
	indextest.InsertRepoRow(t, d, r)
	s := scan.New(scan.Options{DB: d, Repos: fake, LFS: fake.LFS})
	g := s.NewGate(func(context.Context) (bool, error) { return true, nil })
	return &gateFixture{t: t, fake: fake, s: s, g: g, r: r, id: id}
}

func (f *gateFixture) oid(commit model.CommitSHA, p string) string {
	f.t.Helper()
	e, err := f.fake.Entry(f.t.Context(), f.id, commit, p)
	require.NoError(f.t, err)
	require.NotNil(f.t, e.LFS, p)
	return e.LFS.OID
}

func (f *gateFixture) scanCommit(commit model.CommitSHA) {
	f.t.Helper()
	_, err := f.s.ScanCommit(f.t.Context(), f.r.UUID, f.id, commit)
	require.NoError(f.t, err)
}

func (f *gateFixture) checkObjects(oids ...string) map[string]error {
	f.t.Helper()
	blocked, err := f.g.CheckObjects(f.t.Context(), f.r.UUID, f.id, oids)
	require.NoError(f.t, err)
	return blocked
}

func TestGateRefusesObjectsThatOnlyUnscannedCommitsHold(t *testing.T) {
	f := newGateFixture(t, dbtest.SQLite(t), "history")
	ctx := t.Context()
	// Commit A adds a pickle that runs os.system and safe weights, Commit
	// B deletes both, and only B, the branch head, is scanned.
	a := indextest.Commit(t, f.fake, f.id, map[string][]byte{
		"evil.pkl":            scantest.OSSystemProtocol0(),
		"weights.safetensors": indextest.Safetensors(t, nil),
	})
	evil, st := f.oid(a, "evil.pkl"), f.oid(a, "weights.safetensors")
	_, err := f.fake.Commit(ctx, deleteOp(f.id, "evil.pkl"))
	require.NoError(t, err)
	b, err := f.fake.Commit(ctx, deleteOp(f.id, "weights.safetensors"))
	require.NoError(t, err)
	f.scanCommit(b.SHA)

	require.Equal(t, scan.BlockNotScanned, blockedReason(t, f.g.CheckFile(ctx, f.r.UUID, f.id, a, "evil.pkl")))
	blocked := f.checkObjects(evil, st)
	require.Contains(t, blocked, evil, "the pickle of an unscanned Commit must not download by OID")
	require.Equal(t, scan.BlockNotScanned, blockedReason(t, blocked[evil]))
	var be *scan.BlockedError
	require.ErrorAs(t, blocked[evil], &be)
	require.Equal(t, "evil.pkl", be.Path)
	require.NotContains(t, blocked, st, "a file that cannot run code downloads from history, as it does by path")

	// Once Commit A is scanned the pickle is refused as unsafe, by path and
	// by OID.
	f.scanCommit(a)
	require.Equal(t, scan.BlockUnsafe, blockedReason(t, f.g.CheckFile(ctx, f.r.UUID, f.id, a, "evil.pkl")))
	require.Equal(t, scan.BlockUnsafe, blockedReason(t, f.checkObjects(evil)[evil]))
}

func TestGateRefusesObjectsNoCommitPlaces(t *testing.T) {
	f := newGateFixture(t, dbtest.SQLite(t), "unplaced")
	ctx := t.Context()
	sha := indextest.Commit(t, f.fake, f.id, map[string][]byte{"README.md": []byte("# hi")})
	f.scanCommit(sha)
	obj, err := f.fake.LFS.Put(ctx, scantest.OSSystemProtocol0())
	require.NoError(t, err)
	require.NoError(t, f.fake.SetLFSPresence(ctx, f.id, []repo.LFSRef{{OID: obj.OID, Size: obj.Size, Present: true}}))

	blocked := f.checkObjects(obj.OID)
	require.Equal(t, scan.BlockNotScanned, blockedReason(t, blocked[obj.OID]),
		"an object no reachable Commit holds has no path to say whether it runs code")
}

func TestGateTakesTheResultOfTheSameFileInAScannedCommit(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		f := newGateFixture(t, d, "same-file")
		ctx := t.Context()
		// Commit A adds a safe checkpoint and Commit B, the head, only edits the
		// card. Only B is scanned, as when both arrive in one push.
		a := indextest.Commit(t, f.fake, f.id, map[string][]byte{"model.ckpt": scantest.TorchStateDict(), "README.md": []byte("# v1")})
		ckpt := f.oid(a, "model.ckpt")
		b := indextest.Commit(t, f.fake, f.id, map[string][]byte{"README.md": []byte("# v2")})
		f.scanCommit(b)
		require.NoError(t, f.g.CheckFile(ctx, f.r.UUID, f.id, a, "model.ckpt"), "Commit A's checkpoint is the file B's scan found safe")

		// After a Commit deletes it, the object of an older checkout still
		// downloads by OID: the Commit that added it takes B's result.
		c, err := f.fake.Commit(ctx, deleteOp(f.id, "model.ckpt"))
		require.NoError(t, err)
		f.scanCommit(c.SHA)
		require.Empty(t, f.checkObjects(ckpt))

		// A different file at the same path in an unscanned Commit is not
		// covered by B's result.
		later := indextest.Commit(t, f.fake, f.id, map[string][]byte{"model.ckpt": scantest.SubprocessInst()})
		require.Equal(t, scan.BlockNotScanned, blockedReason(t, f.g.CheckFile(ctx, f.r.UUID, f.id, later, "model.ckpt")))
		other := f.oid(later, "model.ckpt")
		require.Equal(t, scan.BlockNotScanned, blockedReason(t, f.checkObjects(other)[other]))
	})
}

func TestGateRefusesPartialMirrorPickleByOID(t *testing.T) {
	f := newGateFixture(t, dbtest.SQLite(t), "partial")
	ctx := t.Context()
	sha := indextest.Commit(t, f.fake, f.id, map[string][]byte{
		"evil.pkl":            scantest.OSSystemProtocol0(),
		"weights.safetensors": indextest.Safetensors(t, nil),
	})
	evil, st := f.oid(sha, "evil.pkl"), f.oid(sha, "weights.safetensors")
	// The Mirror names both files but holds neither, and its head is
	// scanned without them.
	e, err := f.fake.Entry(ctx, f.id, sha, "evil.pkl")
	require.NoError(t, err)
	s, err := f.fake.Entry(ctx, f.id, sha, "weights.safetensors")
	require.NoError(t, err)
	require.NoError(t, f.fake.SetLFSPresence(ctx, f.id, []repo.LFSRef{{OID: evil, Size: e.Size}, {OID: st, Size: s.Size}}))
	f.scanCommit(sha)

	require.Equal(t, scan.BlockNotScanned, blockedReason(t, f.g.CheckFile(ctx, f.r.UUID, f.id, sha, "evil.pkl")))
	blocked := f.checkObjects(evil, st)
	require.Contains(t, blocked, evil, "by OID (git-lfs batch, then Pull-through) the absent pickle must fail closed")
	require.Equal(t, scan.BlockNotScanned, blockedReason(t, blocked[evil]))
	require.NotContains(t, blocked, st)
}

func TestGateChecksObjectsOfTags(t *testing.T) {
	f := newGateFixture(t, dbtest.SQLite(t), "tags")
	ctx := t.Context()
	a := indextest.Commit(t, f.fake, f.id, map[string][]byte{"evil.pkl": scantest.OSSystemProtocol0()})
	evil := f.oid(a, "evil.pkl")
	require.NoError(t, f.fake.CreateTag(ctx, repo.TagParams{Repo: f.id, Tag: "v1", Revision: string(a), Actor: indextest.Owner("alice")}))
	b, err := f.fake.Commit(ctx, deleteOp(f.id, "evil.pkl"))
	require.NoError(t, err)
	f.scanCommit(b.SHA)

	// The tag's Commit is not scanned yet: its pickle fails closed.
	require.Equal(t, scan.BlockNotScanned, blockedReason(t, f.checkObjects(evil)[evil]))
	// Scanned, it is refused as unsafe.
	f.scanCommit(a)
	require.Equal(t, scan.BlockUnsafe, blockedReason(t, f.checkObjects(evil)[evil]))
}

func TestScannedTakesTheNewestScannerVersion(t *testing.T) {
	d := dbtest.SQLite(t)
	ctx := t.Context()
	f := newGateFixture(t, d, "versions")
	a := model.CommitSHA(strings.Repeat("a", 40))
	b := model.CommitSHA(strings.Repeat("b", 40))
	for _, row := range []db.ScanResult{
		{RepoID: f.r.UUID, Commit: string(a), Path: "w.pkl", ScannerVersion: scan.Version - 1, Status: string(scan.StatusUnsafe), Findings: db.JSON("{}")},
		{RepoID: f.r.UUID, Commit: string(a), Path: "w.pkl", ScannerVersion: scan.Version, Status: string(scan.StatusSafe), Findings: db.JSON("{}")},
		{RepoID: f.r.UUID, Commit: string(b), Path: "w.pkl", ScannerVersion: scan.Version, Status: string(scan.StatusSuspicious), Findings: db.JSON("{}")},
		{RepoID: f.r.UUID, Commit: string(b), Path: "other.pkl", ScannerVersion: scan.Version, Status: string(scan.StatusUnsafe), Findings: db.JSON("{}")},
	} {
		_, err := d.NewInsert().Model(&row).Exec(ctx)
		require.NoError(t, err)
	}
	got, err := f.s.Scanned(ctx, f.r.UUID, "w.pkl")
	require.NoError(t, err)
	require.ElementsMatch(t, []scan.ScannedFile{{Commit: a, Status: scan.StatusSafe}, {Commit: b, Status: scan.StatusSuspicious}}, got)
}

// TestGateBlockLevel checks scan.block_level, decided by James on
// 2026-10-18: unsafe (the default) refuses only files with imports on the
// deny list; suspicious also refuses files with imports on neither list,
// by path, in a tree and by OID, including an object only an older Commit
// holds as a suspicious pickle.
func TestGateBlockLevel(t *testing.T) {
	d := dbtest.SQLite(t)
	ctx := t.Context()
	fake := repotest.NewFake(t)
	id := model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "levels"}
	r := indextest.CreateRepo(t, fake, id, model.VisibilityPublic)
	indextest.InsertRepoRow(t, d, r)
	sha := indextest.Commit(t, fake, id, map[string][]byte{
		"model.joblib": scantest.SklearnSuspicious(),
		"model.ckpt":   scantest.TorchStateDict(),
		"README.md":    []byte("# hi"),
	})
	s := scan.New(scan.Options{DB: d, Repos: fake, LFS: fake.LFS})
	res, err := s.ScanCommit(ctx, r.UUID, id, sha)
	require.NoError(t, err)
	require.Equal(t, scan.StatusSuspicious, res.Status)
	suspicious, err := fake.Entry(ctx, id, sha, "model.joblib")
	require.NoError(t, err)
	ckpt, err := fake.Entry(ctx, id, sha, "model.ckpt")
	require.NoError(t, err)
	// A later Commit renames the suspicious file to a name no loader
	// reads, so only the older Commit says what the object is.
	renamed, err := fake.Commit(ctx, repo.CommitParams{Repo: id, Revision: model.DefaultBranch, Summary: "rename", Actor: indextest.Owner("alice"),
		Operations: []repo.Operation{{Kind: repo.OpAddLFS, Path: "weights.data", OID: suspicious.LFS.OID, Size: suspicious.LFS.Size}, {Kind: repo.OpDelete, Path: "model.joblib"}}})
	require.NoError(t, err)
	_, err = s.ScanCommit(ctx, r.UUID, id, renamed.SHA)
	require.NoError(t, err)

	level := scan.Level("")
	g := s.NewGate(func(context.Context) (bool, error) { return true, nil })
	g.Level = func(context.Context) (scan.Level, error) { return level, nil }

	for _, l := range []scan.Level{"", scan.LevelUnsafe} {
		level = l
		require.NoError(t, g.CheckFile(ctx, r.UUID, id, sha, "model.joblib"), "level %q", l)
		require.NoError(t, g.CheckTree(ctx, r.UUID, id, sha, ""), "level %q", l)
		blocked, err := g.CheckObjects(ctx, r.UUID, id, []string{suspicious.LFS.OID, ckpt.LFS.OID})
		require.NoError(t, err)
		require.Empty(t, blocked, "level %q", l)
	}

	level = scan.LevelSuspicious
	err = g.CheckFile(ctx, r.UUID, id, sha, "model.joblib")
	require.Equal(t, scan.BlockSuspicious, blockedReason(t, err))
	require.Contains(t, err.Error(), "model.joblib imports code that Hangar's security scan does not know to be safe")
	require.Equal(t, scan.BlockSuspicious, blockedReason(t, g.CheckTree(ctx, r.UUID, id, sha, "")))
	require.NoError(t, g.CheckFile(ctx, r.UUID, id, sha, "model.ckpt"), "a safe pickle still downloads")
	require.NoError(t, g.CheckFile(ctx, r.UUID, id, sha, "README.md"))
	blocked, err := g.CheckObjects(ctx, r.UUID, id, []string{suspicious.LFS.OID, ckpt.LFS.OID})
	require.NoError(t, err)
	require.Len(t, blocked, 1)
	require.Equal(t, scan.BlockSuspicious, blockedReason(t, blocked[suspicious.LFS.OID]),
		"the object is suspicious although the branch tip holds it as weights.data")
	require.NoError(t, g.CheckFile(ctx, r.UUID, id, renamed.SHA, "weights.data"), "by path, a name no loader reads downloads")

	flagged, err := s.Flagged(ctx, r.UUID, scan.StatusUnsafe, scan.StatusSuspicious)
	require.NoError(t, err)
	require.Equal(t, []model.CommitSHA{sha}, flagged)
	flagged, err = s.Flagged(ctx, r.UUID)
	require.NoError(t, err)
	require.Empty(t, flagged, "without statuses Flagged lists Commits with unsafe files only")

	// A failing level read fails the check rather than allowing it.
	g.Level = func(context.Context) (scan.Level, error) { return "", errors.New("settings unavailable") }
	require.Error(t, g.CheckFile(ctx, r.UUID, id, sha, "README.md"))

	got, ok := scan.ParseLevel("suspicious")
	require.True(t, ok)
	require.Equal(t, scan.LevelSuspicious, got)
	_, ok = scan.ParseLevel("everything")
	require.False(t, ok)
	require.Contains(t, (&scan.BlockedError{Reason: scan.BlockSuspicious}).Error(), "An admin blocks suspicious downloads")
}
