package jobs_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/hangar-hub/hangar/internal/db"
	"github.com/hangar-hub/hangar/internal/db/dbtest"
	"github.com/hangar-hub/hangar/internal/jobs"
)

// clock is a settable clock for leases, backoff and schedules.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock {
	return &clock{t: time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)}
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// newUser inserts a user so created_by satisfies its foreign key.
func newUser(t *testing.T, d *db.DB) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	ns := &db.Namespace{ID: db.NewID(), Name: "u" + uuid.NewString()[:8], Kind: "user"}
	_, err := d.NewInsert().Model(ns).Exec(ctx)
	require.NoError(t, err)
	u := &db.User{ID: db.NewID(), NamespaceID: ns.ID}
	_, err = d.NewInsert().Model(u).Exec(ctx)
	require.NoError(t, err)
	return u.ID
}

func TestQueueEnqueueGetAndDedupe(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		ctx := context.Background()
		clk := newClock()
		reg := jobs.NewRegistry()
		require.NoError(t, reg.Register(jobs.KindGC, noop, jobs.HandlerOptions{MaxAttempts: 2}))
		q := jobs.NewQueue(d, reg, jobs.QueueOptions{Now: clk.Now})
		user := newUser(t, d)

		id, err := q.Enqueue(ctx, jobs.NewJob{Kind: jobs.KindGC, DedupeKey: "all", Payload: map[string]int{"n": 1}, Priority: jobs.PriorityHigh, CreatedBy: user})
		require.NoError(t, err)
		j, err := q.Get(ctx, id)
		require.NoError(t, err)
		require.Equal(t, jobs.KindGC, j.Kind)
		require.Equal(t, jobs.StatusPending, j.Status)
		require.Equal(t, 2, j.MaxAttempts, "the kind's default")
		require.Equal(t, jobs.PriorityHigh, j.Priority)
		require.Equal(t, user, j.CreatedBy)
		require.JSONEq(t, `{"n":1}`, string(j.Payload))
		require.Equal(t, clk.Now(), j.RunAfter)

		dup, err := q.Enqueue(ctx, jobs.NewJob{Kind: jobs.KindGC, DedupeKey: "all"})
		require.NoError(t, err)
		require.Equal(t, id, dup, "a pending Job with the same kind and key is joined")
		other, err := q.Enqueue(ctx, jobs.NewJob{Kind: jobs.KindIndex, DedupeKey: "all"})
		require.NoError(t, err)
		require.NotEqual(t, id, other, "dedupe keys are per kind")
		noKey1, err := q.Enqueue(ctx, jobs.NewJob{Kind: jobs.KindGC})
		require.NoError(t, err)
		noKey2, err := q.Enqueue(ctx, jobs.NewJob{Kind: jobs.KindGC})
		require.NoError(t, err)
		require.NotEqual(t, noKey1, noKey2, "Jobs without a key never dedupe")

		_, err = q.Enqueue(ctx, jobs.NewJob{})
		require.Error(t, err)
		_, err = q.Enqueue(ctx, jobs.NewJob{Kind: jobs.KindGC, Payload: json.RawMessage("{nope")})
		require.Error(t, err)
		_, err = q.Get(ctx, uuid.New())
		require.ErrorIs(t, err, jobs.ErrNotFound)

		// Once the Job is no longer active, the key is free again.
		require.NoError(t, q.Cancel(ctx, id))
		again, err := q.Enqueue(ctx, jobs.NewJob{Kind: jobs.KindGC, DedupeKey: "all"})
		require.NoError(t, err)
		require.NotEqual(t, id, again)
	})
}

func TestQueueConcurrentEnqueueDedupes(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		ctx := context.Background()
		q := jobs.NewQueue(d, nil, jobs.QueueOptions{})
		var wg sync.WaitGroup
		ids := make([]uuid.UUID, 16)
		errs := make([]error, 16)
		for i := range ids {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				ids[i], errs[i] = q.Enqueue(ctx, jobs.NewJob{Kind: jobs.KindPull, DedupeKey: "hf/model@main"})
			}(i)
		}
		wg.Wait()
		for i := range ids {
			require.NoError(t, errs[i])
			require.Equal(t, ids[0], ids[i])
		}
		n, err := d.NewSelect().Model((*db.Job)(nil)).Where("kind = ?", "pull").Count(ctx)
		require.NoError(t, err)
		require.EqualValues(t, 1, n)
	})
}

func TestQueueListCancelRetry(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		ctx := context.Background()
		q := jobs.NewQueue(d, nil, jobs.QueueOptions{})
		alice, bob := newUser(t, d), newUser(t, d)
		parent, err := q.Enqueue(ctx, jobs.NewJob{Kind: jobs.KindPull, CreatedBy: alice})
		require.NoError(t, err)
		var children []uuid.UUID
		for range 3 {
			c, err := q.Enqueue(ctx, jobs.NewJob{Kind: jobs.KindPullFile, ParentID: parent, CreatedBy: alice})
			require.NoError(t, err)
			children = append(children, c)
		}
		grandchild, err := q.Enqueue(ctx, jobs.NewJob{Kind: jobs.KindIndex, ParentID: children[0]})
		require.NoError(t, err)
		_, err = q.Enqueue(ctx, jobs.NewJob{Kind: jobs.KindIndex, CreatedBy: bob})
		require.NoError(t, err)

		mine, next, err := q.List(ctx, jobs.Filter{CreatedBy: alice})
		require.NoError(t, err)
		require.Empty(t, next)
		require.Len(t, mine, 4)
		require.Equal(t, children[2], mine[0].ID, "newest first")

		page1, next, err := q.List(ctx, jobs.Filter{Kinds: []jobs.Kind{jobs.KindPullFile}, Limit: 2})
		require.NoError(t, err)
		require.Len(t, page1, 2)
		require.NotEmpty(t, next)
		page2, next, err := q.List(ctx, jobs.Filter{Kinds: []jobs.Kind{jobs.KindPullFile}, Limit: 2, Cursor: next})
		require.NoError(t, err)
		require.Len(t, page2, 1)
		require.Empty(t, next)
		require.Equal(t, children[0], page2[0].ID)

		kids, _, err := q.List(ctx, jobs.Filter{ParentID: parent})
		require.NoError(t, err)
		require.Len(t, kids, 3)
		_, _, err = q.List(ctx, jobs.Filter{Cursor: "not-a-cursor"})
		require.Error(t, err)

		// Cancelling the Pull cancels its files and their children.
		require.NoError(t, q.Cancel(ctx, parent))
		for _, id := range append(children, parent, grandchild) {
			j, err := q.Get(ctx, id)
			require.NoError(t, err)
			require.Equal(t, jobs.StatusCancelled, j.Status, id)
			require.NotNil(t, j.FinishedAt)
		}
		require.ErrorIs(t, q.Cancel(ctx, parent), jobs.ErrNotActive)
		require.ErrorIs(t, q.Cancel(ctx, uuid.New()), jobs.ErrNotFound)

		cancelled, _, err := q.List(ctx, jobs.Filter{Statuses: []jobs.Status{jobs.StatusCancelled}})
		require.NoError(t, err)
		require.Len(t, cancelled, 5)

		retried, err := q.Retry(ctx, children[1])
		require.NoError(t, err)
		require.NotEqual(t, children[1], retried)
		rj, err := q.Get(ctx, retried)
		require.NoError(t, err)
		require.Equal(t, jobs.StatusPending, rj.Status)
		require.Equal(t, parent, rj.ParentID)
		require.Equal(t, alice, rj.CreatedBy)
		_, err = q.Retry(ctx, retried)
		require.ErrorIs(t, err, jobs.ErrNotRetrying)
		_, err = q.Retry(ctx, uuid.New())
		require.ErrorIs(t, err, jobs.ErrNotFound)
	})
}

func TestQueuePostpone(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		ctx := context.Background()
		clk := newClock()
		q := jobs.NewQueue(d, nil, jobs.QueueOptions{Now: clk.Now})
		later := clk.Now().Add(time.Hour)
		soon, err := q.Enqueue(ctx, jobs.NewJob{Kind: jobs.KindPullFile, DedupeKey: "soon", RunAfter: clk.Now().Add(time.Minute)})
		require.NoError(t, err)
		snoozed, err := q.Enqueue(ctx, jobs.NewJob{Kind: jobs.KindPullFile, DedupeKey: "snoozed", RunAfter: clk.Now().Add(2 * time.Hour)})
		require.NoError(t, err)
		cancelled, err := q.Enqueue(ctx, jobs.NewJob{Kind: jobs.KindPullFile, DedupeKey: "cancelled"})
		require.NoError(t, err)
		require.NoError(t, q.Cancel(ctx, cancelled))

		for _, id := range []jobs.JobID{soon, snoozed, cancelled, uuid.New()} {
			require.NoError(t, q.Postpone(ctx, id, later))
		}
		j, err := q.Get(ctx, soon)
		require.NoError(t, err)
		require.Equal(t, later, j.RunAfter, "a pending Job due sooner waits until then")
		j, err = q.Get(ctx, snoozed)
		require.NoError(t, err)
		require.Equal(t, clk.Now().Add(2*time.Hour), j.RunAfter, "a Job due later is never brought forward")
		j, err = q.Get(ctx, cancelled)
		require.NoError(t, err)
		require.Equal(t, clk.Now(), j.RunAfter, "a finished Job is left alone")
	})
}

func TestQueueSchedules(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		ctx := context.Background()
		clk := newClock()
		london, err := time.LoadLocation("Europe/London")
		require.NoError(t, err)
		q := jobs.NewQueue(d, nil, jobs.QueueOptions{Now: clk.Now, Location: london})
		require.NoError(t, q.SetSchedule(ctx, jobs.Schedule{Kind: jobs.KindGC, Key: "gc", Cron: "0 3 * * *", Enabled: true}))
		all, err := q.Schedules(ctx)
		require.NoError(t, err)
		require.Len(t, all, 1)
		// 03:00 in London on 14 October 2026 is 02:00 UTC (BST).
		require.Equal(t, time.Date(2026, 10, 14, 2, 0, 0, 0, time.UTC), all[0].NextRunAt.UTC())

		clk.Add(time.Hour)
		require.NoError(t, q.SetSchedule(ctx, jobs.Schedule{Kind: jobs.KindGC, Key: "gc", Cron: "0 3 * * *", Enabled: false, Payload: map[string]bool{"x": true}}))
		all, err = q.Schedules(ctx)
		require.NoError(t, err)
		require.Equal(t, time.Date(2026, 10, 14, 2, 0, 0, 0, time.UTC), all[0].NextRunAt.UTC(), "the same cron keeps the next run")
		require.False(t, all[0].Enabled)
		require.JSONEq(t, `{"x":true}`, string(all[0].Payload.(json.RawMessage)))

		require.NoError(t, q.SetSchedule(ctx, jobs.Schedule{Kind: jobs.KindGC, Key: "gc", Cron: "30 * * * *", Enabled: true}))
		all, err = q.Schedules(ctx)
		require.NoError(t, err)
		require.Equal(t, time.Date(2026, 10, 13, 13, 30, 0, 0, time.UTC), all[0].NextRunAt.UTC(), "a new cron moves the next run")

		require.Error(t, q.SetSchedule(ctx, jobs.Schedule{Kind: jobs.KindGC, Key: "bad", Cron: "every day"}))
		require.Error(t, q.SetSchedule(ctx, jobs.Schedule{Kind: jobs.KindGC, Cron: "* * * * *"}))
		require.NoError(t, q.DeleteSchedule(ctx, jobs.KindGC, "gc"))
		all, err = q.Schedules(ctx)
		require.NoError(t, err)
		require.Empty(t, all)
	})
}

func TestEnqueueRecordsTraceContext(t *testing.T) {
	d := dbtest.SQLite(t)
	ctx, span := sdktrace.NewTracerProvider().Tracer("test").Start(context.Background(), "request")
	defer span.End()
	q := jobs.NewQueue(d, nil, jobs.QueueOptions{})
	id, err := q.Enqueue(ctx, jobs.NewJob{Kind: jobs.KindIndex})
	require.NoError(t, err)
	var row db.Job
	require.NoError(t, d.NewSelect().Model(&row).Where("id = ?", id).Scan(context.Background()))
	require.Contains(t, row.TraceContext["traceparent"], span.SpanContext().TraceID().String())
}

func TestEnqueueTxRollsBack(t *testing.T) {
	d := dbtest.SQLite(t)
	ctx := context.Background()
	q := jobs.NewQueue(d, nil, jobs.QueueOptions{})
	var id uuid.UUID
	err := d.InTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		var err error
		id, err = q.EnqueueTx(ctx, tx, jobs.NewJob{Kind: jobs.KindIndex})
		require.NoError(t, err)
		return context.Canceled
	})
	require.ErrorIs(t, err, context.Canceled)
	_, err = q.Get(ctx, id)
	require.ErrorIs(t, err, jobs.ErrNotFound, "the Job disappears with the transaction")
}

func noop(context.Context, *jobs.Job, jobs.Progress) error { return nil }

func TestQueueKeepsTheJobsRepo(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		ctx := context.Background()
		q := jobs.NewQueue(d, jobs.NewRegistry(), jobs.QueueOptions{})
		repo := uuid.New() // No foreign key: a Job outlives a purged Repo.
		id, err := q.Enqueue(ctx, jobs.NewJob{Kind: jobs.KindZipExport, Repo: repo})
		require.NoError(t, err)
		instance, err := q.Enqueue(ctx, jobs.NewJob{Kind: jobs.KindGC})
		require.NoError(t, err)
		require.Equal(t, repo, mustGet(t, q, id).Repo)
		require.Equal(t, uuid.Nil, mustGet(t, q, instance).Repo)
		list, _, err := q.List(ctx, jobs.Filter{})
		require.NoError(t, err)
		require.Len(t, list, 2)
		require.Equal(t, repo, list[1].Repo)

		require.NoError(t, q.Cancel(ctx, id))
		retried, err := q.Retry(ctx, id)
		require.NoError(t, err)
		require.Equal(t, repo, mustGet(t, q, retried).Repo, "a retry stays scoped to the Repo")
	})
}
