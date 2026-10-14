package xettest

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"

	"github.com/pierrec/lz4/v4"

	"github.com/jamesagarside/hangar/internal/xet"
)

// SHA256Hash returns data's SHA-256 in the form a shard's metadata
// extension carries it: its String is the hex digest.
func SHA256Hash(data []byte) xet.Hash {
	sum := sha256.Sum256(data)
	h, err := xet.ParseHash(hex.EncodeToString(sum[:]))
	if err != nil {
		panic(err)
	}
	return h
}

// SplitChunks cuts data into chunks of at most size bytes. Real clients cut
// at content-defined boundaries, but the server never checks boundaries, so
// fixed sizes exercise the same code.
func SplitChunks(data []byte, size int) [][]byte {
	var out [][]byte
	for len(data) > 0 {
		n := min(size, len(data))
		out = append(out, data[:n])
		data = data[n:]
	}
	return out
}

// BG4Split groups bytes by their position in each 4-byte word, the
// transform xet-core applies before LZ4 in the bg4-lz4 scheme.
func BG4Split(data []byte) []byte {
	n := len(data)
	s, r := n/4, n%4
	out := make([]byte, n)
	var starts [4]int
	off := 0
	for k := range 4 {
		starts[k] = off
		off += s
		if k < r {
			off++
		}
	}
	for i := range s {
		for k := range 4 {
			out[starts[k]+i] = data[4*i+k]
		}
	}
	for k := range r {
		out[starts[k]+s] = data[4*s+k]
	}
	return out
}

// LZ4Frame compresses data as one LZ4 frame, as lz4_flex's FrameEncoder does.
func LZ4Frame(data []byte) []byte {
	var b bytes.Buffer
	w := lz4.NewWriter(&b)
	if _, err := w.Write(data); err != nil {
		panic(err)
	}
	if err := w.Close(); err != nil {
		panic(err)
	}
	return b.Bytes()
}

// EncodeChunk returns a chunk header and payload in the given scheme.
func EncodeChunk(data []byte, scheme xet.CompressionScheme) []byte {
	payload := data
	switch scheme {
	case xet.CompressionLZ4:
		payload = LZ4Frame(data)
	case xet.CompressionBG4LZ4:
		payload = LZ4Frame(BG4Split(data))
	}
	h := xet.ChunkHeader{CompressedSize: uint32(len(payload)), Scheme: scheme, Size: uint32(len(data))}.Marshal()
	return append(h[:], payload...)
}

// Xorb is a serialised xorb.
type Xorb struct {
	Hash xet.Hash
	Body []byte
	// Chunks are the leaves, in order.
	Chunks []xet.Chunk
	// Data are the chunks' uncompressed bytes.
	Data [][]byte
}

// BuildXorb serialises chunks, cycling through schemes chunk by chunk
// (none when schemes is empty), and appends a footer-like trailer when
// footer is true.
func BuildXorb(chunks [][]byte, footer bool, schemes ...xet.CompressionScheme) Xorb {
	var b bytes.Buffer
	x := Xorb{Data: chunks}
	for i, c := range chunks {
		scheme := xet.CompressionNone
		if len(schemes) > 0 {
			scheme = schemes[i%len(schemes)]
		}
		b.Write(EncodeChunk(c, scheme))
		x.Chunks = append(x.Chunks, xet.Chunk{Hash: xet.ChunkHash(c), Size: uint64(len(c))})
	}
	x.Hash = xet.XorbHash(x.Chunks)
	if footer {
		b.WriteString("XETBLOB")
		b.Write(bytes.Repeat([]byte{1}, 64))
	}
	x.Body = b.Bytes()
	return x
}

// UploadOptions shape BuildUpload.
type UploadOptions struct {
	// ChunkSize is the chunk size; zero means 64 KiB.
	ChunkSize int
	// ChunksPerXorb caps the chunks in a xorb; zero means xet.MaxXorbChunks.
	ChunksPerXorb int
	// Schemes are cycled through for the chunks; empty means none.
	Schemes []xet.CompressionScheme
	// Known xorbs are referenced instead of uploaded when a file's chunks
	// match theirs exactly, as a client deduplicating against its cache does.
	Known []Xorb
}

// Upload is what a client sends for a set of files: new xorbs and a shard.
type Upload struct {
	Xorbs []Xorb
	Shard *xet.Shard
	// Files has each file's Xet hash and SHA-256, in order.
	Files []UploadedFile
}

// UploadedFile describes one uploaded file.
type UploadedFile struct {
	XetHash xet.Hash
	// SHA256 is the file's hex SHA-256: the LFS object ID.
	SHA256 string
	Size   int
}

// ShardBytes serialises the shard.
func (u *Upload) ShardBytes() []byte { return xet.MarshalShard(u.Shard) }

// BuildUpload packs files into xorbs and builds the shard hf_xet would send:
// a file block per file with verification entries and SHA-256 metadata,
// and a CAS info block per new xorb.
func BuildUpload(files [][]byte, o UploadOptions) *Upload {
	if o.ChunkSize <= 0 {
		o.ChunkSize = 64 << 10
	}
	if o.ChunksPerXorb <= 0 {
		o.ChunksPerXorb = xet.MaxXorbChunks
	}
	u := &Upload{Shard: &xet.Shard{}}
	type pending struct{ chunks [][]byte }
	var cur pending
	// Terms are fixed up with the xorb hash once each xorb is closed.
	type termRef struct{ file, term int }
	var open []termRef
	flush := func() {
		if len(cur.chunks) == 0 {
			return
		}
		x := BuildXorb(cur.chunks, false, o.Schemes...)
		u.Xorbs = append(u.Xorbs, x)
		for _, ref := range open {
			u.Shard.Files[ref.file].Terms[ref.term].Xorb = x.Hash
		}
		var off uint32
		info := xet.ShardXorb{Hash: x.Hash}
		for _, c := range x.Chunks {
			info.Chunks = append(info.Chunks, xet.ShardChunk{Hash: c.Hash, Offset: off, Size: uint32(c.Size)})
			off += uint32(c.Size)
		}
		info.RawBytes = off
		u.Shard.Xorbs = append(u.Shard.Xorbs, info)
		cur, open = pending{}, nil
	}
	for fi, data := range files {
		chunks := SplitChunks(data, o.ChunkSize)
		var leaves []xet.Chunk
		for _, c := range chunks {
			leaves = append(leaves, xet.Chunk{Hash: xet.ChunkHash(c), Size: uint64(len(c))})
		}
		f := xet.ShardFile{Hash: xet.FileHash(leaves), Flags: xet.FileFlagVerification | xet.FileFlagMetadataExt}
		sum := SHA256Hash(data)
		f.SHA256 = &sum
		u.Shard.Files = append(u.Shard.Files, f)
		u.Files = append(u.Files, UploadedFile{XetHash: f.Hash, SHA256: sum.String(), Size: len(data)})
		ff := &u.Shard.Files[fi]
		if known, ok := matchKnown(o.Known, leaves); ok {
			ff.Terms = []xet.Term{{Xorb: known.Hash, UnpackedBytes: uint32(len(data)), Start: 0, End: uint32(len(chunks))}}
			ff.Verification = []xet.Hash{xet.RangeHash(hashesOf(leaves))}
			continue
		}
		for i := 0; i < len(chunks); {
			if len(cur.chunks) == o.ChunksPerXorb {
				flush()
			}
			n := min(o.ChunksPerXorb-len(cur.chunks), len(chunks)-i)
			start := len(cur.chunks)
			var size uint32
			for _, c := range chunks[i : i+n] {
				size += uint32(len(c))
			}
			cur.chunks = append(cur.chunks, chunks[i:i+n]...)
			ff.Terms = append(ff.Terms, xet.Term{UnpackedBytes: size, Start: uint32(start), End: uint32(start + n)})
			ff.Verification = append(ff.Verification, xet.RangeHash(hashesOf(leaves[i:i+n])))
			open = append(open, termRef{file: fi, term: len(ff.Terms) - 1})
			i += n
		}
	}
	flush()
	return u
}

func matchKnown(known []Xorb, leaves []xet.Chunk) (Xorb, bool) {
	for _, x := range known {
		if len(x.Chunks) != len(leaves) {
			continue
		}
		same := true
		for i := range leaves {
			if x.Chunks[i] != leaves[i] {
				same = false
				break
			}
		}
		if same {
			return x, true
		}
	}
	return Xorb{}, false
}

func hashesOf(leaves []xet.Chunk) []xet.Hash {
	out := make([]xet.Hash, len(leaves))
	for i, l := range leaves {
		out[i] = l.Hash
	}
	return out
}
