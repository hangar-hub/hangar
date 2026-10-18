package auth_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/auth"
	"github.com/hangar-hub/hangar/internal/auth/authtest"
	"github.com/hangar-hub/hangar/internal/db/dbtest"
	"github.com/hangar-hub/hangar/internal/httpx"
)

// syncBuffer is a bytes.Buffer safe for a slog handler and a test to share.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// logLevels returns the level of every JSON log line in out.
func logLevels(t *testing.T, out string) []string {
	t.Helper()
	var levels []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &rec), line)
		levels = append(levels, rec["level"].(string))
	}
	return levels
}

// TestAuthenticateClientClosed covers a browser that closes the page while
// a request carrying its session cookie is in flight: the session lookup
// fails because the request context ended. That is the client going away,
// so the answer is 499, not "Authentication is unavailable" with a 500, and
// nothing is logged at ERROR.
func TestAuthenticateClientClosed(t *testing.T) {
	logs := &syncBuffer{}
	d := dbtest.SQLite(t)
	s, _ := authtest.NewService(t, d, authtest.RealOptions{Log: slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))})
	alice := mustUser(t, s, "alice", false)
	_, secret, err := s.Sessions.Create(context.Background(), alice.ID, "", "")
	require.NoError(t, err)

	reached := false
	h := s.Middleware(s, auth.WriteAuthError("http://hangar.test"))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
	}))
	for _, target := range []string{"/ada/edit-x/edit/main/README.md", "/api/hangar/v1/me", "/api/models/ada/edit-x"} {
		ctx, cancel := context.WithCancel(httpx.WithInfo(context.Background(), "req-closed"))
		cancel()
		r := httptest.NewRequest(http.MethodGet, "http://hangar.test"+target, nil).WithContext(ctx)
		r.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: secret})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		require.Equal(t, 499, rec.Code, target)
		require.False(t, reached, target)
		require.NotContains(t, rec.Body.String(), "Authentication is unavailable", target)
	}
	require.NotContains(t, logLevels(t, logs.String()), "ERROR")
	require.NotContains(t, logs.String(), secret, "the session secret is never logged")
}

// TestAuthenticateStoreError covers a real failure of the authentication
// store while the client is still waiting: the answer stays a 500 with the
// request ID, and the error behind it is logged at ERROR with that ID, so
// the 500 in the access log can be traced to its cause.
func TestAuthenticateStoreError(t *testing.T) {
	logs := &syncBuffer{}
	d := dbtest.SQLite(t)
	s, _ := authtest.NewService(t, d, authtest.RealOptions{Log: slog.New(slog.NewJSONHandler(logs, nil))})
	alice := mustUser(t, s, "alice", false)
	_, secret, err := s.Sessions.Create(context.Background(), alice.ID, "", "")
	require.NoError(t, err)
	require.NoError(t, d.Close())

	h := s.Middleware(s, auth.WriteAuthError("http://hangar.test"))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("the handler must not run")
	}))
	r := httptest.NewRequest(http.MethodGet, "http://hangar.test/api/hangar/v1/me", nil)
	r = r.WithContext(httpx.WithInfo(r.Context(), "req-store"))
	r.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: secret})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	require.Equal(t, http.StatusInternalServerError, rec.Code)
	require.Contains(t, rec.Body.String(), "Authentication is unavailable")
	require.Contains(t, rec.Body.String(), "req-store")

	out := logs.String()
	require.Contains(t, logLevels(t, out), "ERROR")
	require.Contains(t, out, "database is closed")
	require.Contains(t, out, "req-store")
	require.NotContains(t, out, secret, "the session secret is never logged")
}
