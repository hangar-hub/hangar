package jobs_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
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

	"github.com/hangar-hub/hangar/internal/db"
	"github.com/hangar-hub/hangar/internal/db/dbtest"
	"github.com/hangar-hub/hangar/internal/jobs"
	"github.com/hangar-hub/hangar/internal/telemetry"
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
		require.Equal(t, "The Job stopped because of an internal error in Hangar. The server log has the details.", mustGet(t, q, verify).Error)

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

// TestWorkerFencesStaleRunOfSameOwner covers a late lease renewal: the
// Job's lease expires, the same worker recovers it and claims it again while
// the first run is still going. The first run must lose the Job at its next
// write, whether that is a progress write or its final result, and must not
// overwrite what the second run records.
func TestWorkerFencesStaleRunOfSameOwner(t *testing.T) {
	for _, tc := range []struct {
		name string
		// staleWrites makes the first run report progress and set a result
		// before it returns; otherwise it returns straight away.
		staleWrites bool
	}{{"progress and result writes", true}, {"finish only", false}} {
		t.Run(tc.name, func(t *testing.T) {
			dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
				ctx := context.Background()
				clk := newClock()
				gates := map[int]chan struct{}{1: make(chan struct{}), 2: make(chan struct{})}
				started := make(chan int, 2)
				type staleOutcome struct {
					reportErr, resultErr, cause error
				}
				stale := make(chan staleOutcome, 1)
				reg := jobs.NewRegistry()
				require.NoError(t, reg.Register(jobs.KindIndex, func(ctx context.Context, j *jobs.Job, p jobs.Progress) error {
					started <- j.Attempts
					<-gates[j.Attempts]
					if j.Attempts == 1 {
						var out staleOutcome
						if tc.staleWrites {
							out.reportErr = p.Report(ctx, jobs.ProgressUpdate{Message: "stale run"})
							out.resultErr = p.SetResult(ctx, "stale")
							out.cause = context.Cause(ctx)
						}
						stale <- out
						return nil
					}
					if err := p.SetResult(ctx, "fresh"); err != nil {
						return err
					}
					return p.Report(ctx, jobs.ProgressUpdate{Message: "fresh run"})
				}, jobs.HandlerOptions{MaxAttempts: 3}))
				// The lease is far longer than the test, so the worker never
				// renews it in real time; the fake clock makes it expire.
				finished := make(chan string, 2)
				w := jobs.NewWorker(d, reg, jobs.WorkerOptions{Owner: "a", Lease: time.Hour, Now: clk.Now,
					Logger: slog.New(finishedOutcomes{finished})})
				id, err := w.Queue().Enqueue(ctx, jobs.NewJob{Kind: jobs.KindIndex})
				require.NoError(t, err)

				w.Tick(ctx)
				require.Equal(t, 1, <-started)
				clk.Add(time.Hour + time.Second)
				w.Tick(ctx) // recovers the expired lease, then claims the Job again
				require.Equal(t, 2, <-started)
				j := mustGet(t, w.Queue(), id)
				require.Equal(t, jobs.StatusRunning, j.Status)
				require.Equal(t, 2, j.Attempts)

				// The first run wakes up while the second still holds the Job.
				close(gates[1])
				out := <-stale
				if tc.staleWrites {
					require.ErrorIs(t, out.reportErr, jobs.ErrLeaseLost)
					require.ErrorIs(t, out.resultErr, jobs.ErrLeaseLost)
					require.ErrorIs(t, out.cause, jobs.ErrLeaseLost, "the stale run's context is cancelled")
				}
				select {
				case outcome := <-finished:
					require.Equal(t, "lost", outcome)
				case <-time.After(5 * time.Second):
					t.Fatal("the stale run did not finish")
				}
				j = mustGet(t, w.Queue(), id)
				require.Equal(t, jobs.StatusRunning, j.Status, "the stale run's result does not finish the Job")
				require.Empty(t, j.Progress.Message)
				require.NotContains(t, string(j.Result), "stale")

				close(gates[2])
				w.Wait()
				require.Equal(t, "succeeded", <-finished)
				j = mustGet(t, w.Queue(), id)
				require.Equal(t, jobs.StatusSucceeded, j.Status)
				require.Equal(t, 2, j.Attempts)
				require.JSONEq(t, `"fresh"`, string(j.Result))
				require.Equal(t, "fresh run", j.Progress.Message)
			})
		})
	}
}

// finishedOutcomes is a slog handler that sends the outcome of every "job
// finished" record the worker logs.
type finishedOutcomes struct{ out chan<- string }

func (finishedOutcomes) Enabled(context.Context, slog.Level) bool { return true }
func (h finishedOutcomes) WithAttrs([]slog.Attr) slog.Handler     { return h }
func (h finishedOutcomes) WithGroup(string) slog.Handler          { return h }
func (h finishedOutcomes) Handle(_ context.Context, r slog.Record) error {
	if r.Message != "job finished" {
		return nil
	}
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "outcome" {
			h.out <- a.Value.String()
			return false
		}
		return true
	})
	return nil
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

func TestWorkerPerProcessKindLimit(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		ctx := context.Background()
		release := make(chan struct{})
		var concurrent, peak atomic.Int32
		block := func(context.Context, *jobs.Job, jobs.Progress) error {
			n := concurrent.Add(1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			<-release
			concurrent.Add(-1)
			return nil
		}
		reg := jobs.NewRegistry()
		require.NoError(t, reg.Register(jobs.KindGC, block, jobs.HandlerOptions{Concurrency: 1}))
		require.NoError(t, reg.Register(jobs.KindIndex, func(context.Context, *jobs.Job, jobs.Progress) error {
			<-release
			return nil
		}, jobs.HandlerOptions{}))
		// pull.file has a per-process limit of 2 and an Instance-wide limit
		// of 3: the tighter one applies.
		require.NoError(t, reg.Register(jobs.KindPullFile, func(context.Context, *jobs.Job, jobs.Progress) error {
			<-release
			return nil
		}, jobs.HandlerOptions{Concurrency: 2}))
		w := jobs.NewWorker(d, reg, jobs.WorkerOptions{Concurrency: 10, InstanceLimits: map[jobs.Kind]int{jobs.KindPullFile: 3}})
		q := w.Queue()
		for i := range 3 {
			_, err := q.Enqueue(ctx, jobs.NewJob{Kind: jobs.KindGC, DedupeKey: fmt.Sprint("gc", i)})
			require.NoError(t, err)
			_, err = q.Enqueue(ctx, jobs.NewJob{Kind: jobs.KindIndex})
			require.NoError(t, err)
			_, err = q.Enqueue(ctx, jobs.NewJob{Kind: jobs.KindPullFile})
			require.NoError(t, err)
		}
		running := func() map[string]int {
			var rows []struct {
				Kind string `bun:"kind"`
				N    int    `bun:"n"`
			}
			require.NoError(t, d.NewSelect().Model((*db.Job)(nil)).ColumnExpr("kind, COUNT(*) AS n").
				Where("status = ?", "running").GroupExpr("kind").Scan(ctx, &rows))
			out := map[string]int{}
			for _, r := range rows {
				out[r.Kind] = r.N
			}
			return out
		}

		w.Tick(ctx)
		w.Tick(ctx)
		require.Equal(t, map[string]int{"gc": 1, "index": 3, "pull.file": 2}, running(),
			"one Tick claims up to each kind's per-process limit and fills the other slots")

		close(release)
		w.Wait()
		for range 5 {
			w.Tick(ctx)
			w.Wait()
		}
		n, err := d.NewSelect().Model((*db.Job)(nil)).Where("status = ?", "succeeded").Count(ctx)
		require.NoError(t, err)
		require.Equal(t, 9, int(n), "every Job ran in the end")
		require.Equal(t, int32(1), peak.Load(), "gc never ran more than once at a time in this process")
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
		// The next renewal notices the cancel, a third of the lease after
		// the last one. The lease is long enough that a slow renewal on a
		// loaded machine can't bring the lease watchdog in first, which
		// stops the run with ErrLeaseLost instead.
		w := jobs.NewWorker(d, reg, jobs.WorkerOptions{Lease: 3 * time.Second})
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

// failLeaseRenewals makes every write that extends a running Job's lease
// fail with a database error, as renewals do when the database is busy or
// cannot be reached. Claims and the final write still work.
func failLeaseRenewals(t *testing.T, d *db.DB) {
	t.Helper()
	ctx := context.Background()
	stmts := []string{`CREATE TRIGGER fail_lease_renewal BEFORE UPDATE OF lease_expires_at ON jobs
		WHEN OLD.status = 'running' AND NEW.status = 'running' AND NEW.lease_expires_at IS NOT NULL
		BEGIN SELECT RAISE(ABORT, 'renewal refused'); END`}
	if !d.IsSQLite() {
		stmts = []string{
			`CREATE FUNCTION fail_lease_renewal() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'renewal refused'; END $$`,
			`CREATE TRIGGER fail_lease_renewal BEFORE UPDATE OF lease_expires_at ON jobs FOR EACH ROW
				WHEN (OLD.status = 'running' AND NEW.status = 'running' AND NEW.lease_expires_at IS NOT NULL)
				EXECUTE FUNCTION fail_lease_renewal()`,
		}
	}
	for _, s := range stmts {
		_, err := d.ExecContext(ctx, s)
		require.NoError(t, err)
	}
}

// TestWorkerStopsHandlerWhenLeaseCannotBeRenewed checks that a run whose
// lease renewals keep failing is stopped before the lease runs out, so it
// never overlaps the run RecoverExpired then starts.
func TestWorkerStopsHandlerWhenLeaseCannotBeRenewed(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		ctx := context.Background()
		failLeaseRenewals(t, d)
		started := make(chan struct{})
		type stop struct {
			cause error
			at    time.Time
		}
		stopped := make(chan stop, 1)
		reg := jobs.NewRegistry()
		require.NoError(t, reg.Register(jobs.KindPull, func(ctx context.Context, _ *jobs.Job, _ jobs.Progress) error {
			close(started)
			select {
			case <-ctx.Done():
				stopped <- stop{cause: context.Cause(ctx), at: db.Now()}
			case <-time.After(30 * time.Second):
				stopped <- stop{}
			}
			return ctx.Err()
		}, jobs.HandlerOptions{}))
		w := jobs.NewWorker(d, reg, jobs.WorkerOptions{Lease: 1200 * time.Millisecond})
		id, err := w.Queue().Enqueue(ctx, jobs.NewJob{Kind: jobs.KindPull})
		require.NoError(t, err)
		w.Tick(ctx)
		<-started
		var row db.Job
		require.NoError(t, d.NewSelect().Model(&row).Where("id = ?", id).Scan(ctx))
		require.NotNil(t, row.LeaseExpiresAt)

		s := <-stopped
		require.ErrorIs(t, s.cause, jobs.ErrLeaseLost, "the handler is stopped when its lease cannot be renewed")
		require.True(t, s.at.Before(*row.LeaseExpiresAt), "stopped at %s, before the lease ran out at %s", s.at, row.LeaseExpiresAt)
		w.Wait()
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

// presignedURL is a URL whose query is a credential, as a presigned S3 GET
// is, with a password in its user information as well.
const presignedURL = "https://hangar:hunter2-secret@bucket.s3.example.com/lfs/sha256/ab/cd?X-Amz-Credential=AKIAEXAMPLE&X-Amz-Signature=deadbeefsignature"

// leaksSecret reports which secret part of presignedURL s contains.
func leaksSecret(s string) string {
	for _, secret := range []string{"hunter2-secret", "AKIAEXAMPLE", "deadbeefsignature", "X-Amz-"} {
		if strings.Contains(s, secret) {
			return secret
		}
	}
	return ""
}

func TestWorkerRedactsURLsInErrors(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		ctx := context.Background()
		rec := tracetest.NewSpanRecorder()
		tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
		var logs bytes.Buffer
		reg := jobs.NewRegistry()
		// A handler that fails fetching a presigned URL returns a wrapped
		// *url.Error, whose text holds the whole URL.
		fail := func(context.Context, *jobs.Job, jobs.Progress) error {
			return fmt.Errorf("fetch object: %w", &url.Error{Op: "Get", URL: presignedURL, Err: errors.New("connection refused")})
		}
		require.NoError(t, reg.Register(jobs.KindIndex, fail, jobs.HandlerOptions{MaxAttempts: 3}))
		require.NoError(t, reg.Register(jobs.KindScan, func(ctx context.Context, j *jobs.Job, p jobs.Progress) error {
			return jobs.Permanent(fail(ctx, j, p))
		}, jobs.HandlerOptions{}))
		w := jobs.NewWorker(d, reg, jobs.WorkerOptions{Tracer: tp.Tracer("test"), Logger: slog.New(slog.NewJSONHandler(&logs, nil))})
		retried, err := w.Queue().Enqueue(ctx, jobs.NewJob{Kind: jobs.KindIndex})
		require.NoError(t, err)
		failed, err := w.Queue().Enqueue(ctx, jobs.NewJob{Kind: jobs.KindScan})
		require.NoError(t, err)
		w.Tick(ctx)
		w.Wait()

		want := `fetch object: Get "https://bucket.s3.example.com/lfs/sha256/ab/cd?redacted": connection refused`
		for _, id := range []uuid.UUID{retried, failed} {
			j := mustGet(t, w.Queue(), id)
			require.Equal(t, want, j.Error, "the stored error keeps the host and path only")
		}
		require.Equal(t, jobs.StatusPending, mustGet(t, w.Queue(), retried).Status)
		require.Equal(t, jobs.StatusFailed, mustGet(t, w.Queue(), failed).Status)

		require.Contains(t, logs.String(), "job finished")
		require.Contains(t, logs.String(), "bucket.s3.example.com/lfs/sha256/ab/cd?redacted")
		require.Empty(t, leaksSecret(logs.String()), "the log holds a secret")

		events := 0
		for _, sp := range rec.Ended() {
			for _, ev := range sp.Events() {
				for _, a := range ev.Attributes {
					require.Empty(t, leaksSecret(a.Value.String()), "span %s event %s attribute %s", sp.Name(), ev.Name, a.Key)
					if a.Key == "exception.message" {
						events++
						require.Equal(t, want, a.Value.AsString())
					}
				}
			}
			for _, a := range sp.Attributes() {
				require.Empty(t, leaksSecret(a.Value.String()), "span %s attribute %s", sp.Name(), a.Key)
			}
			require.Empty(t, leaksSecret(sp.Status().Description))
		}
		require.Equal(t, 2, events, "both failures record an exception event")
	})
}

func TestWorkerKeepsPanicStacksOutOfJobs(t *testing.T) {
	ctx := context.Background()
	d := dbtest.SQLite(t)
	var logs bytes.Buffer
	reg := jobs.NewRegistry()
	require.NoError(t, reg.Register(jobs.KindIndex, func(context.Context, *jobs.Job, jobs.Progress) error {
		panic("cannot read " + presignedURL)
	}, jobs.HandlerOptions{MaxAttempts: 1}))
	w := jobs.NewWorker(d, reg, jobs.WorkerOptions{Logger: slog.New(slog.NewJSONHandler(&logs, nil))})
	id, err := w.Queue().Enqueue(ctx, jobs.NewJob{Kind: jobs.KindIndex})
	require.NoError(t, err)
	w.Tick(ctx)
	w.Wait()

	j := mustGet(t, w.Queue(), id)
	require.Equal(t, jobs.StatusFailed, j.Status)
	require.Equal(t, "The Job stopped because of an internal error in Hangar. The server log has the details.", j.Error)
	require.NotContains(t, j.Error, "goroutine")
	require.NotContains(t, j.Error, ".go:")

	var panicked map[string]any
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var rec map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &rec))
		if rec["msg"] == "job handler panicked" {
			panicked = rec
		}
	}
	require.NotNil(t, panicked, "the panic is logged server-side")
	require.Equal(t, "ERROR", panicked["level"])
	require.Equal(t, id.String(), panicked["job_id"])
	require.Contains(t, panicked["stack"], "goroutine", "the log has the stack")
	require.Contains(t, panicked["stack"], "runtime/debug.Stack")
	require.Equal(t, "cannot read https://bucket.s3.example.com/lfs/sha256/ab/cd?redacted", panicked["panic"])
	require.Empty(t, leaksSecret(logs.String()), "the log holds a secret")
}
