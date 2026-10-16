//go:build compat

// Package ollama_test runs the real Ollama client against Hangar's Ollama
// registry (docs/spec/13-testing.md "Ollama"): pull by quant and by
// latest, show, a one-token generation, a private Repo with a registered
// key, and blobs served directly with no cross-host redirect.
//
// Every scenario runs against each target: the registry in the test
// process over the Repo service fake, and the hangar binary (built from
// ./cmd/hangar, or HANGAR_BINARY) on SQLite and, with Postgres tests on, on
// Postgres, so the wiring in cmd/hangar is checked as well as the package.
//
// Ollama runs from a local binary of the pinned version when
// HANGAR_OLLAMA_BINARY names one or `ollama` on PATH is that version, and
// otherwise from the pinned ollama/ollama image.
package ollama_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/db/dbtest"
	"github.com/jamesagarside/hangar/internal/httpx"
	"github.com/jamesagarside/hangar/internal/index/indextest"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/ollama"
)

// Pinned versions and fixtures.
const (
	ollamaImage = "ollama/ollama:" + ollama.TestedOllamaVersion
	// envBinary names a local ollama binary to use instead of the image.
	envBinary = "HANGAR_OLLAMA_BINARY"

	fixtureRepo   = "ggml-org/tiny-llamas"
	fixtureCommit = "99dd1a73db5a37100bd4ae633f4cfce6560e1567"
	fixtureFile   = "stories260K.gguf"
	fixtureSHA256 = "047bf46455a544931cff6fef14d7910154c56afbc23ab1c5e56a72e69912c04b"
)

// fixture returns stories260K.gguf from compat/.fixtures, downloading it
// once from huggingface.co at the pinned Commit.
func fixture(t *testing.T) []byte {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	p := filepath.Join(filepath.Dir(file), "..", ".fixtures", "models", fixtureRepo, fixtureFile)
	if b, err := os.ReadFile(p); err == nil && sha(b) == fixtureSHA256 {
		return b
	}
	u := "https://huggingface.co/" + fixtureRepo + "/resolve/" + fixtureCommit + "/" + fixtureFile
	resp, err := http.Get(u)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "download %s", u)
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, fixtureSHA256, sha(b), "the downloaded fixture has the pinned SHA-256")
	require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
	require.NoError(t, os.WriteFile(p, b, 0o644))
	return b
}

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// owner is the User who owns the test Repos and registers the Ollama key.
const owner = "alice"

// target is a Hangar Instance the scenarios run against.
type target interface {
	// addr is the host and port Ollama connects to.
	addr() string
	// createRepo creates a model Repo owned by owner and commits files.
	createRepo(t *testing.T, id model.RepoID, vis model.Visibility, files map[string][]byte)
	// registerKey registers an Ollama public key, in authorized_keys
	// form, for owner.
	registerKey(t *testing.T, authorizedKey string)
	// log returns a line per request Ollama sent: the status, method,
	// path, whether the URL was signed, and the Range header.
	log() []string
}

// targetFactory starts a target listening on listen.
type targetFactory struct {
	name  string
	start func(t *testing.T, listen string) target
}

// targets lists the Instances to test: the Ollama registry in the test
// process, and the hangar binary on SQLite and, when Postgres tests are on
// (HANGAR_TEST_POSTGRES=1 or HANGAR_TEST_POSTGRES_DSN), on Postgres.
func targets(t *testing.T) []targetFactory {
	out := []targetFactory{{"in-process", startInProcess}, {"binary-sqlite", startBinarySQLite}}
	if dbtest.PostgresEnabled() {
		out = append(out, targetFactory{"binary-postgres", startBinaryPostgres})
	} else {
		t.Logf("binary-postgres not run: set %s=1 or %s to run it", dbtest.EnvPostgres, dbtest.EnvPostgresDSN)
	}
	return out
}

// recorder records a line per request for target.log.
type recorder struct {
	mu       sync.Mutex
	statuses []string
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// wrap records every request next serves.
func (rec *recorder) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		signed := ""
		if r.URL.Query().Get(httpx.SigParam) != "" {
			signed = " signed"
		}
		rec.mu.Lock()
		rec.statuses = append(rec.statuses, fmt.Sprintf("%d %s %s%s range=%q", sw.status, r.Method, r.URL.Path, signed, r.Header.Get("Range")))
		rec.mu.Unlock()
	})
}

func (rec *recorder) log() []string {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return append([]string(nil), rec.statuses...)
}

// runner runs Ollama commands, locally or in a container.
type runner interface {
	// cli runs `ollama <args>` and returns its combined output.
	cli(ctx context.Context, args ...string) (string, error)
	// api is the base URL of the Ollama API.
	api() string
	// publicKey returns the Ollama key in authorized_keys form.
	publicKey(t *testing.T) string
	// host is how Ollama reaches the Instance listening on port.
	host(port string) string
}

type localRunner struct {
	bin, home, addr string
}

func (l *localRunner) env() []string {
	return append(os.Environ(), "HOME="+l.home, "OLLAMA_HOST="+l.addr, "OLLAMA_MODELS="+filepath.Join(l.home, "models"), "OLLAMA_NOHISTORY=1")
}

func (l *localRunner) cli(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, l.bin, args...)
	cmd.Env = l.env()
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (l *localRunner) api() string { return "http://" + l.addr }

func (l *localRunner) publicKey(t *testing.T) string {
	b, err := os.ReadFile(filepath.Join(l.home, ".ollama", "id_ed25519.pub"))
	require.NoError(t, err)
	return strings.TrimSpace(string(b))
}

func (l *localRunner) host(port string) string { return "127.0.0.1:" + port }

func startLocal(t *testing.T, bin string) *localRunner {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())
	l := &localRunner{bin: bin, home: t.TempDir(), addr: addr}
	out, err := exec.Command(bin, "--version").CombinedOutput()
	require.NoError(t, err)
	require.Contains(t, string(out), ollama.TestedOllamaVersion, "HANGAR_OLLAMA_BINARY must be Ollama %s", ollama.TestedOllamaVersion)
	cmd := exec.Command(bin, "serve")
	cmd.Env = l.env()
	var logs bytes.Buffer
	cmd.Stdout, cmd.Stderr = &logs, &logs
	prepareServe(cmd)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		stopServe(t, cmd, l.home)
		if t.Failed() {
			t.Logf("ollama serve output:\n%s", logs.String())
		}
	})
	waitUp(t, l.api())
	return l
}

type dockerRunner struct {
	id, port string
}

func (d *dockerRunner) cli(ctx context.Context, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, "docker", append([]string{"exec", d.id, "ollama"}, args...)...).CombinedOutput()
	return string(out), err
}

func (d *dockerRunner) api() string { return "http://127.0.0.1:" + d.port }

func (d *dockerRunner) publicKey(t *testing.T) string {
	out, err := exec.Command("docker", "exec", d.id, "cat", "/root/.ollama/id_ed25519.pub").Output()
	require.NoError(t, err)
	return strings.TrimSpace(string(out))
}

func (d *dockerRunner) host(port string) string { return "host.docker.internal:" + port }

func startDocker(t *testing.T) *dockerRunner {
	out, err := exec.Command("docker", "run", "-d", "--rm", "--add-host", "host.docker.internal:host-gateway",
		"-p", "127.0.0.1::11434", ollamaImage).CombinedOutput()
	require.NoError(t, err, string(out))
	id := strings.TrimSpace(string(out))
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", id).Run() })
	out, err = exec.Command("docker", "port", id, "11434/tcp").Output()
	require.NoError(t, err)
	_, port, _ := strings.Cut(strings.TrimSpace(strings.Split(string(out), "\n")[0]), ":")
	d := &dockerRunner{id: id, port: port}
	waitUp(t, d.api())
	return d
}

// localOllama returns HANGAR_OLLAMA_BINARY, or an ollama on PATH of the
// pinned version, or "" to use the image.
func localOllama() string {
	if bin := os.Getenv(envBinary); bin != "" {
		return bin
	}
	bin, err := exec.LookPath("ollama")
	if err != nil {
		return ""
	}
	out, err := exec.Command(bin, "--version").CombinedOutput()
	if err != nil || !strings.Contains(string(out), " "+ollama.TestedOllamaVersion) {
		return ""
	}
	return bin
}

func waitUp(t *testing.T, base string) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if resp, err := http.Get(base + "/api/version"); err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("ollama did not start at %s", base)
}

func generate(t *testing.T, r runner, name string) string {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"model": name, "prompt": "Once upon a time", "stream": false, "options": map[string]any{"num_predict": 1, "temperature": 0}})
	resp, err := http.Post(r.api()+"/api/generate", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(b))
	var out struct {
		Response  string `json:"response"`
		Done      bool   `json:"done"`
		EvalCount int    `json:"eval_count"`
	}
	require.NoError(t, json.Unmarshal(b, &out))
	require.True(t, out.Done)
	return out.Response
}

func TestOllamaClient(t *testing.T) {
	model260K := fixture(t)
	for _, f := range targets(t) {
		t.Run(f.name, func(t *testing.T) {
			r, listen := startRunner(t)
			scenarios(t, r, f.start(t, listen), model260K)
		})
	}
}

// startRunner starts an Ollama with an empty model store and a new key,
// so each target's pulls download every layer and the private Repo check
// starts from an unregistered key. It returns the address the target must
// listen on for that Ollama to reach it.
func startRunner(t *testing.T) (runner, string) {
	t.Helper()
	if bin := localOllama(); bin != "" {
		return startLocal(t, bin), "127.0.0.1:0"
	}
	return startDocker(t), "0.0.0.0:0"
}

// scenarios runs the client checks against one target.
func scenarios(t *testing.T, r runner, in target, model260K []byte) {
	_, port, _ := net.SplitHostPort(in.addr())
	host := r.host(port)

	public := model.RepoID{Type: model.RepoTypeModel, Namespace: owner, Name: "Tiny-Stories-GGUF"}
	in.createRepo(t, public, model.VisibilityPublic, map[string][]byte{
		"stories260K-Q8_0.gguf": model260K,
		"tiny-Q4_K_M.gguf":      indextest.LlamaGGUF(t, 15, "Q4_K"),
		"LICENSE":               []byte("MIT License\n\nPermission is hereby granted, free of charge, to any person obtaining a copy.\n"),
		"params":                []byte(`{"temperature": 0.1}`),
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	t.Run("pull by latest", func(t *testing.T) {
		name := host + "/alice/tiny-stories-gguf"
		out, err := r.cli(ctx, "pull", "--insecure", name)
		require.NoError(t, err, out)
		require.Contains(t, out, "success")
		list, err := r.cli(ctx, "list")
		require.NoError(t, err, list)
		require.Contains(t, list, host+"/alice/tiny-stories-gguf:latest")
	})

	q8 := host + "/alice/tiny-stories-gguf:Q8_0"
	t.Run("pull by quant", func(t *testing.T) {
		out, err := r.cli(ctx, "pull", "--insecure", strings.ToLower(q8))
		require.NoError(t, err, out)
		require.Contains(t, out, "success")
	})

	t.Run("show", func(t *testing.T) {
		out, err := r.cli(ctx, "show", strings.ToLower(q8))
		require.NoError(t, err, out)
		require.Contains(t, out, "llama")
		require.Contains(t, out, "temperature")
		require.Contains(t, out, "MIT License")
		tmpl, err := r.cli(ctx, "show", "--template", strings.ToLower(q8))
		require.NoError(t, err, tmpl)
		require.Contains(t, tmpl, "{{", "a Go template was served or derived")
	})

	t.Run("run one token", func(t *testing.T) {
		_ = generate(t, r, strings.ToLower(q8))
	})

	t.Run("missing quant", func(t *testing.T) {
		out, err := r.cli(ctx, "pull", "--insecure", host+"/alice/tiny-stories-gguf:Q2_K")
		require.Error(t, err, out)
	})

	t.Run("private Repo with a registered key", func(t *testing.T) {
		private := model.RepoID{Type: model.RepoTypeModel, Namespace: owner, Name: "private-stories"}
		in.createRepo(t, private, model.VisibilityPrivate, map[string][]byte{"stories260K-Q8_0.gguf": model260K})
		name := host + "/alice/private-stories:q8_0"
		out, err := r.cli(ctx, "pull", "--insecure", name)
		require.Error(t, err, "an unregistered key is refused: %s", out)
		in.registerKey(t, r.publicKey(t))
		out, err = r.cli(ctx, "pull", "--insecure", name)
		require.NoError(t, err, out)
		require.Contains(t, out, "success")
		_ = generate(t, r, name)
	})

	t.Run("blobs are served directly", func(t *testing.T) {
		var ranged, tokens int
		for _, line := range in.log() {
			code, _ := strconv.Atoi(strings.Fields(line)[0])
			require.False(t, code >= 300 && code < 400, "no redirects: %s", line)
			if strings.Contains(line, "/blobs/") && strings.Contains(line, "signed") && !strings.Contains(line, `range=""`) {
				ranged++
			}
			if strings.Contains(line, "/v2/token") && code == http.StatusOK {
				tokens++
			}
		}
		require.Positive(t, ranged, "Ollama downloaded parts from Hangar's signed URLs: %v", in.log())
		require.Positive(t, tokens, "the private pull went through the token endpoint")
	})
}
