package datasetstest_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/datasets"
	"github.com/jamesagarside/hangar/internal/datasets/datasetstest"
	"github.com/jamesagarside/hangar/internal/model"
)

var (
	ds     = model.RepoID{Type: model.RepoTypeDataset, Namespace: "ns", Name: "reviews"}
	commit = model.CommitSHA("0123456789012345678901234567890123456789")
	anon   = model.Anonymous()
)

func TestLayoutFiles(t *testing.T) {
	ctx := context.Background()
	l := datasetstest.NewLayout()
	resolved := &datasets.Resolved{FromCard: true, Configs: []datasets.Config{
		{Name: "default", Default: true, DataFiles: map[string][]string{"train": {"data/train-*.parquet"}, "test": {"data/test-*.parquet"}}},
		{Name: "fr", DataFiles: map[string][]string{"train": {"fr/train.csv"}}},
	}}
	l.Set(ds, commit, resolved)
	got, err := l.Resolve(ctx, ds, commit)
	require.NoError(t, err)
	require.Len(t, got.Configs, 2)
	_, err = l.Resolve(ctx, ds, "other")
	require.ErrorIs(t, err, datasets.ErrNotFound)

	files, err := l.Files(ctx, got, datasets.Selection{Configs: []string{"default"}, Splits: []string{"test"}})
	require.NoError(t, err)
	require.Equal(t, []string{"data/test-*.parquet"}, files)
	files, err = l.Files(ctx, got, datasets.Selection{Splits: []string{"train"}, ParquetOnly: true})
	require.NoError(t, err)
	require.Equal(t, []string{"default/partial-train/*.parquet", "default/train/*.parquet", "fr/partial-train/*.parquet", "fr/train/*.parquet"}, files)
	all, err := l.Files(ctx, got, datasets.Selection{})
	require.NoError(t, err)
	require.Len(t, all, 3)
	_, err = l.Files(ctx, got, datasets.Selection{Configs: []string{"de"}})
	require.ErrorIs(t, err, datasets.ErrNotFound)
}

func TestViewer(t *testing.T) {
	ctx := context.Background()
	v := datasetstest.NewViewer()
	rows := []map[string]any{
		{"text": "Great film", "label": "pos"},
		{"text": "Awful plot", "label": "neg"},
		{"text": "great acting, awful plot", "label": "neg"},
	}
	v.SetTable(ds, "default", "train", &datasetstest.Table{Columns: []string{"text", "label"}, Types: []string{"", `{"dtype":"string","_type":"Value"}`}, Rows: rows, Ready: true})
	v.SetTable(ds, "default", "test", &datasetstest.Table{Columns: []string{"text"}, Ready: false})
	v.SetParquetFiles(ds, datasets.ParquetFile{Dataset: ds.String(), Config: "default", Split: "train", Filename: "0000.parquet", Size: 10})

	valid, err := v.IsValid(ctx, anon, ds)
	require.NoError(t, err)
	require.True(t, valid.Viewer)
	splits, err := v.Splits(ctx, anon, ds)
	require.NoError(t, err)
	require.Len(t, splits.Splits, 2)
	require.Len(t, splits.Pending, 1)

	first, err := v.FirstRows(ctx, anon, ds, "default", "train")
	require.NoError(t, err)
	require.Len(t, first.Rows, 3)
	require.Equal(t, int64(3), first.NumRowsTotal)
	require.Equal(t, "label", first.Features[1].Name)

	page, err := v.Rows(ctx, anon, datasets.RowsRequest{Repo: ds, Config: "default", Split: "train", Offset: 1, Length: 1})
	require.NoError(t, err)
	require.Len(t, page.Rows, 1)
	require.Equal(t, int64(1), page.Rows[0].RowIdx)

	found, err := v.Search(ctx, anon, datasets.SearchRequest{RowsRequest: datasets.RowsRequest{Repo: ds, Config: "default", Split: "train"}, Query: "GREAT awful"})
	require.NoError(t, err)
	require.Len(t, found.Rows, 1)
	require.Equal(t, int64(2), found.Rows[0].RowIdx)

	filtered, err := v.Filter(ctx, anon, datasets.FilterRequest{RowsRequest: datasets.RowsRequest{Repo: ds, Config: "default", Split: "train"}, Where: `"label" = 'neg'`, OrderBy: `"text" DESC`})
	require.NoError(t, err)
	require.Len(t, filtered.Rows, 2)
	require.Equal(t, int64(2), filtered.Rows[0].RowIdx, "lower-case g sorts after upper-case A")
	_, err = v.Filter(ctx, anon, datasets.FilterRequest{RowsRequest: datasets.RowsRequest{Repo: ds, Config: "default", Split: "train"}, Where: `label > 1`})
	require.ErrorIs(t, err, datasets.ErrInvalidQuery)
	_, err = v.Filter(ctx, anon, datasets.FilterRequest{RowsRequest: datasets.RowsRequest{Repo: ds, Config: "default", Split: "train"}, OrderBy: `a, b`})
	require.ErrorIs(t, err, datasets.ErrInvalidQuery)

	_, err = v.Rows(ctx, anon, datasets.RowsRequest{Repo: ds, Config: "default", Split: "test"})
	require.ErrorIs(t, err, datasets.ErrNotReady)
	_, err = v.Rows(ctx, anon, datasets.RowsRequest{Repo: ds, Config: "x", Split: "y"})
	require.ErrorIs(t, err, datasets.ErrNotFound)

	pq, err := v.Parquet(ctx, anon, ds, "default")
	require.NoError(t, err)
	require.Len(t, pq.ParquetFiles, 1)
	info, err := v.Info(ctx, anon, ds, "")
	require.NoError(t, err)
	require.Contains(t, string(info), `"num_examples":3`)
	size, err := v.Size(ctx, anon, ds, "default")
	require.NoError(t, err)
	require.Contains(t, string(size), "configs")
	stats, err := v.Statistics(ctx, anon, ds, "default", "train")
	require.NoError(t, err)
	require.Contains(t, string(stats), `"num_examples":3`)
	cr, err := v.Croissant(ctx, anon, ds)
	require.NoError(t, err)
	require.Contains(t, string(cr), "croissant")

	v.Readable = func(p model.Principal, _ model.RepoID) bool { return !p.IsAnonymous() }
	_, err = v.Splits(ctx, anon, ds)
	require.ErrorIs(t, err, datasets.ErrNotFound, "private datasets are 404")
	_, err = v.Splits(ctx, model.Principal{Kind: model.PrincipalUser}, model.RepoID{Type: model.RepoTypeDataset, Namespace: "no", Name: "ne"})
	require.ErrorIs(t, err, datasets.ErrNotFound)
}
