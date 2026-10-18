package meta_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/datasets/card"
	"github.com/hangar-hub/hangar/internal/datasets/features"
	"github.com/hangar-hub/hangar/internal/datasets/meta"
	"github.com/hangar-hub/hangar/internal/datasets/pipeline"
)

func parse(t *testing.T, js string) features.Features {
	t.Helper()
	fs, err := features.ParseFeatures([]byte(js))
	require.NoError(t, err)
	return fs
}

func TestSizeCategory(t *testing.T) {
	for n, want := range map[int64]string{
		0: "n<1K", 999: "n<1K", 1000: "1K<n<10K", 99_999: "10K<n<100K", 100_000: "100K<n<1M", 5_000_000: "1M<n<10M",
		10_000_000: "10M<n<100M", 100_000_000: "100M<n<1B", 1_000_000_000: "1B<n<10B", 10_000_000_000: "10B<n<100B",
		100_000_000_000: "100B<n<1T", 1_000_000_000_000: "n>1T",
	} {
		require.Equal(t, want, meta.SizeCategory(n), n)
	}
}

func TestModalities(t *testing.T) {
	for name, tc := range map[string]struct {
		js   string
		want []string
	}{
		"text":        {`{"t":{"dtype":"string","_type":"Value"}}`, []string{"text"}},
		"large text":  {`{"t":{"dtype":"large_string","_type":"Value"}}`, []string{"text"}},
		"translation": {`{"t":{"languages":["en","fr"],"_type":"Translation"}}`, []string{"text"}},
		"tabular":     {`{"a":{"dtype":"int64","_type":"Value"},"b":{"dtype":"float32","_type":"Value"}}`, []string{"tabular"}},
		"one number":  {`{"a":{"dtype":"int64","_type":"Value"}}`, []string{}},
		"image":       {`{"i":{"_type":"Image"},"a":{"dtype":"int64","_type":"Value"},"b":{"dtype":"int64","_type":"Value"}}`, []string{"image"}},
		"nested audio": {`{"s":{"feature":{"a":{"_type":"Audio"}},"_type":"List"},"x":{"dtype":"int8","_type":"Value"},"y":{"dtype":"int8","_type":"Value"}}`,
			[]string{"audio"}},
		"video":      {`{"v":{"_type":"Video"}}`, []string{"video"}},
		"pdf":        {`{"p":{"_type":"Pdf"}}`, []string{"document"}},
		"timeseries": {`{"series":{"feature":{"dtype":"float32","_type":"Value"},"_type":"List"}}`, []string{"timeseries"}},
		"embeddings": {`{"embedding":{"feature":{"dtype":"float32","_type":"Value"},"_type":"List"}}`, []string{}},
	} {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, tc.want, meta.Modalities(parse(t, tc.js)))
		})
	}
}

func TestSummarise(t *testing.T) {
	c, err := card.Parse([]byte("---\npretty_name: Demo\nlicense: mit\nlanguage: [en]\ntask_categories: [text-classification]\nsize_categories: [10K<n<100K]\n---\n"), nil)
	require.NoError(t, err)
	infos := []*pipeline.ConfigInfo{
		{Config: "a", Builder: "csv", Features: parse(t, `{"t":{"dtype":"string","_type":"Value"}}`), Splits: []pipeline.SplitInfo{
			{Name: "train", NumExamples: 900, NumBytes: 9000, Shards: []pipeline.ShardInfo{{Size: 100}, {Size: 50}}},
			{Name: "test", NumExamples: 200, NumBytes: 2000},
		}},
		{Config: "b", Builder: "imagefolder", Features: parse(t, `{"i":{"_type":"Image"}}`), Splits: []pipeline.SplitInfo{{Name: "train", NumExamples: 1}}},
		{Config: "c", Builder: "csv"},
	}
	s := meta.Summarise(c, infos)
	require.EqualValues(t, 1101, s.NumRows)
	require.EqualValues(t, 11000, s.NumBytes)
	require.Equal(t, []string{"csv", "imagefolder"}, s.Formats)
	require.Equal(t, []string{"image", "text"}, s.Modalities)
	require.Equal(t, "1K<n<10K", s.SizeCategory, "converted rows win over the card")
	require.Equal(t, []string{"mit"}, s.Licences)
	require.Equal(t, "Demo", s.PrettyName)
	require.Len(t, s.Configs, 3)
	require.Equal(t, meta.SplitSummary{Name: "train", NumRows: 900, NumBytes: 9000, ParquetBytes: 150}, s.Configs[0].Splits[0])
	require.Equal(t, []string{"text-classification", "en", "1K - 10K", "csv", "imagefolder", "Image", "Text"}, s.Keywords())

	// Before conversion the card's size category is used.
	s = meta.Summarise(c, nil)
	require.Equal(t, "10K<n<100K", s.SizeCategory)
	require.Equal(t, []string{"text-classification", "en", "10K - 100K"}, s.Keywords())

	s = meta.Summarise(nil, nil)
	require.Equal(t, "n<1K", s.SizeCategory)
	require.Equal(t, []string{"< 1K"}, s.Keywords())
	s.SizeCategory = "n>1T"
	require.Equal(t, []string{"> 1T"}, s.Keywords())
	s.SizeCategory = ""
	require.Empty(t, s.Keywords())
}

func TestPlainText(t *testing.T) {
	md := "# Title\n\nSome **bold** and _it_ text with a [link](https://x) and ![img](a.png).\n\n\n\n" +
		"```python\ncode()\n```\n<div class=\"note\">html</div>\n  ## Sub\n`inline`\n"
	require.Equal(t, "Title\n\nSome bold and it text with a link and img.\n\nhtml\nSub\ninline", meta.PlainText(md))
	require.Empty(t, meta.PlainText("  \n"))
}

func TestModelCardDatasets(t *testing.T) {
	require.Nil(t, meta.ModelCardDatasets(nil))
	require.Nil(t, meta.ModelCardDatasets(&card.Card{}))
	c, err := card.Parse([]byte("---\ndatasets:\n- ns/a\n- ''\n- 3\n- b\n---\n"), nil)
	require.NoError(t, err)
	require.Equal(t, []string{"ns/a", "b"}, meta.ModelCardDatasets(c))
	c, err = card.Parse([]byte("---\ndatasets: ns/one\n---\n"), nil)
	require.NoError(t, err)
	require.Equal(t, []string{"ns/one"}, meta.ModelCardDatasets(c))
	c, err = card.Parse([]byte("---\ndatasets: {a: b}\n---\n"), nil)
	require.NoError(t, err)
	require.Empty(t, meta.ModelCardDatasets(c))
}
