package xet_test

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/db"
	"github.com/jamesagarside/hangar/internal/db/dbtest"
	"github.com/jamesagarside/hangar/internal/jobs"
	"github.com/jamesagarside/hangar/internal/jobs/jobstest"
	"github.com/jamesagarside/hangar/internal/xet"
	"github.com/jamesagarside/hangar/internal/xet/xettest"
)

func countXorbs(t *testing.T, d *db.DB) int {
	t.Helper()
	n, err := d.NewSelect().Model((*db.XetXorb)(nil)).Count(context.Background())
	require.NoError(t, err)
	return int(n)
}

func TestCollectGarbage(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		e := newEnv(t, d)
		ctx := context.Background()

		old := xettest.BuildUpload([][]byte{textBytes(20_000, 1)}, xettest.UploadOptions{ChunkSize: 4096})
		reused := xettest.BuildUpload([][]byte{textBytes(20_000, 2)}, xettest.UploadOptions{ChunkSize: 4096})
		e.putAll(t, old)
		e.putAll(t, reused)
		_, _, err := e.accept(t, old)
		require.NoError(t, err)
		_, _, err = e.accept(t, reused)
		require.NoError(t, err)

		// 27 days later a shard names `reused` again; then 2 more days pass.
		e.advance(27 * 24 * time.Hour)
		_, _, err = e.accept(t, xettest.BuildUpload([][]byte{textBytes(20_000, 2)}, xettest.UploadOptions{ChunkSize: 4096, Known: reused.Xorbs}))
		require.NoError(t, err)
		e.advance(2 * 24 * time.Hour)

		// An orphaned object: stored, but its row was never written.
		orphan := xettest.BuildXorb([][]byte{[]byte("orphan")}, false)
		require.NoError(t, e.blobs.Put(ctx, xet.XorbKey(orphan.Hash), bytes.NewReader([]byte("x")), 1))
		// LFS GC removed the object behind `old`'s file record.
		oldOID := old.Files[0].SHA256
		e.gc.remove(oldOID)

		var steps int
		res, err := e.svc.CollectGarbage(ctx, func(xet.GCResult) { steps++ })
		require.NoError(t, err)
		require.Equal(t, int64(1), res.XorbsDeleted)
		require.Positive(t, res.BytesFreed)
		require.Equal(t, int64(1), res.OrphansDeleted)
		require.Equal(t, int64(1), res.FilesPruned)
		require.GreaterOrEqual(t, steps, 3)

		_, ok := e.blobs.Bytes(xet.XorbKey(old.Xorbs[0].Hash))
		require.False(t, ok, "the expired xorb is gone")
		_, ok = e.blobs.Bytes(xet.XorbKey(reused.Xorbs[0].Hash))
		require.True(t, ok, "a xorb named by a recent shard is kept")
		_, ok = e.blobs.Bytes(xet.XorbKey(orphan.Hash))
		require.False(t, ok)
		require.Equal(t, 1, countXorbs(t, d))

		// A shard naming the expired xorb now gets the unknown-xorb error.
		_, _, err = e.accept(t, xettest.BuildUpload([][]byte{textBytes(20_000, 1)}, xettest.UploadOptions{ChunkSize: 4096, Known: old.Xorbs}))
		require.ErrorIs(t, err, xet.ErrUnknownXorb)
		// Uploading it again stores it again.
		e.putAll(t, old)
		require.Equal(t, 2, countXorbs(t, d))

		// A second run finds nothing more; young orphans are left alone. The
		// fake Blob store stamps objects with the real time, so the clock
		// goes back to it.
		e.mu.Lock()
		e.now = time.Now().UTC()
		e.mu.Unlock()
		young := xettest.BuildXorb([][]byte{[]byte("young")}, false)
		require.NoError(t, e.blobs.Put(ctx, xet.XorbKey(young.Hash), bytes.NewReader([]byte("y")), 1))
		res, err = e.svc.CollectGarbage(ctx, nil)
		require.NoError(t, err)
		require.Equal(t, xet.GCResult{}, res)
	})
}

func TestCollectGarbageManyRows(t *testing.T) {
	d := dbtest.SQLite(t)
	e := newEnv(t, d)
	ctx := context.Background()
	// More expired rows than one batch.
	rows := make([]db.XetXorb, 450)
	for i := range rows {
		h := xet.ChunkHash([]byte{byte(i), byte(i >> 8)})
		rows[i] = db.XetXorb{Hash: h.String(), Size: 10, StorageKey: xet.XorbKey(h), LastReferencedAt: e.now.Add(-30 * 24 * time.Hour)}
		require.NoError(t, e.blobs.Put(ctx, rows[i].StorageKey, bytes.NewReader(make([]byte, 10)), 10))
	}
	_, err := d.NewInsert().Model(&rows).Exec(ctx)
	require.NoError(t, err)
	res, err := e.svc.CollectGarbage(ctx, nil)
	require.NoError(t, err)
	require.Equal(t, int64(450), res.XorbsDeleted)
	require.Equal(t, int64(4500), res.BytesFreed)
	require.Zero(t, countXorbs(t, d))
	require.Empty(t, e.blobs.Keys())
}

func TestGCJob(t *testing.T) {
	d := dbtest.SQLite(t)
	e := newEnv(t, d, func(o *xet.Options) { o.XorbRetention = time.Hour })
	ctx := context.Background()
	u := xettest.BuildUpload([][]byte{textBytes(5000, 5)}, xettest.UploadOptions{})
	e.putAll(t, u)
	e.advance(2 * time.Hour)

	reg := jobs.NewRegistry()
	require.NoError(t, e.svc.RegisterJobs(reg))
	require.Error(t, e.svc.RegisterJobs(reg), "registered once")
	q := jobstest.NewFake(reg)
	id, err := q.Enqueue(ctx, jobs.NewJob{Kind: jobs.KindXetGC, DedupeKey: xet.GCJobKey})
	require.NoError(t, err)
	require.Equal(t, 1, q.RunPending(ctx))
	j, err := q.Get(ctx, id)
	require.NoError(t, err)
	require.Equal(t, jobs.StatusSucceeded, j.Status, j.Error)
	var res xet.GCResult
	require.NoError(t, json.Unmarshal(j.Result, &res))
	require.Equal(t, int64(1), res.XorbsDeleted)
	require.Contains(t, j.Progress.Message, "Deleted 1 expired xorbs")

	s := xet.GCSchedule("0 3 * * *")
	require.Equal(t, jobs.Schedule{Kind: jobs.KindXetGC, Key: "xet", Cron: "0 3 * * *", Enabled: true}, s)
	require.NoError(t, q.SetSchedule(ctx, s))
}
