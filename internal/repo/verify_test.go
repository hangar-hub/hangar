package repo_test

import (
	"bytes"
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/repo"
)

// gatedLFS holds every Verify until gate is closed, like a re-hash of a
// large object that takes longer than lfs.verify_wait.
type gatedLFS struct {
	*repo.LFS
	gate  chan struct{}
	calls atomic.Int32
}

func (g *gatedLFS) Verify(ctx context.Context, oid string, size int64) (repo.LFSObject, error) {
	g.calls.Add(1)
	select {
	case <-g.gate:
	case <-ctx.Done():
		return repo.LFSObject{}, ctx.Err()
	}
	return g.LFS.Verify(ctx, oid, size)
}

func TestWritesWaitForLFSVerification(t *testing.T) {
	forEach(t, func(t *testing.T, e *env) {
		g := &gatedLFS{LFS: e.lfs, gate: make(chan struct{})}
		svc, err := repo.NewStore(withOpts(e.opts, func(o *repo.Options) {
			o.LFS = g
			o.VerifyWait = 50 * time.Millisecond
		}))
		require.NoError(t, err)
		alice := e.addUser("alice")
		id := model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "verify"}
		e.create(repo.CreateParams{ID: id, Actor: alice})
		base, err := e.svc.Resolve(e.ctx, id, "main")
		require.NoError(t, err)

		// Two objects written through presigned PUTs and never verified.
		viaCommit := []byte("weights for a commit")
		commitOID := sha256Hex(viaCommit)
		require.NoError(t, e.blobs.Put(e.ctx, model.LFSObjectKey(commitOID), bytes.NewReader(viaCommit), int64(len(viaCommit))))
		viaPush := []byte("weights for a push")
		ptr := model.LFSPointer{OID: sha256Hex(viaPush), Size: int64(len(viaPush))}
		require.NoError(t, e.blobs.Put(e.ctx, model.LFSObjectKey(ptr.OID), bytes.NewReader(viaPush), ptr.Size))

		commit := func() error {
			_, err := svc.Commit(e.ctx, repo.CommitParams{Repo: id, Summary: "weights", Actor: alice,
				Operations: []repo.Operation{{Kind: repo.OpAddLFS, Path: "model.bin", OID: commitOID, Size: int64(len(viaCommit))}}})
			return err
		}
		w := e.clone(id)
		w.write("pushed.bin", string(ptr.Bytes()))
		pushed := w.commit("pointer")
		pack := w.pack(pushed, base)

		// lfs.verify_wait runs out: both writes fail with a clear message
		// and change nothing.
		err = commit()
		require.ErrorIs(t, err, repo.ErrLFSObjectMissing)
		require.Contains(t, err.Error(), "was not verified within 50ms")
		var pe *repo.PolicyError
		err = svc.ApplyPack(e.ctx, repo.ApplyPackParams{Repo: id, Source: repo.SourcePush, Pack: bytes.NewReader(pack),
			Updates: []repo.RefUpdate{{Name: "refs/heads/main", Old: base, New: pushed}}})
		require.ErrorAs(t, err, &pe)
		require.Contains(t, pe.Message, ptr.OID+" (18 bytes) was not verified within 50ms")
		tip, err := e.svc.Resolve(e.ctx, id, "main")
		require.NoError(t, err)
		require.Equal(t, base, tip)

		// The verifications carry on after the wait, so retries find the
		// objects verified without hashing them again.
		close(g.gate)
		require.Eventually(t, func() bool {
			got, err := e.lfs.StatMany(e.ctx, []string{commitOID, ptr.OID})
			return err == nil && len(got) == 2
		}, 10*time.Second, 10*time.Millisecond)
		require.NoError(t, commit())
		tip, err = e.svc.Resolve(e.ctx, id, "main")
		require.NoError(t, err)
		require.Equal(t, string(viaCommit), e.read(id, "main", "model.bin"))
		w2 := e.clone(id)
		w2.write("pushed.bin", string(ptr.Bytes()))
		pushed = w2.commit("pointer")
		require.NoError(t, svc.ApplyPack(e.ctx, repo.ApplyPackParams{Repo: id, Source: repo.SourcePush, Pack: bytes.NewReader(w2.pack(pushed, tip)),
			Updates: []repo.RefUpdate{{Name: "refs/heads/main", Old: tip, New: pushed}}}))
		require.Equal(t, string(viaPush), e.read(id, "main", "pushed.bin"))
		require.Equal(t, int32(2), g.calls.Load())
	})
}
