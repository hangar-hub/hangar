package viewer

import (
	"encoding/json"
	"math"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestAppendHashSuffixMatchesTheHub checks asset names against
// libcommon's append_hash_suffix (testdata/media.json, written by
// compat/viewer/oracle/cells_cases.py).
func TestAppendHashSuffixMatchesTheHub(t *testing.T) {
	raw, err := os.ReadFile("testdata/media.json")
	require.NoError(t, err)
	var oracle struct {
		Names map[string]string `json:"names"`
	}
	require.NoError(t, json.Unmarshal(raw, &oracle))
	require.NotEmpty(t, oracle.Names)
	for pathJSON, want := range oracle.Names {
		var path []any
		require.NoError(t, json.Unmarshal([]byte(pathJSON), &path))
		require.Equal(t, want, appendHashSuffix("image", path), pathJSON)
		// Paths built while encoding hold ints rather than float64s.
		for i, p := range path {
			if f, ok := p.(float64); ok {
				path[i] = int(f)
			}
		}
		require.Equal(t, want, appendHashSuffix("image", path), pathJSON)
	}
	require.Equal(t, "image", appendHashSuffix("image", nil))
}

func TestPyFloat(t *testing.T) {
	for v, want := range map[float64]string{
		1: "1.0", -0.5: "-0.5", 1e16: "1e16", 1e22: "1e22", 1e-7: "1e-7", 2.5e-300: "2.5e-300", 123456789.125: "123456789.125",
		0.0001: "0.0001", 1.5e-5: "0.000015", -2.5e-5: "-0.000025", 1e-6: "1e-6", 1.5e16: "1.5e16", 1e15: "1000000000000000.0",
		9.5e15: "9500000000000000.0", 1.2345678901234568e16: "1.2345678901234568e16", 5e-324: "5e-324",
		1.7976931348623157e308: "1.7976931348623157e308", 0.1: "0.1", 1.0 / 3: "0.3333333333333333", 0: "0.0",
		math.Copysign(0, -1): "-0.0", math.NaN(): "null", math.Inf(1): "null", math.Inf(-1): "null",
	} {
		b, err := json.Marshal(pyFloat(v))
		require.NoError(t, err)
		require.Equal(t, want, string(b), v)
	}
}

func TestPyTimedelta(t *testing.T) {
	require.Equal(t, "0:00:01", pyTimedelta(1, 1))
	require.Equal(t, "0:00:00.000001", pyTimedelta(1000, 1_000_000_000))
	require.Equal(t, "-1 day, 23:59:59", pyTimedelta(-1, 1))
	require.Equal(t, "3 days, 1:00:00", pyTimedelta(3*86400+3600, 1))
}

func TestTruncateCell(t *testing.T) {
	_, ok := truncateCell("short", 100)
	require.False(t, ok)
	s, ok := truncateCell("ééééé", 6)
	require.True(t, ok)
	require.Equal(t, `"éé`, s, "cut at a character boundary")
	_, ok = truncateCell(func() {}, 1)
	require.False(t, ok)
	require.Zero(t, jsonSize(func() {}))
}

func TestUnescapeHTML(t *testing.T) {
	b, err := json.Marshal(map[string]string{"a": "<b> & \\u003c    é"})
	require.NoError(t, err)
	got := unescapeHTML(b)
	require.Equal(t, "{\"a\":\"<b> & \\\\u003c    é\"}", string(got))
	var back map[string]string
	require.NoError(t, json.Unmarshal(got, &back))
	require.Equal(t, "<b> & \\u003c    é", back["a"])
	require.Equal(t, `{"a":"\n"}`, string(unescapeHTML([]byte(`{"a":"\n"}`))))
	require.Equal(t, `"\u0001"`, string(unescapeHTML([]byte(`"\u0001"`))))
}
