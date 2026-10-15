package indexdb_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/db/dbtest"
	"github.com/jamesagarside/hangar/internal/index/indexdb"
	"github.com/jamesagarside/hangar/internal/index/indextest"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/repo/repotest"
)

// TestDatasetRowsReplaceTheCardSizeCategory checks DatasetRows: once a
// dataset's Parquet export exists its rows are in the index, and the size
// category from them replaces the card's, tag included (DATA-9).
func TestDatasetRowsReplaceTheCardSizeCategory(t *testing.T) {
	d := dbtest.SQLite(t)
	ctx := context.Background()
	repos := repotest.NewFake(t)
	id := model.RepoID{Type: model.RepoTypeDataset, Namespace: "alice", Name: "rows"}
	r := indextest.CreateRepo(t, repos, id, model.VisibilityPublic)
	indextest.InsertRepoRow(t, d, r)
	sha := indextest.Commit(t, repos, id, map[string][]byte{
		"README.md":      []byte("---\nsize_categories:\n- n<1K\n---\n# Rows\n"),
		"data/train.csv": []byte("a\n1\n"),
	})
	var rows int64
	var category string
	var ok bool
	var rowsErr error
	svc := indexdb.New(indexdb.Options{DB: d, Repos: repos,
		DatasetRows: func(_ context.Context, repoUUID uuid.UUID, got model.RepoID, commit model.CommitSHA) (int64, string, bool, error) {
			require.Equal(t, r.UUID, repoUUID)
			require.Equal(t, id, got)
			require.Equal(t, sha, commit)
			return rows, category, ok, rowsErr
		}})

	sum, err := svc.Index(ctx, r.UUID, id, sha)
	require.NoError(t, err)
	require.Equal(t, "n<1K", sum.Dataset.SizeCategory, "before the export, the card's size category")
	require.Zero(t, sum.Dataset.Rows)

	rows, category, ok = 12_345, "10K<n<100K", true
	sum, err = svc.Index(ctx, r.UUID, id, sha)
	require.NoError(t, err)
	require.Equal(t, int64(12_345), sum.Dataset.Rows)
	require.Equal(t, "10K<n<100K", sum.Dataset.SizeCategory)
	require.Contains(t, sum.Tags, "size_categories:10K<n<100K")
	require.False(t, slices.Contains(sum.Tags, "size_categories:n<1K"), "the card's tag is replaced")

	rowsErr = errors.New("the export store is unavailable")
	sum, err = svc.Index(ctx, r.UUID, id, sha)
	require.NoError(t, err, "a failed row lookup does not fail the index")
	require.Equal(t, "n<1K", sum.Dataset.SizeCategory)
}
