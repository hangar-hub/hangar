package auth_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/auth"
	"github.com/jamesagarside/hangar/internal/auth/authtest"
	"github.com/jamesagarside/hangar/internal/config"
	"github.com/jamesagarside/hangar/internal/db/dbtest"
	"github.com/jamesagarside/hangar/internal/model"
)

// TestBasicCredentialsOnlyForGit checks that a Token sent as an HTTP Basic
// password works on git and git-lfs routes only (docs/spec/05-auth.md), so
// Basic credentials a browser cached for a git URL cannot reach the Hub or
// native APIs (phase 3 security input 5).
func TestBasicCredentialsOnlyForGit(t *testing.T) {
	d := dbtest.SQLite(t)
	s, _ := authtest.NewService(t, d, authtest.RealOptions{Config: func(c *config.Config) { c.Server.PublicURL = "http://hangar.test" }})
	alice := mustUser(t, s, "alice", false)
	_, secret, err := s.TokenStore.Create(context.Background(), auth.NewToken{UserID: alice.ID, Name: "git", Scope: model.ScopeWrite})
	require.NoError(t, err)
	basic := func(method, path string) (model.Principal, error) {
		r := httptest.NewRequest(method, "http://hangar.test"+path, nil)
		r.SetBasicAuth("alice", secret)
		return s.Authenticate(r)
	}
	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/alice/m.git/info/refs?service=git-receive-pack"},
		{http.MethodPost, "/alice/m.git/git-receive-pack"},
		{http.MethodPost, "/alice/m/git-upload-pack"},
		{http.MethodPost, "/alice/m.git/info/lfs/objects/batch"},
		{http.MethodPut, "/datasets/alice/d.git/info/lfs/objects/" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"},
	} {
		p, err := basic(c.method, c.path)
		require.NoError(t, err, "%s %s", c.method, c.path)
		require.Equal(t, alice.ID, p.UserID, c.path)
	}
	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/api/hangar/v1/me"},
		{http.MethodPost, "/api/hangar/v1/repos/models/alice/m/zip"},
		{http.MethodDelete, "/api/repos/delete"},
		{http.MethodGet, "/api/whoami-v2"},
		{http.MethodGet, "/alice/m/resolve/main/config.json"},
		{http.MethodGet, "/v2/alice/m/manifests/latest"},
		{http.MethodPost, "/auth/logout"},
	} {
		_, err := basic(c.method, c.path)
		require.ErrorIs(t, err, auth.ErrBasicNotAccepted, "%s %s", c.method, c.path)
	}
	// The static UI ignores them, so a browser that cached them still
	// loads pages.
	for _, path := range []string{"/", "/alice/m", "/assets/index.js", "/healthz"} {
		p, err := basic(http.MethodGet, path)
		require.NoError(t, err, path)
		require.True(t, p.IsAnonymous(), path)
	}
	// Bearer Tokens work everywhere.
	r := httptest.NewRequest(http.MethodDelete, "http://hangar.test/api/repos/delete", nil)
	r.Header.Set("Authorization", "Bearer "+secret)
	p, err := s.Authenticate(r)
	require.NoError(t, err)
	require.Equal(t, alice.ID, p.UserID)
}

// TestCrossOriginBrowserCredentials checks that a state-changing request
// that carries a credential the browser adds on its own (Basic or the
// session cookie) is refused when the browser says it came from another
// site, and that clients sending neither Sec-Fetch-Site nor Origin, such
// as git, git-lfs, huggingface_hub, the CLI and Ollama, are unaffected
// (phase 3 security input 5).
func TestCrossOriginBrowserCredentials(t *testing.T) {
	d := dbtest.SQLite(t)
	s, _ := authtest.NewService(t, d, authtest.RealOptions{Config: func(c *config.Config) { c.Server.PublicURL = "https://hangar.example.com" }})
	alice := mustUser(t, s, "alice", false)
	_, secret, err := s.TokenStore.Create(context.Background(), auth.NewToken{UserID: alice.ID, Name: "git", Scope: model.ScopeWrite})
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	sess, err := s.StartSession(context.Background(), rec, httptest.NewRequest(http.MethodPost, "/", nil), alice)
	require.NoError(t, err)
	var cookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == auth.SessionCookie {
			cookie = c
		}
	}
	require.NotNil(t, cookie)

	type headers map[string]string
	browser := map[string]headers{
		"none":                    {},
		"same-origin":             {"Sec-Fetch-Site": "same-origin", "Origin": "https://hangar.example.com"},
		"typed into address bar":  {"Sec-Fetch-Site": "none"},
		"cross-site":              {"Sec-Fetch-Site": "cross-site", "Origin": "https://evil.example"},
		"same-site subdomain":     {"Sec-Fetch-Site": "same-site", "Origin": "https://evil.example.com"},
		"old browser, other site": {"Origin": "https://evil.example"},
		"old browser, public URL": {"Origin": "https://hangar.example.com"},
		"opaque origin":           {"Origin": "null"},
	}
	refused := map[string]bool{"cross-site": true, "same-site subdomain": true, "old browser, other site": true, "opaque origin": true}

	build := func(method, path string, h headers) *http.Request {
		r := httptest.NewRequest(method, "https://hangar.internal"+path, nil)
		for k, v := range h {
			r.Header.Set(k, v)
		}
		return r
	}
	for name, h := range browser {
		// Basic on git and LFS routes.
		for _, path := range []string{"/alice/m.git/git-receive-pack", "/alice/m.git/info/lfs/objects/batch"} {
			r := build(http.MethodPost, path, h)
			r.SetBasicAuth("alice", secret)
			_, err := s.Authenticate(r)
			if refused[name] {
				require.ErrorIs(t, err, auth.ErrCrossOrigin, "%s: Basic POST %s", name, path)
			} else {
				require.NoError(t, err, "%s: Basic POST %s", name, path)
			}
			// Safe methods are never refused.
			r = build(http.MethodGet, "/alice/m.git/info/refs?service=git-upload-pack", h)
			r.SetBasicAuth("alice", secret)
			_, err = s.Authenticate(r)
			require.NoError(t, err, "%s: Basic GET", name)
		}
		// The session cookie, with the right CSRF header.
		r := build(http.MethodPost, "/api/hangar/v1/repos", h)
		r.AddCookie(cookie)
		r.Header.Set(auth.CSRFHeader, sess.CSRFToken)
		p, err := s.Authenticate(r)
		if refused[name] {
			require.ErrorIs(t, err, auth.ErrCrossOrigin, "%s: cookie POST", name)
		} else {
			require.NoError(t, err, "%s: cookie POST", name)
			require.Equal(t, alice.ID, p.UserID)
		}
		// Bearer Tokens are never ambient, so the check does not apply.
		r = build(http.MethodDelete, "/api/repos/delete", h)
		r.Header.Set("Authorization", "Bearer "+secret)
		_, err = s.Authenticate(r)
		require.NoError(t, err, "%s: Bearer", name)
		// Signing in from another site is refused even without a cookie.
		r = build(http.MethodPost, "/api/hangar/v1/auth/login", h)
		_, err = s.Authenticate(r)
		if refused[name] {
			require.ErrorIs(t, err, auth.ErrCrossOrigin, "%s: sign-in", name)
		} else {
			require.NoError(t, err, "%s: sign-in", name)
		}
	}
}
