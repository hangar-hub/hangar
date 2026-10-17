package auth

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/httpx"
)

// TestWriteErrorAnswersClientGoneWith499 checks that a sign-in route
// request the client closed (register, invite, reset or OIDC link) is
// answered 499 and logged at debug level, not recorded as a server error
// (bellatrix finding 9).
func TestWriteErrorAnswersClientGoneWith499(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	s := &Service{log: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))}
	err := fmt.Errorf("create the user: %w", context.Canceled)
	request := func(ctx context.Context) *http.Request {
		return httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/auth/register", nil)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rec := httptest.NewRecorder()
	s.writeError(rec, request(ctx), err)
	require.Equal(t, httpx.StatusClientClosedRequest, rec.Code)
	require.Contains(t, rec.Body.String(), httpx.ClientClosedMessage)
	require.NotContains(t, logs.String(), "level=ERROR")
	require.Contains(t, logs.String(), "level=DEBUG")

	// While the client is still there, it is a server error.
	logs.Reset()
	rec = httptest.NewRecorder()
	s.writeError(rec, request(context.Background()), err)
	require.Equal(t, http.StatusInternalServerError, rec.Code)
	require.Contains(t, logs.String(), "level=ERROR")
}
