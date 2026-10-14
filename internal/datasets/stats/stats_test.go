package stats_test

import (
	"bytes"
	"encoding/json"
	"math"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/datasets/features"
	"github.com/jamesagarside/hangar/internal/datasets/stats"
)

// oracleCase is written by compat/viewer/oracle/stats_cases.py, which runs
// the Hub's own statistics_utils.py.
type oracleCase struct {
	Name   string          `json:"name"`
	Kind   string          `json:"kind"`
	Values []any           `json:"values"`
	Names  []string        `json:"names"`
	Item   json.RawMessage `json:"item"`
	Error  string          `json:"error"`
}

func feature(kind string, names []string) *features.Feature {
	switch kind {
	case "int":
		return features.Value("int64")
	case "float":
		return features.Value("float64")
	case "string":
		return features.Value("string")
	case "bool":
		return features.Value("bool")
	case "class_label":
		return &features.Feature{Kind: features.KindClassLabel, Names: names}
	case "list":
		return features.ListOfFeature(features.Value("int64"))
	case "datetime":
		return features.Value("timestamp[us]")
	}
	return nil
}

func storage(kind string, v any) any {
	if v == nil {
		return nil
	}
	switch kind {
	case "int", "class_label":
		return int64(v.(float64))
	case "list":
		var out []any
		for _, x := range v.([]any) {
			out = append(out, int64(x.(float64)))
		}
		if out == nil {
			out = []any{}
		}
		return out
	case "float":
		if v == "NaN" {
			return math.NaN()
		}
	case "datetime":
		t, err := time.Parse("2006-01-02T15:04:05", v.(string))
		if err != nil {
			panic(err)
		}
		return t
	}
	return v
}

// equalJSON compares two JSON documents, with objects as unordered maps
// and floats equal to 1e-9 relative.
func equalJSON(t *testing.T, want, got any, path string) {
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		require.True(t, ok, "%s: want an object, got %v", path, got)
		require.Len(t, g, len(w), "%s keys: want %v, got %v", path, w, g)
		for k, v := range w {
			equalJSON(t, v, g[k], path+"."+k)
		}
	case []any:
		g, ok := got.([]any)
		require.True(t, ok, "%s: want a list, got %v", path, got)
		require.Len(t, g, len(w), "%s: want %v, got %v", path, w, g)
		for i := range w {
			equalJSON(t, w[i], g[i], path)
		}
	case float64:
		g, ok := got.(float64)
		require.True(t, ok, "%s: want %v, got %v", path, w, got)
		require.InDelta(t, w, g, 1e-9*math.Max(1, math.Abs(w)), path)
	default:
		require.Equal(t, want, got, path)
	}
}

func TestMatchesHubStatistics(t *testing.T) {
	raw, err := os.ReadFile("testdata/cases.json")
	require.NoError(t, err)
	// Python writes NaN as a bare token.
	raw = bytes.ReplaceAll(raw, []byte("NaN"), []byte(`"NaN"`))
	var cases []oracleCase
	require.NoError(t, json.Unmarshal(raw, &cases))
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			var item *stats.Item
			switch c.Kind {
			case "image", "audio":
				vals := make([]float64, len(c.Values))
				for i, v := range c.Values {
					vals[i] = math.NaN()
					if v != nil {
						vals[i] = v.(float64)
					}
				}
				typ := stats.TypeImage
				if c.Kind == "audio" {
					typ = stats.TypeAudio
				}
				item = &stats.Item{ColumnName: c.Name, ColumnType: typ, Statistics: stats.Measures(vals, c.Kind == "image")}
			default:
				vals := make([]any, len(c.Values))
				for i, v := range c.Values {
					vals[i] = storage(c.Kind, v)
				}
				var err error
				item, err = stats.Column(c.Name, feature(c.Kind, c.Names), vals)
				require.NoError(t, err)
			}
			got, err := json.Marshal(item)
			require.NoError(t, err)
			if c.Error != "" {
				// The Hub fails on two-value integer ranges; Hangar answers
				// with a one-bin histogram (compat/viewer/DIFFERENCES.md).
				require.Contains(t, c.Error, "histogram computation")
				require.Contains(t, string(got), `"hist":[4]`)
				return
			}
			var w, g any
			require.NoError(t, json.Unmarshal(c.Item, &w))
			require.NoError(t, json.Unmarshal(got, &g))
			equalJSON(t, w, g, c.Name)
		})
	}
}
