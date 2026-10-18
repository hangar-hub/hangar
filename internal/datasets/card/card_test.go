package card_test

import (
	"encoding/json"
	"errors"
	"math"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/datasets/card"
	"github.com/hangar-hub/hangar/internal/datasets/features"
)

func TestFrontMatter(t *testing.T) {
	for _, tc := range []struct {
		name, readme, yaml, body string
		ok                       bool
	}{
		{"plain", "---\na: 1\n---\n# Title\n", "a: 1", "# Title\n", true},
		{"bom", "\ufeff---\na: 1\n---\nbody", "a: 1", "body", true},
		{"crlf", "---\r\na: 1\r\n---\r\nbody", "a: 1", "body", true},
		{"leading space", "  \n---\na: 1\n---\n", "a: 1", "", true},
		{"none", "# Title\n---\na: 1\n---\n", "", "# Title\n---\na: 1\n---\n", false},
		{"unclosed", "---\na: 1\n", "", "---\na: 1\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			y, b, ok := card.FrontMatter(tc.readme)
			require.Equal(t, tc.ok, ok)
			require.Equal(t, tc.yaml, y)
			require.Equal(t, tc.body, b)
		})
	}
}

const fullCard = `---
pretty_name: My Data
license: apache-2.0
language:
- en
- fr
task_categories: text-classification
task_ids:
- sentiment-classification
size_categories:
- 1K<n<10K
tags:
- a
- 7
modalities: [text]
citation: "@article{x}"
homepage: https://example.com
configs:
- config_name: main
  default: true
  data_files:
  - split: train
    path: data/train-*
  - split: test
    path: [data/test.csv]
  sep: ";"
  features:
  - name: text
    dtype: string
- config_name: extra
  data_dir: /extra/
dataset_info:
- config_name: main
  features:
  - name: text
    dtype: string
  splits:
  - name: train
    num_bytes: 10
    num_examples: 2
  - name: test
    num_bytes: 5.0
    num_examples: 1
  - skipped
  download_size: 20
  dataset_size: 15
- features:
    label:
      dtype: int64
      _type: Value
---
# My Data

Some text.
`

func TestParseFullCard(t *testing.T) {
	c, err := card.Parse([]byte(fullCard), nil)
	require.NoError(t, err)
	require.Equal(t, "My Data", c.PrettyName)
	require.Equal(t, []string{"apache-2.0"}, c.Licenses)
	require.Equal(t, []string{"en", "fr"}, c.Languages)
	require.Equal(t, []string{"text-classification"}, c.TaskCategories)
	require.Equal(t, []string{"sentiment-classification"}, c.TaskIDs)
	require.Equal(t, []string{"1K<n<10K"}, c.SizeCategories)
	require.Equal(t, []string{"a", "7"}, c.Tags)
	require.Equal(t, []string{"text"}, c.Modalities)
	require.Equal(t, "@article{x}", c.Citation)
	require.Equal(t, "https://example.com", c.Homepage)
	require.Equal(t, "# My Data\n\nSome text.\n", c.Body)

	require.Len(t, c.Configs, 2)
	main := c.Configs[0]
	require.Equal(t, "main", main.Name)
	require.True(t, main.Default)
	require.Equal(t, []card.SplitPatterns{{Split: "train", Patterns: []string{"data/train-*"}}, {Split: "test", Patterns: []string{"data/test.csv"}}}, main.DataFiles)
	require.Equal(t, map[string]any{"sep": ";"}, main.Options)
	require.Len(t, main.Features, 1)
	require.Equal(t, "extra", c.Configs[1].DataDir)
	def, err := c.DefaultConfig()
	require.NoError(t, err)
	require.Equal(t, "main", def)

	in, ok := c.Info("main")
	require.True(t, ok)
	require.Equal(t, []card.SplitInfo{{Name: "train", NumBytes: 10, NumExamples: 2}, {Name: "test", NumBytes: 5, NumExamples: 1}}, in.Splits)
	require.EqualValues(t, 20, in.DownloadSize)
	require.EqualValues(t, 15, in.DatasetSize)
	in, ok = c.Info("default")
	require.True(t, ok, "an entry without config_name is the default Config's")
	b, err := json.Marshal(in.Features)
	require.NoError(t, err)
	require.JSONEq(t, `{"label":{"dtype":"int64","_type":"Value"}}`, string(b))
	_, ok = c.Info("missing")
	require.False(t, ok)
}

func TestParseStandaloneYAMLOverrides(t *testing.T) {
	c, err := card.Parse([]byte("---\npretty_name: A\nlicense: mit\n---\n"), []byte("pretty_name: B\nconfigs:\n- config_name: x\n  data_files: a.csv\n"))
	require.NoError(t, err)
	require.Equal(t, "B", c.PrettyName)
	require.Equal(t, []string{"mit"}, c.Licenses)
	require.Equal(t, []card.SplitPatterns{{Split: "train", Patterns: []string{"a.csv"}}}, c.Configs[0].DataFiles)
	def, err := c.DefaultConfig()
	require.NoError(t, err)
	require.Equal(t, "x", def, "a single Config is the default")

	_, err = card.Parse(nil, []byte("a: [unclosed"))
	require.ErrorIs(t, err, card.ErrInvalid)
}

func TestParseEmptyAndOddFrontMatter(t *testing.T) {
	for _, readme := range []string{"", "# no metadata\n", "---\n\n---\nbody", "---\n~\n---\n", "---\n# only a comment\n---\n"} {
		c, err := card.Parse([]byte(readme), nil)
		require.NoError(t, err, readme)
		require.Empty(t, c.Configs)
		require.NotNil(t, c.Data)
	}
	_, err := card.Parse([]byte("---\n- a\n- b\n---\n"), nil)
	require.ErrorIs(t, err, card.ErrInvalid)
	_, err = card.Parse([]byte("---\na: b: c\n---\n"), nil)
	require.ErrorIs(t, err, card.ErrInvalid)
}

func TestParseErrors(t *testing.T) {
	for name, yaml := range map[string]string{
		"configs not a list":      "configs: x",
		"config not a mapping":    "configs:\n- x",
		"config without name":     "configs:\n- data_files: a.csv",
		"duplicate config":        "configs:\n- config_name: a\n- config_name: a",
		"two defaults":            "configs:\n- config_name: a\n  default: true\n- config_name: default",
		"default not bool":        "configs:\n- config_name: a\n  default: yes please",
		"data_dir not string":     "configs:\n- config_name: a\n  data_dir: [x]",
		"data_files number":       "configs:\n- config_name: a\n  data_files: 3",
		"data_files bad entry":    "configs:\n- config_name: a\n  data_files:\n  - split: train\n  - x",
		"data_files bad split":    "configs:\n- config_name: a\n  data_files:\n  - split: 'a b'\n    path: x",
		"data_files dup split":    "configs:\n- config_name: a\n  data_files:\n  - split: train\n    path: x\n  - split: train\n    path: y",
		"data_files bad path":     "configs:\n- config_name: a\n  data_files:\n  - split: train\n    path: 3",
		"data_files map bad path": "configs:\n- config_name: a\n  data_files:\n    train: {a: b}",
		"data_files list numbers": "configs:\n- config_name: a\n  data_files: [1, 2]",
		"config features":         "configs:\n- config_name: a\n  features: 3",
		"dataset_info string":     "dataset_info: x",
		"dataset_info item":       "dataset_info:\n- x",
		"dataset_info features":   "dataset_info:\n  features: x",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := card.Parse([]byte("---\n"+yaml+"\n---\n"), nil)
			require.ErrorIs(t, err, card.ErrInvalid)
		})
	}
}

func TestSanitizePatterns(t *testing.T) {
	m := &card.Map{Items: []card.KV{{Key: "train", Value: "a.csv"}, {Key: "test", Value: []any{"b.csv", "c.csv"}}}}
	got, err := card.SanitizePatterns(m)
	require.NoError(t, err)
	require.Equal(t, []card.SplitPatterns{{Split: "train", Patterns: []string{"a.csv"}}, {Split: "test", Patterns: []string{"b.csv", "c.csv"}}}, got)
	got, err = card.SanitizePatterns([]any{"a.csv", "b.csv"})
	require.NoError(t, err)
	require.Equal(t, []card.SplitPatterns{{Split: "train", Patterns: []string{"a.csv", "b.csv"}}}, got)
	_, err = card.SanitizePatterns(nil)
	require.Error(t, err)
	require.True(t, card.ValidSplitName("train.v1"))
	require.False(t, card.ValidSplitName("train-v1"))
	require.False(t, card.ValidSplitName(""))
}

func TestFeaturesFromYAMLMatchesDatasets(t *testing.T) {
	raw, err := os.ReadFile("testdata/features.json")
	require.NoError(t, err)
	var cases []struct {
		Name     string          `json:"name"`
		YAML     string          `json:"yaml"`
		Features json.RawMessage `json:"features"`
		Error    string          `json:"error"`
	}
	require.NoError(t, json.Unmarshal(raw, &cases))
	require.NotEmpty(t, cases)
	for _, tc := range cases {
		t.Run(tc.Name, func(t *testing.T) {
			c, err := card.Parse([]byte("---\ndataset_info:\n  features:\n"+indent(tc.YAML, "  ")+"---\n"), nil)
			if tc.Error != "" {
				require.ErrorIs(t, err, card.ErrInvalid)
				return
			}
			require.NoError(t, err)
			in, ok := c.Info("default")
			require.True(t, ok)
			want, err := features.ParseFeatures(tc.Features)
			require.NoError(t, err)
			wb, err := json.Marshal(want)
			require.NoError(t, err)
			gb, err := json.Marshal(in.Features)
			require.NoError(t, err)
			require.Equal(t, string(wb), string(gb))
		})
	}
}

func indent(s, prefix string) string {
	lines := strings.SplitAfter(s, "\n")
	for i, l := range lines {
		if l != "" {
			lines[i] = prefix + l
		}
	}
	return strings.Join(lines, "")
}

func TestFeaturesFromYAMLErrors(t *testing.T) {
	for name, v := range map[string]any{
		"not a list":     "x",
		"item not a map": []any{"x"},
		"no name":        []any{&card.Map{Items: []card.KV{{Key: "dtype", Value: "string"}}}},
		"bad label id": []any{&card.Map{Items: []card.KV{{Key: "name", Value: "l"}, {Key: "class_label", Value: &card.Map{Items: []card.KV{
			{Key: "names", Value: &card.Map{Items: []card.KV{{Key: "zero", Value: "a"}}}}}}}}}},
		"list of nonsense": []any{&card.Map{Items: []card.KV{{Key: "name", Value: "l"}, {Key: "list", Value: int64(3)}}}},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := card.FeaturesFromYAML(v)
			require.Error(t, err)
		})
	}
	// Deeply nested lists are refused rather than recursing without end.
	var v any = &card.Map{Items: []card.KV{{Key: "dtype", Value: "string"}}}
	for range 100 {
		v = &card.Map{Items: []card.KV{{Key: "list", Value: v}}}
	}
	_, err := card.FeaturesFromYAML([]any{&card.Map{Items: []card.KV{{Key: "name", Value: "deep"}, {Key: "list", Value: v}}}})
	require.Error(t, err)

	// The JSON dict form is accepted too.
	fs, err := card.FeaturesFromYAML(&card.Map{Items: []card.KV{{Key: "a", Value: &card.Map{Items: []card.KV{{Key: "dtype", Value: "int64"}, {Key: "_type", Value: "Value"}}}}}})
	require.NoError(t, err)
	require.Len(t, fs, 1)
	// A dtype that is a mapping is read as the feature itself.
	fs, err = card.FeaturesFromYAML([]any{&card.Map{Items: []card.KV{{Key: "name", Value: "s"}, {Key: "dtype", Value: &card.Map{Items: []card.KV{{Key: "list", Value: "int64"}}}}}}})
	require.NoError(t, err)
	b, err := json.Marshal(fs)
	require.NoError(t, err)
	require.JSONEq(t, `{"s":{"feature":{"dtype":"int64","_type":"Value"},"_type":"List"}}`, string(b))
}

func TestMap(t *testing.T) {
	c, err := card.Parse([]byte(`---
base: &base
  a: 1
  b: two
merged:
  <<: *base
  b: three
alias: *base
nums:
  int: 12
  big: 123456789012345678901234567890
  float: 1.5
  nan: .nan
  inf: .inf
  ninf: -.inf
  hex: 0x10
  null: ~
  bool: true
  date: 2024-01-02
---
`), nil)
	require.NoError(t, err)
	merged := c.Data.Get("merged").(*card.Map)
	// PyYAML puts merged keys first; explicit keys win.
	require.Equal(t, []card.KV{{Key: "a", Value: int64(1)}, {Key: "b", Value: "three"}}, merged.Items)
	require.Equal(t, int64(1), c.Data.Get("alias").(*card.Map).Get("a"))
	nums := c.Data.Get("nums").(*card.Map)
	require.Equal(t, int64(12), nums.Get("int"))
	require.InDelta(t, 1.2345678901234568e29, nums.Get("big"), 1e15)
	require.Equal(t, 1.5, nums.Get("float"))
	require.True(t, math.IsNaN(nums.Get("nan").(float64)))
	require.True(t, math.IsInf(nums.Get("inf").(float64), 1))
	require.True(t, math.IsInf(nums.Get("ninf").(float64), -1))
	require.Equal(t, int64(16), nums.Get("hex"))
	require.Nil(t, nums.Get("null"))
	require.True(t, nums.Has("null"))
	require.False(t, nums.Has("missing"))
	require.Equal(t, true, nums.Get("bool"))
	require.Equal(t, "2024-01-02", nums.Get("date"))

	b, err := json.Marshal(nums)
	require.NoError(t, err)
	require.Equal(t, `{"int":12,"big":1.2345678901234568e+29,"float":1.5,"nan":"NaN","inf":"+Inf","ninf":"-Inf","hex":16,"null":null,"bool":true,"date":"2024-01-02"}`, string(b))

	var nilMap *card.Map
	require.Nil(t, nilMap.Get("x"))
	require.False(t, nilMap.Has("x"))
	m := &card.Map{}
	m.Set("a", 1)
	m.Set("a", 2)
	cl := m.Clone()
	cl.Set("b", 3)
	require.Len(t, m.Items, 1)
	require.Len(t, cl.Items, 2)
	require.Equal(t, []any{"NaN", int64(1)}, card.ToJSONValue([]any{math.NaN(), int64(1)}))
	_, err = json.Marshal(&card.Map{Items: []card.KV{{Key: "f", Value: func() {}}}})
	require.Error(t, err)
}

func TestDeepYAMLRefused(t *testing.T) {
	deep := strings.Repeat("[", 300) + strings.Repeat("]", 300)
	_, err := card.Parse([]byte("---\na: "+deep+"\n---\n"), nil)
	require.True(t, errors.Is(err, card.ErrInvalid))
}
