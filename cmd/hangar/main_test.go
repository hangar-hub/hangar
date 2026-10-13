package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/config"
)

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	dir := t.TempDir()
	cfg, err := config.Load(config.LoadOptions{Env: []string{}, Set: []string{
		"server.public_url=http://localhost:8080",
		"server.listen=127.0.0.1:0",
		"server.data_dir=" + dir,
		"database.dsn=" + filepath.Join(dir, "hangar.db"),
		"storage.fs.path=" + filepath.Join(dir, "blobs"),
		"git.cache_dir=" + filepath.Join(dir, "git-cache"),
		"telemetry.log_format=text",
		"server.shutdown_timeout=5s",
	}})
	require.NoError(t, err)
	return cfg
}

func TestServerServesHealthAndShutsDown(t *testing.T) {
	cfg := testConfig(t)
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan string, 1)
	var stderr bytes.Buffer
	done := make(chan error, 1)
	go func() { done <- serve(ctx, cfg, &stderr, ready) }()

	var addr string
	select {
	case addr = <-ready:
	case err := <-done:
		t.Fatalf("server exited: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("server did not start")
	}
	base := "http://" + addr

	get := func(path string) (*http.Response, string) {
		resp, err := http.Get(base + path)
		require.NoError(t, err)
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, string(b)
	}
	resp, body := get("/healthz")
	require.Equal(t, 200, resp.StatusCode)
	require.JSONEq(t, `{"status":"ok"}`, body)
	require.NotEmpty(t, resp.Header.Get("X-Request-Id"))

	resp, body = get("/readyz")
	require.Equal(t, 200, resp.StatusCode, body)

	// The native API is mounted from the generated router.
	resp, body = get("/api/hangar/v1/me")
	require.Equal(t, http.StatusNotImplemented, resp.StatusCode, body)
	resp, _ = get("/api/hangar/v1/repos?limit=lots")
	require.Equal(t, http.StatusBadRequest, resp.StatusCode, "the generated router validates parameters")
	require.Equal(t, "application/problem+json", resp.Header.Get("Content-Type"))

	// Other protocols are phase 0 stand-ins that answer in their own format.
	resp, _ = get("/api/models/a/b")
	require.Equal(t, "Hub API", resp.Header.Get("X-Hangar-Fake"))
	require.NotEmpty(t, resp.Header.Get("X-Error-Message"))

	require.NoError(t, checkReady(context.Background(), addr), "hangar healthcheck succeeds against a ready server")

	// First start generated and printed the secrets once.
	require.Contains(t, stderr.String(), "Admin password:")
	_, err := os.Stat(filepath.Join(cfg.Server.DataDir, config.SecretsFileName))
	require.NoError(t, err)

	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(30 * time.Second):
		t.Fatal("server did not shut down")
	}
	require.Error(t, checkReady(context.Background(), addr), "not ready after shutdown")
}

func TestServerRefusesInvalidConfig(t *testing.T) {
	cfg := testConfig(t)
	cfg.Server.PublicURL = ""
	cfg.Storage.Backend = "tape"
	err := serve(context.Background(), cfg, io.Discard, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "server.public_url is required")
	require.Contains(t, err.Error(), "storage.backend")
}

func TestServerValidatesBeforeCreatingDataDir(t *testing.T) {
	cfg := testConfig(t)
	cfg.Server.DataDir = filepath.Join(t.TempDir(), "not-yet", "data")
	cfg.Server.PublicURL = ""
	cfg.Storage.Backend = "tape"
	err := serve(context.Background(), cfg, io.Discard, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "server.public_url is required")
	require.Contains(t, err.Error(), "storage.backend")
	require.NotContains(t, err.Error(), "security.instance_key", "the instance key is generated on first start")
	_, statErr := os.Stat(cfg.Server.DataDir)
	require.ErrorIs(t, statErr, os.ErrNotExist, "an invalid configuration creates nothing")

	// A data directory that cannot be created still reports the full
	// validation error list, not the file system error.
	blocker := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(blocker, nil, 0o600))
	cfg.Server.DataDir = filepath.Join(blocker, "data")
	err = serve(context.Background(), cfg, io.Discard, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "server.public_url is required")
	require.Contains(t, err.Error(), "storage.backend")
	require.NotContains(t, err.Error(), "server.data_dir")
}

func execute(t *testing.T, args ...string) (string, int) {
	t.Helper()
	root := newRootCommand()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	err := root.Execute()
	if err != nil {
		return out.String() + err.Error(), 1
	}
	return out.String(), 0
}

func TestCommands(t *testing.T) {
	out, code := execute(t, "version")
	require.Equal(t, 0, code)
	require.Contains(t, out, "hangar dev")

	out, code = execute(t, "config", "reference")
	require.Equal(t, 0, code)
	require.Contains(t, out, "# Configuration reference")
	require.Contains(t, out, "HANGAR_SERVER_PUBLIC_URL")

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "x.db")
	out, code = execute(t, "admin", "migrate", "--set", "database.dsn="+dbPath)
	require.Equal(t, 0, code, out)
	require.Contains(t, out, "version 0001")

	out, code = execute(t, "config", "validate", "--set", "storage.backend=tape")
	require.Equal(t, 1, code)
	require.Contains(t, out, "storage.backend")
	out, code = execute(t, "config", "validate", "--public-url", "http://h", "--set", "security.instance_key=MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=", "--role", "all", "--listen", ":9")
	require.Equal(t, 0, code, out)
	require.Contains(t, out, "valid")

	require.Equal(t, 2, run([]string{"version", "--no-such-flag"}))
	require.Equal(t, 1, run([]string{"healthcheck", "--listen", "127.0.0.1:1"}))
	require.Equal(t, 0, run([]string{"version"}))
}

func TestReadyzReportsSchemaAndBlobStore(t *testing.T) {
	cfg := testConfig(t)
	d, err := openDatabase(context.Background(), cfg, false)
	require.NoError(t, err)
	defer d.Close()
	h := newHealth(deps{cfg: cfg, db: d, blobs: newEmptyBlobs()})
	failed := h.Failures(context.Background())
	names := []string{}
	for _, f := range failed {
		names = append(names, f.Name)
	}
	require.ElementsMatch(t, []string{"database", "blob_store"}, names)
	require.True(t, strings.Contains(failed[0].Error, "hangar admin migrate") || strings.Contains(failed[1].Error, "hangar admin migrate"))
	b, err := json.Marshal(failed)
	require.NoError(t, err)
	require.NotContains(t, string(b), cfg.Database.DSN, "failures do not reveal the DSN")
}
