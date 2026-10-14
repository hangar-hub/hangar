package upstream_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/db"
	"github.com/jamesagarside/hangar/internal/jobs"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/upstream"
	"github.com/jamesagarside/hangar/internal/upstream/upstreamtest"
)

// requireQuotaFailure checks that a pull Job failed because of the
// Namespace quota, with a message for users, and that its Mirror says so.
func requireQuotaFailure(t *testing.T, e *upstreamtest.Harness, id jobs.JobID, local model.RepoID) map[string]any {
	t.Helper()
	j := e.Job(id)
	require.Equal(t, jobs.StatusFailed, j.Status, "the Pull fails")
	st := pullState(t, j)
	require.Equal(t, "failed", st["phase"])
	require.Equal(t, "quota_exceeded", st["reason"], "the Job's result says the quota is exceeded")
	require.Contains(t, j.Error, "storage quota", "the Job's error explains the quota")
	require.Contains(t, j.Error, "ask an admin to raise the quota")
	require.Equal(t, "Storage quota exceeded", j.Progress.Message)
	r, err := e.Repos.Get(e.Ctx, local)
	require.NoError(t, err)
	m, _, err := e.Service.Mirror(e.Ctx, r)
	require.NoError(t, err)
	require.Equal(t, upstream.MirrorFailed, m.Status)
	require.Contains(t, m.LastError, "storage quota")
	return st
}

func pullFileJobs(t *testing.T, e *upstreamtest.Harness) []*jobs.Job {
	t.Helper()
	js, _, err := e.Queue.List(e.Ctx, jobs.Filter{Kinds: []jobs.Kind{jobs.KindPullFile}})
	require.NoError(t, err)
	return js
}

func TestQuotaExceededKeepsTheMessage(t *testing.T) {
	inner := &upstreamtest.QuotaRefusal{Used: 10, Adding: 5, Limit: 12}
	err := upstream.QuotaExceeded(inner)
	require.ErrorIs(t, err, upstream.ErrQuotaExceeded)
	require.Equal(t, inner.Error(), err.Error(), "the message for users is unchanged")
	var got *upstreamtest.QuotaRefusal
	require.ErrorAs(t, err, &got, "the checker's own error stays in the chain")
	require.NoError(t, upstream.QuotaExceeded(nil))
	wrapped := jobs.Permanent(err)
	require.ErrorIs(t, wrapped, upstream.ErrQuotaExceeded)
}

func TestPullQuotaRefusesHistoryBeforeApplyingThePack(t *testing.T) {
	forEachDB(t, func(t *testing.T, d *db.DB) {
		e := newEnv(t, d, upstreamtest.HarnessOptions{})
		alice := e.User("alice")
		ns := e.MirrorNamespace("acme")
		e.Quota.SetLimit(ns, 1)
		w := []byte("weights")
		commit := e.Seed(tiny, map[string][]byte{"config.json": []byte(`{}`)}, map[string][]byte{"model.safetensors": w})

		id, _, err := e.Service.StartPull(e.Ctx, upstream.PullRequest{Repo: tiny, Actor: alice})
		require.NoError(t, err)
		e.Run(10)
		requireQuotaFailure(t, e, id, tiny)
		require.Equal(t, 1, e.Job(id).Attempts, "a quota refusal is not retried")

		_, err = e.Repos.Resolve(e.Ctx, tiny, "main")
		require.Error(t, err, "no history is stored")
		require.Zero(t, e.Limiter.Bytes(upstream.BandwidthBudget), "no object is transferred")
		require.Empty(t, pullFileJobs(t, e))
		refused := e.Quota.Refusals()
		require.Len(t, refused, 1)
		require.Equal(t, ns, refused[0].Namespace)
		require.Positive(t, refused[0].Adding, "the pack's size is checked")

		// With the quota raised, retrying the Pull stores everything.
		e.Quota.SetLimit(ns, 0)
		retry, err := e.Queue.Retry(e.Ctx, id)
		require.NoError(t, err)
		e.Run(10)
		require.Equal(t, jobs.StatusSucceeded, e.Job(retry).Status, e.Job(retry).Error)
		require.Equal(t, w, readFile(t, e.Repos, tiny, commit, "model.safetensors"))
	})
}

func TestPullQuotaRefusesTheSelectionAtPlanning(t *testing.T) {
	forEachDB(t, func(t *testing.T, d *db.DB) {
		e := newEnv(t, d, upstreamtest.HarnessOptions{})
		alice := e.User("alice")
		ns := e.MirrorNamespace("acme")
		e.Quota.SetLimit(ns, 64<<10) // room for the history, not the weights
		big := bytes.Repeat([]byte("weights!"), 12<<10)
		commit := e.Seed(tiny, map[string][]byte{"config.json": []byte(`{}`)}, map[string][]byte{"model.safetensors": big})

		id, _, err := e.Service.StartPull(e.Ctx, upstream.PullRequest{Repo: tiny, Actor: alice})
		require.NoError(t, err)
		e.Run(10)
		requireQuotaFailure(t, e, id, tiny)
		require.Empty(t, pullFileJobs(t, e), "nothing is queued for transfer")
		require.Zero(t, e.Limiter.Bytes(upstream.BandwidthBudget))
		got, err := e.Repos.Resolve(e.Ctx, tiny, "main")
		require.NoError(t, err)
		require.Equal(t, commit, got, "the history fitted and is kept")
		en, err := e.Repos.Entry(e.Ctx, tiny, commit, "model.safetensors")
		require.NoError(t, err)
		require.False(t, en.Present)
		last := e.Quota.Refusals()
		require.Equal(t, int64(len(big)), last[len(last)-1].Adding, "the whole selection is checked")
	})
}

func TestPullQuotaCountsObjectsTheInstanceAlreadyHolds(t *testing.T) {
	forEachDB(t, func(t *testing.T, d *db.DB) {
		e := newEnv(t, d, upstreamtest.HarnessOptions{})
		alice := e.User("alice")
		w := bytes.Repeat([]byte("shared weights "), 1000)
		e.Seed(tiny, nil, map[string][]byte{"model.safetensors": w})
		twin := model.RepoID{Type: model.RepoTypeModel, Namespace: "beta", Name: "twin"}
		twinCommit := e.Seed(twin, nil, map[string][]byte{"model.safetensors": w})

		first, _, err := e.Service.StartPull(e.Ctx, upstream.PullRequest{Repo: tiny, Actor: alice})
		require.NoError(t, err)
		e.Run(10)
		require.Equal(t, jobs.StatusSucceeded, e.Job(first).Status, e.Job(first).Error)
		cdn := e.Server.Requests("cdn")

		// The object is in the Instance already, but not in beta: holding
		// it still counts towards beta's quota.
		beta := e.MirrorNamespace("beta")
		e.Quota.SetLimit(beta, 8<<10)
		id, _, err := e.Service.StartPull(e.Ctx, upstream.PullRequest{Repo: twin, Actor: alice})
		require.NoError(t, err)
		e.Run(10)
		requireQuotaFailure(t, e, id, twin)
		require.Empty(t, e.Repos.MarkedPresent(twin), "a refused object is not marked present")
		require.Equal(t, cdn, e.Server.Requests("cdn"))

		// Objects the Namespace already holds add nothing.
		e.Quota.Hold(beta, sha(w))
		retry, err := e.Queue.Retry(e.Ctx, id)
		require.NoError(t, err)
		e.Run(10)
		require.Equal(t, jobs.StatusSucceeded, e.Job(retry).Status, e.Job(retry).Error)
		require.EqualValues(t, 1, pullState(t, e.Job(retry))["already_present"])
		require.Contains(t, e.Repos.MarkedPresent(twin), sha(w))
		require.Equal(t, w, readFile(t, e.Repos, twin, twinCommit, "model.safetensors"))
		require.Equal(t, cdn, e.Server.Requests("cdn"), "no transfer")
	})
}

func TestPullQuotaRefusesTransfersAfterPlanning(t *testing.T) {
	forEachDB(t, func(t *testing.T, d *db.DB) {
		e := newEnv(t, d, upstreamtest.HarnessOptions{PartSize: 1 << 10})
		alice := e.User("alice")
		ns := e.MirrorNamespace("acme")
		const limit = 1 << 20
		e.Quota.SetLimit(ns, limit)
		big := bytes.Repeat([]byte("0123456789abcdef"), 300) // multipart: five 1 KiB parts
		small := []byte("small weights")
		commit := e.Seed(tiny, nil, map[string][]byte{"model.safetensors": big, "extra/tokenizer.bin": small})

		// Planned at noon; the transfers wait for the night window.
		e.SetSettings(upstream.Settings{Windows: []string{"01:00-07:00"}, AllowOverrideWindows: true})
		id, _, err := e.Service.StartPull(e.Ctx, upstream.PullRequest{Repo: tiny, Actor: alice})
		require.NoError(t, err)
		e.Run(1)
		require.Equal(t, "transferring", pullState(t, e.Job(id))["phase"])
		require.Len(t, pullFileJobs(t, e), 2)

		// Meanwhile the Namespace fills up.
		e.Quota.SetUsed(ns, limit-8)
		e.SetSettings(upstream.Settings{})
		e.Run(10)
		st := requireQuotaFailure(t, e, id, tiny)
		require.Zero(t, e.Limiter.Bytes(upstream.BandwidthBudget), "no byte is downloaded")
		require.Contains(t, e.Job(id).Error, "were not stored")
		files, _ := st["files"].([]any)
		require.Len(t, files, 2)
		for _, f := range files {
			fm := f.(map[string]any)
			require.Equal(t, "failed", fm["status"])
			require.Equal(t, "quota_exceeded", fm["reason"])
			require.Contains(t, fm["error"], "storage quota")
		}
		for _, fj := range pullFileJobs(t, e) {
			require.Equal(t, jobs.StatusFailed, fj.Status)
			var fr map[string]any
			require.NoError(t, json.Unmarshal(fj.Result, &fr))
			require.Equal(t, "quota_exceeded", fr["reason"], "the transfer Job says why it failed")
			require.Equal(t, "Storage quota exceeded", fj.Progress.Message)
		}

		// Space freed: retrying the Pull transfers the files.
		e.Quota.SetUsed(ns, 0)
		retry, err := e.Queue.Retry(e.Ctx, id)
		require.NoError(t, err)
		e.Run(10)
		require.Equal(t, jobs.StatusSucceeded, e.Job(retry).Status, e.Job(retry).Error)
		require.Equal(t, big, readFile(t, e.Repos, tiny, commit, "model.safetensors"))
		require.Equal(t, small, readFile(t, e.Repos, tiny, commit, "extra/tokenizer.bin"))
	})
}

func TestPullQuotaCountsAnObjectStoredWhileTheTransferWaited(t *testing.T) {
	forEachDB(t, func(t *testing.T, d *db.DB) {
		e := newEnv(t, d, upstreamtest.HarnessOptions{})
		alice := e.User("alice")
		ns := e.MirrorNamespace("acme")
		const limit = 1 << 20
		e.Quota.SetLimit(ns, limit)
		w := bytes.Repeat([]byte("stored elsewhere "), 100)
		commit := e.Seed(tiny, nil, map[string][]byte{"model.safetensors": w})
		e.SetSettings(upstream.Settings{Windows: []string{"01:00-07:00"}, AllowOverrideWindows: true})
		id, _, err := e.Service.StartPull(e.Ctx, upstream.PullRequest{Repo: tiny, Actor: alice})
		require.NoError(t, err)
		e.Run(1)

		// Another Namespace stores the same object before the window opens.
		_, err = e.LFS.Ingest(e.Ctx, sha(w), int64(len(w)), bytes.NewReader(w))
		require.NoError(t, err)
		e.Quota.SetUsed(ns, limit-8)
		e.SetSettings(upstream.Settings{})
		e.Run(10)
		requireQuotaFailure(t, e, id, tiny)
		require.Empty(t, e.Repos.MarkedPresent(tiny), "the stored object is not added to the Namespace")
		_, err = e.Repos.Entry(e.Ctx, tiny, commit, "model.safetensors")
		require.NoError(t, err)
	})
}

func TestPullQuotaChecksATransferSharedWithAnotherNamespace(t *testing.T) {
	forEachDB(t, func(t *testing.T, d *db.DB) {
		e := newEnv(t, d, upstreamtest.HarnessOptions{})
		alice := e.User("alice")
		w := bytes.Repeat([]byte("shared transfer "), 200)
		commit := e.Seed(tiny, nil, map[string][]byte{"model.safetensors": w})
		twin := model.RepoID{Type: model.RepoTypeModel, Namespace: "beta", Name: "twin"}
		twinCommit := e.Seed(twin, nil, map[string][]byte{"model.safetensors": w})
		beta := e.MirrorNamespace("beta")
		const limit = 1 << 20
		e.Quota.SetLimit(beta, limit)

		e.SetSettings(upstream.Settings{Windows: []string{"01:00-07:00"}, AllowOverrideWindows: true})
		a, _, err := e.Service.StartPull(e.Ctx, upstream.PullRequest{Repo: tiny, Actor: alice})
		require.NoError(t, err)
		b, _, err := e.Service.StartPull(e.Ctx, upstream.PullRequest{Repo: twin, Actor: alice})
		require.NoError(t, err)
		e.Run(1)
		filesOf := func(id jobs.JobID) []any {
			fs, _ := pullState(t, e.Job(id))["files"].([]any)
			require.Len(t, fs, 1)
			return fs
		}
		require.Equal(t, filesOf(a)[0].(map[string]any)["job"], filesOf(b)[0].(map[string]any)["job"], "both Pulls share one transfer")

		// beta fills up before the shared transfer, which acme's Pull
		// started, runs.
		e.Quota.SetUsed(beta, limit-8)
		e.SetSettings(upstream.Settings{})
		e.Run(10)
		require.Equal(t, jobs.StatusSucceeded, e.Job(a).Status, e.Job(a).Error)
		require.Equal(t, w, readFile(t, e.Repos, tiny, commit, "model.safetensors"))
		requireQuotaFailure(t, e, b, twin)
		require.Contains(t, e.Repos.MarkedPresent(tiny), sha(w))
		require.Empty(t, e.Repos.MarkedPresent(twin), "beta does not gain the object over its quota")
		_, err = e.Repos.Entry(e.Ctx, twin, twinCommit, "model.safetensors")
		require.NoError(t, err)
	})
}

func TestPullQuotaCheckFailuresAreRetried(t *testing.T) {
	forEachDB(t, func(t *testing.T, d *db.DB) {
		e := newEnv(t, d, upstreamtest.HarnessOptions{})
		alice := e.User("alice")
		w := []byte("weights")
		commit := e.Seed(tiny, nil, map[string][]byte{"model.safetensors": w})
		e.Quota.FailNext(errors.New("database is unavailable"))
		id, _, err := e.Service.StartPull(e.Ctx, upstream.PullRequest{Repo: tiny, Actor: alice})
		require.NoError(t, err)
		e.Run(10)
		j := e.Job(id)
		require.Equal(t, jobs.StatusSucceeded, j.Status, j.Error)
		require.GreaterOrEqual(t, j.Attempts, 2, "the failed check was retried, not taken as a refusal")
		require.Nil(t, pullState(t, j)["reason"])
		require.Equal(t, w, readFile(t, e.Repos, tiny, commit, "model.safetensors"))
	})
}

func TestPullThroughQuota(t *testing.T) {
	forEachDB(t, func(t *testing.T, d *db.DB) {
		e := newEnv(t, d, upstreamtest.HarnessOptions{})
		alice := e.User("alice")
		w := []byte("lfs object over quota")
		commit := e.Seed(tiny, nil, map[string][]byte{"model.bin": w})
		id, _, err := e.Service.StartPull(e.Ctx, upstream.PullRequest{Repo: tiny, Actor: alice, Selection: upstream.Selection{None: true}})
		require.NoError(t, err)
		e.Run(5)
		require.Equal(t, jobs.StatusSucceeded, e.Job(id).Status, e.Job(id).Error)
		e.Quota.SetLimit(e.MirrorNamespace("acme"), 1)
		e.SetSettings(upstream.Settings{PullThroughEnabled: true, PullThroughAllow: []string{"*"}, PullThroughMinRole: "user"})
		r, err := e.Repos.Get(e.Ctx, tiny)
		require.NoError(t, err)
		en, err := e.Repos.Entry(e.Ctx, tiny, commit, "model.bin")
		require.NoError(t, err)
		pt := e.Service.PullThrough()
		cdn := e.Server.Requests("cdn")
		fileJobs := len(pullFileJobs(t, e))

		_, err = pt.File(e.Ctx, alice, r, commit, en)
		require.ErrorIs(t, err, upstream.ErrQuotaExceeded)
		require.Contains(t, err.Error(), "storage quota")

		href, ok, err := pt.DownloadURL(e.Ctx, alice, r, sha(w), int64(len(w)))
		require.NoError(t, err)
		require.True(t, ok)
		srv := httptest.NewServer(pt.Handler())
		defer srv.Close()
		u, _ := url.Parse(href)
		resp, err := http.Get(srv.URL + u.RequestURI())
		require.NoError(t, err)
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		require.Equal(t, http.StatusForbidden, resp.StatusCode)
		require.Contains(t, string(body), "storage quota")

		require.Equal(t, cdn, e.Server.Requests("cdn"), "nothing is transferred")
		require.Len(t, pullFileJobs(t, e), fileJobs, "no transfer is queued")
		require.NotContains(t, e.Repos.MarkedPresent(tiny), sha(w))

		// A Pull-through of a new Repo whose history does not fit.
		gamma := model.RepoID{Type: model.RepoTypeModel, Namespace: "gamma", Name: "new"}
		e.Seed(gamma, map[string][]byte{"config.json": []byte(`{}`)}, nil)
		e.Quota.SetLimit(e.MirrorNamespace("gamma"), 1)
		ctx, cancel := context.WithCancel(e.Ctx)
		defer cancel()
		var wg sync.WaitGroup
		wg.Go(func() {
			for ctx.Err() == nil {
				e.Queue.RunPending(ctx)
				time.Sleep(20 * time.Millisecond)
			}
		})
		_, err = pt.Repo(e.Ctx, alice, gamma)
		cancel()
		wg.Wait()
		require.ErrorIs(t, err, upstream.ErrQuotaExceeded)
		require.Contains(t, err.Error(), "storage quota")
		_, err = e.Repos.Resolve(e.Ctx, gamma, "main")
		require.Error(t, err, "no history is stored")
	})
}
