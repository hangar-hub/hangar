package convert_test

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/datasets"
	"github.com/hangar-hub/hangar/internal/datasets/convert"
	"github.com/hangar-hub/hangar/internal/datasets/features"
	"github.com/hangar-hub/hangar/internal/datasets/layout"
)

func memFile(p string, b []byte) convert.File {
	return convert.File{Path: p, Size: int64(len(b)),
		Open:   func(context.Context) (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil },
		OpenAt: func(context.Context) (io.ReaderAt, error) { return bytes.NewReader(b), nil }}
}

func TestNames(t *testing.T) {
	for in, want := range map[string]string{
		"MATH-500": "math-500", "HelloWorld": "hello_world", "ABCDef": "abc_def", "doc-audio-6": "doc-audio-6", "rotten_tomatoes": "rotten_tomatoes",
	} {
		require.Equal(t, want, convert.SnakeCase(in), in)
	}
	require.Equal(t, "hf://datasets/ns/x@abc/data/a.csv", convert.DownloadChecksumKey("hf://datasets/ns/x@abc", "./data//a.csv"))
	require.Equal(t, "default/train/0003.parquet", convert.ExportPath("default", "train", 3, 4, false))
	require.Equal(t, "c/partial-test/0000.parquet", convert.ExportPath("c", "test", 0, 1, true))
	require.Equal(t, "c/train-part1/0005.parquet", convert.ExportPath("c", "train", convert.MaxFilesPerDirectory+5, convert.MaxFilesPerDirectory+6, false))
}

func csvConfig(files ...string) datasets.Config {
	return datasets.Config{Name: "default", Builder: "csv", Files: map[string][]string{"train": files}, SplitOrder: []string{"train"}}
}

func TestConvertShardsAndLimits(t *testing.T) {
	var b strings.Builder
	b.WriteString("n,s\n")
	for i := range 25000 {
		fmt.Fprintf(&b, "%d,row number %d\n", i, i)
	}
	files := map[string]convert.File{"a.csv": memFile("a.csv", []byte(b.String()))}
	ctx := context.Background()

	// Small shards split the Split into several files.
	res, err := convert.Convert(ctx, csvConfig("a.csv"), files, convert.Options{TempDir: t.TempDir(), ShardBytes: 100_000})
	require.NoError(t, err)
	defer res.Cleanup()
	sr := res.Splits[0]
	require.EqualValues(t, 25000, sr.NumRows)
	// Shards close after a pandas chunk of 10,000 rows, as `datasets`
	// checks the shard size after each table it writes.
	require.Len(t, sr.Shards, 3)
	var rows int64
	for i, sh := range sr.Shards {
		require.Equal(t, fmt.Sprintf("default/train/%04d.parquet", i), sh.ExportPath)
		require.Len(t, sh.SHA256, 64)
		rows += sh.NumRows
	}
	require.EqualValues(t, 25000, rows)
	require.False(t, res.Partial)

	// A byte limit stops the conversion and marks it partial.
	res, err = convert.Convert(ctx, csvConfig("a.csv"), files, convert.Options{TempDir: t.TempDir(), MaxSplitBytes: 10_000})
	require.NoError(t, err)
	defer res.Cleanup()
	require.True(t, res.Partial)
	require.EqualValues(t, 10000, res.Splits[0].NumRows)
	require.Equal(t, "default/partial-train/0000.parquet", res.Splits[0].Shards[0].ExportPath)

	// A cancelled context stops it with an error.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	_, err = convert.Convert(cctx, csvConfig("a.csv"), files, convert.Options{TempDir: t.TempDir()})
	require.Error(t, err)

	_, err = convert.Convert(ctx, csvConfig("missing.csv"), files, convert.Options{TempDir: t.TempDir()})
	require.ErrorContains(t, err, "missing")
	bad := csvConfig("a.csv")
	bad.Builder = "pandas"
	_, err = convert.Convert(ctx, bad, files, convert.Options{TempDir: t.TempDir()})
	require.Error(t, err)

	// An empty Split still gets a Parquet file with the schema.
	empty := map[string]convert.File{"e.csv": memFile("e.csv", []byte("n,s\n"))}
	res, err = convert.Convert(ctx, csvConfig("e.csv"), empty, convert.Options{TempDir: t.TempDir()})
	require.NoError(t, err)
	defer res.Cleanup()
	require.Len(t, res.Splits[0].Shards, 1)
	require.Zero(t, res.Splits[0].NumRows)
}

func TestCopyParquetNeedsLFS(t *testing.T) {
	raw := mustRead(t, "testdata/cases/parquet_plain/files/a.parquet")
	cfg := datasets.Config{Name: "default", Builder: "parquet", Files: map[string][]string{"train": {"a.parquet"}}, SplitOrder: []string{"train"}}
	_, err := convert.Convert(context.Background(), cfg, map[string]convert.File{"a.parquet": memFile("a.parquet", raw)}, convert.Options{TempDir: t.TempDir()})
	require.ErrorIs(t, err, convert.ErrUnsupported)
	cfg.Files["train"] = nil
	_, err = convert.Convert(context.Background(), cfg, map[string]convert.File{}, convert.Options{TempDir: t.TempDir()})
	require.Error(t, err)
}

func TestHead(t *testing.T) {
	ctx := context.Background()
	files := map[string]convert.File{
		"a.csv":     memFile("a.csv", []byte("x,y\n1,a\n2,b\n3,c\n")),
		"b.jsonl":   memFile("b.jsonl", []byte(`{"q": 1}`+"\n"+`{"q": 2}`+"\n")),
		"c.txt":     memFile("c.txt", []byte("one\ntwo\nthree\n")),
		"d.tar":     memFile("d.tar", nil),
		"p.parquet": memFile("p.parquet", mustRead(t, "testdata/cases/parquet_plain/files/a.parquet")),
	}
	fs, rows, err := convert.Head(ctx, csvConfig("a.csv"), "train", files, 2)
	require.NoError(t, err)
	require.Equal(t, []string{"x", "y"}, fs.Names())
	require.Equal(t, [][]any{{int64(1), "a"}, {int64(2), "b"}}, rows)

	cfg := datasets.Config{Name: "default", Builder: "json", Files: map[string][]string{"train": {"b.jsonl"}}, SplitOrder: []string{"train"}}
	_, rows, err = convert.Head(ctx, cfg, "train", files, 10)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	cfg = datasets.Config{Name: "default", Builder: "text", Files: map[string][]string{"train": {"c.txt"}}, SplitOrder: []string{"train"}}
	_, rows, err = convert.Head(ctx, cfg, "train", files, 1)
	require.NoError(t, err)
	require.Equal(t, [][]any{{"one"}}, rows)
	cfg = datasets.Config{Name: "default", Builder: "parquet", Files: map[string][]string{"train": {"p.parquet"}}, SplitOrder: []string{"train"}}
	fs, rows, err = convert.Head(ctx, cfg, "train", files, 10)
	require.NoError(t, err)
	require.Equal(t, []string{"x", "d", "m"}, fs.Names())
	require.Len(t, rows, 2)

	_, _, err = convert.Head(ctx, datasets.Config{Builder: "webdataset", Files: map[string][]string{"train": {"d.tar"}}}, "train", files, 1)
	require.ErrorIs(t, err, convert.ErrUnsupported)
	_, _, err = convert.Head(ctx, csvConfig("a.csv"), "test", files, 1)
	require.ErrorContains(t, err, "no split")
	_, _, err = convert.Head(ctx, csvConfig("gone.csv"), "train", files, 1)
	require.ErrorContains(t, err, "missing")
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := readFile(p)
	require.NoError(t, err)
	return b
}

func arrow(t *testing.T, dtype string) *features.Arrow {
	t.Helper()
	a, err := features.ParseDType(dtype)
	require.NoError(t, err)
	return a
}

func TestCast(t *testing.T) {
	when := time.Date(2020, 1, 2, 3, 4, 5, 123456000, time.UTC)
	ok := []struct {
		v     any
		dtype string
		want  any
	}{
		{true, "bool", true}, {int64(0), "bool", false}, {"TRUE", "bool", true}, {"0", "bool", false},
		{int64(5), "int8", int64(5)}, {uint64(7), "int64", int64(7)}, {uint64(1 << 63), "uint64", uint64(1 << 63)}, {true, "int32", int64(1)},
		{3.0, "int16", int64(3)}, {" 12 ", "int64", int64(12)}, {"18446744073709551615", "uint64", uint64(math.MaxUint64)}, {int64(3), "uint64", uint64(3)},
		{2.5, "float64", 2.5}, {int64(2), "float32", 2.0}, {uint64(2), "float64", 2.0}, {false, "float64", 0.0}, {"1_000.5", "float64", 1000.5},
		{0.1, "float32", float64(float32(0.1))}, {"inf", "float64", math.Inf(1)},
		{"s", "string", "s"}, {int64(-4), "string", "-4"}, {uint64(4), "large_string", "4"}, {1e20, "string", "1e+20"}, {123456789.0, "string", "123456789"},
		{1.5e-7, "string", "1.5e-7"}, {1e-6, "string", "0.000001"}, {math.NaN(), "string", "nan"}, {math.Inf(-1), "string", "-inf"}, {math.Copysign(0, -1), "string", "-0"},
		{true, "string", "true"}, {[]byte("b"), "string", "b"}, {when, "string", "2020-01-02 03:04:05"},
		{[]byte{0}, "binary", []byte{0}}, {"x", "large_binary", []byte("x")},
		{"2020-01-02T03:04:05", "timestamp[s]", time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)},
		{"2020-01-02T03:04:05.5+01:00", "timestamp[ms]", time.Date(2020, 1, 2, 2, 4, 5, 500000000, time.UTC)},
		{int64(1), "timestamp[ms]", time.UnixMilli(1).UTC()}, {int64(1), "date32", time.Date(1970, 1, 2, 0, 0, 0, 0, time.UTC)},
		{int64(86_400_000), "date64", time.Date(1970, 1, 2, 0, 0, 0, 0, time.UTC)}, {when, "date32", time.Date(2020, 1, 2, 0, 0, 0, 0, time.UTC)},
		{int64(5), "time64[us]", int64(5)}, {2.0, "duration[s]", int64(2)},
		{"1.25", "decimal128(5, 2)", "1.25"}, {int64(3), "decimal128(5, 2)", "3"}, {1.5, "decimal128(5, 2)", "1.50"},
	}
	for _, c := range ok {
		got, err := convert.Cast(c.v, arrow(t, c.dtype))
		require.NoError(t, err, "%v as %s", c.v, c.dtype)
		require.Equal(t, c.want, got, "%v as %s", c.v, c.dtype)
	}
	bad := []struct {
		v     any
		dtype string
	}{
		{"maybe", "bool"}, {1.5, "bool"}, {int64(300), "int8"}, {int64(-1), "uint8"}, {int64(-1), "uint64"}, {uint64(math.MaxUint64), "int64"},
		{1.5, "int64"}, {math.NaN(), "int64"}, {"x", "int32"}, {[]any{}, "int64"}, {"x", "float64"}, {[]any{}, "float32"},
		{[]byte{0xff}, "string"}, {[]any{}, "string"}, {int64(1), "binary"}, {"not a date", "timestamp[s]"}, {true, "date32"},
		{"x", "time32[ms]"}, {1.5, "duration[us]"}, {"abc", "decimal128(5, 2)"}, {true, "decimal128(5, 2)"}, {"x", "null"},
	}
	for _, c := range bad {
		_, err := convert.Cast(c.v, arrow(t, c.dtype))
		require.ErrorIs(t, err, convert.ErrCast, "%v as %s", c.v, c.dtype)
	}
	got, err := convert.Cast(nil, arrow(t, "int64"))
	require.NoError(t, err)
	require.Nil(t, got)

	// Lists, fixed-size lists and structs.
	list := &features.Arrow{ID: features.List, Elem: arrow(t, "int8")}
	got, err = convert.Cast([]any{int64(1), "2"}, list)
	require.NoError(t, err)
	require.Equal(t, []any{int64(1), int64(2)}, got)
	_, err = convert.Cast([]any{"x"}, list)
	require.ErrorIs(t, err, convert.ErrCast)
	_, err = convert.Cast("x", list)
	require.ErrorIs(t, err, convert.ErrCast)
	fixed := &features.Arrow{ID: features.FixedSizeList, Size: 2, Elem: arrow(t, "int8")}
	_, err = convert.Cast([]any{int64(1)}, fixed)
	require.ErrorIs(t, err, convert.ErrCast)
	st := &features.Arrow{ID: features.Struct, Fields: []features.ArrowField{{Name: "a", Type: arrow(t, "int8")}, {Name: "b", Type: arrow(t, "string")}}}
	got, err = convert.Cast([]any{int64(1), "x"}, st)
	require.NoError(t, err)
	require.Equal(t, []any{int64(1), "x"}, got)
	_, err = convert.Cast([]any{int64(1)}, st)
	require.ErrorIs(t, err, convert.ErrCast)
	_, err = convert.Cast([]any{"y", "x"}, st)
	require.ErrorIs(t, err, convert.ErrCast)
	_, err = convert.Cast(int64(1), st)
	require.ErrorIs(t, err, convert.ErrCast)
}

func TestCastFeature(t *testing.T) {
	label := &features.Feature{Kind: features.KindClassLabel, Names: []string{"neg", "pos"}}
	st, err := label.Storage()
	require.NoError(t, err)
	got, err := convert.CastFeature("pos", label, st)
	require.NoError(t, err)
	require.Equal(t, int64(1), got)
	got, err = convert.CastFeature(int64(-1), label, st)
	require.NoError(t, err)
	require.Equal(t, int64(-1), got)
	_, err = convert.CastFeature("neutral", label, st)
	require.ErrorIs(t, err, convert.ErrCast)
	_, err = convert.CastFeature(int64(2), label, st)
	require.ErrorIs(t, err, convert.ErrCast)
	_, err = convert.CastFeature(int64(-2), label, st)
	require.ErrorIs(t, err, convert.ErrCast)

	labels := features.ListOfFeature(label)
	lst, err := labels.Storage()
	require.NoError(t, err)
	got, err = convert.CastFeature([]any{"neg", int64(1)}, labels, lst)
	require.NoError(t, err)
	require.Equal(t, []any{int64(0), int64(1)}, got)
	_, err = convert.CastFeature([]any{"bad"}, labels, lst)
	require.Error(t, err)

	fs, err := features.ParseFeatures([]byte(`{"s":{"lab":{"names":["a","b"],"_type":"ClassLabel"},"n":{"dtype":"int8","_type":"Value"}}}`))
	require.NoError(t, err)
	sf := fs[0].Feature
	sst, err := sf.Storage()
	require.NoError(t, err)
	got, err = convert.CastFeature([]any{"b", int64(3)}, sf, sst)
	require.NoError(t, err)
	require.Equal(t, []any{int64(1), int64(3)}, got)
	_, err = convert.CastFeature([]any{"c", int64(3)}, sf, sst)
	require.True(t, errors.Is(err, convert.ErrCast))
	got, err = convert.CastFeature(nil, sf, sst)
	require.NoError(t, err)
	require.Nil(t, got)
}

func readFile(p string) ([]byte, error) { return os.ReadFile(p) }

func zipOf(t *testing.T, files map[string][]byte) []byte {
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for name, b := range files {
		f, err := w.Create(name)
		require.NoError(t, err)
		_, err = f.Write(b)
		require.NoError(t, err)
	}
	require.NoError(t, w.Close())
	return buf.Bytes()
}

// TestFolderArchiveFileNames checks where metadata file names resolve: a
// metadata file in an archive names files of the same archive and nothing
// outside it, and a metadata file in the Repo does not reach into an
// archive, as `datasets` joins the names to the metadata file's folder.
func TestFolderArchiveFileNames(t *testing.T) {
	png := mustRead(t, "../media/testdata/i.png")
	ctx := context.Background()
	convertFiles := func(files map[string]convert.File, data ...string) error {
		cfg := datasets.Config{Name: "default", Builder: "imagefolder", Files: map[string][]string{"train": data}, SplitOrder: []string{"train"}}
		res, err := convert.Convert(ctx, cfg, files, convert.Options{TempDir: t.TempDir(), Open: func(_ context.Context, p string) (convert.File, error) {
			f, ok := files[p]
			if !ok {
				return convert.File{}, fmt.Errorf("no file %s", p)
			}
			return f, nil
		}})
		if res != nil {
			res.Cleanup()
		}
		return err
	}

	inside := zipOf(t, map[string][]byte{"a.png": png, "metadata.csv": []byte("file_name\na.png\n")})
	require.NoError(t, convertFiles(map[string]convert.File{"d/t.zip": memFile("d/t.zip", inside)}, "d/t.zip"))

	missing := zipOf(t, map[string][]byte{"a.png": png, "metadata.csv": []byte("file_name\nb.png\n")})
	err := convertFiles(map[string]convert.File{"d/t.zip": memFile("d/t.zip", missing)}, "d/t.zip")
	require.ErrorContains(t, err, "b.png is not in d/t.zip")

	escape := zipOf(t, map[string][]byte{"a.png": png, "metadata.csv": []byte("file_name\n../x.png\n")})
	err = convertFiles(map[string]convert.File{"d/t.zip": memFile("d/t.zip", escape), "x.png": memFile("x.png", png)}, "d/t.zip")
	require.ErrorContains(t, err, "must not leave the folder of the metadata file")

	// A Repo metadata file naming a file inside an archive finds nothing.
	err = convertFiles(map[string]convert.File{"d/t.zip": memFile("d/t.zip", zipOf(t, map[string][]byte{"a.png": png})),
		"d/metadata.csv": memFile("d/metadata.csv", []byte("file_name\nt.zip/a.png\n"))}, "d/metadata.csv", "d/t.zip")
	require.ErrorContains(t, err, "no file d/t.zip/a.png")

	// A corrupt archive is invalid.
	err = convertFiles(map[string]convert.File{"d/t.zip": memFile("d/t.zip", []byte("PK\x05\x06 not really"))}, "d/t.zip")
	require.ErrorIs(t, err, layout.ErrInvalidArchive)
}
