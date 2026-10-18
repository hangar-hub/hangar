package cli

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/model"
)

// lastProgress returns the last progress line a command printed for label.
func lastProgress(stderr, label string) string {
	var last string
	for _, l := range strings.Split(stderr, "\n") {
		if i := strings.LastIndex(l, label+": "); i >= 0 {
			last = l[i:]
		}
	}
	return last
}

// TestPushRetriesFailedParts is the regression test for bellatrix finding
// 8: one part PUT that failed on a weak link aborted a 46 GB import. A part
// that fails for a reason that may pass is now sent again after a wait,
// and progress counts its bytes once.
func TestPushRetriesFailedParts(t *testing.T) {
	for _, tc := range []struct {
		name   string
		inject func(e *env, sha string)
	}{
		{"503 from a proxy", func(e *env, sha string) { e.inst.FailPartStatus(sha, 2, 1, http.StatusServiceUnavailable) }},
		{"500", func(e *env, sha string) { e.inst.FailPart(sha, 2, 2) }},
		{"connection reset half way through the body", func(e *env, sha string) { e.inst.ResetPart(sha, 2, 1) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.login()
			waits := recordWaits(t)
			dir := filepath.Join(e.home, "model")
			weights := blobOf("retry", 256<<10)
			writeTree(t, dir, map[string][]byte{"model.safetensors": weights})
			id := mustID(t, model.RepoTypeModel, "acme/retry")
			e.inst.AddRepo(id)
			tc.inject(e, sha(weights))

			r := e.ok("push", dir, "acme/retry")
			require.NotEmpty(t, *waits, "the part was sent again after a wait")
			for _, w := range *waits {
				require.LessOrEqual(t, w, maxTransferBackoff)
			}
			require.Equal(t, 4, e.inst.PartPuts(), "each of the four parts is stored once")
			require.Equal(t, weights, e.inst.Files(id, "main")["model.safetensors"])
			require.Equal(t, "Uploading: 100% 256.0 KiB of 256.0 KiB", lastProgress(r.stderr, "Uploading"),
				"a part sent twice counts once")
		})
	}
}

// TestPushDoesNotRetryAPartTheInstanceRefuses checks that an answer that
// will not change, such as 400, fails the part at once.
func TestPushDoesNotRetryAPartTheInstanceRefuses(t *testing.T) {
	e := newEnv(t)
	e.login()
	waits := recordWaits(t)
	dir := filepath.Join(e.home, "model")
	weights := blobOf("refused", 256<<10)
	writeTree(t, dir, map[string][]byte{"model.safetensors": weights})
	e.inst.AddRepo(mustID(t, model.RepoTypeModel, "acme/refused"))
	e.inst.FailPartStatus(sha(weights), 2, 1, http.StatusBadRequest)

	r := e.run("push", dir, "acme/refused")
	require.Equal(t, ExitFailure, r.code)
	require.Contains(t, r.err.Error(), "HTTP 400")
	require.Empty(t, *waits)
}

// TestPushRenewsExpiredPartURLs covers a file that takes longer to upload
// than its presigned part URLs last: S3 answers 403, and the CLI plans the
// file again, which resumes the same upload with new URLs.
func TestPushRenewsExpiredPartURLs(t *testing.T) {
	e := newEnv(t)
	e.login()
	recordWaits(t)
	e.inst.Presign = true
	dir := filepath.Join(e.home, "model")
	weights := blobOf("expired", 256<<10)
	writeTree(t, dir, map[string][]byte{"model.safetensors": weights})
	id := mustID(t, model.RepoTypeModel, "acme/expired")
	e.inst.AddRepo(id)
	e.inst.StalePartURLs(1)

	e.ok("push", dir, "acme/expired")
	require.Equal(t, 2, e.inst.RequestCount("POST /api/hangar/v1/repos/{type}/{ns}/{name}/uploads"), "one plan, and one to renew the URLs")
	require.Equal(t, 4, e.inst.PartPuts())
	require.Equal(t, weights, e.inst.Files(id, "main")["model.safetensors"])
}

// TestPushFinishesACompletionAProxyCutOff covers bellatrix finding 13c: a
// long completion that a proxy or the client's timeout cuts off carries on
// on the Instance. The CLI sends it again; the Instance has finished it
// and removed the upload, so the CLI plans the file again, finds the object
// stored and commits, without uploading anything again.
func TestPushFinishesACompletionAProxyCutOff(t *testing.T) {
	e := newEnv(t)
	e.login()
	waits := recordWaits(t)
	dir := filepath.Join(e.home, "model")
	weights := blobOf("cutoff", 256<<10)
	writeTree(t, dir, map[string][]byte{"model.safetensors": weights})
	id := mustID(t, model.RepoTypeModel, "acme/cutoff")
	e.inst.AddRepo(id)
	e.inst.DropCompletion(sha(weights), 1)

	r := e.ok("push", dir, "acme/cutoff")
	require.Contains(t, r.stderr, "Verifying model.safetensors (256.0 KiB) on the Instance\n", "the long wait is named")
	require.Contains(t, r.stderr, "The connection broke off while the Instance verified model.safetensors", "the retry is named")
	require.Contains(t, r.stderr, "Verifying model.safetensors (256.0 KiB) on the Instance, attempt 2 of 10\n")
	require.Equal(t, 2, e.inst.RequestCount(completeRoute))
	require.Len(t, *waits, 1)
	require.Equal(t, 4, e.inst.PartPuts(), "no part is sent again")
	require.Equal(t, weights, e.inst.Files(id, "main")["model.safetensors"])
}

// TestPullRetriesABrokenDownload covers downloads on a weak link: a body
// that breaks off resumes from where it stopped with a Range request, and
// a 503 is retried, without counting bytes twice.
func TestPullRetriesABrokenDownload(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
	}{{"body breaks off", 0}, {"503", http.StatusServiceUnavailable}} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.login()
			waits := recordWaits(t)
			id, files := ggufRepo(e)
			e.ok("pull", id.String(), "--server-only")
			e.inst.FailDownload("Tiny-Q8_0.gguf", 1, tc.status)

			out := filepath.Join(e.home, "out")
			r := e.ok("pull", id.String(), "--to", out, "--include", "Tiny-Q8_0.gguf")
			require.Equal(t, "Downloading: 100% 300.0 KiB of 300.0 KiB", lastProgress(r.stderr, "Downloading"), "bytes read twice count once")
			got, err := os.ReadFile(filepath.Join(out, "Tiny-Q8_0.gguf"))
			require.NoError(t, err)
			require.Equal(t, files["Tiny-Q8_0.gguf"], got)
			require.Equal(t, 2, e.inst.Downloads("Tiny-Q8_0.gguf"))
			require.Len(t, *waits, 1)
			_, err = os.Stat(filepath.Join(out, "Tiny-Q8_0.gguf.incomplete"))
			require.ErrorIs(t, err, os.ErrNotExist)
		})
	}
}

func TestTransferBackoffGrowsAndIsCapped(t *testing.T) {
	for attempt := 1; attempt <= 12; attempt++ {
		d := transferBackoff(attempt)
		require.Positive(t, d)
		require.LessOrEqual(t, d, maxTransferBackoff)
	}
	require.Less(t, transferBackoff(1), transferBackoff(6))
}

// TestImportSaysWhatItIsWaitingFor is the regression test for bellatrix
// finding 10: while the Instance looked a folder up on the Upstream and
// ran the Pull that creates the Mirror, the import printed nothing, so it
// looked stuck. It now names each step per Repo.
func TestImportSaysWhatItIsWaitingFor(t *testing.T) {
	e := newEnv(t)
	e.login()
	_, files := ggufRepo(e)
	lm := filepath.Join(e.home, ".lmstudio", "models")
	writeTree(t, lm, map[string][]byte{"bartowski/Tiny-GGUF/Tiny-Q8_0.gguf": files["Tiny-Q8_0.gguf"]})

	r := e.ok("import", "--from-lmstudio")
	for _, want := range []string{
		"Looking up bartowski/Tiny-GGUF on huggingface\n",
		"Creating a Mirror of bartowski/Tiny-GGUF: succeeded",
		"Hashing bartowski/Tiny-GGUF: 100% 300.0 KiB of 300.0 KiB",
		"Uploading 1 file, 300.0 KiB to bartowski/Tiny-GGUF.\n",
	} {
		require.Contains(t, r.stderr, want)
	}
	require.Less(t, strings.Index(r.stderr, "Looking up"), strings.Index(r.stderr, "Creating a Mirror"))

	// --json keeps stderr free of status lines.
	r = e.ok("import", "--from-lmstudio", "--json")
	require.NotContains(t, r.stderr, "Looking up")
}

// TestWaitStatusOnATerminal checks the status line counts the seconds on
// a terminal and is cleared when the wait ends.
func TestWaitStatusOnATerminal(t *testing.T) {
	origTerm, origTick := statusTerminal, statusTick
	statusTerminal = func(io.Writer) bool { return true }
	statusTick = time.Millisecond
	t.Cleanup(func() { statusTerminal, statusTick = origTerm, origTick })
	var buf syncBuffer
	a := &app{stderr: &buf}
	stop := a.waitStatus("Looking up acme/x on huggingface")
	require.Eventually(t, func() bool { return strings.Count(buf.String(), "\r\033[K") >= 3 }, 5*time.Second, time.Millisecond)
	stop()
	out := buf.String()
	require.True(t, strings.HasPrefix(out, "\r\033[KLooking up acme/x on huggingface (0s)"), out)
	require.True(t, strings.HasSuffix(out, "\r\033[K"), "the line is cleared at the end")
	n := len(out)
	time.Sleep(10 * time.Millisecond)
	require.Len(t, buf.String(), n, "nothing is drawn after the wait ends")

	quiet := &app{stderr: &buf, json: true}
	quiet.waitStatus("hidden")()
	require.NotContains(t, buf.String(), "hidden")
}

// syncBuffer is a bytes.Buffer safe for concurrent use.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
