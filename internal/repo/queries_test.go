package repo_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"

	"github.com/jamesagarside/hangar/internal/db"
	"github.com/jamesagarside/hangar/internal/db/dbtest"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/repo"
)

// queryLog records the SQL a database runs.
type queryLog struct {
	mu      sync.Mutex
	queries []string
}

func (l *queryLog) BeforeQuery(ctx context.Context, _ *bun.QueryEvent) context.Context { return ctx }

func (l *queryLog) AfterQuery(_ context.Context, ev *bun.QueryEvent) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.queries = append(l.queries, ev.Query)
}

func (l *queryLog) take() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := l.queries
	l.queries = nil
	return out
}

// Entry is on resolve's path for every file a client downloads
// (02-architecture.md "Scale targets"). Once a Commit's tree cache is
// filled, it costs the Repo lookup and the tree row, with no separate
// check that the cache is filled. Paths that do not exist, and Commits
// with no cache yet, still answer correctly.
func TestEntryOnACachedTreeIsTwoQueries(t *testing.T) {
	t.Parallel()
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		log := &queryLog{}
		logged := *d
		logged.DB = d.WithQueryHook(log)
		e := newEnv(t, &logged)
		alice := e.addUser("alice")
		id := model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "entries"}
		e.create(repo.CreateParams{ID: id, Actor: alice})
		sha := e.commit(id, alice, add("config.json", "{}"), add("dir/a.txt", "a")).SHA

		// The first read may fill the cache.
		_, err := e.svc.Entry(e.ctx, id, sha, "config.json")
		require.NoError(t, err)
		log.take()

		for _, p := range []string{"config.json", "dir/a.txt", "dir"} {
			got, err := e.svc.Entry(e.ctx, id, sha, p)
			require.NoError(t, err)
			require.Equal(t, p, got.Path)
			require.Len(t, log.take(), 2, "queries for %s", p)
		}
		_, err = e.svc.Entry(e.ctx, id, sha, "missing.txt")
		require.ErrorIs(t, err, repo.ErrEntryNotFound)
		_, err = e.svc.Entry(e.ctx, id, model.CommitSHA(strings.Repeat("0", 40)), "config.json")
		require.ErrorIs(t, err, repo.ErrRevisionNotFound)
		_, err = e.svc.Entry(e.ctx, id, "main", "config.json")
		require.ErrorIs(t, err, repo.ErrRevisionNotFound)
	})
}

// A Commit whose tree cache is not filled yet, such as one a push just
// stored, is filled on the first Entry.
func TestEntryFillsAMissingTreeCache(t *testing.T) {
	t.Parallel()
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		e := newEnv(t, d)
		alice := e.addUser("alice")
		id := model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "unfilled"}
		created := e.create(repo.CreateParams{ID: id, Actor: alice})
		sha := e.commit(id, alice, add("config.json", "{}")).SHA
		_, err := d.NewDelete().Model((*db.TreeEntry)(nil)).Where("repo_id = ?", created.UUID).Exec(e.ctx)
		require.NoError(t, err)

		got, err := e.svc.Entry(e.ctx, id, sha, "config.json")
		require.NoError(t, err)
		require.Equal(t, int64(2), got.Size)
	})
}

// countRepoLookups returns how many queries read the repos table.
func countRepoLookups(queries []string) int {
	n := 0
	for _, q := range queries {
		if strings.Contains(q, `FROM "repos"`) {
			n++
		}
	}
	return n
}

// Under WithReadCache, the read methods look a Repo up once for the whole
// context, whatever case the ID is in. A Repo that is not there is not
// remembered, so one created later in the same context is found, and
// without the cache every call looks the Repo up.
func TestReadCache(t *testing.T) {
	t.Parallel()
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		log := &queryLog{}
		logged := *d
		logged.DB = d.WithQueryHook(log)
		e := newEnv(t, &logged)
		alice := e.addUser("alice")
		id := model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "cached"}
		e.create(repo.CreateParams{ID: id, Actor: alice})
		sha := e.commit(id, alice, add("config.json", "{}")).SHA
		_, err := e.svc.Entry(e.ctx, id, sha, "config.json")
		require.NoError(t, err)

		reads := func(ctx context.Context) {
			t.Helper()
			_, err := e.svc.Get(ctx, id)
			require.NoError(t, err)
			upper := model.RepoID{Type: id.Type, Namespace: "Alice", Name: "CACHED"}
			got, err := e.svc.Resolve(ctx, upper, "main")
			require.NoError(t, err)
			require.Equal(t, sha, got)
			_, err = e.svc.Entry(ctx, id, sha, "config.json")
			require.NoError(t, err)
			rc, _, err := e.svc.OpenFile(ctx, id, sha, "config.json", nil)
			require.NoError(t, err)
			require.NoError(t, rc.Close())
		}
		log.take()
		reads(e.ctx)
		require.Equal(t, 4, countRepoLookups(log.take()), "without the cache")
		reads(repo.WithReadCache(e.ctx))
		require.Equal(t, 1, countRepoLookups(log.take()), "with the cache")

		ctx := repo.WithReadCache(e.ctx)
		later := model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "later"}
		_, err = e.svc.Get(ctx, later)
		require.ErrorIs(t, err, repo.ErrNotFound)
		e.create(repo.CreateParams{ID: later, Actor: alice})
		_, err = e.svc.Get(ctx, later)
		require.NoError(t, err)
	})
}
