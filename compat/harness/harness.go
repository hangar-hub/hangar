// Package harness starts a real Hangar binary for compatibility tests: on a
// random port, with a temporary SQLite database and Blob store, and waits
// until /readyz answers. compat suites in Go use Start, and call Main from
// TestMain so the binary built for them is removed afterwards; the pytest
// suites use compat/hangar_instance.py, which does the same.
package harness

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// EnvBinary names a prebuilt hangar binary to use instead of building one.
const EnvBinary = "HANGAR_BINARY"

// Options configure an Instance.
type Options struct {
	// Binary is the hangar binary; empty uses HANGAR_BINARY or builds ./cmd/hangar.
	Binary string
	// Set holds extra `key=value` configuration overrides.
	Set []string
	// Env holds extra environment variables.
	Env []string
	// StartTimeout bounds the wait for /readyz. Zero means 60 seconds.
	StartTimeout time.Duration
}

// Instance is a running Hangar.
type Instance struct {
	// URL is the base URL, such as http://127.0.0.1:54321.
	URL     string
	DataDir string
	cmd     *exec.Cmd
	logs    *syncBuffer
	done    chan error
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// Logs returns everything the process wrote to stdout and stderr.
func (i *Instance) Logs() string { return i.logs.String() }

// builder builds the hangar binary once into a temporary directory and
// removes that directory again when the test process ends.
type builder struct {
	once  sync.Once
	build func(out string) error
	dir   string
	path  string
	err   error

	mu     sync.Mutex
	inMain bool
}

func (b *builder) binary() (string, error) {
	b.mu.Lock()
	inMain := b.inMain
	b.mu.Unlock()
	if !inMain {
		return "", errors.New("call harness.Main from the package's TestMain, so the built binary is removed when the tests end")
	}
	b.once.Do(func() {
		dir, err := os.MkdirTemp("", "hangar-compat-bin-")
		if err != nil {
			b.err = err
			return
		}
		b.dir = dir
		path := filepath.Join(dir, "hangar")
		if runtime.GOOS == "windows" {
			path += ".exe"
		}
		if err := b.build(path); err != nil {
			b.err = err
			return
		}
		b.path = path
	})
	return b.path, b.err
}

// run runs the tests with the builder enabled, then removes what it built.
func (b *builder) run(m interface{ Run() int }) int {
	b.mu.Lock()
	b.inMain = true
	b.mu.Unlock()
	code := m.Run()
	b.mu.Lock()
	b.inMain = false
	b.mu.Unlock()
	if b.dir != "" {
		if err := os.RemoveAll(b.dir); err != nil {
			fmt.Fprintf(os.Stderr, "harness: remove the built hangar binary: %v\n", err)
			if code == 0 {
				code = 1
			}
		}
	}
	return code
}

func goBuild(out string) error {
	cmd := exec.Command("go", "build", "-o", out, "./cmd/hangar")
	cmd.Dir = RepoRoot()
	if b, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("go build: %w: %s", err, b)
	}
	return nil
}

var defaultBuilder = &builder{build: goBuild}

// Main runs a compat package's tests and then removes the hangar binary that
// Binary or Start built for them. Every package that uses Binary or Start
// without HANGAR_BINARY must call it from TestMain:
//
//	func TestMain(m *testing.M) { os.Exit(harness.Main(m)) }
func Main(m *testing.M) int { return defaultBuilder.run(m) }

// RepoRoot returns the repository root, found from this source file.
func RepoRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

// Binary returns a hangar binary: HANGAR_BINARY if set, or one built from
// this checkout once per test process. The built binary is removed when Main
// returns.
func Binary(t testing.TB) string {
	t.Helper()
	if b := os.Getenv(EnvBinary); b != "" {
		return b
	}
	path, err := defaultBuilder.binary()
	if err != nil {
		t.Fatalf("build hangar: %v", err)
	}
	return path
}

// FreePort returns a free TCP port on the loopback interface.
func FreePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

// Start runs a Hangar Instance for the test and stops it when the test ends.
func Start(t testing.TB, opts Options) *Instance {
	t.Helper()
	bin := opts.Binary
	if bin == "" {
		bin = Binary(t)
	}
	port, err := FreePort()
	if err != nil {
		t.Fatalf("free port: %v", err)
	}
	dir := t.TempDir()
	url := fmt.Sprintf("http://127.0.0.1:%d", port)
	args := []string{"server",
		"--set", fmt.Sprintf("server.listen=127.0.0.1:%d", port),
		"--set", "server.public_url=" + url,
		"--set", "server.data_dir=" + dir,
		"--set", "database.dsn=" + filepath.Join(dir, "hangar.db"),
		"--set", "storage.fs.path=" + filepath.Join(dir, "blobs"),
		"--set", "git.cache_dir=" + filepath.Join(dir, "git-cache"),
	}
	for _, s := range opts.Set {
		args = append(args, "--set", s)
	}
	inst := &Instance{URL: url, DataDir: dir, logs: &syncBuffer{}, done: make(chan error, 1)}
	inst.cmd = exec.Command(bin, args...)
	inst.cmd.Env = append(filterEnv(os.Environ()), opts.Env...)
	inst.cmd.Stdout = inst.logs
	inst.cmd.Stderr = inst.logs
	if err := inst.cmd.Start(); err != nil {
		t.Fatalf("start hangar: %v", err)
	}
	go func() { inst.done <- inst.cmd.Wait() }()
	t.Cleanup(func() {
		if err := inst.Stop(30 * time.Second); err != nil {
			t.Errorf("stop hangar: %v\n%s", err, inst.Logs())
		}
	})
	timeout := opts.StartTimeout
	if timeout == 0 {
		timeout = 60 * time.Second
	}
	if err := inst.waitReady(timeout); err != nil {
		t.Fatalf("hangar did not become ready: %v\n%s", err, inst.Logs())
	}
	return inst
}

// filterEnv drops HANGAR_* variables from the parent, so the Instance
// depends only on the options.
func filterEnv(env []string) []string {
	out := env[:0:0]
	for _, kv := range env {
		if !strings.HasPrefix(kv, "HANGAR_") {
			out = append(out, kv)
		}
	}
	return out
}

func (i *Instance) waitReady(timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	client := &http.Client{Timeout: 2 * time.Second}
	for {
		select {
		case err := <-i.done:
			i.done <- err
			return fmt.Errorf("process exited: %w", err)
		default:
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, i.URL+"/readyz", nil)
		resp, err := client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// Stop sends SIGTERM (or kills on Windows) and waits for the process to exit.
func (i *Instance) Stop(timeout time.Duration) error {
	if i.cmd.Process == nil {
		return nil
	}
	select {
	case err := <-i.done:
		i.done <- err
		return nil
	default:
	}
	if runtime.GOOS == "windows" {
		_ = i.cmd.Process.Kill()
	} else {
		_ = i.cmd.Process.Signal(syscall.SIGTERM)
	}
	select {
	case err := <-i.done:
		i.done <- err
		var exit *exec.ExitError
		if err != nil && !errors.As(err, &exit) {
			return err
		}
		return nil
	case <-time.After(timeout):
		_ = i.cmd.Process.Kill()
		return errors.New("hangar did not stop in time and was killed")
	}
}
