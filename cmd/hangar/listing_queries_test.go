package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/auth"
	"github.com/hangar-hub/hangar/internal/model"
)

// The Arsenal page and the Hub's filtered and sorted listings cost a fixed
// number of queries, whatever the number of Repos on the Instance and on
// the page (02-architecture.md "Scale targets": 100,000 Repos). Before,
// they read every visible Repo's index results and community counts, and
// then each Repo on the page cost 12 to 15 more queries. The first listing
// after Repos change rebuilds their repo_facts rows, so it costs more.
func TestListingQueriesDoNotGrowWithRepos(t *testing.T) {
	ctx := context.Background()
	in := newTestInstance(t, nil)
	h, err := newHandler(ctx, in, newHealth(in.db, in.blobs), false)
	require.NoError(t, err)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	viewer := testUser(t, in, "viewer")
	_, token, err := in.auth.TokenStore.Create(ctx, auth.NewToken{UserID: viewer.UserID, Name: "t", Scope: model.ScopeRead})
	require.NoError(t, err)

	log := &queryLog{}
	in.db.DB = in.db.WithQueryHook(log)
	get := func(path string) ([]string, int) {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+path, nil)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+token)
		log.start()
		res, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		body, err := io.ReadAll(res.Body)
		require.NoError(t, err)
		queries := log.stop()
		require.NoError(t, res.Body.Close())
		require.Equal(t, http.StatusOK, res.StatusCode, "%s: %s", path, body)
		var items struct {
			Items []json.RawMessage `json:"items"`
		}
		var list []json.RawMessage
		if json.Unmarshal(body, &items) != nil || items.Items == nil {
			require.NoError(t, json.Unmarshal(body, &list), path)
		} else {
			list = items.Items
		}
		return queries, len(list)
	}
	paths := []string{
		"/api/hangar/v1/repos?limit=60",
		"/api/hangar/v1/repos?limit=60&library=transformers&sort=downloads",
		"/api/models?filter=library:transformers&sort=downloads&limit=100",
		"/api/models?sort=likes&limit=100",
		"/api/datasets?sort=downloads&limit=100",
	}
	counts := map[string][][]string{}
	for _, n := range []int{40, 400} {
		seedListing(t, in.db, viewer, n)
		for _, p := range paths {
			get(p) // rebuilds repo_facts for the new Repos
			queries, items := get(p)
			require.NotZero(t, items, p)
			counts[p] = append(counts[p], queries)
		}
	}
	for _, p := range paths {
		require.Len(t, counts[p][1], len(counts[p][0]), "%s: queries with 40 Repos, then with 440: %q\n%q", p, counts[p][0], counts[p][1])
		require.Less(t, len(counts[p][0]), 30, "%s: %q", p, counts[p][0])
		t.Logf("%s: %d queries", p, len(counts[p][0]))
	}
}
