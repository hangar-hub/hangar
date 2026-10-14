//go:build load

package viewer_test

import (
	"bytes"
	"fmt"
	"math/rand/v2"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/datasets/features"
	"github.com/jamesagarside/hangar/internal/datasets/pq"
	"github.com/jamesagarside/hangar/internal/datasets/viewertest"
)

// The load targets of docs/spec/14-datasets.md "Tests": /rows p99 under
// 200 ms on a 10 million row Split, and /filter over a 1 million row
// Split within viewer.max_scan_seconds. Run them with
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
	s := viewertest.New(t, viewertest.Options{})
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
	s := viewertest.New(t, viewertest.Options{})
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
