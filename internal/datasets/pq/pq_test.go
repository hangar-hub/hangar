package pq_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/datasets/features"
	"github.com/hangar-hub/hangar/internal/datasets/pq"
)

func pyISO(t time.Time, a *features.Arrow) string {
	s := t.Format("2006-01-02T15:04:05")
	if us := t.Nanosecond() / 1000; us != 0 {
		s += fmt.Sprintf(".%06d", us)
	}
	if a.TZ != "" {
		s += "+00:00"
	}
	return s
}

func enc(v any, a *features.Arrow) any {
	if v == nil {
		return nil
	}
	switch a.ID {
	case features.Binary, features.LargeBinary:
		return map[string]any{"bytes": base64.StdEncoding.EncodeToString(v.([]byte))}
	case features.Timestamp:
		return map[string]any{"ts": pyISO(v.(time.Time), a)}
	case features.Date32:
		return map[string]any{"date": v.(time.Time).Format("2006-01-02")}
	case features.Duration:
		return map[string]any{"dur": float64(v.(int64))}
	case features.Decimal128:
		return map[string]any{"dec": v.(string)}
	case features.Struct:
		vals := v.([]any)
		out := make([]any, len(vals))
		for i, x := range vals {
			out[i] = enc(x, a.Fields[i].Type)
		}
		return map[string]any{"struct": out}
	case features.List, features.LargeList, features.FixedSizeList:
		vals := v.([]any)
		out := make([]any, len(vals))
		for i, x := range vals {
			out[i] = enc(x, a.Elem)
		}
		return out
	}
	if i, ok := v.(int64); ok {
		return float64(i)
	}
	return v
}

func open(t *testing.T, name string) *pq.File {
	b, err := os.ReadFile("testdata/" + name)
	require.NoError(t, err)
	f, err := pq.Open(bytes.NewReader(b), int64(len(b)))
	require.NoError(t, err)
	return f
}

func TestReadPyarrowFile(t *testing.T) {
	raw, err := os.ReadFile("testdata/types.json")
	require.NoError(t, err)
	var want struct {
		Columns []string         `json:"columns"`
		Rows    map[string][]any `json:"rows"`
	}
	require.NoError(t, json.Unmarshal(raw, &want))
	f := open(t, "types.parquet")
	require.EqualValues(t, 2500, f.NumRows())
	require.Equal(t, 3, f.NumRowGroups())
	require.Equal(t, want.Columns, features.FromArrowSchema(f.Schema).Names())
	var types []string
	for _, fl := range f.Schema.Fields {
		types = append(types, fl.Type.String())
	}
	require.Equal(t, []string{"int64", "uint8", "float32", "string", "large_string", "bool", "binary", "timestamp[s]",
		"timestamp[us, tz=UTC]", "date32", "decimal128(12, 2)", "list<item: int64>", "list<item: list<item: int32>>",
		"struct<a: int64, b: list<item: string>, c: struct<x: float64>>", "list<item: struct<k: string, v: int64>>",
		"fixed_size_list<item: float64>[2]", "list<item: struct<key: string, value: int64>>", "null", "duration[ms]"}, types)
	require.False(t, f.HasFeatures, "the metadata lists one column, so it is ignored")
	cols, err := f.Columns(nil)
	require.NoError(t, err)
	for idx, row := range want.Rows {
		i, _ := strconv.ParseInt(idx, 10, 64)
		got, err := f.ReadRows(cols, i, 1)
		require.NoError(t, err)
		require.Len(t, got, 1)
		for j, v := range got[0] {
			g, _ := json.Marshal(enc(v, f.Schema.Fields[j].Type))
			w, _ := json.Marshal(row[j])
			require.JSONEq(t, string(w), string(g), "row %d column %s", i, want.Columns[j])
		}
	}
	// A projected scan across row groups.
	sub, err := f.Columns([]string{"id", "st"})
	require.NoError(t, err)
	rows, err := f.ReadRows(sub, 990, 20)
	require.NoError(t, err)
	require.Len(t, rows, 20)
	for k, r := range rows {
		require.EqualValues(t, 990+k, r[0])
	}
	all, err := f.ReadRows(sub, 0, -1)
	require.NoError(t, err)
	require.Len(t, all, 2500)
	_, err = f.Columns([]string{"nope"})
	require.Error(t, err)
	_, err = f.Scan(sub, 2501, 1)
	require.Error(t, err)
	require.Positive(t, f.UncompressedBytes())
	require.Positive(t, f.MaxRowGroupBytes())
	start, n := f.LeafRange(13)
	require.Equal(t, 3, n)
	require.Positive(t, start)
}

func TestReadLegacyTwoLevelLists(t *testing.T) {
	f := open(t, "legacy.parquet")
	rows, err := f.ReadRows([]int{0, 1, 2}, 0, -1)
	require.NoError(t, err)
	require.Len(t, rows, 2500)
	require.Nil(t, rows[0][1])
	require.Equal(t, []any{int64(0)}, rows[1][1])
	require.Equal(t, []any{int64(0), int64(1), int64(2)}, rows[3][1])
	require.Equal(t, int64(4), rows[4][2].([]any)[0])
}

func TestWriteAndReadBack(t *testing.T) {
	fs, err := features.ParseFeatures([]byte(`{
		"id":{"dtype":"int64","_type":"Value"},
		"label":{"names":["neg","pos"],"_type":"ClassLabel"},
		"text":{"dtype":"string","_type":"Value"},
		"score":{"dtype":"float32","_type":"Value"},
		"ok":{"dtype":"bool","_type":"Value"},
		"tags":{"feature":{"dtype":"string","_type":"Value"},"_type":"List"},
		"grid":{"feature":{"feature":{"dtype":"int16","_type":"Value"},"_type":"List"},"_type":"List"},
		"img":{"_type":"Image"},
		"meta":{"when":{"dtype":"timestamp[s]","_type":"Value"},"day":{"dtype":"date32","_type":"Value"},"price":{"dtype":"decimal128(10, 3)","_type":"Value"}},
		"items":{"feature":{"k":{"dtype":"string","_type":"Value"},"n":{"dtype":"uint32","_type":"Value"}},"_type":"List"},
		"nothing":{"dtype":"null","_type":"Value"},
		"big":{"dtype":"uint64","_type":"Value"},
		"dur":{"dtype":"duration[us]","_type":"Value"},
		"t":{"dtype":"time64[us]","_type":"Value"}}`))
	require.NoError(t, err)
	st, err := fs.StorageSchema()
	require.NoError(t, err)
	when := time.Date(2024, 5, 6, 7, 8, 9, 0, time.UTC)
	var rows [][]any
	for i := range 2300 {
		var tags any = []any{"a", fmt.Sprint(i)}
		switch i % 10 {
		case 0:
			tags = nil
		case 1:
			tags = []any{}
		}
		var grid any = []any{[]any{int64(i % 7), nil}, []any{}, nil}
		if i%4 == 0 {
			grid = nil
		}
		var meta any = []any{when.Add(time.Duration(i) * time.Second), features.DaysToTime(int64(i)), "-12.345"}
		if i%9 == 0 {
			meta = []any{nil, nil, nil}
		}
		if i%27 == 0 {
			meta = nil
		}
		rows = append(rows, []any{int64(i), int64(i % 2), fmt.Sprintf("row %d ünïcode", i), float64(float32(i) / 3), i%3 == 0,
			tags, grid, []any{[]byte{1, 2, byte(i)}, fmt.Sprintf("%d.jpg", i)}, meta,
			[]any{[]any{"k", int64(4000000000)}, []any{nil, nil}}, nil, math.MaxUint64 - uint64(i), int64(i * 1000), int64(3600_000_000)})
	}
	var buf bytes.Buffer
	w, err := pq.NewWriter(&buf, st, pq.WriterOptions{Features: fs, RowGroupRows: 500})
	require.NoError(t, err)
	require.NoError(t, w.Write(rows[:1000]))
	require.NoError(t, w.Flush())
	require.NoError(t, w.Write(rows[1000:]))
	require.NoError(t, w.Close())

	f, err := pq.Open(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	require.NoError(t, err)
	require.True(t, f.HasFeatures)
	require.Equal(t, fs.Names(), f.Features.Names())
	out, err := json.Marshal(f.Features)
	require.NoError(t, err)
	want, _ := json.Marshal(fs)
	require.JSONEq(t, string(want), string(out))
	require.EqualValues(t, 2300, f.NumRows())
	require.GreaterOrEqual(t, f.NumRowGroups(), 5)
	meta, ok := f.Metadata(pq.MetadataKey)
	require.True(t, ok)
	require.Contains(t, meta, `"info"`)
	cols, _ := f.Columns(nil)
	got, err := f.ReadRows(cols, 0, -1)
	require.NoError(t, err)
	require.Len(t, got, len(rows))
	for i := range rows {
		require.Equal(t, rows[i], got[i], "row %d", i)
	}
	mid, err := f.ReadRows(cols, 1234, 3)
	require.NoError(t, err)
	require.Equal(t, rows[1234:1237], mid)
}

func TestWriterErrors(t *testing.T) {
	_, err := pq.NewWriter(&bytes.Buffer{}, features.Primitive(features.Int64), pq.WriterOptions{})
	require.Error(t, err)
	st := features.StructOf(features.ArrowField{Name: "a", Type: features.Primitive(features.Int64)})
	w, err := pq.NewWriter(&bytes.Buffer{}, st, pq.WriterOptions{})
	require.NoError(t, err)
	require.Error(t, w.Write([][]any{{int64(1), int64(2)}}))
	require.Error(t, w.Write([][]any{{"not an int"}}))
	_, err = pq.NewWriter(&bytes.Buffer{}, features.StructOf(features.ArrowField{Name: "s", Type: features.StructOf()}), pq.WriterOptions{})
	require.ErrorIs(t, err, pq.ErrUnsupported)
}

func TestOpenErrors(t *testing.T) {
	for _, b := range [][]byte{nil, []byte("PAR1"), []byte("not parquet at all")} {
		_, err := pq.Open(bytes.NewReader(b), int64(len(b)))
		require.Error(t, err)
	}
}

// TestDictionaryIndexOutOfRange reads a copy of legacy.parquet whose first
// dictionary-encoded data page refers past the end of its dictionary. These
// two bytes are the input the fuzzer first found panicking in parquet-go's
// dictionary lookup.
func TestDictionaryIndexOutOfRange(t *testing.T) {
	b, err := os.ReadFile("testdata/legacy.parquet")
	require.NoError(t, err)
	require.Equal(t, []byte{0xd3, 0x4e}, b[3304:3306], "testdata/legacy.parquet changed; find a new offset")
	b[3304], b[3305] = 0xff, 0x80
	f, err := pq.Open(bytes.NewReader(b), int64(len(b)))
	require.NoError(t, err)
	cols, err := f.Columns(nil)
	require.NoError(t, err)
	_, err = f.ReadRows(cols, 0, 10)
	require.ErrorIs(t, err, pq.ErrInvalid)
	require.ErrorContains(t, err, "dictionary index")
}

// TestCorruptDeltaByteArray reads a copy of the encodings seed whose
// DELTA_BYTE_ARRAY page in column "s" has bytes 248 to 251 set to ff. They
// are the suffix lengths' header: it then claims 268 million lengths, and
// parquet-go allocated 1 GiB for them each time it decoded the page. An
// independent verifier found this input; it allocated 3.2 GB in one read.
func TestCorruptDeltaByteArray(t *testing.T) {
	b := seeds(t)["encodings.parquet"]
	require.Equal(t, []byte{0x02, 0x02, 0x02, 0x00}, b[248:252], "testdata/seeds/encodings.parquet changed; find a new offset")
	copy(b[248:252], []byte{0xff, 0xff, 0xff, 0xff})
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	f, err := pq.Open(bytes.NewReader(b), int64(len(b)))
	require.NoError(t, err)
	cols, err := f.Columns(nil)
	require.NoError(t, err)
	_, err = f.ReadRows(cols, 0, -1)
	runtime.ReadMemStats(&after)
	require.ErrorIs(t, err, pq.ErrInvalid)
	require.ErrorContains(t, err, "suffix lengths: delta header of 268435455 values")
	require.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(64<<20))
}

// TestCorruptDecimalScale reads a copy of the decimals seed with bytes 373
// and 374 set to 0x80. That lengthens the varint of column "d64"'s scale
// over its precision, giving a scale of 294,912 digits. Formatting each
// value to that scale, one zero at a time, allocated 124 GiB.
func TestCorruptDecimalScale(t *testing.T) {
	b := seeds(t)["decimals.parquet"]
	require.Equal(t, []byte{0x15, 0x04, 0x15}, b[372:375], "testdata/seeds/decimals.parquet changed; find a new offset")
	b[373], b[374] = 0x80, 0x80
	_, err := pq.Open(bytes.NewReader(b), int64(len(b)))
	require.ErrorIs(t, err, pq.ErrInvalid)
	require.ErrorContains(t, err, `column "d64" has decimal precision 0 and scale 294912`)
}

// TestCorruptBytesGiveErrors changes bytes of legacy.parquet and the fuzz
// seeds at fixed, pseudo-random places and reads every row, so the regular
// test run covers what FuzzOpenAndRead checks. No read may allocate more
// than 64 MiB: parquet-go allocates what corrupt sizes and counts claim
// unless pq checks them first.
func TestCorruptBytesGiveErrors(t *testing.T) {
	files := seeds(t)
	legacy, err := os.ReadFile("testdata/legacy.parquet")
	require.NoError(t, err)
	files["legacy.parquet"] = legacy
	names := slices.Sorted(maps.Keys(files))
	rng := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // reproducible test input, not a secret
	var before, after runtime.MemStats
	for _, name := range names {
		for i := range 800 {
			b := bytes.Clone(files[name])
			if i%2 == 0 {
				for range 1 + rng.IntN(3) {
					b[rng.IntN(len(b))] = byte(rng.IntN(256))
				}
			} else {
				// A run of bytes that lengthens or ends varints, as in
				// TestCorruptDeltaByteArray and TestCorruptDecimalScale.
				at, v := rng.IntN(len(b)), []byte{0x00, 0x7f, 0x80, 0xff}[rng.IntN(4)]
				for k := at; k < min(len(b), at+1+rng.IntN(5)); k++ {
					b[k] = v
				}
			}
			runtime.ReadMemStats(&before)
			require.NotPanics(t, func() { readAll(t, b, -1) }, "%s mutation %d", name, i)
			runtime.ReadMemStats(&after)
			require.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(64<<20), "%s mutation %d", name, i)
		}
	}
}

// seeds returns the small files compat/viewer/oracle/parquet_seeds.py
// writes with pyarrow.
func seeds(tb testing.TB) map[string][]byte {
	tb.Helper()
	names, err := filepath.Glob("testdata/seeds/*.parquet")
	require.NoError(tb, err)
	require.NotEmpty(tb, names)
	out := map[string][]byte{}
	for _, n := range names {
		b, err := os.ReadFile(n)
		require.NoError(tb, err)
		out[filepath.Base(n)] = b
	}
	return out
}

// TestSeedsRead checks that the fuzz seeds are valid files the reader
// reads in full, so the fuzzer starts from inputs that reach the pages, and
// that the checks before parquet-go reads a file accept every codec and
// encoding pyarrow writes.
func TestSeedsRead(t *testing.T) {
	all := map[string][][]any{}
	for name, b := range seeds(t) {
		f, err := pq.Open(bytes.NewReader(b), int64(len(b)))
		require.NoError(t, err, name)
		cols, err := f.Columns(nil)
		require.NoError(t, err)
		rows, err := f.ReadRows(cols, 0, -1)
		require.NoError(t, err, name)
		require.Len(t, rows, int(f.NumRows()), name)
		require.Positive(t, f.NumRows(), name)
		all[name] = rows
	}
	// The same table in each codec.
	want := all["list-dict.parquet"]
	require.Equal(t, []any{int64(3), []any{int64(0), int64(1), nil}}, want[3])
	for _, name := range []string{"codec-brotli.parquet", "codec-gzip.parquet", "codec-lz4.parquet", "codec-zstd.parquet", "list-v2-snappy.parquet"} {
		require.Equal(t, want, all[name], name)
	}
	enc := all["encodings.parquet"]
	require.Equal(t, []any{nil, int64(1 << 20), uint64(0), nil, nil, math.Inf(1)}, enc[1])
	require.Equal(t, []any{false, int64(-5), nil, "é"}, enc[2][:4])
	require.Empty(t, enc[2][4])
	require.True(t, math.Signbit(enc[2][5].(float64)))
}

// FuzzOpenAndRead checks that corrupt files give errors, not panics. The
// seeds are a few kilobytes each: Go minimises every input that finds new
// coverage, and minimising a file of tens of kilobytes takes so long that
// the fuzzer runs almost no inputs.
func FuzzOpenAndRead(f *testing.F) {
	for _, b := range seeds(f) {
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, b []byte) { readAll(t, b, 10) })
}

// readAll opens b and reads its statistics, the first n rows (every row
// when n < 0) and the last row. Open may fail, but only with ErrInvalid or
// ErrUnsupported; reads may fail; nothing may panic.
func readAll(t *testing.T, b []byte, n int64) {
	t.Helper()
	f, err := pq.Open(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		if !errors.Is(err, pq.ErrUnsupported) {
			require.ErrorIs(t, err, pq.ErrInvalid)
		}
		return
	}
	cols, err := f.Columns(nil)
	require.NoError(t, err)
	for g := range f.NumRowGroups() {
		for _, c := range cols {
			start, count := f.LeafRange(c)
			for leaf := start; leaf < start+count; leaf++ {
				f.ColumnStats(g, leaf)
			}
		}
	}
	_, _ = f.ReadRows(cols, 0, n)
	if f.NumRows() > 0 {
		_, _ = f.ReadRows(cols, f.NumRows()-1, 1)
	}
}
