package repo

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestCommitBackoff checks the wait between attempts of a Commit without a
// parent Commit: jittered over the whole window, growing with each retry
// from the longer of the base and the attempt's own time, and capped.
func TestCommitBackoff(t *testing.T) {
	top := func(n int64) int64 { return n - 1 }
	bottom := func(int64) int64 { return 0 }
	cases := []struct {
		retry int
		took  time.Duration
		want  time.Duration // the largest wait
	}{
		{1, 0, commitBackoffBase},
		{2, 0, 2 * commitBackoffBase},
		{3, time.Millisecond, 4 * commitBackoffBase},
		{1, 300 * time.Millisecond, 300 * time.Millisecond},
		{2, 300 * time.Millisecond, 600 * time.Millisecond},
		{4, 300 * time.Millisecond, commitBackoffCap},
		{maxCommitAttempts - 1, 0, 64 * commitBackoffBase},
		{60, time.Hour, commitBackoffCap},
		{0, 0, commitBackoffBase},
	}
	for _, c := range cases {
		require.Equal(t, c.want, commitBackoff(c.retry, c.took, top), "retry %d after %s", c.retry, c.took)
		require.Equal(t, time.Duration(1), commitBackoff(c.retry, c.took, bottom), "the shortest wait is above zero")
	}
	// Seven retries wait at most the sum of their windows.
	var total time.Duration
	for retry := 1; retry < maxCommitAttempts; retry++ {
		total += commitBackoff(retry, 0, top)
	}
	require.LessOrEqual(t, total, 7*commitBackoffCap)
}

func TestSleepCtxEndsWithTheContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	require.ErrorIs(t, sleepCtx(ctx, time.Hour), context.Canceled)
	require.Less(t, time.Since(start), time.Second)
	require.NoError(t, sleepCtx(context.Background(), time.Millisecond))
}
