package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"

	"github.com/jamesagarside/hangar/internal/auth"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/repo"
)

// queryLog records the SQL a database runs while it is on.
type queryLog struct {
	mu      sync.Mutex
	on      bool
	queries []string
}

func (l *queryLog) BeforeQuery(ctx context.Context, _ *bun.QueryEvent) context.Context { return ctx }

func (l *queryLog) AfterQuery(_ context.Context, ev *bun.QueryEvent) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.on {
		l.queries = append(l.queries, ev.Query)
	}
}

func (l *queryLog) start() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.on, l.queries = true, nil
}

func (l *queryLog) stop() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.on = false
	return l.queries
}

// count returns how many queries read from table.
func count(queries []string, table string) int {
	n := 0
	for _, q := range queries {
		if strings.Contains(q, `FROM "`+table+`"`) || strings.Contains(q, "FROM "+table+" ") {
			n++
		}
	}
	return n
}

// HEAD /resolve is the hottest call a Hub client makes, at the 200
// requests per second per replica in 02-architecture.md "Scale targets",
// and its database queries set its cost. With a Token, a download looks up
// the Token, its user, the Repo once, the caller's Role (two queries), the
// ref, the Commit and the file's tree row: 8 queries, down from 14.
func TestHubDownloadQueries(t *testing.T) {
	ctx := context.Background()
	in := newTestInstance(t, nil)
	h, err := newHandler(ctx, in, newHealth(in.db, in.blobs), false)
	require.NoError(t, err)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	p := testUser(t, in, "alice")
	_, token, err := in.auth.TokenStore.Create(ctx, auth.NewToken{UserID: p.UserID, Name: "t", Scope: model.ScopeRead})
	require.NoError(t, err)
	id := model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "tiny"}
	_, err = in.repos.Create(ctx, repo.CreateParams{ID: id, Actor: p, InitialReadme: true, Visibility: model.VisibilityPublic})
	require.NoError(t, err)

	do := func(method string, header map[string]string) int {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, method, srv.URL+"/alice/tiny/resolve/main/README.md", nil)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+token)
		for k, v := range header {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		return resp.StatusCode
	}
	// The first download fills the tree cache and records the Token's use.
	require.Equal(t, http.StatusOK, do(http.MethodGet, nil))

	log := &queryLog{}
	// Services hold the *db.DB, so they all run queries through the hook.
	in.db.DB = in.db.WithQueryHook(log)
	for _, tc := range []struct {
		method string
		header map[string]string
		want   int
		// A GET reads the tree row again when it opens the file.
		treeRows, queries int
	}{
		{http.MethodHead, nil, http.StatusOK, 1, 8},
		{http.MethodGet, map[string]string{"Range": "bytes=0-3"}, http.StatusPartialContent, 2, 9},
	} {
		log.start()
		require.Equal(t, tc.want, do(tc.method, tc.header), tc.method)
		queries := log.stop()
		require.Equal(t, 1, count(queries, "repos"), "%s looks the Repo up once: %v", tc.method, queries)
		require.Equal(t, 0, count(queries, "identities"), "%s: %v", tc.method, queries)
		require.Equal(t, 1, count(queries, "collaborators"), "%s checks permission once: %v", tc.method, queries)
		require.Equal(t, tc.treeRows, count(queries, "tree_entries"), "%s: %v", tc.method, queries)
		require.Len(t, queries, tc.queries, "%s: %v", tc.method, queries)
	}
}
