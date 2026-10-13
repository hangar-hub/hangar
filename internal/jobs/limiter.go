package jobs

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/uptrace/bun"

	"github.com/jamesagarside/hangar/internal/db"
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
	// Zero means 2 seconds. Slices expire after three refresh periods, so
	// a stopped process's slice returns to the others.
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
			wait := l.opts.Refresh - now.Sub(b.refreshed)
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

// advance adds the tokens earned since the last call, up to the burst.
// b.mu must be held.
func (b *bucket) advance(now time.Time) {
	if !b.last.IsZero() {
		b.tokens += now.Sub(b.last).Seconds() * b.rate
	}
	b.last = now
	b.tokens = min(b.tokens, b.rate*burst)
}

func (l *DBLimiter) maybeRefresh(ctx context.Context, name string, b *bucket) error {
	b.mu.Lock()
	due := b.refreshed.IsZero() || l.opts.Now().Sub(b.refreshed) >= l.opts.Refresh
	b.mu.Unlock()
	if !due {
		return nil
	}
	b.refreshMu.Lock()
	defer b.refreshMu.Unlock()
	b.mu.Lock()
	due = b.refreshed.IsZero() || l.opts.Now().Sub(b.refreshed) >= l.opts.Refresh
	b.mu.Unlock()
	if !due {
		return nil
	}
	return l.refresh(ctx, name, b)
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
		var others struct {
			N   int64 `bun:"n"`
			Sum int64 `bun:"total"`
		}
		if err := tx.NewSelect().Model((*db.RateLimitLease)(nil)).
			ColumnExpr("COUNT(*) AS n, COALESCE(SUM(bytes_per_second), 0) AS total").
			Where("name = ? AND owner <> ?", name, l.opts.Owner).Scan(ctx, &others); err != nil {
			return err
		}
		fair := total / (others.N + 1)
		allot = max(0, min(fair, total-others.Sum))
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
	b.refreshed = now
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
		if _, err := l.db.NewDelete().Model((*db.RateLimitLease)(nil)).Where("name = ? AND owner = ?", n, l.opts.Owner).Exec(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
