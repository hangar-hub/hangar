package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/blob"
	"github.com/jamesagarside/hangar/internal/config"
	"github.com/jamesagarside/hangar/internal/httpx"
)

// s3ConfigAt points the Blob store at an S3 endpoint.
func s3ConfigAt(t *testing.T, endpoint string, wait time.Duration) *config.Config {
	t.Helper()
	cfg := testConfig(t)
	cfg.Storage.Backend = config.BackendS3
	cfg.Storage.S3.Endpoint = endpoint
	cfg.Storage.S3.Bucket = "hangar"
	cfg.Storage.S3.AccessKeyID = "hangar"
	cfg.Storage.S3.SecretAccessKey = "hangar-secret"
	cfg.Storage.S3.UsePathStyle = true
	cfg.Storage.S3.StartupWait = wait
	return cfg
}

// failingOpen fails the first n calls with err, then returns store.
func failingOpen(n int, err error, store blob.Store) (func(context.Context) (blob.Store, error), *int) {
	calls := 0
	return func(context.Context) (blob.Store, error) {
		calls++
		if calls <= n {
			return nil, err
		}
		return store, nil
	}, &calls
}

// TestOpenBlobStoreWaitsForTheStore checks that a server whose S3 store
// does not answer yet keeps trying, logging what it waits for, and goes
// on once the store answers.
func TestOpenBlobStoreWaitsForTheStore(t *testing.T) {
	cfg := s3ConfigAt(t, "http://minio:9000", time.Minute)
	want := newEmptyBlobs()
	open, calls := failingOpen(2, errors.New(`s3 blob store: reach bucket "hangar": connection refused`), want)
	var logs strings.Builder
	got, err := openBlobStoreWaiting(context.Background(), cfg, open, slog.New(slog.NewTextHandler(&logs, nil)))
	require.NoError(t, err)
	require.Equal(t, want, got)
	require.Equal(t, 3, *calls)
	require.Equal(t, 2, strings.Count(logs.String(), "waiting for the Blob store"), logs.String())
	require.Contains(t, logs.String(), "endpoint=http://minio:9000")
	require.Contains(t, logs.String(), "bucket=hangar")
	require.Contains(t, logs.String(), "connection refused")
	require.Contains(t, logs.String(), "the Blob store answered")
	require.NotContains(t, logs.String(), "hangar-secret")
}

// TestOpenBlobStoreGivesUpWaiting checks that storage.s3.startup_wait is a
// bound: a store that never answers fails start-up once it has passed,
// with the last error.
func TestOpenBlobStoreGivesUpWaiting(t *testing.T) {
	cfg := s3ConfigAt(t, "http://minio:9000", 1500*time.Millisecond)
	open, _ := failingOpen(1000, errors.New("connection refused"), nil)
	start := time.Now()
	_, err := openBlobStoreWaiting(context.Background(), cfg, open, slog.New(slog.DiscardHandler))
	require.ErrorContains(t, err, "the Blob store did not answer within 1.5s (storage.s3.startup_wait)")
	require.ErrorContains(t, err, "connection refused")
	require.GreaterOrEqual(t, time.Since(start), 1500*time.Millisecond, "gave up before the wait was over")
	require.Less(t, time.Since(start), 10*time.Second)
}

// TestOpenBlobStoreFailsAtOnce checks the cases that are not retried: the
// filesystem backend, whose errors do not go away, and startup_wait 0.
func TestOpenBlobStoreFailsAtOnce(t *testing.T) {
	fsCfg := testConfig(t)
	fsCfg.Storage.S3.StartupWait = time.Minute
	for name, cfg := range map[string]*config.Config{
		"filesystem":        fsCfg,
		"s3 without a wait": s3ConfigAt(t, "http://minio:9000", 0),
	} {
		t.Run(name, func(t *testing.T) {
			open, calls := failingOpen(1, errors.New("no"), newEmptyBlobs())
			var logs strings.Builder
			_, err := openBlobStoreWaiting(context.Background(), cfg, open, slog.New(slog.NewTextHandler(&logs, nil)))
			require.EqualError(t, err, "no")
			require.Equal(t, 1, *calls)
			require.Empty(t, logs.String())
		})
	}
}

// TestOpenBlobStoreStopsWaitingOnShutdown checks that a signal during the
// wait ends it at once.
func TestOpenBlobStoreStopsWaitingOnShutdown(t *testing.T) {
	cfg := s3ConfigAt(t, "http://minio:9000", time.Hour)
	open, _ := failingOpen(1000, errors.New("connection refused"), nil)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := openBlobStoreWaiting(ctx, cfg, open, slog.New(slog.DiscardHandler))
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Less(t, time.Since(start), 5*time.Second)
}

// TestServeAnswersHealthWhileWaitingForTheBlobStore starts a server whose
// S3 endpoint refuses connections, as in a GitOps install whose object
// store starts after Hangar. The process must not exit into the kubelet's
// restart back-off: /healthz answers 200 for the startup and liveness
// probes, /readyz answers 503 and names the Blob store, and a shutdown
// signal during the wait stops the server cleanly. TestOpenBlobStore*
// cover the bound and the retries.
func TestServeAnswersHealthWhileWaitingForTheBlobStore(t *testing.T) {
	cfg := s3ConfigAt(t, "http://"+closedPort(t), 10*time.Minute)
	addr := closedPort(t)
	cfg.Server.Listen = addr

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	start := time.Now()
	done := make(chan error, 1)
	go func() { done <- serve(ctx, cfg, io.Discard, nil) }()

	client := &http.Client{Timeout: 5 * time.Second}
	get := func(path string) (int, []byte, error) {
		resp, err := client.Get("http://" + addr + path)
		if err != nil {
			return 0, nil, err
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, b, nil
	}
	// Start-up before the listener (telemetry, the database, migrations)
	// can take many seconds on a loaded machine.
	var healthz int
	var readyz []byte
	for deadline := time.Now().Add(2 * time.Minute); time.Now().Before(deadline) && healthz != http.StatusOK; {
		select {
		case err := <-done:
			t.Fatalf("serve exited after %s instead of waiting for the Blob store: %v", time.Since(start), err)
		case <-time.After(50 * time.Millisecond):
		}
		code, _, err := get("/healthz")
		if err != nil {
			continue
		}
		healthz = code
		var rc int
		rc, readyz, err = get("/readyz")
		require.NoError(t, err)
		require.Equal(t, http.StatusServiceUnavailable, rc, string(readyz))
	}
	require.Equal(t, http.StatusOK, healthz, "/healthz never answered while the server waited")
	var r struct {
		Failed []httpx.CheckResult `json:"failed"`
	}
	require.NoError(t, json.Unmarshal(readyz, &r), string(readyz))
	require.Contains(t, r.Failed, httpx.CheckResult{Name: "startup", Error: "the server is still starting: waiting for the Blob store to answer"})

	cancel()
	select {
	case err := <-done:
		require.NoError(t, err, "a shutdown during the wait is not an error")
	case <-time.After(60 * time.Second):
		t.Fatal("serve did not stop waiting on shutdown")
	}
}
