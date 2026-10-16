package httpx_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
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

// TestReadyzDoesNotCacheAProbeTimeout reproduces the preview outage: a
// probe gave up (kubelet's 1 second timeout) while the Blob store Stat was
// slow, the cache kept the probe's "context deadline exceeded", and the
// next probes answered 503 at once although the store had recovered.
func TestReadyzDoesNotCacheAProbeTimeout(t *testing.T) {
	release := make(chan struct{})
	var calls atomic.Int32
	h := httpx.NewHealth(httpx.Check{Name: "blob_store", CacheFor: 10 * time.Second, Run: func(ctx context.Context) error {
		if calls.Add(1) == 1 {
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	}})
	h.SetLogger(slog.New(slog.DiscardHandler))
	probe := func(timeout time.Duration) *httptest.ResponseRecorder {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil).WithContext(ctx))
		return rec
	}

	rec := probe(50 * time.Millisecond)
	require.Equal(t, 503, rec.Code, "the probe that gave up reports the slow check")
	require.Contains(t, rec.Body.String(), `"name":"blob_store"`)

	close(release)
	require.Eventually(t, func() bool { return probe(time.Second).Code == 200 }, 5*time.Second, 10*time.Millisecond,
		"once the store answers, the next probe is ready")
	require.Equal(t, int32(1), calls.Load(), "the slow run finished and its success was cached")
	require.Equal(t, 200, probe(time.Second).Code)
}

// TestReadyzSharesOneRunOfACachedCheck checks that probes arriving while
// a cached check runs wait for that run instead of starting another.
func TestReadyzSharesOneRunOfACachedCheck(t *testing.T) {
	release := make(chan struct{})
	var calls atomic.Int32
	h := httpx.NewHealth(httpx.Check{Name: "blob_store", CacheFor: time.Hour, Run: func(context.Context) error {
		calls.Add(1)
		<-release
		return nil
	}})
	results := make(chan []httpx.CheckResult, 3)
	for range 3 {
		go func() { results <- h.Failures(context.Background()) }()
	}
	require.Eventually(t, func() bool { return calls.Load() == 1 }, time.Second, time.Millisecond)
	close(release)
	for range 3 {
		require.Empty(t, <-results)
	}
	require.Equal(t, int32(1), calls.Load())
}

// TestReadyzRetriesACheckThatHitItsOwnTimeout checks that a run cut off by
// the check timeout is reported but not cached, so the next probe runs the
// check again instead of failing for the rest of CacheFor.
func TestReadyzRetriesACheckThatHitItsOwnTimeout(t *testing.T) {
	var calls atomic.Int32
	h := httpx.NewHealth(httpx.Check{Name: "blob_store", CacheFor: time.Hour, Run: func(ctx context.Context) error {
		if calls.Add(1) == 1 {
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
	}})
	httpx.SetCheckTimeout(h, 20*time.Millisecond)
	h.SetLogger(slog.New(slog.DiscardHandler))
	require.Equal(t, []httpx.CheckResult{{Name: "blob_store", Error: "context deadline exceeded"}}, h.Failures(context.Background()))
	require.Empty(t, h.Failures(context.Background()))
	require.Equal(t, int32(2), calls.Load())
	require.Empty(t, h.Failures(context.Background()), "a success is cached")
	require.Equal(t, int32(2), calls.Load())
}

// TestReadyzLogsFailedChecks checks that every failed check is logged
// with its name, so a 503 in the access log can be explained.
func TestReadyzLogsFailedChecks(t *testing.T) {
	var logs bytes.Buffer
	h := httpx.NewHealth(
		httpx.Check{Name: "database", Run: func(context.Context) error { return nil }},
		httpx.Check{Name: "blob_store", CacheFor: time.Hour, Run: func(context.Context) error { return errors.New("sentinel missing") }},
	)
	h.SetLogger(slog.New(slog.NewJSONHandler(&logs, nil)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	require.Equal(t, 503, rec.Code)
	var line map[string]any
	require.NoError(t, json.Unmarshal(logs.Bytes(), &line), logs.String())
	require.Equal(t, "WARN", line["level"])
	require.Equal(t, "readiness check failed", line["msg"])
	require.Equal(t, "blob_store", line["check"])
	require.Equal(t, "sentinel missing", line["error"])
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
