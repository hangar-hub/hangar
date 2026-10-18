package auth_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/auth"
	"github.com/hangar-hub/hangar/internal/auth/authtest"
	"github.com/hangar-hub/hangar/internal/config"
	"github.com/hangar-hub/hangar/internal/db"
	"github.com/hangar-hub/hangar/internal/db/dbtest"
	"github.com/hangar-hub/hangar/internal/httpx"
	"github.com/hangar-hub/hangar/internal/model"
)

// cookiesNamed returns the response's Set-Cookie entries for name.
func cookiesNamed(rec *httptest.ResponseRecorder, name string) []*http.Cookie {
	var out []*http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == name {
			out = append(out, c)
		}
	}
	return out
}

// requireCleared checks the response clears both session cookies once.
func requireCleared(t *testing.T, rec *httptest.ResponseRecorder, msg string) {
	t.Helper()
	for _, name := range []string{auth.SessionCookie, auth.CSRFCookie} {
		cs := cookiesNamed(rec, name)
		require.Len(t, cs, 1, "%s: %s", msg, name)
		require.Equal(t, -1, cs[0].MaxAge, "%s: %s", msg, name)
		require.Empty(t, cs[0].Value, "%s: %s", msg, name)
	}
}

// TestStaleSessionCookieIsAnonymous covers a browser whose session cookie
// was revoked, expired, disabled or never existed: every route treats it as
// anonymous, so the sign-in page loads, and the response clears the cookie.
func TestStaleSessionCookieIsAnonymous(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		ctx := context.Background()
		s, clock := newSvc(t, d)
		alice := mustUser(t, s, "alice", false)

		var seen model.Principal
		page := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			seen = httpx.Principal(r.Context())
			w.WriteHeader(http.StatusOK)
		})
		h := s.Middleware(s, auth.WriteAuthError("http://hangar.test"))(page)
		get := func(method, target, cookie string, set func(r *http.Request)) *httptest.ResponseRecorder {
			req := httptest.NewRequest(method, "http://hangar.test"+target, nil)
			if cookie != "" {
				req.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: cookie})
			}
			if set != nil {
				set(req)
			}
			rec := httptest.NewRecorder()
			seen = model.Principal{}
			h.ServeHTTP(rec, req.WithContext(httpx.WithInfo(req.Context(), "req")))
			return rec
		}

		// A live session is signed in and keeps its cookie.
		sess, secret, err := s.Sessions.Create(ctx, alice.ID, "", "")
		require.NoError(t, err)
		rec := get(http.MethodGet, "/auth/login", secret, nil)
		require.Equal(t, http.StatusOK, rec.Code)
		require.Equal(t, alice.ID, seen.UserID)
		require.Empty(t, rec.Result().Cookies(), "a live session cookie stays")

		// Revoked from another device.
		require.NoError(t, s.Sessions.Revoke(ctx, alice.ID, sess.ID))
		for _, target := range []string{"/auth/login", "/", "/arsenal", "/assets/x.js", "/api/hangar/v1/auth/providers", "/api/hangar/v1/me"} {
			rec = get(http.MethodGet, target, secret, nil)
			require.Equal(t, http.StatusOK, rec.Code, target)
			require.True(t, seen.IsAnonymous(), target)
			requireCleared(t, rec, "revoked: "+target)
		}
		// A state-changing call with a stale cookie is anonymous too; the
		// CSRF check belongs to a live session.
		rec = get(http.MethodPost, "/api/hangar/v1/auth/login", secret, nil)
		require.Equal(t, http.StatusOK, rec.Code)
		require.True(t, seen.IsAnonymous())
		requireCleared(t, rec, "revoked POST")

		// Never existed.
		rec = get(http.MethodGet, "/auth/login", "bogus", nil)
		require.Equal(t, http.StatusOK, rec.Code)
		require.True(t, seen.IsAnonymous())
		requireCleared(t, rec, "unknown")

		// Expired.
		_, expiring, err := s.Sessions.Create(ctx, alice.ID, "", "")
		require.NoError(t, err)
		clock.Advance(s.Config().Security.SessionTTL)
		rec = get(http.MethodGet, "/auth/login", expiring, nil)
		require.Equal(t, http.StatusOK, rec.Code)
		require.True(t, seen.IsAnonymous())
		requireCleared(t, rec, "expired")

		// The User was disabled behind the session's back.
		_, live, err := s.Sessions.Create(ctx, alice.ID, "", "")
		require.NoError(t, err)
		now := clock.Now()
		_, err = d.NewUpdate().Model((*db.User)(nil)).Set("disabled_at = ?", now).Where("id = ?", alice.ID).Exec(ctx)
		require.NoError(t, err)
		rec = get(http.MethodGet, "/auth/login", live, nil)
		require.Equal(t, http.StatusOK, rec.Code)
		require.True(t, seen.IsAnonymous())
		requireCleared(t, rec, "disabled")

		// An explicit bad credential still fails, and the cookie is not
		// what failed, so it stays.
		rec = get(http.MethodGet, "/api/hangar/v1/me", "bogus", func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer hgr_0123456789012345678901234567890123456789")
		})
		require.Equal(t, http.StatusUnauthorized, rec.Code)
		require.Empty(t, rec.Result().Cookies())
	})
}

// TestSignInCallsSkipCSRF covers signing in again in a browser that holds
// a live session: the sign-in calls run as anonymous without the CSRF
// header, and every other state-changing call still needs it.
func TestSignInCallsSkipCSRF(t *testing.T) {
	ctx := context.Background()
	s, _ := newSvc(t, dbtest.SQLite(t))
	alice := mustUser(t, s, "alice", false)
	_, secret, err := s.Sessions.Create(ctx, alice.ID, "", "")
	require.NoError(t, err)
	post := func(target string) (model.Principal, error) {
		r := httptest.NewRequest(http.MethodPost, "http://hangar.test"+target, nil)
		r.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: secret})
		return s.Authenticate(r)
	}
	for _, target := range []string{"/api/hangar/v1/auth/login", "/auth/register", "/auth/invite/abc", "/auth/reset/abc"} {
		p, err := post(target)
		require.NoError(t, err, target)
		require.True(t, p.IsAnonymous(), target)
	}
	for _, target := range []string{"/api/hangar/v1/auth/logout", "/api/hangar/v1/me/tokens", "/auth/oidc/sso/link", "/auth/reset/abc/x", "/auth/registers"} {
		_, err := post(target)
		require.ErrorIs(t, err, auth.ErrCSRF, target)
	}
	r := httptest.NewRequest(http.MethodPut, "http://hangar.test/api/hangar/v1/auth/login", nil)
	r.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: secret})
	_, err = s.Authenticate(r)
	require.ErrorIs(t, err, auth.ErrCSRF, "only POST starts a session")
}

// TestSignInAgainReplacesSession signs in through the real handler with a
// stale cookie and then with a live one: each answer sets one session
// cookie, the new one, and ends the session the old cookie named.
func TestSignInAgainReplacesSession(t *testing.T) {
	ctx := context.Background()
	s, _ := authtest.NewService(t, dbtest.SQLite(t), authtest.RealOptions{Config: func(c *config.Config) {
		c.Security.Registration = config.RegistrationOpen
	}})
	h := s.Middleware(s, auth.WriteAuthError("http://hangar.test"))(s.Handler())
	call := func(target, body, cookie string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "http://hangar.test"+target, strings.NewReader(body))
		if cookie != "" {
			req.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: cookie})
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req.WithContext(httpx.WithInfo(req.Context(), "req")))
		return rec
	}
	newSession := func(rec *httptest.ResponseRecorder) string {
		t.Helper()
		cs := cookiesNamed(rec, auth.SessionCookie)
		require.Len(t, cs, 1, "one session cookie per response")
		require.NotEmpty(t, cs[0].Value)
		require.Positive(t, cs[0].MaxAge)
		csrf := cookiesNamed(rec, auth.CSRFCookie)
		require.Len(t, csrf, 1, "one CSRF cookie per response")
		var me auth.MeView
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &me))
		require.Equal(t, me.CSRFToken, csrf[0].Value)
		return cs[0].Value
	}

	rec := call("/auth/register", `{"username":"alice","password":"`+goodPassword+`"}`, "bogus")
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	first := newSession(rec)

	u, err := s.Users.GetByUsername(ctx, "alice")
	require.NoError(t, err)
	link, err := s.CreateResetLink(ctx, u.ID, model.System())
	require.NoError(t, err)
	code := link.URL[strings.LastIndex(link.URL, "/")+1:]
	rec = call("/auth/reset/"+code, `{"password":"a brand new password"}`, first)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	second := newSession(rec)
	require.NotEqual(t, first, second)

	p, err := s.Authenticate(func() *http.Request {
		r := httptest.NewRequest(http.MethodGet, "http://hangar.test/", nil)
		r.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: second})
		return r
	}())
	require.NoError(t, err)
	require.Equal(t, u.ID, p.UserID)

	// Starting a session ends the one the request's cookie names.
	before, err := s.Sessions.List(ctx, u.ID)
	require.NoError(t, err)
	require.Len(t, before, 1)
	req := httptest.NewRequest(http.MethodPost, "http://hangar.test/api/hangar/v1/auth/login", nil)
	req.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: second})
	third, err := s.StartSession(ctx, httptest.NewRecorder(), req, u)
	require.NoError(t, err)
	_, err = s.Sessions.Get(ctx, before[0].ID)
	require.ErrorIs(t, err, auth.ErrNotFound, "the replaced session ends")
	after, err := s.Sessions.List(ctx, u.ID)
	require.NoError(t, err)
	require.Len(t, after, 1)
	require.Equal(t, third.ID, after[0].ID)
}
