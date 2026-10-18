package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/auth"
	"github.com/jamesagarside/hangar/internal/db"
	"github.com/jamesagarside/hangar/internal/model"
)

// TestSelfServiceAccountOnTheFullServer checks PATCH /me and POST
// /me/password (W8 8, decided by James on 2026-10-18) through the full
// server's router and middleware: CSRF and the cross-origin check for
// sessions, Tokens refused for passwords, the admin editor's email clash
// error, OIDC users refused for passwords, and a password change ending the
// user's other sessions while the current one and the Tokens keep working.
func TestSelfServiceAccountOnTheFullServer(t *testing.T) {
	in := newTestInstance(t, nil)
	ctx := context.Background()
	h, err := newHandler(ctx, in, newHealth(in.db, in.blobs), false)
	require.NoError(t, err)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	base, err := url.Parse(srv.URL)
	require.NoError(t, err)

	const oldPassword, newPassword = "a long enough password 1", "an even longer password 2"
	alice := testUser(t, in, "alice")
	testUser(t, in, "bob")
	_, err = in.auth.Users.Update(ctx, alice.UserID, auth.UserUpdate{Email: ptr("alice@example.com")}, model.System())
	require.NoError(t, err)
	_, aliceToken, err := in.auth.TokenStore.Create(ctx, auth.NewToken{UserID: alice.UserID, Name: "ci", Scope: model.ScopeAdmin})
	require.NoError(t, err)

	type browser struct {
		http *http.Client
		csrf string
	}
	signIn := func(user, pw string) *browser {
		t.Helper()
		jar, err := cookiejar.New(nil)
		require.NoError(t, err)
		b := &browser{http: &http.Client{Jar: jar}}
		resp, err := b.http.Post(srv.URL+"/api/hangar/v1/auth/login", "application/json",
			strings.NewReader(`{"username":"`+user+`","password":"`+pw+`"}`))
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)
		var me struct {
			CSRF string `json:"csrf_token"`
		}
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&me))
		b.csrf = me.CSRF
		return b
	}
	type answer struct {
		status int
		body   string
	}
	send := func(c *http.Client, method, path, body string, header map[string]string) answer {
		t.Helper()
		req, err := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		for k, v := range header {
			req.Header.Set(k, v)
		}
		resp, err := c.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		raw, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		return answer{resp.StatusCode, string(raw)}
	}
	do := func(b *browser, method, path, body string) answer {
		return send(b.http, method, path, body, map[string]string{auth.CSRFHeader: b.csrf})
	}
	withToken := func(method, path, body string) answer {
		return send(http.DefaultClient, method, path, body, map[string]string{"Authorization": "Bearer " + aliceToken})
	}
	pwBody := func(cur, next string) string {
		return `{"current_password":"` + cur + `","new_password":"` + next + `"}`
	}

	browserA, browserB := signIn("alice", oldPassword), signIn("alice", oldPassword)

	// Profile.
	r := do(browserA, http.MethodPatch, "/api/hangar/v1/me", `{"display_name":"Alice L"}`)
	require.Equal(t, http.StatusOK, r.status, r.body)
	require.Contains(t, r.body, `"display_name":"Alice L"`)
	require.Contains(t, r.body, `"has_password":true`)
	r = do(signIn("bob", oldPassword), http.MethodPatch, "/api/hangar/v1/me", `{"email":"alice@example.com"}`)
	require.Equal(t, http.StatusConflict, r.status, r.body)
	require.Contains(t, r.body, "the email address is already used")
	r = withToken(http.MethodPatch, "/api/hangar/v1/me", `{"display_name":"From CI"}`)
	require.Equal(t, http.StatusOK, r.status, r.body)

	// Unsafe calls from a session need the CSRF header and a same-origin
	// request.
	r = send(browserA.http, http.MethodPost, "/api/hangar/v1/me/password", pwBody(oldPassword, newPassword), nil)
	require.Equal(t, http.StatusForbidden, r.status, r.body)
	r = send(browserA.http, http.MethodPatch, "/api/hangar/v1/me", `{"display_name":"x"}`, nil)
	require.Equal(t, http.StatusForbidden, r.status, r.body)
	r = send(browserA.http, http.MethodPost, "/api/hangar/v1/me/password", pwBody(oldPassword, newPassword),
		map[string]string{auth.CSRFHeader: browserA.csrf, "Sec-Fetch-Site": "cross-site"})
	require.Equal(t, http.StatusForbidden, r.status, r.body)

	// Tokens never change passwords.
	r = withToken(http.MethodPost, "/api/hangar/v1/me/password", pwBody(oldPassword, newPassword))
	require.Equal(t, http.StatusForbidden, r.status, r.body)

	// Wrong current password, weak new password.
	r = do(browserA, http.MethodPost, "/api/hangar/v1/me/password", pwBody("not the password 1", newPassword))
	require.Equal(t, http.StatusForbidden, r.status, r.body)
	require.Contains(t, r.body, "/current_password")
	r = do(browserA, http.MethodPost, "/api/hangar/v1/me/password", pwBody(oldPassword, "password1"))
	require.Equal(t, http.StatusUnprocessableEntity, r.status, r.body)
	require.Contains(t, r.body, "/new_password")

	// Success: browser B is signed out, browser A and the Token still work.
	r = do(browserA, http.MethodPost, "/api/hangar/v1/me/password", pwBody(oldPassword, newPassword))
	require.Equal(t, http.StatusNoContent, r.status, r.body)
	require.Equal(t, http.StatusOK, do(browserA, http.MethodGet, "/api/hangar/v1/me/sessions", "").status)
	require.Equal(t, http.StatusUnauthorized, do(browserB, http.MethodGet, "/api/hangar/v1/me/sessions", "").status)
	require.Equal(t, http.StatusOK, withToken(http.MethodGet, "/api/hangar/v1/me", "").status)
	signIn("alice", newPassword)

	// A user who signs in only with OIDC has no Hangar password to change.
	olivia, err := in.auth.Users.Create(ctx, auth.NewUser{Username: "olivia"}, model.System())
	require.NoError(t, err)
	_, err = in.db.NewInsert().Model(&db.Identity{ID: db.NewID(), UserID: olivia.ID, Provider: "sso", Subject: "o", Claims: map[string]any{}}).Exec(ctx)
	require.NoError(t, err)
	sess, secret, err := in.auth.Sessions.Create(ctx, olivia.ID, "test", "127.0.0.1")
	require.NoError(t, err)
	jar, err := cookiejar.New(nil)
	require.NoError(t, err)
	jar.SetCookies(base, []*http.Cookie{{Name: auth.SessionCookie, Value: secret, Path: "/"}})
	ob := &browser{http: &http.Client{Jar: jar}, csrf: sess.CSRFToken}
	r = do(ob, http.MethodPost, "/api/hangar/v1/me/password", pwBody("", newPassword))
	require.Equal(t, http.StatusForbidden, r.status, r.body)
	require.Contains(t, r.body, "sign-in provider")
	r = do(ob, http.MethodPatch, "/api/hangar/v1/me", `{"display_name":"Olivia","email":"olivia@example.com"}`)
	require.Equal(t, http.StatusOK, r.status, r.body)
	require.Contains(t, r.body, `"has_password":false`)
}

func ptr[T any](v T) *T { return &v }
