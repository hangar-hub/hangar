package indexdb_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	"github.com/hangar-hub/hangar/internal/blob"
	"github.com/hangar-hub/hangar/internal/db"
	"github.com/hangar-hub/hangar/internal/db/dbtest"
	"github.com/hangar-hub/hangar/internal/index"
	"github.com/hangar-hub/hangar/internal/index/indexdb"
	"github.com/hangar-hub/hangar/internal/index/indextest"
	"github.com/hangar-hub/hangar/internal/index/safetensors"
	"github.com/hangar-hub/hangar/internal/jobs"
	"github.com/hangar-hub/hangar/internal/jobs/jobstest"
	"github.com/hangar-hub/hangar/internal/model"
	"github.com/hangar-hub/hangar/internal/repo"
	"github.com/hangar-hub/hangar/internal/repo/repotest"
	"github.com/hangar-hub/hangar/internal/telemetry"
	"github.com/hangar-hub/hangar/internal/webhook"
	"github.com/hangar-hub/hangar/internal/webhook/webhooktest"
)

type env struct {
	db    *db.DB
	repos *repotest.Fake
	hooks *webhooktest.Fake
	svc   *indexdb.Service
	repo  *repo.Repo
	id    model.RepoID
	sha   model.CommitSHA
	after []index.CommitJob
}

func newEnv(t *testing.T, d *db.DB) *env {
	t.Helper()
	e := &env{db: d, repos: repotest.NewFake(t), hooks: &webhooktest.Fake{}}
	e.id = model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "tiny"}
	e.repo = indextest.CreateRepo(t, e.repos, e.id, model.VisibilityPublic)
	indextest.InsertRepoRow(t, d, e.repo)
	e.sha = indextest.Commit(t, e.repos, e.id, map[string][]byte{
		"model.safetensors": indextest.Safetensors(t, nil, st("w", "F32", 4, 4)),
		"tiny-Q4_0.gguf":    indextest.LlamaGGUF(t, 2, "Q4_0"),
		"tiny-Q8_0.gguf":    indextest.LlamaGGUF(t, 7, "Q8_0"),
		"bad.safetensors":   []byte("junk"),
		"dir/notes.txt":     []byte("hello"),
	})
	reader := sdkmetric.NewManualReader()
	inst, err := telemetry.NewInstruments(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("test"))
	require.NoError(t, err)
	e.svc = indexdb.New(indexdb.Options{DB: d, Repos: e.repos, Webhooks: e.hooks, Instruments: inst,
		AfterIndex: func(_ context.Context, j index.CommitJob, _ *index.Summary) error {
			e.after = append(e.after, j)
			return nil
		}})
	return e
}

func TestGetBeforeIndexing(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		e := newEnv(t, d)
		_, err := e.svc.Get(t.Context(), e.repo.UUID, e.sha)
		require.ErrorIs(t, err, index.ErrNotIndexed)
		_, err = e.svc.IndexedAt(t.Context(), e.repo.UUID, e.sha)
		require.ErrorIs(t, err, index.ErrNotIndexed)
	})
}

func TestFileHeaderAndQuants(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		e := newEnv(t, d)
		ctx := t.Context()
		h, err := e.svc.FileHeader(ctx, e.id, e.sha, "model.safetensors")
		require.NoError(t, err)
		var st map[string]any
		require.NoError(t, json.Unmarshal(h, &st))
		require.Contains(t, st, "w")

		h, err = e.svc.FileHeader(ctx, e.id, e.sha, "tiny-Q4_0.gguf")
		require.NoError(t, err)
		var view indexdb.HeaderView
		require.NoError(t, json.Unmarshal(h, &view))
		require.Equal(t, uint32(3), view.Version)
		require.Equal(t, "Q4_0", view.Tensors[0].Type)
		require.Equal(t, "llama", view.Metadata["general.architecture"], "metadata values are plain values")
		for k, v := range view.Metadata {
			if m, ok := v.(map[string]any); ok {
				require.Equal(t, "array", m["type"], k)
				require.NotNil(t, m["length"], k)
			}
		}

		_, err = e.svc.FileHeader(ctx, e.id, e.sha, "dir/notes.txt")
		require.ErrorIs(t, err, index.ErrNoHeader)
		_, err = e.svc.FileHeader(ctx, e.id, e.sha, "bad.safetensors")
		require.ErrorIs(t, err, index.ErrNoHeader)
		_, err = e.svc.FileHeader(ctx, e.id, e.sha, "missing.gguf")
		require.ErrorIs(t, err, repo.ErrEntryNotFound)

		entry, err := e.repos.Entry(ctx, e.id, e.sha, "tiny-Q8_0.gguf")
		require.NoError(t, err)
		require.NoError(t, e.repos.SetLFSPresence(ctx, e.id, []repo.LFSRef{{OID: entry.LFS.OID, Size: entry.Size, Present: false}}))
		_, err = e.svc.FileHeader(ctx, e.id, e.sha, "tiny-Q8_0.gguf")
		require.ErrorIs(t, err, repo.ErrNotPresent)

		groups, err := e.svc.Quants(ctx, e.id, e.sha)
		require.NoError(t, err)
		require.Len(t, groups, 2)
		require.Equal(t, "Q4_0", groups[0].Name)
	})
}

func TestHandleJobIndexesAndEmits(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		e := newEnv(t, d)
		ctx := t.Context()
		reg := jobs.NewRegistry()
		require.NoError(t, e.svc.Register(reg))
		q := jobstest.NewFake(reg)
		job := index.CommitJob{RepoUUID: e.repo.UUID, Repo: "old/name", Type: model.RepoTypeModel, Commit: e.sha}
		require.Equal(t, e.repo.UUID.String()+":"+string(e.sha), job.DedupeKey())
		id, err := q.Enqueue(ctx, jobs.NewJob{Kind: jobs.KindIndex, DedupeKey: job.DedupeKey(), Payload: job})
		require.NoError(t, err)
		q.RunPending(ctx)
		got, err := q.Get(ctx, id)
		require.NoError(t, err)
		require.Equal(t, jobs.StatusSucceeded, got.Status, got.Error)

		sum, err := e.svc.Get(ctx, e.repo.UUID, e.sha)
		require.NoError(t, err)
		require.Equal(t, []string{"gguf", "safetensors"}, sum.Formats)
		at, err := e.svc.IndexedAt(ctx, e.repo.UUID, e.sha)
		require.NoError(t, err)
		require.WithinDuration(t, time.Now(), at, time.Minute)

		require.Equal(t, []webhook.EventName{webhook.IndexCompleted}, e.hooks.Names())
		ev := e.hooks.Events()[0]
		require.Equal(t, "alice/tiny", ev.Repo.ID)
		require.Equal(t, string(e.sha), ev.Data["commit"])
		require.Len(t, e.after, 1)
		require.Equal(t, "alice/tiny", e.after[0].Repo, "the hook gets the current Repo ID")

		// Indexing again replaces the stored row.
		_, err = e.svc.Index(ctx, e.repo.UUID, e.id, e.sha)
		require.NoError(t, err)
		n, err := d.NewSelect().Model((*db.Metadata)(nil)).Where("repo_id = ?", e.repo.UUID).Count(ctx)
		require.NoError(t, err)
		require.EqualValues(t, 1, n)
	})
}

func TestHandleJobSkipsAndFails(t *testing.T) {
	d := dbtest.SQLite(t)
	e := newEnv(t, d)
	ctx := t.Context()
	run := func(payload any) error {
		raw, _ := json.Marshal(payload)
		return e.svc.HandleJob(ctx, &jobs.Job{Kind: jobs.KindIndex, Payload: raw}, nil)
	}
	require.NoError(t, run(index.CommitJob{RepoUUID: uuid.New(), Commit: e.sha}), "unknown Repos are skipped")
	err := run(index.CommitJob{RepoUUID: e.repo.UUID, Commit: "nope"})
	require.True(t, jobs.IsPermanent(err))
	err = run(index.CommitJob{RepoUUID: e.repo.UUID, Commit: "1234567890123456789012345678901234567890"})
	require.True(t, jobs.IsPermanent(err), "an unknown Commit fails permanently: %v", err)
	err = e.svc.HandleJob(ctx, &jobs.Job{Kind: jobs.KindIndex, Payload: []byte("{")}, nil)
	require.True(t, jobs.IsPermanent(err))

	failing := indexdb.New(indexdb.Options{DB: d, Repos: e.repos, AfterIndex: func(context.Context, index.CommitJob, *index.Summary) error {
		return errors.New("convert queue unavailable")
	}})
	raw, _ := json.Marshal(index.CommitJob{RepoUUID: e.repo.UUID, Commit: e.sha})
	require.ErrorContains(t, failing.HandleJob(ctx, &jobs.Job{Payload: raw}, nil), "convert queue")

	require.NoError(t, e.repos.Delete(ctx, e.id, indextest.Owner("alice")))
	require.NoError(t, run(index.CommitJob{RepoUUID: e.repo.UUID, Commit: e.sha}), "deleted Repos are skipped")
}

func TestReindexStaleQueuesBranchHeadsOnly(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		e := newEnv(t, d)
		ctx := t.Context()
		old := e.sha
		head := indextest.Commit(t, e.repos, e.id, map[string][]byte{"README.md": []byte("# new")})
		for _, sha := range []model.CommitSHA{old, head} {
			_, err := d.NewInsert().Model(&db.Metadata{RepoID: e.repo.UUID, Commit: string(sha), IndexerVersion: index.Version - 1,
				Summary: json.RawMessage(`{"formats":[]}`), IndexedAt: db.Now()}).Exec(ctx)
			require.NoError(t, err)
		}
		q := jobstest.NewFake(jobs.NewRegistry())
		n, err := e.svc.ReindexStale(ctx, q)
		require.NoError(t, err)
		require.Equal(t, 1, n)
		list, _, err := q.List(ctx, jobs.Filter{})
		require.NoError(t, err)
		require.Len(t, list, 1)
		var p index.CommitJob
		require.NoError(t, list[0].DecodePayload(&p))
		require.Equal(t, head, p.Commit)
		require.Equal(t, jobs.PriorityLow, list[0].Priority)

		// Stale results stay readable until re-indexing finishes.
		sum, err := e.svc.Get(ctx, e.repo.UUID, head)
		require.NoError(t, err)
		require.Empty(t, sum.Formats)
	})
}

func st(name, dtype string, shape ...int64) safetensors.Tensor {
	return safetensors.Tensor{Name: name, DType: dtype, Shape: shape}
}

// splitsHook is a DatasetSplits hook whose answer the test changes, as the
// Parquet export does when it converts a Commit.
type splitsHook struct {
	splits []indexdb.DatasetSplit
	err    error
}

func (h *splitsHook) get(context.Context, uuid.UUID, model.CommitSHA) ([]indexdb.DatasetSplit, error) {
	return h.splits, h.err
}

func TestDatasetSplitSizesFollowTheParquetExport(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		ctx := t.Context()
		repos := repotest.NewFake(t)
		id := model.RepoID{Type: model.RepoTypeDataset, Namespace: "alice", Name: "reviews"}
		r := indextest.CreateRepo(t, repos, id, model.VisibilityPublic)
		indextest.InsertRepoRow(t, d, r)
		sha := indextest.Commit(t, repos, id, map[string][]byte{
			"README.md":       []byte("---\nsize_categories:\n- n<1K\nlanguage:\n- en\n---\n# Reviews\n"),
			"data/train.csv":  []byte("text,label\ngood,1\n"),
			"data/test.jsonl": []byte(`{"text":"bad","label":0}` + "\n"),
		})
		hook := &splitsHook{}
		svc := indexdb.New(indexdb.Options{DB: d, Repos: repos, DatasetSplits: hook.get})

		// Indexed before the export: the card's size category, no sizes.
		sum, err := svc.Index(ctx, r.UUID, id, sha)
		require.NoError(t, err)
		require.Equal(t, "n<1K", sum.Dataset.SizeCategory)
		require.Empty(t, sum.Dataset.SplitSizes)
		require.Contains(t, sum.Tags, "size_categories:n<1K")

		// The export finishes: rows and bytes per Split, features per
		// Config, and the size category computed from the rows.
		feat := []index.FeatureColumn{
			{Name: "text", Type: json.RawMessage(`{"dtype":"string","_type":"Value"}`)},
			{Name: "label", Type: json.RawMessage(`{"dtype":"int64","_type":"Value"}`)},
		}
		hook.splits = []indexdb.DatasetSplit{
			{Config: "default", Split: "train", Converted: true, Rows: 12000, Bytes: 900000, ParquetBytes: 400000, Features: feat},
			{Config: "default", Split: "test", Converted: true, Rows: 3000, Bytes: 200000, ParquetBytes: 90000, Features: feat},
		}
		got, err := svc.Get(ctx, r.UUID, sha)
		require.NoError(t, err)
		require.Equal(t, []index.SplitSize{
			{Config: "default", Split: "train", Rows: 12000, Bytes: 900000, ParquetBytes: 400000},
			{Config: "default", Split: "test", Rows: 3000, Bytes: 200000, ParquetBytes: 90000},
		}, got.Dataset.SplitSizes)
		require.Equal(t, int64(15000), got.Dataset.Rows)
		require.Equal(t, int64(1100000), got.Dataset.Bytes)
		cols := got.Dataset.Features["default"]
		require.Len(t, cols, 2)
		for i, c := range cols {
			require.Equal(t, feat[i].Name, c.Name, "columns keep their order")
			require.JSONEq(t, string(feat[i].Type), string(c.Type))
		}
		require.Equal(t, "10K<n<100K", got.Dataset.SizeCategory, "computed from rows, over the card's value")
		require.Contains(t, got.Tags, "size_categories:10K<n<100K")
		require.NotContains(t, got.Tags, "size_categories:n<1K")

		// The sizes are stored: the export drops a Commit's rows when it
		// converts a newer Commit, and the index keeps them.
		var row db.Metadata
		require.NoError(t, d.NewSelect().Model(&row).Where("repo_id = ?", r.UUID).Where("commit_sha = ?", string(sha)).Scan(ctx))
		require.Contains(t, string(row.Summary), `"split_sizes"`)
		_, err = svc.Get(ctx, r.UUID, sha)
		require.NoError(t, err)
		var again db.Metadata
		require.NoError(t, d.NewSelect().Model(&again).Where("repo_id = ?", r.UUID).Where("commit_sha = ?", string(sha)).Scan(ctx))
		require.True(t, row.UpdatedAt.Equal(again.UpdatedAt), "Get writes only when the sizes change")
		hook.splits = nil
		kept, err := svc.Get(ctx, r.UUID, sha)
		require.NoError(t, err)
		require.Equal(t, got.Dataset.SplitSizes, kept.Dataset.SplitSizes)
		require.Equal(t, got.Dataset.SizeCategory, kept.Dataset.SizeCategory)
		require.Len(t, kept.Dataset.Features["default"], 2)

		// A partial export or a Split still converting gives rows so far,
		// but not a size category from them.
		hook.splits = []indexdb.DatasetSplit{
			{Config: "default", Split: "train", Converted: true, Partial: true, Rows: 5_000_000, Bytes: 1 << 30},
			{Config: "default", Split: "test"},
		}
		sum, err = svc.Index(ctx, r.UUID, id, sha)
		require.NoError(t, err)
		require.Equal(t, int64(5_000_000), sum.Dataset.Rows)
		require.True(t, sum.Dataset.SplitSizes[0].Partial)
		require.Equal(t, "n<1K", sum.Dataset.SizeCategory, "the card's value while the rows are incomplete")

		hook.err = errors.New("database unavailable")
		_, err = svc.Get(ctx, r.UUID, sha)
		require.ErrorContains(t, err, "dataset splits")
	})
}

func TestModelCardDatasetsAreIndexed(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		ctx := t.Context()
		repos := repotest.NewFake(t)
		id := model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "classifier"}
		r := indextest.CreateRepo(t, repos, id, model.VisibilityPublic)
		indextest.InsertRepoRow(t, d, r)
		sha := indextest.Commit(t, repos, id, map[string][]byte{
			"README.md": []byte("---\ndatasets:\n- alice/reviews\n- imdb\n---\n# Classifier\n"),
		})
		svc := indexdb.New(indexdb.Options{DB: d, Repos: repos})
		sum, err := svc.Index(ctx, r.UUID, id, sha)
		require.NoError(t, err)
		require.Equal(t, []string{"alice/reviews", "imdb"}, sum.Datasets)
		require.Nil(t, sum.Dataset, "a model has no dataset summary")
	})
}

// deletingRepos deletes the Repo when the index first opens a file, as a
// user deleting the Repo while its index Job runs does.
type deletingRepos struct {
	*repotest.Fake
	id   model.RepoID
	once sync.Once
}

func (d *deletingRepos) OpenFile(ctx context.Context, id model.RepoID, commit model.CommitSHA, p string, rng *blob.Range) (io.ReadCloser, model.Entry, error) {
	d.once.Do(func() {
		if err := d.Delete(ctx, d.id, indextest.Owner(d.id.Namespace)); err != nil {
			panic(err)
		}
	})
	return d.Fake.OpenFile(ctx, id, commit, p, rng)
}

func TestHandleJobOfARepoDeletedWhileItRuns(t *testing.T) {
	d := dbtest.SQLite(t)
	e := newEnv(t, d)
	ctx := t.Context()
	raw, err := json.Marshal(index.CommitJob{RepoUUID: e.repo.UUID, Commit: e.sha})
	require.NoError(t, err)

	svc := indexdb.New(indexdb.Options{DB: d, Repos: &deletingRepos{Fake: e.repos, id: e.id}, Webhooks: e.hooks})
	require.NoError(t, svc.HandleJob(ctx, &jobs.Job{Kind: jobs.KindIndex, Payload: raw}, nil),
		"a Repo deleted mid-index ends the Job without failing it")
	require.Empty(t, e.hooks.Names())
	_, err = e.repos.Restore(ctx, e.id, indextest.Owner("alice"))
	require.NoError(t, err)

	// The hook after the index fails because the Repo was deleted meanwhile.
	hooked := indexdb.New(indexdb.Options{DB: d, Repos: e.repos, AfterIndex: func(ctx context.Context, j index.CommitJob, _ *index.Summary) error {
		if err := e.repos.Delete(ctx, e.id, indextest.Owner("alice")); err != nil {
			return err
		}
		return repo.ErrNotFound
	}})
	require.NoError(t, hooked.HandleJob(ctx, &jobs.Job{Kind: jobs.KindIndex, Payload: raw}, nil))
}

// TestGetManyMatchesGet checks that GetMany gives each Commit what Get
// gives it: the newest indexer version of that Commit's results, with the
// Parquet export's Split sizes, whether the export's Splits are read in
// one batch (DatasetSplitsOf) or a Repo at a time. Commits without results
// and results of another Commit are left out.
func TestGetManyMatchesGet(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		ctx := t.Context()
		e := newEnv(t, d)
		_, err := e.svc.Index(ctx, e.repo.UUID, e.id, e.sha)
		require.NoError(t, err)

		dsID := model.RepoID{Type: model.RepoTypeDataset, Namespace: "alice", Name: "reviews"}
		ds := indextest.CreateRepo(t, e.repos, dsID, model.VisibilityPublic)
		indextest.InsertRepoRow(t, d, ds)
		dsSHA := indextest.Commit(t, e.repos, dsID, map[string][]byte{
			"README.md":      []byte("---\nlanguage:\n- en\n---\n# Reviews\n"),
			"data/train.csv": []byte("text,label\ngood,1\n"),
		})
		splits := []indexdb.DatasetSplit{{Config: "default", Split: "train", Converted: true, Rows: 2000, Bytes: 64000}}
		hook := &splitsHook{splits: splits}
		batches := 0
		batch := func(_ context.Context, commits map[uuid.UUID]model.CommitSHA) (map[uuid.UUID][]indexdb.DatasetSplit, error) {
			batches++
			out := map[uuid.UUID][]indexdb.DatasetSplit{}
			for id := range commits {
				out[id] = splits
			}
			return out, nil
		}

		// An unindexed Repo, and a Commit the index has no results for.
		emptyID := model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "empty"}
		empty := indextest.CreateRepo(t, e.repos, emptyID, model.VisibilityPublic)
		indextest.InsertRepoRow(t, d, empty)
		commits := map[uuid.UUID]model.CommitSHA{
			e.repo.UUID: e.sha,
			ds.UUID:     dsSHA,
			empty.UUID:  model.CommitSHA("0123456789abcdef0123456789abcdef01234567"),
		}

		for _, tc := range []struct {
			name string
			opts indexdb.Options
		}{
			{"one Repo at a time", indexdb.Options{DB: d, Repos: e.repos, DatasetSplits: hook.get}},
			{"batched", indexdb.Options{DB: d, Repos: e.repos, DatasetSplits: hook.get, DatasetSplitsOf: batch}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				svc := indexdb.New(tc.opts)
				_, err := svc.Index(ctx, ds.UUID, dsID, dsSHA)
				require.NoError(t, err)
				before := batches
				got, err := svc.GetMany(ctx, commits)
				require.NoError(t, err)
				require.Len(t, got, 2, "only Commits with results")
				for _, id := range []uuid.UUID{e.repo.UUID, ds.UUID} {
					one, err := svc.Get(ctx, id, commits[id])
					require.NoError(t, err)
					require.Equal(t, one, got[id])
				}
				require.Equal(t, int64(2000), got[ds.UUID].Dataset.Rows)
				if tc.opts.DatasetSplitsOf != nil {
					require.Equal(t, before+1, batches, "one batch for every dataset")
				}

				other, err := svc.GetMany(ctx, map[uuid.UUID]model.CommitSHA{e.repo.UUID: dsSHA})
				require.NoError(t, err)
				require.Empty(t, other, "results of another Commit are not used")
			})
		}

		hook.err = errors.New("database unavailable")
		failing := indexdb.New(indexdb.Options{DB: d, Repos: e.repos, DatasetSplits: hook.get,
			DatasetSplitsOf: func(context.Context, map[uuid.UUID]model.CommitSHA) (map[uuid.UUID][]indexdb.DatasetSplit, error) {
				return nil, hook.err
			}})
		_, err = failing.GetMany(ctx, commits)
		require.ErrorContains(t, err, "dataset splits")
	})
}
