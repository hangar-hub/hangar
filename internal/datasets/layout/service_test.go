package layout_test

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/datasets"
	"github.com/hangar-hub/hangar/internal/datasets/datasetstest"
	"github.com/hangar-hub/hangar/internal/datasets/layout"
	"github.com/hangar-hub/hangar/internal/model"
	"github.com/hangar-hub/hangar/internal/repo/repotest"
)

func TestServiceResolveAndFiles(t *testing.T) {
	ctx := context.Background()
	repos := repotest.NewFake(t)
	id := model.RepoID{Type: model.RepoTypeDataset, Namespace: "ns", Name: "reviews"}
	commit := datasetstest.CreateDataset(t, repos, id, map[string][]byte{
		"README.md":      []byte("---\nlicense: mit\n---\n# Reviews\n"),
		"data/train.csv": []byte("a,b\n1,x\n"),
		"data/test.csv":  []byte("a,b\n2,y\n"),
	})
	svc := &layout.Service{Repos: repos}
	res, err := svc.Resolve(ctx, id, commit)
	require.NoError(t, err)
	require.Equal(t, []string{"train", "test"}, res.Configs[0].Splits())

	files, err := svc.Files(ctx, res, datasets.Selection{Splits: []string{"test"}})
	require.NoError(t, err)
	require.Equal(t, []string{"README.md", "data/test.csv"}, files)
	files, err = svc.Files(ctx, res, datasets.Selection{Configs: []string{"default"}, ParquetOnly: true})
	require.NoError(t, err)
	require.Equal(t, []string{"default/partial-test/*.parquet", "default/partial-train/*.parquet", "default/test-part*/*.parquet",
		"default/test/*.parquet", "default/train-part*/*.parquet", "default/train/*.parquet"}, files)
	_, err = svc.Files(ctx, res, datasets.Selection{Configs: []string{"nope"}})
	require.ErrorIs(t, err, datasets.ErrNotFound)

	_, err = svc.Resolve(ctx, model.RepoID{Type: model.RepoTypeDataset, Namespace: "ns", Name: "missing"}, commit)
	require.ErrorIs(t, err, datasets.ErrNotFound)

	big := datasetstest.CommitFiles(t, repos, id, "main", map[string][]byte{"README.md": []byte(strings.Repeat("x", layout.MaxCardBytes+1))})
	_, err = svc.Resolve(ctx, id, big)
	require.ErrorIs(t, err, layout.ErrCardTooLarge)
	require.True(t, layout.IsLayoutError(err))

	yamlCommit := datasetstest.CommitFiles(t, repos, id, "main", map[string][]byte{
		"README.md":         []byte("# no front matter\n"),
		".huggingface.yaml": []byte("configs:\n- config_name: only_test\n  data_files:\n  - split: test\n    path: data/test.csv\n"),
	})
	res, err = svc.Resolve(ctx, id, yamlCommit)
	require.NoError(t, err)
	require.Equal(t, "only_test", res.Configs[0].Name)
	require.True(t, res.Configs[0].Default)
}

// TestServiceLooksInsideArchives resolves a Repo whose only data file is a
// zip archive: the Service reads the archive's directory from the Repo.
func TestServiceLooksInsideArchives(t *testing.T) {
	ctx := context.Background()
	repos := repotest.NewFake(t)
	id := model.RepoID{Type: model.RepoTypeDataset, Namespace: "ns", Name: "zipped"}
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for _, name := range []string{"a.jsonl", "b.jsonl", "notes.txt"} {
		_, err := w.Create(name)
		require.NoError(t, err)
	}
	require.NoError(t, w.Close())
	commit := datasetstest.CreateDataset(t, repos, id, map[string][]byte{"data/train.zip": buf.Bytes()})
	res, err := (&layout.Service{Repos: repos}).Resolve(ctx, id, commit)
	require.NoError(t, err)
	require.Equal(t, "json", res.Configs[0].Builder)
	require.Equal(t, []string{"data/train.zip"}, res.Configs[0].Files["train"])

	f := layout.OpenRepoFile(ctx, repos, id, commit, "data/train.zip", int64(buf.Len()))
	require.Equal(t, int64(buf.Len()), f.Size())
	got := make([]byte, buf.Len())
	n, err := f.ReadAt(got, 0)
	require.NoError(t, err)
	require.Equal(t, buf.Bytes(), got[:n])
	_, err = f.ReadAt(got, int64(buf.Len()))
	require.ErrorIs(t, err, io.EOF)
	_, err = f.ReadAt(got, -1)
	require.Error(t, err)
}
