package auth_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/auth"
	"github.com/hangar-hub/hangar/internal/auth/authtest"
	"github.com/hangar-hub/hangar/internal/config"
	"github.com/hangar-hub/hangar/internal/db"
	"github.com/hangar-hub/hangar/internal/db/dbtest"
	"github.com/hangar-hub/hangar/internal/httpx"
	"github.com/hangar-hub/hangar/internal/jobs"
	"github.com/hangar-hub/hangar/internal/jobs/jobstest"
	"github.com/hangar-hub/hangar/internal/model"
)

func TestHandler(t *testing.T) {
	ctx := context.Background()
	s, _ := authtest.NewService(t, dbtest.SQLite(t), authtest.RealOptions{Config: func(c *config.Config) {
		c.Security.Registration = config.RegistrationOpen
		c.OIDC.Providers = []config.OIDCProvider{{ID: "sso", Issuer: "http://127.0.0.1:1/unreachable", ClientID: "x"}}
	}})
	h := httpx.Authenticate(s, auth.WriteAuthError("http://hangar.test"))(s.Handler())
	call := func(method, target, body string, cookies []*http.Cookie, csrf string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "http://hangar.test"+target, strings.NewReader(body))
		for _, c := range cookies {
			req.AddCookie(c)
		}
		if csrf != "" {
			req.Header.Set(auth.CSRFHeader, csrf)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req.WithContext(httpx.WithInfo(req.Context(), "req")))
		return rec
	}

	rec := call(http.MethodPost, "/auth/register", `{"username":"alice","password":"`+goodPassword+`","email":"a@example.com"}`, nil, "")
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var me auth.MeView
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &me))
	require.Equal(t, "alice", me.Username)
	require.NotEmpty(t, me.CSRFToken)
	cookies := rec.Result().Cookies()
	require.NotEmpty(t, cookies)

	require.Equal(t, http.StatusUnprocessableEntity, call(http.MethodPost, "/auth/register", `{"username":"b","password":"`+goodPassword+`"}`, nil, "").Code)
	require.Equal(t, http.StatusConflict, call(http.MethodPost, "/auth/register", `{"username":"alice","password":"`+goodPassword+`"}`, nil, "").Code)
	require.Equal(t, http.StatusBadRequest, call(http.MethodPost, "/auth/register", `nope`, nil, "").Code)

	// Linking needs a session and its CSRF token; discovery fails here, so
	// the start answers 500 without leaking details.
	require.Equal(t, http.StatusForbidden, call(http.MethodPost, "/auth/oidc/sso/link", "", cookies, "").Code)
	rec = call(http.MethodPost, "/auth/oidc/sso/link", "", cookies, me.CSRFToken)
	require.Equal(t, http.StatusInternalServerError, rec.Code)
	require.Contains(t, rec.Body.String(), "request ID")
	require.Equal(t, http.StatusNotFound, call(http.MethodPost, "/auth/oidc/nope/link", "", cookies, me.CSRFToken).Code)
	require.Equal(t, http.StatusNotFound, call(http.MethodDelete, "/auth/oidc/sso/link", "", cookies, me.CSRFToken).Code)
	rec = call(http.MethodGet, "/auth/oidc/sso", "", nil, "")
	require.Equal(t, "/auth/login?error=oidc_failed", rec.Header().Get("Location"), "an unreachable provider")

	// Reset by link signs the user in; disabled users are not signed in.
	u, err := s.Users.GetByUsername(ctx, "alice")
	require.NoError(t, err)
	link, err := s.CreateResetLink(ctx, u.ID, model.System())
	require.NoError(t, err)
	code := link.URL[strings.LastIndex(link.URL, "/")+1:]
	require.Equal(t, http.StatusUnprocessableEntity, call(http.MethodPost, "/auth/reset/"+code, `{"password":"short"}`, nil, "").Code)
	rec = call(http.MethodPost, "/auth/reset/"+code, `{"password":"a brand new password"}`, nil, "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	link, err = s.CreateResetLink(ctx, u.ID, model.System())
	require.NoError(t, err)
	yes := true
	_, err = s.Users.Update(ctx, u.ID, auth.UserUpdate{Disabled: &yes}, model.System())
	require.NoError(t, err)
	rec = call(http.MethodPost, "/auth/reset/"+link.URL[strings.LastIndex(link.URL, "/")+1:], `{"password":"another new password"}`, nil, "")
	require.Equal(t, http.StatusNoContent, rec.Code)
	require.Empty(t, rec.Result().Cookies())
}

func TestServiceAccessors(t *testing.T) {
	ctx := context.Background()
	s, _ := authtest.NewService(t, dbtest.SQLite(t), authtest.RealOptions{})
	require.NotNil(t, s.Tokens())
	require.NotNil(t, s.Authz())
	require.NotNil(t, s.DeviceFlow())
	require.NotNil(t, s.OllamaAuth())
	u := mustUser(t, s, "alice", false)
	p := u.Principal(model.AuthSession)
	_, err := s.CurrentSession(ctx, p)
	require.ErrorIs(t, err, auth.ErrNotFound)
	sess, _, err := s.Sessions.Create(ctx, u.ID, "ua", "10.0.0.1")
	require.NoError(t, err)
	p.SessionID = sess.ID
	got, err := s.CurrentSession(ctx, p)
	require.NoError(t, err)
	require.Equal(t, sess.CSRFToken, got.CSRFToken)
	me, err := s.Me(ctx, p, got.CSRFToken)
	require.NoError(t, err)
	require.Equal(t, "alice", me.DisplayName)
	_, err = s.Me(ctx, model.Anonymous(), "")
	require.ErrorIs(t, err, auth.ErrInvalidCredentials)
	s.RecordLogout(ctx, p, "10.0.0.1", "ua")
	require.NoError(t, s.Sessions.RevokeAll(ctx, u.ID))
	_, err = s.Sessions.Get(ctx, sess.ID)
	require.ErrorIs(t, err, auth.ErrNotFound)

	for range 3 {
		_, _, err := s.TokenStore.Create(ctx, auth.NewToken{UserID: u.ID, Name: "t", Scope: model.ScopeRead})
		require.NoError(t, err)
	}
	page, next, err := s.TokenStore.ListPage(ctx, "", 2)
	require.NoError(t, err)
	require.Len(t, page, 2)
	page, next, err = s.TokenStore.ListPage(ctx, next, 2)
	require.NoError(t, err)
	require.Len(t, page, 1)
	require.Empty(t, next)
	_, _, err = s.TokenStore.ListPage(ctx, "!!", 2)
	require.ErrorIs(t, err, auth.ErrInvalid)
	all, err := s.TokenStore.List(ctx, [16]byte{})
	require.NoError(t, err)
	require.Len(t, all, 3, "uuid.Nil lists every Token")

	require.ErrorIs(t, s.OIDC.Unlink(ctx, u.ID, "kc", p), auth.ErrNotFound)
}

func TestUnlinkAndDeviceEdges(t *testing.T) {
	ctx := context.Background()
	d := dbtest.SQLite(t)
	s, _ := authtest.NewService(t, d, authtest.RealOptions{})
	oidcOnly := mustUser(t, s, "oidc-only", false)
	withPw, err := s.Users.Create(ctx, auth.NewUser{Username: "with-pw", Password: goodPassword}, model.System())
	require.NoError(t, err)
	for _, u := range []*auth.User{oidcOnly, withPw} {
		_, err := d.NewInsert().Model(&db.Identity{ID: db.NewID(), UserID: u.ID, Provider: "kc", Subject: u.Username, Claims: map[string]any{}}).Exec(ctx)
		require.NoError(t, err)
	}
	got, err := s.Users.Get(ctx, oidcOnly.ID)
	require.NoError(t, err)
	require.Equal(t, []string{"kc"}, got.OIDCProviders)
	require.ErrorIs(t, s.OIDC.Unlink(ctx, oidcOnly.ID, "kc", model.System()), auth.ErrInvalid, "the only way to sign in")
	require.NoError(t, s.OIDC.Unlink(ctx, withPw.ID, "kc", model.System()))
	got, err = s.Users.Get(ctx, withPw.ID)
	require.NoError(t, err)
	require.Empty(t, got.OIDCProviders)

	_, err = s.Device.Start(ctx, strings.Repeat("x", 201))
	require.ErrorIs(t, err, auth.ErrInvalid)
	require.Equal(t, "ABCD-EFGH", auth.NormaliseUserCode("abcd efgh"))
	require.Equal(t, "ABC", auth.NormaliseUserCode("abc"))
}

func TestSmallEdges(t *testing.T) {
	ctx := context.Background()
	s, _ := authtest.NewService(t, dbtest.SQLite(t), authtest.RealOptions{})
	_, _, err := auth.ParseOllamaPublicKey(strings.Repeat("a", 5000))
	require.ErrorIs(t, err, auth.ErrInvalid)
	_, _, err = auth.ParseOllamaAuthorization("AAAA:" + strings.Repeat("A", 8))
	require.Error(t, err)
	allow := []string{}
	st, err := s.Settings.Update(ctx, auth.SettingsPatch{PullThroughAllow: &allow}, model.System())
	require.NoError(t, err)
	require.NotNil(t, st.PullThroughAllow)
	require.Empty(t, st.PullThroughAllow)
	b, f := true, false
	st, err = s.Settings.Update(ctx, auth.SettingsPatch{LocalLogin: &f, UIExternalImages: &f, ScanBlockUnsafeDownloads: &b,
		WebhooksAllowPrivateTargets: &f, PullAllowOverrideWindows: &f, PullThroughEnabled: &b}, model.System())
	require.NoError(t, err)
	require.False(t, st.LocalLogin)
	require.True(t, st.ScanBlockUnsafeDownloads)
	require.True(t, st.PullThroughEnabled)
	require.False(t, st.PullAllowOverrideWindows)
	require.True(t, auth.LooksLikeToken("hgr_"+strings.Repeat("a", 40)))
	require.False(t, auth.LooksLikeToken("hgr_"+strings.Repeat("-", 40)))
	require.False(t, auth.LooksLikeToken("hgr_x"))
}

func TestCleanup(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		ctx := context.Background()
		clock := authtest.NewClock(start())
		s, _ := authtest.NewService(t, d, authtest.RealOptions{Clock: clock})
		u := mustUser(t, s, "alice", false)
		_, _, err := s.Sessions.Create(ctx, u.ID, "", "")
		require.NoError(t, err)
		_, err = s.Device.Start(ctx, "")
		require.NoError(t, err)
		_, err = s.Login.Password(ctx, auth.LoginAttempt{Username: "alice", Password: "wrong-password-1", IP: "10.0.0.1"})
		require.ErrorIs(t, err, auth.ErrLoginFailed)
		res, err := s.Cleanup(ctx)
		require.NoError(t, err)
		require.Equal(t, auth.CleanupResult{}, res, "nothing has expired yet")
		clock.Advance(s.Config().Security.SessionTTL + 48*time.Hour)
		res, err = s.Cleanup(ctx)
		require.NoError(t, err)
		require.Equal(t, auth.CleanupResult{Sessions: 1, DeviceCodes: 1, LoginAttempts: 2}, res)
		require.Equal(t, time.Hour, auth.CleanupInterval())
	})
}

func TestCleanupJob(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		ctx := context.Background()
		clock := authtest.NewClock(start())
		s, _ := authtest.NewService(t, d, authtest.RealOptions{Clock: clock})
		u := mustUser(t, s, "alice", false)
		_, _, err := s.Sessions.Create(ctx, u.ID, "", "")
		require.NoError(t, err)
		clock.Advance(s.Config().Security.SessionTTL + time.Hour)

		reg := jobs.NewRegistry()
		require.NoError(t, s.RegisterJobs(reg))
		require.Error(t, s.RegisterJobs(reg), "registered once")
		q := jobstest.NewFake(reg)
		sched := auth.CleanupSchedule()
		require.Equal(t, jobs.KindAuthCleanup, sched.Kind)
		require.True(t, sched.Enabled)
		id, err := q.Enqueue(ctx, jobs.NewJob{Kind: jobs.KindAuthCleanup})
		require.NoError(t, err)
		require.Equal(t, 1, q.RunPending(ctx))
		j, err := q.Get(ctx, id)
		require.NoError(t, err)
		require.Equal(t, jobs.StatusSucceeded, j.Status)
		var res auth.CleanupResult
		require.NoError(t, json.Unmarshal(j.Result, &res))
		require.Equal(t, int64(1), res.Sessions)
	})
}
