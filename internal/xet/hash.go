package xet

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strconv"

	"github.com/zeebo/blake3"
)

// Hash is a 32-byte Xet hash (a chunk, xorb, file or verification hash) in
// its raw byte form, as shards and the verification hash input carry it.
// String gives the API string form used in URL paths
// (docs/research/xet-upload-research.md §3.5).
type Hash [32]byte

// Keys of the keyed BLAKE3 hashes, from xet-core
// (merklehash/data_hash.rs and metadata_shard/chunk_verification.rs,
// Apache-2.0; docs/research/xet-upload-research.md §3.4).
var (
	dataKey = [32]byte{
		102, 151, 245, 119, 91, 149, 80, 222, 49, 53, 203, 172, 165, 151, 24, 28,
		157, 228, 33, 16, 155, 235, 43, 88, 180, 208, 176, 75, 147, 173, 242, 41,
	}
	internalNodeKey = [32]byte{
		1, 126, 197, 199, 165, 71, 41, 150, 253, 148, 102, 102, 180, 138, 2, 230,
		93, 221, 83, 111, 55, 199, 109, 210, 248, 99, 82, 230, 74, 83, 113, 63,
	}
	verificationKey = [32]byte{
		127, 24, 87, 214, 206, 86, 237, 102, 18, 127, 249, 19, 231, 165, 195, 243,
		164, 205, 38, 213, 181, 219, 73, 230, 65, 36, 152, 127, 40, 251, 148, 195,
	}
	// fileKey is the salt of file hashes: 32 zero bytes.
	fileKey [32]byte
)

// String returns the API string form: each of the four little-endian 64-bit
// words printed as 16 lower-case hex digits, so every 8-byte group appears
// byte-reversed compared with plain hex.
func (h Hash) String() string {
	var b [64]byte
	h.appendAPIHex(b[:0])
	return string(b[:])
}

func (h Hash) appendAPIHex(dst []byte) []byte {
	var word [8]byte
	for k := range 4 {
		v := binary.LittleEndian.Uint64(h[8*k:])
		binary.BigEndian.PutUint64(word[:], v)
		dst = hex.AppendEncode(dst, word[:])
	}
	return dst
}

// IsZero reports whether every byte is zero: the hash of an empty file or
// an empty chunk list.
func (h Hash) IsZero() bool { return h == Hash{} }

// ParseHash parses the API string form of a hash.
func ParseHash(s string) (Hash, error) {
	var h Hash
	if !ValidHash(s) {
		return h, fmt.Errorf("xet: %q is not a 64-character lowercase hex hash", s)
	}
	for k := range 4 {
		v, err := strconv.ParseUint(s[16*k:16*k+16], 16, 64)
		if err != nil {
			return h, fmt.Errorf("xet: parse hash %q: %w", s, err)
		}
		binary.LittleEndian.PutUint64(h[8*k:], v)
	}
	return h, nil
}

// mod4 is the hash's last word modulo 4, which decides where the merkle
// tree cuts a group (xet-core `impl Rem for DataHash`).
func (h Hash) mod4() uint64 { return binary.LittleEndian.Uint64(h[24:]) % 4 }

func keyedHash(key *[32]byte, data []byte) Hash {
	hasher, err := blake3.NewKeyed(key[:])
	if err != nil {
		// NewKeyed fails only for a key that is not 32 bytes long.
		panic(err)
	}
	_, _ = hasher.Write(data)
	var h Hash
	hasher.Sum(h[:0])
	return h
}

// ChunkHash returns the hash of a chunk's uncompressed bytes.
func ChunkHash(data []byte) Hash { return keyedHash(&dataKey, data) }

// chunkHasher hashes many chunks with one keyed hasher.
type chunkHasher struct{ h *blake3.Hasher }

func newChunkHasher() chunkHasher {
	h, err := blake3.NewKeyed(dataKey[:])
	if err != nil {
		panic(err)
	}
	return chunkHasher{h: h}
}

func (c chunkHasher) sum(data []byte) Hash {
	c.h.Reset()
	_, _ = c.h.Write(data)
	var h Hash
	c.h.Sum(h[:0])
	return h
}

// Chunk is a chunk's hash and uncompressed length, the leaf of the merkle
// trees behind xorb and file hashes.
type Chunk struct {
	Hash Hash
	Size uint64
}

// XorbHash returns the hash of a xorb made of chunks, in order. It is the
// zero hash for no chunks.
func XorbHash(chunks []Chunk) Hash { return merkleRoot(chunks) }

// FileHash returns the Xet file hash of a file made of chunks, in order
// across every term. It is the zero hash for an empty file.
func FileHash(chunks []Chunk) Hash {
	var b merkleBuilder
	for _, c := range chunks {
		b.add(c)
	}
	return b.fileHash()
}

// RangeHash returns the verification hash of a term: keyed BLAKE3 over the
// raw bytes of the chunk hashes it covers.
func RangeHash(hashes []Hash) Hash {
	r := newRangeHasher()
	for i := range hashes {
		r.add(hashes[i])
	}
	return r.sum()
}

// rangeHasher computes RangeHash one chunk hash at a time, so checking a
// term needs no copy of its chunk hashes.
type rangeHasher struct{ h *blake3.Hasher }

func newRangeHasher() rangeHasher {
	h, err := blake3.NewKeyed(verificationKey[:])
	if err != nil {
		panic(err)
	}
	return rangeHasher{h: h}
}

func (r rangeHasher) reset()     { r.h.Reset() }
func (r rangeHasher) add(h Hash) { _, _ = r.h.Write(h[:]) }
func (r rangeHasher) sum() (h Hash) {
	r.h.Sum(h[:0])
	return h
}

// Merkle tree shape, from xet-core merklehash/aggregated_hashes.rs.
const (
	meanBranching = 4
	maxGroup      = 2*meanBranching + 1
)

// merkleRoot ports xet-core's aggregated_node_hash: repeatedly merge runs
// of 2 to 9 nodes, cutting after a node whose hash is 0 modulo 4, until one
// node remains. A single chunk is its own root.
func merkleRoot(chunks []Chunk) Hash {
	var b merkleBuilder
	for _, c := range chunks {
		b.add(c)
	}
	return b.root()
}

// merkleBuilder computes the same root as xet-core's aggregated_node_hash
// one leaf at a time. Each level of the tree keeps only the nodes it has
// not merged yet, at most maxGroup of them, so hashing a file holds a few
// hundred bytes per level instead of every chunk the file names.
//
// A level cuts a group after its third or later node whose hash is 0
// modulo 4, or after maxGroup nodes, exactly as nextMergeCut does over a
// whole level; whatever is left when the leaves end is the level's last
// group. A level that only ever receives one node is the root.
type merkleBuilder struct {
	levels []merkleLevel
	buf    []byte
	node   *blake3.Hasher // keyed with internalNodeKey, reused for every merge
}

type merkleLevel struct {
	pending []Chunk // nodes not merged yet, at most maxGroup
	count   int64   // nodes added to the level in all
}

// add appends a leaf.
func (b *merkleBuilder) add(c Chunk) { b.push(0, c) }

func (b *merkleBuilder) push(k int, c Chunk) {
	if k == len(b.levels) {
		b.levels = append(b.levels, merkleLevel{pending: make([]Chunk, 0, maxGroup)})
	}
	l := &b.levels[k]
	l.pending = append(l.pending, c)
	l.count++
	if n := len(l.pending); n == maxGroup || (n > 2 && c.Hash.mod4() == 0) {
		b.merge(k)
	}
}

// merge hashes level k's pending nodes into one node of level k+1.
func (b *merkleBuilder) merge(k int) {
	merged := b.mergeNodes(b.levels[k].pending)
	b.levels[k].pending = b.levels[k].pending[:0]
	b.push(k+1, merged)
}

// root returns the merkle root of the leaves added so far, or the zero
// hash for none. It finishes the tree, so add must not be called after it.
func (b *merkleBuilder) root() Hash {
	for k := 0; k < len(b.levels); k++ {
		l := &b.levels[k]
		if l.count == 1 {
			return l.pending[0].Hash
		}
		if len(l.pending) > 0 {
			b.merge(k)
		}
	}
	return Hash{}
}

// fileHash returns the Xet file hash of the leaves added so far: the zero
// hash for none. Like root, it finishes the tree.
func (b *merkleBuilder) fileHash() Hash {
	if len(b.levels) == 0 {
		return Hash{}
	}
	root := b.root()
	return keyedHash(&fileKey, root[:])
}

// mergeNodes hashes "<api hex> : <size>\n" for each node with the internal
// node key.
func (b *merkleBuilder) mergeNodes(group []Chunk) Chunk {
	if b.node == nil {
		h, err := blake3.NewKeyed(internalNodeKey[:])
		if err != nil {
			panic(err)
		}
		b.node = h
	}
	buf := b.buf[:0]
	var total uint64
	for _, n := range group {
		buf = n.Hash.appendAPIHex(buf)
		buf = append(buf, " : "...)
		buf = strconv.AppendUint(buf, n.Size, 10)
		buf = append(buf, '\n')
		total += n.Size
	}
	b.buf = buf
	b.node.Reset()
	_, _ = b.node.Write(buf)
	var out Chunk
	b.node.Sum(out.Hash[:0])
	out.Size = total
	return out
}
