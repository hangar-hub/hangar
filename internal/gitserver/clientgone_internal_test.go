package gitserver

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

// TestFailuresAnswerClientGoneWith499 checks that a git or git-lfs request
// the client closed is answered 499 and logged at debug level, not
// recorded as a server error (bellatrix finding 9).
func TestFailuresAnswerClientGoneWith499(t *testing.T) {
	t.Parallel()
	for name, fail := range map[string]func(s *Server, w http.ResponseWriter, r *http.Request, err error){
		"git":     (*Server).gitFailure,
		"git-lfs": (*Server).lfsFailure,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var logs bytes.Buffer
			s := &Server{log: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))}
			err := fmt.Errorf("read the pack: %w", context.Canceled)

			ctx, cancel := context.WithCancel(httpx.WithInfo(context.Background(), "req-1"))
			cancel()
			rec := httptest.NewRecorder()
			fail(s, rec, httptest.NewRequestWithContext(ctx, http.MethodPost, "/owner/tiny.git/git-upload-pack", nil), err)
			require.Equal(t, httpx.StatusClientClosedRequest, rec.Code, rec.Body.String())
			require.Contains(t, rec.Body.String(), httpx.ClientClosedMessage)
			require.NotContains(t, logs.String(), "level=ERROR")
			require.Contains(t, logs.String(), "level=DEBUG")

			// While the client is still there, it is a server error.
			logs.Reset()
			rec = httptest.NewRecorder()
			fail(s, rec, httptest.NewRequestWithContext(httpx.WithInfo(context.Background(), "req-2"), http.MethodPost, "/", nil), err)
			require.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
			require.Contains(t, logs.String(), "level=ERROR")
		})
	}
}
