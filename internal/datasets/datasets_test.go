package datasets_test

import (
	"encoding/json"
	"math"

	"testing"

	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/datasets"
)

func TestConfigSplitsAndLookup(t *testing.T) {
	c := datasets.Config{Name: "default", DataFiles: map[string][]string{
		"zeta": {"z/*"}, "test": {"t/*"}, "train": {"tr/*"}, "alpha": {"a/*"},
	}}
	require.Equal(t, []string{"train", "test", "alpha", "zeta"}, c.Splits())
	r := &datasets.Resolved{Configs: []datasets.Config{c}}
	got, ok := r.Config("default")
	require.True(t, ok)
	require.Equal(t, "default", got.Name)
	_, ok = r.Config("missing")
	require.False(t, ok)
}

func TestSplitOrder(t *testing.T) {
	c := datasets.Config{SplitOrder: []string{"b", "a"}, DataFiles: map[string][]string{"a": nil, "b": nil}}
	got := c.Splits()
	require.Equal(t, []string{"b", "a"}, got)
	got[0] = "changed"
	require.Equal(t, "b", c.SplitOrder[0], "Splits returns a copy")
}

func TestCellsKeepOrder(t *testing.T) {
	c := datasets.Cells{{Name: "z", Value: 1}, {Name: "a", Value: "x"}, {Name: "m", Value: nil}}
	b, err := json.Marshal(c)
	require.NoError(t, err)
	require.JSONEq(t, `{"z":1,"a":"x","m":null}`, string(b))
	require.Equal(t, `{"z":1,"a":"x","m":null}`, string(b))
	v, ok := c.Get("a")
	require.True(t, ok)
	require.Equal(t, "x", v)
	_, ok = c.Get("missing")
	require.False(t, ok)

	b, err = json.Marshal(datasets.Cells{})
	require.NoError(t, err)
	require.Equal(t, "{}", string(b))
	_, err = json.Marshal(datasets.Cells{{Name: "bad", Value: func() {}}})
	require.Error(t, err)
}

func TestParquetResponseShapes(t *testing.T) {
	// A dataset's response always lists pending and failed Configs.
	b, err := json.Marshal(datasets.ParquetResponse{ParquetFiles: []datasets.ParquetFile{}})
	require.NoError(t, err)
	require.Equal(t, `{"parquet_files":[],"pending":[],"failed":[],"partial":false}`, string(b))

	b, err = json.Marshal(datasets.ParquetResponse{ParquetFiles: []datasets.ParquetFile{}, Partial: true,
		Pending: []datasets.ConfigRef{{Dataset: "ns/d", Config: "c"}}})
	require.NoError(t, err)
	require.Equal(t, `{"parquet_files":[],"pending":[{"dataset":"ns/d","config":"c"}],"failed":[],"partial":true}`, string(b))

	// A Config's response carries its Features instead.
	b, err = json.Marshal(datasets.ParquetResponse{ParquetFiles: []datasets.ParquetFile{}, Features: json.RawMessage(`{"a":{"dtype":"int64","_type":"Value"}}`)})
	require.NoError(t, err)
	require.Equal(t, `{"parquet_files":[],"features":{"a":{"dtype":"int64","_type":"Value"}},"partial":false}`, string(b))
}

func TestFormatFloatLikeOrjson(t *testing.T) {
	// Outputs of orjson 3.11.3, which serialises the Hub's responses.
	for v, want := range map[float64]string{
		1: "1.0", -0.5: "-0.5", 0.0001: "0.0001", 1.5e-5: "0.000015", 1e-6: "1e-6", 1e-7: "1e-7",
		1e15: "1000000000000000.0", 9.5e15: "9500000000000000.0", 1e16: "1e16", 1.5e16: "1.5e16",
		1234567.5: "1234567.5", 1.0 / 3: "0.3333333333333333", 5e-324: "5e-324", 1.7976931348623157e308: "1.7976931348623157e308",
	} {
		require.Equal(t, want, datasets.FormatFloat(v), v)
	}
	require.Equal(t, "0.0", datasets.FormatFloat(0))
	require.Equal(t, "-0.0", datasets.FormatFloat(math.Copysign(0, -1)))
	require.Equal(t, "null", datasets.FormatFloat(math.NaN()))
	require.Equal(t, "null", datasets.FormatFloat(math.Inf(-1)))
}
