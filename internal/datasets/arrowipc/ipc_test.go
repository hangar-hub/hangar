package arrowipc_test

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/datasets/arrowipc"
	"github.com/jamesagarside/hangar/internal/datasets/features"
)

// expected is types.json, written by pyarrow 26 from the same table as
// the .arrow fixtures (see the generator in the test data's history).
type expected struct {
	Columns []string `json:"columns"`
	Types   []string `json:"types"`
	Rows    [][]any  `json:"rows"`
}

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

// enc writes values the way the fixture generator wrote pyarrow's.
func enc(v any, a *features.Arrow) any {
	if v == nil {
		return nil
	}
	switch a.ID {
	case features.Binary:
		return map[string]any{"bytes": base64.StdEncoding.EncodeToString(v.([]byte))}
	case features.Timestamp:
		return map[string]any{"ts": pyISO(v.(time.Time), a)}
	case features.Date32:
		return map[string]any{"date": v.(time.Time).Format("2006-01-02")}
	case features.Time64:
		d := time.Duration(v.(int64)) * time.Microsecond
		return map[string]any{"time": fmt.Sprintf("%02d:%02d:%02d", int(d.Hours()), int(d.Minutes())%60, int(d.Seconds())%60)}
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
	case features.List, features.FixedSizeList:
		vals := v.([]any)
		out := make([]any, len(vals))
		for i, x := range vals {
			out[i] = enc(x, a.Elem)
		}
		return out
	case features.Uint64:
		return float64(v.(uint64))
	case features.Int8, features.Uint16:
		return float64(v.(int64))
	}
	if i, ok := v.(int64); ok {
		return float64(i)
	}
	return v
}

func readAll(t *testing.T, name string) (*arrowipc.Reader, [][]any) {
	f, err := os.Open("testdata/" + name)
	require.NoError(t, err)
	defer f.Close()
	r, err := arrowipc.NewReader(f)
	require.NoError(t, err)
	var rows [][]any
	for {
		batch, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		rows = append(rows, batch...)
	}
	return r, rows
}

func TestReadFixtures(t *testing.T) {
	raw, err := os.ReadFile("testdata/types.json")
	require.NoError(t, err)
	var want expected
	require.NoError(t, json.Unmarshal(raw, &want))
	// lz4.arrow and lz4_file.arrow are compat/viewer/oracle/arrow_lz4.py's
	// LZ4 frame copies of types.arrow.
	for _, name := range []string{"types.arrow", "types_file.arrow", "zstd.arrow", "lz4.arrow", "lz4_file.arrow"} {
		r, rows := readAll(t, name)
		require.Len(t, rows, len(want.Rows), name)
		require.Len(t, r.Schema.Type.Fields, len(want.Columns))
		for i, f := range r.Schema.Type.Fields {
			require.Equal(t, want.Columns[i], f.Name)
		}
		require.Contains(t, r.Schema.Metadata["huggingface"], `"_type": "Value"`)
		for i, row := range rows {
			for j, v := range row {
				got := enc(v, r.Schema.Type.Fields[j].Type)
				gb, _ := json.Marshal(got)
				wb, _ := json.Marshal(want.Rows[i][j])
				require.JSONEq(t, string(wb), string(gb), "%s row %d column %s", name, i, want.Columns[j])
			}
		}
	}
}

func TestTypeNames(t *testing.T) {
	r, _ := readAll(t, "types.arrow")
	var names []string
	for _, f := range r.Schema.Type.Fields {
		names = append(names, f.Type.String())
	}
	require.Equal(t, []string{"int8", "uint16", "uint64", "float16", "float32", "float64", "bool", "string", "large_string", "binary",
		"date32", "timestamp[ms]", "timestamp[us, tz=UTC]", "time64[us]", "duration[s]", "decimal128(10, 2)",
		"list<item: int64>", "fixed_size_list<item: float32>[2]", "struct<a: int64, b: string>", "null", "string"}, names)
}

func TestParseSchemaFromParquetMetadata(t *testing.T) {
	b64, err := os.ReadFile("testdata/arrow_schema.b64")
	require.NoError(t, err)
	raw, err := base64.StdEncoding.DecodeString(string(b64))
	require.NoError(t, err)
	s, err := arrowipc.ParseSchema(raw)
	require.NoError(t, err)
	require.Len(t, s.Type.Fields, 19)
	require.Equal(t, "large_string", s.Type.Fields[8].Type.String())
	require.Equal(t, "timestamp[us, tz=UTC]", s.Type.Fields[12].Type.String())
}

func TestCorruptInputs(t *testing.T) {
	good, err := os.ReadFile("testdata/types.arrow")
	require.NoError(t, err)
	for _, b := range [][]byte{nil, {1, 2}, []byte("ARROW1\x00\x00"), good[:20], bytes.Repeat([]byte{0xff}, 16)} {
		r, err := arrowipc.NewReader(bytes.NewReader(b))
		if err == nil {
			for {
				if _, err = r.Next(); err != nil {
					break
				}
			}
			if errors.Is(err, io.EOF) {
				err = nil
			}
		}
		require.Error(t, err, "%x", b)
	}
	// Truncating the body of the first batch is an error, not a panic.
	r, err := arrowipc.NewReader(bytes.NewReader(good[:len(good)-200]))
	require.NoError(t, err)
	_, err = r.Next()
	if err == nil {
		_, err = r.Next()
	}
	require.Error(t, err)
	_, err = arrowipc.ParseSchema([]byte{0, 0, 0, 0})
	require.Error(t, err)
}

// TestLZ4BufferSizes changes the uncompressed length that precedes the
// first LZ4 frame of lz4.arrow: a frame that decodes to more or fewer bytes
// than its length is invalid, and a length of a gigabyte costs no more
// memory than the frame decodes to.
func TestLZ4BufferSizes(t *testing.T) {
	good, err := os.ReadFile("testdata/lz4.arrow")
	require.NoError(t, err)
	at := bytes.Index(good, []byte{0x04, 0x22, 0x4d, 0x18})
	require.Greater(t, at, 8)
	size := int64(binary.LittleEndian.Uint64(good[at-8:]))
	readErr := func(b []byte) error {
		r, err := arrowipc.NewReader(bytes.NewReader(b))
		require.NoError(t, err)
		for {
			if _, err := r.Next(); err != nil {
				if errors.Is(err, io.EOF) {
					return nil
				}
				return err
			}
		}
	}
	for _, claim := range []int64{size - 1, size + 1, 1 << 30} {
		b := bytes.Clone(good)
		binary.LittleEndian.PutUint64(b[at-8:], uint64(claim))
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		err := readErr(b)
		runtime.ReadMemStats(&after)
		require.ErrorIs(t, err, arrowipc.ErrInvalid, "claimed %d bytes", claim)
		require.Contains(t, err.Error(), "LZ4 buffer", "claimed %d bytes", claim)
		require.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(16<<20), "claimed %d bytes", claim)
	}
	// A corrupt frame is invalid too.
	b := bytes.Clone(good)
	for i := at + 7; i < at+40 && i < len(b); i++ {
		b[i] ^= 0xff
	}
	require.ErrorIs(t, readErr(b), arrowipc.ErrInvalid)
}

func TestHelpers(t *testing.T) {
	require.Equal(t, 1.0, arrowipc.HalfToFloat(0x3c00))
	require.Equal(t, -2.0, arrowipc.HalfToFloat(0xc000))
	require.True(t, arrowipc.HalfToFloat(0x7c00) > 1e300)
	require.NotEqual(t, arrowipc.HalfToFloat(0x7e00), arrowipc.HalfToFloat(0x7e00))
	require.Equal(t, 5.960464477539063e-08, arrowipc.HalfToFloat(1))
	le := make([]byte, 16)
	le[0] = 5
	require.Equal(t, "0.05", arrowipc.DecimalString(le, 2))
	require.Equal(t, "500", arrowipc.DecimalString(le, -2))
	neg := bytes.Repeat([]byte{0xff}, 16)
	require.Equal(t, "-0.001", arrowipc.DecimalString(neg, 3))
	require.Equal(t, "-0."+strings.Repeat("0", 37)+"1", arrowipc.DecimalString(neg, 38))
}

// FuzzReader checks that no input makes the reader panic or loop.
func FuzzReader(f *testing.F) {
	for _, name := range []string{"types.arrow", "types_file.arrow", "zstd.arrow", "lz4.arrow"} {
		b, err := os.ReadFile("testdata/" + name)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		r, err := arrowipc.NewReader(bytes.NewReader(b))
		if err != nil {
			return
		}
		for range 1000 {
			if _, err := r.Next(); err != nil {
				return
			}
		}
	})
}
