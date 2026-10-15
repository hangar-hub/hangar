package api_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/db/dbtest"
)

// TestLoginThrottleMessage checks that the sign-in throttle answers 429
// with Retry-After and a sentence the UI can show as it is.
func TestLoginThrottleMessage(t *testing.T) {
	e := newEnv(t, dbtest.SQLite(t))
	e.user("alice", false)
	var last *http.Response
	var body []byte
	for range 7 {
		b, _ := json.Marshal(map[string]string{"username": "alice", "password": "wrong-password-1"})
		resp, err := http.Post(e.srv.URL+"/api/hangar/v1/auth/login", "application/json", bytes.NewReader(b))
		require.NoError(t, err)
		body, _ = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		last = resp
		if resp.StatusCode == http.StatusTooManyRequests {
			break
		}
		require.Equal(t, http.StatusUnauthorized, resp.StatusCode, string(body))
	}
	require.Equal(t, http.StatusTooManyRequests, last.StatusCode, string(body))
	require.NotEmpty(t, last.Header.Get("Retry-After"))
	var p struct {
		Detail string `json:"detail"`
	}
	require.NoError(t, json.Unmarshal(body, &p))
	require.True(t, strings.HasPrefix(p.Detail, "Too many failed sign-in attempts. Try again in "), p.Detail)
	require.True(t, strings.HasSuffix(p.Detail, "."), p.Detail)
}
