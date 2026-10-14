package xet

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
)

// Shard format (docs/research/xet-upload-research.md §4; xet-core
// metadata_shard/shard_format.rs, file_structs.rs and xorb_structs.rs). All
// integers are little-endian and every record is 48 bytes.
const (
	shardRecordSize = 48
	// MaxShardSize is the largest shard Hangar accepts (upload-protocol §6).
	MaxShardSize = 64 << 20
	shardVersion = 2
	// File flags.
	FileFlagVerification = 1 << 31
	FileFlagMetadataExt  = 1 << 30
)

// shardHeaderTag is MDB_SHARD_HEADER_TAG.
var shardHeaderTag = [32]byte{
	'H', 'F', 'R', 'e', 'p', 'o', 'M', 'e', 't', 'a', 'D', 'a', 't', 'a', 0, 85, 105, 103, 69, 106, 123,
	129, 87, 131, 165, 189, 217, 92, 205, 209, 74, 169,
}

// bookendHash marks the end of a shard section: 32 bytes of 0xFF.
var bookendHash = Hash{
	0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
	0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
}

// Shard is a parsed upload shard.
type Shard struct {
	// FooterSize is the declared footer size; hf_xet uploads have none.
	FooterSize uint64
	Files      []ShardFile
	// Xorbs lists the xorbs newly created in the upload session.
	Xorbs []ShardXorb
}

// ShardFile is one file block: how to rebuild the file from xorb chunks.
type ShardFile struct {
	Hash  Hash
	Flags uint32
	Terms []Term
	// Verification holds one range hash per term when the verification flag is set.
	Verification []Hash
	// SHA256 is the file's SHA-256 from the metadata extension, or nil.
	// xet-core stores it like every other hash (Sha256::from_be_bytes, then
	// four little-endian words), so its String form is the usual hex digest.
	// docs/research/xet-upload-research.md §4.2 says raw digest bytes; the
	// reference shards and the source both show this form.
	SHA256 *Hash
}

// Term is a range of chunks [Start, End) of one xorb.
type Term struct {
	Xorb          Hash
	Flags         uint32
	UnpackedBytes uint32
	Start, End    uint32
}

// ShardXorb lists the chunks of a xorb uploaded in the session.
type ShardXorb struct {
	Hash        Hash
	Flags       uint32
	Chunks      []ShardChunk
	RawBytes    uint32
	BytesOnDisk uint32
}

// ShardChunk is a chunk entry of a CAS info block.
type ShardChunk struct {
	Hash Hash
	// Offset is the cumulative uncompressed offset of the chunk in the xorb.
	Offset uint32
	Size   uint32
}

// ParseShard parses a shard. It checks the header, the section layout and
// that nothing trails the sections, never allocating more than the input's
// size justifies. Use (*Shard).CheckUpload for the upload rules and
// Ingestor for the checks against stored xorbs.
func ParseShard(b []byte) (*Shard, error) {
	if len(b) > MaxShardSize {
		return nil, fmt.Errorf("%w: shard is %d bytes, over %d", ErrTooLarge, len(b), MaxShardSize)
	}
	p := &shardParser{b: b}
	hdr, err := p.record()
	if err != nil {
		return nil, fmt.Errorf("%w: no header", ErrBadShard)
	}
	if !bytes.Equal(hdr[:32], shardHeaderTag[:]) {
		return nil, fmt.Errorf("%w: not a shard (bad header tag)", ErrBadShard)
	}
	if v := binary.LittleEndian.Uint64(hdr[32:]); v != shardVersion {
		return nil, fmt.Errorf("%w: shard version %d is not %d", ErrBadShard, v, shardVersion)
	}
	sh := &Shard{FooterSize: binary.LittleEndian.Uint64(hdr[40:])}
	if sh.FooterSize > uint64(len(b)) {
		return nil, fmt.Errorf("%w: footer size %d is larger than the shard", ErrBadShard, sh.FooterSize)
	}
	if err := p.files(sh); err != nil {
		return nil, err
	}
	if err := p.xorbs(sh); err != nil {
		return nil, err
	}
	rest := len(b) - p.off
	switch {
	case sh.FooterSize == 0 && rest != 0:
		return nil, fmt.Errorf("%w: %d bytes after the last section", ErrBadShard, rest)
	case sh.FooterSize != 0 && uint64(rest) < sh.FooterSize: //nolint:gosec // rest is a non-negative length.
		return nil, fmt.Errorf("%w: the footer is truncated", ErrBadShard)
	}
	return sh, nil
}

type shardParser struct {
	b   []byte
	off int
}

func (p *shardParser) remaining() int { return len(p.b) - p.off }

func (p *shardParser) record() ([]byte, error) {
	if p.remaining() < shardRecordSize {
		return nil, fmt.Errorf("%w: truncated at byte %d", ErrBadShard, p.off)
	}
	r := p.b[p.off : p.off+shardRecordSize]
	p.off += shardRecordSize
	return r, nil
}

// need checks that n more records fit in the input before anything is
// allocated for them.
func (p *shardParser) need(n uint64, what string) error {
	if n > uint64(p.remaining()/shardRecordSize) { //nolint:gosec // remaining is a non-negative length.
		return fmt.Errorf("%w: %s declares %d entries, more than the shard holds", ErrBadShard, what, n)
	}
	return nil
}

func isBookend(rec []byte) bool { return bytes.Equal(rec[:32], bookendHash[:]) }

func (p *shardParser) files(sh *Shard) error {
	for {
		rec, err := p.record()
		if err != nil {
			return err
		}
		if isBookend(rec) {
			return nil
		}
		var f ShardFile
		copy(f.Hash[:], rec[:32])
		f.Flags = binary.LittleEndian.Uint32(rec[32:])
		n := uint64(binary.LittleEndian.Uint32(rec[36:]))
		records := n
		if f.Flags&FileFlagVerification != 0 {
			records += n
		}
		if f.Flags&FileFlagMetadataExt != 0 {
			records++
		}
		if err := p.need(records, "a file block"); err != nil {
			return err
		}
		f.Terms = make([]Term, n)
		for i := range f.Terms {
			e, _ := p.record()
			t := &f.Terms[i]
			copy(t.Xorb[:], e[:32])
			t.Flags = binary.LittleEndian.Uint32(e[32:])
			t.UnpackedBytes = binary.LittleEndian.Uint32(e[36:])
			t.Start = binary.LittleEndian.Uint32(e[40:])
			t.End = binary.LittleEndian.Uint32(e[44:])
		}
		if f.Flags&FileFlagVerification != 0 {
			f.Verification = make([]Hash, n)
			for i := range f.Verification {
				e, _ := p.record()
				copy(f.Verification[i][:], e[:32])
			}
		}
		if f.Flags&FileFlagMetadataExt != 0 {
			e, _ := p.record()
			var sum Hash
			copy(sum[:], e[:32])
			f.SHA256 = &sum
		}
		sh.Files = append(sh.Files, f)
	}
}

func (p *shardParser) xorbs(sh *Shard) error {
	for {
		rec, err := p.record()
		if err != nil {
			return err
		}
		if isBookend(rec) {
			return nil
		}
		var x ShardXorb
		copy(x.Hash[:], rec[:32])
		x.Flags = binary.LittleEndian.Uint32(rec[32:])
		n := uint64(binary.LittleEndian.Uint32(rec[36:]))
		x.RawBytes = binary.LittleEndian.Uint32(rec[40:])
		x.BytesOnDisk = binary.LittleEndian.Uint32(rec[44:])
		if err := p.need(n, "a CAS info block"); err != nil {
			return err
		}
		x.Chunks = make([]ShardChunk, n)
		for i := range x.Chunks {
			e, _ := p.record()
			c := &x.Chunks[i]
			copy(c.Hash[:], e[:32])
			c.Offset = binary.LittleEndian.Uint32(e[32:])
			c.Size = binary.LittleEndian.Uint32(e[36:])
		}
		sh.Xorbs = append(sh.Xorbs, x)
	}
}

// CheckUpload applies the rules for uploaded shards that need no stored
// data: every file has verification entries and the metadata extension
// with its SHA-256 (required for model and dataset Repos), every term is a
// non-empty range that fits in a xorb, and an empty file has the zero
// hash. CheckLimits bounds the work the shard describes.
func (s *Shard) CheckUpload() error {
	for i, f := range s.Files {
		if f.Flags&FileFlagVerification == 0 {
			return fmt.Errorf("%w: file %d (%s) has no verification entries", ErrBadShard, i, f.Hash)
		}
		if f.SHA256 == nil {
			return fmt.Errorf("%w: file %d (%s) has no SHA-256 metadata", ErrBadShard, i, f.Hash)
		}
		if len(f.Terms) == 0 && !f.Hash.IsZero() {
			return fmt.Errorf("%w: file %d (%s) has no terms", ErrBadShard, i, f.Hash)
		}
		for j, t := range f.Terms {
			if t.Start >= t.End {
				return fmt.Errorf("%w: file %d term %d has an empty chunk range", ErrBadShard, i, j)
			}
			if t.End > MaxXorbChunks {
				return fmt.Errorf("%w: file %d term %d ends at chunk %d, but a xorb has at most %d chunks",
					ErrBadShard, i, j, t.End, MaxXorbChunks)
			}
		}
	}
	for i, x := range s.Xorbs {
		if len(x.Chunks) == 0 {
			return fmt.Errorf("%w: CAS info block %d (%s) has no chunks", ErrBadShard, i, x.Hash)
		}
	}
	return nil
}

// XorbHashes returns every xorb the shard refers to, in terms or CAS info,
// once each, in first-seen order.
func (s *Shard) XorbHashes() []Hash {
	seen := map[Hash]bool{}
	var out []Hash
	add := func(h Hash) {
		if !seen[h] {
			seen[h] = true
			out = append(out, h)
		}
	}
	for _, f := range s.Files {
		for _, t := range f.Terms {
			add(t.Xorb)
		}
	}
	for _, x := range s.Xorbs {
		add(x.Hash)
	}
	return out
}

// Size returns the file's length: the sum of its terms' unpacked bytes.
func (f *ShardFile) Size() int64 {
	var n int64
	for _, t := range f.Terms {
		n += int64(t.UnpackedBytes)
	}
	return n
}

// errNoIndex is returned by CheckFile when a xorb's index is missing.
var errNoIndex = errors.New("xet: xorb index missing")

// IndexFunc returns the stored index of a xorb. A nil index with a nil
// error means the xorb has none.
type IndexFunc func(Hash) (*XorbIndex, error)

// CheckFile checks a file block against the stored xorb indexes: every
// term's range fits its xorb, its unpacked bytes and verification hash
// match the chunks, and the file hash matches all the chunks in order. It
// reads no chunk data, and its memory does not grow with the number of
// chunks the file names.
func CheckFile(f *ShardFile, index IndexFunc) error {
	var tree merkleBuilder
	rh := newRangeHasher()
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
		var size uint64
		rh.reset()
		for _, c := range x.Chunks[t.Start:t.End] {
			size += uint64(c.Size)
			rh.add(c.Hash)
			tree.add(Chunk{Hash: c.Hash, Size: uint64(c.Size)})
		}
		if size != uint64(t.UnpackedBytes) {
			return fmt.Errorf("%w: file %s term %d declares %d bytes, its chunks hold %d",
				ErrBadShard, f.Hash, j, t.UnpackedBytes, size)
		}
		if len(f.Verification) > j && rh.sum() != f.Verification[j] {
			return fmt.Errorf("%w: file %s term %d has the wrong verification hash", ErrBadShard, f.Hash, j)
		}
	}
	if got := tree.fileHash(); got != f.Hash {
		return fmt.Errorf("%w: file hash %s does not match its chunks (%s)", ErrBadShard, f.Hash, got)
	}
	return nil
}

// CheckXorb checks a CAS info block against the stored index of the xorb
// it names: the same chunks, hashes, offsets and lengths.
func CheckXorb(x *ShardXorb, idx *XorbIndex) error {
	if len(x.Chunks) != len(idx.Chunks) {
		return fmt.Errorf("%w: CAS info for xorb %s lists %d chunks, the xorb has %d",
			ErrBadShard, x.Hash, len(x.Chunks), len(idx.Chunks))
	}
	var off uint64
	for i, c := range x.Chunks {
		s := idx.Chunks[i]
		if c.Hash != s.Hash || c.Size != s.Size || uint64(c.Offset) != off {
			return fmt.Errorf("%w: CAS info for xorb %s does not match chunk %d", ErrBadShard, x.Hash, i)
		}
		off += uint64(s.Size)
	}
	if uint64(x.RawBytes) != off {
		return fmt.Errorf("%w: CAS info for xorb %s declares %d bytes, the xorb has %d",
			ErrBadShard, x.Hash, x.RawBytes, off)
	}
	return nil
}

// MarshalShard serialises a shard without footer, as hf_xet uploads it,
// ignoring FooterSize.
func MarshalShard(s *Shard) []byte {
	var b bytes.Buffer
	rec := make([]byte, shardRecordSize)
	put := func(fill func(r []byte)) {
		clear(rec)
		fill(rec)
		b.Write(rec)
	}
	put(func(r []byte) {
		copy(r, shardHeaderTag[:])
		binary.LittleEndian.PutUint64(r[32:], shardVersion)
		// footer_size: MarshalShard never writes a footer.
	})
	for _, f := range s.Files {
		put(func(r []byte) {
			copy(r, f.Hash[:])
			binary.LittleEndian.PutUint32(r[32:], f.Flags)
			binary.LittleEndian.PutUint32(r[36:], uint32(len(f.Terms))) //nolint:gosec // parsed counts fit in u32.
		})
		for _, t := range f.Terms {
			put(func(r []byte) {
				copy(r, t.Xorb[:])
				binary.LittleEndian.PutUint32(r[32:], t.Flags)
				binary.LittleEndian.PutUint32(r[36:], t.UnpackedBytes)
				binary.LittleEndian.PutUint32(r[40:], t.Start)
				binary.LittleEndian.PutUint32(r[44:], t.End)
			})
		}
		if f.Flags&FileFlagVerification != 0 {
			for j := range f.Terms {
				var h Hash
				if j < len(f.Verification) {
					h = f.Verification[j]
				}
				put(func(r []byte) { copy(r, h[:]) })
			}
		}
		if f.Flags&FileFlagMetadataExt != 0 {
			put(func(r []byte) {
				if f.SHA256 != nil {
					copy(r, f.SHA256[:])
				}
			})
		}
	}
	put(func(r []byte) { copy(r, bookendHash[:]) })
	for _, x := range s.Xorbs {
		put(func(r []byte) {
			copy(r, x.Hash[:])
			binary.LittleEndian.PutUint32(r[32:], x.Flags)
			binary.LittleEndian.PutUint32(r[36:], uint32(len(x.Chunks))) //nolint:gosec // parsed counts fit in u32.
			binary.LittleEndian.PutUint32(r[40:], x.RawBytes)
			binary.LittleEndian.PutUint32(r[44:], x.BytesOnDisk)
		})
		for _, c := range x.Chunks {
			put(func(r []byte) {
				copy(r, c.Hash[:])
				binary.LittleEndian.PutUint32(r[32:], c.Offset)
				binary.LittleEndian.PutUint32(r[36:], c.Size)
			})
		}
	}
	put(func(r []byte) { copy(r, bookendHash[:]) })
	return b.Bytes()
}
