package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"os"
	"runtime/debug"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/jamesagarside/hangar/internal/db"
	"github.com/jamesagarside/hangar/internal/telemetry"
)

// Errors a running handler can see.
var (
	// ErrLeaseLost means another run took the Job over (another worker,
	// or this worker again after its lease expired), or it was cancelled;
	// the handler's context is cancelled with this cause.
	ErrLeaseLost = errors.New("job lease lost")
	// ErrCancelled is the cause of a handler's context when a user cancels
	// the Job.
	ErrCancelled = errors.New("job cancelled")
)

// snoozeError asks the worker to run the Job again later without counting
// an attempt.
type snoozeError struct {
	until  time.Time
	reason string
}

func (e *snoozeError) Error() string {
	return "snoozed until " + e.until.Format(time.RFC3339) + ": " + e.reason
}

// Snooze returns an error that puts the Job back in the queue until the
// given time without using an attempt, with reason as its progress
// message. Pull transfers use it to wait for a pull window ("Waiting for
// pull window (01:00-07:00)").
func Snooze(until time.Time, reason string) error { return &snoozeError{until: until, reason: reason} }

// WorkerOptions configure a Worker.
type WorkerOptions struct {
	// Owner identifies this process in leases. Empty means
	// "<hostname>:<pid>:<random>".
	Owner string
	// Concurrency is how many Jobs run at once in this process (jobs.workers).
	// Zero means 4.
	Concurrency int
	// Lease is the lease length (jobs.lease). Zero means 60 seconds. The
	// worker renews leases every third of it.
	Lease time.Duration
	// PollInterval is how often the worker looks for Jobs and due
	// schedules. Zero means one second.
	PollInterval time.Duration
	// InstanceLimits caps running Jobs of a kind across the whole Instance,
	// for example pull.file at pull.concurrency.
	InstanceLimits map[Kind]int
	// LeaderTasks run every LeaderInterval on exactly one worker of the
	// Instance, such as computing gauges that need a database count.
	LeaderTasks []func(ctx context.Context) error
	// LeaderInterval is how often leader tasks run. Zero means 60 seconds.
	LeaderInterval time.Duration
	// Location is the time zone of schedules. Nil means UTC.
	Location *time.Location
	// Now is the clock. Nil means db.Now.
	Now        func() time.Time
	Logger     *slog.Logger
	Tracer     trace.Tracer
	Instrument *telemetry.Instruments
}

// Worker claims Jobs from the queue and runs their handlers: it renews
// leases, retries failures with backoff, returns Jobs whose lease expired to
// the queue, fires due schedules and runs leader tasks.
type Worker struct {
	db    *db.DB
	reg   *Registry
	queue *DBQueue
	opts  WorkerOptions
	log   *slog.Logger
	inst  *telemetry.Instruments
	trace trace.Tracer

	mu       sync.Mutex
	running  map[Kind]int
	total    int
	wg       sync.WaitGroup
	wake     chan struct{}
	leader   *Leader
	lastLead time.Time
}

// NewWorker returns a worker for the kinds registered in reg.
func NewWorker(d *db.DB, reg *Registry, opts WorkerOptions) *Worker {
	if opts.Owner == "" {
		host, _ := os.Hostname()
		opts.Owner = fmt.Sprintf("%s:%d:%s", host, os.Getpid(), uuid.NewString()[:8])
	}
	if opts.Concurrency <= 0 {
		opts.Concurrency = 4
	}
	if opts.Lease <= 0 {
		opts.Lease = 60 * time.Second
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = time.Second
	}
	if opts.LeaderInterval <= 0 {
		opts.LeaderInterval = 60 * time.Second
	}
	if opts.Now == nil {
		opts.Now = db.Now
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Tracer == nil {
		opts.Tracer = noop.NewTracerProvider().Tracer("hangar")
	}
	w := &Worker{
		db: d, reg: reg, opts: opts, log: opts.Logger.With("worker", opts.Owner), inst: opts.Instrument, trace: opts.Tracer,
		queue:   NewQueue(d, reg, QueueOptions{Location: opts.Location, Now: opts.Now}),
		running: map[Kind]int{}, wake: make(chan struct{}, 1),
	}
	w.leader = NewLeader(d, "leader:jobs", opts.Owner, 3*opts.LeaderInterval/2, opts.Now)
	return w
}

// Owner returns the worker's lease owner name.
func (w *Worker) Owner() string { return w.opts.Owner }

// Queue returns a queue that shares the worker's clock and registry.
func (w *Worker) Queue() *DBQueue { return w.queue }

func (w *Worker) now() time.Time { return w.opts.Now().UTC().Truncate(time.Microsecond) }

// Wake makes the worker look for Jobs now rather than at the next poll.
func (w *Worker) Wake() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// Run works until ctx ends. Then it cancels running handlers, returns
// their Jobs to the queue without using an attempt, and waits for them.
func (w *Worker) Run(ctx context.Context) error {
	ticker := time.NewTicker(w.opts.PollInterval)
	defer ticker.Stop()
	for {
		w.Tick(ctx)
		select {
		case <-ctx.Done():
			w.wg.Wait()
			w.leader.Release(context.WithoutCancel(ctx))
			return nil
		case <-ticker.C:
		case <-w.wake:
		}
	}
}

// Tick does one round of work: lease recovery, schedules, leader tasks
// and claims. Run calls it every poll; tests call it directly.
func (w *Worker) Tick(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	if err := w.RecoverExpired(ctx); err != nil && ctx.Err() == nil {
		w.log.WarnContext(ctx, "recover expired job leases", "error", err)
	}
	if err := w.FireSchedules(ctx); err != nil && ctx.Err() == nil {
		w.log.WarnContext(ctx, "fire schedules", "error", err)
	}
	w.runLeaderTasks(ctx)
	if err := w.claimAndStart(ctx); err != nil && ctx.Err() == nil {
		w.log.WarnContext(ctx, "claim jobs", "error", err)
	}
}

// Wait blocks until every handler this worker started has finished.
func (w *Worker) Wait() { w.wg.Wait() }

// RecoverExpired returns running Jobs whose lease has expired to the
// queue, or fails them if they have used every attempt. Any worker may
// run it; the conditional update makes it safe to run concurrently.
func (w *Worker) RecoverExpired(ctx context.Context) error {
	now := w.now()
	type expired struct {
		Kind   string `bun:"kind"`
		Status string `bun:"status"`
	}
	// Jobs that have used every attempt fail; the rest go back to the queue.
	var rows, requeued []expired
	err := w.db.NewRaw(`UPDATE jobs SET status = 'failed', error = ?, finished_at = ?,
		lease_owner = NULL, lease_expires_at = NULL, updated_at = ?
		WHERE status = 'running' AND lease_expires_at < ? AND attempts >= max_attempts
		RETURNING kind, status`,
		"The worker running this Job stopped renewing its lease, and the Job has used every attempt.", now, now, now).
		Scan(ctx, &rows)
	if err != nil && !db.IsNotFound(err) {
		return err
	}
	err = w.db.NewRaw(`UPDATE jobs SET status = 'pending', run_after = ?,
		lease_owner = NULL, lease_expires_at = NULL, updated_at = ?
		WHERE status = 'running' AND lease_expires_at < ?
		RETURNING kind, status`, now, now, now).
		Scan(ctx, &requeued)
	if err != nil && !db.IsNotFound(err) {
		return err
	}
	rows = append(rows, requeued...)
	for _, r := range rows {
		w.log.WarnContext(ctx, "job lease expired", "kind", r.Kind, "status", r.Status)
		if w.inst != nil {
			w.inst.JobsLeaseExpired.Add(ctx, 1, metric.WithAttributes(attribute.String(telemetry.MetricKind, r.Kind)))
		}
	}
	return nil
}

// FireSchedules enqueues a Job for every schedule whose next run has
// passed. A compare-and-swap on next_run_at makes exactly one worker win
// each run; the Job's dedupe key joins a run that is still going.
func (w *Worker) FireSchedules(ctx context.Context) error {
	now := w.now()
	var due []db.Schedule
	if err := w.db.NewSelect().Model(&due).Where("enabled = ? AND next_run_at <= ?", true, now).
		OrderExpr("next_run_at").Limit(100).Scan(ctx); err != nil {
		return err
	}
	for _, s := range due {
		next, err := NextRun(s.Cron, w.queue.loc, now)
		if err != nil {
			w.log.WarnContext(ctx, "schedule has an invalid cron expression", "kind", s.Kind, "key", s.Key, "error", err)
			continue
		}
		err = w.db.InTx(ctx, func(ctx context.Context, tx bun.Tx) error {
			res, err := tx.NewUpdate().Model((*db.Schedule)(nil)).
				Set("next_run_at = ?", next).Set("last_run_at = ?", now).Set("updated_at = ?", now).
				Where("id = ? AND next_run_at = ?", s.ID, s.NextRunAt).Exec(ctx)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n != 1 {
				return nil // Another worker fired this run.
			}
			_, err = w.queue.EnqueueTx(ctx, tx, NewJob{Kind: Kind(s.Kind), DedupeKey: "schedule:" + s.Key, Payload: s.Payload})
			return err
		})
		if err != nil {
			return fmt.Errorf("fire schedule %s %s: %w", s.Kind, s.Key, err)
		}
	}
	return nil
}

func (w *Worker) runLeaderTasks(ctx context.Context) {
	if len(w.opts.LeaderTasks) == 0 {
		return
	}
	now := w.now()
	w.mu.Lock()
	due := now.Sub(w.lastLead) >= w.opts.LeaderInterval
	w.mu.Unlock()
	if !due {
		return
	}
	ok, err := w.leader.Acquire(ctx)
	if err != nil {
		w.log.WarnContext(ctx, "acquire job leader lease", "error", err)
		return
	}
	w.mu.Lock()
	w.lastLead = now
	w.mu.Unlock()
	if !ok {
		return
	}
	for _, task := range w.opts.LeaderTasks {
		if err := task(ctx); err != nil && ctx.Err() == nil {
			w.log.WarnContext(ctx, "leader task failed", "error", err)
		}
	}
}

// RecordQueueGauge records hangar.jobs.queued for every kind and active
// status. Worker uses it as a leader task when it has instruments.
func RecordQueueGauge(d *db.DB, inst *telemetry.Instruments) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		var rows []struct {
			Kind   string `bun:"kind"`
			Status string `bun:"status"`
			N      int64  `bun:"n"`
		}
		if err := d.NewSelect().Model((*db.Job)(nil)).ColumnExpr("kind, status, COUNT(*) AS n").
			Where("status IN (?)", bun.List(activeStatuses())).GroupExpr("kind, status").Scan(ctx, &rows); err != nil {
			return err
		}
		counts := map[[2]string]int64{}
		for _, r := range rows {
			counts[[2]string{r.Kind, r.Status}] = r.N
		}
		for _, k := range AllKinds() {
			for _, st := range activeStatuses() {
				inst.JobsQueued.Record(ctx, counts[[2]string{string(k), st}], metric.WithAttributes(
					attribute.String(telemetry.MetricKind, string(k)), attribute.String(telemetry.MetricStatus, st)))
			}
		}
		return nil
	}
}

// claimAndStart claims as many Jobs as there are free slots and starts them.
func (w *Worker) claimAndStart(ctx context.Context) error {
	for {
		free, open, limited := w.capacity()
		if free == 0 {
			return nil
		}
		var claimed []db.Job
		if len(open) > 0 {
			rows, err := w.claim(ctx, open, free, nil)
			if err != nil {
				return err
			}
			claimed = append(claimed, rows...)
		}
		for _, s := range limited {
			if len(claimed) >= free {
				break
			}
			var instance *Kind
			if w.opts.InstanceLimits[s.kind] > 0 {
				instance = &s.kind
			}
			rows, err := w.claim(ctx, []string{string(s.kind)}, min(s.room, free-len(claimed)), instance)
			if err != nil {
				return err
			}
			claimed = append(claimed, rows...)
		}
		if len(claimed) == 0 {
			return nil
		}
		for i := range claimed {
			w.start(ctx, &claimed[i])
		}
	}
}

// kindSlots is a kind whose claims are capped, with the number of Jobs of
// it this process may still start.
type kindSlots struct {
	kind Kind
	room int
}

// capacity returns the free slots, the kinds that can be claimed freely
// in one batch, and the kinds capped by a per-process limit
// (HandlerOptions.Concurrency) or an Instance-wide limit, each with the
// room it has left in this process.
func (w *Worker) capacity() (int, []string, []kindSlots) {
	w.mu.Lock()
	defer w.mu.Unlock()
	free := w.opts.Concurrency - w.total
	if free <= 0 {
		return 0, nil, nil
	}
	var open []string
	var limited []kindSlots
	for _, k := range w.reg.Kinds() {
		reg, _ := w.reg.Lookup(k)
		room := free
		if reg.Options.Concurrency > 0 {
			room = min(room, reg.Options.Concurrency-w.running[k])
			if room <= 0 {
				continue
			}
		}
		if reg.Options.Concurrency > 0 || w.opts.InstanceLimits[k] > 0 {
			limited = append(limited, kindSlots{kind: k, room: room})
			continue
		}
		open = append(open, string(k))
	}
	return free, open, limited
}

func advisoryKey(s string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	return int64(h.Sum64()) //nolint:gosec // Any 64-bit value is a valid advisory lock key.
}

// claim marks up to n due pending Jobs of the kinds as running under this
// worker's lease and returns them. With limitKind, it first counts that
// kind's running Jobs across the Instance and claims only up to the limit,
// holding an advisory lock so two workers cannot both take the last slot.
func (w *Worker) claim(ctx context.Context, kinds []string, n int, limitKind *Kind) ([]db.Job, error) {
	var rows []db.Job
	err := w.db.InTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		if limitKind != nil {
			if !w.db.IsSQLite() {
				if _, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(?)", advisoryKey("jobs:"+string(*limitKind))); err != nil {
					return err
				}
			}
			running, err := tx.NewSelect().Model((*db.Job)(nil)).Where("kind = ? AND status = ?", string(*limitKind), string(StatusRunning)).Count(ctx)
			if err != nil {
				return err
			}
			n = min(n, w.opts.InstanceLimits[*limitKind]-int(running))
			if n <= 0 {
				return nil
			}
		}
		now := w.now()
		lock := ""
		if !w.db.IsSQLite() {
			lock = " FOR UPDATE SKIP LOCKED"
		}
		return tx.NewRaw(`UPDATE jobs SET status = 'running', lease_owner = ?, lease_expires_at = ?, attempts = attempts + 1,
			started_at = COALESCE(started_at, ?), updated_at = ?
			WHERE id IN (SELECT id FROM jobs WHERE status = 'pending' AND run_after <= ? AND kind IN (?)
				ORDER BY priority DESC, run_after, id LIMIT ?`+lock+`)
			RETURNING *`,
			w.opts.Owner, now.Add(w.opts.Lease), now, now, now, bun.List(kinds), n).Scan(ctx, &rows)
	})
	if db.IsNotFound(err) {
		return nil, nil
	}
	return rows, err
}

// start runs a claimed Job in its own goroutine.
func (w *Worker) start(ctx context.Context, row *db.Job) {
	job, err := fromRow(row)
	if err != nil {
		w.finish(ctx, job, row.ID, Kind(row.Kind), row.Attempts, Permanent(err), nil, time.Now())
		return
	}
	reg, ok := w.reg.Lookup(job.Kind)
	if !ok {
		return
	}
	w.mu.Lock()
	w.running[job.Kind]++
	w.total++
	w.mu.Unlock()
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		defer func() {
			w.mu.Lock()
			w.running[job.Kind]--
			w.total--
			w.mu.Unlock()
			w.Wake()
		}()
		w.run(ctx, job, reg, row.TraceContext)
	}()
}

func (w *Worker) run(ctx context.Context, job *Job, reg Registration, traceCtx map[string]string) {
	started := time.Now()
	parent := propagation.TraceContext{}.Extract(context.Background(), propagation.MapCarrier(traceCtx))
	var links []trace.Link
	if sc := trace.SpanContextFromContext(parent); sc.IsValid() {
		links = append(links, trace.Link{SpanContext: sc})
	}
	spanCtx, span := w.trace.Start(ctx, "job "+string(job.Kind), trace.WithNewRoot(), trace.WithLinks(links...),
		trace.WithSpanKind(trace.SpanKindConsumer), trace.WithAttributes(
			telemetry.AttrJobKind.String(string(job.Kind)), telemetry.AttrJobID.String(job.ID.String()),
			telemetry.AttrJobAttempt.Int(job.Attempts)))
	defer span.End()

	jobCtx, cancel := context.WithCancelCause(spanCtx)
	defer cancel(nil)
	if reg.Options.Timeout > 0 {
		var cancelTimeout context.CancelFunc
		jobCtx, cancelTimeout = context.WithTimeout(jobCtx, reg.Options.Timeout)
		defer cancelTimeout()
	}
	p := &progress{w: w, id: job.ID, attempt: job.Attempts, cancel: cancel}
	stopRenew := w.renewLease(jobCtx, p)

	err := safeRun(jobCtx, reg.Handler, job, p)
	stopRenew()
	if cause := context.Cause(jobCtx); err != nil && (errors.Is(cause, ErrCancelled) || errors.Is(cause, ErrLeaseLost)) {
		err = cause
	}
	var pe *panicError
	if errors.As(err, &pe) {
		// The stack stays in the server log; the Job's error column, which
		// users read through GET /jobs/{id}, gets panicError's fixed text.
		w.log.ErrorContext(ctx, "job handler panicked", "job_id", job.ID, "kind", job.Kind,
			"panic", RedactURLs(fmt.Sprint(pe.value)), "stack", string(pe.stack))
	}
	outcome := w.finish(ctx, job, job.ID, job.Kind, job.Attempts, err, p, started)
	if outcome == "failed" || outcome == "retry" {
		recordJobError(span, err)
		span.SetStatus(codes.Error, "job failed")
	}
	span.SetAttributes(attribute.String("hangar.job.outcome", outcome))
}

// recordJobError adds an exception event to span like span.RecordError,
// with the error's text passed through RedactError, so a presigned URL or
// a URL password in a handler error never reaches a trace.
func recordJobError(span trace.Span, err error) {
	span.AddEvent(semconv.ExceptionEventName, trace.WithAttributes(
		semconv.ExceptionType(fmt.Sprintf("%T", err)),
		semconv.ExceptionMessage(RedactError(err)),
	))
}

// panicError is a recovered handler panic. Its text is a fixed message for
// users; the worker logs the value and stack.
type panicError struct {
	value any
	stack []byte
}

func (e *panicError) Error() string {
	return "The Job stopped because of an internal error in Hangar. The server log has the details."
}

func safeRun(ctx context.Context, h Handler, j *Job, p Progress) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = &panicError{value: r, stack: debug.Stack()}
		}
	}()
	return h(ctx, j, p)
}

// renewLease extends the lease every third of its length until stopped. If
// the Job was cancelled or taken over, it cancels the handler.
func (w *Worker) renewLease(ctx context.Context, p *progress) (stop func()) {
	done := make(chan struct{})
	var once sync.Once
	go func() {
		t := time.NewTicker(w.opts.Lease / 3)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				if err := p.renew(ctx); err != nil && ctx.Err() == nil {
					w.log.WarnContext(ctx, "renew job lease", "job_id", p.id, "error", err)
				}
			}
		}
	}()
	return func() { once.Do(func() { close(done) }) }
}

// fence limits an update of a Job row to the run that holds its lease: this
// worker, while the Job is running, on the attempt the run claimed. The
// attempt matters when this worker's lease renewal is late: RecoverExpired
// requeues the Job, this same worker can claim it again, and without the
// attempt the first run would keep writing over the second.
func (w *Worker) fence(q *bun.UpdateQuery, id JobID, attempt int) *bun.UpdateQuery {
	return q.Where("id = ? AND lease_owner = ? AND status = ? AND attempts = ?", id, w.opts.Owner, string(StatusRunning), attempt)
}

// finish records the result of a run of the given attempt and returns its
// outcome for metrics: succeeded, failed, retry, snoozed, released,
// cancelled or lost.
func (w *Worker) finish(ctx context.Context, job *Job, id JobID, kind Kind, attempt int, runErr error, p *progress, started time.Time) string {
	bg := context.WithoutCancel(ctx)
	now := w.now()
	upd := w.fence(w.db.NewUpdate().Model((*db.Job)(nil)).
		Set("lease_owner = NULL").Set("lease_expires_at = NULL").Set("updated_at = ?", now), id, attempt)
	if p != nil {
		if pending := p.takePending(); pending != nil {
			upd.Set("progress = ?", pending)
		}
	}
	var snooze *snoozeError
	outcome := ""
	errText := RedactError(runErr)
	switch {
	case runErr == nil:
		outcome = "succeeded"
		upd.Set("status = ?", string(StatusSucceeded)).Set("finished_at = ?", now).Set("error = ''")
	case errors.Is(runErr, ErrCancelled):
		outcome = "cancelled"
	case errors.Is(runErr, ErrLeaseLost):
		outcome = "lost"
	case errors.As(runErr, &snooze):
		outcome = "snoozed"
		msg, _ := json.Marshal(ProgressUpdate{Message: snooze.reason})
		if p != nil {
			if last := p.last(); last != nil {
				last.Message = snooze.reason
				msg, _ = json.Marshal(last)
			}
		}
		upd.Set("status = ?", string(StatusPending)).Set("run_after = ?", snooze.until.UTC().Truncate(time.Microsecond)).
			Set("attempts = attempts - 1").Set("progress = ?", json.RawMessage(msg))
	case ctx.Err() != nil && errors.Is(runErr, ctx.Err()):
		// Shutdown: give the Job back without using an attempt.
		outcome = "released"
		upd.Set("status = ?", string(StatusPending)).Set("run_after = ?", now).Set("attempts = attempts - 1")
	case IsPermanent(runErr) || job == nil || job.Attempts >= job.MaxAttempts:
		outcome = "failed"
		upd.Set("status = ?", string(StatusFailed)).Set("finished_at = ?", now).Set("error = ?", truncateError(errText))
	default:
		outcome = "retry"
		reg, _ := w.reg.Lookup(kind)
		backoff := DefaultBackoff
		if reg.Options.Backoff != nil {
			backoff = reg.Options.Backoff
		}
		upd.Set("status = ?", string(StatusPending)).Set("run_after = ?", now.Add(backoff(job.Attempts))).
			Set("error = ?", truncateError(errText))
	}
	if outcome != "cancelled" && outcome != "lost" {
		res, err := upd.Exec(bg)
		if err != nil {
			w.log.ErrorContext(bg, "record job result", "job_id", id, "kind", kind, "error", err)
		} else if n, _ := res.RowsAffected(); n == 0 {
			outcome = "lost"
		}
	}
	level := slog.LevelInfo
	if outcome == "failed" || outcome == "retry" || outcome == "lost" {
		level = slog.LevelWarn
	}
	attrs := []any{"job_id", id, "kind", kind, "outcome", outcome, "duration_ms", time.Since(started).Milliseconds()}
	if runErr != nil && outcome != "succeeded" {
		attrs = append(attrs, "error", errText)
	}
	w.log.Log(bg, level, "job finished", attrs...)
	if w.inst != nil {
		w.inst.JobsDuration.Record(bg, time.Since(started).Seconds(), metric.WithAttributes(
			attribute.String(telemetry.MetricKind, string(kind)), attribute.String(telemetry.MetricOutcome, outcome)))
	}
	return outcome
}

// progressInterval is the shortest gap between progress writes.
const progressInterval = time.Second

// progress implements Progress for a running Job. Reports are written at
// most once a second; the latest is always written when the Job finishes.
type progress struct {
	w       *Worker
	id      JobID
	attempt int // The attempt this run claimed; writes are fenced on it.
	cancel  context.CancelCauseFunc

	mu      sync.Mutex
	written time.Time
	pending json.RawMessage
	latest  *ProgressUpdate
}

func (p *progress) last() *ProgressUpdate {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.latest == nil {
		return nil
	}
	c := *p.latest
	return &c
}

func (p *progress) takePending() json.RawMessage {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := p.pending
	p.pending = nil
	return out
}

// Report implements Progress.
func (p *progress) Report(ctx context.Context, u ProgressUpdate) error {
	b, err := json.Marshal(u)
	if err != nil {
		return err
	}
	p.mu.Lock()
	p.latest = &u
	if time.Since(p.written) < progressInterval {
		p.pending = b
		p.mu.Unlock()
		return nil
	}
	p.written = time.Now()
	p.pending = nil
	p.mu.Unlock()
	return p.write(ctx, func(q *bun.UpdateQuery) { q.Set("progress = ?", json.RawMessage(b)) })
}

// SetResult implements Progress.
func (p *progress) SetResult(ctx context.Context, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return p.write(ctx, func(q *bun.UpdateQuery) { q.Set("result = ?", json.RawMessage(b)) })
}

func (p *progress) renew(ctx context.Context) error {
	p.mu.Lock()
	pending := p.pending
	if pending != nil {
		p.pending = nil
		p.written = time.Now()
	}
	p.mu.Unlock()
	return p.write(ctx, func(q *bun.UpdateQuery) {
		if pending != nil {
			q.Set("progress = ?", pending)
		}
	})
}

// write updates the Job row and extends the lease, if this run still
// holds it. Otherwise it cancels the handler and returns ErrLeaseLost.
func (p *progress) write(ctx context.Context, set func(*bun.UpdateQuery)) error {
	now := p.w.now()
	q := p.w.fence(p.w.db.NewUpdate().Model((*db.Job)(nil)).
		Set("lease_expires_at = ?", now.Add(p.w.opts.Lease)).Set("updated_at = ?", now), p.id, p.attempt)
	set(q)
	res, err := q.Exec(context.WithoutCancel(ctx))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		return nil
	}
	var row db.Job
	cause := ErrLeaseLost
	if err := p.w.db.NewSelect().Model(&row).Column("status").Where("id = ?", p.id).Scan(context.WithoutCancel(ctx)); err == nil && row.Status == string(StatusCancelled) {
		cause = ErrCancelled
	}
	p.cancel(cause)
	return cause
}
