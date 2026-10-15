package api_test

import (
	"bytes"
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/api"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/repo"
	"github.com/jamesagarside/hangar/internal/scan"
	"github.com/jamesagarside/hangar/internal/scan/scantest"
)

// TestZipExportBlocksUnsafeDownloads checks that a zip export cannot carry
// a file that scan.block_unsafe_downloads refuses.
func TestZipExportBlocksUnsafeDownloads(t *testing.T) {
	block := true
	f := newFixture(t, func(d *api.RepoDeps) {
		d.Downloads = &scan.Gate{Block: func(context.Context) (bool, error) { return block, nil }, Results: d.Scanner.Results, Repos: d.Repos}
	})
	alice := f.user("alice")
	id := model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "m"}
	r, err := f.real.Create(f.ctx, repo.CreateParams{ID: id, Actor: alice, InitialReadme: true, Visibility: model.VisibilityPublic})
	require.NoError(t, err)
	res, err := f.real.Commit(f.ctx, repo.CommitParams{Repo: id, Revision: "main", Summary: "Add files", Actor: alice, Operations: []repo.Operation{
		{Kind: repo.OpAdd, Path: "evil.pkl", Content: bytes.NewReader(scantest.OSSystemProtocol0())},
		{Kind: repo.OpAdd, Path: "docs/notes.md", Content: bytes.NewReader([]byte("notes"))},
	}})
	require.NoError(t, err)
	base := "/repos/models/alice/m/zip"

	body := f.expect(http.StatusForbidden, http.MethodPost, "alice", base, nil).body
	require.Contains(t, string(body), "evil.pkl has not been scanned")
	f.scans.SetResult("evil.pkl", scan.StatusUnsafe, scan.Finding{Module: "os", Name: "system", Status: scan.StatusUnsafe})
	_, err = f.scans.ScanCommit(f.ctx, r.UUID, id, res.SHA)
	require.NoError(t, err)
	body = f.expect(http.StatusForbidden, http.MethodPost, "alice", base, nil).body
	require.Contains(t, string(body), "evil.pkl is marked unsafe")
	require.Contains(t, string(body), "cannot include it")
	f.expect(http.StatusAccepted, http.MethodPost, "alice", base+"?path=docs", nil)
	block = false
	f.expect(http.StatusAccepted, http.MethodPost, "alice", base, nil)
}
