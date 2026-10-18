package upstream_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/db"
	"github.com/hangar-hub/hangar/internal/jobs"
	"github.com/hangar-hub/hangar/internal/model"
	"github.com/hangar-hub/hangar/internal/scan"
	"github.com/hangar-hub/hangar/internal/upstream"
	"github.com/hangar-hub/hangar/internal/upstream/upstreamtest"
)

// TestPullThroughURLChecksUnsafeDownloads checks the signed Pull-through
// URL is refused while scan.block_unsafe_downloads blocks the object, even
// when the link was issued before blocking was turned on, as git-lfs
// signed links are.
func TestPullThroughURLChecksUnsafeDownloads(t *testing.T) {
	forEachDB(t, func(t *testing.T, d *db.DB) {
		var block atomic.Bool
		gate := &scan.Gate{
			Block: func(context.Context) (bool, error) { return block.Load(), nil },
			Results: func(context.Context, uuid.UUID, model.CommitSHA) (*scan.CommitResult, error) {
				return nil, scan.ErrNotScanned
			},
		}
		e := newEnv(t, d, upstreamtest.HarnessOptions{Downloads: gate})
		gate.Repos = e.Repos
		alice := e.User("alice")
		w := []byte("a pickle that has not been scanned")
		e.Seed(tiny, nil, map[string][]byte{"model.pkl": w})
		id, _, err := e.Service.StartPull(e.Ctx, upstream.PullRequest{Repo: tiny, Actor: alice, Selection: upstream.Selection{None: true}})
		require.NoError(t, err)
		e.Run(5)
		require.Equal(t, jobs.StatusSucceeded, e.Job(id).Status, e.Job(id).Error)
		r, err := e.Repos.Get(e.Ctx, tiny)
		require.NoError(t, err)
		e.SetSettings(upstream.Settings{PullThroughEnabled: true, PullThroughAllow: []string{"*"}, PullThroughMinRole: "user"})
		pt := e.Service.PullThrough()
		href, ok, err := pt.DownloadURL(e.Ctx, alice, r, sha(w), int64(len(w)))
		require.NoError(t, err)
		require.True(t, ok)

		srv := httptest.NewServer(pt.Handler())
		defer srv.Close()
		u, err := url.Parse(href)
		require.NoError(t, err)
		get := func() (int, string) {
			t.Helper()
			resp, err := http.Get(srv.URL + u.RequestURI())
			require.NoError(t, err)
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			return resp.StatusCode, string(body)
		}

		block.Store(true)
		cdn := e.Server.Requests("cdn")
		code, body := get()
		require.Equal(t, http.StatusForbidden, code, body)
		require.Contains(t, body, "model.pkl has not been scanned")
		require.Equal(t, cdn, e.Server.Requests("cdn"), "a refused download never reaches the Upstream")

		block.Store(false)
		code, body = get()
		require.Equal(t, http.StatusOK, code, body)
		require.Equal(t, string(w), body)
	})
}
