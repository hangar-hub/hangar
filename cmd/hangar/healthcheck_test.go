package main

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// freeAddr returns a loopback address that nothing listens on.
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := l.Addr().String()
	require.NoError(t, l.Close())
	return addr
}

// TestHealthcheckRetryForWaitsForTheListener checks that hangar healthcheck
// --retry-for keeps trying while the connection is refused. The chart's helm
// test Pod calls /readyz through the Service, which refuses connections for
// a moment after the Pod is ready (until the Service proxy routes to it);
// with one try, ct install failed on GitHub's runners with "connect:
// connection refused" against a ready Pod.
func TestHealthcheckRetryForWaitsForTheListener(t *testing.T) {
	addr := freeAddr(t)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})}
	started := make(chan error, 1)
	go func() {
		time.Sleep(3 * healthcheckRetryInterval)
		l, err := net.Listen("tcp", addr)
		started <- err
		if err == nil {
			_ = srv.Serve(l)
		}
	}()
	t.Cleanup(func() { _ = srv.Close() })

	out, code := execute(t, "healthcheck", "--retry-for", "30s", "--listen", addr)
	require.NoError(t, <-started)
	require.Equal(t, 0, code, out)
}

// TestHealthcheckRetryForGivesUp checks that --retry-for stops once its time
// has passed and reports the connection error.
func TestHealthcheckRetryForGivesUp(t *testing.T) {
	addr := freeAddr(t)
	start := time.Now()
	out, code := execute(t, "healthcheck", "--retry-for", "2s", "--listen", addr)
	require.Equal(t, 1, code, out)
	require.Contains(t, out, "connection refused")
	require.GreaterOrEqual(t, time.Since(start), 2*time.Second-healthcheckRetryInterval, "it retried for about --retry-for")
	require.Less(t, time.Since(start), 20*time.Second, "it gave up")
}

// TestHealthcheckRetryForDoesNotRetryAnAnswer checks that a /readyz answer
// other than 200 fails at once, with or without --retry-for: the server was
// reached and said it is not ready, which the helm test must report rather
// than wait out.
func TestHealthcheckRetryForDoesNotRetryAnAnswer(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	addr := srv.Listener.Addr().String()

	out, code := execute(t, "healthcheck", "--retry-for", "30s", "--listen", addr)
	require.Equal(t, 1, code, out)
	require.Contains(t, out, "not ready: /readyz answered 503 Service Unavailable")
	require.Equal(t, int32(1), calls.Load(), "a 503 is not retried")

	err := checkReadyRetrying(t.Context(), addr, 30*time.Second)
	var answered *errNotReadyAnswer
	require.True(t, errors.As(err, &answered), "%v", err)
}
