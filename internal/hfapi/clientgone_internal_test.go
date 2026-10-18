package hfapi

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
	"github.com/hangar-hub/hangar/internal/telemetry"
)

// TestInternalErrorAnswersClientGoneWith499 checks that a Hub API request
// the client closed is answered 499 and logged at debug level, not
// recorded as a server error (bellatrix finding 9). viewerError and the
// other mappings fall through to internalError.
func TestInternalErrorAnswersClientGoneWith499(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	h := testHandler()
	h.log = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	err := fmt.Errorf("read the dataset: %w", context.Canceled)
	request := func(ctx context.Context) *http.Request {
		ctx = httpx.WithInfo(ctx, "req-1")
		httpx.SetProtocol(ctx, telemetry.ProtocolHub)
		return httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/datasets/a/b/parquet", nil)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for name, write := range map[string]func(w http.ResponseWriter, r *http.Request, err error){
		"internalError": h.internalError,
		"viewerError":   h.viewerError,
	} {
		logs.Reset()
		rec := httptest.NewRecorder()
		write(rec, request(ctx), err)
		require.Equal(t, httpx.StatusClientClosedRequest, rec.Code, name)
		require.Equal(t, httpx.ClientClosedMessage, rec.Header().Get(httpx.HeaderErrorMessage), name)
		require.NotContains(t, logs.String(), "level=ERROR", name)
		require.Contains(t, logs.String(), "level=DEBUG", name)
	}

	// While the client is still there, it is a server error.
	logs.Reset()
	rec := httptest.NewRecorder()
	h.internalError(rec, request(context.Background()), err)
	require.Equal(t, http.StatusInternalServerError, rec.Code)
	require.Contains(t, logs.String(), "level=ERROR")
}
