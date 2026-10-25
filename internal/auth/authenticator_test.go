package auth_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"

	"github.com/hangar-hub/hangar/internal/auth"
	"github.com/hangar-hub/hangar/internal/auth/authtest"
	"github.com/hangar-hub/hangar/internal/config"
	"github.com/hangar-hub/hangar/internal/db"
	"github.com/hangar-hub/hangar/internal/db/dbtest"
	"github.com/hangar-hub/hangar/internal/httpx"
	"github.com/hangar-hub/hangar/internal/model"
)

func start() time.Time { return time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC) }

func newSvc(t *testing.T, d *db.DB) (*auth.Service, *authtest.Clock) {
	t.Helper()
	clock := authtest.NewClock(start())
	s, _ := authtest.NewService(t, d, authtest.RealOptions{Clock: clock})
	return s, clock
}

func mustUser(t *testing.T, s *auth.Service, name string, admin bool) *auth.User {
	t.Helper()
	u, err := s.Users.Create(context.Background(), auth.NewUser{Username: name, IsAdmin: admin}, model.System())
	require.NoError(t, err)
	return u
}

func authReq(t *testing.T, s *auth.Service, method string, set func(r *http.Request)) (model.Principal, error) {
	t.Helper()
	r := httptest.NewRequest(method, "http://hangar.test/api/hangar/v1/me", nil)
	if set != nil {
		set(r)
	}
	return s.Authenticate(r)
}

func TestAuthenticateTokens(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		ctx := context.Background()
		s, clock := newSvc(t, d)
		alice := mustUser(t, s, "alice", false)

		p, err := authReq(t, s, http.MethodGet, nil)
		require.NoError(t, err)
		require.True(t, p.IsAnonymous(), "no credentials is anonymous")

		exp := clock.Now().Add(time.Hour)
		tok, secret, err := s.TokenStore.Create(ctx, auth.NewToken{UserID: alice.ID, Name: "laptop", Scope: model.ScopeWrite,
			Restrictions: model.TokenRestrictions{Namespaces: []string{"alice"}}, ExpiresAt: &exp})
		require.NoError(t, err)
		require.True(t, auth.LooksLikeToken(secret), "secret has the hgr_ form")
		require.Equal(t, secret[:8], tok.Prefix)

		p, err = authReq(t, s, http.MethodPost, func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+secret) })
		require.NoError(t, err, "bearer tokens need no CSRF header")
		require.Equal(t, alice.ID, p.UserID)
		require.Equal(t, model.AuthToken, p.Method)
		require.Equal(t, model.ScopeWrite, p.Token.Scope)
		require.Equal(t, []string{"alice"}, p.Token.Restrictions.Namespaces)
		require.Equal(t, tok.ID, p.Token.ID)

		gitReq := httptest.NewRequest(http.MethodGet, "http://hangar.test/alice/m.git/info/refs?service=git-upload-pack", nil)
		gitReq.SetBasicAuth("anything", secret)
		p, err = s.Authenticate(gitReq)
		require.NoError(t, err, "git sends the token as a Basic password with any username")
		require.Equal(t, alice.ID, p.UserID)

		// last_used_at is written at most once a minute.
		got, err := s.TokenStore.Get(ctx, tok.ID)
		require.NoError(t, err)
		require.NotNil(t, got.LastUsedAt)
		first := *got.LastUsedAt
		clock.Advance(30 * time.Second)
		_, err = authReq(t, s, http.MethodGet, func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+secret) })
		require.NoError(t, err)
		got, _ = s.TokenStore.Get(ctx, tok.ID)
		require.True(t, got.LastUsedAt.Equal(first))
		clock.Advance(31 * time.Second)
		_, err = authReq(t, s, http.MethodGet, func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+secret) })
		require.NoError(t, err)
		got, _ = s.TokenStore.Get(ctx, tok.ID)
		require.True(t, got.LastUsedAt.After(first))

		for name, h := range map[string]string{
			"unknown token":  "Bearer hgr_" + "0123456789012345678901234567890123456789",
			"malformed":      "Bearer hgr_short",
			"other scheme":   "Digest abc",
			"empty bearer":   "Bearer ",
			"unknown bearer": "Bearer not-a-token",
		} {
			_, err = authReq(t, s, http.MethodGet, func(r *http.Request) { r.Header.Set("Authorization", h) })
			require.ErrorIs(t, err, auth.ErrInvalidCredentials, name)
			require.ErrorIs(t, err, httpx.ErrInvalidCredentials, name)
		}

		// Expiry.
		clock.Advance(time.Hour)
		_, err = authReq(t, s, http.MethodGet, func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+secret) })
		require.ErrorIs(t, err, auth.ErrInvalidCredentials, "expired")

		// Revocation, and disabled users.
		_, secret2, err := s.TokenStore.Create(ctx, auth.NewToken{UserID: alice.ID, Name: "ci", Scope: model.ScopeRead})
		require.NoError(t, err)
		bob := mustUser(t, s, "bob", false)
		require.ErrorIs(t, s.TokenStore.Revoke(ctx, tok.ID, bob.Principal(model.AuthSession)), auth.ErrNotFound, "others cannot revoke")
		disabled := true
		_, err = s.Users.Update(ctx, alice.ID, auth.UserUpdate{Disabled: &disabled}, model.System())
		require.NoError(t, err)
		_, err = authReq(t, s, http.MethodGet, func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+secret2) })
		require.ErrorIs(t, err, auth.ErrInvalidCredentials, "disabled users' tokens stop working")
		enabled := false
		_, err = s.Users.Update(ctx, alice.ID, auth.UserUpdate{Disabled: &enabled}, model.System())
		require.NoError(t, err)
		_, err = authReq(t, s, http.MethodGet, func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+secret2) })
		require.NoError(t, err)
		list, err := s.TokenStore.List(ctx, alice.ID)
		require.NoError(t, err)
		require.Len(t, list, 2)
		require.NoError(t, s.TokenStore.Revoke(ctx, list[0].ID, alice.Principal(model.AuthSession)))
		_, err = authReq(t, s, http.MethodGet, func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+secret2) })
		require.ErrorIs(t, err, auth.ErrInvalidCredentials, "revoked")
		require.ErrorIs(t, s.TokenStore.Revoke(ctx, list[0].ID, alice.Principal(model.AuthSession)), auth.ErrNotFound)
	})
}

func TestTokenValidation(t *testing.T) {
	ctx := context.Background()
	s, clock := newSvc(t, dbtest.SQLite(t))
	alice := mustUser(t, s, "alice", false)
	past := clock.Now().Add(-time.Minute)
	for name, nt := range map[string]auth.NewToken{
		"no name":         {UserID: alice.ID, Scope: model.ScopeRead},
		"bad scope":       {UserID: alice.ID, Name: "x", Scope: "owner"},
		"past expiry":     {UserID: alice.ID, Name: "x", Scope: model.ScopeRead, ExpiresAt: &past},
		"bad namespace":   {UserID: alice.ID, Name: "x", Scope: model.ScopeRead, Restrictions: model.TokenRestrictions{Namespaces: []string{"-x"}}},
		"bad repo":        {UserID: alice.ID, Name: "x", Scope: model.ScopeRead, Restrictions: model.TokenRestrictions{Repos: []string{"alice/x"}}},
		"bad repo type":   {UserID: alice.ID, Name: "x", Scope: model.ScopeRead, Restrictions: model.TokenRestrictions{Repos: []string{"space:alice/x"}}},
		"nested repo":     {UserID: alice.ID, Name: "x", Scope: model.ScopeRead, Restrictions: model.TokenRestrictions{Repos: []string{"model:alice/x/y"}}},
		"name too long":   {UserID: alice.ID, Name: string(make([]byte, 101)), Scope: model.ScopeRead},
		"whitespace name": {UserID: alice.ID, Name: "   ", Scope: model.ScopeRead},
	} {
		_, _, err := s.TokenStore.Create(ctx, nt)
		require.ErrorIs(t, err, auth.ErrInvalid, name)
	}
	_, _, err := s.TokenStore.Create(ctx, auth.NewToken{UserID: uuid.New(), Name: "x", Scope: model.ScopeRead})
	require.ErrorIs(t, err, auth.ErrNotFound)
	tok, _, err := s.TokenStore.Create(ctx, auth.NewToken{UserID: alice.ID, Name: "x", Scope: model.ScopeRead,
		Restrictions: model.TokenRestrictions{Repos: []string{"models:alice/x"}}})
	require.NoError(t, err)
	require.Equal(t, []string{"model:alice/x"}, tok.Restrictions.Repos, "plural repo types are normalised")
}

func TestAuthenticateSessionsAndCSRF(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		ctx := context.Background()
		s, clock := newSvc(t, d)
		alice := mustUser(t, s, "alice", false)

		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "http://hangar.test/api/hangar/v1/auth/login", nil)
		req.Header.Set("User-Agent", "test-browser")
		sess, err := s.StartSession(ctx, rec, req, alice)
		require.NoError(t, err)
		cookies := rec.Result().Cookies()
		var sessionCookie, csrfCookie *http.Cookie
		for _, c := range cookies {
			switch c.Name {
			case auth.SessionCookie:
				sessionCookie = c
			case auth.CSRFCookie:
				csrfCookie = c
			}
		}
		require.NotNil(t, sessionCookie)
		require.True(t, sessionCookie.HttpOnly)
		require.Equal(t, http.SameSiteLaxMode, sessionCookie.SameSite)
		require.False(t, sessionCookie.Secure, "public_url is http")
		require.NotNil(t, csrfCookie)
		require.False(t, csrfCookie.HttpOnly, "the UI reads the CSRF cookie")
		require.Equal(t, sess.CSRFToken, csrfCookie.Value)

		withCookie := func(r *http.Request) { r.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: sessionCookie.Value}) }
		p, err := authReq(t, s, http.MethodGet, withCookie)
		require.NoError(t, err)
		require.Equal(t, model.AuthSession, p.Method)
		require.Equal(t, sess.ID, p.SessionID)
		require.Equal(t, alice.ID, p.UserID)
		require.Nil(t, p.Token)

		_, err = authReq(t, s, http.MethodPost, withCookie)
		require.ErrorIs(t, err, auth.ErrCSRF, "state-changing calls need the CSRF header")
		_, err = authReq(t, s, http.MethodDelete, func(r *http.Request) { withCookie(r); r.Header.Set(auth.CSRFHeader, "wrong") })
		require.ErrorIs(t, err, auth.ErrCSRF)
		_, err = authReq(t, s, http.MethodPatch, func(r *http.Request) { withCookie(r); r.Header.Set(auth.CSRFHeader, sess.CSRFToken) })
		require.NoError(t, err)

		p, err = authReq(t, s, http.MethodGet, func(r *http.Request) { r.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: "nope"}) })
		require.NoError(t, err, "an unknown session cookie is stale, not an error")
		require.True(t, p.IsAnonymous())

		// Sessions list and revoke.
		list, err := s.Sessions.List(ctx, alice.ID)
		require.NoError(t, err)
		require.Len(t, list, 1)
		require.Equal(t, "test-browser", list[0].UserAgent)
		bob := mustUser(t, s, "bob", false)
		require.ErrorIs(t, s.Sessions.Revoke(ctx, bob.ID, sess.ID), auth.ErrNotFound, "others cannot revoke a session")

		// Expiry after session_ttl.
		clock.Advance(s.Config().Security.SessionTTL)
		p, err = authReq(t, s, http.MethodGet, withCookie)
		require.NoError(t, err, "an expired session is stale, not an error")
		require.True(t, p.IsAnonymous(), "expired session")
		n, err := s.Sessions.DeleteExpired(ctx)
		require.NoError(t, err)
		require.Equal(t, int64(1), n)

		// Logout clears the cookie and the row.
		sess2, secret2, err := s.Sessions.Create(ctx, alice.ID, "", "")
		require.NoError(t, err)
		rec = httptest.NewRecorder()
		req = httptest.NewRequest(http.MethodPost, "http://hangar.test/api/hangar/v1/auth/logout", nil)
		req.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: secret2})
		s.EndSession(ctx, rec, req)
		_, err = s.Sessions.Get(ctx, sess2.ID)
		require.ErrorIs(t, err, auth.ErrNotFound)
		for _, c := range rec.Result().Cookies() {
			require.Equal(t, -1, c.MaxAge, c.Name)
		}

		// Disabling a user ends their sessions.
		_, secret3, err := s.Sessions.Create(ctx, alice.ID, "", "")
		require.NoError(t, err)
		disabled := true
		_, err = s.Users.Update(ctx, alice.ID, auth.UserUpdate{Disabled: &disabled}, model.System())
		require.NoError(t, err)
		p, err = authReq(t, s, http.MethodGet, func(r *http.Request) { r.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: secret3}) })
		require.NoError(t, err)
		require.True(t, p.IsAnonymous(), "a disabled user's session is stale")
	})
}

func TestSecureCookiesOnHTTPS(t *testing.T) {
	d := dbtest.SQLite(t)
	s, _ := authtest.NewService(t, d, authtest.RealOptions{Config: func(c *config.Config) { c.Server.PublicURL = "https://hangar.example.com" }})
	u := mustUser(t, s, "alice", false)
	rec := httptest.NewRecorder()
	_, err := s.StartSession(context.Background(), rec, httptest.NewRequest(http.MethodPost, "/", nil), u)
	require.NoError(t, err)
	for _, c := range rec.Result().Cookies() {
		require.True(t, c.Secure, c.Name)
	}
}

// TestHostPrefixedCookiesOnHTTPS covers security review finding 14: on an
// HTTPS Instance the session and CSRF cookies are __Host- cookies (Secure,
// Path=/, no Domain), so a sibling subdomain cannot set or overwrite them.
// An unprefixed session cookie, from an Instance before the change or
// planted by a sibling subdomain, is not accepted and is cleared, so the
// user signs in once more.
func TestHostPrefixedCookiesOnHTTPS(t *testing.T) {
	d := dbtest.SQLite(t)
	s, _ := authtest.NewService(t, d, authtest.RealOptions{Config: func(c *config.Config) { c.Server.PublicURL = "https://hangar.example.com" }})
	u := mustUser(t, s, "alice", false)
	rec := httptest.NewRecorder()
	sess, err := s.StartSession(context.Background(), rec, httptest.NewRequest(http.MethodPost, "/", nil), u)
	require.NoError(t, err)
	require.Equal(t, "__Host-hangar_session", s.SessionCookieName())
	require.Equal(t, "__Host-hangar_csrf", s.CSRFCookieName())
	names := map[string]*http.Cookie{}
	for _, c := range rec.Result().Cookies() {
		names[c.Name] = c
		require.True(t, c.Secure, c.Name)
		require.Equal(t, "/", c.Path, c.Name)
		require.Empty(t, c.Domain, c.Name)
	}
	require.Contains(t, names, "__Host-hangar_session")
	require.Equal(t, sess.CSRFToken, names["__Host-hangar_csrf"].Value)
	secret := names["__Host-hangar_session"].Value

	p, err := authReq(t, s, http.MethodGet, func(r *http.Request) { r.AddCookie(&http.Cookie{Name: "__Host-hangar_session", Value: secret}) })
	require.NoError(t, err)
	require.Equal(t, u.ID, p.UserID)

	// The same secret under the old name is not accepted, and the response
	// clears the old cookies.
	p, err = authReq(t, s, http.MethodGet, func(r *http.Request) { r.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: secret}) })
	require.NoError(t, err)
	require.True(t, p.IsAnonymous(), "an unprefixed session cookie is not accepted on HTTPS")
	srv := s.Middleware(s, auth.WriteAuthError("https://hangar.example.com"))(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	req := httptest.NewRequest(http.MethodGet, "https://hangar.example.com/api/hangar/v1/me", nil)
	req.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: secret})
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	cleared := map[string]bool{}
	for _, c := range rec.Result().Cookies() {
		if c.MaxAge < 0 {
			cleared[c.Name] = true
		}
	}
	require.True(t, cleared[auth.SessionCookie] && cleared[auth.CSRFCookie], "the old cookies are cleared: %v", cleared)
}

// TestIdleSessionsExpire covers security review finding 14: a session
// that goes unused for security.session_idle_timeout (7 days by default)
// ends, though its 30-day lifetime has not, while one in use lives on.
func TestIdleSessionsExpire(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		ctx := context.Background()
		s, clock := newSvc(t, d)
		u := mustUser(t, s, "alice", false)
		_, idle, err := s.Sessions.Create(ctx, u.ID, "", "")
		require.NoError(t, err)
		_, used, err := s.Sessions.Create(ctx, u.ID, "", "")
		require.NoError(t, err)
		with := func(secret string) func(*http.Request) {
			return func(r *http.Request) { r.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: secret}) }
		}
		for range 3 {
			clock.Advance(3 * 24 * time.Hour)
			p, err := authReq(t, s, http.MethodGet, with(used))
			require.NoError(t, err)
			require.Equal(t, model.AuthSession, p.Method, "a session in use stays")
		}
		list, err := s.Sessions.List(ctx, u.ID)
		require.NoError(t, err)
		require.Len(t, list, 1, "the idle session is not listed")
		n, err := s.Sessions.DeleteExpired(ctx)
		require.NoError(t, err)
		require.Equal(t, int64(1), n, "cleanup removes the idle session")
		p, err := authReq(t, s, http.MethodGet, with(idle))
		require.NoError(t, err)
		require.True(t, p.IsAnonymous(), "after 9 days unused the session has ended")
	})
}

func TestWriteAuthError(t *testing.T) {
	write := auth.WriteAuthError("http://hangar.test")
	cases := []struct {
		path   string
		err    error
		status int
		header string
	}{
		{"/api/hangar/v1/me", auth.ErrCSRF, http.StatusForbidden, ""},
		{"/api/hangar/v1/me", auth.ErrCrossOrigin, http.StatusForbidden, ""},
		{"/api/repos/delete", auth.ErrCrossOrigin, http.StatusForbidden, ""},
		{"/a/b.git/git-receive-pack", auth.ErrCrossOrigin, http.StatusForbidden, ""},
		{"/api/hangar/v1/me", auth.ErrBasicNotAccepted, http.StatusUnauthorized, ""},
		{"/api/models/a/b", auth.ErrBasicNotAccepted, http.StatusUnauthorized, ""},
		{"/api/models/a/b", auth.ErrInvalidCredentials, http.StatusUnauthorized, ""},
		{"/a/b.git/info/refs", auth.ErrInvalidCredentials, http.StatusUnauthorized, `Basic realm="Hangar"`},
		{"/api/hangar/v1/me", context.Canceled, http.StatusInternalServerError, ""},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "http://hangar.test"+c.path, nil)
		r = r.WithContext(httpx.WithInfo(r.Context(), "req-1"))
		write(rec, r, c.err)
		require.Equal(t, c.status, rec.Code, c.path)
		require.Equal(t, c.header, rec.Header().Get("WWW-Authenticate"), c.path)
	}
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "http://hangar.test/api/models/a/b", nil)
	r = r.WithContext(httpx.WithInfo(r.Context(), "req-2"))
	write(rec, r, auth.ErrInvalidCredentials)
	require.Equal(t, httpx.MsgInvalidCredentials, rec.Header().Get("X-Error-Message"), "Hub clients get the Hub's message")

	// The client closed the request: 499 in every protocol, and a bad
	// credential is still a bad credential.
	closed := []struct {
		path   string
		err    error
		status int
	}{
		{"/api/hangar/v1/me", context.Canceled, 499},
		{"/ada/x/edit/main/README.md", errors.New("sql: interrupted"), 499},
		{"/api/models/a/b", context.Canceled, 499},
		{"/a/b.git/info/refs", context.DeadlineExceeded, 499},
		{"/api/models/a/b", auth.ErrInvalidCredentials, http.StatusUnauthorized},
	}
	for _, c := range closed {
		ctx, cancel := context.WithCancel(httpx.WithInfo(context.Background(), "req-3"))
		cancel()
		rec := httptest.NewRecorder()
		write(rec, httptest.NewRequest(http.MethodGet, "http://hangar.test"+c.path, nil).WithContext(ctx), c.err)
		require.Equal(t, c.status, rec.Code, c.path)
		if c.status == 499 {
			require.Contains(t, rec.Body.String(), "The client closed the request.", c.path)
		}
	}
}

// queryLog records the SQL a database runs.
type queryLog struct {
	mu      sync.Mutex
	queries []string
}

func (l *queryLog) BeforeQuery(ctx context.Context, _ *bun.QueryEvent) context.Context { return ctx }

func (l *queryLog) AfterQuery(_ context.Context, ev *bun.QueryEvent) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.queries = append(l.queries, ev.Query)
}

func (l *queryLog) take() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := l.queries
	l.queries = nil
	return out
}

// Every authenticated request pays for its lookups (02-architecture.md
// "Scale targets"), so a Token or session costs its own row and the
// user's, and never the user's linked OIDC providers, which a Principal
// does not carry.
func TestAuthenticateLooksUpOnlyWhatAPrincipalNeeds(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		ctx := context.Background()
		log := &queryLog{}
		logged := *d
		logged.DB = d.WithQueryHook(log)
		s, _ := newSvc(t, &logged)
		alice := mustUser(t, s, "alice", false)
		_, secret, err := s.TokenStore.Create(ctx, auth.NewToken{UserID: alice.ID, Name: "laptop", Scope: model.ScopeRead})
		require.NoError(t, err)
		// The first use records last_used_at.
		_, err = authReq(t, s, http.MethodGet, func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+secret) })
		require.NoError(t, err)
		log.take()

		p, err := authReq(t, s, http.MethodGet, func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+secret) })
		require.NoError(t, err)
		require.Equal(t, alice.ID, p.UserID)
		queries := log.take()
		require.Len(t, queries, 2, "the Token and its user: %v", queries)
		for _, q := range queries {
			require.NotContains(t, q, "identities")
		}
	})
}
