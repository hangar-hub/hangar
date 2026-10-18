package gitserver_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/gitserver/gitservertest"
	"github.com/hangar-hub/hangar/internal/model"
	"github.com/hangar-hub/hangar/internal/repo"
	"github.com/hangar-hub/hangar/internal/scan"
	"github.com/hangar-hub/hangar/internal/scan/scantest"
)

// TestLFSBlocksUnsafeDownloads checks that git-lfs follows
// scan.block_unsafe_downloads like the Hub API: no download action for an
// object the scan marked unsafe, and signed links refused once blocking is
// on.
func TestLFSBlocksUnsafeDownloads(t *testing.T) {
	t.Parallel()
	block := false
	var scanner *scantest.Fake
	f := newFixture(t, func(o *gitservertest.Options, f *fixture) {
		scanner = scantest.NewFake(f.repos)
		o.Downloads = &scan.Gate{Block: func(context.Context) (bool, error) { return block, nil }, Results: scanner.Results, Repos: f.repos}
	})
	ctx := context.Background()
	content := scantest.OSSystemProtocol0()
	stored, err := f.repos.LFS.Put(ctx, content)
	require.NoError(t, err)
	res, err := f.repos.Commit(ctx, repo.CommitParams{Repo: publicID, Revision: model.DefaultBranch, Summary: "Add a pickle", Actor: alice,
		Operations: []repo.Operation{{Kind: repo.OpAddLFS, Path: "evil.pkl", OID: stored.OID, Size: stored.Size}}})
	require.NoError(t, err)
	r, err := f.repos.Get(ctx, publicID)
	require.NoError(t, err)
	scanner.SetResult("evil.pkl", scan.StatusUnsafe, scan.Finding{Module: "os", Name: "system", Status: scan.StatusUnsafe})

	req := batchReq("download", nil, obj(stored.OID, int(stored.Size)))
	_, b := f.batch(t, publicID, tokAlice, req)
	link := b.Objects[0].Actions["download"]
	require.NotNil(t, link, "the setting is off by default")

	block = true
	_, b = f.batch(t, publicID, tokAlice, req)
	require.Nil(t, b.Objects[0].Actions)
	require.Equal(t, http.StatusForbidden, b.Objects[0].Error.Code, "an object of an unscanned head that can run code fails closed")
	require.Contains(t, b.Objects[0].Error.Message, "has not been scanned")

	_, err = scanner.ScanCommit(ctx, r.UUID, publicID, res.SHA)
	require.NoError(t, err)
	_, b = f.batch(t, publicID, tokAlice, req)
	require.Equal(t, http.StatusForbidden, b.Objects[0].Error.Code)
	require.Contains(t, b.Objects[0].Error.Message, "evil.pkl is marked unsafe")
	resp := call(t, http.MethodGet, link, nil)
	require.Equal(t, http.StatusForbidden, resp.StatusCode, "a link issued before blocking was turned on is refused")
	require.Contains(t, readAll(t, resp.Body), "marked unsafe")

	scanner.SetResult("evil.pkl", scan.StatusSafe)
	_, err = scanner.ScanCommit(ctx, r.UUID, publicID, res.SHA)
	require.NoError(t, err)
	_, b = f.batch(t, publicID, tokAlice, req)
	require.NotNil(t, b.Objects[0].Actions["download"], "a file the scan finds safe downloads")
	require.Equal(t, string(content), readAll(t, call(t, http.MethodGet, link, nil).Body))
}
