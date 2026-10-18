package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"
	"go.opentelemetry.io/otel/propagation"

	"github.com/hangar-hub/hangar/internal/config"
	"github.com/hangar-hub/hangar/internal/db"
)

// List limits, from docs/spec/10-native-api.md.
const (
	DefaultListLimit = 50
	MaxListLimit     = 500
)

// maxErrorLen caps the error text stored on a Job.
const maxErrorLen = 4096

// DBQueue is the Job queue on the jobs and schedules tables, for SQLite and
// Postgres. It is safe for concurrent use and across processes.
type DBQueue struct {
	db       *db.DB
	registry *Registry
	loc      *time.Location
	now      func() time.Time
}

var _ Queue = (*DBQueue)(nil)

// QueueOptions configure a DBQueue.
type QueueOptions struct {
	// Location is the time zone of schedules (server.timezone). Nil means UTC.
	Location *time.Location
	// Now is the clock. Nil means db.Now.
	Now func() time.Time
}

// NewQueue returns a queue on d. reg gives each kind's default
// MaxAttempts; it may be nil.
func NewQueue(d *db.DB, reg *Registry, opts QueueOptions) *DBQueue {
	if reg == nil {
		reg = NewRegistry()
	}
	if opts.Location == nil {
		opts.Location = time.UTC
	}
	if opts.Now == nil {
		opts.Now = db.Now
	}
	return &DBQueue{db: d, registry: reg, loc: opts.Location, now: opts.Now}
}

func (q *DBQueue) clock() time.Time { return q.now().UTC().Truncate(time.Microsecond) }

// Enqueue implements Queue.
func (q *DBQueue) Enqueue(ctx context.Context, nj NewJob) (JobID, error) {
	return q.enqueue(ctx, q.db, nj)
}

// EnqueueTx enqueues inside tx, so the Job exists only if tx commits.
// Other packages use it to enqueue follow-up Jobs atomically with their
// own writes, such as index and scan Jobs with a ref update.
func (q *DBQueue) EnqueueTx(ctx context.Context, tx bun.IDB, nj NewJob) (JobID, error) {
	return q.enqueue(ctx, tx, nj)
}

func marshalPayload(v any) (json.RawMessage, error) {
	switch p := v.(type) {
	case nil:
		return json.RawMessage("{}"), nil
	case json.RawMessage:
		if len(p) == 0 {
			return json.RawMessage("{}"), nil
		}
		if !json.Valid(p) {
			return nil, errors.New("payload is not valid JSON")
		}
		return p, nil
	default:
		return json.Marshal(v)
	}
}

func (q *DBQueue) enqueue(ctx context.Context, idb bun.IDB, nj NewJob) (JobID, error) {
	if nj.Kind == "" {
		return uuid.Nil, errors.New("enqueue: kind is required")
	}
	payload, err := marshalPayload(nj.Payload)
	if err != nil {
		return uuid.Nil, fmt.Errorf("enqueue %s: %w", nj.Kind, err)
	}
	maxAttempts := nj.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 5
		if reg, ok := q.registry.Lookup(nj.Kind); ok {
			maxAttempts = reg.Options.MaxAttempts
		}
	}
	now := q.clock()
	runAfter := nj.RunAfter.UTC().Truncate(time.Microsecond)
	if nj.RunAfter.IsZero() {
		runAfter = now
	}
	carrier := propagation.MapCarrier{}
	propagation.TraceContext{}.Inject(ctx, carrier)
	row := &db.Job{
		ID: db.NewID(), Kind: string(nj.Kind), Payload: payload, Status: string(StatusPending),
		Priority: nj.Priority, MaxAttempts: maxAttempts, RunAfter: runAfter,
		Progress: json.RawMessage("{}"), Result: json.RawMessage("{}"), TraceContext: map[string]string(carrier),
	}
	if nj.DedupeKey != "" {
		key := nj.DedupeKey
		row.DedupeKey = &key
	}
	if nj.CreatedBy != uuid.Nil {
		u := nj.CreatedBy
		row.CreatedBy = &u
	}
	if nj.ParentID != uuid.Nil {
		p := nj.ParentID
		row.ParentID = &p
	}
	if nj.Repo != uuid.Nil {
		r := nj.Repo
		row.RepoID = &r
	}
	for range 5 {
		res, err := idb.NewInsert().Model(row).
			On("CONFLICT (kind, dedupe_key) WHERE status IN ('pending', 'running') AND dedupe_key IS NOT NULL DO NOTHING").
			Exec(ctx)
		if err != nil {
			return uuid.Nil, fmt.Errorf("enqueue %s: %w", nj.Kind, err)
		}
		if n, _ := res.RowsAffected(); n == 1 {
			return row.ID, nil
		}
		// A pending or running Job has the same kind and dedupe key: join it.
		var existing db.Job
		err = idb.NewSelect().Model(&existing).Column("id").
			Where("kind = ? AND dedupe_key = ? AND status IN (?)", row.Kind, nj.DedupeKey, bun.List(activeStatuses())).
			Limit(1).Scan(ctx)
		if err == nil {
			return existing.ID, nil
		}
		if !db.IsNotFound(err) {
			return uuid.Nil, fmt.Errorf("enqueue %s: %w", nj.Kind, err)
		}
		// The other Job finished in between; try the insert again.
	}
	return uuid.Nil, fmt.Errorf("enqueue %s: the dedupe key %q kept changing hands", nj.Kind, nj.DedupeKey)
}

func activeStatuses() []string { return []string{string(StatusPending), string(StatusRunning)} }

// Cancel implements Queue. A pending Job is cancelled at once; a running
// Job is marked cancelled and its worker stops the handler at its next
// lease renewal. Active child Jobs are cancelled too.
func (q *DBQueue) Cancel(ctx context.Context, id JobID) error {
	return q.db.InTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		var row db.Job
		if err := tx.NewSelect().Model(&row).Column("id", "status").Where("id = ?", id).Scan(ctx); err != nil {
			if db.IsNotFound(err) {
				return fmt.Errorf("%w: %s", ErrNotFound, id)
			}
			return err
		}
		if !Status(row.Status).Active() {
			return fmt.Errorf("%w: %s is %s", ErrNotActive, id, row.Status)
		}
		now := q.clock()
		ids := []uuid.UUID{id}
		for len(ids) > 0 {
			if _, err := tx.NewUpdate().Model((*db.Job)(nil)).
				Set("status = ?", string(StatusCancelled)).Set("finished_at = ?", now).Set("updated_at = ?", now).
				Where("id IN (?) AND status IN (?)", bun.List(ids), bun.List(activeStatuses())).Exec(ctx); err != nil {
				return err
			}
			var children []uuid.UUID
			if err := tx.NewSelect().Model((*db.Job)(nil)).Column("id").
				Where("parent_id IN (?) AND status IN (?)", bun.List(ids), bun.List(activeStatuses())).Scan(ctx, &children); err != nil {
				return err
			}
			ids = children
		}
		return nil
	})
}

// Get implements Queue.
func (q *DBQueue) Get(ctx context.Context, id JobID) (*Job, error) {
	var row db.Job
	if err := q.db.NewSelect().Model(&row).Where("id = ?", id).Scan(ctx); err != nil {
		if db.IsNotFound(err) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
		}
		return nil, err
	}
	return fromRow(&row)
}

// List implements Queue. The cursor is the last Job ID of the previous page.
func (q *DBQueue) List(ctx context.Context, f Filter) ([]*Job, string, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = DefaultListLimit
	}
	limit = min(limit, MaxListLimit)
	sel := q.db.NewSelect().Model((*db.Job)(nil)).OrderExpr("id DESC").Limit(int64(limit + 1))
	if len(f.Kinds) > 0 {
		kinds := make([]string, len(f.Kinds))
		for i, k := range f.Kinds {
			kinds[i] = string(k)
		}
		sel.Where("kind IN (?)", bun.List(kinds))
	}
	if len(f.Statuses) > 0 {
		st := make([]string, len(f.Statuses))
		for i, s := range f.Statuses {
			st[i] = string(s)
		}
		sel.Where("status IN (?)", bun.List(st))
	}
	if f.CreatedBy != uuid.Nil {
		sel.Where("created_by = ?", f.CreatedBy)
	}
	if f.ParentID != uuid.Nil {
		sel.Where("parent_id = ?", f.ParentID)
	}
	if f.Cursor != "" {
		after, err := uuid.Parse(f.Cursor)
		if err != nil {
			return nil, "", fmt.Errorf("invalid cursor %q", f.Cursor)
		}
		sel.Where("id < ?", after)
	}
	var rows []db.Job
	if err := sel.Scan(ctx, &rows); err != nil {
		return nil, "", err
	}
	next := ""
	if len(rows) > limit {
		rows = rows[:limit]
		next = rows[len(rows)-1].ID.String()
	}
	out := make([]*Job, 0, len(rows))
	for i := range rows {
		j, err := fromRow(&rows[i])
		if err != nil {
			return nil, "", err
		}
		out = append(out, j)
	}
	return out, next, nil
}

// Retry implements Queue.
func (q *DBQueue) Retry(ctx context.Context, id JobID) (JobID, error) {
	old, err := q.Get(ctx, id)
	if err != nil {
		return uuid.Nil, err
	}
	if old.Status != StatusFailed && old.Status != StatusCancelled {
		return uuid.Nil, fmt.Errorf("%w: %s is %s", ErrNotRetrying, id, old.Status)
	}
	return q.Enqueue(ctx, NewJob{
		Kind: old.Kind, DedupeKey: old.DedupeKey, Payload: old.Payload, Priority: old.Priority,
		MaxAttempts: old.MaxAttempts, CreatedBy: old.CreatedBy, ParentID: old.ParentID, Repo: old.Repo,
	})
}

// NextRun returns the first time after now that the 5-field cron
// expression fires, in loc.
func NextRun(expr string, loc *time.Location, now time.Time) (time.Time, error) {
	sched, err := config.CronParser.Parse(expr)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid cron expression %q: %w", expr, err)
	}
	next := sched.Next(now.In(loc))
	if next.IsZero() {
		return time.Time{}, fmt.Errorf("cron expression %q never fires", expr)
	}
	return next.UTC().Truncate(time.Microsecond), nil
}

// SetSchedule implements Queue. Changing the cron expression moves the
// next run; setting the same expression keeps it.
func (q *DBQueue) SetSchedule(ctx context.Context, s Schedule) error {
	if s.Kind == "" || s.Key == "" || s.Cron == "" {
		return errors.New("schedule needs a kind, key and cron expression")
	}
	now := q.clock()
	next, err := NextRun(s.Cron, q.loc, now)
	if err != nil {
		return err
	}
	payload, err := marshalPayload(s.Payload)
	if err != nil {
		return fmt.Errorf("schedule %s %s: %w", s.Kind, s.Key, err)
	}
	row := &db.Schedule{ID: db.NewID(), Kind: string(s.Kind), Key: s.Key, Cron: s.Cron, Payload: payload, Enabled: s.Enabled, NextRunAt: next}
	_, err = q.db.NewInsert().Model(row).
		On("CONFLICT (kind, key) DO UPDATE").
		Set("payload = EXCLUDED.payload").
		Set("enabled = EXCLUDED.enabled").
		Set("next_run_at = CASE WHEN ?TableAlias.cron = EXCLUDED.cron THEN ?TableAlias.next_run_at ELSE EXCLUDED.next_run_at END").
		Set("cron = EXCLUDED.cron").
		Set("updated_at = EXCLUDED.updated_at").
		Exec(ctx)
	return err
}

// DeleteSchedule implements Queue.
func (q *DBQueue) DeleteSchedule(ctx context.Context, kind Kind, key string) error {
	_, err := q.db.NewDelete().Model((*db.Schedule)(nil)).Where("kind = ? AND key = ?", string(kind), key).Exec(ctx)
	return err
}

// ScheduleInfo is a schedule and its next run.
type ScheduleInfo struct {
	Schedule
	NextRunAt time.Time
	LastRunAt *time.Time
}

// Schedules lists every schedule, by kind and key.
func (q *DBQueue) Schedules(ctx context.Context) ([]ScheduleInfo, error) {
	var rows []db.Schedule
	if err := q.db.NewSelect().Model(&rows).OrderExpr("kind, key").Scan(ctx); err != nil {
		return nil, err
	}
	out := make([]ScheduleInfo, len(rows))
	for i, r := range rows {
		out[i] = ScheduleInfo{
			Schedule:  Schedule{Kind: Kind(r.Kind), Key: r.Key, Cron: r.Cron, Payload: r.Payload, Enabled: r.Enabled},
			NextRunAt: r.NextRunAt, LastRunAt: r.LastRunAt,
		}
	}
	return out, nil
}

func fromRow(r *db.Job) (*Job, error) {
	j := &Job{
		ID: r.ID, Kind: Kind(r.Kind), Payload: r.Payload, Status: Status(r.Status),
		Priority: r.Priority, Attempts: r.Attempts, MaxAttempts: r.MaxAttempts, RunAfter: r.RunAfter.UTC(),
		Result: r.Result, Error: r.Error, CreatedAt: r.CreatedAt.UTC(), UpdatedAt: r.UpdatedAt.UTC(),
		StartedAt: utcPtr(r.StartedAt), FinishedAt: utcPtr(r.FinishedAt),
	}
	if r.DedupeKey != nil {
		j.DedupeKey = *r.DedupeKey
	}
	if r.CreatedBy != nil {
		j.CreatedBy = *r.CreatedBy
	}
	if r.ParentID != nil {
		j.ParentID = *r.ParentID
	}
	if r.RepoID != nil {
		j.Repo = *r.RepoID
	}
	if len(r.Progress) > 0 && strings.TrimSpace(string(r.Progress)) != "{}" {
		if err := json.Unmarshal(r.Progress, &j.Progress); err != nil {
			return nil, fmt.Errorf("decode progress of job %s: %w", r.ID, err)
		}
	}
	return j, nil
}

func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

func truncateError(s string) string {
	if len(s) <= maxErrorLen {
		return s
	}
	return s[:maxErrorLen] + "..."
}
