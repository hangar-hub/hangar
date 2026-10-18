package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/httpx"
)

// clientGoneCase runs write once with a request whose client has gone and
// once with a live request, for the same context.Canceled error, and
// checks that only the live one is a logged server error. Bellatrix
// finding 9: a cancelled login was answered 500 and logged at ERROR.
func clientGoneCase(t *testing.T, logs *bytes.Buffer, write func(w http.ResponseWriter, r *http.Request, err error)) {
	t.Helper()
	err := fmt.Errorf("read the user: %w", context.Canceled)

	ctx, cancel := context.WithCancel(httpx.WithInfo(context.Background(), "req-1"))
	cancel()
	rec := httptest.NewRecorder()
	write(rec, httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/hangar/v1/auth/login", nil), err)
	require.Equal(t, httpx.StatusClientClosedRequest, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "Client Closed Request")
	require.Contains(t, rec.Body.String(), httpx.ClientClosedMessage)
	require.NotContains(t, logs.String(), "level=ERROR")
	require.Contains(t, logs.String(), "level=DEBUG")

	// The same error while the client is still there is a server error:
	// the context.Canceled came from somewhere else.
	logs.Reset()
	rec = httptest.NewRecorder()
	write(rec, httptest.NewRequestWithContext(httpx.WithInfo(context.Background(), "req-2"), http.MethodPost, "/", nil), err)
	require.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
	require.Contains(t, logs.String(), "level=ERROR")

	// Other errors are unchanged when the client has gone.
	logs.Reset()
	rec = httptest.NewRecorder()
	write(rec, httptest.NewRequestWithContext(ctx, http.MethodPost, "/", nil), errors.New("boom"))
	require.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
}

func debugLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func TestIdentityFailAnswersClientGoneWith499(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	h := &Identity{log: debugLogger(&logs)}
	clientGoneCase(t, &logs, h.fail)
}

func TestDatasetsProblemAnswersClientGoneWith499(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	a := &DatasetsAPI{d: DatasetsDeps{Log: debugLogger(&logs)}}
	clientGoneCase(t, &logs, a.problem)
}

// TestPlatformFailAnswersClientGoneWith499 swaps the default logger, which
// platformFail writes to, so it does not run in parallel.
func TestPlatformFailAnswersClientGoneWith499(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(debugLogger(&logs))
	t.Cleanup(func() { slog.SetDefault(prev) })
	clientGoneCase(t, &logs, func(w http.ResponseWriter, r *http.Request, err error) {
		platformFail(w, r, "Could not list Jobs.", err)
	})
}
