package pq_test

import (
	"bytes"
	"fmt"
	"runtime"
	"strings"
	"testing"

	"github.com/parquet-go/parquet-go"
	"github.com/parquet-go/parquet-go/compress"
	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/datasets/pq"
)

// writeColumn writes a valid Parquet file of one string column "s", in
// one page per row group, with an encoding and codec.
func writeColumn(t testing.TB, enc parquet.Node, codec compress.Codec, rowGroups ...[]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	schema := parquet.NewSchema("r", parquet.Group{"s": enc})
	// Statistics would copy the large values into the footer.
	w := parquet.NewWriter(&buf, schema, parquet.Compression(codec), parquet.PageBufferSize(256<<20),
		parquet.DataPageStatistics(false), parquet.SkipPageBounds("s"))
	for _, values := range rowGroups {
		rows := make([]parquet.Row, len(values))
		for i, v := range values {
			rows[i] = parquet.Row{parquet.ByteArrayValue([]byte(v)).Level(0, 0, 0)}
		}
		_, err := w.WriteRows(rows)
		require.NoError(t, err)
		require.NoError(t, w.Flush())
	}
	require.NoError(t, w.Close())
	return buf.Bytes()
}

// bomb is a page of one value of size zero bytes: a few kilobytes that
// decompress to size bytes.
func bomb(t testing.TB, codec compress.Codec, size int) []byte {
	return writeColumn(t, parquet.String(), codec, []string{strings.Repeat("\x00", size)})
}

// prefixBomb is a DELTA_BYTE_ARRAY page whose values each repeat the
// 64 KiB value before them as a prefix: about 100 KB of page that builds
// n*64 KiB of values.
func prefixBomb(t testing.TB, n int) []byte {
	base := strings.Repeat("z", 64<<10)
	values := make([]string, n)
	for i := range values {
		values[i] = base + fmt.Sprintf("%05d", i)
	}
	return writeColumn(t, parquet.Encoded(parquet.String(), &parquet.DeltaByteArray), &parquet.Uncompressed, values)
}

func readWithin(t *testing.T, b []byte, budget *pq.Budget) (int64, error) {
	t.Helper()
	f, err := pq.Open(bytes.NewReader(b), int64(len(b)))
	require.NoError(t, err)
	cols, err := f.Columns(nil)
	require.NoError(t, err)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	_, err = f.ReadRowsWithin(budget, cols, 0, -1)
	runtime.ReadMemStats(&after)
	return int64(after.TotalAlloc - before.TotalAlloc), err
}

// TestBudgetStopsHostilePages reads valid files whose pages decode to far
// more than they hold, the zstd and Brotli bombs and DELTA_BYTE_ARRAY
// prefixes the earlier checks let through at up to 1 GiB a page, with a
// Budget of 4 MiB. Each read fails with ErrTooBig, the Hub's error, before
// it decodes the page, so it allocates about what the file holds.
func TestBudgetStopsHostilePages(t *testing.T) {
	const decoded, limit = 32 << 20, 4 << 20
	files := map[string][]byte{
		"zstd":             bomb(t, &parquet.Zstd, decoded),
		"brotli":           bomb(t, &parquet.Brotli, decoded),
		"gzip":             bomb(t, &parquet.Gzip, decoded),
		"delta byte array": prefixBomb(t, decoded/(64<<10)),
	}
	for name, b := range files {
		t.Run(name, func(t *testing.T) {
			require.Less(t, len(b), 1<<20, "the file is small")
			alloc, err := readWithin(t, b, pq.NewBudget(limit))
			require.ErrorIs(t, err, pq.ErrTooBig)
			var tooBig *pq.TooBigError
			require.ErrorAs(t, err, &tooBig)
			require.Greater(t, tooBig.Read, int64(limit))
			require.Equal(t, int64(limit), tooBig.Limit)
			require.Regexp(t, `^Scan size limit exceeded: attempted to read \d+ bytes, limit is 4194304 bytes$`, tooBig.Error())
			require.Less(t, alloc, int64(limit), "the read allocated %d bytes", alloc)

			// The same file reads in full without a Budget, so the
			// Budget is what stopped it.
			alloc, err = readWithin(t, b, nil)
			require.NoError(t, err)
			require.Greater(t, alloc, int64(decoded))
		})
	}
}

// TestBudgetCountsWhatRequestsHold checks the accounting: ReadRowsWithin
// keeps its charge, so reads in one request add up; a Scanner gives its
// charge back as it moves on and when it closes; and a chunk checked under
// a large Budget is still refused under a small one.
func TestBudgetCountsWhatRequestsHold(t *testing.T) {
	group := func(c byte) []string {
		out := make([]string, 100)
		for i := range out {
			out[i] = strings.Repeat(string(c), 10<<10) + fmt.Sprint(i)
		}
		return out
	}
	b := writeColumn(t, parquet.String(), &parquet.Snappy, group('a'), group('b'), group('c'))
	f, err := pq.Open(bytes.NewReader(b), int64(len(b)))
	require.NoError(t, err)
	require.Equal(t, 3, f.NumRowGroups())
	cols, err := f.Columns(nil)
	require.NoError(t, err)

	// One row group decodes to about 1 MB.
	budget := pq.NewBudget(pq.DefaultBudgetBytes)
	rows, err := f.ReadRowsWithin(budget, cols, 0, 100)
	require.NoError(t, err)
	require.Len(t, rows, 100)
	one := budget.Used()
	require.Greater(t, one, int64(100*10<<10))
	require.Less(t, one, int64(2<<20))

	// Reads in one request add up.
	small := pq.NewBudget(one * 2)
	_, err = f.ReadRowsWithin(small, cols, 0, 100)
	require.NoError(t, err)
	_, err = f.ReadRowsWithin(small, cols, 100, 100)
	require.NoError(t, err)
	_, err = f.ReadRowsWithin(small, cols, 200, 100)
	require.ErrorIs(t, err, pq.ErrTooBig)
	require.Equal(t, 2*one, small.Used(), "a refused read takes nothing")

	// A Scanner over every row group holds one at a time.
	small = pq.NewBudget(one + one/2)
	s, err := f.ScanWithin(small, cols, 0, -1)
	require.NoError(t, err)
	n := 0
	for {
		_, _, err := s.Next()
		if err != nil {
			break
		}
		n++
		require.LessOrEqual(t, small.Used(), one+one/2)
	}
	require.Equal(t, 300, n)
	s.Close()
	require.Zero(t, small.Used())

	// The chunks were checked under a large Budget above; a small one
	// still refuses them.
	_, err = f.ReadRowsWithin(pq.NewBudget(one/2), cols, 0, 1)
	require.ErrorIs(t, err, pq.ErrTooBig)
}
