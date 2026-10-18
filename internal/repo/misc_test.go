package repo_test

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/db"
	"github.com/jamesagarside/hangar/internal/jobs"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/repo"
)

func TestNewStoreNeedsItsDependencies(t *testing.T) {
	_, err := repo.NewStore(repo.Options{})
	require.Error(t, err)
}

// TestOptionalCollaboratorsMayFail checks that a failing webhook emitter or
// Job queue never fails a write that has already happened, and that the
// service works with none of them configured.
func TestOptionalCollaboratorsMayFail(t *testing.T) {
	forEach(t, func(t *testing.T, e *env) {
		alice := e.addUser("alice")
		e.hooks.Err = errors.New("webhooks down")
		failing, err := repo.NewStore(withOpts(e.opts, func(o *repo.Options) { o.Jobs = failingQueue{e.jobs} }))
		require.NoError(t, err)
		id := model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "x"}
		_, err = failing.Create(e.ctx, repo.CreateParams{ID: id, Actor: alice})
		require.NoError(t, err)
		_, err = failing.Commit(e.ctx, repo.CommitParams{Repo: id, Summary: "s", Actor: alice, Operations: []repo.Operation{add("a", "a")}})
		require.NoError(t, err)

		bare, err := repo.NewStore(repo.Options{DB: e.d, Git: e.git, LFS: e.lfs, Namespaces: e.nsNames})
		require.NoError(t, err)
		id2 := model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "y"}
		_, err = bare.Create(e.ctx, repo.CreateParams{ID: id2, Actor: alice})
		require.NoError(t, err)
		require.NoError(t, bare.Delete(e.ctx, id2, alice))
	})
}

type failingQueue struct{ jobs.Queue }

func (failingQueue) Enqueue(context.Context, jobs.NewJob) (jobs.JobID, error) {
	return jobs.JobID{}, errors.New("queue down")
}

// quota refuses everything over a byte limit.
type quota struct{ limit int64 }

func (q quota) CheckQuota(_ context.Context, _ uuid.UUID, add int64) error {
	if add > q.limit {
		return errors.New("the namespace quota is full")
	}
	return nil
}

func TestPresignedStoreAndQuota(t *testing.T) {
	forEach(t, func(t *testing.T, e *env) {
		e.blobs.PresignBaseURL = "https://s3.example"
		content := []byte("presign me")
		oid := e.putLFS(content)
		u, err := e.lfs.PresignGet(e.ctx, oid, time.Hour, "w.bin")
		require.NoError(t, err)
		require.Contains(t, u, "https://s3.example")
		req, err := e.lfs.PresignPut(e.ctx, sha256Hex([]byte("new")), 3, time.Hour, uuid.Nil)
		require.NoError(t, err)
		require.NotEmpty(t, req.URL)
		up, err := e.lfs.CreateUpload(e.ctx, repo.UploadParams{OID: oid, Size: 20, Multipart: true, PartSize: 10})
		require.NoError(t, err)
		part, err := e.lfs.PresignPart(e.ctx, up.ID, 2, time.Hour)
		require.NoError(t, err)
		require.NotEmpty(t, part)
		_, err = e.lfs.PresignPart(e.ctx, up.ID, 3, time.Hour)
		require.ErrorIs(t, err, repo.ErrInvalid)
		single, err := e.lfs.CreateUpload(e.ctx, repo.UploadParams{OID: oid, Size: 1})
		require.NoError(t, err)
		_, err = e.lfs.PresignPart(e.ctx, single.ID, 1, time.Hour)
		require.ErrorIs(t, err, repo.ErrInvalid)
		owner, err := e.lfs.UploadRepo(e.ctx, single.ID)
		require.NoError(t, err)
		require.Zero(t, owner)
		_, err = e.lfs.PresignGet(e.ctx, sha256Hex([]byte("missing")), time.Hour, "x")
		require.ErrorIs(t, err, repo.ErrNotFound)

		alice := e.addUser("alice")
		id := model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "quota"}
		e.create(repo.CreateParams{ID: id, Actor: alice})
		limited, err := repo.NewStore(withOpts(e.opts, func(o *repo.Options) { o.Quota = quota{limit: 5} }))
		require.NoError(t, err)
		_, err = limited.Commit(e.ctx, repo.CommitParams{Repo: id, Summary: "s", Actor: alice, Operations: []repo.Operation{add("a.txt", "too long for quota")}})
		require.ErrorContains(t, err, "quota")
		_, err = limited.Commit(e.ctx, repo.CommitParams{Repo: id, Summary: "s", Actor: alice, Operations: []repo.Operation{add("a.txt", "ok")}})
		require.NoError(t, err)
		head, err := e.svc.Resolve(e.ctx, id, "")
		require.NoError(t, err)
		w := e.clone(id)
		w.write("big.txt", "bigger than five")
		next := w.commit("big")
		var pe *repo.PolicyError
		err = limited.ApplyPack(e.ctx, repo.ApplyPackParams{Repo: id, Source: repo.SourcePush, Pack: bytes.NewReader(w.pack(next, head)),
			Updates: []repo.RefUpdate{{Name: "refs/heads/main", Old: head, New: next}}})
		require.ErrorAs(t, err, &pe)
		require.Contains(t, pe.Error(), "quota")
	})
}

func TestPurgeJobWaitsForTheRestoreWindow(t *testing.T) {
	forEach(t, func(t *testing.T, e *env) {
		require.NoError(t, e.svc.RegisterJobs(e.reg, e.lfs, repo.JobOptions{Blobs: e.blobs}))
		alice := e.addUser("alice")
		id := model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "soon"}
		r := e.create(repo.CreateParams{ID: id, Actor: alice})
		require.NoError(t, e.svc.Delete(e.ctx, id, alice))
		deleted, err := e.svc.GetByUUID(e.ctx, r.UUID)
		require.NoError(t, err)

		// Run early, the purge refuses and the Job is retried later.
		jid, err := e.jobs.Enqueue(e.ctx, jobs.NewJob{Kind: jobs.KindRepoPurge, Payload: repo.PurgeJob{RepoUUID: r.UUID, DeletedAt: *deleted.DeletedAt}})
		require.NoError(t, err)
		e.jobs.RunPending(e.ctx)
		j, err := e.jobs.Get(e.ctx, jid)
		require.NoError(t, err)
		require.Contains(t, j.Error, "cannot be purged before")
		_, err = e.svc.GetByUUID(e.ctx, r.UUID)
		require.NoError(t, err)

		bad, err := e.jobs.Enqueue(e.ctx, jobs.NewJob{Kind: jobs.KindRepoPurge, DedupeKey: "bad", Payload: "not an object"})
		require.NoError(t, err)
		gone, err := e.jobs.Enqueue(e.ctx, jobs.NewJob{Kind: jobs.KindRepoPurge, DedupeKey: "gone", Payload: repo.PurgeJob{RepoUUID: db.NewID()}})
		require.NoError(t, err)
		e.jobs.RunPending(e.ctx)
		j, err = e.jobs.Get(e.ctx, bad)
		require.NoError(t, err)
		require.Equal(t, jobs.StatusFailed, j.Status)
		j, err = e.jobs.Get(e.ctx, gone)
		require.NoError(t, err)
		require.Equal(t, jobs.StatusSucceeded, j.Status)
	})
}
