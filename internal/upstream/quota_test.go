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

	"github.com/hangar-hub/hangar/internal/db"
	"github.com/hangar-hub/hangar/internal/jobs"
	"github.com/hangar-hub/hangar/internal/model"
	"github.com/hangar-hub/hangar/internal/upstream"
	"github.com/hangar-hub/hangar/internal/upstream/upstreamtest"
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

// TestPullQuotaOfTheStartingNamespaceDoesNotFailAJoinedPull covers the
// other direction: the Namespace whose Pull started the shared transfer
// is full, and a Pull that joined it from a Namespace with room must
// still complete, without seeing the other Namespace's usage or limit.
func TestPullQuotaOfTheStartingNamespaceDoesNotFailAJoinedPull(t *testing.T) {
	forEachDB(t, func(t *testing.T, d *db.DB) {
		e := newEnv(t, d, upstreamtest.HarnessOptions{})
		alice := e.User("alice")
		w := bytes.Repeat([]byte("shared transfer "), 200)
		commit := e.Seed(tiny, nil, map[string][]byte{"model.safetensors": w})
		twin := model.RepoID{Type: model.RepoTypeModel, Namespace: "beta", Name: "twin"}
		twinCommit := e.Seed(twin, nil, map[string][]byte{"model.safetensors": w})
		acme := e.MirrorNamespace("acme")
		const limit = 987654321
		e.Quota.SetLimit(acme, limit)

		e.SetSettings(upstream.Settings{Windows: []string{"01:00-07:00"}, AllowOverrideWindows: true})
		a, _, err := e.Service.StartPull(e.Ctx, upstream.PullRequest{Repo: tiny, Actor: alice})
		require.NoError(t, err)
		b, _, err := e.Service.StartPull(e.Ctx, upstream.PullRequest{Repo: twin, Actor: alice})
		require.NoError(t, err)
		e.Run(1)
		fileOf := func(id jobs.JobID) map[string]any {
			fs, _ := pullState(t, e.Job(id))["files"].([]any)
			require.Len(t, fs, 1)
			return fs[0].(map[string]any)
		}
		shared := fileOf(a)["job"]
		require.Equal(t, shared, fileOf(b)["job"], "both Pulls share one transfer, started by acme's Pull")

		// acme fills up before the shared transfer runs; beta has no quota.
		e.Quota.SetUsed(acme, limit-8)
		e.SetSettings(upstream.Settings{})
		e.Run(20)
		requireQuotaFailure(t, e, a, tiny)
		require.Contains(t, e.Job(a).Error, "987654321", "acme sees its own figures")

		jb := e.Job(b)
		require.Equal(t, jobs.StatusSucceeded, jb.Status, "beta's Pull is not failed by acme's quota: %s", jb.Error)
		require.Empty(t, jb.Error)
		st := pullState(t, jb)
		require.Nil(t, st["reason"])
		f := fileOf(b)
		require.Equal(t, "done", f["status"])
		require.NotEqual(t, shared, f["job"], "beta's Pull queued a transfer of its own")
		require.Nil(t, f["reason"])
		require.Nil(t, f["error"])
		require.NotContains(t, string(jb.Result), "987654321", "beta never sees acme's limit")

		r, err := e.Repos.Get(e.Ctx, twin)
		require.NoError(t, err)
		m, _, err := e.Service.Mirror(e.Ctx, r)
		require.NoError(t, err)
		require.NotEqual(t, upstream.MirrorFailed, m.Status)
		require.Empty(t, m.LastError)

		require.Equal(t, w, readFile(t, e.Repos, twin, twinCommit, "model.safetensors"))
		require.Contains(t, e.Repos.MarkedPresent(twin), sha(w))
		require.NotContains(t, e.Repos.MarkedPresent(tiny), sha(w), "acme does not gain the object over its quota")
		_, err = e.Repos.Entry(e.Ctx, tiny, commit, "model.safetensors")
		require.NoError(t, err)
		for _, fj := range pullFileJobs(t, e) {
			if fj.ID.String() == shared {
				continue
			}
			require.NotContains(t, fj.Error, "987654321", "only acme's own transfer carries its figures")
		}
	})
}

// TestPullQuotaRefusalsOfASharedTransferStayWithEachNamespace fills both
// Namespaces: the joined Pull queues its own transfer once, which its own
// quota refuses, and each Pull reports only its own Namespace's figures.
func TestPullQuotaRefusalsOfASharedTransferStayWithEachNamespace(t *testing.T) {
	forEachDB(t, func(t *testing.T, d *db.DB) {
		e := newEnv(t, d, upstreamtest.HarnessOptions{})
		alice := e.User("alice")
		w := bytes.Repeat([]byte("shared transfer "), 200)
		e.Seed(tiny, nil, map[string][]byte{"model.safetensors": w})
		twin := model.RepoID{Type: model.RepoTypeModel, Namespace: "beta", Name: "twin"}
		e.Seed(twin, nil, map[string][]byte{"model.safetensors": w})
		acme, beta := e.MirrorNamespace("acme"), e.MirrorNamespace("beta")
		const acmeLimit, betaLimit = 111111111, 222222222
		e.Quota.SetLimit(acme, acmeLimit)
		e.Quota.SetLimit(beta, betaLimit)

		e.SetSettings(upstream.Settings{Windows: []string{"01:00-07:00"}, AllowOverrideWindows: true})
		a, _, err := e.Service.StartPull(e.Ctx, upstream.PullRequest{Repo: tiny, Actor: alice})
		require.NoError(t, err)
		b, _, err := e.Service.StartPull(e.Ctx, upstream.PullRequest{Repo: twin, Actor: alice})
		require.NoError(t, err)
		e.Run(1)
		e.Quota.SetUsed(acme, acmeLimit-8)
		e.Quota.SetUsed(beta, betaLimit-8)
		e.SetSettings(upstream.Settings{})
		e.Run(20)

		requireQuotaFailure(t, e, a, tiny)
		requireQuotaFailure(t, e, b, twin)
		require.Contains(t, e.Job(a).Error, "111111111")
		require.NotContains(t, e.Job(a).Error, "222222222")
		require.Contains(t, e.Job(b).Error, "222222222", "beta's Pull reports its own quota")
		require.NotContains(t, e.Job(b).Error, "111111111", "beta never sees acme's limit")
		r, err := e.Repos.Get(e.Ctx, twin)
		require.NoError(t, err)
		m, _, err := e.Service.Mirror(e.Ctx, r)
		require.NoError(t, err)
		require.NotContains(t, m.LastError, "111111111")
		require.Len(t, pullFileJobs(t, e), 2, "beta's Pull queued one transfer of its own, then stopped")
		require.Empty(t, e.Repos.MarkedPresent(tiny))
		require.Empty(t, e.Repos.MarkedPresent(twin))
	})
}

// TestPullQuotaOfAnotherNamespaceCannotStarveAPull keeps a full
// Namespace's Pulls of an object coming while a Pull from a Namespace with
// room waits for the same object. Each acme Pull plans while acme has room
// and its transfer is refused once acme fills up. beta's Pull joined the
// first of those shared transfers; after that refusal it must queue a
// transfer only beta's Pulls can join, instead of joining acme's next one,
// and finish.
func TestPullQuotaOfAnotherNamespaceCannotStarveAPull(t *testing.T) {
	forEachDB(t, func(t *testing.T, d *db.DB) {
		e := newEnv(t, d, upstreamtest.HarnessOptions{})
		alice := e.User("alice")
		w := bytes.Repeat([]byte("contended transfer "), 200)
		twin := model.RepoID{Type: model.RepoTypeModel, Namespace: "beta", Name: "twin"}
		twinCommit := e.Seed(twin, nil, map[string][]byte{"model.safetensors": w})
		acme := e.MirrorNamespace("acme")
		const acmeLimit = 1 << 20
		e.Quota.SetLimit(acme, acmeLimit)
		const rounds = 2
		acmeRepos := make([]model.RepoID, rounds+1)
		for i := range acmeRepos {
			acmeRepos[i] = model.RepoID{Type: model.RepoTypeModel, Namespace: "acme", Name: "copy-" + string(rune('a'+i))}
			e.Seed(acmeRepos[i], nil, map[string][]byte{"model.safetensors": w})
		}
		closed := upstream.Settings{Windows: []string{"01:00-07:00"}, AllowOverrideWindows: true}
		run := func() { e.Queue.RunPending(e.Ctx) }
		// refuseAcme fills acme and opens the window, so the transfer
		// acme's Pull queued runs and is refused.
		refuseAcme := func() {
			e.Quota.SetUsed(acme, acmeLimit-8)
			e.SetSettings(upstream.Settings{})
			e.Advance(10 * time.Minute)
			run()
		}

		e.SetSettings(closed)
		acmePulls := []jobs.JobID{}
		a, _, err := e.Service.StartPull(e.Ctx, upstream.PullRequest{Repo: acmeRepos[0], Actor: alice})
		require.NoError(t, err)
		acmePulls = append(acmePulls, a)
		b, _, err := e.Service.StartPull(e.Ctx, upstream.PullRequest{Repo: twin, Actor: alice})
		require.NoError(t, err)
		run()
		fileJob := func(id jobs.JobID) string {
			fs, _ := pullState(t, e.Job(id))["files"].([]any)
			require.Len(t, fs, 1)
			return fs[0].(map[string]any)["job"].(string)
		}
		require.Equal(t, fileJob(a), fileJob(b), "beta's Pull joined the transfer acme's Pull started")
		refuseAcme()

		for i := 1; i <= rounds; i++ {
			// acme has room again and Pulls the object once more, before
			// beta's Pull sees the refusal.
			e.SetSettings(closed)
			e.Quota.SetUsed(acme, 0)
			a, _, err := e.Service.StartPull(e.Ctx, upstream.PullRequest{Repo: acmeRepos[i], Actor: alice})
			require.NoError(t, err)
			acmePulls = append(acmePulls, a)
			run()
			e.Advance(10 * time.Minute)
			run()
			refuseAcme()
		}

		jb := e.Job(b)
		require.Equal(t, jobs.StatusSucceeded, jb.Status, "acme's refusals do not hold beta's Pull up: %s", jb.Error)
		require.Nil(t, pullState(t, jb)["reason"])
		require.Equal(t, w, readFile(t, e.Repos, twin, twinCommit, "model.safetensors"))
		require.Contains(t, e.Repos.MarkedPresent(twin), sha(w))

		// Once beta holds the object, later acme Pulls find it in the
		// Instance at planning and need no transfer.
		acmeTransfers := map[string]bool{}
		for _, id := range acmePulls {
			fs, _ := pullState(t, e.Job(id))["files"].([]any)
			for _, f := range fs {
				if job, ok := f.(map[string]any)["job"].(string); ok {
					acmeTransfers[job] = true
				}
			}
		}
		var betaTransfers []*jobs.Job
		for _, fj := range pullFileJobs(t, e) {
			if fj.ParentID == b {
				betaTransfers = append(betaTransfers, fj)
			}
		}
		require.Len(t, betaTransfers, 1, "beta's Pull requeued the file once")
		require.False(t, acmeTransfers[betaTransfers[0].ID.String()], "no acme Pull joined beta's own transfer")
		require.Equal(t, jobs.StatusSucceeded, betaTransfers[0].Status)
		require.Equal(t, jobs.StatusFailed, e.Job(acmePulls[0]).Status, "acme's first Pull was refused")
		require.NotContains(t, string(jb.Result), "1048576", "beta never sees acme's limit")
	})
}

// TestPullQuotaRefusalOfAJoinedTransferStandsWithinANamespace covers two
// Pulls into Repos of the same Namespace: the refusal of the transfer
// they share is their own Namespace's, so neither queues another.
func TestPullQuotaRefusalOfAJoinedTransferStandsWithinANamespace(t *testing.T) {
	forEachDB(t, func(t *testing.T, d *db.DB) {
		e := newEnv(t, d, upstreamtest.HarnessOptions{})
		alice := e.User("alice")
		w := bytes.Repeat([]byte("shared transfer "), 200)
		e.Seed(tiny, nil, map[string][]byte{"model.safetensors": w})
		sibling := model.RepoID{Type: model.RepoTypeModel, Namespace: "acme", Name: "sibling"}
		e.Seed(sibling, nil, map[string][]byte{"model.safetensors": w})
		acme := e.MirrorNamespace("acme")
		const limit = 1 << 20
		e.Quota.SetLimit(acme, limit)

		e.SetSettings(upstream.Settings{Windows: []string{"01:00-07:00"}, AllowOverrideWindows: true})
		a, _, err := e.Service.StartPull(e.Ctx, upstream.PullRequest{Repo: tiny, Actor: alice})
		require.NoError(t, err)
		b, _, err := e.Service.StartPull(e.Ctx, upstream.PullRequest{Repo: sibling, Actor: alice})
		require.NoError(t, err)
		e.Run(1)
		e.Quota.SetUsed(acme, limit-8)
		e.SetSettings(upstream.Settings{})
		e.Run(20)

		requireQuotaFailure(t, e, a, tiny)
		requireQuotaFailure(t, e, b, sibling)
		require.Len(t, pullFileJobs(t, e), 1, "a refusal by the Namespace's own quota is not requeued")
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
