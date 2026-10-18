package api_test

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/api"
	"github.com/hangar-hub/hangar/internal/audit"
	"github.com/hangar-hub/hangar/internal/auth"
	"github.com/hangar-hub/hangar/internal/auth/authtest"
	"github.com/hangar-hub/hangar/internal/db"
	"github.com/hangar-hub/hangar/internal/db/dbtest"
	"github.com/hangar-hub/hangar/internal/httpx"
	"github.com/hangar-hub/hangar/internal/model"
)

// TestCancelledLoginIsNotAServerError is bellatrix finding 9: a POST
// /auth/login the browser cancelled was answered 500 and logged at ERROR
// as "native API request failed ... context canceled". It is the client's
// doing, so it is answered 499 and logged at debug level.
func TestCancelledLoginIsNotAServerError(t *testing.T) {
	t.Parallel()
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		var logs bytes.Buffer
		svc, orgs := authtest.NewService(t, d, authtest.RealOptions{Audit: audit.NewDB(d, nil)})
		id := api.NewIdentity(api.IdentityOptions{Auth: svc, Orgs: orgs, Repos: dbRepos{d: d, orgs: orgs},
			Log: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))})
		_, err := svc.Users.Create(context.Background(), auth.NewUser{Username: "alice", Password: password}, model.System())
		require.NoError(t, err)

		login := func(ctx context.Context) *httptest.ResponseRecorder {
			body := strings.NewReader(`{"username":"alice","password":"` + password + `"}`)
			req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/hangar/v1/auth/login", body)
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			id.Login(rec, req)
			return rec
		}

		ctx, cancel := context.WithCancel(httpx.WithInfo(context.Background(), "req-1"))
		cancel()
		rec := login(ctx)
		require.Equal(t, httpx.StatusClientClosedRequest, rec.Code, rec.Body.String())
		require.Contains(t, rec.Body.String(), "Client Closed Request")
		require.NotContains(t, logs.String(), "level=ERROR")
		require.Contains(t, logs.String(), "client closed the request")

		// The same login with the client still there signs in.
		rec = login(httpx.WithInfo(context.Background(), "req-2"))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	})
}
