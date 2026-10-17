package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/model"
)

const completeRoute = "POST /api/hangar/v1/repos/{type}/{ns}/{name}/uploads/{id}/complete"

// recordWaits replaces waitRetry for one test and returns the waits asked
// for. Parts upload in parallel, so the record is locked; read it once the
// command has returned.
func recordWaits(t *testing.T) *[]time.Duration {
	t.Helper()
	var (
		mu    sync.Mutex
		waits []time.Duration
	)
	orig := waitRetry
	waitRetry = func(ctx context.Context, d time.Duration) error {
		mu.Lock()
		waits = append(waits, d)
		mu.Unlock()
		return ctx.Err()
	}
	t.Cleanup(func() { waitRetry = orig })
	return &waits
}

func TestPushResendsCompletionAfter503WithRetryAfter(t *testing.T) {
	e := newEnv(t)
	e.login()
	waits := recordWaits(t)
	dir := filepath.Join(e.home, "model")
	weights := blobOf("complete", 200<<10)
	writeTree(t, dir, map[string][]byte{"model.safetensors": weights})
	e.inst.AddRepo(mustID(t, model.RepoTypeModel, "acme/complete"))
	e.inst.FailComplete(sha(weights), 2, "5")

	e.ok("push", dir, "acme/complete")
	require.Equal(t, 3, e.inst.RequestCount(completeRoute), "two 503s, then the completion that succeeds")
	require.Equal(t, []time.Duration{5 * time.Second, 5 * time.Second}, *waits, "the CLI waits as long as Retry-After says")
	require.Equal(t, 4, e.inst.PartPuts(), "no part is sent again")
	require.Equal(t, weights, e.inst.Files(mustID(t, model.RepoTypeModel, "acme/complete"), "main")["model.safetensors"])
}

func TestPushStopsResendingCompletionAfterTheLimit(t *testing.T) {
	e := newEnv(t)
	e.login()
	waits := recordWaits(t)
	dir := filepath.Join(e.home, "model")
	weights := blobOf("limit", 100<<10)
	writeTree(t, dir, map[string][]byte{"model.safetensors": weights})
	e.inst.AddRepo(mustID(t, model.RepoTypeModel, "acme/limit"))
	e.inst.FailComplete(sha(weights), completeAttempts, "1")

	r := e.run("push", dir, "acme/limit")
	require.Equal(t, ExitFailure, r.code)
	require.Contains(t, r.err.Error(), "HTTP 503")
	require.Contains(t, r.err.Error(), "run the command again")
	require.Equal(t, completeAttempts, e.inst.RequestCount(completeRoute))
	require.Len(t, *waits, completeAttempts-1)

	// The Instance kept the upload, so the next run finishes it without
	// sending any part again.
	puts := e.inst.PartPuts()
	e.ok("push", dir, "acme/limit")
	require.Equal(t, puts, e.inst.PartPuts())
	require.Equal(t, weights, e.inst.Files(mustID(t, model.RepoTypeModel, "acme/limit"), "main")["model.safetensors"])
}

func TestPushDoesNotResendCompletionWithoutRetryAfter(t *testing.T) {
	e := newEnv(t)
	e.login()
	waits := recordWaits(t)
	dir := filepath.Join(e.home, "model")
	weights := blobOf("noretry", 100<<10)
	writeTree(t, dir, map[string][]byte{"model.safetensors": weights})
	e.inst.AddRepo(mustID(t, model.RepoTypeModel, "acme/noretry"))
	e.inst.FailComplete(sha(weights), 1, "")

	r := e.run("push", dir, "acme/noretry")
	require.Equal(t, ExitFailure, r.code)
	require.Contains(t, r.err.Error(), "HTTP 503")
	require.Equal(t, 1, e.inst.RequestCount(completeRoute), "a 503 without Retry-After does not say the upload was kept")
	require.Empty(t, *waits)
}

func TestImportResendsCompletionAfter503WithRetryAfter(t *testing.T) {
	e := newEnv(t)
	e.login()
	waits := recordWaits(t)
	id, files := ggufRepo(e)
	cache := filepath.Join(e.home, "hf-cache")
	commit := strings.Repeat("ef", 20)
	gguf := files["Tiny-Q4_K_M.gguf"]
	cacheSnapshot(t, cache, id, commit, map[string][]byte{"Tiny-Q4_K_M.gguf": gguf})
	require.Greater(t, len(gguf), 1<<10)
	e.inst.InlineMax = 1 << 10 // so the GGUF goes up as a multipart upload
	e.inst.FailComplete(sha(gguf), 1, "7")

	r := e.ok("import", "--from-hf-cache", "--json")
	var res []importResult
	require.NoError(t, json.Unmarshal([]byte(r.stdout), &res))
	require.Len(t, res, 1)
	require.Empty(t, res[0].Error)
	require.Equal(t, []string{"Tiny-Q4_K_M.gguf"}, res[0].Uploaded)
	require.Equal(t, []time.Duration{7 * time.Second}, *waits)
	require.Equal(t, 2, e.inst.RequestCount(completeRoute))
	require.True(t, e.inst.Present(id, commit, "Tiny-Q4_K_M.gguf"))
}

func TestCompletionWaitEndsWithTheContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	require.ErrorIs(t, waitRetry(ctx, time.Hour), context.Canceled)
	require.Less(t, time.Since(start), time.Second)
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		in   string
		want time.Duration
		ok   bool
	}{
		{"5", 5 * time.Second, true},
		{" 0 ", 0, true},
		{"86400", maxRetryAfter, true},
		{now.Add(30 * time.Second).Format(http.TimeFormat), 30 * time.Second, true},
		{now.Add(-time.Minute).Format(http.TimeFormat), 0, true},
		{"", 0, false},
		{"-3", 0, false},
		{"soon", 0, false},
	} {
		got, ok := parseRetryAfter(tc.in, now)
		require.Equal(t, tc.ok, ok, tc.in)
		require.Equal(t, tc.want, got, tc.in)
	}
}

func TestAPIErrorIsRetryableOnlyFor503WithRetryAfter(t *testing.T) {
	h := http.Header{"Retry-After": []string{"5"}}
	var ae *APIError
	require.ErrorAs(t, apiError(http.StatusServiceUnavailable, h, nil), &ae)
	require.True(t, ae.Retryable)
	require.Equal(t, 5*time.Second, ae.RetryAfter)
	require.ErrorAs(t, apiError(http.StatusServiceUnavailable, http.Header{}, nil), &ae)
	require.False(t, ae.Retryable)
	require.ErrorAs(t, apiError(http.StatusInternalServerError, h, nil), &ae)
	require.False(t, ae.Retryable)
}
