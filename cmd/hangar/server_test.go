package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/httpx"
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
	code, body, err := get("/api/hangar/v1/me")
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
