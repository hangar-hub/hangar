package api_test

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/api/gen"
	"github.com/hangar-hub/hangar/internal/audit"
	"github.com/hangar-hub/hangar/internal/auth"
	"github.com/hangar-hub/hangar/internal/db"
	"github.com/hangar-hub/hangar/internal/db/dbtest"
	"github.com/hangar-hub/hangar/internal/model"
)

const newPassword = "a brand new passphrase 77"

// sessionFor signs a User in without a password, as an OIDC sign-in does,
// and returns a browser client with the session cookie and CSRF token.
func (e *env) sessionFor(u *auth.User) *client {
	e.t.Helper()
	sess, secret, err := e.svc.Sessions.Create(context.Background(), u.ID, "test", "127.0.0.1")
	require.NoError(e.t, err)
	c := e.anon()
	base, err := url.Parse(e.srv.URL)
	require.NoError(e.t, err)
	c.http.Jar.SetCookies(base, []*http.Cookie{{Name: auth.SessionCookie, Value: secret, Path: "/"}})
	c.csrf = sess.CSRFToken
	return c
}

func (e *env) auditActions() map[audit.Action][]audit.Entry {
	e.t.Helper()
	list, _, err := audit.NewDB(e.d, nil).List(context.Background(), audit.Filter{Limit: 500})
	require.NoError(e.t, err)
	out := map[audit.Action][]audit.Entry{}
	for _, ev := range list {
		out[ev.Action] = append(out[ev.Action], ev)
	}
	return out
}

// TestUpdateMe covers PATCH /me (W8 8, decided by James on 2026-10-18):
// the user's own display name with the admin editor's checks, a new email
// that waits for the confirmation link an admin creates, the admin
// editor's error for an email another user has, CSRF for sessions and the
// write scope for Tokens.
func TestUpdateMe(t *testing.T) {
	t.Parallel()
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		ctx := context.Background()
		e := newEnv(t, d)
		alice := e.user("alice", false)
		e.user("bob", false)
		e.user("root", true)
		aliceClient, anon, rootClient := e.login("alice"), e.anon(), e.login("root")

		anon.do(http.MethodPatch, "/api/hangar/v1/me", map[string]any{"display_name": "Nobody"}, http.StatusUnauthorized, nil)

		var me gen.Me
		aliceClient.do(http.MethodPatch, "/api/hangar/v1/me", map[string]any{"display_name": "Alice Liddell", "email": "alice@example.com"}, http.StatusOK, &me)
		require.Equal(t, "Alice Liddell", me.DisplayName)
		require.Nil(t, me.Email, "a new email waits for confirmation")
		require.Equal(t, "alice@example.com", *me.PendingEmail)
		require.True(t, me.HasPassword)
		require.NotNil(t, me.CsrfToken, "a session caller keeps its CSRF token")

		// The admin sees the pending email and creates the link; the
		// person who opens it confirms it, once.
		var au gen.AdminUserList
		rootClient.do(http.MethodGet, "/api/hangar/v1/admin/users?q=alice", nil, http.StatusOK, &au)
		require.Equal(t, "alice@example.com", *au.Items[0].PendingEmail)
		var link gen.EmailLink
		rootClient.do(http.MethodPost, "/api/hangar/v1/admin/users/"+alice.ID.String()+"/email-link", map[string]any{"email": "alice@example.com"}, http.StatusCreated, &link)
		require.Equal(t, "alice@example.com", link.Email, "the response carries the bound address")
		require.Contains(t, link.Url, "http://hangar.test/auth/email/")
		path := strings.TrimPrefix(link.Url, "http://hangar.test")
		anon.do(http.MethodPost, path, nil, http.StatusNoContent, nil)
		anon.do(http.MethodPost, path, nil, http.StatusNotFound, nil)
		var confirmed gen.Me
		aliceClient.do(http.MethodGet, "/api/hangar/v1/me", nil, http.StatusOK, &confirmed)
		require.Equal(t, "alice@example.com", *confirmed.Email)
		require.Nil(t, confirmed.PendingEmail)
		bob, err := e.svc.Users.GetByUsername(ctx, "bob")
		require.NoError(t, err)
		var none gen.Problem
		rootClient.do(http.MethodPost, "/api/hangar/v1/admin/users/"+bob.ID.String()+"/email-link", map[string]any{"email": "bob@example.com"}, http.StatusUnprocessableEntity, &none)
		require.Contains(t, *none.Detail, "no email change waiting")
		// A change the user makes between the admin viewing the list and
		// approving gives a conflict, not a link for another address.
		bobClient0 := e.login("bob")
		bobClient0.do(http.MethodPatch, "/api/hangar/v1/me", map[string]any{"email": "bob@seen.example"}, http.StatusOK, nil)
		bobClient0.do(http.MethodPatch, "/api/hangar/v1/me", map[string]any{"email": "bob@other.example"}, http.StatusOK, nil)
		var stale gen.Problem
		rootClient.do(http.MethodPost, "/api/hangar/v1/admin/users/"+bob.ID.String()+"/email-link", map[string]any{"email": "bob@seen.example"}, http.StatusConflict, &stale)
		require.Contains(t, *stale.Detail, "pending email has changed")
		rootClient.do(http.MethodPost, "/api/hangar/v1/admin/users/"+bob.ID.String()+"/email-link", map[string]any{}, http.StatusConflict, nil)
		// The link is the credential, so it ignores a session cookie sent
		// without the CSRF header.
		e.user("lena", false)
		lena := e.login("lena")
		lena.do(http.MethodPatch, "/api/hangar/v1/me", map[string]any{"email": "lena@example.com"}, http.StatusOK, nil)
		lenaUser, err := e.svc.Users.GetByUsername(ctx, "lena")
		require.NoError(t, err)
		rootClient.do(http.MethodPost, "/api/hangar/v1/admin/users/"+lenaUser.ID.String()+"/email-link", map[string]any{"email": "lena@example.com"}, http.StatusCreated, &link)
		lenaCSRF := lena.csrf
		lena.csrf = ""
		lena.do(http.MethodPost, strings.TrimPrefix(link.Url, "http://hangar.test"), nil, http.StatusNoContent, nil)
		lena.csrf = lenaCSRF

		// A field left out is unchanged; an empty display name shows the
		// username.
		me = gen.Me{}
		aliceClient.do(http.MethodPatch, "/api/hangar/v1/me", map[string]any{"display_name": ""}, http.StatusOK, &me)
		require.Equal(t, "alice", me.DisplayName, "the username stands in for a cleared display name")
		require.Equal(t, "alice@example.com", *me.Email)

		// The admin editor's validation.
		aliceClient.do(http.MethodPatch, "/api/hangar/v1/me", map[string]any{"email": "not an email"}, http.StatusUnprocessableEntity, nil)
		aliceClient.do(http.MethodPatch, "/api/hangar/v1/me", map[string]any{"display_name": strings.Repeat("a", 101)}, http.StatusUnprocessableEntity, nil)

		// An email another user has fails with the admin path's error.
		bobClient := e.login("bob")
		var clash, adminClash gen.Problem
		bobClient.do(http.MethodPatch, "/api/hangar/v1/me", map[string]any{"email": "ALICE@example.com"}, http.StatusConflict, &clash)
		rootClient.do(http.MethodPatch, "/api/hangar/v1/admin/users/"+bob.ID.String(), map[string]any{"email": "alice@example.com"}, http.StatusConflict, &adminClash)
		require.Equal(t, *adminClash.Detail, *clash.Detail)

		// Squatting: a pending address blocks nobody. Bob asks for an
		// address, and the admin can still give it to someone else, who
		// then has it; Bob's link fails.
		var bm gen.Me
		bobClient.do(http.MethodPatch, "/api/hangar/v1/me", map[string]any{"email": "boss@example.com"}, http.StatusOK, &bm)
		require.Equal(t, "boss@example.com", *bm.PendingEmail)
		_, err = e.svc.Users.GetByEmail(ctx, "boss@example.com")
		require.ErrorIs(t, err, auth.ErrNotFound)
		rootClient.do(http.MethodPost, "/api/hangar/v1/admin/users/"+bob.ID.String()+"/email-link", map[string]any{"email": "boss@example.com"}, http.StatusCreated, &link)
		rootClient.do(http.MethodPost, "/api/hangar/v1/admin/users", map[string]any{"username": "boss", "email": "boss@example.com"}, http.StatusCreated, nil)
		anon.do(http.MethodPost, strings.TrimPrefix(link.Url, "http://hangar.test"), nil, http.StatusConflict, nil)

		// CSRF is needed with a session.
		csrf := aliceClient.csrf
		aliceClient.csrf = ""
		aliceClient.do(http.MethodPatch, "/api/hangar/v1/me", map[string]any{"display_name": "No CSRF"}, http.StatusForbidden, nil)
		aliceClient.csrf = csrf

		// Tokens: write may change the profile, read may not.
		var wt, rt gen.TokenCreated
		aliceClient.do(http.MethodPost, "/api/hangar/v1/me/tokens", map[string]any{"name": "w", "scope": "write"}, http.StatusCreated, &wt)
		aliceClient.do(http.MethodPost, "/api/hangar/v1/me/tokens", map[string]any{"name": "r", "scope": "read"}, http.StatusCreated, &rt)
		(&client{e: e, http: http.DefaultClient, bearer: rt.Secret}).do(http.MethodPatch, "/api/hangar/v1/me", map[string]any{"display_name": "Read"}, http.StatusForbidden, nil)
		var tokMe gen.Me
		(&client{e: e, http: http.DefaultClient, bearer: wt.Secret}).do(http.MethodPatch, "/api/hangar/v1/me", map[string]any{"display_name": "Written"}, http.StatusOK, &tokMe)
		require.Equal(t, "Written", tokMe.DisplayName)
		require.Nil(t, tokMe.CsrfToken, "token callers get no CSRF token")

		// Users who sign in with OIDC change both fields too.
		oidcUser, err := e.svc.Users.Create(ctx, auth.NewUser{Username: "olivia", Email: "olivia@corp.example", EmailVerified: true}, model.System())
		require.NoError(t, err)
		_, err = d.NewInsert().Model(&db.Identity{ID: db.NewID(), UserID: oidcUser.ID, Provider: "sso", Subject: "o", Claims: map[string]any{}}).Exec(ctx)
		require.NoError(t, err)
		var om gen.Me
		e.sessionFor(oidcUser).do(http.MethodPatch, "/api/hangar/v1/me", map[string]any{"display_name": "Olivia", "email": "olivia@home.example"}, http.StatusOK, &om)
		require.Equal(t, "olivia@corp.example", *om.Email)
		require.Equal(t, "olivia@home.example", *om.PendingEmail)
		require.False(t, om.HasPassword)

		got := e.auditActions()
		require.Len(t, got[audit.UserProfileChanged], 8, "every change is audited, and failed ones are not")
		require.NotEmpty(t, got[audit.UserEmailConfirmation])
	})
}

// TestChangeMyPassword covers POST /me/password (W8 8): the current
// password, the password policy, the other sessions ending, CSRF, the
// sign-in throttle, Tokens and OIDC users refused, and the audit events.
func TestChangeMyPassword(t *testing.T) {
	t.Parallel()
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		e := newEnv(t, d)
		e.user("alice", false)
		alice, laptop, anon := e.login("alice"), e.login("alice"), e.anon()
		var wt gen.TokenCreated
		alice.do(http.MethodPost, "/api/hangar/v1/me/tokens", map[string]any{"name": "w", "scope": "admin"}, http.StatusCreated, &wt)
		tok := &client{e: e, http: http.DefaultClient, bearer: wt.Secret}
		change := func(current, next string) map[string]string {
			return map[string]string{"current_password": current, "new_password": next}
		}

		anon.do(http.MethodPost, "/api/hangar/v1/me/password", change(password, newPassword), http.StatusUnauthorized, nil)

		// Tokens never change passwords, whatever their scope.
		var p gen.Problem
		tok.do(http.MethodPost, "/api/hangar/v1/me/password", change(password, newPassword), http.StatusForbidden, &p)
		require.Contains(t, *p.Detail, "web UI")

		// CSRF is needed.
		csrf := alice.csrf
		alice.csrf = ""
		alice.do(http.MethodPost, "/api/hangar/v1/me/password", change(password, newPassword), http.StatusForbidden, nil)
		alice.csrf = csrf

		// A wrong current password.
		alice.do(http.MethodPost, "/api/hangar/v1/me/password", change("not my password 1", newPassword), http.StatusForbidden, &p)
		require.Equal(t, "The current password is wrong.", *p.Detail)
		require.Equal(t, "/current_password", *(*p.Errors)[0].Pointer)
		alice.do(http.MethodPost, "/api/hangar/v1/me/password", change("", newPassword), http.StatusForbidden, nil)

		// A weak new password.
		alice.do(http.MethodPost, "/api/hangar/v1/me/password", change(password, "short"), http.StatusUnprocessableEntity, &p)
		require.Equal(t, "Use at least 10 characters.", *p.Detail)
		require.Equal(t, "/new_password", *(*p.Errors)[0].Pointer)
		alice.do(http.MethodPost, "/api/hangar/v1/me/password", change(password, "password123"), http.StatusUnprocessableEntity, &p)
		require.Contains(t, *p.Detail, "too common")
		alice.do(http.MethodPost, "/api/hangar/v1/me/password", change(password, "ALICE"), http.StatusUnprocessableEntity, nil)

		// Nothing changed so far.
		laptop.do(http.MethodGet, "/api/hangar/v1/me/sessions", nil, http.StatusOK, nil)

		// Success ends the other sessions and keeps this one and the Tokens.
		alice.do(http.MethodPost, "/api/hangar/v1/me/password", change(password, newPassword), http.StatusNoContent, nil)
		var sessions gen.SessionList
		alice.do(http.MethodGet, "/api/hangar/v1/me/sessions", nil, http.StatusOK, &sessions)
		require.Len(t, sessions.Items, 1)
		require.True(t, sessions.Items[0].Current)
		laptop.do(http.MethodGet, "/api/hangar/v1/me", nil, http.StatusNoContent, nil)
		laptop.do(http.MethodGet, "/api/hangar/v1/me/sessions", nil, http.StatusUnauthorized, nil)
		tok.do(http.MethodGet, "/api/hangar/v1/me", nil, http.StatusOK, nil)

		// Sign-in takes the new password only.
		anon.do(http.MethodPost, "/api/hangar/v1/auth/login", map[string]string{"username": "alice", "password": password}, http.StatusUnauthorized, nil)
		anon.do(http.MethodPost, "/api/hangar/v1/auth/login", map[string]string{"username": "alice", "password": newPassword}, http.StatusOK, nil)

		// OIDC users have no Hangar password to change.
		oidcUser, err := e.svc.Users.Create(context.Background(), auth.NewUser{Username: "olivia"}, model.System())
		require.NoError(t, err)
		_, err = d.NewInsert().Model(&db.Identity{ID: db.NewID(), UserID: oidcUser.ID, Provider: "sso", Subject: "o", Claims: map[string]any{}}).Exec(context.Background())
		require.NoError(t, err)
		e.sessionFor(oidcUser).do(http.MethodPost, "/api/hangar/v1/me/password", change("anything at all 1", newPassword), http.StatusForbidden, &p)
		require.Contains(t, *p.Detail, "sign-in provider")

		got := e.auditActions()
		require.Len(t, got[audit.UserPasswordChanged], 1)
		require.EqualValues(t, 1, got[audit.UserPasswordChanged][0].Data["sessions_ended"])
		reasons := map[string]int{}
		for _, ev := range got[audit.UserPasswordChangeFailed] {
			reasons[ev.Data["reason"].(string)]++
		}
		require.Equal(t, map[string]int{"wrong_password": 2, "weak_password": 3, "no_password": 1}, reasons)
	})
}

// TestChangeMyPasswordIsThrottled checks that wrong current passwords
// count towards the sign-in throttle: after five, the next attempt gets
// 429 with Retry-After even with the right password, and sign-in for the
// username is throttled too.
func TestChangeMyPasswordIsThrottled(t *testing.T) {
	t.Parallel()
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		e := newEnv(t, d)
		e.user("alice", false)
		alice := e.login("alice")
		for i := 0; i < 5; i++ {
			alice.do(http.MethodPost, "/api/hangar/v1/me/password", map[string]string{"current_password": "wrong password 12", "new_password": newPassword}, http.StatusForbidden, nil)
		}
		resp := alice.do(http.MethodPost, "/api/hangar/v1/me/password", map[string]string{"current_password": password, "new_password": newPassword}, http.StatusTooManyRequests, nil)
		require.NotEmpty(t, resp.Header.Get("Retry-After"))
		e.anon().do(http.MethodPost, "/api/hangar/v1/auth/login", map[string]string{"username": "alice", "password": password}, http.StatusTooManyRequests, nil)
	})
}
