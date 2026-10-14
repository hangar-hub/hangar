package pq

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/bits"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/snappy"
	"github.com/klauspost/compress/zstd"
	"github.com/parquet-go/parquet-go/encoding/thrift"
	"github.com/parquet-go/parquet-go/format"
	"github.com/pierrec/lz4/v4"
)

// parquet-go trusts the sizes and counts it reads from a file: it allocates
// a footer, a list, a page or a page's levels as large as the file claims,
// and its LZ4 decoder doubles its buffer until a corrupt block decodes. A
// corrupt file of a few hundred bytes can make it allocate gigabytes. The
// checks here run before parquet-go reads the footer or a column chunk and
// reject any size the file cannot back.

const (
	// trailerSize is the footer length and the closing magic number.
	trailerSize = 8
	// maxThriftDepth bounds the nesting of Thrift structures. The deepest
	// Parquet metadata nests about 8 levels.
	maxThriftDepth = 32
	// maxPageHeaderSize is the largest page header read, as in Arrow.
	maxPageHeaderSize = 16 << 20
	// maxPageValues bounds the values of one page. parquet-go allocates
	// levels and offsets for every value before decoding, and run-length
	// encoding can claim millions of values in a few bytes. Writers end
	// pages long before this (pyarrow at 20,000 rows).
	maxPageValues = 1 << 24
	// maxPageBytes bounds the uncompressed size of one page, which
	// parquet-go allocates in one piece.
	maxPageBytes = 1 << 30
)

// readAt fills b from off. Unlike io.ReaderAt, it accepts io.EOF with a full
// read.
func readAt(r io.ReaderAt, b []byte, off int64) error {
	n, err := r.ReadAt(b, off)
	if n == len(b) {
		return nil
	}
	if err == nil {
		err = io.ErrUnexpectedEOF
	}
	return err
}

// errShort means a Thrift structure runs past the bytes read so far.
var errShort = errors.New("parquet: short thrift structure")

// checkTrailer checks the closing magic number and that the footer fits in
// the file, and returns the footer length.
func checkTrailer(r io.ReaderAt, size int64) (int64, error) {
	if size < 4+trailerSize {
		return 0, fmt.Errorf("%w: %d bytes is too short", ErrInvalid, size)
	}
	var b [trailerSize]byte
	if err := readAt(r, b[:], size-trailerSize); err != nil {
		return 0, fmt.Errorf("%w: read the footer: %w", ErrInvalid, err)
	}
	if string(b[4:]) != "PAR1" {
		return 0, fmt.Errorf("%w: no closing magic number", ErrInvalid)
	}
	n := int64(binary.LittleEndian.Uint32(b[:4]))
	if n > size-4-trailerSize {
		return 0, fmt.Errorf("%w: footer of %d bytes in a file of %d", ErrInvalid, n, size)
	}
	return n, nil
}

// checkFooter checks the footer's Thrift structure, so every list and
// string it declares fits in the footer.
func checkFooter(r io.ReaderAt, size int64) error {
	n, err := checkTrailer(r, size)
	if err != nil {
		return err
	}
	b := make([]byte, n)
	if err := readAt(r, b, size-trailerSize-n); err != nil {
		return fmt.Errorf("%w: read the footer: %w", ErrInvalid, err)
	}
	if _, err := thriftStructLen(b); err != nil {
		return fmt.Errorf("%w: footer: %w", ErrInvalid, err)
	}
	return nil
}

// thriftStructLen returns the length of the compact-protocol struct at the
// start of b, checking that every length and count fits in b.
func thriftStructLen(b []byte) (int, error) {
	p := thriftParser{b: b}
	if err := p.structure(0); err != nil {
		return 0, err
	}
	return p.pos, nil
}

type thriftParser struct {
	b   []byte
	pos int
}

func (p *thriftParser) byte() (byte, error) {
	if p.pos >= len(p.b) {
		return 0, errShort
	}
	c := p.b[p.pos]
	p.pos++
	return c, nil
}

func (p *thriftParser) uvarint() (uint64, error) {
	v, n := binary.Uvarint(p.b[p.pos:])
	switch {
	case n == 0:
		return 0, errShort
	case n < 0:
		return 0, errors.New("varint overflows 64 bits")
	}
	p.pos += n
	return v, nil
}

// need checks that n more items of at least size bytes each fit.
func (p *thriftParser) need(n uint64, size int) error {
	if n > uint64(max(len(p.b)-p.pos, 0))/uint64(max(size, 1)) {
		return errShort
	}
	return nil
}

func (p *thriftParser) structure(depth int) error {
	if depth > maxThriftDepth {
		return errors.New("thrift structures nest too deeply")
	}
	for {
		h, err := p.byte()
		if err != nil {
			return err
		}
		typ := h & 0x0f
		if typ == 0 {
			return nil
		}
		if h>>4 == 0 {
			// The field ID follows as a zigzag varint.
			if _, err := p.uvarint(); err != nil {
				return err
			}
		}
		if err := p.value(typ, depth); err != nil {
			return err
		}
	}
}

// value skips one value of a compact-protocol type.
func (p *thriftParser) value(typ byte, depth int) error {
	switch typ {
	case 1, 2: // a boolean field, held in its type
		return nil
	case 3: // byte
		_, err := p.byte()
		return err
	case 4, 5, 6: // i16, i32, i64
		_, err := p.uvarint()
		return err
	case 7: // double
		if err := p.need(1, 8); err != nil {
			return err
		}
		p.pos += 8
		return nil
	case 8: // binary
		n, err := p.uvarint()
		if err != nil {
			return err
		}
		if err := p.need(n, 1); err != nil {
			return err
		}
		p.pos += int(n) //nolint:gosec // need checked n fits in the slice
		return nil
	case 9, 10: // list, set
		h, err := p.byte()
		if err != nil {
			return err
		}
		n := uint64(h >> 4)
		if n == 15 {
			if n, err = p.uvarint(); err != nil {
				return err
			}
		}
		elem := h & 0x0f
		if elem == 1 || elem == 2 {
			// List elements of type bool take a byte each.
			elem = 3
		}
		if err := p.need(n, 1); err != nil {
			return err
		}
		for range n {
			if err := p.value(elem, depth+1); err != nil {
				return err
			}
		}
		return nil
	case 11: // map
		n, err := p.uvarint()
		if err != nil || n == 0 {
			return err
		}
		kv, err := p.byte()
		if err != nil {
			return err
		}
		if err := p.need(n, 2); err != nil {
			return err
		}
		key, val := kv>>4, kv&0x0f
		for range n {
			if err := p.value(key, depth+1); err != nil {
				return err
			}
			if err := p.value(val, depth+1); err != nil {
				return err
			}
		}
		return nil
	case 12: // struct
		return p.structure(depth + 1)
	}
	return fmt.Errorf("unknown thrift type %d", typ)
}

// checkChunk checks the pages of a column chunk before parquet-go reads
// it, once per chunk.
func (f *File) checkChunk(rowGroup, leaf int) error {
	key := [2]int{rowGroup, leaf}
	f.mu.Lock()
	err, ok := f.checked[key]
	f.mu.Unlock()
	if ok {
		return err
	}
	err = f.walkChunk(rowGroup, leaf)
	f.mu.Lock()
	f.checked[key] = err
	f.mu.Unlock()
	return err
}

func (f *File) walkChunk(rowGroup, leaf int) error {
	groups := f.f.Metadata().RowGroups
	if rowGroup >= len(groups) || leaf >= len(groups[rowGroup].Columns) || leaf >= len(groups[0].Columns) {
		return fmt.Errorf("%w: missing column chunk", ErrInvalid)
	}
	if leaf >= len(f.leaves) {
		return fmt.Errorf("%w: column chunk %d has no column", ErrInvalid, leaf)
	}
	m := groups[rowGroup].Columns[leaf].MetaData
	// parquet-go decompresses every chunk of a column with the codec of
	// the first chunk.
	pages := pageChecker{codec: groups[0].Columns[leaf].MetaData.Codec, chunkValues: m.NumValues, chunkBytes: m.TotalUncompressedSize,
		maxDef: f.leaves[leaf].maxDef, maxRep: f.leaves[leaf].maxRep}
	defer pages.close()
	start := m.DataPageOffset
	if m.DictionaryPageOffset != 0 {
		start = m.DictionaryPageOffset
	}
	end := start + m.TotalCompressedSize
	if start < 0 || m.TotalCompressedSize < 0 || end < start || end > f.size || m.NumValues < 0 {
		return fmt.Errorf("%w: column chunk %d of row group %d is outside the file", ErrInvalid, leaf, rowGroup)
	}
	for pos := start; pos < end; {
		header, n, err := f.pageHeader(pos, end)
		if err != nil {
			return err
		}
		data := pos + int64(n)
		if header.CompressedPageSize < 0 || data+int64(header.CompressedPageSize) > end {
			return fmt.Errorf("%w: page at %d runs past its column chunk", ErrInvalid, pos)
		}
		if err := pages.check(header, func(off, n int64) ([]byte, error) {
			b := make([]byte, n)
			return b, readAt(f.r, b, data+off)
		}); err != nil {
			return fmt.Errorf("page at %d: %w", pos, err)
		}
		pos = data + int64(header.CompressedPageSize)
	}
	return nil
}

// pageHeader reads the page header at pos, reading more of the file only
// as far as the header needs.
func (f *File) pageHeader(pos, end int64) (*format.PageHeader, int, error) {
	limit := min(end-pos, maxPageHeaderSize)
	for window := min(limit, 4096); ; window = min(window*8, limit) {
		b := make([]byte, window)
		if err := readAt(f.r, b, pos); err != nil {
			return nil, 0, fmt.Errorf("%w: read the page header at %d: %w", ErrInvalid, pos, err)
		}
		n, err := thriftStructLen(b)
		if errors.Is(err, errShort) && window < limit {
			continue
		}
		if err != nil {
			return nil, 0, fmt.Errorf("%w: page header at %d: %w", ErrInvalid, pos, err)
		}
		header := new(format.PageHeader)
		if err := thrift.Unmarshal(new(thrift.CompactProtocol), b[:n], header); err != nil {
			return nil, 0, fmt.Errorf("%w: page header at %d: %w", ErrInvalid, pos, err)
		}
		return header, n, nil
	}
}

// pageChecker checks the pages of one column chunk.
type pageChecker struct {
	codec          format.CompressionCodec
	chunkValues    int64
	chunkBytes     int64
	maxDef, maxRep int
	zstd           *zstd.Decoder
}

func (c *pageChecker) close() {
	if c.zstd != nil {
		c.zstd.Close()
	}
}

// check checks one page: its header against the column chunk, that its
// compressed data decodes to exactly the size the header gives (which
// parquet-go allocates before decoding), and that the run-length and delta
// headers in a data page claim no more values than the page holds (parquet-go
// allocates what they claim). read reads n bytes of the page's data from off.
func (c *pageChecker) check(h *format.PageHeader, read func(off, n int64) ([]byte, error)) error {
	if h.UncompressedPageSize < 0 || int64(h.UncompressedPageSize) > c.chunkBytes {
		return fmt.Errorf("%w: uncompressed size %d in a column chunk of %d bytes", ErrInvalid, h.UncompressedPageSize, c.chunkBytes)
	}
	if h.UncompressedPageSize > maxPageBytes {
		return fmt.Errorf("%w: a page of %d bytes is more than %d", ErrUnsupported, h.UncompressedPageSize, maxPageBytes)
	}
	values := func(n int32) error {
		switch {
		case n < 0 || int64(n) > c.chunkValues:
			return fmt.Errorf("%w: %d values in a column chunk of %d", ErrInvalid, n, c.chunkValues)
		case n > maxPageValues:
			return fmt.Errorf("%w: a page of %d values is more than %d", ErrUnsupported, n, maxPageValues)
		}
		return nil
	}
	if err := c.dictionary(h, read); err != nil {
		return err
	}
	compressed := c.codec != format.Uncompressed
	skip, size := int64(0), int64(h.UncompressedPageSize)
	switch h.Type {
	case format.DataPage:
		v := h.DataPageHeader
		if !v.Valid {
			return nil
		}
		if err := values(v.V.NumValues); err != nil {
			return err
		}
		body, err := c.body(read, 0, int64(h.CompressedPageSize), size, compressed)
		if err != nil {
			return err
		}
		n := int64(v.V.NumValues)
		off := 0
		for _, l := range []struct {
			max int
			enc format.Encoding
		}{{c.maxRep, v.V.RepetitionLevelEncoding}, {c.maxDef, v.V.DefinitionLevelEncoding}} {
			if l.max == 0 {
				continue
			}
			k, err := levelsV1(body[off:], l.enc, bits.Len(uint(l.max)), n)
			if err != nil {
				return err
			}
			off += k
		}
		return checkValues(body[off:], v.V.Encoding, n)
	case format.DataPageV2:
		v := h.DataPageHeaderV2
		if !v.Valid {
			return nil
		}
		if err := values(v.V.NumValues); err != nil {
			return err
		}
		if v.V.NumNulls < 0 || v.V.NumNulls > v.V.NumValues || v.V.NumRows < 0 || v.V.NumRows > v.V.NumValues {
			return fmt.Errorf("%w: %d nulls and %d rows in %d values", ErrInvalid, v.V.NumNulls, v.V.NumRows, v.V.NumValues)
		}
		// The levels come first and are never compressed.
		rep, def := int64(v.V.RepetitionLevelsByteLength), int64(v.V.DefinitionLevelsByteLength)
		if rep < 0 || def < 0 || rep+def > int64(h.CompressedPageSize) || rep+def > size {
			return fmt.Errorf("%w: levels of %d bytes in a page of %d", ErrInvalid, rep+def, h.CompressedPageSize)
		}
		skip, size = rep+def, size-rep-def
		if v.V.IsCompressed.Valid && !v.V.IsCompressed.V {
			compressed = false
		}
		n := int64(v.V.NumValues)
		levels, err := read(0, skip)
		if err != nil {
			return fmt.Errorf("%w: %w", ErrInvalid, err)
		}
		if err := runs(levels[:rep], bits.Len(uint(c.maxRep)), n); err != nil {
			return err
		}
		if err := runs(levels[rep:], bits.Len(uint(c.maxDef)), n); err != nil {
			return err
		}
		body, err := c.body(read, skip, int64(h.CompressedPageSize)-skip, size, compressed)
		if err != nil {
			return err
		}
		return checkValues(body, v.V.Encoding, n-int64(v.V.NumNulls))
	case format.DictionaryPage, format.IndexPage:
		return nil
	}
	return fmt.Errorf("%w: page of type %d", ErrInvalid, h.Type)
}

// dictionary checks a page's dictionary header. parquet-go reads the first
// page of a chunk as its dictionary whenever that header is set, whatever
// the page's type.
func (c *pageChecker) dictionary(h *format.PageHeader, read func(off, n int64) ([]byte, error)) error {
	v := h.DictionaryPageHeader
	if !v.Valid {
		return nil
	}
	size := int64(h.UncompressedPageSize)
	// Every dictionary entry takes at least a bit.
	switch n := v.V.NumValues; {
	case n < 0 || int64(n) > 8*size+8:
		return fmt.Errorf("%w: dictionary of %d values in %d bytes", ErrInvalid, n, size)
	case n > maxPageValues:
		return fmt.Errorf("%w: a dictionary of %d values is more than %d", ErrUnsupported, n, maxPageValues)
	}
	_, err := c.body(read, 0, int64(h.CompressedPageSize), size, c.codec != format.Uncompressed)
	return err
}

// body returns n bytes of page data from off, decompressed to size bytes
// when compressed is true.
func (c *pageChecker) body(read func(off, n int64) ([]byte, error), off, n, size int64, compressed bool) ([]byte, error) {
	src, err := read(off, n)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if !compressed {
		return src, nil
	}
	// The most each codec can expand its input; the others are decoded
	// with a limit.
	ratio := map[format.CompressionCodec]int64{format.Snappy: 24, format.Lz4Raw: 256, format.Gzip: 1040}
	if r, ok := ratio[c.codec]; ok && size > r*n+1024 {
		return nil, fmt.Errorf("%w: %d bytes of %s data cannot decode to %d", ErrInvalid, n, c.codec, size)
	}
	out, err := c.decompress(src, size)
	if err != nil {
		return nil, fmt.Errorf("%w: %s data: %w", ErrInvalid, c.codec, err)
	}
	if int64(len(out)) != size {
		return nil, fmt.Errorf("%w: %s data decodes to %d bytes, not %d", ErrInvalid, c.codec, len(out), size)
	}
	return out, nil
}

// decompress decodes src, reading at most one byte past size.
func (c *pageChecker) decompress(src []byte, size int64) ([]byte, error) {
	switch c.codec {
	case format.Snappy:
		n, err := snappy.DecodedLen(src)
		if err != nil {
			return nil, err
		}
		if int64(n) != size {
			return nil, fmt.Errorf("decodes to %d bytes, not %d", n, size)
		}
		return snappy.Decode(make([]byte, n), src)
	case format.Lz4Raw:
		out := make([]byte, size)
		n, err := lz4.UncompressBlock(src, out)
		return out[:n], err
	case format.Zstd:
		if c.zstd == nil {
			d, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1))
			if err != nil {
				return nil, err
			}
			c.zstd = d
		}
		// The decoder counts a frame's window, at least 1 KB, against the
		// limit.
		if err := c.zstd.ResetWithOptions(nil, zstd.WithDecoderMaxMemory(uint64(max(size, zstd.MinWindowSize)))); err != nil {
			return nil, err
		}
		return c.zstd.DecodeAll(src, nil)
	case format.Gzip:
		r, err := gzip.NewReader(bytes.NewReader(src))
		if err != nil {
			return nil, err
		}
		return io.ReadAll(io.LimitReader(r, size+1))
	case format.Brotli:
		return io.ReadAll(io.LimitReader(brotli.NewReader(bytes.NewReader(src)), size+1))
	}
	// parquet-go has no decoder for the other codecs and fails on them.
	return nil, fmt.Errorf("%w: compression codec %s", ErrUnsupported, c.codec)
}

// levelsV1 checks the levels at the start of a v1 data page and returns
// their length.
func levelsV1(b []byte, enc format.Encoding, width int, n int64) (int, error) {
	switch enc {
	case format.RLE:
		if len(b) < 4 {
			return 0, fmt.Errorf("%w: levels run past the page", ErrInvalid)
		}
		k := int64(binary.LittleEndian.Uint32(b))
		if k > int64(len(b)-4) {
			return 0, fmt.Errorf("%w: levels of %d bytes run past the page", ErrInvalid, k)
		}
		return 4 + int(k), runs(b[4:4+k], width, n)
	case format.BitPacked:
		k := (n*int64(width) + 7) / 8
		if k > int64(len(b)) {
			return 0, fmt.Errorf("%w: levels of %d bytes run past the page", ErrInvalid, k)
		}
		return int(k), nil
	}
	return 0, fmt.Errorf("%w: levels encoded as %s", ErrUnsupported, enc)
}

// checkValues checks the run-length or delta header of n values.
func checkValues(b []byte, enc format.Encoding, n int64) error {
	switch enc {
	case format.PlainDictionary, format.RLEDictionary: //nolint:staticcheck // older writers use PLAIN_DICTIONARY in data pages

		if len(b) == 0 {
			return nil
		}
		if b[0] > 32 {
			return fmt.Errorf("%w: dictionary indexes of %d bits", ErrInvalid, b[0])
		}
		return runs(b[1:], int(b[0]), n)
	case format.RLE:
		// Booleans, with a length first.
		if len(b) < 4 {
			return nil
		}
		k := min(int64(binary.LittleEndian.Uint32(b)), int64(len(b)-4))
		return runs(b[4:4+k], 1, n)
	case format.DeltaBinaryPacked, format.DeltaLengthByteArray, format.DeltaByteArray:
		if len(b) == 0 {
			return nil
		}
		// Each starts with a binary-packed header: block size, miniblocks
		// per block, then the number of values, which parquet-go allocates.
		p := thriftParser{b: b}
		var total uint64
		for range 3 {
			v, err := p.uvarint()
			if err != nil {
				return fmt.Errorf("%w: delta header: %w", ErrInvalid, err)
			}
			total = v
		}
		if total > uint64(max(n, 0)) {
			return fmt.Errorf("%w: delta header of %d values in a page of %d", ErrInvalid, total, n)
		}
	}
	return nil
}

// runs checks a run-length and bit-packed hybrid stream of values of width
// bits, which must hold no more than n values (rounded up to a group of
// eight).
func runs(b []byte, width int, n int64) error {
	limit := uint64(max(n, 0)) + 8
	bitWidth := uint64(max(width, 0))
	valueBytes := (bitWidth + 7) / 8
	var total uint64
	for len(b) > 0 {
		h, k := binary.Uvarint(b)
		if k <= 0 {
			return fmt.Errorf("%w: run header", ErrInvalid)
		}
		b = b[k:]
		count, size := h>>1, valueBytes
		if h&1 == 1 {
			// Bit-packed groups of eight values.
			if count > limit {
				return fmt.Errorf("%w: run of %d groups in a page of %d values", ErrInvalid, count, n)
			}
			count, size = count*8, count*bitWidth
		}
		if total += count; count > limit || total > limit {
			return fmt.Errorf("%w: runs of more than %d values", ErrInvalid, n)
		}
		if size >= uint64(len(b)) {
			// parquet-go stops at the end of the data.
			return nil
		}
		b = b[size:]
	}
	return nil
}
