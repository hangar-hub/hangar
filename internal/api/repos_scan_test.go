package api_test

import (
	"bytes"
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/api"
	"github.com/jamesagarside/hangar/internal/api/gen"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/repo"
	"github.com/jamesagarside/hangar/internal/scan"
	"github.com/jamesagarside/hangar/internal/scan/scantest"
)

func scanStatusOf(t *testing.T, l gen.TreeListing, p string) string {
	t.Helper()
	for _, e := range l.Items {
		if e.Path == p {
			if e.ScanStatus == nil {
				return ""
			}
			return string(*e.ScanStatus)
		}
	}
	t.Fatalf("%s is not in the listing", p)
	return ""
}

// TestTreeScanStatusWhileScanIsPending checks the tree listing between a
// new Commit and the end of its scan Job: an unchanged pickle keeps its
// result, so the file page shows the scan panel and the risky download
// that the download gate expects, and a changed pickle is pending.
func TestTreeScanStatusWhileScanIsPending(t *testing.T) {
	f := newFixture(t, func(d *api.RepoDeps) {
		d.Downloads = &scan.Gate{Block: func(context.Context) (bool, error) { return true, nil }, Results: d.Scanner.Results, Repos: d.Repos}
	})
	alice := f.user("alice")
	id := model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "pending"}
	r, err := f.real.Create(f.ctx, repo.CreateParams{ID: id, Actor: alice, Visibility: model.VisibilityPublic})
	require.NoError(t, err)
	first, err := f.real.Commit(f.ctx, repo.CommitParams{Repo: id, Revision: "main", Summary: "Add pickles", Actor: alice, Operations: []repo.Operation{
		{Kind: repo.OpAdd, Path: "evil.pkl", Content: bytes.NewReader(scantest.OSSystemProtocol0())},
		{Kind: repo.OpAdd, Path: "model.ckpt", Content: bytes.NewReader(scantest.TorchStateDict())},
	}})
	require.NoError(t, err)
	base := "/repos/models/alice/pending/tree/main"

	// Before any scan, the pickles are pending and nothing else has a status.
	var l gen.TreeListing
	f.expect(http.StatusOK, http.MethodGet, "alice", base, nil).json(t, &l)
	require.Equal(t, "pending", scanStatusOf(t, l, "evil.pkl"))
	require.Equal(t, "pending", scanStatusOf(t, l, "model.ckpt"))

	f.scans.SetResult("evil.pkl", scan.StatusUnsafe, scan.Finding{Module: "os", Name: "system", Status: scan.StatusUnsafe})
	_, err = f.scans.ScanCommit(f.ctx, r.UUID, id, first.SHA)
	require.NoError(t, err)

	// A new Commit lands; its scan Job has not run yet.
	next, err := f.real.Commit(f.ctx, repo.CommitParams{Repo: id, Revision: "main", Summary: "Update", Actor: alice, Operations: []repo.Operation{
		{Kind: repo.OpAdd, Path: "model.ckpt", Content: bytes.NewReader(scantest.SklearnSuspicious())},
		{Kind: repo.OpAdd, Path: "notes.md", Content: bytes.NewReader([]byte("notes"))},
	}})
	require.NoError(t, err)
	_, err = f.scans.Results(f.ctx, r.UUID, next.SHA)
	require.ErrorIs(t, err, scan.ErrNotScanned)

	l = gen.TreeListing{}
	f.expect(http.StatusOK, http.MethodGet, "alice", base, nil).json(t, &l)
	require.Equal(t, string(next.SHA), l.Commit)
	require.Equal(t, "unsafe", scanStatusOf(t, l, "evil.pkl"), "an unchanged pickle keeps its result")
	require.Equal(t, "pending", scanStatusOf(t, l, "model.ckpt"), "a changed pickle is pending")
	require.Empty(t, scanStatusOf(t, l, "notes.md"), "the scan does not read notes.md")

	// The file page reads the single-file listing and the file's findings.
	l = gen.TreeListing{}
	f.expect(http.StatusOK, http.MethodGet, "alice", base+"/evil.pkl", nil).json(t, &l)
	require.Equal(t, "unsafe", scanStatusOf(t, l, "evil.pkl"))

	// The gate refuses the file as unsafe, not as unscanned, so the
	// download the page offers with accept_risk=1 is the one that works.
	body := f.expect(http.StatusForbidden, http.MethodPost, "alice", "/repos/models/alice/pending/zip", nil).body
	require.Contains(t, string(body), "is marked unsafe")

	// Once the scan Job runs, the Commit's own results apply.
	_, err = f.scans.ScanCommit(f.ctx, r.UUID, id, next.SHA)
	require.NoError(t, err)
	l = gen.TreeListing{}
	f.expect(http.StatusOK, http.MethodGet, "alice", base, nil).json(t, &l)
	require.Equal(t, "unsafe", scanStatusOf(t, l, "evil.pkl"))
	require.Equal(t, "safe", scanStatusOf(t, l, "model.ckpt"))
}
