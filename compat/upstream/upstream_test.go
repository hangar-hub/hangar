//go:build compat

// Package upstreamcompat is the Upstream Pull compatibility suite
// (docs/spec/13-testing.md "Upstream pulls"): Pulls from huggingface.co
// itself and from a second Instance, Partial mirrors, tracking,
// Pull-through, the bandwidth cap across two workers and resuming after a
// worker is killed. The huggingface.co tests need the network; CI runs
// the suite nightly and on pull requests that change internal/upstream.
package upstreamcompat

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"math"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/config"
	"github.com/jamesagarside/hangar/internal/db"
	"github.com/jamesagarside/hangar/internal/db/dbtest"
	"github.com/jamesagarside/hangar/internal/jobs"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/repo"
	"github.com/jamesagarside/hangar/internal/upstream"
	"github.com/jamesagarside/hangar/internal/upstream/upstreamtest"
)

const hfURL = "https://huggingface.co"

var (
	tinyBert   = model.RepoID{Type: model.RepoTypeModel, Namespace: "hf-internal-testing", Name: "tiny-random-bert"}
	tinyLlamas = model.RepoID{Type: model.RepoTypeModel, Namespace: "ggml-org", Name: "tiny-llamas"}
)

// realClient talks to huggingface.co with real backoff.
func realClient() *upstream.HTTPClient {
	return upstream.NewHTTPClient(upstream.HTTPOptions{Version: "compat"})
}

// hfHarness is a Harness whose default Upstream is huggingface.co.
func hfHarness(t *testing.T, d *db.DB) *upstreamtest.Harness {
	t.Helper()
	cfg := map[string]config.UpstreamConfig{upstream.DefaultUpstream: {BaseURL: hfURL, Enabled: true, Token: os.Getenv("HF_TOKEN")}}
	return upstreamtest.NewHarness(t, d, upstreamtest.HarnessOptions{Upstreams: cfg, Client: realClient(), PartSize: 8 << 20})
}

func hashOf(t *testing.T, rc io.ReadCloser) string {
	t.Helper()
	defer rc.Close()
	h := sha256.New()
	_, err := io.Copy(h, rc)
	require.NoError(t, err)
	return hex.EncodeToString(h.Sum(nil))
}

// gitIn runs git in dir.
func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
	return string(bytes.TrimSpace(out))
}

// TestPullTinyRepoFromHuggingFace pulls a whole Repo and checks the
// Mirror's Commit SHAs equal the Hub's and every LFS object's SHA-256.
func TestPullTinyRepoFromHuggingFace(t *testing.T) {
	h := hfHarness(t, dbtest.SQLite(t))
	alice := h.User("alice")
	hub, err := realClient().RepoInfo(h.Ctx, upstream.Upstream{Name: "hf", BaseURL: hfURL}, upstream.Credentials{}, tinyBert, "main")
	require.NoError(t, err)

	id, local, err := h.Service.StartPull(h.Ctx, upstream.PullRequest{Repo: tinyBert, Actor: alice})
	require.NoError(t, err)
	require.Equal(t, tinyBert, local)
	h.Run(30)
	j := h.Job(id)
	require.Equal(t, jobs.StatusSucceeded, j.Status, j.Error)

	head, err := h.Repos.Resolve(h.Ctx, tinyBert, "main")
	require.NoError(t, err)
	require.Equal(t, hub.SHA, head, "the Mirror's main is the Hub's sha")
	entries, err := h.Repos.Tree(h.Ctx, tinyBert, head, "", repo.TreeOpts{Recursive: true})
	require.NoError(t, err)
	lfs := 0
	for _, e := range entries {
		if e.LFS == nil {
			continue
		}
		lfs++
		require.True(t, e.Present, e.Path)
		rc, _, err := h.Repos.OpenFile(h.Ctx, tinyBert, head, e.Path, nil)
		require.NoError(t, err)
		require.Equal(t, e.LFS.OID, hashOf(t, rc), e.Path)
	}
	require.Positive(t, lfs)
	for _, sib := range hub.Siblings {
		_, err := h.Repos.Entry(h.Ctx, tinyBert, head, sib.Path)
		require.NoError(t, err, "%s is in the Mirror", sib.Path)
	}
	// The git history is the Hub's, and complete.
	view, err := h.Repos.OpenView(h.Ctx, tinyBert)
	require.NoError(t, err)
	defer view.Close()
	require.Equal(t, string(hub.SHA), gitIn(t, view.Dir, "rev-parse", "refs/heads/main"))
	gitIn(t, view.Dir, "fsck", "--no-dangling")
	t.Logf("pulled %s at %s: %d LFS objects, %d bytes", tinyBert, head, lfs, j.Progress.BytesTotal)
}

// TestPartialMirrorOfOneQuantFromHuggingFace pulls one GGUF quant.
func TestPartialMirrorOfOneQuantFromHuggingFace(t *testing.T) {
	h := hfHarness(t, dbtest.SQLite(t))
	alice := h.User("alice")
	id, _, err := h.Service.StartPull(h.Ctx, upstream.PullRequest{Repo: tinyLlamas, Actor: alice, Selection: upstream.Selection{Quants: []string{"Q4_0"}}})
	require.NoError(t, err)
	h.Run(30)
	j := h.Job(id)
	require.Equal(t, jobs.StatusSucceeded, j.Status, j.Error)
	head, err := h.Repos.Resolve(h.Ctx, tinyLlamas, "main")
	require.NoError(t, err)
	entries, err := h.Repos.Tree(h.Ctx, tinyLlamas, head, "", repo.TreeOpts{Recursive: true})
	require.NoError(t, err)
	present := map[string]bool{}
	for _, e := range entries {
		if e.LFS != nil {
			present[e.Path] = e.Present
		}
	}
	require.True(t, present["stories15M-q4_0.gguf"])
	require.False(t, present["stories15M-q8_0.gguf"])
	require.False(t, present["stories15M.gguf"])
	_, _, err = h.Repos.OpenFile(h.Ctx, tinyLlamas, head, "stories15M.gguf", nil)
	require.ErrorIs(t, err, repo.ErrNotPresent, "files not pulled are not in the arsenal")
	det, err := h.Service.DiscoverDetail(h.Ctx, alice, "", tinyLlamas, "")
	require.NoError(t, err)
	require.Equal(t, upstream.ArsenalPartial, det.Arsenal)
}

// TestPullThroughFromHuggingFace starts a Pull-through for a missing
// Repo and streams a file that is not in the arsenal as it arrives.
func TestPullThroughFromHuggingFace(t *testing.T) {
	h := hfHarness(t, dbtest.SQLite(t))
	alice := h.User("alice")
	h.SetSettings(upstream.Settings{PullThroughEnabled: true, PullThroughAllow: []string{"hf-internal-testing/*"}, PullThroughMinRole: "user"})
	ctx, cancel := context.WithCancel(h.Ctx)
	defer cancel()
	var wg sync.WaitGroup
	wg.Go(func() {
		for ctx.Err() == nil {
			h.Queue.RunPending(ctx)
			time.Sleep(50 * time.Millisecond)
		}
	})
	defer wg.Wait()
	defer cancel()
	pt := h.Service.PullThrough()
	r, err := pt.Repo(h.Ctx, alice, tinyBert)
	require.NoError(t, err)
	head, err := h.Repos.Resolve(h.Ctx, tinyBert, "main")
	require.NoError(t, err)
	e, err := h.Repos.Entry(h.Ctx, tinyBert, head, "onnx/model.onnx")
	require.NoError(t, err)
	require.False(t, e.Present)
	rc, err := pt.File(h.Ctx, alice, r, head, e)
	require.NoError(t, err)
	require.Equal(t, e.LFS.OID, hashOf(t, rc))
	e, err = h.Repos.Entry(h.Ctx, tinyBert, head, "onnx/model.onnx")
	require.NoError(t, err)
	require.True(t, e.Present, "the streamed file is now in the arsenal")
}

// TestTrackingPicksUpANewCommitFromASecondInstance uses a second Instance
// (here the upstreamtest Hub server) as the Upstream, so the test controls it.
func TestTrackingPicksUpANewCommitFromASecondInstance(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		h := upstreamtest.NewHarness(t, d, upstreamtest.HarnessOptions{})
		alice := h.User("alice")
		id := model.RepoID{Type: model.RepoTypeModel, Namespace: "lab", Name: "tracked"}
		h.Seed(id, map[string][]byte{"config.json": []byte(`{}`)}, map[string][]byte{"model.safetensors": []byte("v1 weights")})
		jid, _, err := h.Service.StartPull(h.Ctx, upstream.PullRequest{Repo: id, Actor: alice, Track: true})
		require.NoError(t, err)
		h.Run(10)
		require.Equal(t, jobs.StatusSucceeded, h.Job(jid).Status)
		scheds := h.Queue.Schedules()
		require.Len(t, scheds, 1)

		next := h.Seed(id, nil, map[string][]byte{"model.safetensors": []byte("v2 weights")})
		sync, err := h.Queue.Enqueue(h.Ctx, jobs.NewJob{Kind: jobs.KindMirrorSync, Payload: scheds[0].Payload})
		require.NoError(t, err)
		h.Run(10)
		require.Equal(t, jobs.StatusSucceeded, h.Job(sync).Status)
		head, err := h.Repos.Resolve(h.Ctx, id, "main")
		require.NoError(t, err)
		require.Equal(t, next, head)
		rc, _, err := h.Repos.OpenFile(h.Ctx, id, head, "model.safetensors", nil)
		require.NoError(t, err)
		b, _ := io.ReadAll(rc)
		_ = rc.Close()
		require.Equal(t, "v2 weights", string(b))
	})
}

// sharedBucket is one bandwidth budget spent by several workers, like
// jobs.DBLimiter on one database.
type sharedBucket struct {
	rate  float64
	mu    sync.Mutex
	next  time.Time
	spent atomic.Int64
	// first and last are when the first and last bytes were granted.
	first, last time.Time
}

func (b *sharedBucket) WaitN(ctx context.Context, _ string, n int64) error {
	b.mu.Lock()
	now := time.Now()
	if b.next.Before(now) {
		b.next = now
	}
	at := b.next
	b.next = b.next.Add(time.Duration(float64(n) / b.rate * float64(time.Second)))
	b.mu.Unlock()
	t := time.NewTimer(time.Until(at))
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
	}
	b.spent.Add(n)
	b.mu.Lock()
	if b.first.IsZero() {
		b.first = at
	}
	b.last = time.Now()
	b.mu.Unlock()
	return nil
}

// TestBandwidthCapAcrossTwoWorkers runs two workers that pull at once
// under one budget and checks the bytes they move stay within 10 % of
// the cap over the window (60 seconds by default;
// HANGAR_UPSTREAM_CAP_SECONDS changes it).
func TestBandwidthCapAcrossTwoWorkers(t *testing.T) {
	seconds := 60
	if v, err := strconv.Atoi(os.Getenv("HANGAR_UPSTREAM_CAP_SECONDS")); err == nil && v > 0 {
		seconds = v
	}
	const rate = 256 << 10 // bytes a second
	bucket := &sharedBucket{rate: rate}
	window := time.Duration(seconds) * time.Second
	// Together the two files fill the window at the cap.
	size := int(rate * float64(seconds) / 2)
	var hs []*upstreamtest.Harness
	for i := range 2 {
		h := upstreamtest.NewHarness(t, dbtest.SQLite(t), upstreamtest.HarnessOptions{PartSize: 1 << 20, Limiter: bucket})
		id := model.RepoID{Type: model.RepoTypeModel, Namespace: "big", Name: "repo-" + strconv.Itoa(i)}
		h.Seed(id, nil, map[string][]byte{"w.bin": bytes.Repeat([]byte{byte('a' + i)}, size)})
		_, _, err := h.Service.StartPull(h.Ctx, upstream.PullRequest{Repo: id, Actor: h.User("alice")})
		require.NoError(t, err)
		hs = append(hs, h)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*window)
	defer cancel()
	start := time.Now()
	var wg sync.WaitGroup
	for _, h := range hs {
		wg.Go(func() {
			for ctx.Err() == nil {
				h.Queue.RunPending(ctx)
				h.Advance(time.Second) // past the Pull's snoozed checks
				active, _, err := h.Queue.List(ctx, jobs.Filter{Kinds: []jobs.Kind{jobs.KindPull, jobs.KindPullFile}, Statuses: []jobs.Status{jobs.StatusPending, jobs.StatusRunning}})
				if err != nil || len(active) == 0 {
					return
				}
				time.Sleep(10 * time.Millisecond)
			}
		})
	}
	wg.Wait()
	require.NoError(t, ctx.Err(), "both Pulls finished")
	// The rate is measured from the first byte to the last, without the
	// planning before and the bookkeeping after.
	elapsed := bucket.last.Sub(bucket.first).Seconds()
	got := float64(bucket.spent.Load())
	want := rate * elapsed
	t.Logf("moved %.0f bytes in %.1f s across 2 workers (%.1f s with planning); cap %.0f bytes (%.1f %% of the cap)",
		got, elapsed, time.Since(start).Seconds(), want, 100*got/want)
	require.GreaterOrEqual(t, elapsed, 0.9*window.Seconds(), "the transfers ran for the whole window")
	require.Less(t, math.Abs(got-want)/want, 0.10, "the bandwidth cap holds within 10 %%")
}

// TestResumeAfterKillingTheWorkerMidFile stops a worker in the middle of
// a multipart transfer and finishes it with a new worker process, which
// resumes with an HTTP Range from the parts already uploaded.
func TestResumeAfterKillingTheWorkerMidFile(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		const part = 1 << 20
		h := upstreamtest.NewHarness(t, d, upstreamtest.HarnessOptions{PartSize: part})
		alice := h.User("alice")
		id := model.RepoID{Type: model.RepoTypeModel, Namespace: "big", Name: "resume"}
		content := bytes.Repeat([]byte("resume after a kill "), 5*part/20) // 5 MiB
		commit := h.Seed(id, nil, map[string][]byte{"w.bin": content})
		pull, _, err := h.Service.StartPull(h.Ctx, upstream.PullRequest{Repo: id, Actor: alice})
		require.NoError(t, err)
		// Plan only: run the pull Job and leave the pull.file Job queued.
		reg, _ := h.Jobs.Lookup(jobs.KindPull)
		pj := h.Job(pull)
		pp := &upstreamtest.Progress{}
		_ = reg.Handler(h.Ctx, pj, pp)
		files, _, err := h.Queue.List(h.Ctx, jobs.Filter{Kinds: []jobs.Kind{jobs.KindPullFile}})
		require.NoError(t, err)
		require.Len(t, files, 1)
		fj := files[0]

		// Worker one is killed after two and a half parts.
		ctx, kill := context.WithCancel(h.Ctx)
		h.Limiter.After(5*part/2, kill)
		one, _ := h.Jobs.Lookup(jobs.KindPullFile)
		p1 := &upstreamtest.Progress{}
		err = one.Handler(ctx, fj, p1)
		require.ErrorIs(t, err, context.Canceled)
		require.NotEmpty(t, p1.Result, "the upload ID was recorded")
		var res map[string]string
		require.NoError(t, json.Unmarshal(p1.Result, &res))
		require.NotEmpty(t, res["upload_id"])

		// Worker two is a new process with nothing in memory.
		svc2 := h.NewService(nil)
		reg2 := jobs.NewRegistry()
		require.NoError(t, svc2.RegisterJobs(reg2))
		two, _ := reg2.Lookup(jobs.KindPullFile)
		fj.Result = p1.Result
		require.NoError(t, two.Handler(h.Ctx, fj, &upstreamtest.Progress{}))
		require.Contains(t, h.Server.Ranges(), "bytes="+strconv.Itoa(2*part)+"-", "worker two resumes after the two finished parts")
		rc, _, err := h.Repos.OpenFile(h.Ctx, id, commit, "w.bin", nil)
		require.NoError(t, err)
		sum := sha256.Sum256(content)
		require.Equal(t, hex.EncodeToString(sum[:]), hashOf(t, rc))
	})
}
