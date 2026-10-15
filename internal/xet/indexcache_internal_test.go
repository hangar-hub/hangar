package xet

import (
	"encoding/binary"
	"errors"
	"math"
	"math/rand/v2"
	"testing"

	"github.com/stretchr/testify/require"
)

// fakeChunks is the chunk count of fakeLoad's xorb h: 1 to 64.
func fakeChunks(h Hash) int { return 1 + int(h[0])%64 }

// fakeLoad stands in for stored xorbs, charging like loadIndex does.
func fakeLoad(loads *int) indexLoader {
	return func(h Hash, charge func(int) error) (*XorbIndex, error) {
		*loads++
		n := fakeChunks(h)
		if err := charge(n); err != nil {
			return nil, err
		}
		return &XorbIndex{Chunks: make([]XorbChunk, n)}, nil
	}
}

func hashWith(chunks, id int) Hash {
	var h Hash
	h[0] = byte(chunks - 1)
	binary.LittleEndian.PutUint32(h[1:], uint32(id)) //nolint:gosec // small test ids.
	return h
}

// checkCache asserts the cache's bookkeeping: what it holds is what it
// counts, and no bound is exceeded.
func checkCache(t testing.TB, c *indexCache) {
	t.Helper()
	var held int64
	for e := c.lru.Front(); e != nil; e = e.Next() {
		ci := e.Value.(*cachedIndex)
		if c.byHash[ci.hash] != e {
			t.Fatalf("xorb %s is in the list but not the map", ci.hash)
		}
		held += int64(len(ci.idx.Chunks))
	}
	if len(c.byHash) != c.lru.Len() || held != c.held {
		t.Fatalf("the cache holds %d chunks in %d indexes but counts %d in %d", held, c.lru.Len(), c.held, len(c.byHash))
	}
	if c.held > c.maxHeld || c.peak > c.maxHeld || c.loaded > c.maxLoaded || c.loaded > c.allowed {
		t.Fatalf("over the bounds: held %d, peak %d of %d; loaded %d of %d, %d allowed",
			c.held, c.peak, c.maxHeld, c.loaded, c.maxLoaded, c.allowed)
	}
}

func TestIndexCache(t *testing.T) {
	loads := 0
	c := newIndexCache(Limits{MaxShardIndexChunks: 10, MaxShardIndexLoads: 30}, math.MaxInt64, fakeLoad(&loads))
	a, b, d := hashWith(4, 1), hashWith(4, 2), hashWith(4, 3)

	x, err := c.get(a)
	require.NoError(t, err)
	again, err := c.get(a)
	require.NoError(t, err)
	require.Same(t, x, again, "a held index is not loaded again")
	require.Equal(t, 1, loads)
	_, err = c.get(b)
	require.NoError(t, err)
	_, err = c.get(a) // a is now the most recently used
	require.NoError(t, err)
	_, err = c.get(d) // 12 chunks do not fit: b goes
	require.NoError(t, err)
	checkCache(t, c)
	require.Equal(t, int64(8), c.held)
	require.Contains(t, c.byHash, a)
	require.NotContains(t, c.byHash, b)
	require.Equal(t, int64(12), c.loaded)
	require.Equal(t, int64(8), c.peak)

	// An index larger than the cache is refused before it is read.
	_, err = c.get(hashWith(11, 4))
	require.ErrorIs(t, err, ErrTooLarge)
	require.ErrorContains(t, err, "over the 10 index chunks a shard may hold")
	checkCache(t, c)

	// A failed load gives its room back but stays charged.
	failing := newIndexCache(Limits{MaxShardIndexChunks: 10, MaxShardIndexLoads: 30}, math.MaxInt64, func(h Hash, charge func(int) error) (*XorbIndex, error) {
		if err := charge(5); err != nil {
			return nil, err
		}
		return nil, errors.New("read failed")
	})
	_, err = failing.get(a)
	require.ErrorContains(t, err, "read failed")
	require.Zero(t, failing.held)
	require.Equal(t, int64(5), failing.loaded)
	checkCache(t, failing)

	// A loader that does not charge what it loads is an error.
	uncharged := newIndexCache(Limits{MaxShardIndexChunks: 10, MaxShardIndexLoads: 30}, math.MaxInt64, func(Hash, func(int) error) (*XorbIndex, error) {
		return &XorbIndex{Chunks: make([]XorbChunk, 3)}, nil
	})
	_, err = uncharged.get(a)
	require.ErrorContains(t, err, "without being accounted for")
	checkCache(t, uncharged)

	// Reloads count against the load limit.
	loads = 0
	c = newIndexCache(Limits{MaxShardIndexChunks: 4, MaxShardIndexLoads: 12}, math.MaxInt64, fakeLoad(&loads))
	for _, h := range []Hash{a, b, a} {
		_, err = c.get(h)
		require.NoError(t, err)
	}
	_, err = c.get(b)
	require.ErrorIs(t, err, ErrTooLarge)
	require.ErrorContains(t, err, "over 12 index chunks loaded")
	require.Equal(t, int64(12), c.loaded)
	require.Equal(t, 4, loads)
	checkCache(t, c)

	// Within the ceiling, a shard may load each xorb indexLoadsFactor times
	// and reload only what its allowance pays for: here 4 chunks, so with
	// two 4-chunk xorbs, 4 + 2*4*4 = 36 chunks, nine loads.
	loads = 0
	c = newIndexCache(Limits{MaxShardIndexChunks: 4, MaxShardIndexLoads: 1000}, 4, fakeLoad(&loads))
	for i := range 9 {
		_, err = c.get([]Hash{a, b}[i%2])
		require.NoError(t, err, "load %d", i+1)
		checkCache(t, c)
	}
	require.Equal(t, int64(36), c.allowed)
	_, err = c.get(b)
	require.ErrorIs(t, err, ErrTooLarge)
	require.ErrorContains(t, err, "over the 36 index chunks it may load for the xorbs and chunks it names")
	require.Equal(t, int64(36), c.loaded)
	require.Equal(t, 10, loads, "the refused load stops after the header")
	checkCache(t, c)

	// A xorb loaded for the first time brings its own allowance.
	_, err = c.get(d)
	require.NoError(t, err)
	require.Equal(t, int64(36+16), c.allowed)
	require.Equal(t, int64(40), c.loaded)
	checkCache(t, c)
}

// TestIndexReloadAllowance checks what a shard pays for: one refill of the
// index cache, plus indexLoadsFactor index chunks per chunk its terms and
// CAS info name.
func TestIndexReloadAllowance(t *testing.T) {
	sh := Shard{
		Files: []ShardFile{
			{Terms: []Term{{Start: 0, End: 1}, {Start: 2, End: 5}}},
			{Terms: []Term{{Start: 7, End: 7}}}, // empty, refused by CheckUpload
		},
		Xorbs: []ShardXorb{{Chunks: make([]ShardChunk, 3)}},
	}
	require.Equal(t, int64(100+4*7), sh.indexReloadAllowance(Limits{MaxShardIndexChunks: 100}))
	require.Equal(t, int64(DefaultMaxShardIndexChunks), (&Shard{}).indexReloadAllowance(Limits{}))
	huge := Limits{MaxShardIndexChunks: math.MaxInt64 - 1}
	require.Equal(t, int64(math.MaxInt64), sh.indexReloadAllowance(huge))
}

// FuzzIndexCache runs a fuzzed shard's checks through an index cache with
// small bounds and asserts after every lookup that the cache never holds
// or loads more than its bounds, the shard's allowance included, and
// returns the right index.
func FuzzIndexCache(f *testing.F) {
	many := Shard{}
	file := ShardFile{Flags: FileFlagVerification | FileFlagMetadataExt}
	for i := range 40 {
		file.Terms = append(file.Terms, Term{Xorb: hashWith(64, i), Start: 0, End: 1})
	}
	file.Terms = append(file.Terms, file.Terms[:5]...)
	file.Verification = make([]Hash, len(file.Terms))
	file.SHA256 = &Hash{}
	many.Files = append(many.Files, file)
	many.Xorbs = append(many.Xorbs, ShardXorb{Hash: hashWith(30, 99), Chunks: make([]ShardChunk, 30)})
	seed := MarshalShard(&many)
	if _, err := ParseShard(seed); err != nil {
		f.Fatalf("the seed does not parse: %v", err)
	}
	f.Add(seed)
	f.Fuzz(func(t *testing.T, b []byte) {
		sh, err := ParseShard(b)
		if err != nil {
			return
		}
		loads := 0
		lim := Limits{MaxShardIndexChunks: 100, MaxShardIndexLoads: 400}
		c := newIndexCache(lim, sh.indexReloadAllowance(lim), fakeLoad(&loads))
		index := func(h Hash) (*XorbIndex, error) {
			x, err := c.get(h)
			checkCache(t, c)
			if err != nil {
				if !errors.Is(err, ErrTooLarge) {
					t.Fatalf("unexpected cache error: %v", err)
				}
				return nil, err
			}
			if len(x.Chunks) != fakeChunks(h) {
				t.Fatalf("xorb %s: got an index of %d chunks, want %d", h, len(x.Chunks), fakeChunks(h))
			}
			return x, nil
		}
		allowed := func(err error) {
			if err != nil && !errors.Is(err, ErrBadShard) && !errors.Is(err, ErrTooLarge) && !errors.Is(err, errNoIndex) {
				t.Fatalf("unexpected check error: %v", err)
			}
		}
		allowed(CheckRanges(sh, index))
		for i := range sh.Xorbs {
			if x, err := index(sh.Xorbs[i].Hash); err == nil {
				allowed(CheckXorb(&sh.Xorbs[i], x))
			}
		}
		for i := range sh.Files {
			allowed(CheckFile(&sh.Files[i], index))
		}
	})
}

// batchMerkleRoot is the whole-level form of merkleRoot, as xet-core
// writes it, kept to check merkleBuilder against.
func batchMerkleRoot(chunks []Chunk) Hash {
	if len(chunks) == 0 {
		return Hash{}
	}
	nodes := append([]Chunk(nil), chunks...)
	var b merkleBuilder
	for len(nodes) > 1 {
		out := nodes[:0]
		for i := 0; i < len(nodes); {
			cut := batchMergeCut(nodes[i:])
			out = append(out, b.mergeNodes(nodes[i:i+cut]))
			i += cut
		}
		nodes = out
	}
	return nodes[0].Hash
}

func batchMergeCut(nodes []Chunk) int {
	if len(nodes) <= 2 {
		return len(nodes)
	}
	end := min(maxGroup, len(nodes))
	for i := 2; i < end; i++ {
		if nodes[i].Hash.mod4() == 0 {
			return i + 1
		}
	}
	return end
}

// leavesFrom turns bytes into leaves whose hashes cover every residue
// modulo 4.
func leavesFrom(b []byte) []Chunk {
	out := make([]Chunk, len(b))
	for i, v := range b {
		h := ChunkHash([]byte{v, byte(i), byte(i >> 8)})
		h[24] = h[24]&^3 | v&3
		out[i] = Chunk{Hash: h, Size: uint64(v) + 1}
	}
	return out
}

func TestMerkleBuilderMatchesBatch(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	for _, n := range []int{0, 1, 2, 3, 4, 8, 9, 10, 17, 81, 82, 100, 729, 1000, 8192} {
		b := make([]byte, n)
		for i := range b {
			b[i] = byte(r.IntN(256))
		}
		leaves := leavesFrom(b)
		require.Equal(t, batchMerkleRoot(leaves), merkleRoot(leaves), "%d leaves", n)
	}
}

// FuzzMerkleBuilder checks the streaming merkle root against the batch
// one for any leaves.
func FuzzMerkleBuilder(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0, 0, 0, 0})
	f.Add([]byte("hangar xet merkle tree"))
	f.Fuzz(func(t *testing.T, b []byte) {
		leaves := leavesFrom(b)
		if got, want := merkleRoot(leaves), batchMerkleRoot(leaves); got != want {
			t.Fatalf("%d leaves: streaming root %s, batch root %s", len(leaves), got, want)
		}
	})
}
