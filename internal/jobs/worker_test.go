package jobs_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/jamesagarside/hangar/internal/db"
	"github.com/jamesagarside/hangar/internal/db/dbtest"
	"github.com/jamesagarside/hangar/internal/jobs"
	"github.com/jamesagarside/hangar/internal/telemetry"
)

func mustGet(t *testing.T, q jobs.Queue, id uuid.UUID) *jobs.Job {
	t.Helper()
	j, err := q.Get(context.Background(), id)
	require.NoError(t, err)
	return j
}

func TestWorkerRunsByPriorityAndRecordsResult(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		ctx := context.Background()
		reg := jobs.NewRegistry()
		var mu sync.Mutex
		var order []string
		require.NoError(t, reg.Register(jobs.KindIndex, func(ctx context.Context, j *jobs.Job, p jobs.Progress) error {
			var payload struct{ Name string }
			require.NoError(t, j.DecodePayload(&payload))
			mu.Lock()
			order = append(order, payload.Name)
			mu.Unlock()
			require.NoError(t, p.Report(ctx, jobs.ProgressUpdate{BytesDone: 5, BytesTotal: 10, Message: "halfway"}))
			require.NoError(t, p.Report(ctx, jobs.ProgressUpdate{BytesDone: 10, BytesTotal: 10}))
			return p.SetResult(ctx, map[string]string{"done": payload.Name})
		}, jobs.HandlerOptions{}))
		w := jobs.NewWorker(d, reg, jobs.WorkerOptions{Concurrency: 1})
		q := w.Queue()
		low, err := q.Enqueue(ctx, jobs.NewJob{Kind: jobs.KindIndex, Payload: map[string]string{"Name": "low"}, Priority: jobs.PriorityLow})
		require.NoError(t, err)
		high, err := q.Enqueue(ctx, jobs.NewJob{Kind: jobs.KindIndex, Payload: map[string]string{"Name": "high"}, Priority: jobs.PriorityHigh})
		require.NoError(t, err)
		unhandled, err := q.Enqueue(ctx, jobs.NewJob{Kind: jobs.KindScan})
		require.NoError(t, err)
		for range 3 {
			w.Tick(ctx)
			w.Wait()
		}
		require.Equal(t, []string{"high", "low"}, order)
		for _, id := range []uuid.UUID{low, high} {
			j := mustGet(t, q, id)
			require.Equal(t, jobs.StatusSucceeded, j.Status)
			require.Equal(t, 1, j.Attempts)
			require.NotNil(t, j.StartedAt)
			require.NotNil(t, j.FinishedAt)
			require.Equal(t, int64(10), j.Progress.BytesDone, "the last report is written when the Job finishes")
		}
		require.JSONEq(t, `{"done":"low"}`, string(mustGet(t, q, low).Result))
		require.Equal(t, jobs.StatusPending, mustGet(t, q, unhandled).Status, "kinds without a handler are not claimed")
	})
}

func TestWorkerRetriesWithBackoffAndFails(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		ctx := context.Background()
		clk := newClock()
		reg := jobs.NewRegistry()
		var calls atomic.Int32
		require.NoError(t, reg.Register(jobs.KindIndex, func(context.Context, *jobs.Job, jobs.Progress) error {
			if calls.Add(1) < 3 {
				return errors.New("upstream timed out")
			}
			return nil
		}, jobs.HandlerOptions{MaxAttempts: 5}))
		require.NoError(t, reg.Register(jobs.KindScan, func(context.Context, *jobs.Job, jobs.Progress) error {
			return errors.New("always broken")
		}, jobs.HandlerOptions{MaxAttempts: 2, Backoff: func(int) time.Duration { return time.Minute }}))
		require.NoError(t, reg.Register(jobs.KindGC, func(context.Context, *jobs.Job, jobs.Progress) error {
			return jobs.Permanent(errors.New("bad payload"))
		}, jobs.HandlerOptions{}))
		require.NoError(t, reg.Register(jobs.KindLFSVerify, func(context.Context, *jobs.Job, jobs.Progress) error {
			panic("boom")
		}, jobs.HandlerOptions{MaxAttempts: 1}))
		w := jobs.NewWorker(d, reg, jobs.WorkerOptions{Now: clk.Now})
		q := w.Queue()
		idx, _ := q.Enqueue(ctx, jobs.NewJob{Kind: jobs.KindIndex})
		scan, _ := q.Enqueue(ctx, jobs.NewJob{Kind: jobs.KindScan})
		gc, _ := q.Enqueue(ctx, jobs.NewJob{Kind: jobs.KindGC})
		verify, _ := q.Enqueue(ctx, jobs.NewJob{Kind: jobs.KindLFSVerify})

		w.Tick(ctx)
		w.Wait()
		j := mustGet(t, q, idx)
		require.Equal(t, jobs.StatusPending, j.Status)
		require.Equal(t, 1, j.Attempts)
		require.Equal(t, "upstream timed out", j.Error)
		require.Equal(t, clk.Now().Add(30*time.Second), j.RunAfter, "the default backoff starts at 30 seconds")
		require.Equal(t, jobs.StatusFailed, mustGet(t, q, gc).Status, "permanent errors are not retried")
		require.Equal(t, "bad payload", mustGet(t, q, gc).Error)
		require.Equal(t, jobs.StatusFailed, mustGet(t, q, verify).Status)
		require.Contains(t, mustGet(t, q, verify).Error, "handler panicked: boom")

		// Not due yet: nothing runs.
		w.Tick(ctx)
		w.Wait()
		require.Equal(t, int32(1), calls.Load(), "only the first index attempt has run")

		clk.Add(time.Minute)
		w.Tick(ctx)
		w.Wait()
		require.Equal(t, jobs.StatusFailed, mustGet(t, q, scan).Status, "the second attempt was the last")
		require.Equal(t, 2, mustGet(t, q, scan).Attempts)
		require.Equal(t, 2, mustGet(t, q, idx).Attempts)
		clk.Add(2 * time.Minute)
		w.Tick(ctx)
		w.Wait()
		j = mustGet(t, q, idx)
		require.Equal(t, jobs.StatusSucceeded, j.Status)
		require.Equal(t, 3, j.Attempts)
		require.Empty(t, j.Error)
	})
}

func TestWorkerLeaseExpiryHandsJobToAnotherWorker(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		ctx := context.Background()
		clk := newClock()
		release := make(chan struct{})
		var runs atomic.Int32
		var owners sync.Map
		handler := func(owner string) jobs.Handler {
			return func(ctx context.Context, j *jobs.Job, _ jobs.Progress) error {
				runs.Add(1)
				owners.Store(owner, j.Attempts)
				if owner == "a" {
					<-release // A hangs and never renews (its lease is longer than the test).
				}
				return nil
			}
		}
		newWorker := func(owner string) *jobs.Worker {
			reg := jobs.NewRegistry()
			require.NoError(t, reg.Register(jobs.KindIndex, handler(owner), jobs.HandlerOptions{MaxAttempts: 3}))
			return jobs.NewWorker(d, reg, jobs.WorkerOptions{Owner: owner, Lease: time.Hour, Now: clk.Now})
		}
		a, b := newWorker("a"), newWorker("b")
		id, err := a.Queue().Enqueue(ctx, jobs.NewJob{Kind: jobs.KindIndex})
		require.NoError(t, err)
		a.Tick(ctx)
		require.Eventually(t, func() bool { return runs.Load() == 1 }, 5*time.Second, 10*time.Millisecond)
		require.Equal(t, jobs.StatusRunning, mustGet(t, a.Queue(), id).Status)

		// Before the lease runs out, B leaves the Job alone.
		b.Tick(ctx)
		b.Wait()
		require.Equal(t, int32(1), runs.Load())

		clk.Add(time.Hour + time.Second)
		b.Tick(ctx) // recovers the expired lease, then claims the Job
		b.Wait()
		j := mustGet(t, b.Queue(), id)
		require.Equal(t, jobs.StatusSucceeded, j.Status)
		require.Equal(t, 2, j.Attempts)
		att, _ := owners.Load("b")
		require.Equal(t, 2, att)

		// A wakes up: its result is discarded because it lost the lease.
		close(release)
		a.Wait()
		require.Equal(t, jobs.StatusSucceeded, mustGet(t, a.Queue(), id).Status)
		require.Equal(t, 2, mustGet(t, a.Queue(), id).Attempts)
	})
}

func TestWorkerLeaseExpiryOnLastAttemptFails(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		ctx := context.Background()
		clk := newClock()
		block := make(chan struct{})
		defer close(block)
		reg := jobs.NewRegistry()
		require.NoError(t, reg.Register(jobs.KindIndex, func(ctx context.Context, _ *jobs.Job, _ jobs.Progress) error {
			<-block
			return nil
		}, jobs.HandlerOptions{MaxAttempts: 1}))
		reader := sdkmetric.NewManualReader()
		inst, err := telemetry.NewInstruments(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("test"))
		require.NoError(t, err)
		w := jobs.NewWorker(d, reg, jobs.WorkerOptions{Owner: "a", Lease: time.Hour, Now: clk.Now, Instrument: inst})
		id, _ := w.Queue().Enqueue(ctx, jobs.NewJob{Kind: jobs.KindIndex})
		w.Tick(ctx)
		require.Eventually(t, func() bool { return mustGet(t, w.Queue(), id).Status == jobs.StatusRunning }, 5*time.Second, 10*time.Millisecond)
		clk.Add(2 * time.Hour)
		require.NoError(t, w.RecoverExpired(ctx))
		j := mustGet(t, w.Queue(), id)
		require.Equal(t, jobs.StatusFailed, j.Status)
		require.Contains(t, j.Error, "stopped renewing its lease")

		var rm metricdata.ResourceMetrics
		require.NoError(t, reader.Collect(ctx, &rm))
		require.Equal(t, int64(1), sumCounter(rm, "hangar.jobs.lease.expired"))
	})
}

func sumCounter(rm metricdata.ResourceMetrics, name string) int64 {
	var n int64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			if s, ok := m.Data.(metricdata.Sum[int64]); ok {
				for _, dp := range s.DataPoints {
					n += dp.Value
				}
			}
		}
	}
	return n
}

func TestTwoWorkersRunEachJobOnce(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		ctx := context.Background()
		var runs sync.Map
		var dupes atomic.Int32
		var concurrent, peak atomic.Int32
		newWorker := func(owner string) *jobs.Worker {
			reg := jobs.NewRegistry()
			require.NoError(t, reg.Register(jobs.KindIndex, func(ctx context.Context, j *jobs.Job, _ jobs.Progress) error {
				if _, loaded := runs.LoadOrStore(j.ID, owner); loaded {
					dupes.Add(1)
				}
				return nil
			}, jobs.HandlerOptions{}))
			require.NoError(t, reg.Register(jobs.KindPullFile, func(ctx context.Context, j *jobs.Job, _ jobs.Progress) error {
				n := concurrent.Add(1)
				for {
					p := peak.Load()
					if n <= p || peak.CompareAndSwap(p, n) {
						break
					}
				}
				time.Sleep(20 * time.Millisecond)
				concurrent.Add(-1)
				runs.Store(j.ID, owner)
				return nil
			}, jobs.HandlerOptions{}))
			return jobs.NewWorker(d, reg, jobs.WorkerOptions{Owner: owner, Concurrency: 4, InstanceLimits: map[jobs.Kind]int{jobs.KindPullFile: 2}})
		}
		a, b := newWorker("a"), newWorker("b")
		var ids []uuid.UUID
		for i := range 40 {
			kind := jobs.KindIndex
			if i%4 == 0 {
				kind = jobs.KindPullFile
			}
			id, err := a.Queue().Enqueue(ctx, jobs.NewJob{Kind: kind})
			require.NoError(t, err)
			ids = append(ids, id)
		}
		done := func() bool {
			n, err := d.NewSelect().Model((*db.Job)(nil)).Where("status = ?", "succeeded").Count(ctx)
			require.NoError(t, err)
			return int(n) == len(ids)
		}
		var wg sync.WaitGroup
		for _, w := range []*jobs.Worker{a, b} {
			wg.Add(1)
			go func(w *jobs.Worker) {
				defer wg.Done()
				deadline := time.Now().Add(30 * time.Second)
				for !done() && time.Now().Before(deadline) {
					w.Tick(ctx)
					time.Sleep(5 * time.Millisecond)
				}
				w.Wait()
			}(w)
		}
		wg.Wait()
		require.True(t, done(), "every Job ran")
		require.Zero(t, dupes.Load(), "no Job ran twice")
		require.LessOrEqual(t, peak.Load(), int32(2), "pull.file never ran more than the Instance limit at once")
		owners := map[string]int{}
		runs.Range(func(_, v any) bool { owners[v.(string)]++; return true })
		require.Len(t, owners, 2, "both workers took part: %v", owners)
	})
}

func TestWorkerCancelStopsRunningHandler(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		ctx := context.Background()
		started := make(chan struct{})
		var cause atomic.Value
		reg := jobs.NewRegistry()
		require.NoError(t, reg.Register(jobs.KindPull, func(ctx context.Context, _ *jobs.Job, _ jobs.Progress) error {
			close(started)
			<-ctx.Done()
			cause.Store(context.Cause(ctx))
			return ctx.Err()
		}, jobs.HandlerOptions{}))
		w := jobs.NewWorker(d, reg, jobs.WorkerOptions{Lease: 300 * time.Millisecond})
		id, _ := w.Queue().Enqueue(ctx, jobs.NewJob{Kind: jobs.KindPull})
		w.Tick(ctx)
		<-started
		require.NoError(t, w.Queue().Cancel(ctx, id))
		w.Wait()
		require.ErrorIs(t, cause.Load().(error), jobs.ErrCancelled)
		j := mustGet(t, w.Queue(), id)
		require.Equal(t, jobs.StatusCancelled, j.Status)
		require.Equal(t, 1, j.Attempts)
	})
}

func TestWorkerSnoozeKeepsAttempts(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		ctx := context.Background()
		clk := newClock()
		windows, err := jobs.ParseWindows([]string{"01:00-07:00"}, time.UTC)
		require.NoError(t, err)
		reg := jobs.NewRegistry()
		var runs atomic.Int32
		require.NoError(t, reg.Register(jobs.KindPullFile, func(ctx context.Context, _ *jobs.Job, p jobs.Progress) error {
			runs.Add(1)
			now := clk.Now()
			if !windows.Open(now) {
				_ = p.Report(ctx, jobs.ProgressUpdate{BytesDone: 1, BytesTotal: 2})
				return jobs.Snooze(windows.NextOpen(now), windows.WaitMessage())
			}
			return nil
		}, jobs.HandlerOptions{MaxAttempts: 1}))
		w := jobs.NewWorker(d, reg, jobs.WorkerOptions{Now: clk.Now})
		id, _ := w.Queue().Enqueue(ctx, jobs.NewJob{Kind: jobs.KindPullFile})
		w.Tick(ctx)
		w.Wait()
		j := mustGet(t, w.Queue(), id)
		require.Equal(t, jobs.StatusPending, j.Status)
		require.Zero(t, j.Attempts, "a snooze does not use an attempt")
		require.Equal(t, "Waiting for pull window (01:00-07:00)", j.Progress.Message)
		require.Equal(t, int64(1), j.Progress.BytesDone, "progress so far is kept")
		require.Equal(t, time.Date(2026, 10, 14, 1, 0, 0, 0, time.UTC), j.RunAfter)

		clk.Add(13 * time.Hour)
		w.Tick(ctx)
		w.Wait()
		require.Equal(t, jobs.StatusSucceeded, mustGet(t, w.Queue(), id).Status)
		require.Equal(t, int32(2), runs.Load())
	})
}

func TestWorkerShutdownReleasesJobs(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		started := make(chan struct{})
		reg := jobs.NewRegistry()
		require.NoError(t, reg.Register(jobs.KindPull, func(ctx context.Context, _ *jobs.Job, _ jobs.Progress) error {
			close(started)
			<-ctx.Done()
			return ctx.Err()
		}, jobs.HandlerOptions{}))
		w := jobs.NewWorker(d, reg, jobs.WorkerOptions{PollInterval: 10 * time.Millisecond})
		id, _ := w.Queue().Enqueue(context.Background(), jobs.NewJob{Kind: jobs.KindPull})
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error)
		go func() { done <- w.Run(ctx) }()
		<-started
		cancel()
		require.NoError(t, <-done)
		j := mustGet(t, w.Queue(), id)
		require.Equal(t, jobs.StatusPending, j.Status)
		require.Zero(t, j.Attempts, "shutdown does not use an attempt")
	})
}

func TestWorkerTimeout(t *testing.T) {
	d := dbtest.SQLite(t)
	ctx := context.Background()
	reg := jobs.NewRegistry()
	require.NoError(t, reg.Register(jobs.KindZipExport, func(ctx context.Context, _ *jobs.Job, _ jobs.Progress) error {
		<-ctx.Done()
		return ctx.Err()
	}, jobs.HandlerOptions{Timeout: 50 * time.Millisecond, MaxAttempts: 1}))
	w := jobs.NewWorker(d, reg, jobs.WorkerOptions{})
	id, _ := w.Queue().Enqueue(ctx, jobs.NewJob{Kind: jobs.KindZipExport})
	w.Tick(ctx)
	w.Wait()
	j := mustGet(t, w.Queue(), id)
	require.Equal(t, jobs.StatusFailed, j.Status)
	require.Contains(t, j.Error, "deadline exceeded")
}

func TestSchedulesFireOncePerRun(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		ctx := context.Background()
		clk := newClock()
		reg := jobs.NewRegistry()
		a := jobs.NewWorker(d, reg, jobs.WorkerOptions{Owner: "a", Now: clk.Now})
		b := jobs.NewWorker(d, reg, jobs.WorkerOptions{Owner: "b", Now: clk.Now})
		require.NoError(t, a.Queue().SetSchedule(ctx, jobs.Schedule{Kind: jobs.KindGC, Key: "gc", Cron: "*/5 * * * *", Payload: map[string]bool{"full": true}, Enabled: true}))
		require.NoError(t, a.Queue().SetSchedule(ctx, jobs.Schedule{Kind: jobs.KindXetGC, Key: "off", Cron: "* * * * *", Enabled: false}))

		count := func() int {
			n, err := d.NewSelect().Model((*db.Job)(nil)).Count(ctx)
			require.NoError(t, err)
			return int(n)
		}
		require.NoError(t, a.FireSchedules(ctx))
		require.Zero(t, count(), "not due yet")

		clk.Add(5 * time.Minute)
		var wg sync.WaitGroup
		errs := make(chan error, 8)
		for i := range 8 {
			wg.Add(1)
			go func(w *jobs.Worker) {
				defer wg.Done()
				errs <- w.FireSchedules(ctx)
			}([]*jobs.Worker{a, b}[i%2])
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			require.NoError(t, err)
		}
		require.Equal(t, 1, count(), "one worker wins the run")
		list, _, err := a.Queue().List(ctx, jobs.Filter{})
		require.NoError(t, err)
		require.Equal(t, jobs.KindGC, list[0].Kind)
		require.JSONEq(t, `{"full":true}`, string(list[0].Payload))

		// The next run joins the first while it is still pending.
		clk.Add(5 * time.Minute)
		require.NoError(t, b.FireSchedules(ctx))
		require.Equal(t, 1, count())
		require.NoError(t, a.Queue().Cancel(ctx, list[0].ID))
		clk.Add(5 * time.Minute)
		require.NoError(t, b.FireSchedules(ctx))
		require.Equal(t, 2, count())

		sch, err := a.Queue().Schedules(ctx)
		require.NoError(t, err)
		require.Equal(t, clk.Now().Add(5*time.Minute), sch[0].NextRunAt.UTC())
		require.NotNil(t, sch[0].LastRunAt)
	})
}

func TestLeaderElection(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		ctx := context.Background()
		clk := newClock()
		a := jobs.NewLeader(d, "leader:test", "a", time.Minute, clk.Now)
		b := jobs.NewLeader(d, "leader:test", "b", time.Minute, clk.Now)
		ok, err := a.Acquire(ctx)
		require.NoError(t, err)
		require.True(t, ok)
		ok, err = b.Acquire(ctx)
		require.NoError(t, err)
		require.False(t, ok)
		ok, err = a.Acquire(ctx)
		require.NoError(t, err)
		require.True(t, ok, "the holder renews")
		clk.Add(2 * time.Minute)
		ok, err = b.Acquire(ctx)
		require.NoError(t, err)
		require.True(t, ok, "an expired lease passes to another process")
		b.Release(ctx)
		ok, err = a.Acquire(ctx)
		require.NoError(t, err)
		require.True(t, ok)
	})
}

func TestLeaderTasksRunOnOneWorker(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		ctx := context.Background()
		reader := sdkmetric.NewManualReader()
		inst, err := telemetry.NewInstruments(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("test"))
		require.NoError(t, err)
		var ran sync.Map
		newWorker := func(owner string) *jobs.Worker {
			return jobs.NewWorker(d, jobs.NewRegistry(), jobs.WorkerOptions{Owner: owner, LeaderTasks: []func(context.Context) error{
				func(context.Context) error { ran.Store(owner, true); return nil },
				jobs.RecordQueueGauge(d, inst),
			}})
		}
		a, b := newWorker("a"), newWorker("b")
		_, err = a.Queue().Enqueue(ctx, jobs.NewJob{Kind: jobs.KindPull})
		require.NoError(t, err)
		_, err = a.Queue().Enqueue(ctx, jobs.NewJob{Kind: jobs.KindPull})
		require.NoError(t, err)
		a.Tick(ctx)
		b.Tick(ctx)
		_, okA := ran.Load("a")
		_, okB := ran.Load("b")
		require.True(t, okA)
		require.False(t, okB, "only the leader runs leader tasks")

		var rm metricdata.ResourceMetrics
		require.NoError(t, reader.Collect(ctx, &rm))
		found := false
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				if m.Name != "hangar.jobs.queued" {
					continue
				}
				for _, dp := range m.Data.(metricdata.Gauge[int64]).DataPoints {
					k, _ := dp.Attributes.Value(telemetry.MetricKind)
					s, _ := dp.Attributes.Value(telemetry.MetricStatus)
					if k.AsString() == "pull" && s.AsString() == "pending" {
						require.Equal(t, int64(2), dp.Value)
						found = true
					}
				}
			}
		}
		require.True(t, found)
	})
}

func TestJobSpanLinksToEnqueuer(t *testing.T) {
	d := dbtest.SQLite(t)
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	reg := jobs.NewRegistry()
	require.NoError(t, reg.Register(jobs.KindIndex, noop, jobs.HandlerOptions{}))
	w := jobs.NewWorker(d, reg, jobs.WorkerOptions{Tracer: tp.Tracer("test")})
	reqCtx, reqSpan := tp.Tracer("test").Start(context.Background(), "POST /api/x")
	id, err := w.Queue().Enqueue(reqCtx, jobs.NewJob{Kind: jobs.KindIndex})
	require.NoError(t, err)
	reqSpan.End()
	w.Tick(context.Background())
	w.Wait()
	var jobSpan sdktrace.ReadOnlySpan
	for _, s := range rec.Ended() {
		if s.Name() == "job index" {
			jobSpan = s
		}
	}
	require.NotNil(t, jobSpan)
	require.Len(t, jobSpan.Links(), 1)
	require.Equal(t, reqSpan.SpanContext().TraceID(), jobSpan.Links()[0].SpanContext.TraceID())
	attrs := map[string]string{}
	for _, a := range jobSpan.Attributes() {
		attrs[string(a.Key)] = a.Value.String()
	}
	require.Equal(t, id.String(), attrs["hangar.job.id"])
	require.Equal(t, "succeeded", attrs["hangar.job.outcome"])
}
