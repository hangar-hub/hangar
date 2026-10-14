package xet

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestIngestSlots checks the per-Namespace slots: case-insensitive keys,
// ErrBusy after the wait, the context's error, and no entry left behind
// once a Namespace has nothing in progress.
func TestIngestSlots(t *testing.T) {
	l := newIngestSlots(1)
	ctx := context.Background()
	release, err := l.acquire(ctx, "Alice", time.Second)
	require.NoError(t, err)
	_, err = l.acquire(ctx, "alice", 10*time.Millisecond)
	require.ErrorIs(t, err, ErrBusy)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = l.acquire(cancelled, "alice", time.Minute)
	require.ErrorIs(t, err, context.Canceled)
	other, err := l.acquire(ctx, "bob", 10*time.Millisecond)
	require.NoError(t, err)
	other()
	release()
	require.Empty(t, l.byNS)
	again, err := l.acquire(ctx, "alice", 10*time.Millisecond)
	require.NoError(t, err)
	again()
	require.Empty(t, l.byNS)
}
