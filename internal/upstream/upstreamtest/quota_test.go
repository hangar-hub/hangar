package upstreamtest_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/upstream"
	"github.com/jamesagarside/hangar/internal/upstream/upstreamtest"
)

func TestQuotaFake(t *testing.T) {
	ctx := context.Background()
	q := upstreamtest.NewQuota()
	ns, other := uuid.New(), uuid.New()

	require.NoError(t, q.CheckQuota(ctx, ns, 1<<40), "no limit means no quota")
	q.SetLimit(ns, 100)
	q.SetUsed(ns, 60)
	require.NoError(t, q.CheckQuota(ctx, ns, 40), "exactly full is allowed")
	require.NoError(t, q.CheckQuota(ctx, ns, 0))
	err := q.CheckQuota(ctx, ns, 41)
	require.ErrorIs(t, err, upstream.ErrQuotaExceeded)
	var refusal *upstreamtest.QuotaRefusal
	require.ErrorAs(t, err, &refusal)
	require.Equal(t, upstreamtest.QuotaRefusal{Used: 60, Adding: 41, Limit: 100}, *refusal)
	require.Contains(t, err.Error(), "ask an admin to raise the quota")
	require.NoError(t, q.CheckQuota(ctx, other, 41), "limits are per Namespace")
	require.Equal(t, []upstreamtest.QuotaCheck{{Namespace: ns, Adding: 41, Refused: true}}, q.Refusals())
	require.Len(t, q.Checks(), 5)

	q.Hold(ns, "a")
	n, err := q.NewLFSBytes(ctx, ns, map[string]int64{"a": 10, "b": 20})
	require.NoError(t, err)
	require.EqualValues(t, 20, n, "objects the Namespace holds add nothing")
	n, err = q.NewLFSBytes(ctx, other, map[string]int64{"a": 10, "b": 20})
	require.NoError(t, err)
	require.EqualValues(t, 30, n)

	down := errors.New("database is unavailable")
	q.FailNext(down)
	_, err = q.NewLFSBytes(ctx, ns, map[string]int64{"a": 1})
	require.ErrorIs(t, err, down)
	q.FailNext(down)
	err = q.CheckQuota(ctx, ns, 1000)
	require.ErrorIs(t, err, down)
	require.NotErrorIs(t, err, upstream.ErrQuotaExceeded, "a failed check is not a refusal")

	q.SetLimit(ns, 0)
	require.NoError(t, q.CheckQuota(ctx, ns, 1000), "a zero limit removes the quota")
}
