package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/auth"
	"github.com/hangar-hub/hangar/internal/config"
	"github.com/hangar-hub/hangar/internal/index"
	"github.com/hangar-hub/hangar/internal/jobs"
	"github.com/hangar-hub/hangar/internal/model"
	"github.com/hangar-hub/hangar/internal/repo"
	"github.com/hangar-hub/hangar/internal/scan"
	"github.com/hangar-hub/hangar/internal/scan/scantest"
)

// TestUnsafeDownloadGateAfterAFailedScanJob checks the Instance end to end
// when the scan Job of a pickle fails: with scan.block_unsafe_downloads on,
// the pickle is still refused as not scanned, and accept_risk=1 still
// downloads it.
func TestUnsafeDownloadGateAfterAFailedScanJob(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.NotFoundHandler())
	pub := "http://" + srv.Listener.Addr().String()
	in := newTestInstance(t, func(c *config.Config) {
		c.Server.PublicURL = pub
		c.Security.AnonymousRead = true
	})
	ctx := context.Background()
	h, err := newHandler(ctx, in, newHealth(in.db, in.blobs), false)
	require.NoError(t, err)
	srv.Config.Handler = h
	srv.Start()
	t.Cleanup(srv.Close)

	on := true
	_, err = in.auth.Settings.Update(ctx, auth.SettingsPatch{ScanBlockUnsafeDownloads: &on}, model.System())
	require.NoError(t, err)
	// The database refuses the pickle's scan result, so every scan Job of
	// the Commit fails while it stores its results.
	require.True(t, in.db.IsSQLite(), "the test Instance runs on SQLite")
	_, err = in.db.ExecContext(ctx, `CREATE TRIGGER test_fail_scan_store BEFORE INSERT ON scan_results
		WHEN NEW.path = 'evil.pkl' BEGIN SELECT RAISE(ABORT, 'injected scan store failure'); END`)
	require.NoError(t, err)

	p := testUser(t, in, "alice")
	id := model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "failed-scan"}
	_, err = in.repos.Create(ctx, repo.CreateParams{ID: id, Actor: p, InitialReadme: true, Visibility: model.VisibilityPublic})
	require.NoError(t, err)
	pickle := scantest.OSSystemProtocol0()
	_, err = in.repos.Commit(ctx, repo.CommitParams{Repo: id, Revision: "main", Summary: "Add a pickle", Actor: p,
		Operations: []repo.Operation{{Kind: repo.OpAdd, Path: "evil.pkl", Content: bytes.NewReader(pickle)}}})
	require.NoError(t, err)
	r, err := in.repos.Get(ctx, id)
	require.NoError(t, err)
	head, err := in.repos.Resolve(ctx, id, "")
	require.NoError(t, err)

	// A scan Job of the head with one attempt, so it fails for good instead
	// of waiting for a retry.
	job := index.CommitJob{RepoUUID: r.UUID, Repo: id.String(), Type: id.Type, Commit: head}
	jid, err := in.queue.Enqueue(ctx, jobs.NewJob{Kind: jobs.KindScan, DedupeKey: job.DedupeKey() + ":once", Payload: job,
		MaxAttempts: 1, Repo: r.UUID})
	require.NoError(t, err)
	w := in.newWorker()
	deadline := time.Now().Add(2 * time.Minute)
	for {
		j, err := in.queue.Get(ctx, jid)
		require.NoError(t, err)
		if j.Status == jobs.StatusFailed {
			require.Contains(t, j.Error, "injected scan store failure")
			break
		}
		require.NotEqual(t, jobs.StatusSucceeded, j.Status, "the scan Job must fail")
		require.True(t, time.Now().Before(deadline), "the scan Job is still %s", j.Status)
		w.Tick(ctx)
		w.Wait()
	}
	_, err = in.scanner.Results(ctx, r.UUID, head)
	require.ErrorIs(t, err, scan.ErrNotScanned, "the failed Job stored nothing")

	get := func(path string) (*http.Response, []byte) {
		t.Helper()
		resp, err := http.Get(pub + "/alice/failed-scan/resolve/main/" + path)
		require.NoError(t, err)
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		return resp, body
	}
	resp, _ := get("evil.pkl")
	require.Equal(t, http.StatusForbidden, resp.StatusCode, "a pickle whose scan failed does not download")
	require.Contains(t, resp.Header.Get("X-Error-Message"), "evil.pkl has not been scanned")
	require.Contains(t, resp.Header.Get("X-Error-Message"), "accept_risk=1")
	resp, body := get("evil.pkl?accept_risk=1")
	require.Equal(t, http.StatusOK, resp.StatusCode, "accept_risk=1 downloads it anyway")
	require.Equal(t, pickle, body)
	resp, _ = get("README.md")
	require.Equal(t, http.StatusOK, resp.StatusCode, "files that cannot run code still download")
}
