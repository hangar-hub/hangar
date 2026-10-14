package indexdb_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	"github.com/jamesagarside/hangar/internal/db"
	"github.com/jamesagarside/hangar/internal/db/dbtest"
	"github.com/jamesagarside/hangar/internal/index"
	"github.com/jamesagarside/hangar/internal/index/indexdb"
	"github.com/jamesagarside/hangar/internal/index/indextest"
	"github.com/jamesagarside/hangar/internal/index/safetensors"
	"github.com/jamesagarside/hangar/internal/jobs"
	"github.com/jamesagarside/hangar/internal/jobs/jobstest"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/repo"
	"github.com/jamesagarside/hangar/internal/repo/repotest"
	"github.com/jamesagarside/hangar/internal/telemetry"
	"github.com/jamesagarside/hangar/internal/webhook"
	"github.com/jamesagarside/hangar/internal/webhook/webhooktest"
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
