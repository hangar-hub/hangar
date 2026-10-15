package scan_test

import (
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/blob"
	"github.com/jamesagarside/hangar/internal/db"
	"github.com/jamesagarside/hangar/internal/db/dbtest"
	"github.com/jamesagarside/hangar/internal/index"
	"github.com/jamesagarside/hangar/internal/index/indextest"
	"github.com/jamesagarside/hangar/internal/jobs"
	"github.com/jamesagarside/hangar/internal/jobs/jobstest"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/repo"
	"github.com/jamesagarside/hangar/internal/repo/repotest"
	"github.com/jamesagarside/hangar/internal/scan"
	"github.com/jamesagarside/hangar/internal/scan/scantest"
	"github.com/jamesagarside/hangar/internal/webhook"
	"github.com/jamesagarside/hangar/internal/webhook/webhooktest"
)

// unreadableRepos fails every read of one path while broken is set, as a
// Blob store outage does part-way through a scan.
type unreadableRepos struct {
	*repotest.Fake
	path   string
	broken atomic.Bool
}

func (u *unreadableRepos) OpenFile(ctx context.Context, id model.RepoID, commit model.CommitSHA, p string, rng *blob.Range) (io.ReadCloser, model.Entry, error) {
	if p == u.path && u.broken.Load() {
		return nil, model.Entry{}, errors.New("blob store unavailable")
	}
	return u.Fake.OpenFile(ctx, id, commit, p, rng)
}

// failStoreOf makes the database refuse to store a scan result for path,
// so a scan Job fails while it writes its results.
func failStoreOf(t *testing.T, d *db.DB, path string) {
	t.Helper()
	ctx := t.Context()
	stmts := []string{
		`CREATE TRIGGER test_fail_scan_store BEFORE INSERT ON scan_results
		 WHEN NEW.path = '` + path + `' BEGIN SELECT RAISE(ABORT, 'injected scan store failure'); END`,
	}
	if !d.IsSQLite() {
		stmts = []string{
			`CREATE FUNCTION test_fail_scan_store() RETURNS trigger LANGUAGE plpgsql AS
			 $$ BEGIN RAISE EXCEPTION 'injected scan store failure'; END $$`,
			`CREATE TRIGGER test_fail_scan_store BEFORE INSERT ON scan_results FOR EACH ROW
			 WHEN (NEW.path = '` + path + `') EXECUTE FUNCTION test_fail_scan_store()`,
		}
	}
	for _, s := range stmts {
		_, err := d.ExecContext(ctx, s)
		require.NoError(t, err)
	}
}

// runScanJob enqueues a scan Job for commit and runs it, moving the clock
// past each retry's backoff, until it finishes. It returns the Job.
func runScanJob(t *testing.T, q *jobstest.Fake, now *time.Time, r *repo.Repo, commit model.CommitSHA) *jobs.Job {
	t.Helper()
	ctx := t.Context()
	job := index.CommitJob{RepoUUID: r.UUID, Repo: r.ID.String(), Type: r.ID.Type, Commit: commit}
	jid, err := q.Enqueue(ctx, jobs.NewJob{Kind: jobs.KindScan, DedupeKey: job.DedupeKey(), Payload: job, Repo: r.UUID})
	require.NoError(t, err)
	for range 10 {
		q.RunPending(ctx)
		j, err := q.Get(ctx, jid)
		require.NoError(t, err)
		if j.Status == jobs.StatusSucceeded || j.Status == jobs.StatusFailed {
			return j
		}
		*now = now.Add(2 * time.Hour)
	}
	t.Fatalf("scan Job %s did not finish", jid)
	return nil
}

// TestGateAfterAFailedScanJob checks the unsafe-download gate when a scan
// Job fails, while it reads the files or while it stores the results. A
// failed Job stores nothing, so every file that can run code still fails
// closed, even the ones the Job read before it failed; a failed rescan
// keeps the results stored before it. Without both, a partial result would
// let through a pickle the scan never read, or drop the result that marks
// a pickle unsafe.
func TestGateAfterAFailedScanJob(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		ctx := t.Context()
		fake := repotest.NewFake(t)
		id := model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "failed-scan"}
		r := indextest.CreateRepo(t, fake, id, model.VisibilityPublic)
		indextest.InsertRepoRow(t, d, r)
		// The tree lists a.ckpt and evil.pkl before z.npy, so the Job reads
		// both pickles before it fails on z.npy.
		sha := indextest.Commit(t, fake, id, map[string][]byte{
			"a.ckpt":    scantest.TorchStateDict(),
			"evil.pkl":  scantest.OSSystemProtocol0(),
			"z.npy":     scantest.NumpyFloatArray(),
			"README.md": []byte("# hi"),
		})
		oid := func(commit model.CommitSHA, p string) string {
			e, err := fake.Entry(ctx, id, commit, p)
			require.NoError(t, err)
			require.NotNil(t, e.LFS, p)
			return e.LFS.OID
		}
		ckpt, evil, npy := oid(sha, "a.ckpt"), oid(sha, "evil.pkl"), oid(sha, "z.npy")

		repos := &unreadableRepos{Fake: fake, path: "z.npy"}
		hooks := &webhooktest.Fake{}
		s := scan.New(scan.Options{DB: d, Repos: repos, LFS: fake.LFS, Webhooks: hooks})
		g := s.NewGate(func(context.Context) (bool, error) { return true, nil })
		reg := jobs.NewRegistry()
		require.NoError(t, s.Register(reg))
		q := jobstest.NewFake(reg)
		now := time.Now()
		q.SetClock(func() time.Time { return now })

		// The Job fails while it reads the files, on every attempt.
		repos.broken.Store(true)
		j := runScanJob(t, q, &now, r, sha)
		require.Equal(t, jobs.StatusFailed, j.Status)
		require.Equal(t, 5, j.Attempts, "the Job used every attempt")
		require.Contains(t, j.Error, "z.npy")
		require.Empty(t, hooks.Names(), "no scan.completed for a failed scan")

		_, err := s.Results(ctx, r.UUID, sha)
		require.ErrorIs(t, err, scan.ErrNotScanned, "a failed Job stores no partial results")
		_, err = s.FileResult(ctx, r.UUID, sha, "a.ckpt")
		require.ErrorIs(t, err, scan.ErrNotScanned)
		for _, p := range []string{"evil.pkl", "a.ckpt", "z.npy"} {
			require.Equal(t, scan.BlockNotScanned, blockedReason(t, g.CheckFile(ctx, r.UUID, id, sha, p)),
				"%s can run code and has no result after the failed Job", p)
		}
		require.NoError(t, g.CheckFile(ctx, r.UUID, id, sha, "README.md"), "files that cannot run code still download")
		require.Equal(t, scan.BlockNotScanned, blockedReason(t, g.CheckTree(ctx, r.UUID, id, sha, "")))
		blocked, err := g.CheckObjects(ctx, r.UUID, id, []string{ckpt, evil, npy})
		require.NoError(t, err)
		require.Len(t, blocked, 3)
		for _, o := range []string{ckpt, evil, npy} {
			require.Equal(t, scan.BlockNotScanned, blockedReason(t, blocked[o]))
		}

		// Once the store is back, a new Job scans the Commit and only the
		// unsafe pickle is refused.
		repos.broken.Store(false)
		j = runScanJob(t, q, &now, r, sha)
		require.Equal(t, jobs.StatusSucceeded, j.Status, j.Error)
		require.Equal(t, scan.BlockUnsafe, blockedReason(t, g.CheckFile(ctx, r.UUID, id, sha, "evil.pkl")))
		require.NoError(t, g.CheckFile(ctx, r.UUID, id, sha, "a.ckpt"))
		require.NoError(t, g.CheckFile(ctx, r.UUID, id, sha, "z.npy"))
		before, err := s.Results(ctx, r.UUID, sha)
		require.NoError(t, err)

		// A rescan that fails while it stores the results keeps the results
		// it was replacing: the unsafe pickle stays refused as unsafe and the
		// safe ones still download.
		failStoreOf(t, d, "z.npy")
		j = runScanJob(t, q, &now, r, sha)
		require.Equal(t, jobs.StatusFailed, j.Status)
		require.Contains(t, j.Error, "injected scan store failure")
		after, err := s.Results(ctx, r.UUID, sha)
		require.NoError(t, err)
		require.Equal(t, before, after, "a failed rescan leaves the stored results as they were")
		require.Equal(t, scan.BlockUnsafe, blockedReason(t, g.CheckFile(ctx, r.UUID, id, sha, "evil.pkl")))
		require.NoError(t, g.CheckFile(ctx, r.UUID, id, sha, "a.ckpt"))
		blocked, err = g.CheckObjects(ctx, r.UUID, id, []string{ckpt, evil, npy})
		require.NoError(t, err)
		require.Len(t, blocked, 1)
		require.Equal(t, scan.BlockUnsafe, blockedReason(t, blocked[evil]))

		// The first scan of a new Commit that fails while it stores the
		// results stores none of them, not even the Commit's own row.
		next := indextest.Commit(t, fake, id, map[string][]byte{"b.pkl": scantest.SubprocessInst()})
		j = runScanJob(t, q, &now, r, next)
		require.Equal(t, jobs.StatusFailed, j.Status)
		_, err = s.Results(ctx, r.UUID, next)
		require.ErrorIs(t, err, scan.ErrNotScanned)
		require.Equal(t, scan.BlockNotScanned, blockedReason(t, g.CheckFile(ctx, r.UUID, id, next, "b.pkl")))
		require.Equal(t, scan.BlockUnsafe, blockedReason(t, g.CheckFile(ctx, r.UUID, id, next, "evil.pkl")),
			"an unchanged file keeps the result of the scanned parent")
		require.Equal(t, []webhook.EventName{webhook.ScanCompleted}, hooks.Names(), "only the scan that succeeded emits scan.completed")
	})
}
