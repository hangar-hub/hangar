package jobs

import (
	"context"
	"time"

	"github.com/uptrace/bun"

	"github.com/hangar-hub/hangar/internal/db"
)

// Leader elects one process of the Instance for a named task with a lease
// in the rate_limit_leases table. The holder renews it by calling Acquire
// again before it expires; if the holder stops, another process takes over
// once the lease runs out.
type Leader struct {
	db    *db.DB
	name  string
	owner string
	ttl   time.Duration
	now   func() time.Time
}

// NewLeader returns an election for name. now may be nil.
func NewLeader(d *db.DB, name, owner string, ttl time.Duration, now func() time.Time) *Leader {
	if now == nil {
		now = db.Now
	}
	return &Leader{db: d, name: name, owner: owner, ttl: ttl, now: now}
}

// lockShared serialises writers of a lease name: an advisory lock on
// Postgres, and the write transaction itself on SQLite.
func lockShared(ctx context.Context, d *db.DB, tx bun.Tx, name string) error {
	if d.IsSQLite() {
		return nil
	}
	_, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(?)", advisoryKey("lease:"+name))
	return err
}

// Acquire takes or renews the lease and reports whether this process holds it.
func (l *Leader) Acquire(ctx context.Context) (bool, error) {
	held := false
	err := l.db.InTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		if err := lockShared(ctx, l.db, tx, l.name); err != nil {
			return err
		}
		now := l.now().UTC().Truncate(time.Microsecond)
		if _, err := tx.NewDelete().Model((*db.RateLimitLease)(nil)).Where("name = ? AND expires_at < ?", l.name, now).Exec(ctx); err != nil {
			return err
		}
		others, err := tx.NewSelect().Model((*db.RateLimitLease)(nil)).Where("name = ? AND owner <> ?", l.name, l.owner).Count(ctx)
		if err != nil {
			return err
		}
		if others > 0 {
			return nil
		}
		row := &db.RateLimitLease{Name: l.name, Owner: l.owner, ExpiresAt: now.Add(l.ttl)}
		if _, err := tx.NewInsert().Model(row).On("CONFLICT (name, owner) DO UPDATE").
			Set("expires_at = EXCLUDED.expires_at").Set("updated_at = EXCLUDED.updated_at").Exec(ctx); err != nil {
			return err
		}
		held = true
		return nil
	})
	return held, err
}

// Release gives the lease up, so another process can take over at once.
func (l *Leader) Release(ctx context.Context) {
	_, _ = l.db.NewDelete().Model((*db.RateLimitLease)(nil)).Where("name = ? AND owner = ?", l.name, l.owner).Exec(ctx)
}
