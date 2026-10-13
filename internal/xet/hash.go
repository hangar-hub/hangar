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
		return h, fmt.Errorf("xet: %q is not a 64-character lower-case hex hash", s)
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
	if len(chunks) == 0 {
		return Hash{}
	}
	root := merkleRoot(chunks)
	return keyedHash(&fileKey, root[:])
}

// RangeHash returns the verification hash of a term: keyed BLAKE3 over the
// raw bytes of the chunk hashes it covers.
func RangeHash(hashes []Hash) Hash {
	hasher, err := blake3.NewKeyed(verificationKey[:])
	if err != nil {
		panic(err)
	}
	for i := range hashes {
		_, _ = hasher.Write(hashes[i][:])
	}
	var h Hash
	hasher.Sum(h[:0])
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
	if len(chunks) == 0 {
		return Hash{}
	}
	nodes := append([]Chunk(nil), chunks...)
	buf := make([]byte, 0, maxGroup*88)
	for len(nodes) > 1 {
		out := nodes[:0]
		for i := 0; i < len(nodes); {
			cut := nextMergeCut(nodes[i:])
			var merged Chunk
			merged, buf = mergeNodes(nodes[i:i+cut], buf)
			out = append(out, merged)
			i += cut
		}
		nodes = out
	}
	return nodes[0].Hash
}

func nextMergeCut(nodes []Chunk) int {
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

// mergeNodes hashes "<api hex> : <size>\n" for each node with the internal
// node key. It reuses buf and returns it for the next call.
func mergeNodes(group []Chunk, buf []byte) (Chunk, []byte) {
	buf = buf[:0]
	var total uint64
	for _, n := range group {
		buf = n.Hash.appendAPIHex(buf)
		buf = append(buf, " : "...)
		buf = strconv.AppendUint(buf, n.Size, 10)
		buf = append(buf, '\n')
		total += n.Size
	}
	return Chunk{Hash: keyedHash(&internalNodeKey, buf), Size: total}, buf
}
