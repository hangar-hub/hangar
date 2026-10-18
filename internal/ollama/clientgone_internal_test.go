package ollama

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/httpx"
)

// TestInternalAnswersClientGoneWith499 checks that a registry request the
// client closed, such as `ollama pull` stopped with Ctrl-C, is answered
// 499 and logged at debug level, not recorded as a server error
// (bellatrix finding 9).
func TestInternalAnswersClientGoneWith499(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	h := &Handler{o: Options{Log: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))}}
	err := fmt.Errorf("look up repo: %w", context.Canceled)
	request := func(ctx context.Context) *http.Request {
		return httptest.NewRequestWithContext(ctx, http.MethodGet, "/v2/acme/m/manifests/latest", nil)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rec := httptest.NewRecorder()
	h.internal(rec, request(ctx), err)
	require.Equal(t, httpx.StatusClientClosedRequest, rec.Code)
	require.Contains(t, rec.Body.String(), httpx.ClientClosedMessage)
	require.NotContains(t, logs.String(), "level=ERROR")
	require.Contains(t, logs.String(), "level=DEBUG")

	// While the client is still there, it is a server error.
	logs.Reset()
	rec = httptest.NewRecorder()
	h.internal(rec, request(context.Background()), err)
	require.Equal(t, http.StatusInternalServerError, rec.Code)
	require.Contains(t, logs.String(), "level=ERROR")
}
