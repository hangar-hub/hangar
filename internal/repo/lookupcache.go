package repo

import (
	"context"
	"strings"
	"sync"

	"github.com/hangar-hub/hangar/internal/model"
)

// lookupCacheKey is the context key of a *lookupCache.
type lookupCacheKey struct{}

// lookupCache holds the Repo rows the read methods of a Store looked up
// for one request, by the RepoID they were asked for.
type lookupCache struct {
	mu   sync.Mutex
	rows map[model.RepoID]*repoRow
}

// WithReadCache returns a context in which the read methods of Store (Get,
// Resolve, Entry, OpenFile, Tree and the other methods that only read)
// look each Repo up once and reuse the row for the rest of the context. A
// Hub download calls three or four of them for one Repo, and each lookup
// is a query.
//
// Use it only for a request that does not change Repos, such as a GET or
// HEAD: a rename or settings change made under the same context is not
// seen by later reads. Methods that write never use the cache.
func WithReadCache(ctx context.Context) context.Context {
	return context.WithValue(ctx, lookupCacheKey{}, &lookupCache{rows: map[model.RepoID]*repoRow{}})
}

func cacheKey(id model.RepoID) model.RepoID {
	return model.RepoID{Type: id.Type, Namespace: strings.ToLower(id.Namespace), Name: strings.ToLower(id.Name)}
}

// readLookup is lookup for read methods: from the context's read cache
// when it has one.
func (s *Store) readLookup(ctx context.Context, id model.RepoID) (*repoRow, error) {
	c, _ := ctx.Value(lookupCacheKey{}).(*lookupCache)
	if c == nil {
		return s.lookup(ctx, s.db, id)
	}
	key := cacheKey(id)
	c.mu.Lock()
	row, ok := c.rows[key]
	c.mu.Unlock()
	if ok {
		cp := *row
		return &cp, nil
	}
	row, err := s.lookup(ctx, s.db, id)
	if err != nil {
		return nil, err
	}
	cp := *row
	c.mu.Lock()
	c.rows[key] = &cp
	c.mu.Unlock()
	return row, nil
}
