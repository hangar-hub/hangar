//go:build load

package viewer_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/blob"
	"github.com/hangar-hub/hangar/internal/datasets/features"
	"github.com/hangar-hub/hangar/internal/datasets/pq"
	"github.com/hangar-hub/hangar/internal/datasets/viewertest"
	"github.com/hangar-hub/hangar/internal/model"
	"github.com/hangar-hub/hangar/internal/repo"
	"github.com/hangar-hub/hangar/internal/repo/repotest"
)

// The load targets of docs/spec/14-datasets.md "Tests": /rows p99 under
// 200 ms on a 10 million row Split, and /filter over a 1 million row
// Split within viewer.max_scan_seconds. The Viewer gets a Repo service
// with the real one's caches (see cachedRepos). Run them with
// `go test -tags load -run TestLoad -timeout 30m ./internal/datasets/viewer`.

// parquetRows writes a Parquet file of n rows with an integer, a string
// and a float column, in row groups of 1,000 rows as the export writes.
func parquetRows(t *testing.T, n int) []byte {
	t.Helper()
	fs, err := features.ParseFeatures([]byte(`{"n":{"dtype":"int64","_type":"Value"},"s":{"dtype":"string","_type":"Value"},"f":{"dtype":"float64","_type":"Value"}}`))
	require.NoError(t, err)
	st, err := fs.StorageSchema()
	require.NoError(t, err)
	var buf bytes.Buffer
	w, err := pq.NewWriter(&buf, st, pq.WriterOptions{Features: fs})
	require.NoError(t, err)
	batch := make([][]any, 0, 10_000)
	for i := range n {
		batch = append(batch, []any{int64(i), fmt.Sprintf("row %d of the load test", i), float64(i) / 7})
		if len(batch) == cap(batch) {
			require.NoError(t, w.Write(batch))
			batch = batch[:0]
		}
	}
	if len(batch) > 0 {
		require.NoError(t, w.Write(batch))
	}
	require.NoError(t, w.Close())
	return buf.Bytes()
}

// cachedRepos stands in for W2's Repo service, which resolves branches in
// process and keeps tree entries in a cache. The fake runs git for every
// Resolve and for every file read (to find the tree entry), and on a loaded
// machine starting git costs more than the rest of a request, so without
// these caches the test measures process start-up rather than the Viewer.
// Branch heads do not move while the test measures, and entries are
// addressed by Commit, so the caches stay correct.
type cachedRepos struct {
	*repotest.Fake
	mu      sync.Mutex
	heads   map[string]model.CommitSHA
	entries map[string]model.Entry
}

func newCachedRepos(f *repotest.Fake) repo.Service {
	return &cachedRepos{Fake: f, heads: map[string]model.CommitSHA{}, entries: map[string]model.Entry{}}
}

func (c *cachedRepos) Resolve(ctx context.Context, id model.RepoID, revision string) (model.CommitSHA, error) {
	key := id.String() + "@" + revision
	c.mu.Lock()
	defer c.mu.Unlock()
	if sha, ok := c.heads[key]; ok {
		return sha, nil
	}
	sha, err := c.Fake.Resolve(ctx, id, revision)
	if err != nil {
		return "", err
	}
	c.heads[key] = sha
	return sha, nil
}

// OpenFile reads LFS files from the LFS store through the entry cache, as
// the real service reads them from the Blob store.
func (c *cachedRepos) OpenFile(ctx context.Context, id model.RepoID, commit model.CommitSHA, path string, rng *blob.Range) (io.ReadCloser, model.Entry, error) {
	key := id.String() + "@" + string(commit) + "/" + path
	c.mu.Lock()
	e, ok := c.entries[key]
	c.mu.Unlock()
	if !ok {
		var err error
		if e, err = c.Fake.Entry(ctx, id, commit, path); err != nil {
			return nil, model.Entry{}, err
		}
		c.mu.Lock()
		c.entries[key] = e
		c.mu.Unlock()
	}
	if e.Type != model.EntryFile || e.LFS == nil || !e.Present {
		return c.Fake.OpenFile(ctx, id, commit, path, rng)
	}
	rc, _, err := c.Fake.LFS.Open(ctx, e.LFS.OID, rng)
	return rc, e, err
}

// loadOptions gives the Viewer the caches.
func loadOptions() viewertest.Options {
	return viewertest.Options{ViewerRepos: newCachedRepos}
}

func timedGet(t *testing.T, url string) time.Duration {
	t.Helper()
	start := time.Now()
	res, err := http.Get(url)
	require.NoError(t, err)
	_, err = bytes.NewBuffer(nil).ReadFrom(res.Body)
	require.NoError(t, err)
	require.NoError(t, res.Body.Close())
	require.Equal(t, http.StatusOK, res.StatusCode, url)
	return time.Since(start)
}

func percentile(d []time.Duration, p float64) time.Duration {
	s := slices.Clone(d)
	slices.Sort(s)
	return s[int(float64(len(s)-1)*p)]
}

func TestLoadRows(t *testing.T) {
	const rows = 10_000_000
	start := time.Now()
	data := parquetRows(t, rows)
	t.Logf("wrote %d rows, %d MB, in %s", rows, len(data)>>20, time.Since(start).Round(time.Millisecond))
	s := viewertest.New(t, loadOptions())
	r, sha := s.Dataset(t, "big", map[string][]byte{"data/train-00000-of-00001.parquet": data})
	start = time.Now()
	s.Convert(t, r, sha)
	t.Logf("converted and computed statistics in %s", time.Since(start).Round(time.Millisecond))

	base := s.URL + "/rows?dataset=" + r.ID.String() + "&config=default&split=train&length=100&offset="
	timedGet(t, base+"0") // the first request reads the footer
	rng := rand.New(rand.NewPCG(1, 2))
	var d []time.Duration
	for range 500 {
		d = append(d, timedGet(t, base+fmt.Sprint(rng.Int64N(rows-100))))
	}
	p50, p99 := percentile(d, 0.5), percentile(d, 0.99)
	t.Logf("/rows over %d rows: p50 %s, p99 %s, max %s", rows, p50, p99, percentile(d, 1))
	require.Less(t, p99, 200*time.Millisecond)
}

func TestLoadFilter(t *testing.T) {
	const rows = 1_000_000
	data := parquetRows(t, rows)
	s := viewertest.New(t, loadOptions())
	r, sha := s.Dataset(t, "filter", map[string][]byte{"data/train-00000-of-00001.parquet": data})
	s.Convert(t, r, sha)

	for _, c := range []struct{ where, orderby string }{
		{`"n" >= 999900`, ""},
		{`"s" LIKE '%of the load%' AND "f" > 100000`, `"n" DESC`},
		{`"s" ILIKE '%ROW 99999 %' OR "n" IN (5, 7)`, `"f" ASC`},
	} {
		params := q("dataset", r.ID.String(), "config", "default", "split", "train", "length", "100", "where", c.where)
		if c.orderby != "" {
			params.Set("orderby", c.orderby)
		}
		start := time.Now()
		res := get(t, s, "/filter", params, "")
		d := time.Since(start)
		require.Equal(t, http.StatusOK, res.status, string(res.raw))
		require.Equal(t, false, res.body["partial"], c.where)
		t.Logf("/filter where %s orderby %q over %d rows: %s, %v matches", c.where, c.orderby, rows, d.Round(time.Millisecond), res.body["num_rows_total"])
		require.Less(t, d, 10*time.Second, "viewer.max_scan_seconds")
		// The cached row index makes the next page fast.
		params.Set("offset", "100")
		start = time.Now()
		res = get(t, s, "/filter", params, "")
		require.Equal(t, http.StatusOK, res.status)
		t.Logf("  next page from the cached row index: %s", time.Since(start).Round(time.Millisecond))
	}
}
