package httpx_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/httpx"
)

func TestHealth(t *testing.T) {
	var dbErr error
	calls := 0
	h := httpx.NewHealth(
		httpx.Check{Name: "database", Run: func(context.Context) error { return dbErr }},
		httpx.Check{Name: "blob", CacheFor: time.Hour, Run: func(context.Context) error { calls++; return nil }},
	)
	get := func(p string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		return rec
	}
	require.Equal(t, 200, get("/healthz").Code)
	require.Equal(t, 200, get("/readyz").Code)
	require.Equal(t, 200, get("/readyz").Code)
	require.Equal(t, 1, calls, "cached checks run once per CacheFor")

	dbErr = errors.New("connection refused")
	rec := get("/readyz")
	require.Equal(t, 503, rec.Code)
	require.JSONEq(t, `{"status":"not ready","failed":[{"name":"database","error":"connection refused"}]}`, rec.Body.String())
	require.Equal(t, 200, get("/healthz").Code, "liveness does not depend on dependencies")

	h.SetDraining()
	require.Equal(t, 503, get("/healthz").Code)
	require.Equal(t, 503, get("/readyz").Code)
	require.Equal(t, 404, get("/other").Code)
}

// TestReadyzAnswersWithinReadyTimeout checks that /readyz answers within
// its budget however many checks hang, so a probe that waits longer than
// ReadyTimeout always gets an answer, and that a check the budget leaves no
// time for fails without running and without caching a result.
func TestReadyzAnswersWithinReadyTimeout(t *testing.T) {
	hang := func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }
	ran := 0
	h := httpx.NewHealth(
		httpx.Check{Name: "database", Run: hang},
		httpx.Check{Name: "blob_store", Run: hang},
		httpx.Check{Name: "late", CacheFor: time.Hour, Run: func(context.Context) error { ran++; return nil }},
	)
	h.SetTimeouts(100*time.Millisecond, 150*time.Millisecond)

	start := time.Now()
	failed := h.Failures(context.Background())
	elapsed := time.Since(start)
	require.Less(t, elapsed, time.Second, "/readyz must answer within its budget")
	require.Len(t, failed, 3)
	require.Equal(t, "late", failed[2].Name)
	require.Contains(t, failed[2].Error, "readiness budget")
	require.Zero(t, ran, "a check started after the budget is spent does not run")

	// The skipped result was not cached: with time to spare the check runs.
	h.SetTimeouts(100*time.Millisecond, 10*time.Second)
	failed = h.Failures(context.Background())
	require.Len(t, failed, 2)
	require.Equal(t, 1, ran)
}

// TestReadyzSlowButHealthy checks that two checks that each take most of
// CheckTimeout, one after another, still pass: the budget covers both.
func TestReadyzSlowButHealthy(t *testing.T) {
	slow := func(ctx context.Context) error {
		select {
		case <-time.After(80 * time.Millisecond):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	h := httpx.NewHealth(httpx.Check{Name: "database", Run: slow}, httpx.Check{Name: "blob_store", Run: slow})
	h.SetTimeouts(100*time.Millisecond, 200*time.Millisecond)
	require.Empty(t, h.Failures(context.Background()))
	require.GreaterOrEqual(t, httpx.ReadyTimeout, 2*httpx.CheckTimeout,
		"the default budget must cover the database and Blob store checks at their full timeout")
}
