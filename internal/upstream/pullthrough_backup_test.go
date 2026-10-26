package upstream_test

import (
	"bytes"
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/db"
	"github.com/hangar-hub/hangar/internal/jobs"
	"github.com/hangar-hub/hangar/internal/upstream"
	"github.com/hangar-hub/hangar/internal/upstream/upstreamtest"
)

// TestPullThroughHoldsBackItsBackupJob covers role=web with a separate
// role=worker: the web process streams a Pull-through transfer and queues
// a backup pull.file Job, which the worker cannot tell is running
// elsewhere. While the transfer runs, the web process keeps moving the
// Job's start back, so the worker does not download the object a second
// time; once the transfer has stored the object, the Job finds it stored.
func TestPullThroughHoldsBackItsBackupJob(t *testing.T) {
	forEachDB(t, func(t *testing.T, d *db.DB) {
		e := newEnv(t, d, upstreamtest.HarnessOptions{PullThroughWait: 2 * time.Minute})
		alice := e.User("alice")
		w := bytes.Repeat([]byte("held back "), 2000)
		commit := e.Seed(tiny, nil, map[string][]byte{"model.safetensors": w})
		e.SetSettings(upstream.Settings{PullThroughEnabled: true, PullThroughAllow: []string{"acme/*"}, PullThroughMinRole: "user"})
		// The Harness's Service runs the queue's Jobs: it is the worker.
		// web is another process that serves requests and runs no Jobs.
		web := e.NewService(nil).PullThrough()

		// Pull the history, with the worker running in the background.
		ctx, cancel := context.WithCancel(e.Ctx)
		var bg sync.WaitGroup
		bg.Go(func() {
			for ctx.Err() == nil {
				e.Queue.RunPending(ctx)
				time.Sleep(20 * time.Millisecond)
			}
		})
		r, err := web.Repo(e.Ctx, alice, tiny)
		cancel()
		bg.Wait()
		require.NoError(t, err)
		en, err := e.Repos.Entry(e.Ctx, tiny, commit, "model.safetensors")
		require.NoError(t, err)
		require.False(t, en.Present)

		// The web process starts a transfer, which the Upstream holds.
		e.Server.Hold()
		t.Cleanup(e.Server.Release)
		cdn := e.Server.Requests("cdn")
		var (
			reader sync.WaitGroup
			out    []byte
			rerr   error
		)
		defer reader.Wait()
		reader.Go(func() {
			rc, err := web.File(e.Ctx, alice, r, commit, en)
			if err != nil {
				rerr = err
				return
			}
			defer rc.Close()
			out, rerr = io.ReadAll(rc)
		})
		require.Eventually(t, func() bool { return e.Server.Requests("cdn") == cdn+1 }, 30*time.Second, 10*time.Millisecond)
		backups, _, err := e.Queue.List(e.Ctx, jobs.Filter{Kinds: []jobs.Kind{jobs.KindPullFile}, Statuses: []jobs.Status{jobs.StatusPending}})
		require.NoError(t, err)
		require.Len(t, backups, 1)
		backup := backups[0].ID

		// The backup delay passes while the transfer is still running. The
		// web process moves the Job's start back, so the worker leaves it.
		e.Advance(2 * time.Minute)
		require.Eventually(t, func() bool { return e.Job(backup).RunAfter.After(e.Now()) }, 30*time.Second, 50*time.Millisecond,
			"the web process holds the backup Job back")
		ran := make(chan struct{})
		go func() {
			e.Queue.RunPending(e.Ctx)
			close(ran)
		}()
		select {
		case <-ran:
		case <-time.After(30 * time.Second):
			t.Fatal("the worker started a second download of the object")
		}
		require.Equal(t, jobs.StatusPending, e.Job(backup).Status)
		require.Equal(t, cdn+1, e.Server.Requests("cdn"))

		// The transfer finishes, and the backup Job finds the object stored.
		e.Server.Release()
		reader.Wait()
		require.NoError(t, rerr)
		require.Equal(t, w, out)
		require.Eventually(t, func() bool {
			en, err := e.Repos.Entry(e.Ctx, tiny, commit, "model.safetensors")
			return err == nil && en.Present
		}, 30*time.Second, 10*time.Millisecond)
		require.Eventually(t, func() bool {
			e.Advance(2 * time.Minute)
			e.Queue.RunPending(e.Ctx)
			return e.Job(backup).Status == jobs.StatusSucceeded
		}, 60*time.Second, 100*time.Millisecond, "the backup Job runs once the web process stops holding it back")
		require.Equal(t, cdn+1, e.Server.Requests("cdn"), "the object was downloaded once")
	})
}
