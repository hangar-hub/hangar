package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/config"
	"github.com/jamesagarside/hangar/internal/telemetry"
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

	// The native API is the generated router over the real handlers.
	resp, body = get("/api/hangar/v1/me")
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode, body)
	require.Equal(t, "application/problem+json", resp.Header.Get("Content-Type"))
	resp, _ = get("/api/hangar/v1/repos?limit=lots")
	require.Equal(t, http.StatusBadRequest, resp.StatusCode, "the generated router validates parameters")
	require.Equal(t, "application/problem+json", resp.Header.Get("Content-Type"))

	// The Hub API answers in its own error format.
	resp, _ = get("/api/models/a/b")
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	require.Equal(t, "RepoNotFound", resp.Header.Get("X-Error-Code"))
	require.Empty(t, resp.Header.Get("X-Hangar-Fake"), "no phase 0 stand-ins")

	// A bad git credential gets git's Basic challenge, because the
	// protocol is classified before authentication.
	req, _ := http.NewRequest(http.MethodGet, base+"/a/b.git/info/refs?service=git-upload-pack", nil)
	req.SetBasicAuth("user", "hgr_not_a_real_token")
	gitResp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = gitResp.Body.Close()
	require.Equal(t, http.StatusUnauthorized, gitResp.StatusCode)
	require.Contains(t, gitResp.Header.Get("WWW-Authenticate"), "Basic")
	resp, _ = get("/v2/")
	require.NotEqual(t, http.StatusNotFound, resp.StatusCode)

	// The bootstrap admin signs in with the password printed once.
	password := ""
	for _, line := range strings.Split(stderr.String(), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "Admin password: "); ok {
			password = v
		}
	}
	require.NotEmpty(t, password)
	login, err := http.Post(base+"/api/hangar/v1/auth/login", "application/json",
		strings.NewReader(`{"username":"hangar-admin","password":"`+password+`"}`))
	require.NoError(t, err)
	_ = login.Body.Close()
	require.Equal(t, http.StatusOK, login.StatusCode)
	req, _ = http.NewRequest(http.MethodGet, base+"/api/hangar/v1/admin/system", nil)
	for _, c := range login.Cookies() {
		req.AddCookie(c)
	}
	sys, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	b, _ := io.ReadAll(sys.Body)
	_ = sys.Body.Close()
	require.Equal(t, http.StatusOK, sys.StatusCode, string(b))
	require.Contains(t, string(b), `"blob_store":{"backend":"fs"}`)

	require.NoError(t, checkReady(context.Background(), addr), "hangar healthcheck succeeds against a ready server")

	// First start generated and printed the secrets once.
	require.Contains(t, stderr.String(), "Admin password:")
	_, err = os.Stat(filepath.Join(cfg.Server.DataDir, config.SecretsFileName))
	require.NoError(t, err)

	// The client can be left holding a connection it dialled but never
	// used; net/http counts such a connection as active for its first 5
	// seconds, as long as this test's shutdown_timeout. A client going
	// away closes it.
	http.DefaultClient.CloseIdleConnections()
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
	h := newHealth(d, newEmptyBlobs(), func() bool { return true })
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

func TestReadyzFailsUntilTheInstanceHasStarted(t *testing.T) {
	cfg := testConfig(t)
	ctx := context.Background()
	d, err := openDatabase(ctx, cfg, false)
	require.NoError(t, err)
	defer d.Close()
	require.NoError(t, d.Migrate(ctx))
	blobs := newEmptyBlobs()
	require.NoError(t, ensureSentinel(ctx, blobs))
	var started atomic.Bool
	h := newHealth(d, blobs, started.Load)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "the Instance is still starting")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	require.Equal(t, http.StatusOK, rec.Code, "liveness does not wait for startup")

	started.Store(true)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// TestFirstReadyAnswerServesEveryRoute polls /readyz from the moment the
// port opens, as compat/harness and load balancers do, and checks that the
// first 200 means the API is served: before, /readyz answered 200 while the
// starting handler still answered 404 to everything else.
func TestFirstReadyAnswerServesEveryRoute(t *testing.T) {
	cfg := testConfig(t)
	port, err := freePort()
	require.NoError(t, err)
	cfg.Server.Listen = "127.0.0.1:" + strconv.Itoa(port)
	base := "http://" + cfg.Server.Listen
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serve(ctx, cfg, io.Discard, nil) }()
	t.Cleanup(func() {
		cancel()
		require.NoError(t, <-done)
	})

	client := &http.Client{Timeout: 5 * time.Second}
	deadline := time.Now().Add(60 * time.Second)
	for {
		require.True(t, time.Now().Before(deadline), "/readyz did not answer 200 within 60 seconds")
		resp, err := client.Get(base + "/readyz")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		time.Sleep(time.Millisecond)
	}
	resp, err := client.Get(base + "/api/hangar/v1/me")
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode, "the native API is served once /readyz answers 200")
}

func freePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

// TestEveryCommandTakesJSON walks the whole binary's command tree, the
// server and admin commands here as well as the client commands from
// internal/cli, because 08-local-tools.md puts --json on every command.
func TestEveryCommandTakesJSON(t *testing.T) {
	var checked int
	var missing []string
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		for _, sub := range c.Commands() {
			if sub.Hidden || sub.Name() == "help" || sub.Name() == "completion" {
				continue
			}
			if sub.Runnable() {
				checked++
				if sub.Flag("json") == nil {
					missing = append(missing, sub.CommandPath())
				}
			}
			walk(sub)
		}
	}
	walk(newRootCommand())
	require.Empty(t, missing, "commands without --json")
	require.Greater(t, checked, 25)
}

func TestCommandsJSON(t *testing.T) {
	out, code := execute(t, "version", "--json")
	require.Equal(t, 0, code, out)
	var v versionInfo
	require.NoError(t, json.Unmarshal([]byte(out), &v))
	require.Equal(t, versionInfo{Version: "dev", Commit: "none", Date: "unknown"}, v)

	out, code = execute(t, "config", "reference", "--json")
	require.Equal(t, 0, code)
	var fields []referenceField
	require.NoError(t, json.Unmarshal([]byte(out), &fields))
	byKey := map[string]referenceField{}
	for _, f := range fields {
		byKey[f.Key] = f
	}
	require.Equal(t, "HANGAR_SERVER_PUBLIC_URL", byKey["server.public_url"].Env)
	require.Equal(t, "json", byKey["telemetry.log_format"].Default)
	require.True(t, byKey["security.instance_key"].Secret)

	dir := t.TempDir()
	out, code = execute(t, "admin", "migrate", "--json", "--set", "database.dsn="+filepath.Join(dir, "x.db"))
	require.Equal(t, 0, code, out)
	var sv schemaVersion
	require.NoError(t, json.Unmarshal([]byte(out), &sv))
	require.Equal(t, sv.Expected, sv.Current)
	require.Contains(t, sv.Current, "0001")
	require.NotNil(t, sv.Pending)
	require.Empty(t, sv.Pending)

	// Validation problems go to stdout as JSON, and the exit code is 1.
	out, code = execute(t, "config", "validate", "--json", "--set", "storage.backend=tape")
	require.Equal(t, 1, code)
	jsonPart := out[:strings.LastIndex(out, "}")+1]
	var val validation
	require.NoError(t, json.Unmarshal([]byte(jsonPart), &val), out)
	require.False(t, val.Valid)
	require.NotEmpty(t, val.Problems)
	require.True(t, slices.ContainsFunc(val.Problems, func(p string) bool { return strings.Contains(p, "storage.backend") }))
	out, code = execute(t, "config", "validate", "--json", "--public-url", "http://h", "--set", "security.instance_key=MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=")
	require.Equal(t, 0, code, out)
	require.JSONEq(t, `{"valid":true,"problems":[]}`, out)

	out, code = execute(t, "healthcheck", "--json", "--listen", "127.0.0.1:1")
	require.Equal(t, 1, code)
	var r readiness
	require.NoError(t, json.Unmarshal([]byte(out[:strings.LastIndex(out, "}")+1]), &r), out)
	require.False(t, r.Ready)
	require.Contains(t, r.Error, "not ready")
	require.Equal(t, 1, run([]string{"healthcheck", "--json", "--listen", "127.0.0.1:1"}))
	require.Equal(t, 0, run([]string{"version", "--json"}))
}

func TestHealthcheckJSONWhenReady(t *testing.T) {
	cfg := testConfig(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan string, 1)
	done := make(chan error, 1)
	go func() { done <- serve(ctx, cfg, io.Discard, ready) }()
	var addr string
	select {
	case addr = <-ready:
	case err := <-done:
		t.Fatalf("server exited: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("server did not start")
	}
	out, code := execute(t, "healthcheck", "--json", "--listen", addr)
	require.Equal(t, 0, code, out)
	require.JSONEq(t, `{"ready":true}`, out)
	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(30 * time.Second):
		t.Fatal("server did not shut down")
	}
}

func TestServerJSONSelectsJSONLogs(t *testing.T) {
	var f configFlags
	f.set = []string{"telemetry.log_format=text"}
	cfg, err := f.loadServer("", false)
	require.NoError(t, err)
	require.Equal(t, "text", cfg.Telemetry.LogFormat)
	cfg, err = f.loadServer("worker", true)
	require.NoError(t, err)
	require.Equal(t, "json", cfg.Telemetry.LogFormat, "--json overrides telemetry.log_format")
	require.Equal(t, "worker", cfg.Server.Role)
}

// TestWorkerServesOnlyHealth checks that a worker process serves the
// health endpoints and no protocol. A worker needs Postgres, so the test
// builds the handler directly over an Instance on SQLite.
func TestWorkerServesOnlyHealth(t *testing.T) {
	cfg := testConfig(t)
	ctx := context.Background()
	_, err := config.EnsureSecrets(cfg)
	require.NoError(t, err)
	tel, err := telemetry.Setup(ctx, telemetry.Options{Config: cfg.Telemetry, Version: version, Role: config.RoleWorker,
		Getenv: func(string) string { return "" }, Stdout: io.Discard})
	require.NoError(t, err)
	t.Cleanup(func() { _ = tel.Shutdown(context.Background()) })
	d, err := openDatabase(ctx, cfg, false)
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	require.NoError(t, d.Migrate(ctx))
	blobs, err := openBlobStore(ctx, cfg, tel)
	require.NoError(t, err)
	in, err := newInstance(ctx, cfg, tel, d, blobs)
	require.NoError(t, err)
	t.Cleanup(func() { in.close(ctx) })
	h, err := newHandler(ctx, in, newHealth(d, blobs, func() bool { return true }), true)
	require.NoError(t, err)
	for path, want := range map[string]int{"/healthz": http.StatusOK, "/readyz": http.StatusOK, "/api/hangar/v1/me": http.StatusNotFound,
		"/": http.StatusNotFound, "/api/models/a/b": http.StatusNotFound} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		require.Equal(t, want, rec.Code, path)
	}
}
