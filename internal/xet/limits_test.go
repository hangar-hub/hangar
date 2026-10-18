package xet_test

import (
	"bytes"
	"context"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/db"
	"github.com/hangar-hub/hangar/internal/db/dbtest"
	"github.com/hangar-hub/hangar/internal/xet"
	"github.com/hangar-hub/hangar/internal/xet/xettest"
)

// chunkRange is a term's [start, end) chunk range.
type chunkRange [2]uint32

// buildFile describes a file made of the given ranges of x, with every
// hash right, as a hostile client could build to name the same chunks many
// times over. It returns the file block and the file's bytes.
func buildFile(x xettest.Xorb, ranges ...chunkRange) (xet.ShardFile, []byte) {
	parts := make([]xorbPart, len(ranges))
	for i, r := range ranges {
		parts[i] = xorbPart{x, r}
	}
	return buildFileFrom(parts...)
}

// xorbPart is one term of a file: a chunk range of a xorb.
type xorbPart struct {
	x xettest.Xorb
	r chunkRange
}

// buildFileFrom describes a file made of chunk ranges of any xorbs, with
// every hash right.
func buildFileFrom(parts ...xorbPart) (xet.ShardFile, []byte) {
	f := xet.ShardFile{Flags: xet.FileFlagVerification | xet.FileFlagMetadataExt}
	var leaves []xet.Chunk
	var data []byte
	for _, p := range parts {
		x, r := p.x, p.r
		var size uint32
		var hashes []xet.Hash
		for i := r[0]; i < r[1]; i++ {
			c := x.Chunks[i]
			leaves = append(leaves, c)
			hashes = append(hashes, c.Hash)
			data = append(data, x.Data[i]...)
			size += uint32(c.Size) //nolint:gosec // test chunks are small.
		}
		f.Terms = append(f.Terms, xet.Term{Xorb: x.Hash, UnpackedBytes: size, Start: r[0], End: r[1]})
		f.Verification = append(f.Verification, xet.RangeHash(hashes))
	}
	f.Hash = xet.FileHash(leaves)
	sum := xettest.SHA256Hash(data)
	f.SHA256 = &sum
	return f, data
}

func repeat(r chunkRange, n int) []chunkRange {
	out := make([]chunkRange, n)
	for i := range out {
		out[i] = r
	}
	return out
}

func (e *env) acceptFiles(t *testing.T, files ...xet.ShardFile) ([]xet.ShardEvent, error) {
	t.Helper()
	_, events, err := e.accept(t, &xettest.Upload{Shard: &xet.Shard{Files: files}})
	return events, err
}

func (e *env) lastReferenced(t *testing.T, h xet.Hash) time.Time {
	t.Helper()
	var row db.XetXorb
	require.NoError(t, e.db.NewSelect().Model(&row).Where("hash = ?", h.String()).Scan(context.Background()))
	return row.LastReferencedAt
}

func (e *env) stored(t *testing.T, data []byte) bool {
	t.Helper()
	_, err := e.lfs.Stat(context.Background(), sha(data))
	return err == nil
}

func TestLimitsDefaults(t *testing.T) {
	l := xet.Limits{}.WithDefaults()
	require.Equal(t, xet.Limits{
		MaxFileSize: 50 << 30, MaxFileTerms: 1 << 18, MaxFileChunks: 50 << 30 / (16 << 10),
		MaxShardBytes: 4 * 50 << 30, MaxShardChunks: 8 * 50 << 30 / (16 << 10),
		MaxShardIndexChunks: 16 * 8192, MaxShardIndexLoads: 4 * 8 * 50 << 30 / (16 << 10),
	}, l)
	// Small files still allow a whole xorb of chunks, and nothing overflows.
	require.Equal(t, int64(xet.MaxXorbChunks), xet.Limits{MaxFileSize: 1 << 20}.WithDefaults().MaxFileChunks)
	big := xet.Limits{MaxFileSize: math.MaxInt64 / 2, MaxFileChunks: math.MaxInt64 / 3}.WithDefaults()
	require.Equal(t, int64(math.MaxInt64), big.MaxShardBytes)
	require.Equal(t, int64(math.MaxInt64), big.MaxShardChunks)
	require.Equal(t, int64(math.MaxInt64), big.MaxShardIndexLoads)
	// Set fields are kept.
	set := xet.Limits{
		MaxFileSize: 1, MaxFileTerms: 2, MaxFileChunks: 3, MaxShardBytes: 4, MaxShardChunks: 5,
		MaxShardIndexChunks: 6, MaxShardIndexLoads: 7,
	}
	require.Equal(t, set, set.WithDefaults())
}

// TestShardLimitsBoundTheWork checks that a shard cannot make Hangar
// rebuild or hash more than the limits allow by naming the same xorb
// ranges many times, and that terms outside their xorb are refused before
// any file is checked.
func TestShardLimitsBoundTheWork(t *testing.T) {
	lim := xet.Limits{MaxFileSize: 64 << 10, MaxFileTerms: 8, MaxFileChunks: 64, MaxShardBytes: 96 << 10, MaxShardChunks: 128}
	e := newEnv(t, dbtest.SQLite(t), func(o *xet.Options) { o.Limits = lim })
	ctx := context.Background()

	// x has four 4 KiB chunks; tiny has nine 16-byte chunks.
	u := xettest.BuildUpload([][]byte{textBytes(16<<10, 1)}, xettest.UploadOptions{ChunkSize: 4096})
	require.Len(t, u.Xorbs, 1)
	e.putAll(t, u)
	x := u.Xorbs[0]
	require.Len(t, x.Chunks, 4)
	tiny := xettest.BuildXorb(xettest.SplitChunks(textBytes(144, 2)[:144], 16), false)
	require.Len(t, tiny.Chunks, 9)
	_, err := e.svc.PutXorb(ctx, e.claims, tiny.Hash.String(), bytes.NewReader(tiny.Body), int64(len(tiny.Body)))
	require.NoError(t, err)
	all, nine := chunkRange{0, 4}, chunkRange{0, 9}

	// refusedEarly checks a shard is refused before any xorb is touched.
	refusedEarly := func(t *testing.T, want error, files ...xet.ShardFile) error {
		t.Helper()
		e.advance(time.Hour)
		before := e.lastReferenced(t, x.Hash)
		_, err := e.acceptFiles(t, files...)
		require.ErrorIs(t, err, want)
		require.Equal(t, before, e.lastReferenced(t, x.Hash), "the xorb was touched before the limits were checked")
		return err
	}

	t.Run("repeated ranges within the limits", func(t *testing.T) {
		f, data := buildFile(x, all, all)
		_, err := e.acceptFiles(t, f)
		require.NoError(t, err)
		require.Equal(t, data, e.objectBytes(t, sha(data)))
	})

	t.Run("file over the size limit", func(t *testing.T) {
		f, data := buildFile(x, repeat(all, 5)...) // 80 KiB
		err := refusedEarly(t, xet.ErrTooLarge, f)
		require.ErrorContains(t, err, "xet.max_file_size")
		require.False(t, e.stored(t, data))
	})

	t.Run("file with too many terms", func(t *testing.T) {
		f, data := buildFile(x, repeat(chunkRange{0, 1}, 9)...) // 36 KiB in 9 terms
		err := refusedEarly(t, xet.ErrTooLarge, f)
		require.ErrorContains(t, err, "9 terms")
		require.False(t, e.stored(t, data))
	})

	t.Run("file naming too many chunks", func(t *testing.T) {
		f, _ := buildFile(tiny, repeat(nine, 8)...) // 72 chunk references, 1 KiB
		err := refusedEarly(t, xet.ErrTooLarge, f)
		require.ErrorContains(t, err, "names 72 chunks")
	})

	t.Run("shard naming too many chunks", func(t *testing.T) {
		a, _ := buildFile(tiny, repeat(nine, 7)...)                           // 63
		b, _ := buildFile(tiny, append(repeat(nine, 7), chunkRange{0, 1})...) // 64
		c, _ := buildFile(tiny, chunkRange{0, 2})                             // 2
		err := refusedEarly(t, xet.ErrTooLarge, a, b, c)
		require.ErrorContains(t, err, "over 128 chunks")
	})

	t.Run("shard over the rebuild limit", func(t *testing.T) {
		a, aData := buildFile(x, repeat(all, 3)...)                           // 48 KiB
		b, bData := buildFile(x, append(repeat(all, 3), chunkRange{0, 1})...) // 52 KiB
		_, err := e.acceptFiles(t, a, b)
		require.ErrorIs(t, err, xet.ErrTooLarge)
		require.ErrorContains(t, err, "rebuilt bytes")
		require.True(t, e.stored(t, aData), "the first file fits the budget")
		require.False(t, e.stored(t, bData), "the second file was not rebuilt")
		_, err = e.acceptFiles(t, b)
		require.NoError(t, err)
		// Files rebuilt before are skipped and cost nothing.
		_, err = e.acceptFiles(t, a, b)
		require.NoError(t, err)
	})

	t.Run("term beyond its stored xorb", func(t *testing.T) {
		good, goodData := buildFile(x, chunkRange{1, 3})
		bad, _ := buildFile(x, chunkRange{0, 3})
		bad.Terms[0].End = 5
		events, err := e.acceptFiles(t, good, bad)
		require.ErrorIs(t, err, xet.ErrBadShard)
		require.ErrorContains(t, err, "has 4 chunks")
		for _, ev := range events {
			require.Zero(t, ev.Verified, "a file was checked before every range was")
		}
		require.False(t, e.stored(t, goodData))
	})

	t.Run("term beyond any xorb", func(t *testing.T) {
		bad, _ := buildFile(x, all)
		bad.Terms[0].End = xet.MaxXorbChunks + 1
		err := refusedEarly(t, xet.ErrBadShard, bad)
		require.ErrorContains(t, err, "at most 8192 chunks")
	})
}
