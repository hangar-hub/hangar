package jobstest_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/jobs"
	"github.com/hangar-hub/hangar/internal/jobs/jobstest"
)

func TestFakeRunsJobsByPriorityAndDedupes(t *testing.T) {
	ctx := context.Background()
	reg := jobs.NewRegistry()
	var order []string
	require.NoError(t, reg.Register(jobs.KindIndex, func(ctx context.Context, j *jobs.Job, p jobs.Progress) error {
		var payload struct{ Name string }
		require.NoError(t, j.DecodePayload(&payload))
		order = append(order, payload.Name)
		require.NoError(t, p.Report(ctx, jobs.ProgressUpdate{BytesDone: 5, BytesTotal: 10}))
		return p.SetResult(ctx, map[string]string{"done": payload.Name})
	}, jobs.HandlerOptions{}))
	q := jobstest.NewFake(reg)

	user := uuid.New()
	low, err := q.Enqueue(ctx, jobs.NewJob{Kind: jobs.KindIndex, Payload: map[string]string{"Name": "low"}, Priority: jobs.PriorityLow, CreatedBy: user})
	require.NoError(t, err)
	high, err := q.Enqueue(ctx, jobs.NewJob{Kind: jobs.KindIndex, DedupeKey: "k", Payload: map[string]string{"Name": "high"}, Priority: jobs.PriorityHigh})
	require.NoError(t, err)
	dup, err := q.Enqueue(ctx, jobs.NewJob{Kind: jobs.KindIndex, DedupeKey: "k", Payload: map[string]string{"Name": "dup"}})
	require.NoError(t, err)
	require.Equal(t, high, dup, "a pending Job with the same key is joined")
	_, err = q.Enqueue(ctx, jobs.NewJob{Kind: jobs.KindScan}) // no handler: never runs
	require.NoError(t, err)
	_, err = q.Enqueue(ctx, jobs.NewJob{})
	require.Error(t, err)
	_, err = q.Enqueue(ctx, jobs.NewJob{Kind: jobs.KindIndex, Payload: func() {}})
	require.Error(t, err)

	require.Equal(t, 2, q.RunPending(ctx))
	require.Equal(t, []string{"high", "low"}, order)
	j, err := q.Get(ctx, low)
	require.NoError(t, err)
	require.Equal(t, jobs.StatusSucceeded, j.Status)
	require.Equal(t, int64(5), j.Progress.BytesDone)
	require.JSONEq(t, `{"done":"low"}`, string(j.Result))
	require.NotNil(t, j.FinishedAt)

	// Once finished, the key is free again.
	again, err := q.Enqueue(ctx, jobs.NewJob{Kind: jobs.KindIndex, DedupeKey: "k", Payload: map[string]string{"Name": "again"}})
	require.NoError(t, err)
	require.NotEqual(t, high, again)

	mine, _, err := q.List(ctx, jobs.Filter{CreatedBy: user})
	require.NoError(t, err)
	require.Len(t, mine, 1)
	page, next, err := q.List(ctx, jobs.Filter{Limit: 2})
	require.NoError(t, err)
	require.Len(t, page, 2)
	require.Equal(t, "2", next)
	rest, _, err := q.List(ctx, jobs.Filter{Cursor: next, Limit: 2})
	require.NoError(t, err)
	require.Len(t, rest, 2)
	pending, _, err := q.List(ctx, jobs.Filter{Statuses: []jobs.Status{jobs.StatusPending}})
	require.NoError(t, err)
	require.Len(t, pending, 2, "the new Job and the one with no handler")
	_, _, err = q.List(ctx, jobs.Filter{Cursor: "x"})
	require.Error(t, err)
	_, err = q.Get(ctx, uuid.New())
	require.ErrorIs(t, err, jobs.ErrNotFound)
}

func TestFakeRetriesWithBackoffAndPermanentErrors(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	reg := jobs.NewRegistry()
	attempts := 0
	require.NoError(t, reg.Register(jobs.KindPullFile, func(context.Context, *jobs.Job, jobs.Progress) error {
		attempts++
		if attempts < 3 {
			return errors.New("connection reset")
		}
		return nil
	}, jobs.HandlerOptions{MaxAttempts: 3}))
	require.NoError(t, reg.Register(jobs.KindGC, func(context.Context, *jobs.Job, jobs.Progress) error {
		return jobs.Permanent(errors.New("bad config"))
	}, jobs.HandlerOptions{}))
	q := jobstest.NewFake(reg)
	q.SetClock(func() time.Time { return now })

	id, err := q.Enqueue(ctx, jobs.NewJob{Kind: jobs.KindPullFile})
	require.NoError(t, err)
	require.Equal(t, 1, q.RunPending(ctx))
	j, _ := q.Get(ctx, id)
	require.Equal(t, jobs.StatusPending, j.Status)
	require.Equal(t, "connection reset", j.Error)
	require.Equal(t, now.Add(30*time.Second), j.RunAfter)
	require.Equal(t, 0, q.RunPending(ctx), "not due yet")
	now = now.Add(time.Hour)
	require.Equal(t, 1, q.RunPending(ctx), "second attempt fails and backs off for a minute")
	now = now.Add(time.Hour)
	require.Equal(t, 1, q.RunPending(ctx))
	j, _ = q.Get(ctx, id)
	require.Equal(t, jobs.StatusSucceeded, j.Status)
	require.Equal(t, 3, j.Attempts)

	gc, err := q.Enqueue(ctx, jobs.NewJob{Kind: jobs.KindGC})
	require.NoError(t, err)
	q.RunPending(ctx)
	j, _ = q.Get(ctx, gc)
	require.Equal(t, jobs.StatusFailed, j.Status, "permanent errors are not retried")
	require.Equal(t, 1, j.Attempts)

	retry, err := q.Retry(ctx, gc)
	require.NoError(t, err)
	require.NotEqual(t, gc, retry)
	_, err = q.Retry(ctx, retry)
	require.ErrorIs(t, err, jobs.ErrNotRetrying)
	_, err = q.Retry(ctx, uuid.New())
	require.ErrorIs(t, err, jobs.ErrNotFound)
}

func TestFakeCancelAndSchedules(t *testing.T) {
	ctx := context.Background()
	reg := jobs.NewRegistry()
	var q *jobstest.Fake
	require.NoError(t, reg.Register(jobs.KindZipExport, func(ctx context.Context, j *jobs.Job, _ jobs.Progress) error {
		require.NoError(t, q.Cancel(ctx, j.ID))
		<-ctx.Done()
		return ctx.Err()
	}, jobs.HandlerOptions{}))
	q = jobstest.NewFake(reg)
	id, err := q.Enqueue(ctx, jobs.NewJob{Kind: jobs.KindZipExport})
	require.NoError(t, err)
	q.RunPending(ctx)
	j, _ := q.Get(ctx, id)
	require.Equal(t, jobs.StatusCancelled, j.Status, "a running Job sees its context cancelled")
	require.ErrorIs(t, q.Cancel(ctx, id), jobs.ErrNotActive)
	require.ErrorIs(t, q.Cancel(ctx, uuid.New()), jobs.ErrNotFound)

	// Cancelling a Job cancels its active children and theirs.
	parent, err := q.Enqueue(ctx, jobs.NewJob{Kind: jobs.KindGC})
	require.NoError(t, err)
	child, err := q.Enqueue(ctx, jobs.NewJob{Kind: jobs.KindGC, DedupeKey: "child", ParentID: parent})
	require.NoError(t, err)
	grandchild, err := q.Enqueue(ctx, jobs.NewJob{Kind: jobs.KindGC, DedupeKey: "grandchild", ParentID: child})
	require.NoError(t, err)
	other, err := q.Enqueue(ctx, jobs.NewJob{Kind: jobs.KindGC, DedupeKey: "other"})
	require.NoError(t, err)
	require.NoError(t, q.Cancel(ctx, parent))
	for _, c := range []jobs.JobID{parent, child, grandchild} {
		j, _ := q.Get(ctx, c)
		require.Equal(t, jobs.StatusCancelled, j.Status)
	}
	j, _ = q.Get(ctx, other)
	require.Equal(t, jobs.StatusPending, j.Status)

	require.NoError(t, q.SetSchedule(ctx, jobs.Schedule{Kind: jobs.KindMirrorSync, Key: "repo-1", Cron: "0 */6 * * *", Enabled: true}))
	require.NoError(t, q.SetSchedule(ctx, jobs.Schedule{Kind: jobs.KindGC, Key: "gc", Cron: "0 3 * * *"}))
	require.Error(t, q.SetSchedule(ctx, jobs.Schedule{Kind: jobs.KindGC}))
	require.Len(t, q.Schedules(), 2)
	require.Equal(t, jobs.KindGC, q.Schedules()[0].Kind)
	require.NoError(t, q.DeleteSchedule(ctx, jobs.KindGC, "gc"))
	require.Len(t, q.Schedules(), 1)
}

func TestFakeLimiter(t *testing.T) {
	var l jobstest.Limiter
	require.NoError(t, l.WaitN(context.Background(), "pull", 100))
	require.NoError(t, l.WaitN(context.Background(), "pull", 50))
	require.Equal(t, int64(150), l.Bytes("pull"))
	require.Error(t, l.WaitN(context.Background(), "pull", -1))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, l.WaitN(ctx, "pull", 1), context.Canceled)
}

func TestFakeKeepsRepoAndRedactsErrors(t *testing.T) {
	ctx := context.Background()
	reg := jobs.NewRegistry()
	require.NoError(t, reg.Register(jobs.KindZipExport, func(context.Context, *jobs.Job, jobs.Progress) error {
		return jobs.Permanent(errors.New(`Get "https://b.example/k?X-Amz-Signature=s3cr3t": EOF`))
	}, jobs.HandlerOptions{}))
	q := jobstest.NewFake(reg)
	repo := uuid.New()
	id, err := q.Enqueue(ctx, jobs.NewJob{Kind: jobs.KindZipExport, Repo: repo})
	require.NoError(t, err)
	require.Equal(t, 1, q.RunPending(ctx))
	j, err := q.Get(ctx, id)
	require.NoError(t, err)
	require.Equal(t, repo, j.Repo)
	require.Equal(t, `Get "https://b.example/k?redacted": EOF`, j.Error, "the fake stores errors as the worker does")
	retried, err := q.Retry(ctx, id)
	require.NoError(t, err)
	j, err = q.Get(ctx, retried)
	require.NoError(t, err)
	require.Equal(t, repo, j.Repo)
}
