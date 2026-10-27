package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/cli/clitest"
	"github.com/hangar-hub/hangar/internal/model"
)

func TestMain(m *testing.M) {
	pollInterval = 5 * time.Millisecond
	openBrowser = func(string) error { return nil }
	os.Exit(m.Run())
}

// env is an isolated home for one test: config, caches and LM Studio and
// Ollama folders all live under a temporary directory.
type env struct {
	t    *testing.T
	home string
	inst *clitest.Instance
	deps AdminDeps
}

func newEnv(t *testing.T) *env {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	t.Setenv("HF_HUB_CACHE", filepath.Join(home, "hf-cache"))
	t.Setenv("HF_HOME", "")
	t.Setenv(EnvNoKeychain, "1")
	t.Setenv(EnvURL, "")
	t.Setenv(EnvToken, "")
	t.Setenv(EnvConfigFile, "")
	t.Setenv("OLLAMA_MODELS", "")
	t.Setenv("SHELL", "/bin/zsh")
	return &env{t: t, home: home, inst: clitest.New(t)}
}

type result struct {
	stdout, stderr string
	code           int
	err            error
}

// run executes hangar with args against the fake Instance.
func (e *env) run(args ...string) result {
	return e.runIn("", args...)
}

func (e *env) runIn(stdin string, args ...string) result {
	e.t.Helper()
	root := &cobra.Command{Use: "hangar", SilenceUsage: true, SilenceErrors: true}
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return &ExitError{Code: ExitUsage, Err: err} })
	root.AddCommand(&cobra.Command{Use: "admin", Short: "Administer an Instance"})
	Register(root, Options{Version: "test", Admin: e.deps})
	SetDefaultCompletions(root)
	var out, errb bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errb)
	root.SetIn(strings.NewReader(stdin))
	root.SetArgs(args)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	err := root.ExecuteContext(ctx)
	return result{stdout: out.String(), stderr: errb.String(), code: ExitCode(err), err: err}
}

// ok runs a command that must succeed.
func (e *env) ok(args ...string) result {
	e.t.Helper()
	r := e.run(args...)
	require.NoError(e.t, r.err, "hangar %s\nstdout: %s\nstderr: %s", strings.Join(args, " "), r.stdout, r.stderr)
	return r
}

// login saves the fake Instance's URL and Token.
func (e *env) login() {
	e.t.Helper()
	e.ok("login", "--url", e.inst.URL, "--token", clitest.Token)
}

func mustID(t *testing.T, typ model.RepoType, s string) model.RepoID {
	t.Helper()
	id, err := model.ParseRepoID(typ, s)
	require.NoError(t, err)
	return id
}

// blobOf returns n bytes of repeatable content.
func blobOf(seed string, n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = seed[i%len(seed)] + byte(i/len(seed))
	}
	return b
}
