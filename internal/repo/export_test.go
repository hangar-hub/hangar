package repo

import (
	"context"
	"sync/atomic"
	"time"
)

// CountRetryWaits makes s count the waits between attempts of Commits
// without a parent Commit, still waiting as it would. It is for tests.
func CountRetryWaits(s *Store) *atomic.Int64 {
	n := new(atomic.Int64)
	wait := s.retryWait
	s.retryWait = func(ctx context.Context, d time.Duration) error {
		n.Add(1)
		return wait(ctx, d)
	}
	return n
}
