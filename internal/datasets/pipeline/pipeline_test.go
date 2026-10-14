package pipeline_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/blob"
	"github.com/jamesagarside/hangar/internal/blob/blobtest"
	"github.com/jamesagarside/hangar/internal/datasets/card"
	"github.com/jamesagarside/hangar/internal/datasets/datasetstest"
	"github.com/jamesagarside/hangar/internal/datasets/features"
	"github.com/jamesagarside/hangar/internal/datasets/layout"
	"github.com/jamesagarside/hangar/internal/datasets/pipeline"
	"github.com/jamesagarside/hangar/internal/datasets/pq"
	"github.com/jamesagarside/hangar/internal/db"
	"github.com/jamesagarside/hangar/internal/db/dbtest"
	"github.com/jamesagarside/hangar/internal/jobs"
	"github.com/jamesagarside/hangar/internal/jobs/jobstest"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/repo"
	"github.com/jamesagarside/hangar/internal/repo/repotest"
)

type env struct {
	ctx   context.Context
	repos *repotest.Fake
	db    *db.DB
	q     *jobstest.Fake
	p     *pipeline.Pipeline
	log   *syncBuffer
}

// syncBuffer is the pipeline's log output.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func newEnv(t *testing.T, d *db.DB, maxBytes int64) *env {
	return newEnvWith(t, d, maxBytes, nil)
}

// newEnvWith is newEnv with the pipeline's Repo service wrapped by wrap.
func newEnvWith(t *testing.T, d *db.DB, maxBytes int64, wrap func(*repotest.Fake) repo.Service) *env {
	reg := jobs.NewRegistry()
	q := jobstest.NewFake(reg)
	repos := repotest.NewFake(t)
	var svc repo.Service = repos
	if wrap != nil {
		svc = wrap(repos)
	}
	log := &syncBuffer{}
	p, err := pipeline.New(pipeline.Deps{DB: d, Blobs: blobtest.NewFake(), Repos: svc, LFS: repos.LFS, Jobs: q,
		MaxConvertBytes: maxBytes, TempDir: t.TempDir(), Log: slog.New(slog.NewTextHandler(log, nil))})
	require.NoError(t, err)
	require.NoError(t, p.Register(reg))
	return &env{ctx: context.Background(), repos: repos, db: d, q: q, p: p, log: log}
}

func (e *env) dataset(t *testing.T, name string, files map[string][]byte) (*repo.Repo, model.CommitSHA) {
	id := model.RepoID{Type: model.RepoTypeDataset, Namespace: "ns", Name: name}
	sha := datasetstest.CreateDataset(t, e.repos, id, files)
	r, err := e.repos.Get(e.ctx, id)
	require.NoError(t, err)
	datasetstest.InsertRepoRow(t, e.db, r)
	return r, sha
}

func (e *env) run(t *testing.T, r *repo.Repo, sha model.CommitSHA) {
	_, err := e.p.EnqueueConvert(e.ctx, r.UUID, sha)
	require.NoError(t, err)
	for e.q.RunPending(e.ctx) > 0 {
	}
}

const demoCSV = "id,review,star\n1,good,5\n2,bad,1\n3,fine,3\n"

func TestConvertAndStats(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		e := newEnv(t, d, 0)
		r, sha := e.dataset(t, "demo", map[string][]byte{
			"README.md":      []byte("---\nlicense: mit\n---\n"),
			"data/train.csv": []byte(demoCSV),
			"data/test.csv":  []byte("id,review,star\n4,ok,4\n"),
		})
		e.run(t, r, sha)
		store := e.p.Store()
		rows, err := store.Splits(e.ctx, r.UUID, sha)
		require.NoError(t, err)
		require.Len(t, rows, 2)
		require.Equal(t, "train", rows[0].Split)
		require.Equal(t, pipeline.StatusReady, rows[0].Status)
		require.EqualValues(t, 3, rows[0].NumRows)
		require.EqualValues(t, len(demoCSV), rows[0].NumBytesSource)
		require.Positive(t, rows[0].NumBytesParquet)
		require.Equal(t, []string{"id", "review", "star"}, rows[0].Features.Names())

		info, err := store.Info(e.ctx, r.UUID, sha, "default")
		require.NoError(t, err)
		require.Equal(t, "csv", info.Builder)
		require.Equal(t, "default/train/0000.parquet", info.Splits[0].Shards[0].Path)
		require.Equal(t, rows[0].ConvertedCommit, info.ExportCommit)

		// The export is on refs/convert/parquet, made by hangar-bot from the
		// source Commit, with .gitattributes and the Parquet files only.
		tip, err := e.repos.Resolve(e.ctx, r.ID, pipeline.ExportRef)
		require.NoError(t, err)
		require.Equal(t, info.ExportCommit, tip)
		entries, err := e.repos.Tree(e.ctx, r.ID, tip, "", repo.TreeOpts{Recursive: true})
		require.NoError(t, err)
		var paths []string
		for _, en := range entries {
			if en.Type == model.EntryFile {
				paths = append(paths, en.Path)
			}
		}
		require.Equal(t, []string{".gitattributes", "default/test/0000.parquet", "default/train/0000.parquet"}, paths)
		ci, err := e.repos.GetCommit(e.ctx, r.ID, tip)
		require.NoError(t, err)
		require.Equal(t, pipeline.CommitMessage, ci.Title)
		require.Contains(t, []string{model.SystemUsername, model.System().DisplayName}, ci.Author.Name)
		rc, _, err := e.repos.OpenFile(e.ctx, r.ID, tip, "default/train/0000.parquet", nil)
		require.NoError(t, err)
		raw, err := io.ReadAll(rc)
		require.NoError(t, err)
		_ = rc.Close()
		pf, err := pq.Open(bytes.NewReader(raw), int64(len(raw)))
		require.NoError(t, err)
		require.True(t, pf.HasFeatures)
		require.EqualValues(t, 3, pf.NumRows())

		doc, err := store.Stats(e.ctx, r.UUID, sha, "default", "train")
		require.NoError(t, err)
		require.EqualValues(t, 3, doc.NumExamples)
		require.Len(t, doc.Statistics, 3)
		require.Contains(t, string(doc.Statistics[2]), `"column_name":"star"`)

		// A new Commit replaces the export and the old rows.
		sha2 := datasetstest.CommitFiles(t, e.repos, r.ID, "main", map[string][]byte{"data/test.csv": nil})
		e.run(t, r, sha2)
		rows, err = store.Splits(e.ctx, r.UUID, sha2)
		require.NoError(t, err)
		require.Len(t, rows, 1)
		old, err := store.Splits(e.ctx, r.UUID, sha)
		require.NoError(t, err)
		require.Empty(t, old)
		tip2, err := e.repos.Resolve(e.ctx, r.ID, pipeline.ExportRef)
		require.NoError(t, err)
		require.NotEqual(t, tip, tip2)
		_, err = e.repos.Entry(e.ctx, r.ID, tip2, "default/test/0000.parquet")
		require.ErrorIs(t, err, repo.ErrEntryNotFound)

		// Converting the same Commit again does nothing.
		e.run(t, r, sha2)
		tip3, err := e.repos.Resolve(e.ctx, r.ID, pipeline.ExportRef)
		require.NoError(t, err)
		require.Equal(t, tip2, tip3)
	})
}

func TestParquetIsCopied(t *testing.T) {
	d := dbtest.SQLite(t)
	e := newEnv(t, d, 0)
	fs := features.Features{{Name: "x", Feature: features.Value("int64")}}
	st, _ := fs.StorageSchema()
	var buf bytes.Buffer
	w, err := pq.NewWriter(&buf, st, pq.WriterOptions{Features: fs})
	require.NoError(t, err)
	require.NoError(t, w.Write([][]any{{int64(1)}, {int64(2)}}))
	require.NoError(t, w.Close())
	r, sha := e.dataset(t, "pq", map[string][]byte{"train.parquet": buf.Bytes()})
	e.run(t, r, sha)
	info, err := e.p.Store().Info(e.ctx, r.UUID, sha, "default")
	require.NoError(t, err)
	require.True(t, info.Copied)
	entry, err := e.repos.Entry(e.ctx, r.ID, sha, "train.parquet")
	require.NoError(t, err)
	require.Equal(t, entry.LFS.OID, info.Splits[0].Shards[0].OID)
	require.EqualValues(t, 2, info.Splits[0].NumExamples)
}

func TestFailures(t *testing.T) {
	d := dbtest.SQLite(t)
	e := newEnv(t, d, 0)
	r, sha := e.dataset(t, "empty", map[string][]byte{"README.md": []byte("# nothing\n")})
	e.run(t, r, sha)
	rows, err := e.p.Store().Splits(e.ctx, r.UUID, sha)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, pipeline.StatusFailed, rows[0].Status)
	require.Equal(t, pipeline.DatasetLevel, rows[0].Config)
	require.Equal(t, pipeline.FailureEmpty, rows[0].Error)
	require.Contains(t, e.log.String(), "no data files")

	// The error behind a failure is logged; only its code is stored, so
	// no error text reaches the Viewer or the API.
	r2, sha2 := e.dataset(t, "badcsv", map[string][]byte{"train.csv": []byte("a,b\n1,2,3\n")})
	e.run(t, r2, sha2)
	rows, err = e.p.Store().Splits(e.ctx, r2.UUID, sha2)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, pipeline.StatusFailed, rows[0].Status)
	require.Equal(t, pipeline.FailureGeneration, rows[0].Error)
	require.Contains(t, e.log.String(), "expected 2 fields")
	_, err = e.repos.Resolve(e.ctx, r2.ID, pipeline.ExportRef)
	require.ErrorIs(t, err, repo.ErrRevisionNotFound)
}

// failingCard fails to read README.md, as a Blob store outage would.
type failingCard struct {
	repo.Service
	fail bool
}

func (f *failingCard) OpenFile(ctx context.Context, id model.RepoID, commit model.CommitSHA, p string, rng *blob.Range) (io.ReadCloser, model.Entry, error) {
	if f.fail && p == "README.md" {
		return nil, model.Entry{}, errors.New("blob: connection refused to 10.0.0.7:9000")
	}
	return f.Service.OpenFile(ctx, id, commit, p, rng)
}

// TestReadFailureIsNotAFailedDataset checks that an error reading a
// dataset's files fails the Job, which retries it, rather than marking the
// dataset as failed for good with the error's text.
func TestReadFailureIsNotAFailedDataset(t *testing.T) {
	wrapper := &failingCard{fail: true}
	e := newEnvWith(t, dbtest.SQLite(t), 0, func(f *repotest.Fake) repo.Service {
		wrapper.Service = f
		return wrapper
	})
	r, sha := e.dataset(t, "flaky", map[string][]byte{"README.md": []byte("# Flaky\n"), "train.csv": []byte(demoCSV)})
	payload, err := json.Marshal(pipeline.Payload{Repo: r.UUID, Commit: sha})
	require.NoError(t, err)
	job := &jobs.Job{Kind: jobs.KindDatasetConvert, Payload: payload}
	err = e.p.HandleConvert(e.ctx, job, nil)
	require.ErrorContains(t, err, "connection refused")
	require.False(t, jobs.IsPermanent(err))
	rows, err := e.p.Store().Splits(e.ctx, r.UUID, sha)
	require.NoError(t, err)
	for _, row := range rows {
		require.NotEqual(t, pipeline.StatusFailed, row.Status, "%+v", row)
	}
	wrapper.fail = false
	require.NoError(t, e.p.HandleConvert(e.ctx, job, nil))
	rows, err = e.p.Store().Splits(e.ctx, r.UUID, sha)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, pipeline.StatusReady, rows[0].Status)
}

func TestFailureCodes(t *testing.T) {
	for code, msg := range map[string]string{
		pipeline.FailureGeneration:     "The dataset generation failed",
		pipeline.FailureEmpty:          "The dataset is empty.",
		pipeline.FailureFormatMismatch: "Couldn't infer the same data file format for all splits.",
		pipeline.FailureConfigNames:    "Cannot get the config names for the dataset.",
		pipeline.FailureStatistics:     "Unexpected error.",
	} {
		require.Equal(t, pipeline.Failure{Code: code, Message: msg}, pipeline.FailureOf(code))
	}
	// Rows from before codes were stored hold error text, which is never
	// shown.
	legacy := pipeline.FailureOf("split train: open /var/lib/hangar/tmp/x.parquet: no space left on device")
	require.Equal(t, pipeline.Failure{Code: pipeline.FailureGeneration, Message: "The dataset generation failed"}, legacy)

	for err, want := range map[error]string{
		fmt.Errorf("x: %w", layout.ErrEmptyDataset):   pipeline.FailureEmpty,
		fmt.Errorf("x: %w", layout.ErrMixedFormats):   pipeline.FailureFormatMismatch,
		fmt.Errorf("x: %w", layout.ErrMissingFile):    pipeline.FailureConfigNames,
		fmt.Errorf("x: %w", card.ErrInvalid):          pipeline.FailureConfigNames,
		fmt.Errorf("x: %w", layout.ErrInvalidArchive): pipeline.FailureConfigNames,
	} {
		got, ok := pipeline.LayoutFailure(err)
		require.True(t, ok, err)
		require.Equal(t, want, got, err)
	}
	_, ok := pipeline.LayoutFailure(errors.New("database is locked"))
	require.False(t, ok)
}

func TestPartialExport(t *testing.T) {
	d := dbtest.SQLite(t)
	e := newEnv(t, d, 1)
	var b strings.Builder
	b.WriteString("text\n")
	for i := 0; i < 25000; i++ {
		b.WriteString("line\n")
	}
	r, sha := e.dataset(t, "big", map[string][]byte{"train.csv": []byte(b.String())})
	e.run(t, r, sha)
	rows, err := e.p.Store().Splits(e.ctx, r.UUID, sha)
	require.NoError(t, err)
	require.Equal(t, pipeline.StatusPartial, rows[0].Status)
	require.EqualValues(t, 10000, rows[0].NumRows, "the first CSV chunk passes the limit")
	info, err := e.p.Store().Info(e.ctx, r.UUID, sha, "default")
	require.NoError(t, err)
	require.Equal(t, "default/partial-train/0000.parquet", info.Splits[0].Shards[0].Path)
	doc, err := e.p.Store().Stats(e.ctx, r.UUID, sha, "default", "train")
	require.NoError(t, err)
	require.True(t, doc.Partial)
}

func TestMirrorAdoptsUpstreamExport(t *testing.T) {
	d := dbtest.SQLite(t)
	e := newEnv(t, d, 0)
	r, sha := e.dataset(t, "mirror", map[string][]byte{"data/train.csv": []byte(demoCSV)})
	// Write an export first, as the Upstream would have, then make the
	// Repo a Mirror and convert a later Commit.
	e.run(t, r, sha)
	tip, err := e.repos.Resolve(e.ctx, r.ID, pipeline.ExportRef)
	require.NoError(t, err)
	require.NoError(t, e.repos.SetMirror(r.ID, true))
	r, err = e.repos.Get(e.ctx, r.ID)
	require.NoError(t, err)
	_, err = e.db.NewDelete().Model((*db.DatasetSplit)(nil)).Where("repo_id = ?", r.UUID).Exec(e.ctx)
	require.NoError(t, err)
	e.run(t, r, sha)
	rows, err := e.p.Store().Splits(e.ctx, r.UUID, sha)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, pipeline.StatusReady, rows[0].Status)
	require.Equal(t, tip, rows[0].ConvertedCommit)
	require.EqualValues(t, 3, rows[0].NumRows)
	after, err := e.repos.Resolve(e.ctx, r.ID, pipeline.ExportRef)
	require.NoError(t, err)
	require.Equal(t, tip, after, "a Mirror's export is used, not rewritten")
	info, err := e.p.Store().Info(e.ctx, r.UUID, sha, "default")
	require.NoError(t, err)
	b, _ := json.Marshal(info.Features)
	require.Contains(t, string(b), "review")
}
