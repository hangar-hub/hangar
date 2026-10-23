package upstream_test

import (
	"bytes"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/db"
	"github.com/hangar-hub/hangar/internal/jobs"
	"github.com/hangar-hub/hangar/internal/model"
	"github.com/hangar-hub/hangar/internal/upstream"
	"github.com/hangar-hub/hangar/internal/upstream/upstreamtest"
)

// sharedPulls starts two Pulls of the same object, into two Namespaces,
// while the pull window is closed, and returns them with the transfer
// they share, which the first Pull started.
func sharedPulls(t *testing.T, e *upstreamtest.Harness, w []byte) (a, b jobs.JobID, twin model.RepoID, twinCommit model.CommitSHA, shared string) {
	t.Helper()
	alice := e.User("alice")
	e.Seed(tiny, nil, map[string][]byte{"model.safetensors": w})
	twin = model.RepoID{Type: model.RepoTypeModel, Namespace: "beta", Name: "twin"}
	twinCommit = e.Seed(twin, nil, map[string][]byte{"model.safetensors": w})
	e.SetSettings(upstream.Settings{Windows: []string{"01:00-07:00"}, AllowOverrideWindows: true})
	a, _, err := e.Service.StartPull(e.Ctx, upstream.PullRequest{Repo: tiny, Actor: alice})
	require.NoError(t, err)
	b, _, err = e.Service.StartPull(e.Ctx, upstream.PullRequest{Repo: twin, Actor: alice})
	require.NoError(t, err)
	e.Run(1)
	shared, _ = pullFileOf(t, e, a)["job"].(string)
	require.NotEmpty(t, shared)
	require.Equal(t, shared, pullFileOf(t, e, b)["job"], "both Pulls share one transfer")
	return a, b, twin, twinCommit, shared
}

func pullFileOf(t *testing.T, e *upstreamtest.Harness, id jobs.JobID) map[string]any {
	t.Helper()
	fs, _ := pullState(t, e.Job(id))["files"].([]any)
	require.Len(t, fs, 1)
	return fs[0].(map[string]any)
}

// TestCancellingAPullKeepsTransfersOthersJoined covers a Pull that joined
// a transfer another Pull started: cancelling the other Pull cancels the
// shared transfer with it, and the joined Pull queues it again instead of
// ending partial.
func TestCancellingAPullKeepsTransfersOthersJoined(t *testing.T) {
	forEachDB(t, func(t *testing.T, d *db.DB) {
		e := newEnv(t, d, upstreamtest.HarnessOptions{})
		w := bytes.Repeat([]byte("shared transfer "), 200)
		a, b, twin, twinCommit, shared := sharedPulls(t, e, w)

		require.NoError(t, e.Queue.Cancel(e.Ctx, a))
		require.Equal(t, jobs.StatusCancelled, e.Job(uuid.MustParse(shared)).Status, "the shared transfer is cancelled with its Pull")
		e.SetSettings(upstream.Settings{})
		e.Run(20)

		jb := e.Job(b)
		require.Equal(t, jobs.StatusSucceeded, jb.Status, jb.Error)
		f := pullFileOf(t, e, b)
		require.Equal(t, "done", f["status"])
		require.NotEqual(t, shared, f["job"], "the joined Pull queued the transfer again")
		require.Equal(t, w, readFile(t, e.Repos, twin, twinCommit, "model.safetensors"))
		require.Contains(t, e.Repos.MarkedPresent(twin), sha(w))
		require.Equal(t, jobs.StatusCancelled, e.Job(a).Status)
	})
}

// TestCancellingASharedTransferItselfCancelsTheFile covers a transfer
// cancelled on its own: every Pull that waits for it counts the file as
// cancelled, as before.
func TestCancellingASharedTransferItselfCancelsTheFile(t *testing.T) {
	forEachDB(t, func(t *testing.T, d *db.DB) {
		e := newEnv(t, d, upstreamtest.HarnessOptions{})
		w := bytes.Repeat([]byte("shared transfer "), 200)
		a, b, _, _, shared := sharedPulls(t, e, w)

		require.NoError(t, e.Queue.Cancel(e.Ctx, uuid.MustParse(shared)))
		e.SetSettings(upstream.Settings{})
		e.Run(20)
		for _, id := range []jobs.JobID{a, b} {
			require.Equal(t, jobs.StatusFailed, e.Job(id).Status)
			f := pullFileOf(t, e, id)
			require.Equal(t, "cancelled", f["status"])
			require.Equal(t, shared, f["job"])
		}
	})
}
