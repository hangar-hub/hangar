package xet

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/pierrec/lz4/v4"
)

// Xorb limits (docs/research/xet-upload-research.md §2.2 and §3.6; xet-core
// xorb_object/constants.rs).
const (
	// MaxChunkSize is the largest uncompressed chunk: 2 × the 64 KiB target.
	MaxChunkSize = 128 << 10
	// MaxCompressedChunkSize is the largest compressed chunk payload.
	MaxCompressedChunkSize = 2 * MaxChunkSize
	// MaxXorbChunks is the most chunks one xorb may hold.
	MaxXorbChunks = 8192
	// MaxXorbBytes is the most uncompressed bytes one xorb may hold.
	MaxXorbBytes = 64 << 20
	// MaxXorbSize is the largest xorb body Hangar accepts: MaxXorbBytes of
	// chunks stored uncompressed, their 8-byte headers, and room for an
	// optional footer, which hf_xet never sends but older clients may.
	MaxXorbSize = MaxXorbBytes + MaxXorbChunks*chunkHeaderSize + 1<<20
)

// chunkHeaderSize is the size of a chunk header in a xorb.
const chunkHeaderSize = 8

// footerIdent starts the optional xorb footer (XorbObjectInfoV1).
var footerIdent = []byte("XETBLOB")

// CompressionScheme says how a chunk's payload is encoded.
type CompressionScheme byte

// Compression schemes. Auto (99) is never valid on the wire.
const (
	CompressionNone   CompressionScheme = 0
	CompressionLZ4    CompressionScheme = 1
	CompressionBG4LZ4 CompressionScheme = 2
)

// ChunkHeader is the 8-byte header before each chunk payload in a xorb.
type ChunkHeader struct {
	Version        byte
	CompressedSize uint32
	Scheme         CompressionScheme
	Size           uint32
}

// Marshal returns the header's wire form.
func (c ChunkHeader) Marshal() [chunkHeaderSize]byte {
	var b [chunkHeaderSize]byte
	b[0] = c.Version
	putUint24(b[1:4], c.CompressedSize)
	b[4] = byte(c.Scheme)
	putUint24(b[5:8], c.Size)
	return b
}

func putUint24(b []byte, v uint32) { b[0], b[1], b[2] = byte(v), byte(v>>8), byte(v>>16) } //nolint:gosec // truncation to bytes is the encoding.

func uint24(b []byte) uint32 { return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 }

// parseChunkHeader parses and checks a chunk header. footer is true when the
// bytes start the optional xorb footer instead.
func parseChunkHeader(b []byte) (h ChunkHeader, footer bool, err error) {
	if bytes.HasPrefix(b, footerIdent) {
		return h, true, nil
	}
	h = ChunkHeader{Version: b[0], CompressedSize: uint24(b[1:4]), Scheme: CompressionScheme(b[4]), Size: uint24(b[5:8])}
	switch {
	case h.Version != 0:
		return h, false, fmt.Errorf("chunk header version %d is not 0", h.Version)
	case h.Scheme > CompressionBG4LZ4:
		return h, false, fmt.Errorf("unknown compression scheme %d", h.Scheme)
	case h.CompressedSize > MaxCompressedChunkSize:
		return h, false, fmt.Errorf("compressed chunk length %d is over %d", h.CompressedSize, MaxCompressedChunkSize)
	case h.Size > MaxChunkSize:
		return h, false, fmt.Errorf("chunk length %d is over %d", h.Size, MaxChunkSize)
	case h.Size == 0:
		return h, false, errors.New("chunk is empty")
	case h.Scheme == CompressionNone && h.CompressedSize != h.Size:
		return h, false, fmt.Errorf("uncompressed chunk declares %d stored bytes for %d bytes", h.CompressedSize, h.Size)
	}
	return h, false, nil
}

var lz4Readers = sync.Pool{New: func() any { return lz4.NewReader(nil) }}

// decoder decompresses chunk payloads, reusing its buffers.
type decoder struct {
	src     bytes.Reader
	grouped []byte
}

// decompress decodes payload into dst[:size] and returns it. The output must
// be exactly size bytes, and the payload must be used up.
func (d *decoder) decompress(scheme CompressionScheme, payload []byte, size uint32, dst []byte) ([]byte, error) {
	dst = growBuf(dst, int(size))
	switch scheme {
	case CompressionNone:
		if len(payload) != int(size) {
			return nil, fmt.Errorf("uncompressed chunk has %d bytes, not %d", len(payload), size)
		}
		copy(dst, payload)
		return dst, nil
	case CompressionLZ4:
		if err := d.lz4(payload, dst); err != nil {
			return nil, err
		}
		return dst, nil
	case CompressionBG4LZ4:
		d.grouped = growBuf(d.grouped, int(size))
		if err := d.lz4(payload, d.grouped); err != nil {
			return nil, err
		}
		bg4Regroup(d.grouped, dst)
		return dst, nil
	default:
		return nil, fmt.Errorf("unknown compression scheme %d", scheme)
	}
}

// lz4 decodes an LZ4 frame (the format lz4_flex's FrameEncoder writes) into
// exactly len(dst) bytes. Reading one byte past the declared length guards
// against decompression bombs.
func (d *decoder) lz4(payload, dst []byte) error {
	d.src.Reset(payload)
	zr := lz4Readers.Get().(*lz4.Reader)
	defer lz4Readers.Put(zr)
	zr.Reset(&d.src)
	if _, err := io.ReadFull(zr, dst); err != nil {
		return fmt.Errorf("LZ4 chunk decodes to fewer than %d bytes: %w", len(dst), err)
	}
	var extra [1]byte
	n, err := zr.Read(extra[:])
	if n > 0 {
		return fmt.Errorf("LZ4 chunk decodes to more than %d bytes", len(dst))
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("LZ4 chunk is corrupt: %w", err)
	}
	if d.src.Len() != 0 {
		return fmt.Errorf("LZ4 chunk has %d bytes after its frame", d.src.Len())
	}
	return nil
}

func growBuf(b []byte, n int) []byte {
	if cap(b) < n {
		return make([]byte, n)
	}
	return b[:n]
}

// bg4Regroup undoes byte grouping (xet-core byte_grouping/bg4.rs): g holds
// four groups laid out one after another, with lengths
// [s+(r>0), s+(r>1), s+(r>2), s] for s = n/4 and r = n%4; byte k of each
// 4-byte word comes from group k.
func bg4Regroup(g, out []byte) {
	n := len(g)
	s, r := n/4, n%4
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
		out[4*i] = g[starts[0]+i]
		out[4*i+1] = g[starts[1]+i]
		out[4*i+2] = g[starts[2]+i]
		out[4*i+3] = g[starts[3]+i]
	}
	for k := range r {
		out[4*s+k] = g[starts[k]+s]
	}
}

// XorbChunk describes one chunk of a stored xorb.
type XorbChunk struct {
	Hash Hash
	// Offset is where the chunk's header starts in the xorb's chunk data.
	Offset uint32
	// CompressedSize is the payload length after the header.
	CompressedSize uint32
	// Size is the uncompressed length.
	Size uint32
}

// End returns the offset just past the chunk's payload.
func (c XorbChunk) End() uint32 { return c.Offset + chunkHeaderSize + c.CompressedSize }

// XorbIndex lists a xorb's chunks: the sidecar index Hangar stores with
// every xorb so shards can be validated without reading chunk data.
type XorbIndex struct {
	Chunks []XorbChunk
	// DataSize is the length of the chunk data, without any footer.
	DataSize int64
}

// RawSize returns the total uncompressed length.
func (x *XorbIndex) RawSize() uint64 {
	var n uint64
	for _, c := range x.Chunks {
		n += uint64(c.Size)
	}
	return n
}

// Leaves returns the chunks as merkle leaves.
func (x *XorbIndex) Leaves() []Chunk {
	out := make([]Chunk, len(x.Chunks))
	for i, c := range x.Chunks {
		out[i] = Chunk{Hash: c.Hash, Size: uint64(c.Size)}
	}
	return out
}

// Hash returns the xorb hash of the indexed chunks.
func (x *XorbIndex) Hash() Hash { return XorbHash(x.Leaves()) }

// ParseXorb reads a serialised xorb (chunk headers and payloads, optionally
// followed by a footer) from r, which must end where the xorb ends. It
// checks every header and the limits, decompresses every chunk to its exact
// declared length, hashes it, and copies the chunk data (without any footer)
// to data when data is not nil. It does not check the xorb hash; compare
// the index's Hash with the expected one.
func ParseXorb(r io.Reader, data io.Writer) (*XorbIndex, error) {
	br := bufio.NewReaderSize(r, 64<<10)
	idx := &XorbIndex{}
	var (
		hdr     [chunkHeaderSize]byte
		payload = make([]byte, MaxCompressedChunkSize)
		raw     = make([]byte, MaxChunkSize)
		dec     decoder
		hasher  = newChunkHasher()
		offset  int64
		total   uint64
	)
	for {
		n, err := io.ReadFull(br, hdr[:])
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%w: truncated chunk header after %d bytes", ErrBadXorb, offset+int64(n))
		}
		h, footer, err := parseChunkHeader(hdr[:])
		if err != nil {
			return nil, fmt.Errorf("%w: chunk %d: %w", ErrBadXorb, len(idx.Chunks), err)
		}
		if footer {
			// The footer is rebuilt from the chunks when needed; discard it.
			if _, err := io.Copy(io.Discard, br); err != nil {
				return nil, err
			}
			break
		}
		if len(idx.Chunks) == MaxXorbChunks {
			return nil, fmt.Errorf("%w: more than %d chunks", ErrBadXorb, MaxXorbChunks)
		}
		p := payload[:h.CompressedSize]
		if _, err := io.ReadFull(br, p); err != nil {
			return nil, fmt.Errorf("%w: chunk %d is truncated", ErrBadXorb, len(idx.Chunks))
		}
		out, err := dec.decompress(h.Scheme, p, h.Size, raw)
		if err != nil {
			return nil, fmt.Errorf("%w: chunk %d: %w", ErrBadXorb, len(idx.Chunks), err)
		}
		total += uint64(h.Size)
		if total > MaxXorbBytes {
			return nil, fmt.Errorf("%w: more than %d uncompressed bytes", ErrBadXorb, MaxXorbBytes)
		}
		idx.Chunks = append(idx.Chunks, XorbChunk{
			Hash: hasher.sum(out), Offset: uint32(offset), CompressedSize: h.CompressedSize, Size: h.Size,
		})
		if data != nil {
			if _, err := data.Write(hdr[:]); err != nil {
				return nil, err
			}
			if _, err := data.Write(p); err != nil {
				return nil, err
			}
		}
		offset += chunkHeaderSize + int64(h.CompressedSize)
	}
	if len(idx.Chunks) == 0 {
		return nil, fmt.Errorf("%w: no chunks", ErrBadXorb)
	}
	idx.DataSize = offset
	return idx, nil
}

// Stored xorb layout. Hangar keeps each xorb as one object under
// xet/xorbs/<hash>: an index header, the index entries, then the chunk
// data exactly as uploaded (without footer). One object keeps the index and
// data consistent, and a ranged read of the first indexReadSize bytes
// returns the whole index.
const (
	indexMagic      = "HXORBIX1"
	indexHeaderSize = 24 // magic, chunk count (u32), reserved (u32), data size (u64)
	indexEntrySize  = 40 // hash, offset (u32), uncompressed size (u32)
	// indexReadSize covers the header and the largest index.
	indexReadSize = indexHeaderSize + MaxXorbChunks*indexEntrySize
)

// MarshalIndex returns the stored index header and entries for idx.
func (x *XorbIndex) MarshalIndex() []byte {
	b := make([]byte, indexHeaderSize+len(x.Chunks)*indexEntrySize)
	copy(b, indexMagic)
	binary.LittleEndian.PutUint32(b[8:], uint32(len(x.Chunks))) //nolint:gosec // at most MaxXorbChunks.
	binary.LittleEndian.PutUint64(b[16:], uint64(x.DataSize))   //nolint:gosec // a non-negative length under MaxXorbSize.
	p := b[indexHeaderSize:]
	for _, c := range x.Chunks {
		copy(p, c.Hash[:])
		binary.LittleEndian.PutUint32(p[32:], c.Offset)
		binary.LittleEndian.PutUint32(p[36:], c.Size)
		p = p[indexEntrySize:]
	}
	return b
}

// dataStart returns where chunk data starts in the stored object.
func (x *XorbIndex) dataStart() int64 { return int64(indexHeaderSize + len(x.Chunks)*indexEntrySize) }

// UnmarshalIndex parses the start of a stored xorb object. b may run past
// the index; it must hold at least the header and every entry.
func UnmarshalIndex(b []byte) (*XorbIndex, error) {
	if len(b) < indexHeaderSize || string(b[:8]) != indexMagic {
		return nil, errors.New("xet: stored xorb has no index header")
	}
	n := int(binary.LittleEndian.Uint32(b[8:]))
	if n == 0 || n > MaxXorbChunks {
		return nil, fmt.Errorf("xet: stored xorb index has %d chunks", n)
	}
	if len(b) < indexHeaderSize+n*indexEntrySize {
		return nil, errors.New("xet: stored xorb index is truncated")
	}
	size := binary.LittleEndian.Uint64(b[16:])
	if size > MaxXorbSize {
		return nil, fmt.Errorf("xet: stored xorb index declares %d bytes of data", size)
	}
	x := &XorbIndex{Chunks: make([]XorbChunk, n), DataSize: int64(size)} //nolint:gosec // checked against MaxXorbSize above.
	p := b[indexHeaderSize:]
	for i := range n {
		c := &x.Chunks[i]
		copy(c.Hash[:], p[:32])
		c.Offset = binary.LittleEndian.Uint32(p[32:])
		c.Size = binary.LittleEndian.Uint32(p[36:])
		p = p[indexEntrySize:]
	}
	if x.Chunks[0].Offset != 0 {
		return nil, errors.New("xet: stored xorb index does not start at offset 0")
	}
	// Compressed sizes follow from the offsets.
	for i := range n {
		end := x.DataSize
		if i+1 < n {
			end = int64(x.Chunks[i+1].Offset)
		}
		cs := end - int64(x.Chunks[i].Offset) - chunkHeaderSize
		if cs <= 0 || cs > MaxCompressedChunkSize {
			return nil, fmt.Errorf("xet: stored xorb index entry %d is inconsistent", i)
		}
		x.Chunks[i].CompressedSize = uint32(cs)
	}
	return x, nil
}
