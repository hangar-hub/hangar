package scan_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/blob"
	"github.com/hangar-hub/hangar/internal/db/dbtest"
	"github.com/hangar-hub/hangar/internal/hfapi"
	"github.com/hangar-hub/hangar/internal/hfapi/hfapitest"
	"github.com/hangar-hub/hangar/internal/index"
	"github.com/hangar-hub/hangar/internal/index/indextest"
	"github.com/hangar-hub/hangar/internal/jobs"
	"github.com/hangar-hub/hangar/internal/jobs/jobstest"
	"github.com/hangar-hub/hangar/internal/model"
	"github.com/hangar-hub/hangar/internal/repo/repotest"
	"github.com/hangar-hub/hangar/internal/scan"
	"github.com/hangar-hub/hangar/internal/scan/scantest"
)

// brokenFileRepos fails every read of one file while broken is set, as a
// Blob store outage that outlasts a scan Job's retries does.
type brokenFileRepos struct {
	*repotest.Fake
	path   string
	broken atomic.Bool
}

func (b *brokenFileRepos) OpenFile(ctx context.Context, id model.RepoID, commit model.CommitSHA, p string, rng *blob.Range) (io.ReadCloser, model.Entry, error) {
	if p == b.path && b.broken.Load() {
		return nil, model.Entry{}, errors.New("blob store unavailable")
	}
	return b.Fake.OpenFile(ctx, id, commit, p, rng)
}

// TestGateOverHTTPAfterAFailedScanJob checks the gate, directly and through
// the resolve handler, for a scan Job that fails after it has read some
// files: the files it read before the failure get no result, so with
// blocking on every file that can run code stays refused, and
// accept_risk=1 still downloads it. A failed rescan of a scanned Commit
// keeps the results of the scan before it. TestGateAfterAFailedScanJob
// covers the same failure on both drivers and a failure while storing.
func TestGateOverHTTPAfterAFailedScanJob(t *testing.T) {
	d := dbtest.SQLite(t)
	ctx := t.Context()
	fake := repotest.NewFake(t)
	// The scan reads files in path order: the safe checkpoint and the
	// unsafe pickle come before the file whose reads fail.
	repos := &brokenFileRepos{Fake: fake, path: "z-broken.pkl"}
	repos.broken.Store(true)
	id := model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "failed-scan"}
	r := indextest.CreateRepo(t, fake, id, model.VisibilityPublic)
	indextest.InsertRepoRow(t, d, r)
	sha := indextest.Commit(t, fake, id, map[string][]byte{
		"a-safe.ckpt":  scantest.TorchStateDict(),
		"evil.pkl":     scantest.OSSystemProtocol0(),
		"z-broken.pkl": scantest.TorchStateDict(),
		"README.md":    []byte("# hi"),
	})
	oid := func(p string) string {
		e, err := fake.Entry(ctx, id, sha, p)
		require.NoError(t, err)
		require.NotNil(t, e.LFS, p)
		return e.LFS.OID
	}
	safe, evil := oid("a-safe.ckpt"), oid("evil.pkl")

	s := scan.New(scan.Options{DB: d, Repos: repos, LFS: fake.LFS})
	g := s.NewGate(func(context.Context) (bool, error) { return true, nil })
	e := hfapitest.New(t, hfapitest.Options{Repos: repos, LFS: fake.LFS, Deps: func(deps *hfapi.Deps) { deps.Downloads = g }})
	get := func(p string) int {
		t.Helper()
		resp := e.Request(t, http.MethodGet, "/alice/failed-scan/resolve/main/"+p, "", "")
		_ = resp.Body.Close()
		return resp.StatusCode
	}

	// runToFailure runs a scan Job of the Commit through every attempt.
	runToFailure := func() {
		t.Helper()
		reg := jobs.NewRegistry()
		require.NoError(t, s.Register(reg))
		q := jobstest.NewFake(reg)
		clock := time.Now()
		q.SetClock(func() time.Time { return clock })
		job := index.CommitJob{RepoUUID: r.UUID, Repo: id.String(), Type: id.Type, Commit: sha}
		jid, err := q.Enqueue(ctx, jobs.NewJob{Kind: jobs.KindScan, DedupeKey: job.DedupeKey(), Payload: job})
		require.NoError(t, err)
		for range 10 {
			q.RunPending(ctx)
			clock = clock.Add(24 * time.Hour)
		}
		j, err := q.Get(ctx, jid)
		require.NoError(t, err)
		require.Equal(t, jobs.StatusFailed, j.Status, "the Job fails once its retries run out")
		require.Equal(t, 5, j.Attempts)
		require.Contains(t, j.Error, "blob store unavailable")
	}

	// A first scan that fails stores nothing, not the results of the
	// files it read before the failure.
	runToFailure()
	_, err := s.Results(ctx, r.UUID, sha)
	require.ErrorIs(t, err, scan.ErrNotScanned, "a failed scan stores no partial results")
	for _, p := range []string{"a-safe.ckpt", "evil.pkl", "z-broken.pkl"} {
		require.Equal(t, scan.BlockNotScanned, blockedReason(t, g.CheckFile(ctx, r.UUID, id, sha, p)), p)
	}
	require.NoError(t, g.CheckFile(ctx, r.UUID, id, sha, "README.md"))
	require.Equal(t, scan.BlockNotScanned, blockedReason(t, g.CheckTree(ctx, r.UUID, id, sha, "")))
	blocked, err := g.CheckObjects(ctx, r.UUID, id, []string{safe, evil})
	require.NoError(t, err)
	require.Equal(t, scan.BlockNotScanned, blockedReason(t, blocked[safe]))
	require.Equal(t, scan.BlockNotScanned, blockedReason(t, blocked[evil]))
	require.Equal(t, http.StatusForbidden, get("a-safe.ckpt"), "a file the failed Job found safe is still not scanned")
	require.Equal(t, http.StatusForbidden, get("evil.pkl"))
	require.Equal(t, http.StatusOK, get("evil.pkl?accept_risk=1"), "accept_risk=1 downloads it anyway")
	require.Equal(t, http.StatusOK, get("README.md"))

	// Once a scan completes, the gate takes its results.
	repos.broken.Store(false)
	_, err = s.ScanCommit(ctx, r.UUID, id, sha)
	require.NoError(t, err)
	require.NoError(t, g.CheckFile(ctx, r.UUID, id, sha, "a-safe.ckpt"))
	require.Equal(t, scan.BlockUnsafe, blockedReason(t, g.CheckFile(ctx, r.UUID, id, sha, "evil.pkl")))

	// A rescan that fails keeps them: it neither clears the Commit's
	// results nor replaces some of them.
	repos.broken.Store(true)
	runToFailure()
	res, err := s.Results(ctx, r.UUID, sha)
	require.NoError(t, err, "a failed rescan keeps the earlier results")
	require.Equal(t, scan.StatusUnsafe, res.Status)
	require.Len(t, res.Files, 3)
	require.NoError(t, g.CheckFile(ctx, r.UUID, id, sha, "a-safe.ckpt"))
	require.Equal(t, scan.BlockUnsafe, blockedReason(t, g.CheckFile(ctx, r.UUID, id, sha, "evil.pkl")))
	blocked, err = g.CheckObjects(ctx, r.UUID, id, []string{safe, evil})
	require.NoError(t, err)
	require.NotContains(t, blocked, safe)
	require.Equal(t, scan.BlockUnsafe, blockedReason(t, blocked[evil]))
	require.Equal(t, http.StatusOK, get("a-safe.ckpt"))
	require.Equal(t, http.StatusForbidden, get("evil.pkl"))
	require.Equal(t, http.StatusOK, get("evil.pkl?accept_risk=1"), "accept_risk=1 downloads it anyway")
}
