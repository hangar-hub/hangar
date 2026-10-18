package repo

import (
	"container/list"
	"context"
	"fmt"
	"sync"

	"github.com/google/uuid"

	"github.com/hangar-hub/hangar/internal/gitstore"
)

// maxCachedBlob is the largest git blob kept in the blob cache: model
// cards, configs, tokenizer configs and .gitattributes, which the web UI
// and clients read again and again.
const maxCachedBlob = 64 << 10

// blobCacheSize bounds the memory the blob cache uses.
const blobCacheSize = 32 << 20

// blobCache keeps the content of small git blobs in memory, least recently
// used first out. Entries are keyed by blob OID, which names the content,
// so they never go stale. Callers find the OID in a Repo's own tree before
// asking, so the cache never shows a Repo content it does not hold.
type blobCache struct {
	mu    sync.Mutex
	max   int64
	size  int64
	order *list.List
	items map[string]*list.Element
}

type cachedBlob struct {
	oid  string
	data []byte
}

func newBlobCache(maxBytes int64) *blobCache {
	return &blobCache{max: maxBytes, order: list.New(), items: map[string]*list.Element{}}
}

// get returns a blob's content. The slice is shared and must not be
// changed.
func (c *blobCache) get(oid string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[oid]
	if !ok {
		return nil, false
	}
	c.order.MoveToFront(el)
	return el.Value.(*cachedBlob).data, true
}

func (c *blobCache) add(oid string, data []byte) {
	if int64(len(data)) > c.max {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.items[oid]; ok {
		return
	}
	c.items[oid] = c.order.PushFront(&cachedBlob{oid: oid, data: data})
	c.size += int64(len(data))
	for c.size > c.max {
		el := c.order.Back()
		b := el.Value.(*cachedBlob)
		c.order.Remove(el)
		delete(c.items, b.oid)
		c.size -= int64(len(b.data))
	}
}

// smallBlob returns the content of a git blob of at most maxCachedBlob
// bytes, from the blob cache or with one git process.
func (s *Store) smallBlob(ctx context.Context, repoID uuid.UUID, oid string) ([]byte, error) {
	if data, ok := s.blobs.get(oid); ok {
		return data, nil
	}
	objs, err := s.git.Acquire(ctx, repoID)
	if err != nil {
		return nil, err
	}
	defer objs.Release()
	var (
		data  []byte
		found bool
	)
	err = s.git.Git().BatchReadHead(ctx, objs.Dir, []string{oid}, maxCachedBlob+1, func(info gitstore.ObjectInfo, content []byte) error {
		if !info.Missing && info.Type == "blob" && info.Size <= maxCachedBlob {
			data, found = content, true
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("repo: git blob %s is missing or larger than %d bytes", oid, maxCachedBlob)
	}
	s.blobs.add(oid, data)
	return data, nil
}
