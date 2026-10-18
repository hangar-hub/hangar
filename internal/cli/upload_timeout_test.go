package cli

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/model"
)

// shortCompletionTimeout makes every completion wait at most d for its
// answer, for one test.
func shortCompletionTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	orig := completionTimeout
	completionTimeout = func(int64) time.Duration { return d }
	t.Cleanup(func() { completionTimeout = orig })
}

// TestLongCompletionsSayWhatTheyWaitFor is the regression test for the
// 19.85 GB GGUF import on the bellatrix preview: the CLI's 5-minute
// response timeout cut off the upload completion while the Instance was
// still verifying the file, the automatic retry got 200 a minute later,
// and the CLI printed nothing the whole time. It now shows a status line
// while it waits, and one line when it asks again after a timeout.
func TestLongCompletionsSayWhatTheyWaitFor(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(e *env) (result, string)
	}{
		{"push", func(e *env) (result, string) {
			dir := filepath.Join(e.home, "model")
			weights := blobOf("slow", 256<<10)
			writeTree(e.t, dir, map[string][]byte{"model.safetensors": weights})
			e.inst.AddRepo(mustID(e.t, model.RepoTypeModel, "acme/slow"))
			e.inst.SlowCompletion(sha(weights), 1, time.Minute)
			return e.ok("push", dir, "acme/slow"), "model.safetensors (256.0 KiB)"
		}},
		{"import", func(e *env) (result, string) {
			dir := filepath.Join(e.home, "weights")
			weights := blobOf("slow gguf", 300<<10)
			writeTree(e.t, dir, map[string][]byte{"w-Q8_0.gguf": weights})
			// The fake's new Repo has no .gitattributes; send the GGUF
			// through the upload API, as a real Instance's LFS rules do.
			e.inst.InlineMax = 1 << 10
			e.inst.SlowCompletion(sha(weights), 1, time.Minute)
			return e.ok("import", "--from-dir", dir, "--repo", "acme/slow-gguf"), "w-Q8_0.gguf (300.0 KiB)"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.login()
			waits := recordWaits(t)
			// The fake Instance would take a minute; the CLI waits a second.
			shortCompletionTimeout(t, time.Second)
			start := time.Now()
			r, file := tc.run(e)
			require.Less(t, time.Since(start), 30*time.Second, "the CLI stopped waiting after its timeout")
			path := strings.SplitN(file, " ", 2)[0]
			require.Contains(t, r.stderr, "Verifying "+file+" on the Instance\n")
			require.Contains(t, r.stderr, "No answer from the Instance within 1s while it verified "+path+
				". It carries on verifying, so the CLI asks again.\n")
			require.Equal(t, 1, strings.Count(r.stderr, "No answer from the Instance"), "one line per retry")
			require.Contains(t, r.stderr, "Verifying "+file+" on the Instance, attempt 2 of 10\n")
			// The fake counts a request when its handler returns, and the
			// first, abandoned completion returns only once the server
			// notices the client went away, which can be after the CLI exits.
			require.Eventually(t, func() bool { return e.inst.RequestCount(completeRoute) == 2 },
				5*time.Second, 10*time.Millisecond, "the completion is sent again")
			require.Len(t, *waits, 1)
		})
	}
}

// TestCompletionTimeoutGrowsWithTheFile checks the completion's wait for
// an answer: 5 minutes, or 40 seconds per GiB for a file larger than 7.5
// GiB. The 19.85 GB GGUF that the Instance took about 6 minutes to verify
// gets more than 12 minutes.
func TestCompletionTimeoutGrowsWithTheFile(t *testing.T) {
	require.Equal(t, 5*time.Minute, completionTimeout(0))
	require.Equal(t, 5*time.Minute, completionTimeout(1<<30))
	require.Equal(t, 5*time.Minute, completionTimeout(15<<29))
	require.Equal(t, 800*time.Second, completionTimeout(20<<30))
	require.Greater(t, completionTimeout(19_850_000_000), 12*time.Minute)
	require.Equal(t, 40*time.Minute, completionTimeout(60<<30))
}

// TestResponseTimeoutIsPerRequest checks the CLI's HTTP client: a request
// whose context sets a longer wait (withResponseTimeout) outlasts the
// default, a request past its wait fails with a ResponseTimeoutError that
// the retry logic treats as a cut-off, and a slow body after the headers
// is not cut.
func TestResponseTimeoutIsPerRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow-headers" {
			select {
			case <-time.After(600 * time.Millisecond):
			case <-r.Context().Done():
				return
			}
		}
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		if r.URL.Path == "/slow-body" {
			time.Sleep(600 * time.Millisecond)
		}
		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(srv.Close)
	hc := newHTTPClient()
	hc.Transport = responseTimeout{next: hc.Transport.(responseTimeout).next, wait: 200 * time.Millisecond}
	get := func(ctx context.Context, path string) (string, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+path, nil)
		require.NoError(t, err)
		resp, err := redactingDoer{hc}.Do(req)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		return string(b), err
	}
	ctx := context.Background()

	_, err := get(ctx, "/slow-headers")
	var te *ResponseTimeoutError
	require.ErrorAs(t, err, &te)
	require.Equal(t, 200*time.Millisecond, te.Wait)
	require.True(t, completionCutOff(ctx, err), "a timed-out completion is sent again")
	require.False(t, errors.Is(err, context.Canceled))

	body, err := get(withResponseTimeout(ctx, 5*time.Second), "/slow-headers")
	require.NoError(t, err)
	require.Equal(t, "ok", body)

	body, err = get(ctx, "/slow-body")
	require.NoError(t, err, "the wait covers the headers only")
	require.Equal(t, "ok", body)

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = get(cancelled, "/slow-headers")
	require.ErrorIs(t, err, context.Canceled, "a cancelled command is not a timeout")
	require.False(t, completionCutOff(cancelled, err))
}

// TestWaitStatusRepeatsInALog checks that a status line on stderr that is
// not a terminal is printed again with the time taken, so a log of a long
// wait shows it is still going.
func TestWaitStatusRepeatsInALog(t *testing.T) {
	origTerm, origRepeat := statusTerminal, statusRepeat
	statusTerminal = func(io.Writer) bool { return false }
	statusRepeat = time.Millisecond
	t.Cleanup(func() { statusTerminal, statusRepeat = origTerm, origRepeat })
	var buf syncBuffer
	a := &app{stderr: &buf}
	stop := a.waitStatus("Verifying w.gguf (19.8 GiB) on the Instance")
	require.Eventually(t, func() bool {
		return strings.Count(buf.String(), "Verifying w.gguf (19.8 GiB) on the Instance (") >= 2
	}, 5*time.Second, time.Millisecond)
	stop()
	out := buf.String()
	require.True(t, strings.HasPrefix(out, "Verifying w.gguf (19.8 GiB) on the Instance\n"), out)
	require.NotContains(t, out, "\r", "no terminal control codes in a log")
	n := len(out)
	time.Sleep(10 * time.Millisecond)
	require.Len(t, buf.String(), n, "nothing is printed after the wait ends")
}
