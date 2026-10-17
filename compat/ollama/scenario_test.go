//go:build compat

package ollama_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/httpx"
)

// TestScenarioDeadlinesAreSeparate checks that a scenario that runs out
// its time leaves the next scenario its whole timeout. With one context
// for every scenario, a pull that stalled for four minutes on a loaded
// host left the scenarios after it seconds, and they failed as well.
func TestScenarioDeadlinesAreSeparate(t *testing.T) {
	const timeout = 200 * time.Millisecond
	scenario(t, "uses all its time", timeout, func(t *testing.T, ctx context.Context) {
		<-ctx.Done()
	})
	ok := scenario(t, "starts with its whole timeout", timeout, func(t *testing.T, ctx context.Context) {
		require.NoError(t, ctx.Err())
		deadline, set := ctx.Deadline()
		require.True(t, set)
		require.Greater(t, time.Until(deadline), timeout/2)
	})
	require.True(t, ok)
}

// TestRecorderTimeline checks the timeline a failed scenario logs: every
// request in arrival order with when it arrived and how long it took, a
// request still in progress, and no query string, which carries
// signatures.
func TestRecorderTimeline(t *testing.T) {
	rec := &recorder{}
	release := make(chan struct{})
	h := rec.wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/stall" {
			<-release
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	srv := httptest.NewServer(h)
	defer srv.Close()
	defer close(release)

	resp, err := http.Get(srv.URL + "/v2/first?" + httpx.SigParam + "=secret")
	require.NoError(t, err)
	_ = resp.Body.Close()
	go func() {
		if resp, err := http.Get(srv.URL + "/v2/stall"); err == nil {
			_ = resp.Body.Close()
		}
	}()
	require.Eventually(t, func() bool { return len(rec.timeline()) == 2 }, 5*time.Second, 10*time.Millisecond)

	lines := rec.timeline()
	require.Regexp(t, `^\+0\.000s took \d+(\.\d+)?m?s: 404 GET /v2/first signed range=""$`, lines[0])
	require.Regexp(t, `^\+\d+\.\d{3}s unfinished after \d+(\.\d+)?m?s: GET /v2/stall$`, lines[1])
	require.NotContains(t, strings.Join(lines, "\n"), "secret")
	require.Equal(t, []string{`404 GET /v2/first signed range=""`}, rec.log(), "log has finished requests only")
}
