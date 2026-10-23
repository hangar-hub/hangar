package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/auth"
	"github.com/hangar-hub/hangar/internal/community"
	"github.com/hangar-hub/hangar/internal/db"
	"github.com/hangar-hub/hangar/internal/hfapi"
	"github.com/hangar-hub/hangar/internal/index"
	"github.com/hangar-hub/hangar/internal/model"
	"github.com/hangar-hub/hangar/internal/repo"
)

// Services that hide the methods that read many Repos at once, so the Hub
// API reads each Repo's data on its own, as it did before pages were read
// in batches.
type (
	oneRepoAtATime  struct{ repo.Service }
	oneIndexAtATime struct{ index.Indexer }
	oneCountAtATime struct{ community.Service }
)

// Reading a page's data in batches changes nothing in the answers: each
// Arsenal list item is the Repo as GET /repos/{type}/{ns}/{name} gives it,
// permissions included, and every /api/models and /api/datasets answer is
// byte for byte the one the Hub API gives when it reads each Repo on its
// own.
func TestListingPagesMatchReadingEachRepo(t *testing.T) {
	ctx := context.Background()
	in := newTestInstance(t, nil)
	viewer := testUser(t, in, "viewer")
	_, token, err := in.auth.TokenStore.Create(ctx, auth.NewToken{UserID: viewer.UserID, Name: "t", Scope: model.ScopeWrite})
	require.NoError(t, err)
	seedListing(t, in.db, viewer, 150)

	// Some Repos have no scan results, some datasets have converted
	// Splits, and the viewer collaborates on some private Repos.
	var repos []db.Repo
	require.NoError(t, in.db.NewSelect().Model(&repos).Order("id").Scan(ctx))
	for i, r := range repos {
		switch {
		case i%7 == 0:
			_, err := in.db.NewDelete().Model((*db.ScanResult)(nil)).Where("repo_id = ?", r.ID).Exec(ctx)
			require.NoError(t, err)
		case r.Type == "dataset" && i%2 == 0:
			var ref db.Ref
			require.NoError(t, in.db.NewSelect().Model(&ref).Where("repo_id = ?", r.ID).Scan(ctx))
			rows := int64(1000 * (i + 1))
			_, err := in.db.NewInsert().Model(&db.DatasetSplit{RepoID: r.ID, Commit: ref.Target, Config: "default", Split: "train",
				NumRows: &rows, Features: db.JSON("{}"), Status: "ready"}).Exec(ctx)
			require.NoError(t, err)
		case r.Visibility == "private" && i%3 == 0:
			_, err := in.db.NewInsert().Model(&db.Collaborator{ID: db.NewID(), RepoID: r.ID, UserID: viewer.UserID, Role: "write"}).Exec(ctx)
			require.NoError(t, err)
		}
	}

	batched, err := newHandler(ctx, in, newHealth(in.db, in.blobs), false)
	require.NoError(t, err)
	batchedSrv := httptest.NewServer(batched)
	t.Cleanup(batchedSrv.Close)
	hubDeps := hfapi.Deps{Settings: hfapi.FromConfig(in.cfg), Repos: oneRepoAtATime{in.repos}, LFS: in.lfs, Authz: in.auth.Authz(),
		Orgs: in.orgs, Community: oneCountAtATime{in.community}, Collections: in.collections, Index: oneIndexAtATime{in.indexer},
		Scan: in.scanner, Xet: in.xet, Devices: in.auth.DeviceFlow(), Viewer: disabledViewer{}, Log: in.log, Downloads: in.unsafeGate}
	if in.viewer != nil {
		hubDeps.Viewer = in.viewer
	}
	in.hub, err = hfapi.New(hubDeps)
	require.NoError(t, err)
	single, err := newHandler(ctx, in, newHealth(in.db, in.blobs), false)
	require.NoError(t, err)
	singleSrv := httptest.NewServer(single)
	t.Cleanup(singleSrv.Close)

	get := func(base, path string) (int, http.Header, []byte) {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+token)
		res, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer res.Body.Close()
		body, err := io.ReadAll(res.Body)
		require.NoError(t, err)
		return res.StatusCode, res.Header, body
	}

	var list struct {
		Items []json.RawMessage `json:"items"`
	}
	status, _, body := get(batchedSrv.URL, "/api/hangar/v1/repos?limit=500")
	require.Equal(t, http.StatusOK, status, string(body))
	require.NoError(t, json.Unmarshal(body, &list))
	require.Greater(t, len(list.Items), 100)
	for _, item := range list.Items {
		var r struct {
			Type, Namespace, Name string
		}
		require.NoError(t, json.Unmarshal(item, &r))
		status, _, one := get(batchedSrv.URL, "/api/hangar/v1/repos/"+r.Type+"s/"+r.Namespace+"/"+r.Name)
		require.Equal(t, http.StatusOK, status, string(one))
		require.JSONEq(t, string(one), string(item))
	}

	for _, path := range []string{
		"/api/models?limit=200",
		"/api/models?sort=downloads&limit=200",
		"/api/models?filter=library:transformers&sort=likes&direction=1&limit=50",
		"/api/models?pipeline_tag=text-generation&sort=trendingScore&limit=200",
		"/api/models?cardData=true&config=true&limit=200",
		"/api/models?expand=downloads&expand=downloadsAllTime&expand=likes&expand=library_name&expand=pipeline_tag&expand=tags&expand=sha&limit=200",
		"/api/models?search=" + url.QueryEscape("repo-00001"),
		"/api/datasets?sort=downloads&limit=200",
		"/api/datasets?expand=tags&expand=cardData&expand=downloadsAllTime&limit=200",
	} {
		wantStatus, wantHeader, want := get(singleSrv.URL, path)
		gotStatus, gotHeader, got := get(batchedSrv.URL, path)
		require.Equal(t, http.StatusOK, wantStatus, "%s: %s", path, want)
		require.Equal(t, wantStatus, gotStatus, path)
		require.Equal(t, wantHeader.Get("Link"), gotHeader.Get("Link"), path)
		require.Equal(t, string(want), string(got), path)
	}
}
