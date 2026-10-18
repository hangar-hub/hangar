package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/httpx"
)

// TestReadyzWaitsForTheFullHandler checks that /readyz answers 503 until
// the Instance is built and its handler installed: a launcher that waits
// for /readyz must never reach the health-only handler, which answers
// every API call with 404.
func TestReadyzWaitsForTheFullHandler(t *testing.T) {
	cfg := testConfig(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())
	cfg.Server.Listen = addr

	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan string, 1)
	done := make(chan error, 1)
	go func() { done <- serve(ctx, cfg, io.Discard, ready) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Error("server did not shut down")
		}
	})

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
	deadline := time.Now().Add(30 * time.Second)
	for {
		require.True(t, time.Now().Before(deadline), "the server never became ready")
		code, body, err := get("/readyz")
		if err == nil && code == http.StatusOK {
			break
		}
		if err == nil {
			require.Equal(t, http.StatusServiceUnavailable, code, string(body))
			var r struct {
				Failed []httpx.CheckResult `json:"failed"`
			}
			require.NoError(t, json.Unmarshal(body, &r), string(body))
			require.NotEmpty(t, r.Failed)
		}
		select {
		case err := <-done:
			t.Fatalf("server exited: %v", err)
		case <-time.After(time.Millisecond):
		}
	}
	// The first 200 means the API is served, not the start-up handler.
	code, body, err := get("/api/hangar/v1/me/tokens")
	require.NoError(t, err)
	require.Equal(t, http.StatusUnauthorized, code, string(body))
	code, _, err = get("/api/models/a/b")
	require.NoError(t, err)
	require.Equal(t, http.StatusUnauthorized, code)
	select {
	case got := <-ready:
		require.Equal(t, addr, got, "serve reports the address once it is ready")
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not report ready after /readyz answered 200")
	}
}

func TestStartupCheckFailsUntilStarted(t *testing.T) {
	cfg := testConfig(t)
	d, err := openDatabase(context.Background(), cfg, false)
	require.NoError(t, err)
	defer d.Close()
	require.NoError(t, d.Migrate(context.Background()))
	blobs := newEmptyBlobs()
	require.NoError(t, ensureSentinel(context.Background(), blobs))
	var started atomic.Bool
	h := newHealth(d, blobs, startupCheck(&started))
	require.Equal(t, []httpx.CheckResult{{Name: "startup", Error: "the server is still starting"}}, h.Failures(context.Background()))
	started.Store(true)
	require.Empty(t, h.Failures(context.Background()))
}

// TestWaitForSchemaWaitsForMigrate checks that a web or worker process
// waits for hangar admin migrate, logging why once, and starts as soon as
// the schema is current.
func TestWaitForSchemaWaitsForMigrate(t *testing.T) {
	cfg := testConfig(t)
	d, err := openDatabase(context.Background(), cfg, false)
	require.NoError(t, err)
	defer d.Close()
	var logs strings.Builder
	log := slog.New(slog.NewTextHandler(&logs, nil))

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, waitForSchema(ctx, d, log), context.DeadlineExceeded)
	require.Equal(t, 1, strings.Count(logs.String(), "waiting for hangar admin migrate"), logs.String())

	done := make(chan error, 1)
	go func() { done <- waitForSchema(context.Background(), d, log) }()
	require.NoError(t, d.Migrate(context.Background()))
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("waitForSchema did not notice the migration")
	}

	// A database that cannot answer is logged as such.
	require.NoError(t, d.Close())
	logs.Reset()
	ctx, cancel2 := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel2()
	require.Error(t, waitForSchema(ctx, d, log))
	require.Contains(t, logs.String(), "waiting for the database")
}

func TestServerCommandRefusesInvalidConfig(t *testing.T) {
	for _, use := range []string{"server", "worker"} {
		out, code := execute(t, use, "--set", "storage.backend=tape", "--set", "server.data_dir="+t.TempDir())
		require.Equal(t, 1, code, use)
		require.Contains(t, out, "storage.backend", use)
	}
}

// TestHealthcheckOutwaitsReadyz checks that hangar healthcheck waits longer
// than /readyz may take, so a slow but healthy dependency is not reported
// as not ready.
func TestHealthcheckOutwaitsReadyz(t *testing.T) {
	if readyzWait <= httpx.ReadyTimeout {
		t.Fatalf("readyzWait %s must be longer than httpx.ReadyTimeout %s", readyzWait, httpx.ReadyTimeout)
	}
}
