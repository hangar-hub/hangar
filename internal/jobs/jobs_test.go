package jobs_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/jobs"
)

func TestRegistry(t *testing.T) {
	r := jobs.NewRegistry()
	h := func(context.Context, *jobs.Job, jobs.Progress) error { return nil }
	require.NoError(t, r.Register(jobs.KindIndex, h, jobs.HandlerOptions{}))
	require.NoError(t, r.Register(jobs.KindGC, h, jobs.HandlerOptions{MaxAttempts: 1}))
	require.Error(t, r.Register(jobs.KindIndex, h, jobs.HandlerOptions{}), "one handler per kind")
	require.Error(t, r.Register(jobs.KindScan, nil, jobs.HandlerOptions{}))
	reg, ok := r.Lookup(jobs.KindIndex)
	require.True(t, ok)
	require.Equal(t, 5, reg.Options.MaxAttempts, "default attempts")
	require.NotNil(t, reg.Options.Backoff)
	_, ok = r.Lookup(jobs.KindPull)
	require.False(t, ok)
	require.Equal(t, []jobs.Kind{jobs.KindGC, jobs.KindIndex}, r.Kinds())
}

func TestKindsAndStatus(t *testing.T) {
	require.Len(t, jobs.AllKinds(), 16)
	require.True(t, jobs.StatusPending.Active())
	require.True(t, jobs.StatusRunning.Active())
	require.False(t, jobs.StatusFailed.Active())
}

func TestPermanentAndBackoff(t *testing.T) {
	base := errors.New("bad payload")
	p := jobs.Permanent(base)
	require.True(t, jobs.IsPermanent(p))
	require.ErrorIs(t, p, base)
	require.Equal(t, "bad payload", p.Error())
	require.False(t, jobs.IsPermanent(base))
	require.NoError(t, jobs.Permanent(nil))

	require.Equal(t, 30*time.Second, jobs.DefaultBackoff(1))
	require.Equal(t, time.Minute, jobs.DefaultBackoff(2))
	require.Equal(t, time.Hour, jobs.DefaultBackoff(20))
}

func TestDecodePayload(t *testing.T) {
	j := &jobs.Job{Kind: jobs.KindPull, Payload: json.RawMessage(`{"repo":"a/b"}`)}
	var p struct{ Repo string }
	require.NoError(t, j.DecodePayload(&p))
	require.Equal(t, "a/b", p.Repo)
	j.Payload = json.RawMessage(`nope`)
	require.Error(t, j.DecodePayload(&p))
}
