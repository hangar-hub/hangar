package pipeline_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/blob/blobtest"
	"github.com/hangar-hub/hangar/internal/datasets/datasetstest"
	"github.com/hangar-hub/hangar/internal/datasets/pipeline"
	"github.com/hangar-hub/hangar/internal/db/dbtest"
	"github.com/hangar-hub/hangar/internal/jobs"
	"github.com/hangar-hub/hangar/internal/jobs/jobstest"
	"github.com/hangar-hub/hangar/internal/model"
	"github.com/hangar-hub/hangar/internal/repo"
	"github.com/hangar-hub/hangar/internal/repo/repotest"
)

// TestAfterConvertRunsOncePerExport checks the AfterConvert hook: it runs
// after a new export is stored, not when the export already exists (so
// an index that queues a conversion again cannot loop), and a failing hook
// leaves the export done.
func TestAfterConvertRunsOncePerExport(t *testing.T) {
	d := dbtest.SQLite(t)
	ctx := context.Background()
	reg := jobs.NewRegistry()
	q := jobstest.NewFake(reg)
	repos := repotest.NewFake(t)
	var calls []model.CommitSHA
	var hookErr error
	var log bytes.Buffer
	p, err := pipeline.New(pipeline.Deps{DB: d, Blobs: blobtest.NewFake(), Repos: repos, LFS: repos.LFS, Jobs: q, TempDir: t.TempDir(),
		Log: slog.New(slog.NewTextHandler(&log, nil)),
		AfterConvert: func(_ context.Context, r *repo.Repo, commit model.CommitSHA) error {
			require.Equal(t, model.RepoTypeDataset, r.ID.Type)
			calls = append(calls, commit)
			return hookErr
		}})
	require.NoError(t, err)
	require.NoError(t, p.Register(reg))
	run := func(r *repo.Repo, sha model.CommitSHA) {
		t.Helper()
		_, err := p.EnqueueConvert(ctx, r.UUID, sha)
		require.NoError(t, err)
		for q.RunPending(ctx) > 0 {
		}
	}

	id := model.RepoID{Type: model.RepoTypeDataset, Namespace: "ns", Name: "after"}
	sha := datasetstest.CreateDataset(t, repos, id, map[string][]byte{"data/train.csv": []byte("a,b\n1,2\n")})
	r, err := repos.Get(ctx, id)
	require.NoError(t, err)
	datasetstest.InsertRepoRow(t, d, r)
	run(r, sha)
	require.Equal(t, []model.CommitSHA{sha}, calls)
	run(r, sha)
	require.Len(t, calls, 1, "an export that exists does not run the hook again")

	hookErr = errors.New("the queue is full")
	other := model.RepoID{Type: model.RepoTypeDataset, Namespace: "ns", Name: "failing"}
	sha2 := datasetstest.CreateDataset(t, repos, other, map[string][]byte{"data/train.csv": []byte("a\n1\n")})
	r2, err := repos.Get(ctx, other)
	require.NoError(t, err)
	datasetstest.InsertRepoRow(t, d, r2)
	run(r2, sha2)
	require.Len(t, calls, 2)
	splits, err := p.Store().Splits(ctx, r2.UUID, sha2)
	require.NoError(t, err)
	require.NotEmpty(t, splits, "the export is kept when the hook fails")
	require.Contains(t, log.String(), "the queue is full")
}
