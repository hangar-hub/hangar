//go:build compat

package viewer

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/datasets/card"
	"github.com/hangar-hub/hangar/internal/datasets/layout"
)

type layoutCase struct {
	Name  string             `json:"name"`
	Files map[string]*string `json:"files"`
	// Archives are zip archives and the names of their entries, in order.
	Archives map[string][]string `json:"archives,omitempty"`
}

func readme(yaml string) *string {
	s := "---\n" + yaml + "---\n# Card\n"
	return &s
}

// layoutCases are layouts from the `datasets` library's own tests
// (tests/test_data_files.py, tests/test_load.py and
// tests/test_metadata_util.py) and edge cases around them.
func layoutCases() []layoutCase {
	names := func(name string, paths ...string) layoutCase {
		c := layoutCase{Name: name, Files: map[string]*string{}}
		for _, p := range paths {
			c.Files[p] = nil
		}
		return c
	}
	withCard := func(c layoutCase, yaml string) layoutCase {
		c.Files["README.md"] = readme(yaml)
		return c
	}
	// withZip adds a zip archive with entries to a case. `datasets` looks
	// inside archives to choose the builder
	// (infer_module_for_data_files_list_in_archives).
	withZip := func(c layoutCase, archive string, entries ...string) layoutCase {
		if c.Archives == nil {
			c.Archives = map[string][]string{}
		}
		c.Archives[archive] = entries
		return c
	}
	eleven := names("zip_eleven_archives")
	for i := range 10 {
		eleven = withZip(eleven, fmt.Sprintf("data/a%02d.zip", i), "notes.txt")
	}
	eleven = withZip(eleven, "data/a10.zip", "1.csv", "2.csv", "3.csv", "4.csv", "5.csv", "6.csv", "7.csv", "8.csv", "9.csv", "10.csv", "11.csv")
	return []layoutCase{
		names("flat_splits", "train.csv", "valid.csv", "test.csv"),
		names("data_dir_splits", "data/train.txt", "data/valid.txt", "data/test.txt"),
		names("split_dirs", "train/split.jsonl", "valid/split.jsonl", "test/split.jsonl"),
		names("numbered_shards", "data/train_0.txt", "data/train_1.txt", "data/train_2.txt", "data/validation_0.txt", "data/test_0.txt"),
		names("hub_shards", "data/train-00000-of-00003.parquet", "data/train-00001-of-00003.parquet",
			"data/train-00002-of-00003.parquet", "data/test-00000-of-00001.parquet"),
		names("custom_split_names", "data/random-00000-of-00001.parquet", "data/train-00000-of-00001.parquet"),
		names("custom_split_dirs", "data/random/0000.parquet", "data/train/0000.parquet"),
		names("single_file", "dataset.txt"),
		names("single_file_in_data", "data/dataset.txt"),
		names("keyword_in_dir_name", "my_train_dir/dataset.txt", "my_test_dir/dataset.txt"),
		names("keyword_in_file_name", "data/my_train_file.txt", "data/my_validation_file.txt"),
		names("keyword_variants", "training.csv", "dev.csv", "eval.csv"),
		names("keyword_val_valid", "val.csv", "train.csv"),
		names("not_a_keyword", "data/pretrain.csv", "data/contest.csv"),
		names("nested_split_dirs", "data/train/part-0.parquet", "data/train/part-1.parquet", "data/test/part-0.parquet"),
		names("data_dir_wins", "data/train.csv", "data/test.csv", "other/train.csv"),
		names("hidden_files", "train.csv", ".hidden/test.csv", "data_dir/.x.csv", "test.csv"),
		names("special_dirs", "train.csv", "__pycache__/x.csv", "test.csv"),
		names("tsv", "train.tsv", "test.tsv"),
		names("jsonl_gz", "train.jsonl.gz", "test.jsonl.gz"),
		names("majority_extension", "data/a.csv", "data/b.csv", "notes.txt"),
		names("mixed_formats", "train.csv", "test.json"),
		names("readme_only"),
		names("unsupported_only", "model.safetensors", "script.py"),
		names("imagefolder_metadata", "train/0.jpg", "train/1.jpg", "train/metadata.jsonl", "test/0.jpg", "test/metadata.jsonl"),
		names("imagefolder_metadata_csv", "data/image.jpg", "metadata.csv"),
		names("imagefolder_labels", "train/cat/1.png", "train/dog/2.png", "test/cat/3.png"),
		names("imagefolder_with_text", "images/1.png", "images/2.png", "notes.txt"),
		names("audiofolder", "1.wav", "2.wav", "metadata.jsonl"),
		names("arrow", "data-00000-of-00001.arrow", "state.json", "dataset_info.json"),
		names("webdataset", "train-000.tar", "train-001.tar", "test-000.tar"),
		names("unicode_paths", "données/train.csv", "données/test.csv"),
		names("spaces", "my data/train file.csv", "my data/test file.csv"),
		names("all_splits_dirs", "train/a.parquet", "validation/a.parquet", "test/a.parquet", "random/a.parquet"),
		withCard(names("card_two_configs", "a/train.csv", "a/test.csv", "b/train.csv"),
			"configs:\n- config_name: first\n  data_files:\n  - split: train\n    path: a/train.csv\n  - split: test\n    path: a/test.csv\n- config_name: second\n  data_files: b/*.csv\n"),
		withCard(names("card_default_flag", "a/train.csv", "b/train.csv"),
			"configs:\n- config_name: first\n  data_files: a/*.csv\n- config_name: second\n  default: true\n  data_files: b/*.csv\n"),
		withCard(names("card_default_name", "a/train.csv", "b/train.csv"),
			"configs:\n- config_name: default\n  data_files: a/*.csv\n- config_name: other\n  data_files: b/*.csv\n"),
		withCard(names("card_no_default", "a/train.csv", "b/train.csv"),
			"configs:\n- config_name: first\n  data_files: a/*.csv\n- config_name: second\n  data_files: b/*.csv\n"),
		withCard(names("card_data_dir", "en/train.csv", "en/test.csv", "fr/train.csv"),
			"configs:\n- config_name: en\n  data_dir: en\n- config_name: fr\n  data_dir: fr\n"),
		withCard(names("card_split_list", "data/x1.csv", "data/x2.csv", "data/y.csv"),
			"configs:\n- config_name: default\n  data_files:\n  - split: train\n    path:\n    - data/x1.csv\n    - data/x2.csv\n  - split: test\n    path: data/y*.csv\n"),
		withCard(names("card_missing_file", "data/x.csv"),
			"configs:\n- config_name: default\n  data_files:\n  - split: train\n    path: data/missing.csv\n"),
		withCard(names("card_glob_star_star", "data/a/x.jsonl", "data/b/y.jsonl"),
			"configs:\n- config_name: default\n  data_files: data/**/*.jsonl\n"),
		withCard(names("card_custom_split", "data/x.csv", "data/y.csv"),
			"configs:\n- config_name: default\n  data_files:\n  - split: random\n    path: data/x.csv\n  - split: other\n    path: data/y.csv\n"),
		withCard(names("card_without_configs", "train.csv"),
			"license: mit\npretty_name: Something\n"),
		withCard(names("card_dataset_info_only", "train.csv", "test.csv"),
			"dataset_info:\n  features:\n  - name: a\n    dtype: int64\n  splits:\n  - name: train\n    num_examples: 1\n"),
		withZip(names("zip_csv"), "data/train.zip", "a.csv"),
		withZip(names("zip_images_metadata"), "train.zip", "0.jpg", "1.jpg", "metadata.csv"),
		withZip(names("zip_tie_metadata_first"), "train.zip", "metadata.csv", "z.jpg"),
		withZip(names("zip_tie_image_first"), "train.zip", "a.jpg", "metadata.csv"),
		withZip(names("zip_dotted_folders"), "train.zip", "x.v1/a.csv", "y.v1/b.jsonl", "c.jsonl"),
		withZip(names("zip_hidden_entries"), "train.zip", "a.csv", "__MACOSX/._a.csv", "__MACOSX/._b.jsonl", "__MACOSX/._c.jsonl", ".hidden.jsonl", "b.jsonl"),
		withZip(names("zip_readme_inside"), "train.zip", "README.md", "a.csv"),
		withZip(names("zip_upper_case"), "train.zip", "A.JPG", "B.JPG"),
		withZip(names("zip_folder_entries"), "train.zip", "d/", "d/a.csv"),
		withZip(names("zip_compressed_inside"), "train.zip", "a.csv.gz"),
		withZip(names("zip_empty"), "train.zip"),
		withZip(names("zip_unknown_extension"), "train.zip", "a.xyz", "b.xyz"),
		withZip(withZip(names("zip_splits"), "data/train.zip", "a.csv"), "data/test.zip", "b.csv"),
		withZip(withZip(names("zip_mixed_formats"), "train.zip", "a.csv"), "test.zip", "b.jsonl"),
		withZip(names("zip_beside_csv", "data/train.csv"), "data/train.zip", "a.jsonl", "b.jsonl"),
		withZip(names("zip_beside_two_csv", "data/train_1.csv", "data/train_2.csv"), "data/train.zip", "a.jsonl"),
		withZip(names("zip_audio_metadata", "data/train/notes.txt"), "data/train/clips.zip", "a.wav", "b.wav", "metadata.jsonl"),
		eleven,
	}
}

// fixtureCases are the compat/viewer datasets' own files and cards.
func fixtureCases(t *testing.T) []layoutCase {
	var out []layoutCase
	for _, entry := range fixtures(t) {
		c := layoutCase{Name: "fixture_" + strings.NewReplacer("/", "_", "-", "_").Replace(entry.Repo), Files: map[string]*string{}}
		for p, b := range sourceFiles(t, entry.Repo) {
			if p == "README.md" {
				s := string(b)
				c.Files[p] = &s
				continue
			}
			c.Files[p] = nil
		}
		out = append(out, c)
	}
	return out
}

type resolvedConfig struct {
	Name    string              `json:"name"`
	Default bool                `json:"default"`
	Builder string              `json:"builder"`
	Splits  []string            `json:"splits"`
	Files   map[string][]string `json:"files"`
}

type resolution struct {
	Configs []resolvedConfig `json:"configs,omitempty"`
	Error   string           `json:"error,omitempty"`
	Message string           `json:"message,omitempty"`
}

// errorKinds maps the library's exception classes to Hangar's errors.
var errorKinds = map[string]error{
	"DataFilesNotFoundError": layout.ErrEmptyDataset,
	"EmptyDatasetError":      layout.ErrEmptyDataset,
	"FileNotFoundError":      layout.ErrMissingFile,
	"ValueError":             layout.ErrMixedFormats,
}

func hangarResolve(c layoutCase) (resolution, error) {
	var files []layout.File
	var readmeText []byte
	for p := range c.Files {
		if p == "README.md" {
			readmeText = []byte(*c.Files[p])
		}
		files = append(files, layout.File{Path: p, Size: 1})
	}
	for p, entries := range c.Archives {
		files = append(files, layout.File{Path: p, Size: 1, Entries: func(fn func(string) error) error {
			for _, e := range entries {
				if err := fn(e); err != nil {
					return err
				}
			}
			return nil
		}})
	}
	slices.SortFunc(files, func(a, b layout.File) int { return strings.Compare(a.Path, b.Path) })
	var cd *card.Card
	if readmeText != nil {
		var err error
		if cd, err = card.Parse(readmeText, nil); err != nil {
			return resolution{}, err
		}
	}
	res, err := layout.Resolve(files, cd)
	if err != nil {
		return resolution{}, err
	}
	var out resolution
	for _, cfg := range res.Configs {
		rc := resolvedConfig{Name: cfg.Name, Default: cfg.Default, Builder: cfg.Builder, Splits: cfg.Splits(), Files: map[string][]string{}}
		for _, sp := range rc.Splits {
			rc.Files[sp] = cfg.Files[sp]
		}
		out.Configs = append(out.Configs, rc)
	}
	return out, nil
}

func TestLayoutMatchesDatasetsLibrary(t *testing.T) {
	cases := append(layoutCases(), fixtureCases(t)...)
	work := t.TempDir()
	in := filepath.Join(work, "cases.json")
	raw, err := json.Marshal(cases)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(in, raw, 0o600))
	cmd := exec.Command("uv", "run", "--no-project", "--quiet",
		"--with", "datasets==5.1.0", "--with", "pandas==2.3.3", "--with", "pyarrow==24.0.0",
		"python", "-I", filepath.Join(here(), "oracle", "layout_cases.py"), in, filepath.Join(work, "repos"))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	require.NoError(t, err, "run the datasets library: %s", stderr.String())
	var want map[string]resolution
	require.NoError(t, json.Unmarshal(out, &want))

	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			w, ok := want[c.Name]
			require.True(t, ok)
			t.Logf("datasets: %+v", w)
			got, err := hangarResolve(c)
			if w.Error != "" {
				require.Error(t, err, "the datasets library failed with %s: %s", w.Error, w.Message)
				if kind, ok := errorKinds[w.Error]; ok {
					require.True(t, errors.Is(err, kind), "the datasets library failed with %s (%s); Hangar with %v", w.Error, w.Message, err)
				}
				return
			}
			require.NoError(t, err)
			require.Equal(t, w.Configs, got.Configs)
		})
	}
}
