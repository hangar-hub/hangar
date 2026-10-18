package jobs_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/db"
	"github.com/hangar-hub/hangar/internal/db/dbtest"
	"github.com/hangar-hub/hangar/internal/jobs"
)

// TestLimiterSharedAcrossTwoWorkers runs two limiters, as two worker
// processes would, against one database and checks that together they
// stay within the Instance-wide budget and both get a share (PULL-7).
func TestLimiterSharedAcrossTwoWorkers(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		const (
			budget   = 4 << 20 // bytes per second
			chunk    = 64 << 10
			duration = 4 * time.Second
		)
		ctx := context.Background()
		budgets := jobs.StaticBudget(map[string]int64{"pull": budget})
		a := jobs.NewLimiter(d, jobs.LimiterOptions{Owner: "worker-a", Budget: budgets, Refresh: 200 * time.Millisecond})
		b := jobs.NewLimiter(d, jobs.LimiterOptions{Owner: "worker-b", Budget: budgets, Refresh: 200 * time.Millisecond})

		var totals [2]atomic.Int64
		start := time.Now()
		var wg sync.WaitGroup
		for i, l := range []*jobs.DBLimiter{a, b} {
			// Two transfers per worker, as pull.file Jobs run in parallel.
			for range 2 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for time.Since(start) < duration {
						if err := l.WaitN(ctx, "pull", chunk); err != nil {
							t.Error(err)
							return
						}
						totals[i].Add(chunk)
					}
				}()
			}
		}
		wg.Wait()
		elapsed := time.Since(start).Seconds()
		total := totals[0].Load() + totals[1].Load()
		limit := float64(budget) * elapsed
		t.Logf("transferred %d bytes in %.2fs (%.0f%% of the budget); a %d, b %d",
			total, elapsed, 100*float64(total)/limit, totals[0].Load(), totals[1].Load())
		require.LessOrEqual(t, float64(total), limit*1.10, "the two workers together stay within 10%% of the budget")
		require.GreaterOrEqual(t, float64(total), limit*0.70, "the budget is used")
		for i := range totals {
			require.Greater(t, float64(totals[i].Load()), float64(total)*0.25, "worker %d gets a fair share", i)
		}
		shareA, _ := a.Share("pull")
		shareB, _ := b.Share("pull")
		require.LessOrEqual(t, shareA+shareB, int64(budget), "slices never add up to more than the budget")
		require.Equal(t, int64(budget/2), shareA)

		// When B stops, A gets the whole budget after its next refresh.
		require.NoError(t, b.Close(ctx))
		time.Sleep(250 * time.Millisecond)
		require.NoError(t, a.WaitN(ctx, "pull", 1))
		shareA, _ = a.Share("pull")
		require.Equal(t, int64(budget), shareA)
		require.NoError(t, a.Close(ctx))
	})
}

func TestLimiterUnlimitedAndErrors(t *testing.T) {
	d := dbtest.SQLite(t)
	ctx := context.Background()
	l := jobs.NewLimiter(d, jobs.LimiterOptions{Owner: "w"})
	start := time.Now()
	for range 1000 {
		require.NoError(t, l.WaitN(ctx, "pull", 1<<30))
	}
	require.Less(t, time.Since(start), time.Second, "no budget means no waiting")
	_, unlimited := l.Share("pull")
	require.True(t, unlimited)
	require.Error(t, l.WaitN(ctx, "pull", -1))

	failing := jobs.NewLimiter(d, jobs.LimiterOptions{Owner: "w2", Budget: func(context.Context, string) (int64, error) {
		return 0, errors.New("settings unavailable")
	}})
	require.Error(t, failing.WaitN(ctx, "pull", 1))

	slow := jobs.NewLimiter(d, jobs.LimiterOptions{Owner: "w3", Budget: jobs.StaticBudget(map[string]int64{"pull": 1})})
	cctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, slow.WaitN(cctx, "pull", 1<<20), context.DeadlineExceeded)
}

func TestLimiterNewcomerWaitsForAShare(t *testing.T) {
	d := dbtest.SQLite(t)
	ctx := context.Background()
	clk := newClock()
	advance := func(_ context.Context, dur time.Duration) error { clk.Add(dur); return nil }
	parked := make(chan time.Duration)
	resume := make(chan struct{})
	var first atomic.Bool
	// B's first wait parks until the test lets it go; later waits pass.
	sleepB := func(ctx context.Context, dur time.Duration) error {
		if first.CompareAndSwap(false, true) {
			parked <- dur
			<-resume
			return nil
		}
		return advance(ctx, dur)
	}
	budgets := jobs.StaticBudget(map[string]int64{"pull": 1000})
	a := jobs.NewLimiter(d, jobs.LimiterOptions{Owner: "a", Budget: budgets, Refresh: time.Second, Now: clk.Now, Sleep: advance})
	b := jobs.NewLimiter(d, jobs.LimiterOptions{Owner: "b", Budget: budgets, Refresh: time.Second, Now: clk.Now, Sleep: sleepB})
	require.NoError(t, a.WaitN(ctx, "pull", 1))
	shareA, _ := a.Share("pull")
	require.Equal(t, int64(1000), shareA, "alone, A has the whole budget")

	// B joins while A holds everything: B gets nothing until A shrinks.
	done := make(chan error)
	go func() { done <- b.WaitN(ctx, "pull", 100) }()
	<-parked
	shareB, _ := b.Share("pull")
	require.Zero(t, shareB)

	clk.Add(time.Second)
	require.NoError(t, a.WaitN(ctx, "pull", 1)) // A refreshes and halves its slice.
	shareA, _ = a.Share("pull")
	require.Equal(t, int64(500), shareA)
	close(resume)
	require.NoError(t, <-done)
	shareB, _ = b.Share("pull")
	require.Equal(t, int64(500), shareB)
}
