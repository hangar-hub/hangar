package scan_test

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/blob"
	"github.com/jamesagarside/hangar/internal/db"
	"github.com/jamesagarside/hangar/internal/db/dbtest"
	"github.com/jamesagarside/hangar/internal/index"
	"github.com/jamesagarside/hangar/internal/index/gguf"
	"github.com/jamesagarside/hangar/internal/index/indextest"
	"github.com/jamesagarside/hangar/internal/index/safetensors"
	"github.com/jamesagarside/hangar/internal/jobs"
	"github.com/jamesagarside/hangar/internal/jobs/jobstest"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/repo"
	"github.com/jamesagarside/hangar/internal/repo/repotest"
	"github.com/jamesagarside/hangar/internal/scan"
	"github.com/jamesagarside/hangar/internal/scan/scantest"
	"github.com/jamesagarside/hangar/internal/webhook"
	"github.com/jamesagarside/hangar/internal/webhook/webhooktest"
)

func TestScanFileByType(t *testing.T) {
	gz := func(b []byte) []byte {
		var buf bytes.Buffer
		w := gzip.NewWriter(&buf)
		_, _ = w.Write(b)
		_ = w.Close()
		return buf.Bytes()
	}
	deflated := func() []byte {
		var b bytes.Buffer
		w := zip.NewWriter(&b)
		fw, _ := w.CreateHeader(&zip.FileHeader{Name: "archive/data.pkl", Method: zip.Deflate})
		_, _ = fw.Write(scantest.OSSystemProtocol0())
		_ = w.Close()
		return b.Bytes()
	}
	st, _ := safetensors.Encode(nil, []safetensors.Tensor{{Name: "w", DType: "F32", Shape: []int64{2}}})
	g, _ := gguf.Encode([]gguf.KV{{Key: "general.architecture", Value: "llama"}}, []gguf.EncodeTensor{{Name: "t", Type: "F32", Dims: []uint64{8}}})
	cases := []struct {
		path   string
		data   []byte
		status scan.Status
	}{
		{"pytorch_model.bin", scantest.TorchZip(scantest.TorchStateDict(), 4096), scan.StatusSafe},
		{"evil.bin", scantest.TorchZip(scantest.OSSystemProtocol0(), 10), scan.StatusUnsafe},
		{"evil-deflate.pt", deflated(), scan.StatusUnsafe},
		{"descriptor.pt", scantest.ZipWithDescriptors(scantest.TorchStateDict()), scan.StatusError},
		{"legacy.pt", scantest.LegacyTorch(scantest.PosixSystemStackGlobal()), scan.StatusUnsafe},
		{"model.ckpt", scantest.TorchStateDict(), scan.StatusSafe},
		{"weights.pth", scantest.SklearnSuspicious(), scan.StatusSuspicious},
		{"data.pkl", scantest.SubprocessInst(), scan.StatusUnsafe},
		{"data.pickle", scantest.PartialSafe(), scan.StatusSuspicious},
		{"objects.npy", scantest.NumpyObjectArray(scantest.BuiltinsEval()), scan.StatusUnsafe},
		{"floats.npy", scantest.NumpyFloatArray(), scan.StatusSafe},
		{"bad.npy", []byte("not numpy"), scan.StatusError},
		{"model.joblib", scantest.Joblib(scantest.SklearnSuspicious(), false), scan.StatusSuspicious},
		{"zlib.joblib", scantest.Joblib(scantest.OSSystemProtocol0(), true), scan.StatusUnsafe},
		{"gzip.joblib", gz(scantest.PartialOSSystem()), scan.StatusUnsafe},
		{"xz.joblib", []byte{0xfd, '7', 'z', 'X', 'Z', 0, 1, 2}, scan.StatusError},
		{"lz4.joblib", []byte{0x04, 0x22, 0x4d, 0x18, 0, 0}, scan.StatusError},
		{"random.bin", []byte("\x00\x01\x02"), scan.StatusError},
		{"empty.pt", []byte("PK\x05\x06" + string(make([]byte, 18))), scan.StatusError},
		{"model.safetensors", st, scan.StatusSafe},
		{"broken.safetensors", st[:len(st)-1], scan.StatusError},
		{"model.gguf", g, scan.StatusSafe},
		{"broken.gguf", g[:len(g)-4], scan.StatusError},
		{"README.md", []byte("# hi"), scan.StatusSafe},
	}
	s := scan.New(scan.Options{})
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			fr, err := s.ScanFile(t.Context(), c.path, bytes.NewReader(c.data))
			require.NoError(t, err)
			require.Equal(t, c.status, fr.Status, "%+v", fr)
			require.Equal(t, c.path, fr.Path)
		})
	}
}

// countingRepos counts the bytes read through OpenFile.
type countingRepos struct {
	*repotest.Fake
	read atomic.Int64
}

type countingReader struct {
	io.ReadCloser
	n *atomic.Int64
}

func (c countingReader) Read(p []byte) (int, error) {
	n, err := c.ReadCloser.Read(p)
	c.n.Add(int64(n))
	return n, err
}

func (c *countingRepos) OpenFile(ctx context.Context, id model.RepoID, commit model.CommitSHA, p string, rng *blob.Range) (io.ReadCloser, model.Entry, error) {
	rc, e, err := c.Fake.OpenFile(ctx, id, commit, p, rng)
	if err != nil {
		return nil, e, err
	}
	return countingReader{ReadCloser: rc, n: &c.read}, e, nil
}

// lfsSizes reports a different stored size for one object, and none for
// another, to exercise the LFS pointer check.
type lfsSizes struct {
	repo.LFSStore
	wrongSize, missing string
}

func (l lfsSizes) Stat(ctx context.Context, oid string) (repo.LFSObject, error) {
	obj, err := l.LFSStore.Stat(ctx, oid)
	switch oid {
	case l.wrongSize:
		obj.Size++
	case l.missing:
		return repo.LFSObject{}, repo.ErrNotFound
	}
	return obj, err
}

func TestScanCommitStoresResults(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		ctx := t.Context()
		fake := repotest.NewFake(t)
		repos := &countingRepos{Fake: fake}
		id := model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "mixed"}
		r := indextest.CreateRepo(t, fake, id, model.VisibilityPublic)
		indextest.InsertRepoRow(t, d, r)
		big := scantest.TorchZip(scantest.TorchStateDict(), 4<<20)
		sha := indextest.Commit(t, fake, id, map[string][]byte{
			"pytorch_model.bin":  big,
			"legacy.pt":          scantest.LegacyTorch(scantest.OSSystemProtocol0()),
			"openvino_model.bin": []byte("raw weights"),
			"openvino_model.xml": []byte("<net/>"),
			"weights.npy":        scantest.NumpyFloatArray(),
			"wrong.safetensors":  indextest.Safetensors(t, nil, safetensors.Tensor{Name: "a", DType: "F32", Shape: []int64{1}}),
			"gone.onnx":          []byte("onnx"),
			"absent.pkl":         scantest.SubprocessInst(),
			"README.md":          []byte("# card"),
		})
		entry := func(p string) model.Entry {
			e, err := fake.Entry(ctx, id, sha, p)
			require.NoError(t, err)
			return e
		}
		require.NoError(t, fake.SetLFSPresence(ctx, id, []repo.LFSRef{{OID: entry("absent.pkl").LFS.OID, Size: entry("absent.pkl").Size, Present: false}}))
		hooks := &webhooktest.Fake{}
		s := scan.New(scan.Options{DB: d, Repos: repos, Webhooks: hooks, Window: 64 << 10,
			LFS: lfsSizes{LFSStore: fake.LFS, wrongSize: entry("wrong.safetensors").LFS.OID, missing: entry("gone.onnx").LFS.OID}})

		_, err := s.Results(ctx, r.UUID, sha)
		require.ErrorIs(t, err, scan.ErrNotScanned)

		reg := jobs.NewRegistry()
		require.NoError(t, s.Register(reg))
		q := jobstest.NewFake(reg)
		job := index.CommitJob{RepoUUID: r.UUID, Repo: id.String(), Type: id.Type, Commit: sha}
		jid, err := q.Enqueue(ctx, jobs.NewJob{Kind: jobs.KindScan, DedupeKey: job.DedupeKey(), Payload: job})
		require.NoError(t, err)
		q.RunPending(ctx)
		j, err := q.Get(ctx, jid)
		require.NoError(t, err)
		require.Equal(t, jobs.StatusSucceeded, j.Status, j.Error)
		require.Less(t, repos.read.Load(), int64(len(big)), "tensor data in zip archives is skipped")

		res, err := s.Results(ctx, r.UUID, sha)
		require.NoError(t, err)
		require.Equal(t, scan.StatusUnsafe, res.Status)
		status := map[string]scan.Status{}
		for _, f := range res.Files {
			status[f.Path] = f.Status
		}
		require.Equal(t, map[string]scan.Status{
			"pytorch_model.bin": scan.StatusSafe, "legacy.pt": scan.StatusUnsafe, "weights.npy": scan.StatusSafe,
			"wrong.safetensors": scan.StatusError, "gone.onnx": scan.StatusError,
		}, status)
		for _, f := range res.Files {
			if f.Path == "legacy.pt" {
				require.Equal(t, []scan.Finding{{Module: "os", Name: "system", Opcode: "GLOBAL", Status: scan.StatusUnsafe}}, f.Findings)
			}
			if f.Path == "wrong.safetensors" {
				require.Contains(t, f.Error, "LFS pointer says")
			}
		}
		fr, err := s.FileResult(ctx, r.UUID, sha, "legacy.pt")
		require.NoError(t, err)
		require.Equal(t, scan.StatusUnsafe, fr.Status)
		fr, err = s.FileResult(ctx, r.UUID, sha, "README.md")
		require.NoError(t, err)
		require.Equal(t, scan.StatusSafe, fr.Status)

		require.Equal(t, []webhook.EventName{webhook.ScanCompleted}, hooks.Names())
		ev := hooks.Events()[0]
		require.Equal(t, "unsafe", ev.Data["status"])
		raw, _ := json.Marshal(ev.Data["files"])
		require.Contains(t, string(raw), "legacy.pt")

		// Scanning again replaces the rows.
		_, err = s.ScanCommit(ctx, r.UUID, id, sha)
		require.NoError(t, err)
		again, err := s.Results(ctx, r.UUID, sha)
		require.NoError(t, err)
		require.Len(t, again.Files, len(res.Files))
	})
}

func TestScanCommitWithNothingToScan(t *testing.T) {
	d := dbtest.SQLite(t)
	ctx := t.Context()
	fake := repotest.NewFake(t)
	id := model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "text"}
	r := indextest.CreateRepo(t, fake, id, model.VisibilityPublic)
	indextest.InsertRepoRow(t, d, r)
	sha := indextest.Commit(t, fake, id, map[string][]byte{"README.md": []byte("# hi")})
	s := scan.New(scan.Options{DB: d, Repos: fake})
	res, err := s.ScanCommit(ctx, r.UUID, id, sha)
	require.NoError(t, err)
	require.Equal(t, scan.StatusSafe, res.Status)
	got, err := s.Results(ctx, r.UUID, sha)
	require.NoError(t, err, "a Commit with nothing to scan still has results")
	require.Equal(t, scan.StatusSafe, got.Status)
	require.Empty(t, got.Files)
	_, err = s.FileResult(ctx, r.UUID, model.CommitSHA("1234567890123456789012345678901234567890"), "x")
	require.ErrorIs(t, err, scan.ErrNotScanned)

	run := func(payload string) error {
		return s.HandleJob(ctx, &jobs.Job{Payload: []byte(payload)}, nil)
	}
	require.True(t, jobs.IsPermanent(run("{")))
	require.True(t, jobs.IsPermanent(run(`{"commit":"x"}`)))
	require.NoError(t, run(`{"repo_uuid":"`+uuid.NewString()+`","commit":"`+string(sha)+`"}`))
	require.True(t, jobs.IsPermanent(run(`{"repo_uuid":"`+r.UUID.String()+`","commit":"1234567890123456789012345678901234567890"}`)))
	require.NoError(t, fake.Delete(ctx, id, indextest.Owner("alice")))
	require.NoError(t, run(`{"repo_uuid":"`+r.UUID.String()+`","commit":"`+string(sha)+`"}`))
}

func TestKindOfAndWorst(t *testing.T) {
	require.Equal(t, scan.KindPickle, scan.KindOf("a/b/MODEL.BIN"))
	require.Equal(t, scan.KindJoblib, scan.KindOf("m.joblib"))
	require.Equal(t, scan.KindNumpy, scan.KindOf("m.npy"))
	require.Equal(t, scan.KindSafetensors, scan.KindOf("m.safetensors"))
	require.Equal(t, scan.KindGGUF, scan.KindOf("m.gguf"))
	require.Equal(t, scan.KindNone, scan.KindOf("m.txt"))
	require.Equal(t, scan.StatusUnsafe, scan.Worst(scan.StatusSuspicious, scan.StatusUnsafe))
	require.Equal(t, scan.StatusSuspicious, scan.Worst(scan.StatusError, scan.StatusSuspicious))
}

func FuzzScanFile(f *testing.F) {
	f.Add("model.bin", scantest.TorchZip(scantest.TorchStateDict(), 16))
	f.Add("model.bin", scantest.ZipWithDescriptors(scantest.OSSystemProtocol0()))
	f.Add("x.npy", scantest.NumpyObjectArray(scantest.OSSystemProtocol0()))
	f.Add("x.joblib", scantest.Joblib(scantest.SklearnSuspicious(), true))
	f.Add("x.pkl", scantest.LegacyTorch(scantest.TorchStateDict()))
	s := scan.New(scan.Options{})
	f.Fuzz(func(t *testing.T, name string, data []byte) {
		fr, err := s.ScanFile(context.Background(), name, bytes.NewReader(data))
		require.NoError(t, err)
		for _, finding := range fr.Findings {
			require.Equal(t, scan.Worst(fr.Status, finding.Status), fr.Status, "the file status is the worst finding")
		}
	})
}

func TestRescanStaleQueuesBranchHeadsOnly(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		ctx := t.Context()
		fake := repotest.NewFake(t)
		id := model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "stale"}
		r := indextest.CreateRepo(t, fake, id, model.VisibilityPublic)
		indextest.InsertRepoRow(t, d, r)
		old := indextest.Commit(t, fake, id, map[string][]byte{"a.txt": []byte("a")})
		head := indextest.Commit(t, fake, id, map[string][]byte{"b.txt": []byte("b")})
		for _, sha := range []model.CommitSHA{old, head} {
			_, err := d.NewInsert().Model(&db.ScanResult{RepoID: r.UUID, Commit: string(sha), Path: "", ScannerVersion: scan.Version - 1,
				Status: "safe", Findings: db.JSON("[]")}).Exec(ctx)
			require.NoError(t, err)
		}
		s := scan.New(scan.Options{DB: d, Repos: fake})
		got, err := s.Results(ctx, r.UUID, head)
		require.NoError(t, err, "stale results stay readable")
		require.Equal(t, scan.StatusSafe, got.Status)
		q := jobstest.NewFake(jobs.NewRegistry())
		n, err := s.RescanStale(ctx, q)
		require.NoError(t, err)
		require.Equal(t, 1, n)
		list, _, err := q.List(ctx, jobs.Filter{})
		require.NoError(t, err)
		var p index.CommitJob
		require.NoError(t, list[0].DecodePayload(&p))
		require.Equal(t, head, p.Commit)
	})
}
