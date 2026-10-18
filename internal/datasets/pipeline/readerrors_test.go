package pipeline_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/blob"
	"github.com/hangar-hub/hangar/internal/datasets/pipeline"
	"github.com/hangar-hub/hangar/internal/db"
	"github.com/hangar-hub/hangar/internal/db/dbtest"
	"github.com/hangar-hub/hangar/internal/jobs"
	"github.com/hangar-hub/hangar/internal/model"
	"github.com/hangar-hub/hangar/internal/repo"
	"github.com/hangar-hub/hangar/internal/repo/repotest"
)

// errOutage is what a Blob store outage looks like to the pipeline.
var errOutage = errors.New("blob: dial tcp 10.0.0.7:9000: connect: connection refused")

// flakyReads fails reads of the files match selects while fail is set: the
// open fails, or, with midRead, the read stops after the first byte.
type flakyReads struct {
	repo.Service
	fail    atomic.Bool
	match   func(path string) bool
	midRead bool
	err     error
}

func (f *flakyReads) OpenFile(ctx context.Context, id model.RepoID, commit model.CommitSHA, p string, rng *blob.Range) (io.ReadCloser, model.Entry, error) {
	if !f.fail.Load() || !f.match(p) {
		return f.Service.OpenFile(ctx, id, commit, p, rng)
	}
	if !f.midRead {
		return nil, model.Entry{}, f.err
	}
	rc, e, err := f.Service.OpenFile(ctx, id, commit, p, rng)
	if err != nil {
		return nil, e, err
	}
	return &brokenReader{ReadCloser: rc, err: f.err}, e, nil
}

// brokenReader returns one byte, then its error.
type brokenReader struct {
	io.ReadCloser
	err  error
	done bool
}

func (b *brokenReader) Read(p []byte) (int, error) {
	if b.done || len(p) == 0 {
		return 0, b.err
	}
	b.done = true
	n, err := b.ReadCloser.Read(p[:1])
	if err != nil && !errors.Is(err, io.EOF) {
		return n, err
	}
	return n, nil
}

func flakyEnv(t *testing.T, d *db.DB, f *flakyReads) *env {
	return newEnvWith(t, d, 0, func(fake *repotest.Fake) repo.Service {
		f.Service = fake
		return f
	})
}

func convertJob(t *testing.T, r *repo.Repo, sha model.CommitSHA, kind jobs.Kind) *jobs.Job {
	payload, err := json.Marshal(pipeline.Payload{Repo: r.UUID, Commit: sha})
	require.NoError(t, err)
	return &jobs.Job{Kind: kind, Payload: payload, Attempts: 1, MaxAttempts: 3}
}

func statsJobs(t *testing.T, e *env) []*jobs.Job {
	list, _, err := e.q.List(e.ctx, jobs.Filter{Kinds: []jobs.Kind{jobs.KindDatasetStats}})
	require.NoError(t, err)
	return list
}

// TestConvertRetriesReadErrors checks that an error reading a data file
// while a Config converts fails the Job, which retries, and leaves the
// Splits pending, rather than recording DatasetGenerationError for good.
func TestConvertRetriesReadErrors(t *testing.T) {
	for _, midRead := range []bool{false, true} {
		t.Run(fmt.Sprintf("midRead=%v", midRead), func(t *testing.T) {
			f := &flakyReads{match: func(p string) bool { return p == "data/train.csv" }, midRead: midRead, err: errOutage}
			e := flakyEnv(t, dbtest.SQLite(t), f)
			r, sha := e.dataset(t, "flaky", map[string][]byte{
				"README.md":      []byte("# Flaky\n"),
				"data/train.csv": []byte(demoCSV),
				"data/test.csv":  []byte("id,review,star\n4,ok,4\n"),
			})
			f.fail.Store(true)
			err := e.p.HandleConvert(e.ctx, convertJob(t, r, sha, jobs.KindDatasetConvert), nil)
			require.ErrorContains(t, err, "connection refused")
			require.False(t, jobs.IsPermanent(err))
			rows, err := e.p.Store().Splits(e.ctx, r.UUID, sha)
			require.NoError(t, err)
			require.Len(t, rows, 2)
			for _, row := range rows {
				require.Equal(t, pipeline.StatusPending, row.Status, "%+v", row)
				require.Empty(t, row.Error)
			}
			_, err = e.repos.Resolve(e.ctx, r.ID, pipeline.ExportRef)
			require.ErrorIs(t, err, repo.ErrRevisionNotFound, "nothing is exported from a conversion that could not read its files")
			require.Empty(t, statsJobs(t, e))

			f.fail.Store(false)
			require.NoError(t, e.p.HandleConvert(e.ctx, convertJob(t, r, sha, jobs.KindDatasetConvert), nil))
			rows, err = e.p.Store().Splits(e.ctx, r.UUID, sha)
			require.NoError(t, err)
			require.Len(t, rows, 2)
			for _, row := range rows {
				require.Equal(t, pipeline.StatusReady, row.Status, "%+v", row)
			}
		})
	}
}

// TestConvertReadErrorsOutlastingRetries runs the conversion through the
// queue: every attempt fails while the files cannot be read, and the
// Splits stay pending, so the Viewer can queue the conversion again.
func TestConvertReadErrorsOutlastingRetries(t *testing.T) {
	f := &flakyReads{match: func(p string) bool { return strings.HasSuffix(p, ".csv") }, err: errOutage}
	e := flakyEnv(t, dbtest.SQLite(t), f)
	now := time.Now()
	e.q.SetClock(func() time.Time { return now })
	r, sha := e.dataset(t, "outage", map[string][]byte{"train.csv": []byte(demoCSV)})
	f.fail.Store(true)
	id, err := e.p.EnqueueConvert(e.ctx, r.UUID, sha)
	require.NoError(t, err)
	for range 3 {
		require.Equal(t, 1, e.q.RunPending(e.ctx))
		now = now.Add(24 * time.Hour)
	}
	j, err := e.q.Get(e.ctx, id)
	require.NoError(t, err)
	require.Equal(t, jobs.StatusFailed, j.Status)
	require.Equal(t, 3, j.Attempts)
	rows, err := e.p.Store().Splits(e.ctx, r.UUID, sha)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, pipeline.StatusPending, rows[0].Status)

	f.fail.Store(false)
	e.run(t, r, sha)
	rows, err = e.p.Store().Splits(e.ctx, r.UUID, sha)
	require.NoError(t, err)
	require.Equal(t, pipeline.StatusReady, rows[0].Status)
	doc, err := e.p.Store().Stats(e.ctx, r.UUID, sha, "default", "train")
	require.NoError(t, err)
	require.Empty(t, doc.Error)
}

// TestConvertRecordsContentReadErrors checks that a read error that is a
// fact about the dataset, such as a file not in the arsenal or an object
// that fails its checksum, still fails the Config.
func TestConvertRecordsContentReadErrors(t *testing.T) {
	for _, cause := range []error{repo.ErrNotPresent, repo.ErrLFSObjectMissing, blob.ErrChecksumMismatch, blob.ErrNotFound} {
		t.Run(cause.Error(), func(t *testing.T) {
			f := &flakyReads{match: func(p string) bool { return p == "train.csv" }, err: fmt.Errorf("open train.csv: %w", cause)}
			e := flakyEnv(t, dbtest.SQLite(t), f)
			r, sha := e.dataset(t, "content", map[string][]byte{"train.csv": []byte(demoCSV)})
			f.fail.Store(true)
			require.NoError(t, e.p.HandleConvert(e.ctx, convertJob(t, r, sha, jobs.KindDatasetConvert), nil))
			rows, err := e.p.Store().Splits(e.ctx, r.UUID, sha)
			require.NoError(t, err)
			require.Len(t, rows, 1)
			require.Equal(t, pipeline.StatusFailed, rows[0].Status)
			require.Equal(t, pipeline.FailureGeneration, rows[0].Error)
		})
	}
}

// TestStatsRetriesReadErrors checks that an error reading the Parquet
// export fails the statistics Job, which retries, and stores nothing for
// that Split, while the other Splits get their statistics.
func TestStatsRetriesReadErrors(t *testing.T) {
	for _, midRead := range []bool{false, true} {
		t.Run(fmt.Sprintf("midRead=%v", midRead), func(t *testing.T) {
			f := &flakyReads{match: func(p string) bool { return p == "default/train/0000.parquet" }, midRead: midRead, err: errOutage}
			e := flakyEnv(t, dbtest.SQLite(t), f)
			r, sha := e.dataset(t, "stats", map[string][]byte{
				"data/train.csv": []byte(demoCSV),
				"data/test.csv":  []byte("id,review,star\n4,ok,4\n"),
			})
			require.NoError(t, e.p.HandleConvert(e.ctx, convertJob(t, r, sha, jobs.KindDatasetConvert), nil))

			f.fail.Store(true)
			err := e.p.HandleStats(e.ctx, convertJob(t, r, sha, jobs.KindDatasetStats), nil)
			require.ErrorContains(t, err, "connection refused")
			require.ErrorContains(t, err, "default/train")
			require.False(t, jobs.IsPermanent(err))
			_, err = e.p.Store().Stats(e.ctx, r.UUID, sha, "default", "train")
			require.ErrorIs(t, err, pipeline.ErrNoStats, "no UnexpectedError is stored for a read error")
			doc, err := e.p.Store().Stats(e.ctx, r.UUID, sha, "default", "test")
			require.NoError(t, err)
			require.Empty(t, doc.Error)
			require.EqualValues(t, 1, doc.NumExamples)

			f.fail.Store(false)
			require.NoError(t, e.p.HandleStats(e.ctx, convertJob(t, r, sha, jobs.KindDatasetStats), nil))
			doc, err = e.p.Store().Stats(e.ctx, r.UUID, sha, "default", "train")
			require.NoError(t, err)
			require.Empty(t, doc.Error)
			require.EqualValues(t, 3, doc.NumExamples)
		})
	}
}

// TestStatsRecordsContentFailures checks that a Split whose statistics
// cannot be computed from readable data still records UnexpectedError.
func TestStatsRecordsContentFailures(t *testing.T) {
	f := &flakyReads{match: func(p string) bool { return p == "default/train/0000.parquet" }, err: blob.ErrChecksumMismatch}
	e := flakyEnv(t, dbtest.SQLite(t), f)
	r, sha := e.dataset(t, "corrupt", map[string][]byte{"train.csv": []byte(demoCSV)})
	require.NoError(t, e.p.HandleConvert(e.ctx, convertJob(t, r, sha, jobs.KindDatasetConvert), nil))
	f.fail.Store(true)
	require.NoError(t, e.p.HandleStats(e.ctx, convertJob(t, r, sha, jobs.KindDatasetStats), nil))
	doc, err := e.p.Store().Stats(e.ctx, r.UUID, sha, "default", "train")
	require.NoError(t, err)
	require.Equal(t, pipeline.FailureStatistics, doc.Error)
}

// TestMirrorExportReadErrorsRetry checks that an error reading a Mirror's
// own Parquet export fails the Job rather than the Split.
func TestMirrorExportReadErrorsRetry(t *testing.T) {
	f := &flakyReads{match: func(p string) bool { return strings.HasSuffix(p, ".parquet") }, err: errOutage}
	e := flakyEnv(t, dbtest.SQLite(t), f)
	r, sha := e.dataset(t, "mirror", map[string][]byte{"data/train.csv": []byte(demoCSV)})
	e.run(t, r, sha)
	require.NoError(t, e.repos.SetMirror(r.ID, true))
	_, err := e.db.NewDelete().Model((*db.DatasetSplit)(nil)).Where("repo_id = ?", r.UUID).Exec(e.ctx)
	require.NoError(t, err)

	f.fail.Store(true)
	err = e.p.HandleConvert(e.ctx, convertJob(t, r, sha, jobs.KindDatasetConvert), nil)
	require.ErrorContains(t, err, "connection refused")
	require.False(t, jobs.IsPermanent(err))
	rows, err := e.p.Store().Splits(e.ctx, r.UUID, sha)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, pipeline.StatusPending, rows[0].Status)

	f.fail.Store(false)
	require.NoError(t, e.p.HandleConvert(e.ctx, convertJob(t, r, sha, jobs.KindDatasetConvert), nil))
	rows, err = e.p.Store().Splits(e.ctx, r.UUID, sha)
	require.NoError(t, err)
	require.Equal(t, pipeline.StatusReady, rows[0].Status)
}
