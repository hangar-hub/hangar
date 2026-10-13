// Package jobstest provides a fake jobs.Queue that keeps Jobs in memory
// and runs them synchronously with RunPending, and a fake Limiter.
package jobstest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/jamesagarside/hangar/internal/jobs"
)

// Fake is an in-memory jobs.Queue. It honours deduplication, priorities,
// RunAfter, cancellation, retries with MaxAttempts and Permanent errors,
// but never runs anything on its own: call RunPending.
type Fake struct {
	Registry *jobs.Registry

	mu        sync.Mutex
	jobs      map[jobs.JobID]*jobs.Job
	order     []jobs.JobID
	schedules map[string]jobs.Schedule
	cancels   map[jobs.JobID]context.CancelFunc
	now       func() time.Time
}

var _ jobs.Queue = (*Fake)(nil)

// NewFake returns an empty Fake that runs handlers from reg.
func NewFake(reg *jobs.Registry) *Fake {
	if reg == nil {
		reg = jobs.NewRegistry()
	}
	return &Fake{
		Registry: reg, jobs: map[jobs.JobID]*jobs.Job{}, schedules: map[string]jobs.Schedule{},
		cancels: map[jobs.JobID]context.CancelFunc{}, now: time.Now,
	}
}

// SetClock makes the fake use now, for RunAfter and backoff.
func (f *Fake) SetClock(now func() time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = now
}

func clone(j *jobs.Job) *jobs.Job {
	c := *j
	c.Payload = append(json.RawMessage(nil), j.Payload...)
	c.Result = append(json.RawMessage(nil), j.Result...)
	c.Progress.Files = slices.Clone(j.Progress.Files)
	return &c
}

// Enqueue implements jobs.Queue.
func (f *Fake) Enqueue(_ context.Context, nj jobs.NewJob) (jobs.JobID, error) {
	if nj.Kind == "" {
		return uuid.Nil, errors.New("enqueue: kind is required")
	}
	payload := json.RawMessage("{}")
	if nj.Payload != nil {
		b, err := json.Marshal(nj.Payload)
		if err != nil {
			return uuid.Nil, fmt.Errorf("enqueue %s: %w", nj.Kind, err)
		}
		payload = b
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if nj.DedupeKey != "" {
		for _, j := range f.jobs {
			if j.Kind == nj.Kind && j.DedupeKey == nj.DedupeKey && j.Status.Active() {
				return j.ID, nil
			}
		}
	}
	maxAttempts := nj.MaxAttempts
	if maxAttempts == 0 {
		maxAttempts = 5
		if reg, ok := f.Registry.Lookup(nj.Kind); ok {
			maxAttempts = reg.Options.MaxAttempts
		}
	}
	now := f.now()
	runAfter := nj.RunAfter
	if runAfter.IsZero() {
		runAfter = now
	}
	j := &jobs.Job{
		ID: uuid.Must(uuid.NewV7()), Kind: nj.Kind, DedupeKey: nj.DedupeKey, Payload: payload,
		Status: jobs.StatusPending, Priority: nj.Priority, MaxAttempts: maxAttempts, RunAfter: runAfter,
		Result: json.RawMessage("{}"), CreatedBy: nj.CreatedBy, ParentID: nj.ParentID, CreatedAt: now, UpdatedAt: now,
	}
	f.jobs[j.ID] = j
	f.order = append(f.order, j.ID)
	return j.ID, nil
}

// Cancel implements jobs.Queue.
func (f *Fake) Cancel(_ context.Context, id jobs.JobID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	j, ok := f.jobs[id]
	if !ok {
		return fmt.Errorf("%w: %s", jobs.ErrNotFound, id)
	}
	if !j.Status.Active() {
		return fmt.Errorf("%w: %s is %s", jobs.ErrNotActive, id, j.Status)
	}
	if cancel, ok := f.cancels[id]; ok {
		cancel()
	}
	now := f.now()
	j.Status, j.FinishedAt, j.UpdatedAt = jobs.StatusCancelled, &now, now
	return nil
}

// Get implements jobs.Queue.
func (f *Fake) Get(_ context.Context, id jobs.JobID) (*jobs.Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	j, ok := f.jobs[id]
	if !ok {
		return nil, fmt.Errorf("%w: %s", jobs.ErrNotFound, id)
	}
	return clone(j), nil
}

// List implements jobs.Queue. The cursor is an offset.
func (f *Fake) List(_ context.Context, flt jobs.Filter) ([]*jobs.Job, string, error) {
	f.mu.Lock()
	var out []*jobs.Job
	for i := len(f.order) - 1; i >= 0; i-- {
		j := f.jobs[f.order[i]]
		if len(flt.Kinds) > 0 && !slices.Contains(flt.Kinds, j.Kind) {
			continue
		}
		if len(flt.Statuses) > 0 && !slices.Contains(flt.Statuses, j.Status) {
			continue
		}
		if flt.CreatedBy != uuid.Nil && j.CreatedBy != flt.CreatedBy {
			continue
		}
		if flt.ParentID != uuid.Nil && j.ParentID != flt.ParentID {
			continue
		}
		out = append(out, clone(j))
	}
	f.mu.Unlock()
	offset := 0
	if flt.Cursor != "" {
		n, err := strconv.Atoi(flt.Cursor)
		if err != nil || n < 0 {
			return nil, "", fmt.Errorf("bad cursor %q", flt.Cursor)
		}
		offset = min(n, len(out))
	}
	out = out[offset:]
	next := ""
	if flt.Limit > 0 && len(out) > flt.Limit {
		out = out[:flt.Limit]
		next = strconv.Itoa(offset + flt.Limit)
	}
	return out, next, nil
}

// Retry implements jobs.Queue.
func (f *Fake) Retry(ctx context.Context, id jobs.JobID) (jobs.JobID, error) {
	old, err := f.Get(ctx, id)
	if err != nil {
		return uuid.Nil, err
	}
	if old.Status != jobs.StatusFailed && old.Status != jobs.StatusCancelled {
		return uuid.Nil, fmt.Errorf("%w: %s is %s", jobs.ErrNotRetrying, id, old.Status)
	}
	return f.Enqueue(ctx, jobs.NewJob{Kind: old.Kind, DedupeKey: old.DedupeKey, Payload: old.Payload, Priority: old.Priority,
		MaxAttempts: old.MaxAttempts, CreatedBy: old.CreatedBy, ParentID: old.ParentID})
}

// SetSchedule implements jobs.Queue.
func (f *Fake) SetSchedule(_ context.Context, s jobs.Schedule) error {
	if s.Kind == "" || s.Key == "" || s.Cron == "" {
		return errors.New("schedule needs a kind, key and cron expression")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.schedules[string(s.Kind)+"\x00"+s.Key] = s
	return nil
}

// DeleteSchedule implements jobs.Queue.
func (f *Fake) DeleteSchedule(_ context.Context, kind jobs.Kind, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.schedules, string(kind)+"\x00"+key)
	return nil
}

// Schedules returns the current schedules, sorted by kind and key.
func (f *Fake) Schedules() []jobs.Schedule {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]jobs.Schedule, 0, len(f.schedules))
	for _, s := range f.schedules {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Key < out[j].Key
	})
	return out
}

type progress struct {
	f  *Fake
	id jobs.JobID
}

func (p progress) Report(_ context.Context, u jobs.ProgressUpdate) error {
	p.f.mu.Lock()
	defer p.f.mu.Unlock()
	j := p.f.jobs[p.id]
	j.Progress = u
	j.UpdatedAt = p.f.now()
	return nil
}

func (p progress) SetResult(_ context.Context, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	p.f.mu.Lock()
	defer p.f.mu.Unlock()
	p.f.jobs[p.id].Result = b
	return nil
}

// next claims the highest-priority pending Job that is due and has a handler.
func (f *Fake) next() (*jobs.Job, jobs.Registration, context.Context, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := f.now()
	var best *jobs.Job
	var bestReg jobs.Registration
	for _, id := range f.order {
		j := f.jobs[id]
		if j.Status != jobs.StatusPending || j.RunAfter.After(now) {
			continue
		}
		reg, ok := f.Registry.Lookup(j.Kind)
		if !ok {
			continue
		}
		if best == nil || j.Priority > best.Priority {
			best, bestReg = j, reg
		}
	}
	if best == nil {
		return nil, jobs.Registration{}, nil, false
	}
	ctx, cancel := context.WithCancel(context.Background())
	if bestReg.Options.Timeout > 0 {
		ctx, cancel = context.WithTimeout(context.Background(), bestReg.Options.Timeout)
	}
	f.cancels[best.ID] = cancel
	best.Status = jobs.StatusRunning
	best.Attempts++
	best.StartedAt = &now
	best.UpdatedAt = now
	return clone(best), bestReg, ctx, true
}

// RunPending runs every due pending Job that has a handler, including Jobs
// the handlers enqueue, until none are left. It returns how many runs
// happened. Failed attempts are rescheduled with the handler's backoff, so
// they run again only once the clock passes RunAfter.
func (f *Fake) RunPending(ctx context.Context) int {
	runs := 0
	for ctx.Err() == nil {
		j, reg, jobCtx, ok := f.next()
		if !ok {
			return runs
		}
		runs++
		err := reg.Handler(jobCtx, j, progress{f: f, id: j.ID})
		f.finish(j.ID, reg, err)
	}
	return runs
}

func (f *Fake) finish(id jobs.JobID, reg jobs.Registration, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if cancel, ok := f.cancels[id]; ok {
		cancel()
		delete(f.cancels, id)
	}
	j := f.jobs[id]
	now := f.now()
	j.UpdatedAt = now
	if j.Status == jobs.StatusCancelled {
		return
	}
	switch {
	case err == nil:
		j.Status, j.FinishedAt, j.Error = jobs.StatusSucceeded, &now, ""
	case jobs.IsPermanent(err) || j.Attempts >= j.MaxAttempts:
		j.Status, j.FinishedAt, j.Error = jobs.StatusFailed, &now, err.Error()
	default:
		j.Status, j.Error = jobs.StatusPending, err.Error()
		j.RunAfter = now.Add(reg.Options.Backoff(j.Attempts))
	}
}

// Limiter is a jobs.Limiter that never waits and records the bytes asked for.
type Limiter struct {
	mu    sync.Mutex
	bytes map[string]int64
}

var _ jobs.Limiter = (*Limiter)(nil)

// WaitN implements jobs.Limiter.
func (l *Limiter) WaitN(ctx context.Context, name string, n int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if n < 0 {
		return fmt.Errorf("negative byte count %d", n)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.bytes == nil {
		l.bytes = map[string]int64{}
	}
	l.bytes[name] += n
	return nil
}

// Bytes returns the total bytes requested under name.
func (l *Limiter) Bytes(name string) int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.bytes[name]
}
