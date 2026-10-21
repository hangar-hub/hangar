package repo_test

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/blob"
	"github.com/hangar-hub/hangar/internal/blob/blobtest"
	"github.com/hangar-hub/hangar/internal/db"
	"github.com/hangar-hub/hangar/internal/gc"
	"github.com/hangar-hub/hangar/internal/gitstore"
	"github.com/hangar-hub/hangar/internal/model"
	"github.com/hangar-hub/hangar/internal/repo"
)

// beforeGitPut runs a function once, before the first git data is written:
// in a Commit, after its LFS objects are checked and before the transaction
// that records its references.
type beforeGitPut struct {
	*blobtest.Fake
	once sync.Once
	fn   func()
}

func (b *beforeGitPut) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	if strings.HasPrefix(key, blob.PrefixGit) {
		b.once.Do(b.fn)
	}
	return b.Fake.Put(ctx, key, r, size)
}

// TestCommitReferencingAnObjectGCDeletes runs garbage collection in the
// window between a Commit checking that an LFS object is stored and the
// transaction that records the Commit's reference to it, through the real
// writer. The Commit must fail with ErrLFSObjectMissing rather than point
// at an object that is gone, and succeed once the object is uploaded again.
func TestCommitReferencingAnObjectGCDeletes(t *testing.T) {
	forEach(t, func(t *testing.T, e *env) {
		alice := e.addUser("alice")
		id := model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "raced"}
		e.create(repo.CreateParams{ID: id, Actor: alice})
		head, err := e.svc.Resolve(e.ctx, id, "main")
		require.NoError(t, err)
		weights := []byte("weights nobody referenced for a month")
		oid := e.putLFS(weights, alice)

		// Garbage collection a month later: the object is unreferenced and
		// past its grace period.
		collector := gc.New(e.d, e.blobs, gc.Options{Now: func() time.Time { return time.Now().Add(30 * 24 * time.Hour) }})
		hook := &beforeGitPut{Fake: e.blobs, fn: func() {
			r, err := collector.Run(context.Background(), gc.ScopeLFS, nil)
			require.NoError(t, err)
			require.Equal(t, 1, r.LFSObjectsDeleted)
		}}
		gs, err := gitstore.New(gitstore.Options{DB: e.d, Blobs: hook, CacheDir: t.TempDir(), Timeout: time.Minute})
		require.NoError(t, err)
		opts := e.opts
		opts.Git = gs
		svc, err := repo.NewStore(opts)
		require.NoError(t, err)

		op := repo.Operation{Kind: repo.OpAddLFS, Path: "model.safetensors", OID: oid, Size: int64(len(weights))}
		_, err = svc.Commit(e.ctx, repo.CommitParams{Repo: id, Summary: "add weights", Operations: []repo.Operation{op}, Actor: alice})
		require.ErrorIs(t, err, repo.ErrLFSObjectMissing)
		after, err := e.svc.Resolve(e.ctx, id, "main")
		require.NoError(t, err)
		require.Equal(t, head, after, "the branch did not move")
		n, err := e.d.NewSelect().Model((*db.RepoLFS)(nil)).Where("oid = ?", oid).Count(e.ctx)
		require.NoError(t, err)
		require.Zero(t, n, "no Repo references the deleted object")

		// The client uploads the object again and commits.
		e.putLFS(weights, alice)
		_, err = svc.Commit(e.ctx, repo.CommitParams{Repo: id, Summary: "add weights", Operations: []repo.Operation{op}, Actor: alice})
		require.NoError(t, err)
		require.Equal(t, string(weights), e.read(id, "main", "model.safetensors"))
	})
}
