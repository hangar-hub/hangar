package upstream_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/auth"
	"github.com/hangar-hub/hangar/internal/datasets"
	"github.com/hangar-hub/hangar/internal/db"
	"github.com/hangar-hub/hangar/internal/jobs"
	"github.com/hangar-hub/hangar/internal/model"
	"github.com/hangar-hub/hangar/internal/repo"
	"github.com/hangar-hub/hangar/internal/upstream"
	"github.com/hangar-hub/hangar/internal/upstream/upstreamtest"
	"github.com/hangar-hub/hangar/internal/webhook"
)

func readFile(t *testing.T, r repo.Service, id model.RepoID, commit model.CommitSHA, path string) []byte {
	t.Helper()
	rc, _, err := r.OpenFile(context.Background(), id, commit, path, nil)
	require.NoError(t, err)
	defer rc.Close()
	b, err := io.ReadAll(rc)
	require.NoError(t, err)
	return b
}

func pullState(t *testing.T, j *jobs.Job) map[string]any {
	t.Helper()
	var m map[string]any
	require.NoError(t, json.Unmarshal(j.Result, &m))
	return m
}

func TestPullWholeRepoKeepsSHAs(t *testing.T) {
	forEachDB(t, func(t *testing.T, d *db.DB) {
		e := newEnv(t, d, upstreamtest.HarnessOptions{PartSize: 1 << 10})
		alice := e.User("alice")
		big := bytes.Repeat([]byte("0123456789abcdef"), 300) // 4,800 bytes: five 1 KiB parts
		small := []byte("small weights")
		commit := e.Seed(tiny, map[string][]byte{"config.json": []byte(`{"model_type":"bert"}`)},
			map[string][]byte{"model.safetensors": big, "extra/tokenizer.bin": small})

		id, local, err := e.Service.StartPull(e.Ctx, upstream.PullRequest{Repo: tiny, Actor: alice})
		require.NoError(t, err)
		require.Equal(t, tiny, local)
		e.Run(10)

		j := e.Job(id)
		require.Equal(t, jobs.StatusSucceeded, j.Status, j.Error)
		st := pullState(t, j)
		require.Equal(t, "done", st["phase"])
		require.Equal(t, int64(len(big)+len(small)), j.Progress.BytesTotal)
		require.Equal(t, j.Progress.BytesTotal, j.Progress.BytesDone)
		require.Equal(t, 2, j.Progress.FilesDone)

		r, err := e.Repos.Get(e.Ctx, tiny)
		require.NoError(t, err)
		require.True(t, r.Mirror)
		got, err := e.Repos.Resolve(e.Ctx, tiny, "main")
		require.NoError(t, err)
		require.Equal(t, commit, got, "the Mirror keeps the upstream Commit SHA")
		require.Equal(t, big, readFile(t, e.Repos, tiny, commit, "model.safetensors"))
		require.Equal(t, small, readFile(t, e.Repos, tiny, commit, "extra/tokenizer.bin"))
		require.Equal(t, int64(len(big)+len(small)), e.Limiter.Bytes(upstream.BandwidthBudget), "every byte goes through the limiter")
		require.Zero(t, e.Server.AuthOnCDN())

		names := e.Hooks.Names()
		require.Contains(t, names, webhook.PullStarted)
		require.Contains(t, names, webhook.PullCompleted)
		idx, _, err := e.Queue.List(e.Ctx, jobs.Filter{Kinds: []jobs.Kind{jobs.KindIndex, jobs.KindScan}})
		require.NoError(t, err)
		require.Len(t, idx, 2, "the Commit is indexed and scanned")

		m, up, err := e.Service.Mirror(e.Ctx, r)
		require.NoError(t, err)
		require.Equal(t, "huggingface", up.Name)
		require.Equal(t, upstream.MirrorOK, m.Status)
		require.Equal(t, commit, m.LastSyncedCommit)
		require.True(t, m.Selection.All())

		// Pulling again needs no transfer or history fetch.
		gitCalls := e.Server.Requests("git")
		id2, _, err := e.Service.StartPull(e.Ctx, upstream.PullRequest{Repo: tiny, Actor: alice})
		require.NoError(t, err)
		e.Run(10)
		require.Equal(t, jobs.StatusSucceeded, e.Job(id2).Status)
		require.Equal(t, gitCalls, e.Server.Requests("git"), "no history fetch for a Revision already held")
		require.EqualValues(t, 2, pullState(t, e.Job(id2))["already_present"])
	})
}

func TestPartialMirrorOfOneQuant(t *testing.T) {
	forEachDB(t, func(t *testing.T, d *db.DB) {
		e := newEnv(t, d, upstreamtest.HarnessOptions{})
		alice := e.User("alice")
		gguf := model.RepoID{Type: model.RepoTypeModel, Namespace: "bartowski", Name: "Tiny-GGUF"}
		commit := e.Seed(gguf, map[string][]byte{"README.md": []byte("# tiny")}, map[string][]byte{
			"Tiny-Q4_K_M.gguf":              []byte("q4 weights"),
			"Tiny-Q8_0-00001-of-00002.gguf": []byte("q8 part one"),
			"Tiny-Q8_0-00002-of-00002.gguf": []byte("q8 part two"),
			"mmproj-Tiny-f16.gguf":          []byte("projector"),
			"Tiny-F16.gguf":                 []byte("f16 weights"),
		})
		id, _, err := e.Service.StartPull(e.Ctx, upstream.PullRequest{Repo: gguf, Selection: upstream.Selection{Quants: []string{"q4_k_m"}}, Actor: alice})
		require.NoError(t, err)
		e.Run(10)
		require.Equal(t, jobs.StatusSucceeded, e.Job(id).Status, e.Job(id).Error)

		entries, err := e.Repos.Tree(e.Ctx, gguf, commit, "", repo.TreeOpts{Recursive: true})
		require.NoError(t, err)
		require.Equal(t, map[string]bool{"Tiny-Q4_K_M.gguf": true, "mmproj-Tiny-f16.gguf": true, "Tiny-Q8_0-00001-of-00002.gguf": false,
			"Tiny-Q8_0-00002-of-00002.gguf": false, "Tiny-F16.gguf": false}, lfsIDs(entries))
		_, _, err = e.Repos.OpenFile(e.Ctx, gguf, commit, "Tiny-F16.gguf", nil)
		require.ErrorIs(t, err, repo.ErrNotPresent)

		// Pulling another quant later transfers only its files and skips the history fetch.
		fetches, cdn := e.Server.Requests("git"), e.Server.Requests("cdn")
		r, err := e.Repos.Get(e.Ctx, gguf)
		require.NoError(t, err)
		id2, err := e.Service.PullMore(e.Ctx, alice, r, upstream.Selection{Quants: []string{"Q8_0"}}, false)
		require.NoError(t, err)
		e.Run(10)
		require.Equal(t, jobs.StatusSucceeded, e.Job(id2).Status, e.Job(id2).Error)
		require.Equal(t, fetches, e.Server.Requests("git"))
		require.Equal(t, cdn+2, e.Server.Requests("cdn"), "only the two Q8_0 split files")
		entries, err = e.Repos.Tree(e.Ctx, gguf, commit, "", repo.TreeOpts{Recursive: true})
		require.NoError(t, err)
		require.False(t, lfsIDs(entries)["Tiny-F16.gguf"])
		require.True(t, lfsIDs(entries)["Tiny-Q8_0-00002-of-00002.gguf"])
		m, _, err := e.Service.Mirror(e.Ctx, r)
		require.NoError(t, err)
		require.Equal(t, []string{"Q4_K_M", "Q8_0"}, m.Selection.Quants, "the Mirror keeps the union of its selections by quant name")
	})
}

func TestPullDeduplicatesObjectsTheInstanceHolds(t *testing.T) {
	forEachDB(t, func(t *testing.T, d *db.DB) {
		e := newEnv(t, d, upstreamtest.HarnessOptions{})
		alice := e.User("alice")
		w := []byte("shared weights")
		e.Seed(tiny, nil, map[string][]byte{"model.bin": w})
		_, err := e.LFS.Put(e.Ctx, w)
		require.NoError(t, err)
		id, _, err := e.Service.StartPull(e.Ctx, upstream.PullRequest{Repo: tiny, Actor: alice})
		require.NoError(t, err)
		e.Run(10)
		require.Equal(t, jobs.StatusSucceeded, e.Job(id).Status)
		require.Zero(t, e.Server.Requests("cdn"), "nothing is transferred")
		files, _, err := e.Queue.List(e.Ctx, jobs.Filter{Kinds: []jobs.Kind{jobs.KindPullFile}})
		require.NoError(t, err)
		require.Empty(t, files)
		require.EqualValues(t, 1, pullState(t, e.Job(id))["already_present"])
	})
}

func TestPullResumesFromUploadedParts(t *testing.T) {
	forEachDB(t, func(t *testing.T, d *db.DB) {
		e := newEnv(t, d, upstreamtest.HarnessOptions{PartSize: 1 << 10})
		alice := e.User("alice")
		big := bytes.Repeat([]byte("resume!"), 1000) // 7,000 bytes, 7 parts
		commit := e.Seed(tiny, nil, map[string][]byte{"model.safetensors": big})
		// The connection drops after 3.5 parts on the first download.
		e.Server.BreakAfter(3584)
		id, _, err := e.Service.StartPull(e.Ctx, upstream.PullRequest{Repo: tiny, Actor: alice})
		require.NoError(t, err)
		e.Queue.RunPending(e.Ctx)
		files, _, err := e.Queue.List(e.Ctx, jobs.Filter{Kinds: []jobs.Kind{jobs.KindPullFile}})
		require.NoError(t, err)
		require.Len(t, files, 1)
		require.Equal(t, jobs.StatusPending, files[0].Status, "the transfer failed and will retry")
		require.Equal(t, 1, files[0].Attempts)

		e.Server.BreakAfter(0)
		e.Run(10)
		require.Equal(t, jobs.StatusSucceeded, e.Job(id).Status, e.Job(id).Error)
		require.Equal(t, big, readFile(t, e.Repos, tiny, commit, "model.safetensors"))
		ranges := e.Server.Ranges()
		require.Equal(t, []string{"bytes=3072-"}, ranges, "the retry resumes after the three parts already uploaded")
	})
}

func TestPullRejectsCorruptDownloadsAndRetries(t *testing.T) {
	forEachDB(t, func(t *testing.T, d *db.DB) {
		e := newEnv(t, d, upstreamtest.HarnessOptions{PartSize: 1 << 10})
		alice := e.User("alice")
		small, big := []byte("small object"), bytes.Repeat([]byte("x"), 3000)
		commit := e.Seed(tiny, nil, map[string][]byte{"a.bin": small, "b.bin": big})
		e.Server.CorruptNext(sha(small), 1)
		e.Server.CorruptNext(sha(big), 1)
		id, _, err := e.Service.StartPull(e.Ctx, upstream.PullRequest{Repo: tiny, Actor: alice})
		require.NoError(t, err)
		e.Run(20)
		require.Equal(t, jobs.StatusSucceeded, e.Job(id).Status, e.Job(id).Error)
		require.Equal(t, small, readFile(t, e.Repos, tiny, commit, "a.bin"))
		require.Equal(t, big, readFile(t, e.Repos, tiny, commit, "b.bin"))
		files, _, err := e.Queue.List(e.Ctx, jobs.Filter{Kinds: []jobs.Kind{jobs.KindPullFile}})
		require.NoError(t, err)
		for _, f := range files {
			require.Equal(t, 2, f.Attempts, "a corrupt object is rejected and downloaded again")
		}
	})
}

// TestPullPlansAgainWhenAnotherRunStoredTheHistory covers two runs storing
// a Mirror's history at once (a sync, another Pull, or a second run of the
// same Job after a lost lease). The run that loses gets repo.ErrRefConflict
// and must plan again at once, not back off as from a failure.
func TestPullPlansAgainWhenAnotherRunStoredTheHistory(t *testing.T) {
	forEachDB(t, func(t *testing.T, d *db.DB) {
		e := newEnv(t, d, upstreamtest.HarnessOptions{})
		alice := e.User("alice")
		w := []byte("weights")
		commit := e.Seed(tiny, map[string][]byte{"config.json": []byte(`{}`)}, map[string][]byte{"model.safetensors": w})
		var raced atomic.Bool
		e.Repos.BeforeApplyPack(func(ctx context.Context, p repo.ApplyPackParams) error {
			if p.Pack == nil || !raced.CompareAndSwap(false, true) {
				return nil
			}
			// The other run stores the same history first.
			if err := e.Repos.Fake.ApplyPack(ctx, p); err != nil {
				return err
			}
			return fmt.Errorf("apply: %w", repo.ErrRefConflict)
		})
		id, _, err := e.Service.StartPull(e.Ctx, upstream.PullRequest{Repo: tiny, Actor: alice})
		require.NoError(t, err)
		e.Queue.RunPending(e.Ctx)
		require.True(t, raced.Load(), "the race happened")
		j := e.Job(id)
		require.Equal(t, jobs.StatusPending, j.Status)
		require.Contains(t, j.Error, "Planning again: the Mirror's history changed while this Pull stored it",
			"the run that lost the race plans again: %s", j.Error)
		require.NotContains(t, j.Error, "store the upstream history")
		e.Run(10)
		j = e.Job(id)
		require.Equal(t, jobs.StatusSucceeded, j.Status, j.Error)
		require.Equal(t, w, readFile(t, e.Repos, tiny, commit, "model.safetensors"))
	})
}

// TestPullBacksOffFromAConflictThatKeepsComingBack bounds the immediate
// re-planning: a ref conflict on every try ends in the Job's normal
// retries with backoff after a few rounds.
func TestPullBacksOffFromAConflictThatKeepsComingBack(t *testing.T) {
	forEachDB(t, func(t *testing.T, d *db.DB) {
		e := newEnv(t, d, upstreamtest.HarnessOptions{})
		alice := e.User("alice")
		e.Seed(tiny, map[string][]byte{"config.json": []byte(`{}`)}, nil)
		e.Repos.BeforeApplyPack(func(context.Context, repo.ApplyPackParams) error {
			return fmt.Errorf("apply: %w", repo.ErrRefConflict)
		})
		id, _, err := e.Service.StartPull(e.Ctx, upstream.PullRequest{Repo: tiny, Actor: alice})
		require.NoError(t, err)
		for range 5 {
			e.Queue.RunPending(e.Ctx)
			require.Contains(t, e.Job(id).Error, "Planning again")
			e.Advance(time.Hour)
		}
		e.Queue.RunPending(e.Ctx)
		j := e.Job(id)
		require.Contains(t, j.Error, "store the upstream history", "the sixth conflict is a failure to retry with backoff")
		require.EqualValues(t, 5, pullState(t, j)["ref_conflicts"])
	})
}

func TestPullEndsPartialWhenFilesFail(t *testing.T) {
	forEachDB(t, func(t *testing.T, d *db.DB) {
		e := newEnv(t, d, upstreamtest.HarnessOptions{})
		alice := e.User("alice")
		e.Seed(tiny, nil, map[string][]byte{"good.bin": []byte("good"), "bad.bin": []byte("bad one")})
		e.Server.CorruptNext(sha([]byte("bad one")), 100)
		id, _, err := e.Service.StartPull(e.Ctx, upstream.PullRequest{Repo: tiny, Actor: alice})
		require.NoError(t, err)
		e.Run(40)
		j := e.Job(id)
		require.Equal(t, jobs.StatusFailed, j.Status)
		require.Contains(t, j.Error, "partial")
		require.Contains(t, j.Error, "bad.bin")
		require.Equal(t, "partial", pullState(t, j)["phase"])
		require.Contains(t, e.Hooks.Names(), webhook.PullFailed)
		r, _ := e.Repos.Get(e.Ctx, tiny)
		m, _, err := e.Service.Mirror(e.Ctx, r)
		require.NoError(t, err)
		require.Equal(t, upstream.MirrorPartial, m.Status)

		// Retry failed: the retried Pull transfers only the failed file.
		e.Server.CorruptNext(sha([]byte("bad one")), 0)
		cdn := e.Server.Requests("cdn")
		id2, err := e.Queue.Retry(e.Ctx, id)
		require.NoError(t, err)
		e.Run(10)
		require.Equal(t, jobs.StatusSucceeded, e.Job(id2).Status, e.Job(id2).Error)
		require.Equal(t, cdn+1, e.Server.Requests("cdn"))
	})
}

func TestPullWindowsAndPullNow(t *testing.T) {
	forEachDB(t, func(t *testing.T, d *db.DB) {
		e := newEnv(t, d, upstreamtest.HarnessOptions{PartSize: 1 << 10})
		alice := e.User("alice")
		big := bytes.Repeat([]byte("w"), 2500)
		commit := e.Seed(tiny, nil, map[string][]byte{"model.bin": big})
		e.SetSettings(upstream.Settings{Windows: []string{"01:00-07:00"}, AllowOverrideWindows: true})

		id, _, err := e.Service.StartPull(e.Ctx, upstream.PullRequest{Repo: tiny, Actor: alice})
		require.NoError(t, err)
		e.Queue.RunPending(e.Ctx)
		files, _, err := e.Queue.List(e.Ctx, jobs.Filter{Kinds: []jobs.Kind{jobs.KindPullFile}})
		require.NoError(t, err)
		require.Len(t, files, 1)
		require.Equal(t, "Waiting for pull window (01:00-07:00)", files[0].Progress.Message)
		require.Contains(t, files[0].Error, "snoozed until 2026-10-14T01:00:00Z")
		require.Zero(t, e.Server.Requests("cdn"), "nothing transfers outside the window")

		// Inside the window the transfer runs.
		e.Advance(14 * time.Hour) // 02:00
		e.Run(10)
		require.Equal(t, jobs.StatusSucceeded, e.Job(id).Status, e.Job(id).Error)
		require.Equal(t, big, readFile(t, e.Repos, tiny, commit, "model.bin"))

		// Pull now ignores the window, when the Instance allows it.
		other := model.RepoID{Type: model.RepoTypeModel, Namespace: "acme", Name: "other"}
		e.Seed(other, nil, map[string][]byte{"o.bin": []byte("other")})
		e.Advance(10 * time.Hour) // 12:00, outside
		id3, _, err := e.Service.StartPull(e.Ctx, upstream.PullRequest{Repo: other, Actor: alice, Now: true})
		require.NoError(t, err)
		e.Run(5)
		require.Equal(t, jobs.StatusSucceeded, e.Job(id3).Status, e.Job(id3).Error)
		e.SetSettings(upstream.Settings{Windows: []string{"01:00-07:00"}, AllowOverrideWindows: false})
		_, _, err = e.Service.StartPull(e.Ctx, upstream.PullRequest{Repo: other, Actor: alice, Now: true})
		require.ErrorIs(t, err, upstream.ErrOverrideNotAllowed)
	})
}

func TestRunningTransferWaitsAtPartBoundaryWhenWindowCloses(t *testing.T) {
	forEachDB(t, func(t *testing.T, d *db.DB) {
		e := newEnv(t, d, upstreamtest.HarnessOptions{PartSize: 1 << 10})
		alice := e.User("alice")
		big := bytes.Repeat([]byte("p"), 3000)
		commit := e.Seed(tiny, nil, map[string][]byte{"model.bin": big})
		e.Advance(-10 * time.Hour) // 02:00, inside the window
		e.SetSettings(upstream.Settings{Windows: []string{"01:00-07:00"}})
		// The window closes while the first part is transferring.
		e.Limiter.After(1024, func() { e.Advance(6 * time.Hour) }) // 08:00
		id, _, err := e.Service.StartPull(e.Ctx, upstream.PullRequest{Repo: tiny, Actor: alice})
		require.NoError(t, err)
		e.Queue.RunPending(e.Ctx)
		files, _, err := e.Queue.List(e.Ctx, jobs.Filter{Kinds: []jobs.Kind{jobs.KindPullFile}})
		require.NoError(t, err)
		require.Len(t, files, 1)
		require.Equal(t, jobs.StatusPending, files[0].Status)
		require.Contains(t, files[0].Error, "snoozed until 2026-10-14T01:00:00Z", "the transfer stops at the part boundary")
		require.Equal(t, "Waiting for pull window (01:00-07:00)", files[0].Progress.Message)

		e.Advance(18 * time.Hour) // 02:00 the next day
		e.Run(10)
		require.Equal(t, jobs.StatusSucceeded, e.Job(id).Status, e.Job(id).Error)
		require.Equal(t, big, readFile(t, e.Repos, tiny, commit, "model.bin"))
		require.Equal(t, []string{"bytes=1024-"}, e.Server.Ranges(), "the transfer resumes after the finished part")
	})
}

func TestStartPullRules(t *testing.T) {
	forEachDB(t, func(t *testing.T, d *db.DB) {
		e := newEnv(t, d, upstreamtest.HarnessOptions{})
		alice := e.User("alice")
		e.Seed(tiny, nil, map[string][]byte{"m.bin": []byte("m")})

		_, _, err := e.Service.StartPull(e.Ctx, upstream.PullRequest{Repo: tiny, Actor: model.Anonymous()})
		require.ErrorIs(t, err, auth.ErrForbidden)
		ro := alice
		ro.Token = &model.TokenInfo{Scope: model.ScopeRead}
		_, _, err = e.Service.StartPull(e.Ctx, upstream.PullRequest{Repo: tiny, Actor: ro})
		require.ErrorIs(t, err, auth.ErrForbidden)
		_, _, err = e.Service.StartPull(e.Ctx, upstream.PullRequest{Repo: tiny, Actor: alice, Upstream: "nope"})
		require.ErrorIs(t, err, upstream.ErrUpstreamNotFound)
		_, _, err = e.Service.StartPull(e.Ctx, upstream.PullRequest{Repo: tiny, Actor: alice, Selection: upstream.Selection{Paths: []string{"../x"}}})
		require.ErrorIs(t, err, upstream.ErrInvalidSelection)

		// A local Namespace with the upstream name blocks the Mirror.
		owned := model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "tiny"}
		e.Seed(owned, nil, map[string][]byte{"m.bin": []byte("m")})
		_, _, err = e.Service.StartPull(e.Ctx, upstream.PullRequest{Repo: owned, Actor: alice})
		require.ErrorIs(t, err, upstream.ErrConflict)
		require.Contains(t, err.Error(), "namespace_prefix")

		// The same Pull twice joins the existing Job.
		a, _, err := e.Service.StartPull(e.Ctx, upstream.PullRequest{Repo: tiny, Actor: alice})
		require.NoError(t, err)
		b, _, err := e.Service.StartPull(e.Ctx, upstream.PullRequest{Repo: tiny, Actor: alice})
		require.NoError(t, err)
		require.Equal(t, a, b)
		e.Run(10)
		require.Equal(t, jobs.StatusSucceeded, e.Job(a).Status)

		// A second Upstream cannot provide the same ID without a prefix.
		require.NoError(t, e.AddUpstream("mirror2", ""))
		_, _, err = e.Service.StartPull(e.Ctx, upstream.PullRequest{Upstream: "mirror2", Repo: tiny, Actor: alice})
		require.ErrorIs(t, err, upstream.ErrConflict)
		// With a prefix, it can, as hf-acme/tiny.
		require.NoError(t, e.AddUpstream("prefixed", "hf-"))
		id, local, err := e.Service.StartPull(e.Ctx, upstream.PullRequest{Upstream: "prefixed", Repo: tiny, Actor: alice})
		require.NoError(t, err)
		require.Equal(t, "hf-acme/tiny", local.String())
		e.Run(10)
		require.Equal(t, jobs.StatusSucceeded, e.Job(id).Status, e.Job(id).Error)
		ns, err := e.Orgs.Get(e.Ctx, "hf-acme")
		require.NoError(t, err)
		require.True(t, ns.ReservedMirror)

		// A local Repo is not a Mirror.
		bob := e.User("bob")
		local2 := model.RepoID{Type: model.RepoTypeModel, Namespace: "bob", Name: "mine"}
		_, err = e.Repos.Create(e.Ctx, repo.CreateParams{ID: local2, Actor: bob})
		require.NoError(t, err)
		require.NoError(t, e.AddUpstream("bobs", ""))
		_, _, err = e.Service.StartPull(e.Ctx, upstream.PullRequest{Upstream: "bobs", Repo: local2, Actor: bob})
		require.ErrorIs(t, err, upstream.ErrConflict)

		// Upstream errors end the Pull at once with a reason.
		missing := model.RepoID{Type: model.RepoTypeModel, Namespace: "acme", Name: "missing"}
		id, _, err = e.Service.StartPull(e.Ctx, upstream.PullRequest{Repo: missing, Actor: alice})
		require.NoError(t, err)
		e.Run(3)
		j := e.Job(id)
		require.Equal(t, jobs.StatusFailed, j.Status)
		require.Equal(t, 1, j.Attempts)
		require.Equal(t, "upstream_not_found", pullState(t, j)["reason"])
	})
}

func TestPullUsesTheUsersUpstreamToken(t *testing.T) {
	forEachDB(t, func(t *testing.T, d *db.DB) {
		e := newEnv(t, d, upstreamtest.HarnessOptions{})
		e.Remote.RequireToken = "users-token"
		alice := e.User("alice")
		priv := model.RepoID{Type: model.RepoTypeModel, Namespace: "acme", Name: "private"}
		_, err := e.Remote.Repos.Create(e.Ctx, repo.CreateParams{ID: priv, Visibility: model.VisibilityPrivate})
		require.NoError(t, err)
		e.Seed(priv, nil, map[string][]byte{"m.bin": []byte("secret weights")})

		id, _, err := e.Service.StartPull(e.Ctx, upstream.PullRequest{Repo: priv, Actor: alice})
		require.NoError(t, err)
		e.Run(3)
		require.Equal(t, jobs.StatusFailed, e.Job(id).Status, "anonymous cannot see the private Repo")

		e.SetToken(alice.UserID, "users-token")
		id, _, err = e.Service.StartPull(e.Ctx, upstream.PullRequest{Repo: priv, Actor: alice, Revision: "main"})
		require.NoError(t, err)
		e.Run(10)
		require.Equal(t, jobs.StatusSucceeded, e.Job(id).Status, e.Job(id).Error)
		r, err := e.Repos.Get(e.Ctx, priv)
		require.NoError(t, err)
		require.Equal(t, model.VisibilityPrivate, r.Visibility, "a private upstream Repo makes a private Mirror")
	})
}

func TestDatasetConfigSplitAndParquetOnlyPulls(t *testing.T) {
	forEachDB(t, func(t *testing.T, d *db.DB) {
		e := newEnv(t, d, upstreamtest.HarnessOptions{})
		alice := e.User("alice")
		ds := model.RepoID{Type: model.RepoTypeDataset, Namespace: "acme", Name: "data"}
		commit := e.Seed(ds, map[string][]byte{"README.md": []byte("---\nconfigs: []\n---\n")}, map[string][]byte{
			"data/train-00000.parquet": []byte("train rows"),
			"data/test-00000.parquet":  []byte("test rows"),
			"other/train.parquet":      []byte("other train"),
		})
		// The Parquet export lives on refs/convert/parquet.
		_, err := e.Remote.Repos.Commit(e.Ctx, repo.CommitParams{Repo: ds, Revision: "refs/convert/parquet", CreateFrom: commit, Summary: "export",
			Actor: model.System(), AllowMirror: true, Operations: []repo.Operation{{Kind: repo.OpAddLFS, Path: "default/train/0000.parquet",
				OID: mustPut(t, e, []byte("exported train")), Size: int64(len("exported train"))}}})
		require.NoError(t, err)
		resolved := &datasetsResolved{"default": {"train": {"data/train-*"}, "test": {"data/test-*"}}, "other": {"train": {"other/*"}}}
		e.Layout.Set(ds, commit, resolved.toResolved())

		id, _, err := e.Service.StartPull(e.Ctx, upstream.PullRequest{Repo: ds, Actor: alice, Selection: upstream.Selection{Configs: []string{"default"}, Splits: []string{"train"}}})
		require.NoError(t, err)
		e.Run(10)
		require.Equal(t, jobs.StatusSucceeded, e.Job(id).Status, e.Job(id).Error)
		entries, err := e.Repos.Tree(e.Ctx, ds, commit, "", repo.TreeOpts{Recursive: true})
		require.NoError(t, err)
		require.Equal(t, map[string]bool{"data/train-00000.parquet": true, "data/test-00000.parquet": false, "other/train.parquet": false}, lfsIDs(entries))

		r, _ := e.Repos.Get(e.Ctx, ds)
		id, err = e.Service.PullMore(e.Ctx, alice, r, upstream.Selection{Configs: []string{"default"}, Splits: []string{"train"}, ParquetOnly: true}, false)
		require.NoError(t, err)
		e.Run(10)
		require.Equal(t, jobs.StatusSucceeded, e.Job(id).Status, e.Job(id).Error)
		conv, err := e.Repos.Resolve(e.Ctx, ds, "refs/convert/parquet")
		require.NoError(t, err)
		require.Equal(t, []byte("exported train"), readFile(t, e.Repos, ds, conv, "default/train/0000.parquet"))

		_, err = e.Service.PullMore(e.Ctx, alice, r, upstream.Selection{Configs: []string{"nope"}}, false)
		require.NoError(t, err, "the Config is checked when the Pull plans")
		e.Run(5)
		failed, _, err := e.Queue.List(e.Ctx, jobs.Filter{Kinds: []jobs.Kind{jobs.KindPull}, Statuses: []jobs.Status{jobs.StatusFailed}})
		require.NoError(t, err)
		require.Len(t, failed, 1)
		require.Contains(t, failed[0].Error, `no Config "nope"`)
	})
}

func mustPut(t *testing.T, e *upstreamtest.Harness, b []byte) string {
	t.Helper()
	obj, err := e.Remote.Repos.LFS.Put(e.Ctx, b)
	require.NoError(t, err)
	return obj.OID
}

func TestTrackingFollowsNewCommitsAndForcePushes(t *testing.T) {
	forEachDB(t, func(t *testing.T, d *db.DB) {
		e := newEnv(t, d, upstreamtest.HarnessOptions{})
		alice := e.User("alice")
		gguf := model.RepoID{Type: model.RepoTypeModel, Namespace: "acme", Name: "track-GGUF"}
		first := e.Seed(gguf, nil, map[string][]byte{"m-Q4_K_M.gguf": []byte("q4 v1"), "m-Q8_0.gguf": []byte("q8 v1")})
		id, _, err := e.Service.StartPull(e.Ctx, upstream.PullRequest{Repo: gguf, Actor: alice, Track: true, Selection: upstream.Selection{Quants: []string{"Q4_K_M"}}})
		require.NoError(t, err)
		e.Run(10)
		require.Equal(t, jobs.StatusSucceeded, e.Job(id).Status, e.Job(id).Error)
		r, _ := e.Repos.Get(e.Ctx, gguf)
		scheds := e.Queue.Schedules()
		require.Len(t, scheds, 1)
		require.Equal(t, jobs.KindMirrorSync, scheds[0].Kind)
		require.Equal(t, r.UUID.String(), scheds[0].Key)
		require.Regexp(t, `^\d+ \*/6 \* \* \*$`, scheds[0].Cron)

		// The upstream renames the quant's file in a new Commit.
		obj := mustPut(t, e, []byte("q4 v2"))
		res, err := e.Remote.Repos.Commit(e.Ctx, repo.CommitParams{Repo: gguf, Summary: "rename", Actor: model.System(), Operations: []repo.Operation{
			{Kind: repo.OpDelete, Path: "m-Q4_K_M.gguf"}, {Kind: repo.OpAddLFS, Path: "model.Q4_K_M.gguf", OID: obj, Size: 5}}})
		require.NoError(t, err)
		sync, err := e.Queue.Enqueue(e.Ctx, jobs.NewJob{Kind: jobs.KindMirrorSync, Payload: scheds[0].Payload})
		require.NoError(t, err)
		e.Run(10)
		require.Equal(t, jobs.StatusSucceeded, e.Job(sync).Status)
		head, err := e.Repos.Resolve(e.Ctx, gguf, "main")
		require.NoError(t, err)
		require.Equal(t, res.SHA, head)
		require.Equal(t, []byte("q4 v2"), readFile(t, e.Repos, gguf, head, "model.Q4_K_M.gguf"), "the renamed file of the selected quant is followed")
		entries, _ := e.Repos.Tree(e.Ctx, gguf, head, "", repo.TreeOpts{Recursive: true})
		require.False(t, lfsIDs(entries)["m-Q8_0.gguf"])
		require.Contains(t, e.Hooks.Names(), webhook.MirrorSynced)

		// The upstream force-pushes main back to the first Commit's parent
		// line: the replaced tip stays reachable.
		require.NoError(t, e.Remote.Repos.UpdateRefs(e.Ctx, gguf, []repo.RefUpdate{{Name: "refs/heads/main", Old: res.SHA, New: first}}))
		sync, err = e.Queue.Enqueue(e.Ctx, jobs.NewJob{Kind: jobs.KindMirrorSync, Payload: scheds[0].Payload})
		require.NoError(t, err)
		e.Run(10)
		require.Equal(t, jobs.StatusSucceeded, e.Job(sync).Status)
		head, _ = e.Repos.Resolve(e.Ctx, gguf, "main")
		require.Equal(t, first, head, "upstream force-pushes are applied")
		refs, err := e.Repos.Refs(e.Ctx, gguf)
		require.NoError(t, err)
		var kept []string
		for _, o := range refs.Other {
			if strings.HasPrefix(o.Name, "refs/hangar/previous/") && o.Target == res.SHA {
				kept = append(kept, o.Name)
			}
		}
		require.Len(t, kept, 1, "the replaced tip is kept under refs/hangar/previous/")
		require.True(t, strings.HasSuffix(kept[0], "/heads/main"))

		// Turning tracking off removes the schedule.
		off := false
		_, err = e.Service.UpdateMirror(e.Ctx, alice, r, upstream.MirrorUpdate{Track: &off})
		require.NoError(t, err)
		require.Empty(t, e.Queue.Schedules())
		bad := "every day"
		_, err = e.Service.UpdateMirror(e.Ctx, alice, r, upstream.MirrorUpdate{Schedule: &bad})
		require.ErrorIs(t, err, upstream.ErrInvalidSelection)
	})
}

func TestDetachMakesAnOrdinaryRepo(t *testing.T) {
	forEachDB(t, func(t *testing.T, d *db.DB) {
		e := newEnv(t, d, upstreamtest.HarnessOptions{})
		alice := e.User("alice")
		e.Seed(tiny, nil, map[string][]byte{"m.bin": []byte("m")})
		id, _, err := e.Service.StartPull(e.Ctx, upstream.PullRequest{Repo: tiny, Actor: alice, Track: true})
		require.NoError(t, err)
		e.Run(10)
		require.Equal(t, jobs.StatusSucceeded, e.Job(id).Status)
		r, _ := e.Repos.Get(e.Ctx, tiny)
		require.ErrorIs(t, e.Service.Detach(e.Ctx, alice, r), auth.ErrForbidden, "detaching needs admin on the Namespace")
		admin := alice
		admin.IsAdmin = true
		require.NoError(t, e.Service.Detach(e.Ctx, admin, r))
		r, _ = e.Repos.Get(e.Ctx, tiny)
		require.False(t, r.Mirror)
		require.Empty(t, e.Queue.Schedules())
		m, _, err := e.Service.Mirror(e.Ctx, r)
		require.NoError(t, err)
		require.Equal(t, upstream.MirrorDetached, m.Status)
		_, _, err = e.Service.StartPull(e.Ctx, upstream.PullRequest{Repo: tiny, Actor: alice})
		require.ErrorIs(t, err, upstream.ErrConflict)
		require.ErrorIs(t, e.Service.Detach(e.Ctx, admin, r), upstream.ErrNotMirror)
	})
}

func TestSyncHandlerDropsSchedulesOfGoneMirrors(t *testing.T) {
	forEachDB(t, func(t *testing.T, d *db.DB) {
		e := newEnv(t, d, upstreamtest.HarnessOptions{})
		gone := "0198d2a4-0000-7000-8000-000000000000"
		require.NoError(t, e.Queue.SetSchedule(e.Ctx, jobs.Schedule{Kind: jobs.KindMirrorSync, Key: gone, Cron: "0 * * * *"}))
		id, err := e.Queue.Enqueue(e.Ctx, jobs.NewJob{Kind: jobs.KindMirrorSync, Payload: map[string]string{"repo_uuid": gone}})
		require.NoError(t, err)
		e.Run(2)
		require.Equal(t, jobs.StatusSucceeded, e.Job(id).Status, e.Job(id).Error)
		require.Empty(t, e.Queue.Schedules())
	})
}

// datasetsResolved maps Config names to Split names to file patterns.
type datasetsResolved map[string]map[string][]string

func (d datasetsResolved) toResolved() *datasets.Resolved {
	out := &datasets.Resolved{FromCard: true}
	for name, splits := range d {
		out.Configs = append(out.Configs, datasets.Config{Name: name, DataFiles: splits, Default: name == "default"})
	}
	return out
}
