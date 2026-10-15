// Package gitservertest starts the git server on a local test HTTP server
// with the same middleware order as cmd/hangar, and runs the git CLI against
// it with a clean configuration. internal/gitserver's tests and the
// compat/git suite use it, with fakes or the real Repo service.
package gitservertest

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jamesagarside/hangar/internal/auth"
	"github.com/jamesagarside/hangar/internal/gitserver"
	"github.com/jamesagarside/hangar/internal/httpx"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/repo"
	"github.com/jamesagarside/hangar/internal/scan"
	"github.com/jamesagarside/hangar/internal/telemetry"
)

// InstanceKey is the Instance key test servers sign URLs with.
var InstanceKey = bytes.Repeat([]byte{7}, 32)

// Options configure a test server.
type Options struct {
	Repos       repo.Service
	LFS         repo.LFSStore
	Authn       httpx.Authenticator
	Authz       auth.Authz
	Quota       gitserver.QuotaChecker
	PullThrough gitserver.PullThrough
	// Downloads enforces scan.block_unsafe_downloads; optional.
	Downloads   *scan.Gate
	Instruments *telemetry.Instruments
	// Config is used as given, except that PublicURL is the test server's
	// URL and TempDir defaults to a test temporary directory.
	Config gitserver.Config
}

// Instance is a running test server.
type Instance struct {
	URL    string
	Server *gitserver.Server
	HTTP   *httptest.Server
}

// Handler wraps the git and git-lfs handlers in the middleware cmd/hangar
// uses: request IDs, authentication and protocol dispatch. The protocol is
// classified before authentication, so a bad credential gets git's Basic
// challenge.
func Handler(srv *gitserver.Server, authn httpx.Authenticator, publicURL string) http.Handler {
	h := httpx.Dispatch(httpx.Protocols{Git: srv.Git(), LFS: srv.LFS()}, publicURL)
	onError := func(w http.ResponseWriter, r *http.Request, _ error) {
		httpx.SetProtocol(r.Context(), httpx.Classify(r, "").Protocol())
		httpx.WriteUnauthorized(w, r)
	}
	h = httpx.Authenticate(authn, onError)(h)
	return httpx.RequestIDs(h)
}

// Start runs a git server for the test and stops it when the test ends.
func Start(t testing.TB, o Options) *Instance {
	t.Helper()
	hs := httptest.NewUnstartedServer(nil)
	base := "http://" + hs.Listener.Addr().String()
	cfg := o.Config
	cfg.PublicURL = base
	if cfg.TempDir == "" {
		cfg.TempDir = t.TempDir()
	}
	signer, err := httpx.NewSigner(InstanceKey)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := gitserver.New(cfg, gitserver.Deps{
		Repos: o.Repos, LFS: o.LFS, Authz: o.Authz, Signer: signer,
		Quota: o.Quota, PullThrough: o.PullThrough, Downloads: o.Downloads, Instruments: o.Instruments,
	})
	if err != nil {
		t.Fatal(err)
	}
	hs.Config.Handler = Handler(srv, o.Authn, base)
	hs.Start()
	t.Cleanup(func() {
		hs.Close()
		_ = srv.Close()
	})
	return &Instance{URL: base, Server: srv, HTTP: hs}
}

// RemoteURL returns the clone URL of a Repo, with token as the password
// when it is not empty, and with `.git` when dotGit is set.
func (i *Instance) RemoteURL(id model.RepoID, token string, dotGit bool) string {
	u, _ := url.Parse(i.URL)
	u.Path = "/" + id.Path()
	if dotGit {
		u.Path += ".git"
	}
	if token != "" {
		u.User = url.UserPassword("user", token)
	}
	return u.String()
}

// Git runs the git CLI with no system or global configuration and no
// credential prompts.
type Git struct {
	// Home is HOME for git, an empty directory.
	Home string
	// Env holds extra environment variables.
	Env []string
}

// NewGit returns a Git runner with a fresh HOME.
func NewGit(t testing.TB) *Git {
	t.Helper()
	home := t.TempDir()
	return &Git{Home: home}
}

func (g *Git) env() []string {
	env := []string{
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=" + filepath.Join(g.Home, ".gitconfig"),
		"HOME=" + g.Home,
		"PATH=" + os.Getenv("PATH"),
		"LANG=C", "LC_ALL=C",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=",
		"SSH_ASKPASS=",
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com",
	}
	return append(env, g.Env...)
}

// Run runs git in dir and returns combined output.
func (g *Git) Run(ctx context.Context, dir string, args ...string) (string, error) {
	return g.RunEnv(ctx, dir, nil, args...)
}

// RunEnv runs git in dir with extra environment variables.
func (g *Git) RunEnv(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(g.env(), env...)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// Must runs git and fails the test on error.
func (g *Git) Must(t testing.TB, dir string, args ...string) string {
	t.Helper()
	out, err := g.Run(context.Background(), dir, args...)
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}
