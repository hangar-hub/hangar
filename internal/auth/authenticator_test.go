package auth_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/auth"
	"github.com/jamesagarside/hangar/internal/auth/authtest"
	"github.com/jamesagarside/hangar/internal/config"
	"github.com/jamesagarside/hangar/internal/db"
	"github.com/jamesagarside/hangar/internal/db/dbtest"
	"github.com/jamesagarside/hangar/internal/httpx"
	"github.com/jamesagarside/hangar/internal/model"
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

		p, err = authReq(t, s, http.MethodGet, func(r *http.Request) { r.SetBasicAuth("anything", secret) })
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

func TestWriteAuthError(t *testing.T) {
	write := auth.WriteAuthError("http://hangar.test")
	cases := []struct {
		path   string
		err    error
		status int
		header string
	}{
		{"/api/hangar/v1/me", auth.ErrCSRF, http.StatusForbidden, ""},
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
}
