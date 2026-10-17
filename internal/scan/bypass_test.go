package scan_test

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/pierrec/lz4/v4"
	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/db"
	"github.com/jamesagarside/hangar/internal/db/dbtest"
	"github.com/jamesagarside/hangar/internal/index/indextest"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/repo"
	"github.com/jamesagarside/hangar/internal/scan"
	"github.com/jamesagarside/hangar/internal/scan/scantest"
)

// The tests in this file are regressions for files that could run code
// when loaded while the scan reported them safe, or reported nothing, so
// scan.block_unsafe_downloads let them through (phase 3 security review).

func gzipped(b []byte) []byte {
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	_, _ = w.Write(b)
	_ = w.Close()
	return buf.Bytes()
}

func zstded(b []byte) []byte {
	enc, _ := zstd.NewWriter(nil)
	return enc.EncodeAll(b, nil)
}

func lz4ed(b []byte) []byte {
	var buf bytes.Buffer
	w := lz4.NewWriter(&buf)
	_, _ = w.Write(b)
	_ = w.Close()
	return buf.Bytes()
}

func zipped(members map[string][]byte) []byte {
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for name, content := range members {
		fw, _ := w.Create(name)
		_, _ = fw.Write(content)
	}
	_ = w.Close()
	return buf.Bytes()
}

func tarred(name string, content []byte) []byte {
	var buf bytes.Buffer
	w := tar.NewWriter(&buf)
	_ = w.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content))})
	_, _ = w.Write(content)
	_ = w.Close()
	return buf.Bytes()
}

func TestScanFileReadsLoadableFilesByContent(t *testing.T) {
	evil := scantest.OSSystemProtocol0()
	cases := []struct {
		path    string
		data    []byte
		status  scan.Status
		partial bool
	}{
		// joblib.dump and pandas.to_pickle compress by extension, and
		// joblib.load and pandas.read_pickle decompress.
		{"model.joblib.gz", gzipped(evil), scan.StatusUnsafe, false},
		{"frame.pkl.gz", gzipped(evil), scan.StatusUnsafe, false},
		{"frame.pkl.zst", zstded(evil), scan.StatusUnsafe, false},
		{"model.pkl.lz4", lz4ed(evil), scan.StatusUnsafe, false},
		{"model.pkl.xz", []byte{0xfd, '7', 'z', 'X', 'Z', 0, 1, 2}, scan.StatusError, true},
		{"model.joblib", []byte{0x5d, 0x00, 0x00, 0x80, 0x00}, scan.StatusError, true},
		// A gzip file named .bin is still decompressed and checked.
		{"weights.bin", gzipped(evil), scan.StatusUnsafe, false},
		// pandas reads the one member of a zip, whatever its name.
		{"frame.pkl.zip", zipped(map[string][]byte{"data": evil}), scan.StatusUnsafe, false},
		// numpy.load(allow_pickle=True) reads .npz members and unpickles a
		// file without the .npy magic.
		{"arrays.npz", zipped(map[string][]byte{"a.npy": scantest.NumpyObjectArray(evil)}), scan.StatusUnsafe, false},
		{"array.npy", evil, scan.StatusUnsafe, false},
		{"object.npy", scantest.NumpyObjectArray(evil), scan.StatusUnsafe, false},
		// torch.save checkpoints are often named .pth.tar.
		{"checkpoint.pth.tar", evil, scan.StatusUnsafe, false},
		// Old PyTorch tar archives are unpickled member by member.
		{"old.pt", tarred("pickle", evil), scan.StatusError, true},
		// joblib.load skips array data that pickle.load reads as opcodes.
		{"model.joblib", scantest.JoblibHiddenOSSystem(), scan.StatusUnsafe, false},
		{"model.pkl", scantest.JoblibHiddenOSSystem(), scan.StatusUnsafe, false},
		// Data that is not a pickle: loading it runs nothing.
		{"openvino_model.bin", []byte("raw weights"), scan.StatusSafe, false},
		{"plain.npy", scantest.NumpyFloatArray(), scan.StatusSafe, false},
		// A pickle that stops part-way: every loader stops there too.
		{"truncated.pkl", []byte("(cos\nsys"), scan.StatusError, false},
	}
	s := scan.New(scan.Options{})
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			fr, err := s.ScanFile(t.Context(), c.path, bytes.NewReader(c.data))
			require.NoError(t, err)
			require.Equal(t, c.status, fr.Status, "%+v", fr)
			require.Equal(t, c.partial, fr.Partial, "%+v", fr)
		})
	}
}

// TestScanFileReadsRealJoblibFiles scans joblib.dump output: arrays of
// every common dtype, object arrays, protocols 2 to 5 and zlib, which must
// scan in full.
func TestScanFileReadsRealJoblibFiles(t *testing.T) {
	files, err := filepath.Glob("pickle/testdata/joblib/*.joblib")
	require.NoError(t, err)
	require.NotEmpty(t, files)
	s := scan.New(scan.Options{})
	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			raw, err := os.ReadFile(f)
			require.NoError(t, err)
			fr, err := s.ScanFile(t.Context(), filepath.Base(f), bytes.NewReader(raw))
			require.NoError(t, err)
			require.False(t, fr.Partial, "%+v", fr)
			require.Empty(t, fr.Error)
			require.NotEqual(t, scan.StatusUnsafe, fr.Status)
		})
	}
}

func TestKindOfCompressedAndArchivedNames(t *testing.T) {
	for p, want := range map[string]scan.Kind{
		"model.joblib.gz": scan.KindJoblib, "df.pkl.bz2": scan.KindPickle, "df.pkl.xz": scan.KindPickle,
		"m.pkl.z": scan.KindPickle, "m.pkl.lzma": scan.KindPickle, "m.pkl.zst": scan.KindPickle, "m.pkl.lz4": scan.KindPickle,
		"m.pkl.zip": scan.KindPickle, "checkpoint.pth.tar": scan.KindPickle, "a.npz": scan.KindNumpy, "a.npy.gz": scan.KindNumpy,
		"data.tar": scan.KindNone, "logs.txt.gz": scan.KindNone, "archive.zip": scan.KindNone,
	} {
		require.Equal(t, want, scan.KindOf(p), p)
	}
}

func TestGateRefusesPartialScans(t *testing.T) {
	f := newGateFixture(t, dbtest.SQLite(t), "partial-scans")
	ctx := t.Context()
	sha := indextest.Commit(t, f.fake, f.id, map[string][]byte{
		"xz.joblib":     {0xfd, '7', 'z', 'X', 'Z', 0, 1, 2},
		"old.pt":        tarred("pickle", scantest.OSSystemProtocol0()),
		"truncated.pkl": []byte("(cos\nsys"),
		"model.joblib":  scantest.JoblibHiddenOSSystem(),
	})
	f.scanCommit(sha)
	require.Equal(t, scan.BlockPartial, blockedReason(t, f.g.CheckFile(ctx, f.r.UUID, f.id, sha, "xz.joblib")))
	require.Equal(t, scan.BlockPartial, blockedReason(t, f.g.CheckFile(ctx, f.r.UUID, f.id, sha, "old.pt")))
	err := f.g.CheckFile(ctx, f.r.UUID, f.id, sha, "old.pt")
	require.Contains(t, err.Error(), "old.pt could not be read in full")
	require.NoError(t, f.g.CheckFile(ctx, f.r.UUID, f.id, sha, "truncated.pkl"), "every loader stops where the scan stopped")
	require.Equal(t, scan.BlockUnsafe, blockedReason(t, f.g.CheckFile(ctx, f.r.UUID, f.id, sha, "model.joblib")))
	xz, old := f.oid(sha, "xz.joblib"), f.oid(sha, "old.pt")
	blocked := f.checkObjects(xz, old, f.oid(sha, "truncated.pkl"))
	require.Len(t, blocked, 2)
	require.Equal(t, scan.BlockPartial, blockedReason(t, blocked[xz]))
}

// TestGateTreatsErrorsOfOlderScannersAsPartial checks results stored by
// scanner version 1, which did not record Partial: an error there may hide
// code the scan never read, so the Gate fails closed until the rescan.
func TestGateTreatsErrorsOfOlderScannersAsPartial(t *testing.T) {
	d := dbtest.SQLite(t)
	f := newGateFixture(t, d, "old-errors")
	ctx := t.Context()
	sha := indextest.Commit(t, f.fake, f.id, map[string][]byte{"model.joblib": []byte("x"), "w.safetensors": []byte("x")})
	for _, row := range []db.ScanResult{
		{RepoID: f.r.UUID, Commit: string(sha), Path: "", ScannerVersion: 1, Status: string(scan.StatusSuspicious), Findings: db.JSON("[]")},
		{RepoID: f.r.UUID, Commit: string(sha), Path: "model.joblib", ScannerVersion: 1, Status: string(scan.StatusSuspicious),
			Findings: db.JSON(`{"findings":[{"module":"sklearn","name":"X","status":"suspicious"}],"error":"invalid pickle: unknown opcode 0x09"}`)},
		{RepoID: f.r.UUID, Commit: string(sha), Path: "w.safetensors", ScannerVersion: 1, Status: string(scan.StatusError),
			Findings: db.JSON(`{"findings":[],"error":"bad header"}`)},
	} {
		_, err := d.NewInsert().Model(&row).Exec(ctx)
		require.NoError(t, err)
	}
	require.Equal(t, scan.BlockPartial, blockedReason(t, f.g.CheckFile(ctx, f.r.UUID, f.id, sha, "model.joblib")))
	require.NoError(t, f.g.CheckFile(ctx, f.r.UUID, f.id, sha, "w.safetensors"), "a file that cannot run code is never partial")
	res, err := f.s.Results(ctx, f.r.UUID, sha)
	require.NoError(t, err)
	require.True(t, res.Files[0].Partial)
	files, err := f.s.Scanned(ctx, f.r.UUID, "model.joblib")
	require.NoError(t, err)
	require.Equal(t, []scan.ScannedFile{{Commit: sha, Status: scan.StatusSuspicious, Partial: true}}, files)
}

// TestGateChecksEveryCommitThatHoldsAnObject is the regression for
// CheckObjects deciding an object only from the ref tips that hold it: the
// tip holds a pickle's bytes under a name that does not run code, while an
// older Commit holds them as evil.pkl, which git-lfs checks out when a
// client checks out that Commit.
func TestGateChecksEveryCommitThatHoldsAnObject(t *testing.T) {
	f := newGateFixture(t, dbtest.SQLite(t), "older-commit")
	ctx := t.Context()
	a := indextest.Commit(t, f.fake, f.id, map[string][]byte{"evil.pkl": scantest.OSSystemProtocol0()})
	evil := f.oid(a, "evil.pkl")
	_, err := f.fake.Commit(ctx, deleteOp(f.id, "evil.pkl"))
	require.NoError(t, err)
	b := indextest.Commit(t, f.fake, f.id, map[string][]byte{"weights.safetensors": scantest.OSSystemProtocol0()})
	require.Equal(t, evil, f.oid(b, "weights.safetensors"), "the same bytes are the same LFS object")
	f.scanCommit(b)

	require.Equal(t, scan.BlockNotScanned, blockedReason(t, f.checkObjects(evil)[evil]),
		"the older Commit that holds the object as a pickle is not scanned")
	f.scanCommit(a)
	require.Equal(t, scan.BlockUnsafe, blockedReason(t, f.checkObjects(evil)[evil]))
}

// TestGateTakesAnObjectsResultFromAnyScannedPickle checks that an object
// a scanned tip holds as a file that can run code is decided by that
// result, so a rename in an unscanned Commit does not refuse it.
func TestGateTakesAnObjectsResultFromAnyScannedPickle(t *testing.T) {
	f := newGateFixture(t, dbtest.SQLite(t), "renamed")
	ctx := t.Context()
	a := indextest.Commit(t, f.fake, f.id, map[string][]byte{"old-name.ckpt": scantest.TorchStateDict()})
	ckpt := f.oid(a, "old-name.ckpt")
	_, err := f.fake.Commit(ctx, deleteOp(f.id, "old-name.ckpt"))
	require.NoError(t, err)
	b := indextest.Commit(t, f.fake, f.id, map[string][]byte{"model.ckpt": scantest.TorchStateDict()})
	f.scanCommit(b)
	require.Empty(t, f.checkObjects(ckpt), "the tip's scan of the same bytes decides the object")
}

// gitPickleFixture commits a pickle stored in git rather than LFS, as a
// Repo whose .gitattributes does not track it does.
func gitPickleFixture(t *testing.T, f *gateFixture, p string, content []byte) model.CommitSHA {
	t.Helper()
	ctx := t.Context()
	_, err := f.fake.Commit(ctx, repo.CommitParams{Repo: f.id, Revision: model.DefaultBranch, Summary: "no LFS", Actor: indextest.Owner(f.id.Namespace),
		Operations: []repo.Operation{{Kind: repo.OpAdd, Path: ".gitattributes", Content: strings.NewReader("*.txt -text\n")}}})
	require.NoError(t, err)
	res, err := f.fake.Commit(ctx, repo.CommitParams{Repo: f.id, Revision: model.DefaultBranch, Summary: "add " + p, Actor: indextest.Owner(f.id.Namespace),
		Operations: []repo.Operation{{Kind: repo.OpAdd, Path: p, Content: bytes.NewReader(content)}}})
	require.NoError(t, err)
	e, err := f.fake.Entry(ctx, f.id, res.SHA, p)
	require.NoError(t, err)
	require.Nil(t, e.LFS, "the pickle is a git blob")
	return res.SHA
}

func TestGateChecksGitFetches(t *testing.T) {
	ctx := context.Background()
	t.Run("a pickle in git at the tip", func(t *testing.T) {
		f := newGateFixture(t, dbtest.SQLite(t), "git-tip")
		sha := gitPickleFixture(t, f, "evil.pkl", scantest.OSSystemProtocol0())
		require.Equal(t, scan.BlockNotScanned, blockedReason(t, f.g.CheckGit(ctx, f.r.UUID, f.id)))
		f.scanCommit(sha)
		require.Equal(t, scan.BlockUnsafe, blockedReason(t, f.g.CheckGit(ctx, f.r.UUID, f.id)))
	})
	t.Run("a pickle in git only in history", func(t *testing.T) {
		f := newGateFixture(t, dbtest.SQLite(t), "git-history")
		a := gitPickleFixture(t, f, "evil.pkl.lz4", lz4ed(scantest.OSSystemProtocol0()))
		b, err := f.fake.Commit(ctx, deleteOp(f.id, "evil.pkl.lz4"))
		require.NoError(t, err)
		f.scanCommit(b.SHA)
		require.Equal(t, scan.BlockNotScanned, blockedReason(t, f.g.CheckGit(ctx, f.r.UUID, f.id)),
			"a clone holds every reachable Commit")
		f.scanCommit(a)
		require.Equal(t, scan.BlockUnsafe, blockedReason(t, f.g.CheckGit(ctx, f.r.UUID, f.id)))
	})
	t.Run("safe files and LFS pickles", func(t *testing.T) {
		f := newGateFixture(t, dbtest.SQLite(t), "git-safe")
		sha := indextest.Commit(t, f.fake, f.id, map[string][]byte{"evil.pkl": scantest.OSSystemProtocol0(), "README.md": []byte("# hi")})
		safe := gitPickleFixture(t, f, "model.ckpt", scantest.TorchStateDict())
		f.scanCommit(sha)
		f.scanCommit(safe)
		require.NoError(t, f.g.CheckGit(ctx, f.r.UUID, f.id), "LFS files are not in the pack; git-lfs checks them by OID")
	})
	t.Run("off", func(t *testing.T) {
		f := newGateFixture(t, dbtest.SQLite(t), "git-off")
		gitPickleFixture(t, f, "evil.pkl", scantest.OSSystemProtocol0())
		off := f.s.NewGate(func(context.Context) (bool, error) { return false, nil })
		require.NoError(t, off.CheckGit(ctx, f.r.UUID, f.id))
	})
}
