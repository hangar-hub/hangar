package jobs

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/uptrace/bun"

	"github.com/hangar-hub/hangar/internal/db"
)

// Budget returns the Instance-wide byte rate of a named budget, such as
// "pull" for pull.bandwidth_limit. Zero or less means unlimited. It is read
// at every refresh, so a changed setting applies within one refresh period.
type Budget func(ctx context.Context, name string) (int64, error)

// StaticBudget returns a Budget with fixed rates. Names not in the map are
// unlimited.
func StaticBudget(rates map[string]int64) Budget {
	return func(_ context.Context, name string) (int64, error) { return rates[name], nil }
}

// LimiterOptions configure a DBLimiter.
type LimiterOptions struct {
	// Owner identifies this process. It must differ between processes.
	Owner string
	// Budget gives each budget's Instance-wide rate.
	Budget Budget
	// Refresh is how often a busy process renews its slice of a budget.
	// Zero means 2 seconds. A process that asks for no bytes for half a
	// refresh period gives its slice back, and slices expire after three
	// refresh periods, so the slice of a process that stopped returns to
	// the others. A process with less than the whole budget looks for
	// more every quarter of a refresh period.
	Refresh time.Duration
	// Now is the clock. Nil means time.Now.
	Now func() time.Time
	// Sleep waits for d or until ctx ends. Nil means a timer.
	Sleep func(ctx context.Context, d time.Duration) error
}

// DBLimiter is the shared Pull bandwidth limiter (PULL-7). Each process that
// is transferring holds a lease on a slice of the budget in the
// rate_limit_leases table and spends it through a local token bucket.
//
// A slice is the fair share, budget / active processes, but never more
// than what the other processes' current slices leave, so the slices never
// add up to more than the budget. A process that joins gets what is free
// at once and its fair share after the others' next refresh.
//
// A process that stops transferring, such as a worker whose last pull.file
// Job finished, deletes its lease once it has asked for no bytes for half a
// refresh period, rather than holding it until it expires. A process with
// less than the whole budget reads the others' slices every quarter of a
// refresh period and renews its own as soon as it can have more, so a
// slice given back or shrunk to make room is not left unused for a whole
// refresh period.
type DBLimiter struct {
	db   *db.DB
	opts LimiterOptions

	mu      sync.Mutex
	buckets map[string]*bucket
}

var _ Limiter = (*DBLimiter)(nil)

type bucket struct {
	mu        sync.Mutex
	unlimited bool
	rate      float64 // bytes per second
	tokens    float64
	last      time.Time
	refreshed time.Time
	refreshMu sync.Mutex
	// inFlight counts the WaitN calls under way, and calls counts every
	// WaitN call. idle gives the slice back when the bucket has been
	// unused for idleAfter; idleCalls is calls when it was last set.
	inFlight  int
	calls     uint64
	idleCalls uint64
	idle      *time.Timer
	// total is the budget at the last refresh, and looked is when this
	// process last renewed its slice or read the others'.
	total  int64
	looked time.Time
}

// NewLimiter returns a limiter on d.
func NewLimiter(d *db.DB, opts LimiterOptions) *DBLimiter {
	if opts.Refresh <= 0 {
		opts.Refresh = 2 * time.Second
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Sleep == nil {
		opts.Sleep = sleepCtx
	}
	if opts.Budget == nil {
		opts.Budget = StaticBudget(nil)
	}
	return &DBLimiter{db: d, opts: opts, buckets: map[string]*bucket{}}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (l *DBLimiter) bucket(name string) *bucket {
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[name]
	if !ok {
		b = &bucket{}
		l.buckets[name] = b
	}
	return b
}

// burst is how many seconds of tokens a bucket may save up.
const burst = 0.5

// WaitN implements Limiter. It blocks until n bytes may be transferred
// under the named budget. Callers pass the size of each chunk they are
// about to read; a chunk larger than the burst simply waits longer.
func (l *DBLimiter) WaitN(ctx context.Context, name string, n int64) error {
	if n < 0 {
		return fmt.Errorf("negative byte count %d", n)
	}
	b := l.bucket(name)
	l.enter(b)
	defer l.leave(name, b)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := l.maybeRefresh(ctx, name, b); err != nil {
			return err
		}
		now := l.opts.Now()
		b.mu.Lock()
		if b.unlimited {
			b.mu.Unlock()
			return nil
		}
		if b.rate <= 0 {
			// No slice yet: wait for the others to shrink theirs.
			wait := l.lookEvery() - now.Sub(b.looked)
			b.mu.Unlock()
			if err := l.opts.Sleep(ctx, max(wait, 10*time.Millisecond)); err != nil {
				return err
			}
			continue
		}
		b.advance(now)
		b.tokens -= float64(n)
		wait := time.Duration(0)
		if b.tokens < 0 {
			wait = time.Duration(-b.tokens / b.rate * float64(time.Second))
		}
		b.mu.Unlock()
		if wait <= 0 {
			return nil
		}
		if err := l.opts.Sleep(ctx, wait); err != nil {
			// Give the unspent tokens back.
			b.mu.Lock()
			b.tokens += float64(n)
			b.mu.Unlock()
			return err
		}
		return nil
	}
}

// idleAfter is how long a process may ask for no bytes before it gives
// its slice back.
func (l *DBLimiter) idleAfter() time.Duration { return l.opts.Refresh / 2 }

// enter records a WaitN call starting.
func (l *DBLimiter) enter(b *bucket) {
	b.mu.Lock()
	b.inFlight++
	b.calls++
	b.mu.Unlock()
}

// leave records a WaitN call ending. When it was the last one under way
// and this process holds a slice, it sets the idle timer.
func (l *DBLimiter) leave(name string, b *bucket) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.inFlight--
	if b.inFlight > 0 || b.unlimited || b.refreshed.IsZero() {
		return
	}
	b.idleCalls = b.calls
	if b.idle == nil {
		b.idle = time.AfterFunc(l.idleAfter(), func() { l.releaseIdle(name, b) })
		return
	}
	b.idle.Reset(l.idleAfter())
}

// releaseIdle gives the slice back if no WaitN call has started since the
// idle timer was set. The bucket forgets its slice first, so the next
// WaitN call joins again through refresh, which waits for the delete.
func (l *DBLimiter) releaseIdle(name string, b *bucket) {
	b.refreshMu.Lock()
	defer b.refreshMu.Unlock()
	b.mu.Lock()
	if b.inFlight > 0 || b.calls != b.idleCalls || b.unlimited || b.refreshed.IsZero() {
		b.mu.Unlock()
		return
	}
	b.rate, b.tokens, b.last, b.refreshed = 0, 0, time.Time{}, time.Time{}
	b.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 3*l.opts.Refresh)
	defer cancel()
	l.release(ctx, name)
}

// advance adds the tokens earned since the last call, up to the burst.
// b.mu must be held.
func (b *bucket) advance(now time.Time) {
	if !b.last.IsZero() {
		b.tokens += now.Sub(b.last).Seconds() * b.rate
	}
	b.last = now
	b.tokens = min(b.tokens, b.rate*burst)
}

// lookEvery is how often a process with less than the whole budget reads
// the others' slices.
func (l *DBLimiter) lookEvery() time.Duration { return l.opts.Refresh / 4 }

// due reports whether b's slice must be renewed, or else whether it is
// time to look for a bigger one. b.mu must be held.
func (l *DBLimiter) due(b *bucket) (refresh, look bool) {
	if b.refreshed.IsZero() {
		return true, false
	}
	now := l.opts.Now()
	if now.Sub(b.refreshed) >= l.opts.Refresh {
		return true, false
	}
	return false, !b.unlimited && b.rate < float64(b.total) && now.Sub(b.looked) >= l.lookEvery()
}

func (l *DBLimiter) maybeRefresh(ctx context.Context, name string, b *bucket) error {
	b.mu.Lock()
	due, look := l.due(b)
	b.mu.Unlock()
	if !due && !look {
		return nil
	}
	b.refreshMu.Lock()
	defer b.refreshMu.Unlock()
	b.mu.Lock()
	due, look = l.due(b)
	b.mu.Unlock()
	if !due && look {
		due = l.peek(ctx, name, b)
	}
	if !due {
		return nil
	}
	return l.refresh(ctx, name, b)
}

// peek reads the other processes' slices and reports whether a refresh now
// would give this process a bigger slice, as when another process has
// given its slice back or shrunk it to make room. It writes nothing; on an
// error it reports false and the next refresh renews the slice as usual.
func (l *DBLimiter) peek(ctx context.Context, name string, b *bucket) bool {
	now := l.opts.Now()
	b.mu.Lock()
	b.looked = now
	total, rate := b.total, b.rate
	b.mu.Unlock()
	var others leaseSum
	err := l.db.NewSelect().Model((*db.RateLimitLease)(nil)).
		ColumnExpr("COUNT(*) AS n, COALESCE(SUM(bytes_per_second), 0) AS total").
		Where("name = ? AND owner <> ? AND expires_at >= ?", name, l.opts.Owner, now.UTC().Truncate(time.Microsecond)).
		Scan(ctx, &others)
	if err != nil {
		return false
	}
	return float64(allotment(total, others)) > rate
}

// leaseSum is how many other processes hold a slice of a budget, and the
// slices' sum.
type leaseSum struct {
	N   int64 `bun:"n"`
	Sum int64 `bun:"total"`
}

// allotment is a process's slice of total: the fair share, but never more
// than the others' slices leave.
func allotment(total int64, others leaseSum) int64 {
	fair := total / (others.N + 1)
	return max(0, min(fair, total-others.Sum))
}

// refresh renews this process's slice of the budget.
func (l *DBLimiter) refresh(ctx context.Context, name string, b *bucket) error {
	total, err := l.opts.Budget(ctx, name)
	if err != nil {
		return fmt.Errorf("read %s bandwidth budget: %w", name, err)
	}
	if total <= 0 {
		l.release(ctx, name)
		b.mu.Lock()
		b.unlimited, b.refreshed = true, l.opts.Now()
		b.mu.Unlock()
		return nil
	}
	var allot int64
	err = l.db.InTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		if err := lockShared(ctx, l.db, tx, "limit:"+name); err != nil {
			return err
		}
		now := l.opts.Now().UTC().Truncate(time.Microsecond)
		if _, err := tx.NewDelete().Model((*db.RateLimitLease)(nil)).Where("name = ? AND expires_at < ?", name, now).Exec(ctx); err != nil {
			return err
		}
		var others leaseSum
		if err := tx.NewSelect().Model((*db.RateLimitLease)(nil)).
			ColumnExpr("COUNT(*) AS n, COALESCE(SUM(bytes_per_second), 0) AS total").
			Where("name = ? AND owner <> ?", name, l.opts.Owner).Scan(ctx, &others); err != nil {
			return err
		}
		allot = allotment(total, others)
		row := &db.RateLimitLease{Name: name, Owner: l.opts.Owner, BytesPerSecond: allot, ExpiresAt: now.Add(3 * l.opts.Refresh)}
		_, err := tx.NewInsert().Model(row).On("CONFLICT (name, owner) DO UPDATE").
			Set("bytes_per_second = EXCLUDED.bytes_per_second").Set("expires_at = EXCLUDED.expires_at").
			Set("updated_at = EXCLUDED.updated_at").Exec(ctx)
		return err
	})
	if err != nil {
		return fmt.Errorf("renew %s bandwidth lease: %w", name, err)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	now := l.opts.Now()
	if !b.unlimited && b.rate > 0 {
		b.advance(now)
	} else {
		b.last = now
		b.tokens = 0
	}
	b.unlimited = false
	b.rate = float64(allot)
	b.tokens = min(b.tokens, b.rate*burst)
	b.refreshed, b.looked, b.total = now, now, total
	return nil
}

// Share returns this process's current slice of a budget in bytes per
// second, and whether the budget is unlimited.
func (l *DBLimiter) Share(name string) (int64, bool) {
	b := l.bucket(name)
	b.mu.Lock()
	defer b.mu.Unlock()
	return int64(b.rate), b.unlimited
}

func (l *DBLimiter) release(ctx context.Context, name string) {
	_, _ = l.db.NewDelete().Model((*db.RateLimitLease)(nil)).Where("name = ? AND owner = ?", name, l.opts.Owner).Exec(ctx)
}

// Close gives every slice back, so other processes get the whole budget
// at their next refresh.
func (l *DBLimiter) Close(ctx context.Context) error {
	l.mu.Lock()
	names := make([]string, 0, len(l.buckets))
	for n := range l.buckets {
		names = append(names, n)
	}
	l.mu.Unlock()
	var errs []error
	for _, n := range names {
		b := l.bucket(n)
		b.mu.Lock()
		if b.idle != nil {
			b.idle.Stop()
		}
		b.mu.Unlock()
		if _, err := l.db.NewDelete().Model((*db.RateLimitLease)(nil)).Where("name = ? AND owner = ?", n, l.opts.Owner).Exec(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
