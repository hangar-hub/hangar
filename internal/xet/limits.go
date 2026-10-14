package xet

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"
)

// Limits on the work one shard can cause. A 64 MiB shard can describe
// files whose terms name the same xorb ranges many times over, so without
// them a small upload could make Hangar read, decompress and hash data
// without bound.
const (
	// DefaultMaxFileSize is the default of xet.max_file_size: 50 GiB, in
	// line with the Hub's 50 GB file limit. huggingface_hub does not
	// download larger files over plain HTTP (03-hf-api.md rule 14).
	DefaultMaxFileSize = 50 << 30
	// DefaultMaxFileTerms is the default limit on the terms of one file. A
	// file stored in whole xorbs has one term per 64 MiB.
	DefaultMaxFileTerms = 1 << 18
	// chunkBudget is the average chunk size the chunk reference limits
	// allow for: a quarter of hf_xet's 64 KiB target.
	chunkBudget = 16 << 10
	// shardBytesFactor is how many times MaxFileSize one shard may rebuild.
	shardBytesFactor = 4
	// shardChunksFactor is how many times MaxFileChunks one shard may name.
	shardChunksFactor = 8
	// DefaultMaxShardIndexChunks is the default limit on the xorb index
	// chunks one shard holds in memory at once: 16 whole xorbs, about
	// 5.5 MiB at 44 bytes a chunk.
	DefaultMaxShardIndexChunks = 16 * MaxXorbChunks
	// indexLoadsFactor is how many times MaxShardChunks of index one shard
	// may load in all. A shard is checked in passes (ranges, CAS info,
	// files, rebuilds), and each may load every index again.
	indexLoadsFactor = 4
)

// Limits bound the work one shard can cause. Zero fields take their
// defaults from WithDefaults.
type Limits struct {
	// MaxFileSize is the most bytes one file may rebuild to
	// (xet.max_file_size).
	MaxFileSize int64
	// MaxFileTerms is the most terms one file may have.
	MaxFileTerms int
	// MaxFileChunks is the most chunk references the terms of one file may
	// make. Checking a file hashes every chunk reference.
	MaxFileChunks int64
	// MaxShardBytes is the most bytes one shard may rebuild. Files that
	// were rebuilt and recorded before are skipped, so they do not count.
	MaxShardBytes int64
	// MaxShardChunks is the most chunk references all the files of one
	// shard may make.
	MaxShardChunks int64
	// MaxShardIndexChunks is the most xorb index chunks one shard may hold
	// in memory at once. One 48-byte term can name a whole xorb, whose
	// index takes about 44 bytes a chunk, so indexes are loaded as they
	// are needed and the least recently used are dropped to stay within
	// this. A xorb with more chunks than this cannot be named at all, so
	// it should be at least MaxXorbChunks.
	MaxShardIndexChunks int64
	// MaxShardIndexLoads is the most xorb index chunks one shard may load
	// in all, reloads after a drop included. It bounds the reading and
	// hashing a shard can cause by naming many xorbs.
	MaxShardIndexLoads int64
}

// WithDefaults fills the zero fields: MaxFileSize with DefaultMaxFileSize,
// MaxFileTerms with DefaultMaxFileTerms, MaxFileChunks with one chunk per
// 16 KiB of MaxFileSize (at least MaxXorbChunks), MaxShardBytes with four
// times MaxFileSize, MaxShardChunks with eight times MaxFileChunks,
// MaxShardIndexChunks with DefaultMaxShardIndexChunks and
// MaxShardIndexLoads with four times MaxShardChunks.
func (l Limits) WithDefaults() Limits {
	if l.MaxFileSize <= 0 {
		l.MaxFileSize = DefaultMaxFileSize
	}
	if l.MaxFileTerms <= 0 {
		l.MaxFileTerms = DefaultMaxFileTerms
	}
	if l.MaxFileChunks <= 0 {
		l.MaxFileChunks = max(l.MaxFileSize/chunkBudget, MaxXorbChunks)
	}
	if l.MaxShardBytes <= 0 {
		l.MaxShardBytes = satMul(shardBytesFactor, l.MaxFileSize)
	}
	if l.MaxShardChunks <= 0 {
		l.MaxShardChunks = satMul(shardChunksFactor, l.MaxFileChunks)
	}
	if l.MaxShardIndexChunks <= 0 {
		l.MaxShardIndexChunks = DefaultMaxShardIndexChunks
	}
	if l.MaxShardIndexLoads <= 0 {
		l.MaxShardIndexLoads = satMul(indexLoadsFactor, l.MaxShardChunks)
	}
	return l
}

// satMul multiplies two positive numbers, saturating at math.MaxInt64.
func satMul(a, b int64) int64 {
	if b > math.MaxInt64/a {
		return math.MaxInt64
	}
	return a * b
}

// CheckLimits checks the shard against l (after WithDefaults) using only
// what the shard declares, so it runs before any xorb is looked up: the
// term count, declared size and chunk references of each file, and the
// chunk references of the whole shard. A file's declared size is what it
// rebuilds to, because CheckFile rejects any term whose chunks hold a
// different number of bytes before anything is rebuilt.
func (s *Shard) CheckLimits(l Limits) error {
	l = l.WithDefaults()
	var shardChunks int64
	for i, f := range s.Files {
		if len(f.Terms) > l.MaxFileTerms {
			return fmt.Errorf("%w: file %d (%s) has %d terms, over the limit of %d",
				ErrTooLarge, i, f.Hash, len(f.Terms), l.MaxFileTerms)
		}
		var chunks int64
		for _, t := range f.Terms {
			if t.End > t.Start {
				chunks += int64(t.End - t.Start)
			}
		}
		if size := f.Size(); size > l.MaxFileSize {
			return fmt.Errorf("%w: file %d (%s) is %d bytes, over the limit of %d (xet.max_file_size)",
				ErrTooLarge, i, f.Hash, size, l.MaxFileSize)
		}
		if chunks > l.MaxFileChunks {
			return fmt.Errorf("%w: file %d (%s) names %d chunks, over the limit of %d",
				ErrTooLarge, i, f.Hash, chunks, l.MaxFileChunks)
		}
		shardChunks += chunks
		if shardChunks > l.MaxShardChunks {
			return fmt.Errorf("%w: the shard names over %d chunks", ErrTooLarge, l.MaxShardChunks)
		}
	}
	return nil
}

// CheckRanges checks that every term of every file ends within the stored
// xorb it names. It reads only the chunk counts, so it runs before any
// file is checked or rebuilt.
func CheckRanges(s *Shard, index IndexFunc) error {
	for i := range s.Files {
		f := &s.Files[i]
		for j, t := range f.Terms {
			x, err := index(t.Xorb)
			if err != nil {
				return err
			}
			if x == nil {
				return fmt.Errorf("%w: %s", errNoIndex, t.Xorb)
			}
			if t.Start >= t.End || int(t.End) > len(x.Chunks) {
				return fmt.Errorf("%w: file %s term %d is chunks %d to %d, but xorb %s has %d chunks",
					ErrBadShard, f.Hash, j, t.Start, t.End, t.Xorb, len(x.Chunks))
			}
		}
	}
	return nil
}

// spend takes n bytes from a shard's rebuild budget, or reports that
// rebuilding them would take the shard over its limit.
func spend(budget *int64, f *ShardFile, n, limit int64) error {
	if n > *budget {
		return fmt.Errorf("%w: rebuilding file %s would take the shard over %d rebuilt bytes", ErrTooLarge, f.Hash, limit)
	}
	*budget -= n
	return nil
}

// Defaults for the shard ingest slots of HandlerOptions.
const (
	// DefaultShardIngests is how many shards one Namespace may have in
	// progress at once.
	DefaultShardIngests = 2
	// DefaultShardQueueWait is how long a shard upload waits for a slot
	// before it gets 429, which hf_xet retries. It is well under the
	// 100-second idle timeout of common proxies, because nothing is sent
	// while it waits.
	DefaultShardQueueWait = 30 * time.Second
	// shardRetryAfter is the Retry-After value sent with 429, in seconds.
	shardRetryAfter = "10"
)

// ingestSlots bounds the shard ingests in progress per Namespace. A slot is
// taken before the shard body is read, so waiting uploads hold no shard in
// memory.
type ingestSlots struct {
	size int
	mu   sync.Mutex
	byNS map[string]*nsSlots
}

type nsSlots struct {
	ch    chan struct{}
	users int // holders and waiters; the entry is removed at zero
}

func newIngestSlots(size int) *ingestSlots {
	return &ingestSlots{size: size, byNS: map[string]*nsSlots{}}
}

// acquire takes a slot for the Namespace, waiting up to wait. It returns
// ErrBusy if none frees up in time, or the context's error.
func (l *ingestSlots) acquire(ctx context.Context, namespace string, wait time.Duration) (release func(), err error) {
	key := strings.ToLower(namespace)
	l.mu.Lock()
	s := l.byNS[key]
	if s == nil {
		s = &nsSlots{ch: make(chan struct{}, l.size)}
		l.byNS[key] = s
	}
	s.users++
	l.mu.Unlock()
	leave := func() {
		l.mu.Lock()
		s.users--
		if s.users == 0 {
			delete(l.byNS, key)
		}
		l.mu.Unlock()
	}
	select {
	case s.ch <- struct{}{}:
		return func() { <-s.ch; leave() }, nil
	default:
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case s.ch <- struct{}{}:
		return func() { <-s.ch; leave() }, nil
	case <-t.C:
		leave()
		return nil, ErrBusy
	case <-ctx.Done():
		leave()
		return nil, ctx.Err()
	}
}
