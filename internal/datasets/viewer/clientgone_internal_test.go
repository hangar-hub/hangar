package viewer

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

// TestInternalErrorAnswersClientGoneWith499 checks that a Viewer request
// the client closed, such as a tab shut mid-query, is answered 499 and
// logged at debug level, not recorded as a server error (bellatrix
// finding 9). writeErr and the asset route fall through to internalError.
func TestInternalErrorAnswersClientGoneWith499(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	s := &Service{d: Deps{Log: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))}}
	err := fmt.Errorf("query rows: %w", context.Canceled)
	request := func(ctx context.Context) *http.Request {
		return httptest.NewRequestWithContext(ctx, http.MethodGet, "/rows?dataset=a/b", nil)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for name, write := range map[string]func(w http.ResponseWriter, r *http.Request, err error){
		"writeErr": func(w http.ResponseWriter, r *http.Request, err error) { s.writeErr(w, r, nil, err) },
		"internalError": func(w http.ResponseWriter, r *http.Request, err error) {
			s.internalError(w, r, "dataset asset failed", err)
		},
	} {
		logs.Reset()
		rec := httptest.NewRecorder()
		write(rec, request(ctx), err)
		require.Equal(t, httpx.StatusClientClosedRequest, rec.Code, name)
		require.Equal(t, "ClientClosedRequest", rec.Header().Get("X-Error-Code"), name)
		require.NotContains(t, logs.String(), "level=ERROR", name)
		require.Contains(t, logs.String(), "level=DEBUG", name)
	}

	// While the client is still there, it is a server error.
	logs.Reset()
	rec := httptest.NewRecorder()
	s.writeErr(rec, request(context.Background()), nil, err)
	require.Equal(t, http.StatusInternalServerError, rec.Code)
	require.Contains(t, logs.String(), "level=ERROR")
}
