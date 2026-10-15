package main

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/datasets/features"
	"github.com/jamesagarside/hangar/internal/datasets/pipeline"
	"github.com/jamesagarside/hangar/internal/db/dbtest"
	"github.com/jamesagarside/hangar/internal/index/indexdb"
	"github.com/jamesagarside/hangar/internal/index/indextest"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/repo"
)

func TestDatasetSplitsGiveTheIndexSizesAndColumns(t *testing.T) {
	d := dbtest.SQLite(t)
	ctx := t.Context()
	repoID := uuid.New()
	indextest.InsertRepoRow(t, d, &repo.Repo{UUID: repoID, ID: model.RepoID{Type: model.RepoTypeDataset, Namespace: "alice", Name: "reviews"},
		Visibility: model.VisibilityPublic, DefaultBranch: "main", HeadRef: "refs/heads/main"})
	st := &pipeline.Store{DB: d}
	commit := model.CommitSHA("0123456789abcdef0123456789abcdef01234567")
	fs, err := features.ParseFeatures([]byte(`{"text":{"dtype":"string","_type":"Value"},"label":{"dtype":"int64","_type":"Value"}}`))
	require.NoError(t, err)
	require.NoError(t, st.ReplaceSplits(ctx, repoID, commit, []pipeline.SplitRow{
		{Config: pipeline.DatasetLevel, Split: pipeline.DatasetLevel, Status: pipeline.StatusFailed, Error: "an unreadable file"},
		{Config: "default", Split: "train", Status: pipeline.StatusReady, NumRows: 10, NumBytesSource: 100, NumBytesParquet: 50, Features: fs},
		{Config: "default", Split: "test", Status: pipeline.StatusPartial, NumRows: 4, NumBytesSource: 40, NumBytesParquet: 20, Features: fs},
		{Config: "default", Split: "validation", Status: pipeline.StatusPending},
	}))

	got, err := datasetSplits(st)(ctx, repoID, commit)
	require.NoError(t, err)
	require.Len(t, got, 3, "the dataset-level error row is not a Split")
	cols := got[0].Features
	got[0].Features, got[1].Features = nil, nil
	require.Equal(t, []indexdb.DatasetSplit{
		{Config: "default", Split: "train", Converted: true, Rows: 10, Bytes: 100, ParquetBytes: 50},
		{Config: "default", Split: "test", Converted: true, Partial: true, Rows: 4, Bytes: 40, ParquetBytes: 20},
		{Config: "default", Split: "validation"},
	}, got)
	require.Len(t, cols, 2)
	require.Equal(t, "text", cols[0].Name, "columns keep their order")
	require.Equal(t, "label", cols[1].Name)
	var typ map[string]any
	require.NoError(t, json.Unmarshal(cols[1].Type, &typ))
	require.Equal(t, "int64", typ["dtype"])
}
