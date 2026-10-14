package layout_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/datasets"
	"github.com/jamesagarside/hangar/internal/datasets/datasetstest"
	"github.com/jamesagarside/hangar/internal/datasets/layout"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/repo/repotest"
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
	require.Error(t, err)

	yamlCommit := datasetstest.CommitFiles(t, repos, id, "main", map[string][]byte{
		"README.md":         []byte("# no front matter\n"),
		".huggingface.yaml": []byte("configs:\n- config_name: only_test\n  data_files:\n  - split: test\n    path: data/test.csv\n"),
	})
	res, err = svc.Resolve(ctx, id, yamlCommit)
	require.NoError(t, err)
	require.Equal(t, "only_test", res.Configs[0].Name)
	require.True(t, res.Configs[0].Default)
}
