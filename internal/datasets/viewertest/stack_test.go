package viewertest_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/parquet-go/parquet-go"
	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/datasets/viewer"
	"github.com/hangar-hub/hangar/internal/datasets/viewertest"
	"github.com/hangar-hub/hangar/internal/model"
	"github.com/hangar-hub/hangar/internal/repo"
	"github.com/hangar-hub/hangar/internal/repo/repotest"
)

func TestStackServesAConvertedDataset(t *testing.T) {
	s := viewertest.New(t, viewertest.Options{MaxConvertBytes: 1 << 20})
	r, sha := s.Dataset(t, "demo", map[string][]byte{"train.csv": []byte("a,b\n1,x\n2,y\n")})
	require.Equal(t, "ns/demo", r.ID.String())
	s.Convert(t, r, sha)

	res, err := http.Get(s.URL + "/rows?dataset=ns/demo&config=default&split=train")
	require.NoError(t, err)
	defer res.Body.Close()
	require.Equal(t, http.StatusOK, res.StatusCode)
	require.Equal(t, string(sha), res.Header.Get("X-Revision"))
	var body struct {
		NumRowsTotal int `json:"num_rows_total"`
	}
	require.NoError(t, json.NewDecoder(res.Body).Decode(&body))
	require.Equal(t, 2, body.NumRowsTotal)
	s.RunJobs(t)
}

// countingRepos counts the Resolve calls the Viewer makes.
type countingRepos struct {
	repo.Service
	resolves atomic.Int64
}

func (c *countingRepos) Resolve(ctx context.Context, id model.RepoID, revision string) (model.CommitSHA, error) {
	c.resolves.Add(1)
	return c.Service.Resolve(ctx, id, revision)
}

func TestViewerReposWrapsOnlyTheViewer(t *testing.T) {
	counter := &countingRepos{}
	s := viewertest.New(t, viewertest.Options{ViewerRepos: func(r *repotest.Fake) repo.Service {
		counter.Service = r
		return counter
	}})
	r, sha := s.Dataset(t, "wrapped", map[string][]byte{"train.csv": []byte("a\n1\n")})
	s.Convert(t, r, sha)
	require.Zero(t, counter.resolves.Load(), "the pipeline uses the fake directly")

	res, err := http.Get(s.URL + "/is-valid?dataset=ns/wrapped")
	require.NoError(t, err)
	require.NoError(t, res.Body.Close())
	require.Equal(t, http.StatusOK, res.StatusCode)
	require.Positive(t, counter.resolves.Load())
}

// TestCorruptParquetUpload uploads a 978-byte Parquet file whose
// DELTA_BYTE_ARRAY page claims 268 million suffix lengths (see
// pq.TestCorruptDeltaByteArray). Converting it and computing its statistics
// allocated about 2 GiB, and each /first-rows request 1 GiB, before pq
// checked those lengths.
func TestCorruptParquetUpload(t *testing.T) {
	b, err := os.ReadFile("../pq/testdata/seeds/encodings.parquet")
	require.NoError(t, err)
	require.Equal(t, []byte{0x02, 0x02, 0x02, 0x00}, b[248:252], "the encodings seed changed; find a new offset")
	copy(b[248:252], []byte{0xff, 0xff, 0xff, 0xff})
	s := viewertest.New(t, viewertest.Options{})
	r, sha := s.Dataset(t, "corrupt", map[string][]byte{"train.parquet": b})

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	s.Convert(t, r, sha)
	for range 3 {
		res, err := http.Get(s.URL + "/first-rows?dataset=ns/corrupt&config=default&split=train")
		require.NoError(t, err)
		_, err = io.Copy(io.Discard, res.Body)
		require.NoError(t, err)
		require.NoError(t, res.Body.Close())
		require.NotEqual(t, http.StatusOK, res.StatusCode)
	}
	runtime.ReadMemStats(&after)
	require.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(256<<20))
}

// prefixBombParquet writes a valid Parquet file of one string column "s"
// in one DELTA_BYTE_ARRAY page: n values that each repeat the 64 KiB value
// before them as a prefix, so about 100 KB of page builds n*64 KiB of
// values when read.
func prefixBombParquet(t *testing.T, n int) []byte {
	base := strings.Repeat("z", 64<<10)
	rows := make([]parquet.Row, n)
	for i := range rows {
		rows[i] = parquet.Row{parquet.ByteArrayValue([]byte(base+fmt.Sprintf("%05d", i))).Level(0, 0, 0)}
	}
	var buf bytes.Buffer
	schema := parquet.NewSchema("r", parquet.Group{"s": parquet.Encoded(parquet.String(), &parquet.DeltaByteArray)})
	w := parquet.NewWriter(&buf, schema, parquet.PageBufferSize(256<<20), parquet.DataPageStatistics(false), parquet.SkipPageBounds("s"))
	_, err := w.WriteRows(rows)
	require.NoError(t, err)
	require.NoError(t, w.Close())
	return buf.Bytes()
}

// TestRowsTooBigForTheBudget uploads a valid Parquet file whose page of
// about 100 KB builds 32 MiB of values, and serves it with a budget of
// 8 MiB per request. Without a budget each read decoded the whole page;
// with it /first-rows (from the source while the conversion is pending, then
// from the export), /rows and /search answer with the Hub's
// TooBigContentError before parquet-go decodes the page.
func TestRowsTooBigForTheBudget(t *testing.T) {
	const budget = 8 << 20
	b := prefixBombParquet(t, 512)
	require.Less(t, len(b), 1<<20)
	s := viewertest.New(t, viewertest.Options{Viewer: viewer.Options{MaxRequestBytes: budget}})
	r, sha := s.Dataset(t, "bomb", map[string][]byte{"train.parquet": b})

	// While the conversion is pending, first rows come from the source.
	res, err := http.Get(s.URL + "/first-rows?dataset=ns/bomb&config=default&split=train")
	require.NoError(t, err)
	require.NoError(t, res.Body.Close())
	require.Equal(t, http.StatusNotImplemented, res.StatusCode)
	require.Equal(t, "TooBigContentError", res.Header.Get("X-Error-Code"))
	s.Convert(t, r, sha)

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	for _, c := range []struct {
		path   string
		status int
	}{
		{"/first-rows?dataset=ns/bomb&config=default&split=train", http.StatusNotImplemented},
		{"/rows?dataset=ns/bomb&config=default&split=train&offset=10&length=5", http.StatusInternalServerError},
		{"/search?dataset=ns/bomb&config=default&split=train&query=zz", http.StatusInternalServerError},
	} {
		res, err := http.Get(s.URL + c.path)
		require.NoError(t, err)
		var body map[string]string
		require.NoError(t, json.NewDecoder(res.Body).Decode(&body))
		require.NoError(t, res.Body.Close())
		require.Equal(t, c.status, res.StatusCode, c.path)
		require.Equal(t, "TooBigContentError", res.Header.Get("X-Error-Code"), c.path)
		require.Regexp(t, `^Scan size limit exceeded: attempted to read \d+ bytes, limit is 8388608 bytes\n\nMake sure that\n\n`, body["error"], c.path)
	}
	runtime.ReadMemStats(&after)
	require.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(3*budget), "the three requests allocated %d bytes", after.TotalAlloc-before.TotalAlloc)
}
