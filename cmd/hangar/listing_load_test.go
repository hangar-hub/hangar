//go:build load

package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"

	"github.com/hangar-hub/hangar/internal/auth"
	"github.com/hangar-hub/hangar/internal/config"
	"github.com/hangar-hub/hangar/internal/db"
	"github.com/hangar-hub/hangar/internal/db/dbtest"
	"github.com/hangar-hub/hangar/internal/model"
)

// TestLoadListing measures the listings that grow with the number of Repos
// on an Instance (02-architecture.md "Scale targets": 100,000 Repos): the
// Arsenal page (the native API's Repo list, which always counts facets),
// the same page with a library filter and the downloads sort, and the Hub's
// /api/models with a filter and a sort. It seeds the database directly with
// Repos, branch heads, index results, scan results, likes and downloads,
// so the sizes it reaches are not limited by how fast git can create
// Repos.
//
// HANGAR_LOAD_REPOS lists the sizes, 1000,10000 by default;
// HANGAR_LOAD_RUNS sets how many requests each scenario times after the
// first, 20 by default and 5 from 50,000 Repos; and
// HANGAR_LOAD_DRIVER=postgres runs on the test Postgres server instead of
// SQLite. HANGAR_LOAD_TRACE=1 also logs the slowest queries of each
// scenario's last request:
//
//	go test -tags load -run TestLoadListing -timeout 60m -v ./cmd/hangar
//	HANGAR_LOAD_REPOS=100000 go test -tags load -run TestLoadListing -timeout 60m -v ./cmd/hangar
func TestLoadListing(t *testing.T) {
	sizes := []int{1000, 10000}
	if v := os.Getenv("HANGAR_LOAD_REPOS"); v != "" {
		sizes = nil
		for _, s := range strings.Split(v, ",") {
			n, err := strconv.Atoi(strings.TrimSpace(s))
			require.NoError(t, err)
			sizes = append(sizes, n)
		}
	}
	for _, n := range sizes {
		t.Run(fmt.Sprintf("repos=%d", n), func(t *testing.T) { loadListing(t, n) })
	}
}

func loadListing(t *testing.T, n int) {
	ctx := context.Background()
	var mutate func(*config.Config)
	if os.Getenv("HANGAR_LOAD_DRIVER") == db.Postgres {
		dsn := dbtest.PostgresDSN(t)
		mutate = func(c *config.Config) { c.Database.Driver, c.Database.DSN = db.Postgres, dsn }
	}
	in := newTestInstance(t, mutate)
	h, err := newHandler(ctx, in, newHealth(in.db, in.blobs), false)
	require.NoError(t, err)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	viewer := testUser(t, in, "viewer")
	_, token, err := in.auth.TokenStore.Create(ctx, auth.NewToken{UserID: viewer.UserID, Name: "load", Scope: model.ScopeRead})
	require.NoError(t, err)
	start := time.Now()
	seedListing(t, in.db, viewer, n)
	t.Logf("seeded %d Repos on %s in %s", n, in.db.Driver(), time.Since(start).Round(time.Millisecond))

	log, slow := &queryLog{}, &slowLog{}
	in.db.DB = in.db.WithQueryHook(log).WithQueryHook(slow)
	get := func(path string) (time.Duration, int, []byte) {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+path, nil)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+token)
		log.start()
		slow.start()
		begin := time.Now()
		res, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		body, err := io.ReadAll(res.Body)
		require.NoError(t, err)
		d := time.Since(begin)
		queries := len(log.stop())
		require.NoError(t, res.Body.Close())
		require.Equal(t, http.StatusOK, res.StatusCode, "%s: %s", path, body)
		return d, queries, body
	}
	runs := 20
	if n >= 50000 {
		runs = 5
	}
	if v := os.Getenv("HANGAR_LOAD_RUNS"); v != "" {
		var err error
		runs, err = strconv.Atoi(v)
		require.NoError(t, err)
	}
	for _, sc := range []struct{ name, path string }{
		{"Arsenal page", "/api/hangar/v1/repos?limit=60"},
		{"Arsenal, library filter, downloads sort", "/api/hangar/v1/repos?limit=60&library=transformers&sort=downloads"},
		{"Hub list_models, library filter, downloads sort", "/api/models?filter=library:transformers&sort=downloads&limit=100"},
		{"Hub list_models, likes sort", "/api/models?sort=likes&limit=100"},
	} {
		first, firstQueries, body := get(sc.path)
		require.NotEqual(t, "[]", strings.TrimSpace(string(body)))
		var d []time.Duration
		var queries []int
		for range runs {
			dur, q, _ := get(sc.path)
			d = append(d, dur)
			queries = append(queries, q)
		}
		t.Logf("%-50s first %8s (%6d queries), then p50 %8s, p95 %8s, max %8s, %d to %d queries",
			sc.name, first.Round(time.Millisecond), firstQueries, loadPercentile(d, 0.5).Round(time.Millisecond),
			loadPercentile(d, 0.95).Round(time.Millisecond), loadPercentile(d, 1).Round(time.Millisecond),
			slices.Min(queries), slices.Max(queries))
		if os.Getenv("HANGAR_LOAD_TRACE") != "" {
			for _, q := range slow.slowest(5) {
				t.Logf("  %8s %.300s", q.d.Round(time.Millisecond), q.query)
			}
		}
	}
}

// slowLog records how long each query of the last request took.
type slowLog struct {
	mu      sync.Mutex
	queries []timedQuery
}

type timedQuery struct {
	d     time.Duration
	query string
}

func (l *slowLog) BeforeQuery(ctx context.Context, _ *bun.QueryEvent) context.Context { return ctx }

func (l *slowLog) AfterQuery(_ context.Context, ev *bun.QueryEvent) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.queries = append(l.queries, timedQuery{d: time.Since(ev.StartTime), query: ev.Query})
}

func (l *slowLog) start() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.queries = nil
}

func (l *slowLog) slowest(n int) []timedQuery {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := slices.Clone(l.queries)
	slices.SortFunc(out, func(a, b timedQuery) int { return int(b.d - a.d) })
	return out[:min(n, len(out))]
}

func loadPercentile(d []time.Duration, p float64) time.Duration {
	s := slices.Clone(d)
	slices.Sort(s)
	return s[int(float64(len(s)-1)*p)]
}
