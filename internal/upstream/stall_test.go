package upstream_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/model"
	"github.com/hangar-hub/hangar/internal/upstream"
)

// stallServer serves a download through handle, and releases handlers
// that are still waiting when the test ends.
func stallServer(t *testing.T, handle func(w http.ResponseWriter, r *http.Request, release <-chan struct{})) upstream.Upstream {
	t.Helper()
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handle(w, r, release) }))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })
	return upstream.Upstream{Name: "x", BaseURL: srv.URL}
}

func stallClient(timeout time.Duration) *upstream.HTTPClient {
	return upstream.NewHTTPClient(upstream.HTTPOptions{Version: "test", Sleep: noSleep, BaseBackoff: time.Millisecond, StallTimeout: timeout})
}

func download(ctx context.Context, c *upstream.HTTPClient, u upstream.Upstream) (io.ReadCloser, error) {
	rc, _, err := c.Download(ctx, u, upstream.Credentials{}, tiny, model.CommitSHA(strings.Repeat("a", 40)), "f.bin", nil)
	return rc, err
}

func TestDownloadBodyStallIsCut(t *testing.T) {
	u := stallServer(t, func(w http.ResponseWriter, r *http.Request, release <-chan struct{}) {
		w.Header().Set("Content-Length", "1000")
		_, _ = w.Write([]byte("0123456789"))
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	rc, err := download(context.Background(), stallClient(200*time.Millisecond), u)
	require.NoError(t, err)
	defer rc.Close()
	start := time.Now()
	b, err := io.ReadAll(rc)
	require.Equal(t, "0123456789", string(b))
	require.ErrorIs(t, err, upstream.ErrUnavailable)
	require.ErrorContains(t, err, "sent no data for 200ms")
	require.NotErrorIs(t, err, context.Canceled)
	require.Less(t, time.Since(start), 10*time.Second)
}

func TestDownloadHeaderStallIsCut(t *testing.T) {
	u := stallServer(t, func(_ http.ResponseWriter, r *http.Request, release <-chan struct{}) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	start := time.Now()
	_, err := download(upstream.WithoutRetries(context.Background()), stallClient(200*time.Millisecond), u)
	require.ErrorIs(t, err, upstream.ErrUnavailable)
	require.ErrorContains(t, err, "timeout awaiting response headers")
	require.Less(t, time.Since(start), 10*time.Second)
}

func TestDownloadSlowButSteadyIsKept(t *testing.T) {
	// The upstream sends a byte every 50ms, within the 300ms stall
	// timeout, for longer than the timeout in all.
	u := stallServer(t, func(w http.ResponseWriter, _ *http.Request, _ <-chan struct{}) {
		w.Header().Set("Content-Length", "12")
		for i := range 12 {
			_, _ = w.Write([]byte{byte('a' + i)})
			w.(http.Flusher).Flush()
			time.Sleep(50 * time.Millisecond)
		}
	})
	rc, err := download(context.Background(), stallClient(300*time.Millisecond), u)
	require.NoError(t, err)
	b, err := io.ReadAll(rc)
	require.NoError(t, err)
	require.Equal(t, "abcdefghijkl", string(b))
	require.NoError(t, rc.Close())

	// A reader that pauses for longer than the timeout between reads is
	// not taken for a stalled upstream.
	u = stallServer(t, func(w http.ResponseWriter, _ *http.Request, _ <-chan struct{}) {
		_, _ = w.Write([]byte("hello world"))
	})
	rc, err = download(context.Background(), stallClient(100*time.Millisecond), u)
	require.NoError(t, err)
	defer rc.Close()
	one := make([]byte, 1)
	_, err = io.ReadFull(rc, one)
	require.NoError(t, err)
	time.Sleep(300 * time.Millisecond)
	rest, err := io.ReadAll(rc)
	require.NoError(t, err)
	require.Equal(t, "hello world", string(one)+string(rest))
}
