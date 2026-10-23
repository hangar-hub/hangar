package convert_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/datasets"
	"github.com/hangar-hub/hangar/internal/datasets/convert"
)

// repeatReader yields pattern over and over until n bytes have been read,
// so tests can feed files larger than they would want to hold.
type repeatReader struct {
	pattern []byte
	off     int
	n       int64
}

func (r *repeatReader) Read(p []byte) (int, error) {
	if r.n <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > r.n {
		p = p[:r.n]
	}
	for i := range p {
		p[i] = r.pattern[r.off]
		r.off = (r.off + 1) % len(r.pattern)
	}
	r.n -= int64(len(p))
	return len(p), nil
}

// streamFile is a File read from readers made on each Open.
func streamFile(p string, parts func() []io.Reader) convert.File {
	return convert.File{Path: p, Open: func(context.Context) (io.ReadCloser, error) {
		return io.NopCloser(io.MultiReader(parts()...)), nil
	}}
}

func builderConfig(builder string, opts map[string]any, files ...string) datasets.Config {
	return datasets.Config{Name: "default", Builder: builder, BuilderOptions: opts,
		Files: map[string][]string{"train": files}, SplitOrder: []string{"train"}}
}

// bigJSONList writes n objects of about 200 bytes, the last with a key the
// others lack.
func bigJSONList(b *bytes.Buffer, n int) {
	pad := strings.Repeat("x", 180)
	b.WriteString("[")
	for i := range n {
		if i > 0 {
			b.WriteString(",\n")
		}
		if i == n-1 {
			fmt.Fprintf(b, `{"id": %d, "text": "%s", "late": true}`, i, pad)
			continue
		}
		fmt.Fprintf(b, `{"id": %d, "text": "%s"}`, i, pad)
	}
	b.WriteString("]")
}

func TestJSONArrayStreamsInBatches(t *testing.T) {
	// About 24 MB: several batches. The key only the last item has is
	// still inferred, as `datasets` infers from the whole array.
	const n = 120_000
	var b bytes.Buffer
	b.Write([]byte{0xef, 0xbb, 0xbf})
	bigJSONList(&b, n)
	files := map[string]convert.File{"a.json": memFile("a.json", b.Bytes())}
	res, err := convert.Convert(context.Background(), builderConfig("json", nil, "a.json"), files, convert.Options{TempDir: t.TempDir()})
	require.NoError(t, err)
	defer res.Cleanup()
	require.EqualValues(t, n, res.Splits[0].NumRows)
	require.Equal(t, []string{"id", "text", "late"}, res.Features.Names())

	// An empty array has no columns and no rows.
	files = map[string]convert.File{"e.json": memFile("e.json", []byte("[]"))}
	_, err = convert.Convert(context.Background(), builderConfig("json", nil, "e.json"), files, convert.Options{TempDir: t.TempDir()})
	require.NoError(t, err)
}

func TestJSONFieldStreams(t *testing.T) {
	const n = 60_000
	var b bytes.Buffer
	b.WriteString(`{"version": "1.0", "skipped": {"a": [1, 2.5, {"b": null}], "c": "d"}, "data": `)
	bigJSONList(&b, n)
	b.WriteString(`, "after": [true]}`)
	files := map[string]convert.File{"a.json": memFile("a.json", b.Bytes())}
	opts := map[string]any{"field": "data"}
	res, err := convert.Convert(context.Background(), builderConfig("json", opts, "a.json"), files, convert.Options{TempDir: t.TempDir()})
	require.NoError(t, err)
	defer res.Cleanup()
	require.EqualValues(t, n, res.Splits[0].NumRows)
	require.Equal(t, []string{"id", "text", "late"}, res.Features.Names())

	for name, body := range map[string]string{
		"has no field":           `{"other": []}`,
		"is not a list":          `{"data": {"a": 1}}`,
		"is not an object":       `[{"data": []}]`,
		"more than once":         `{"data": [{"a": 1}], "data": [{"a": 2}]}`,
		"extra data":             `{"data": [{"a": 1}]} {}`,
		"JSON nested too deeply": `{"skip": ` + strings.Repeat("[", 600) + strings.Repeat("]", 600) + `, "data": []}`,
	} {
		files := map[string]convert.File{"a.json": memFile("a.json", []byte(body))}
		_, err := convert.Convert(context.Background(), builderConfig("json", opts, "a.json"), files, convert.Options{TempDir: t.TempDir()})
		require.ErrorContains(t, err, name, body)
	}
}

func TestOversizedValuesFailCleanly(t *testing.T) {
	const big = 80 << 20 // more than the 64 MiB cap, plus a 10 MB chunk
	cases := []struct {
		name, file, builder, want string
		opts                      map[string]any
		parts                     func() []io.Reader
	}{
		{name: "one-line JSON Lines", file: "a.jsonl", builder: "json", want: "a line longer than",
			parts: func() []io.Reader {
				return []io.Reader{strings.NewReader(`{"text": "`), &repeatReader{pattern: []byte("a"), n: big}, strings.NewReader(`"}`)}
			}},
		{name: "one-line text", file: "a.txt", builder: "text", want: "a line longer than",
			parts: func() []io.Reader { return []io.Reader{&repeatReader{pattern: []byte("a"), n: big}} }},
		{name: "huge JSON array item", file: "a.json", builder: "json", want: "a JSON value longer than",
			parts: func() []io.Reader {
				return []io.Reader{strings.NewReader(`[{"text": "`), &repeatReader{pattern: []byte("a"), n: big}, strings.NewReader(`"}]`)}
			}},
		{name: "huge JSON field item", file: "a.json", builder: "json", want: "a JSON value longer than", opts: map[string]any{"field": "data"},
			parts: func() []io.Reader {
				return []io.Reader{strings.NewReader(`{"data": [{"text": "`), &repeatReader{pattern: []byte("a"), n: big}, strings.NewReader(`"}]}`)}
			}},
		{name: "huge skipped JSON member", file: "a.json", builder: "json", want: "a JSON value longer than", opts: map[string]any{"field": "data"},
			parts: func() []io.Reader {
				return []io.Reader{strings.NewReader(`{"skip": "`), &repeatReader{pattern: []byte("a"), n: big}, strings.NewReader(`", "data": []}`)}
			}},
		{name: "text document", file: "a.txt", builder: "text", want: "a file larger than", opts: map[string]any{"sample_by": "document"},
			parts: func() []io.Reader { return []io.Reader{&repeatReader{pattern: []byte("line\n"), n: big}} }},
		{name: "text paragraph", file: "a.txt", builder: "text", want: "a paragraph longer than", opts: map[string]any{"sample_by": "paragraph"},
			parts: func() []io.Reader { return []io.Reader{&repeatReader{pattern: []byte("line\n"), n: big}} }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			files := map[string]convert.File{c.file: streamFile(c.file, c.parts)}
			_, err := convert.Convert(context.Background(), builderConfig(c.builder, c.opts, c.file), files, convert.Options{TempDir: t.TempDir()})
			require.ErrorIs(t, err, convert.ErrUnsupported)
			require.ErrorContains(t, err, c.want)
		})
	}
}
