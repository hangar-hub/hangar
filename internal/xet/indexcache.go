package xet

import (
	"container/list"
	"fmt"
)

// indexLoader reads one xorb's stored index. It must call charge with the
// index's chunk count after reading the index header and before decoding
// any entry, and stop with charge's error if it returns one.
type indexLoader func(h Hash, charge func(chunks int) error) (*XorbIndex, error)

// indexCache gives the xorb indexes one shard needs, loading each on first
// use. One 48-byte term can name a whole 8,192-chunk xorb, whose index
// takes about 360 KiB in memory, so holding every index a shard names
// would let a small shard fill memory. The cache instead holds at most
// maxHeld chunks of index, dropping the least recently used index to make
// room, and charges every chunk it loads, reloads included, against two
// bounds: maxLoaded, the fixed ceiling of MaxShardIndexLoads, and allowed,
// what the shard itself pays for. allowed starts at the shard's reload
// allowance (Shard.indexReloadAllowance) and grows by indexLoadsFactor
// times a xorb's chunk count the first time that xorb is loaded, so a shard
// may load each xorb it names once per pass, and reload indexes only in
// proportion to the chunks it names. Everything is charged from the index
// header, before an index's entries are read.
//
// An index dropped while a caller still uses it stays alive until that
// caller moves on, so memory is at most maxHeld chunks plus the one or two
// indexes in use. An indexCache is used by one goroutine.
type indexCache struct {
	load      indexLoader
	maxHeld   int64
	maxLoaded int64
	allowed   int64 // the index chunks the shard has paid for so far

	held   int64 // chunks of the indexes in lru
	loaded int64 // chunks loaded in all
	peak   int64 // the most chunks held at once
	lru    *list.List
	byHash map[Hash]*list.Element
	seen   map[Hash]struct{} // xorbs loaded at least once
}

type cachedIndex struct {
	hash Hash
	idx  *XorbIndex
}

// newIndexCache returns a cache bounded by l.MaxShardIndexChunks,
// l.MaxShardIndexLoads (after WithDefaults) and a shard's reload
// allowance.
func newIndexCache(l Limits, reloads int64, load indexLoader) *indexCache {
	l = l.WithDefaults()
	return &indexCache{
		load: load, maxHeld: l.MaxShardIndexChunks, maxLoaded: l.MaxShardIndexLoads, allowed: reloads,
		lru: list.New(), byHash: map[Hash]*list.Element{}, seen: map[Hash]struct{}{},
	}
}

// get returns the index of xorb h, loading it if the cache does not hold
// it. It has the IndexFunc signature.
func (c *indexCache) get(h Hash) (*XorbIndex, error) {
	if e, ok := c.byHash[h]; ok {
		c.lru.MoveToFront(e)
		return e.Value.(*cachedIndex).idx, nil
	}
	var charged int64
	idx, err := c.load(h, func(chunks int) error {
		n := int64(chunks)
		if err := c.reserve(h, n); err != nil {
			return err
		}
		charged = n
		return nil
	})
	if err == nil && (idx == nil || int64(len(idx.Chunks)) != charged) {
		err = fmt.Errorf("xet: the index of xorb %s was loaded without being accounted for", h)
	}
	if err != nil {
		c.held -= charged
		return nil, err
	}
	c.byHash[h] = c.lru.PushFront(&cachedIndex{hash: h, idx: idx})
	return idx, nil
}

// reserve makes room for an index of n chunks and charges it, or reports
// that the shard would go over its limits.
func (c *indexCache) reserve(h Hash, n int64) error {
	if n > c.maxHeld {
		return fmt.Errorf("%w: xorb %s has %d chunks, over the %d index chunks a shard may hold",
			ErrTooLarge, h, n, c.maxHeld)
	}
	if n > c.maxLoaded-c.loaded {
		return fmt.Errorf("%w: loading the index of xorb %s would take the shard over %d index chunks loaded",
			ErrTooLarge, h, c.maxLoaded)
	}
	_, seen := c.seen[h]
	allowed := c.allowed
	if !seen {
		allowed = satAdd(allowed, satMul(indexLoadsFactor, n))
	}
	if n > allowed-c.loaded {
		return fmt.Errorf("%w: loading the index of xorb %s would take the shard over the %d index chunks "+
			"it may load for the xorbs and chunks it names", ErrTooLarge, h, allowed)
	}
	c.allowed = allowed
	c.seen[h] = struct{}{}
	for c.held+n > c.maxHeld {
		e := c.lru.Back()
		old := c.lru.Remove(e).(*cachedIndex)
		delete(c.byHash, old.hash)
		c.held -= int64(len(old.idx.Chunks))
	}
	c.held += n
	c.loaded += n
	c.peak = max(c.peak, c.held)
	return nil
}
