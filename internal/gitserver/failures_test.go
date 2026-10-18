package gitserver_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/hangar-hub/hangar/internal/gitserver/gitservertest"
	"github.com/hangar-hub/hangar/internal/model"
	"github.com/hangar-hub/hangar/internal/repo"
	"github.com/hangar-hub/hangar/internal/repo/repotest"
	"github.com/hangar-hub/hangar/internal/telemetry"
)

// faulty wraps the fake Repo service and fails chosen calls.
type faulty struct {
	*repotest.Fake
	mu        sync.Mutex
	openErr   error
	applyErr  error
	lfsRefErr error
	getErr    error
	badView   bool
}

func (f *faulty) set(fn func(*faulty)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *faulty) Get(ctx context.Context, id model.RepoID) (*repo.Repo, error) {
	f.mu.Lock()
	err := f.getErr
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return f.Fake.Get(ctx, id)
}

func (f *faulty) OpenView(ctx context.Context, id model.RepoID) (*repo.GitView, error) {
	f.mu.Lock()
	err, bad := f.openErr, f.badView
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if bad {
		return repo.NewGitView("/nonexistent/hangar-view", func() error { return errors.New("close failed") }), nil
	}
	return f.Fake.OpenView(ctx, id)
}

func (f *faulty) ApplyPack(ctx context.Context, p repo.ApplyPackParams) error {
	f.mu.Lock()
	err := f.applyErr
	f.mu.Unlock()
	if err != nil {
		if p.Pack != nil {
			_, _ = io.Copy(io.Discard, p.Pack)
		}
		return err
	}
	return f.Fake.ApplyPack(ctx, p)
}

func (f *faulty) LFSRefs(ctx context.Context, id model.RepoID, oids []string) (map[string]repo.LFSRef, error) {
	f.mu.Lock()
	err := f.lfsRefErr
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return f.Fake.LFSRefs(ctx, id, oids)
}

func newFaultyFixture(t *testing.T, reader *sdkmetric.ManualReader) (*fixture, *faulty) {
	t.Helper()
	var fr *faulty
	f := newFixture(t, func(o *gitservertest.Options, f *fixture) {
		fr = &faulty{Fake: f.repos}
		o.Repos = fr
		if reader != nil {
			inst, err := telemetry.NewInstruments(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("test"))
			require.NoError(t, err)
			o.Instruments = inst
		}
	})
	return f, fr
}

func pushCounts(t *testing.T, reader *sdkmetric.ManualReader) map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	out := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "hangar.git.push" {
				continue
			}
			for _, dp := range m.Data.(metricdata.Sum[int64]).DataPoints {
				v, _ := dp.Attributes.Value("outcome")
				out[v.AsString()] += dp.Value
			}
		}
	}
	return out
}

func TestPushOutcomesAndFailures(t *testing.T) {
	t.Parallel()
	reader := sdkmetric.NewManualReader()
	f, fr := newFaultyFixture(t, reader)
	ctx := context.Background()
	dir := f.clone(t, publicID, tokAlice)

	f.commit(t, dir, "a.txt", "a", "a")
	f.git.Must(t, dir, "push", "-q", "origin", "main")

	f.commit(t, dir, "b.txt", "b", "b")
	fr.set(func(f *faulty) { f.applyErr = repo.ErrRefConflict })
	out, err := f.git.Run(ctx, dir, "push", "origin", "main")
	require.Error(t, err)
	require.Contains(t, out, "reference has changed concurrently, fetch first")

	fr.set(func(f *faulty) { f.applyErr = errors.New("database is down") })
	out, err = f.git.Run(ctx, dir, "push", "origin", "main")
	require.Error(t, err)
	require.Contains(t, out, "internal server error, the request ID is")
	require.NotContains(t, out, "database is down")

	fr.set(func(f *faulty) { f.applyErr = nil })
	_, err = f.git.Run(ctx, dir, "push", "origin", "HEAD:refs/pr/9")
	require.Error(t, err)

	counts := pushCounts(t, reader)
	require.Equal(t, int64(1), counts["ok"])
	require.Equal(t, int64(1), counts["conflict"])
	require.Equal(t, int64(1), counts["error"])
	require.Equal(t, int64(1), counts["rejected"])

	// A view that cannot be opened is a 500 for every service.
	fr.set(func(f *faulty) { f.openErr = errors.New("cache full") })
	for _, path := range []string{"/alice/tiny.git/info/refs?service=git-upload-pack", "/alice/tiny.git/info/refs?service=git-receive-pack"} {
		resp := f.get(t, path, tokAlice)
		require.Equal(t, http.StatusInternalServerError, resp.StatusCode, path)
		require.Contains(t, readAll(t, resp.Body), "request ID")
	}
	for _, svc := range []string{"git-upload-pack", "git-receive-pack"} {
		resp := f.do(t, http.MethodPost, "/alice/tiny.git/"+svc, tokAlice, strings.NewReader("0000"),
			http.Header{"Content-Type": {"application/x-" + svc + "-request"}})
		require.Equal(t, http.StatusInternalServerError, resp.StatusCode, svc)
	}

	// A view that is not a repository fails upload-pack before any output.
	fr.set(func(f *faulty) { f.openErr = nil; f.badView = true })
	resp := f.get(t, "/alice/tiny.git/info/refs?service=git-upload-pack", tokAlice)
	require.Equal(t, http.StatusInternalServerError, resp.StatusCode)
	resp = f.do(t, http.MethodPost, "/alice/tiny.git/git-upload-pack", tokAlice, strings.NewReader("0000"),
		http.Header{"Content-Type": {"application/x-git-upload-pack-request"}})
	require.Equal(t, http.StatusInternalServerError, resp.StatusCode)
	resp = f.get(t, "/alice/tiny.git/info/refs?service=git-receive-pack", tokAlice)
	require.Equal(t, http.StatusInternalServerError, resp.StatusCode)
	resp = f.do(t, http.MethodPost, "/alice/tiny.git/git-receive-pack", tokAlice, strings.NewReader("0000"),
		http.Header{"Content-Type": {"application/x-git-receive-pack-request"}})
	require.Equal(t, http.StatusInternalServerError, resp.StatusCode)

	// Lookup and LFS service failures are 500s in each protocol's format.
	fr.set(func(f *faulty) { f.badView = false; f.lfsRefErr = errors.New("db down") })
	r, b := f.batch(t, publicID, "", batchReq("download", nil, obj(strings.Repeat("a", 64), 1)))
	require.Equal(t, http.StatusInternalServerError, r.StatusCode)
	require.Equal(t, "Internal server error.", b.Message)
	fr.set(func(f *faulty) { f.getErr = errors.New("db down") })
	resp = f.get(t, "/alice/tiny.git/info/refs?service=git-upload-pack", "")
	require.Equal(t, http.StatusInternalServerError, resp.StatusCode)
	r, _ = f.batch(t, publicID, "", batchReq("download", nil))
	require.Equal(t, http.StatusInternalServerError, r.StatusCode)
}
