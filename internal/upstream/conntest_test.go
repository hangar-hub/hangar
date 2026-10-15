package upstream_test

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/upstream"
)

// failOnSleep is a Sleep that fails the test: a connection test must never
// back off.
func failOnSleep(t *testing.T) func(context.Context, time.Duration) error {
	return func(context.Context, time.Duration) error {
		t.Errorf("the connection test backed off and retried")
		return nil
	}
}

func TestConnectionTestMakesOneAttempt(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	c := upstream.NewHTTPClient(upstream.HTTPOptions{Version: "test", Sleep: failOnSleep(t)})
	_, err := upstream.Test(context.Background(), c, upstream.Upstream{Name: "busy", BaseURL: srv.URL}, upstream.Credentials{})
	require.ErrorIs(t, err, upstream.ErrUnavailable)
	require.EqualValues(t, 1, hits.Load(), "a connection test makes one request")

	// The same client still retries outside a connection test.
	hits.Store(0)
	retrying := upstream.NewHTTPClient(upstream.HTTPOptions{Version: "test", Sleep: noSleep})
	_, err = retrying.Search(context.Background(), upstream.Upstream{Name: "busy", BaseURL: srv.URL}, upstream.Credentials{}, upstream.SearchQuery{Limit: 1})
	require.ErrorIs(t, err, upstream.ErrUnavailable)
	require.EqualValues(t, 4, hits.Load(), "other requests keep their retries")
}

func TestConnectionTestAnswersAtOnceWhenRefused(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())
	// The real backoff: before the fix this waited up to 7 seconds.
	c := upstream.NewHTTPClient(upstream.HTTPOptions{Version: "test"})
	start := time.Now()
	_, err = upstream.Test(context.Background(), c, upstream.Upstream{Name: "down", BaseURL: "http://" + addr}, upstream.Credentials{})
	require.ErrorIs(t, err, upstream.ErrUnavailable)
	require.ErrorContains(t, err, "connection refused")
	require.Less(t, time.Since(start), time.Second, "a refused connection is reported without backoff")
}

func TestConnectionTestIsBounded(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { close(release); srv.Close() })
	upstream.SetTestTimeout(t, 200*time.Millisecond)
	c := upstream.NewHTTPClient(upstream.HTTPOptions{Version: "test", Sleep: failOnSleep(t)})
	start := time.Now()
	_, err := upstream.Test(context.Background(), c, upstream.Upstream{Name: "slow", BaseURL: srv.URL}, upstream.Credentials{})
	require.ErrorIs(t, err, upstream.ErrUnavailable)
	require.ErrorContains(t, err, "no answer within 200ms")
	require.Less(t, time.Since(start), 3*time.Second)
}
