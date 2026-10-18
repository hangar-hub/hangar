package layout

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/datasets/card"
)

func files(paths ...string) []File {
	out := make([]File, len(paths))
	for i, p := range paths {
		out[i] = File{Path: p, Size: 1}
	}
	return out
}

// TestGetDataFilesPatterns ports test_get_data_files_patterns from
// datasets 5.1.0 tests/test_data_files.py: the patterns found for each
// layout match exactly the files of each Split, in Split order.
func TestGetDataFilesPatterns(t *testing.T) {
	rng := func(f string, n int) []string {
		out := make([]string, n)
		for i := range n {
			out[i] = sprintf(f, i)
		}
		return out
	}
	cases := []map[string][]string{
		{"train": {"train.txt"}, "validation": {"valid.txt"}, "test": {"test.txt"}},
		{"train": {"data/train.txt"}, "validation": {"data/valid.txt"}, "test": {"data/test.txt"}},
		{"train": {"train/split.txt"}, "validation": {"valid/split.txt"}, "test": {"test/split.txt"}},
		{"train": rng("data/train_%d.txt", 3), "validation": rng("data/validation_%d.txt", 3), "test": rng("data/test_%d.txt", 3)},
		{"train": rng("data/train-0000%d-of-00003.txt", 3), "validation": rng("data/validation-0000%d-of-00003.txt", 3),
			"test": rng("data/test-0000%d-of-00003.txt", 3), "random": rng("data/random-0000%d-of-00003.txt", 3)},
		{"train": {"dataset.txt"}},
		{"train": {"data/dataset.txt"}},
		{"train": {"data/image.jpg", "metadata.jsonl"}},
		{"train": {"data/image.jpg", "metadata.csv"}},
		{"train": {"my_train_dir/dataset.txt"}},
		{"train": {"data/my_train_file.txt"}},
		{"test": {"my_test_dir/dataset.txt"}},
		{"test": {"data/my_test_file.txt"}},
		{"validation": {"my_validation_dir/dataset.txt"}},
		{"validation": {"data/my_validation_file.txt"}},
		{"train": {"train_dir/dataset.txt"}},
		{"train": {"data/train_file.txt"}},
		{"test": {"test_dir/dataset.txt"}},
		{"test": {"data/test_file.txt"}},
		{"validation": {"validation_dir/dataset.txt"}},
		{"validation": {"data/validation_file.txt"}},
		{"train": {"my_train/dataset.txt"}},
		{"train": {"data/my_train.txt"}},
		{"test": {"my_test/dataset.txt"}},
		{"test": {"data/my_test.txt"}},
		{"validation": {"my_validation/dataset.txt"}},
		{"validation": {"data/my_validation.txt"}},
		{"test": {"eval.txt"}},
		{"test": {"data/eval.txt"}},
		{"test": {"eval/dataset.txt"}},
		{"validation": {"dev.txt"}},
		{"validation": {"data/dev.txt"}},
		{"validation": {"dev/dataset.txt"}},
		{"validation": {"val.txt"}},
		{"validation": {"data/val.txt"}},
		{"train": {"train.parquet"}, "validation": {"valid.parquet"}, "test": {"test.parquet"}},
		{"train": {"developers_list.txt"}},
		{"train": {"data/seqeval_results.txt"}},
		{"train": {"contest.txt"}},
		{"test": {"my.test.file.txt"}},
		{"test": {"my-test-file.txt"}},
		{"test": {"my_test_file.txt"}},
		{"test": {"my test file.txt"}},
		{"test": {"my-test_file.txt"}},
		{"test": {"test00001.txt"}},
		{"test": {"test/train.txt"}},
	}
	order := []string{"train", "validation", "test", "random"}
	for _, bases := range []string{"", "my_dir"} {
		for _, c := range cases {
			var all []string
			var wantOrder []string
			for _, s := range order {
				for _, f := range c[s] {
					if bases != "" {
						f = bases + "/" + f
					}
					all = append(all, f)
				}
				if c[s] != nil {
					wantOrder = append(wantOrder, s)
				}
			}
			r := &resolver{files: files(all...)}
			ps, err := r.dataPatterns(bases)
			require.NoError(t, err, c)
			var gotOrder []string
			for _, sp := range ps {
				gotOrder = append(gotOrder, sp.Split)
				var matched []string
				for _, p := range sp.Patterns {
					got, err := r.resolve(p, bases, nil)
					if err == nil {
						matched = append(matched, got...)
					}
				}
				var want []string
				for _, f := range c[sp.Split] {
					if bases != "" {
						f = bases + "/" + f
					}
					want = append(want, f)
				}
				require.Equal(t, want, matched, "%v split %s", c, sp.Split)
			}
			require.Equal(t, wantOrder, gotOrder, "%v", c)
		}
	}
}

func sprintf(f string, i int) string {
	out := []byte{}
	for j := 0; j < len(f); j++ {
		if f[j] == '%' && j+1 < len(f) && f[j+1] == 'd' {
			out = append(out, byte('0'+i))
			j++
			continue
		}
		out = append(out, f[j])
	}
	return string(out)
}

func TestDataPatternsWithTheWordDataTwice(t *testing.T) {
	r := &resolver{files: files("data/train-00001-of-00009.parquet")}
	ps, err := r.dataPatterns("")
	require.NoError(t, err)
	require.Equal(t, patternSet{{Split: "train", Patterns: []string{"data/train-[0-9][0-9][0-9][0-9][0-9]-of-[0-9][0-9][0-9][0-9][0-9]*.*"}}}, ps)
}

// The complex_data_dir fixture of tests/test_data_files.py and the counts
// of _TEST_PATTERNS_SIZES.
func TestResolvePatternComplexDataDir(t *testing.T) {
	r := &resolver{files: files("data/train.txt", "data/test.txt", "README.md", ".dummy", "data/subdir/train.txt",
		"data/subdir/test.txt", "data/.dummy_subdir/train.txt", "data/.dummy_subdir/test.txt", "__pycache__/script.py")}
	for pattern, want := range map[string]int{"*": 0, "**": 4, "**/*": 4, "*.txt": 0, "data/*": 2, "data/**": 4, "**/*.txt": 4, "**/train.txt": 2} {
		got, err := r.resolve(pattern, "", nil)
		if want == 0 {
			require.ErrorIs(t, err, ErrMissingFile, pattern)
			continue
		}
		require.NoError(t, err, pattern)
		require.Len(t, got, want, pattern)
	}
	got, err := r.resolve(".dummy", "", nil)
	require.NoError(t, err)
	require.Equal(t, []string{".dummy"}, got)
	got, err = r.resolve("data/.dummy_subdir/*", "", nil)
	require.NoError(t, err)
	require.Len(t, got, 2)
	got, err = r.resolve("__pycache__/*", "", nil)
	require.NoError(t, err)
	require.Equal(t, []string{"__pycache__/script.py"}, got)
	got, err = r.resolve("README.md", "", nil)
	require.NoError(t, err, "an explicitly named ignored file is kept")
	require.Equal(t, []string{"README.md"}, got)
	got, err = r.resolve("**", "", []string{".txt"})
	require.NoError(t, err)
	require.Len(t, got, 4)
	_, err = r.resolve("**", "", []string{".blablabla"})
	require.ErrorIs(t, err, ErrMissingFile)
	got, err = r.resolve("*.txt", "data", nil)
	require.NoError(t, err)
	require.Equal(t, []string{"data/test.txt", "data/train.txt"}, got)
	_, err = r.resolve("https://example.com/x.csv", "", nil)
	require.Error(t, err)
}

func TestSpecialAndHiddenHelpers(t *testing.T) {
	f := insideUnrequestedSpecialDir
	require.True(t, f("__pycache__/b.txt", "**"))
	require.True(t, f("__pycache__/b.txt", "*/b.txt"))
	require.False(t, f("__pycache__/b.txt", "__pycache__/*"))
	require.False(t, f("__pycache__/__b.txt", "__pycache__/*"))
	require.False(t, f("__pycache__/__b.txt", "__*/*"))
	require.False(t, f("__b.txt", "*"))
	h := unrequestedHidden
	require.True(t, h(".hidden_file.txt", "**"))
	require.False(t, h(".hidden_file.txt", ".*"))
	require.True(t, h(".hidden_dir/a.txt", "**"))
	require.False(t, h(".hidden_dir/a.txt", ".*/*"))
	require.False(t, h(".hidden_dir/a.txt", ".hidden_dir/*"))
	require.True(t, h(".hidden_dir/.hidden_file.txt", "**"))
	require.True(t, h(".hidden_dir/.hidden_file.txt", ".*/*"))
	require.False(t, h(".hidden_dir/.hidden_file.txt", ".*/.*"))
	require.True(t, h(".hidden_dir/.hidden_file.txt", ".hidden_dir/*"))
	require.False(t, h(".hidden_dir/.hidden_file.txt", ".hidden_dir/.*"))
}

func TestGlobRegexp(t *testing.T) {
	cases := []struct {
		pat     string
		matches []string
		not     []string
	}{
		{"**/train[-._ 0-9]*", []string{"train.csv", "a/b/train-1.json", "train_x"}, []string{"training.csv", "a/xtrain.csv"}},
		{"*", []string{"a"}, []string{"a/b"}},
		{"**", []string{"a", "a/b/c"}, nil},
		{"data/*.csv", []string{"data/x.csv"}, []string{"data/a/x.csv", "x.csv"}},
		{"a?c", []string{"abc"}, []string{"ac", "abbc"}},
		{"[!a]b", []string{"cb"}, []string{"ab"}},
		{"x[]", []string{}, []string{"x"}},
		{"x[!]y", []string{"x[!]y"}, []string{"xzy"}},
		{"[a-c]", []string{"b"}, []string{"d"}},
		{"[z-a]x", []string{}, []string{"ax", "zx"}},
		{"é*.txt", []string{"été.txt"}, []string{"a.txt"}},
		{"x[", []string{"x["}, nil},
		{"a[&|]", []string{"a&", "a|"}, nil},
		{"a[^b]", []string{"a^", "ab"}, []string{"ac"}},
	}
	for _, c := range cases {
		re, err := globRegexp(c.pat)
		require.NoError(t, err, c.pat)
		for _, m := range c.matches {
			require.True(t, re.MatchString(m), "%s should match %s (%s)", c.pat, m, re)
		}
		for _, m := range c.not {
			require.False(t, re.MatchString(m), "%s should not match %s (%s)", c.pat, m, re)
		}
	}
	_, err := globRegexp("a**b")
	require.ErrorIs(t, err, ErrInvalidPattern)
}

func TestResolveDefaultLayout(t *testing.T) {
	res, err := Resolve(files("README.md", ".gitattributes", "data/train.csv", "data/test.csv"), nil)
	require.NoError(t, err)
	require.Len(t, res.Configs, 1)
	c := res.Configs[0]
	require.Equal(t, "default", c.Name)
	require.True(t, c.Default)
	require.Equal(t, "csv", c.Builder)
	require.Equal(t, []string{"train", "test"}, c.Splits())
	require.Equal(t, []string{"data/train.csv"}, c.Files["train"])
	require.False(t, res.FromCard)

	res, err = Resolve(files("a.tsv"), nil)
	require.NoError(t, err)
	require.Equal(t, "\t", res.Configs[0].BuilderOptions["sep"])

	res, err = Resolve(files("train/0.jpg", "train/1.png", "train/metadata.csv", "test/0.jpg"), nil)
	require.NoError(t, err)
	require.Equal(t, "imagefolder", res.Configs[0].Builder)
	require.Equal(t, []string{"train/0.jpg", "train/1.png", "train/metadata.csv"}, res.Configs[0].Files["train"])

	// Parquet wins a tie.
	res, err = Resolve(files("a.parquet", "b.csv"), nil)
	require.NoError(t, err)
	require.Equal(t, "parquet", res.Configs[0].Builder)
	require.Equal(t, []string{"a.parquet"}, res.Configs[0].Files["train"])

	_, err = Resolve(files("train.csv", "test.json"), nil)
	require.ErrorIs(t, err, ErrMixedFormats)
	_, err = Resolve(files("README.md"), nil)
	require.ErrorIs(t, err, ErrEmptyDataset)
	_, err = Resolve(files("x.unknownext"), nil)
	require.ErrorIs(t, err, ErrEmptyDataset)
}

func TestResolveFromCard(t *testing.T) {
	readme := `---
configs:
- config_name: en
  data_files:
  - split: test
    path: en/test-*.csv
  - split: train
    path: [en/train.csv]
  sep: ";"
- config_name: fr
  data_dir: fr
  default: true
- config_name: all
  data_files: "*/train*"
dataset_info:
- config_name: fr
  features:
  - name: text
    dtype: string
  - name: label
    dtype:
      class_label:
        names:
          '0': neg
          '1': pos
---
# Card
`
	c, err := card.Parse([]byte(readme), nil)
	require.NoError(t, err)
	res, err := Resolve(files("README.md", "en/test-0.csv", "en/test-1.csv", "en/train.csv", "fr/train.csv", "fr/valid.csv"), c)
	require.NoError(t, err)
	require.True(t, res.FromCard)
	require.Len(t, res.Configs, 3)
	en, fr, all := res.Configs[0], res.Configs[1], res.Configs[2]
	require.Equal(t, []string{"test", "train"}, en.Splits())
	require.Equal(t, []string{"en/test-0.csv", "en/test-1.csv"}, en.Files["test"])
	require.Equal(t, ";", en.BuilderOptions["sep"])
	require.False(t, en.Default)
	require.True(t, fr.Default)
	require.Equal(t, []string{"train", "validation"}, fr.Splits())
	require.Equal(t, []string{"fr/valid.csv"}, fr.Files["validation"])
	require.Equal(t, []string{"text", "label"}, fr.Features.Names())
	require.Equal(t, []string{"en/train.csv", "fr/train.csv"}, all.Files["train"])

	bad, err := card.Parse([]byte("---\nconfigs:\n- config_name: x\n  data_files: missing.csv\n---\n"), nil)
	require.NoError(t, err)
	_, err = Resolve(files("a.csv"), bad)
	require.True(t, errors.Is(err, ErrMissingFile), err)
}
