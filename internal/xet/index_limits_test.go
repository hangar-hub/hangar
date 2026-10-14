package xet_test

import (
	"bytes"
	"context"
	"io"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/jamesagarside/hangar/internal/blob"
	"github.com/jamesagarside/hangar/internal/db/dbtest"
	"github.com/jamesagarside/hangar/internal/xet"
	"github.com/jamesagarside/hangar/internal/xet/xettest"
)

// indexReads counts reads of stored xorb indexes: ranged reads from the
// start of a xorb object.
type indexReads struct {
	blob.Store
	n atomic.Int64
}

func (s *indexReads) Get(ctx context.Context, key string, rng *blob.Range) (io.ReadCloser, blob.Info, error) {
	if strings.HasPrefix(key, "xet/xorbs/") && rng != nil && rng.Start == 0 {
		s.n.Add(1)
	}
	return s.Store.Get(ctx, key, rng)
}

// indexEnv is an env whose index reads are counted and whose AcceptShard
// spans are recorded.
type indexEnv struct {
	*env
	reads *indexReads
	spans *tracetest.SpanRecorder
}

func newIndexEnv(t *testing.T, lim xet.Limits) *indexEnv {
	t.Helper()
	ie := &indexEnv{spans: tracetest.NewSpanRecorder()}
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(ie.spans))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	ie.env = newEnv(t, dbtest.SQLite(t), func(o *xet.Options) {
		o.Limits = lim
		o.Tracer = tp.Tracer("test")
		ie.reads = &indexReads{Store: o.Blobs}
		o.Blobs = ie.reads
	})
	return ie
}

// indexChunks returns the index chunks the last AcceptShard span recorded
// as loaded in all and held at most at once.
func (ie *indexEnv) indexChunks(t *testing.T) (loaded, peak int64) {
	t.Helper()
	ended := ie.spans.Ended()
	for i := len(ended) - 1; i >= 0; i-- {
		if ended[i].Name() != "xet.AcceptShard" {
			continue
		}
		found := 0
		for _, kv := range ended[i].Attributes() {
			switch kv.Key {
			case attribute.Key("xet.shard.index_chunks_loaded"):
				loaded = kv.Value.AsInt64()
				found++
			case attribute.Key("xet.shard.index_chunks_peak"):
				peak = kv.Value.AsInt64()
				found++
			}
		}
		require.Equal(t, 2, found, "the span does not record the index chunks")
		return loaded, peak
	}
	t.Fatal("no xet.AcceptShard span")
	return 0, 0
}

// fullXorbs stores n distinct xorbs of chunks one-byte chunks each: the
// cheapest xorbs whose indexes are largest, as an attacker would upload
// once and then name from any number of shards.
func (ie *indexEnv) fullXorbs(t *testing.T, n, chunks int) []xettest.Xorb {
	t.Helper()
	out := make([]xettest.Xorb, n)
	for i := range out {
		out[i] = xettest.BuildXorb(xettest.SplitChunks(textBytes(chunks, i+3), 1), false)
		_, err := ie.svc.PutXorb(context.Background(), ie.claims, out[i].Hash.String(), bytes.NewReader(out[i].Body), int64(len(out[i].Body)))
		require.NoError(t, err)
	}
	return out
}

// oneChunkOfEach is a file of one term per xorb, each naming one chunk:
// the most index loaded per byte of shard.
func oneChunkOfEach(xorbs []xettest.Xorb) (xet.ShardFile, []byte) {
	parts := make([]xorbPart, len(xorbs))
	for i, x := range xorbs {
		parts[i] = xorbPart{x, chunkRange{0, 1}}
	}
	return buildFileFrom(parts...)
}

// TestShardIndexMemoryIsBounded checks that a shard naming more xorb index
// than a shard may hold is processed within that bound by dropping and
// reloading indexes, and that the total it may load is bounded too. Before
// the fix, every index a shard named was loaded up front and held until the
// shard was done, so a shard of a few kilobytes naming many large xorbs
// could take hundreds of megabytes.
func TestShardIndexMemoryIsBounded(t *testing.T) {
	t.Run("default limits", func(t *testing.T) {
		// The reviewer's probe, scaled down: 20 full 8,192-chunk xorbs, one
		// chunk of each named in a shard of about 2 KB. Holding them all
		// would take 163,840 index chunks (about 7 MiB).
		ie := newIndexEnv(t, xet.Limits{})
		xorbs := ie.fullXorbs(t, 20, xet.MaxXorbChunks)
		f, data := oneChunkOfEach(xorbs)
		_, err := ie.acceptFiles(t, f)
		require.NoError(t, err)
		require.Equal(t, data, ie.objectBytes(t, sha(data)))
		loaded, peak := ie.indexChunks(t)
		require.LessOrEqual(t, peak, int64(xet.DefaultMaxShardIndexChunks))
		require.Greater(t, loaded, int64(20*xet.MaxXorbChunks), "the indexes were never dropped and reloaded")
		require.LessOrEqual(t, loaded, xet.Limits{}.WithDefaults().MaxShardIndexLoads)
	})

	// Xorbs of 64 chunks; a shard may hold two of them at once.
	lim := xet.Limits{MaxShardIndexChunks: 128}

	t.Run("more xorbs than fit are still accepted", func(t *testing.T) {
		ie := newIndexEnv(t, lim)
		xorbs := ie.fullXorbs(t, 6, 64)
		f, data := oneChunkOfEach(xorbs)
		ie.reads.n.Store(0)
		_, err := ie.acceptFiles(t, f)
		require.NoError(t, err)
		require.Equal(t, data, ie.objectBytes(t, sha(data)))
		_, peak := ie.indexChunks(t)
		require.LessOrEqual(t, peak, int64(128))
		// Ranges, file check and rebuild each load all six again.
		require.Equal(t, int64(18), ie.reads.n.Load())
	})

	t.Run("a shard over the index load limit", func(t *testing.T) {
		l := lim
		l.MaxShardIndexLoads = 6*64 + 63 // the ranges pass, and not the file check
		ie := newIndexEnv(t, l)
		xorbs := ie.fullXorbs(t, 6, 64)
		f, data := oneChunkOfEach(xorbs)
		_, err := ie.acceptFiles(t, f)
		require.ErrorIs(t, err, xet.ErrTooLarge)
		require.ErrorContains(t, err, "over 447 index chunks loaded")
		require.False(t, ie.stored(t, data))
		loaded, _ := ie.indexChunks(t)
		require.LessOrEqual(t, loaded, l.MaxShardIndexLoads)
	})

	t.Run("a xorb larger than a shard may hold", func(t *testing.T) {
		ie := newIndexEnv(t, xet.Limits{MaxShardIndexChunks: 63})
		xorbs := ie.fullXorbs(t, 1, 64)
		f, data := oneChunkOfEach(xorbs)
		_, err := ie.acceptFiles(t, f)
		require.ErrorIs(t, err, xet.ErrTooLarge)
		require.ErrorContains(t, err, "over the 63 index chunks a shard may hold")
		require.False(t, ie.stored(t, data))
		loaded, peak := ie.indexChunks(t)
		require.Zero(t, loaded)
		require.Zero(t, peak)
	})

	t.Run("CAS info xorbs share the bound", func(t *testing.T) {
		ie := newIndexEnv(t, lim)
		u := xettest.BuildUpload([][]byte{textBytes(10000, 7)}, xettest.UploadOptions{ChunkSize: 50, ChunksPerXorb: 64})
		require.Greater(t, len(u.Xorbs), 2)
		ie.putAll(t, u)
		res, _, err := ie.accept(t, u)
		require.NoError(t, err)
		require.Len(t, res.Files, 1)
		_, peak := ie.indexChunks(t)
		require.LessOrEqual(t, peak, int64(128))
	})
}

// TestCheckFileMemoryIsFlat checks that checking a file allocates about the
// same whatever number of chunks it names. Before the fix, CheckFile
// collected a leaf per chunk reference and a copy of each term's chunk
// hashes, so a shard of a few hundred terms naming whole 8,192-chunk ranges
// made it allocate hundreds of megabytes.
func TestCheckFileMemoryIsFlat(t *testing.T) {
	x := xettest.BuildXorb(xettest.SplitChunks(textBytes(xet.MaxXorbChunks, 5), 1), false)
	idx, err := xet.ParseXorb(bytes.NewReader(x.Body), nil)
	require.NoError(t, err)
	index := func(xet.Hash) (*xet.XorbIndex, error) { return idx, nil }
	all := chunkRange{0, xet.MaxXorbChunks}
	f, _ := buildFile(x, repeat(all, 12)...) // 98,304 chunk references

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	require.NoError(t, xet.CheckFile(&f, index))
	runtime.ReadMemStats(&after)
	// Holding the references would take 98,304 x 40 bytes (3.75 MiB) for
	// the leaves alone.
	require.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(256<<10))
}
