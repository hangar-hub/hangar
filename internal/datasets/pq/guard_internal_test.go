package pq

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"strings"
	"testing"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/snappy"
	"github.com/klauspost/compress/zstd"
	"github.com/parquet-go/parquet-go/encoding/thrift"
	"github.com/parquet-go/parquet-go/format"
	"github.com/pierrec/lz4/v4"
	"github.com/stretchr/testify/require"
)

func TestThriftStructLen(t *testing.T) {
	h := format.PageHeader{Type: format.DataPage, UncompressedPageSize: 10, CompressedPageSize: 7, CRC: 3}
	h.DataPageHeader.Valid = true
	h.DataPageHeader.V.NumValues = 2
	h.DataPageHeader.V.Statistics.MaxValue = []byte("max")
	b, err := thrift.Marshal(new(thrift.CompactProtocol), &h)
	require.NoError(t, err)
	n, err := thriftStructLen(append(b, 0xff, 0xff))
	require.NoError(t, err)
	require.Equal(t, len(b), n)
	for i := range len(b) - 1 {
		_, err := thriftStructLen(b[:i])
		require.ErrorIs(t, err, errShort, "truncated at %d", i)
	}

	for name, c := range map[string]struct {
		b    []byte
		want string
	}{
		// Field 1, binary, claiming 2^31 bytes.
		"binary":        {[]byte{0x18, 0x80, 0x80, 0x80, 0x80, 0x08}, errShort.Error()},
		"list":          {[]byte{0x19, 0xf5, 0x80, 0x80, 0x80, 0x80, 0x08}, errShort.Error()},
		"set":           {[]byte{0x1a, 0xf5, 0x80, 0x80, 0x80, 0x80, 0x08}, errShort.Error()},
		"map":           {[]byte{0x1b, 0x80, 0x80, 0x80, 0x80, 0x08, 0x55}, errShort.Error()},
		"double":        {[]byte{0x17, 1, 2, 3}, errShort.Error()},
		"unknown type":  {[]byte{0x1e, 0}, "unknown thrift type 14"},
		"varint":        {[]byte{0x15, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x01}, "overflows"},
		"long field ID": {[]byte{0x05, 0xff}, errShort.Error()},
		"deep":          {append(bytes.Repeat([]byte{0x1c}, maxThriftDepth+2), 0), "nest too deeply"},
	} {
		_, err := thriftStructLen(c.b)
		require.ErrorContains(t, err, c.want, name)
	}
	// Lists of booleans, a map, a byte and a double, then the end.
	ok := []byte{0x19, 0x21, 0x01, 0x02, 0x1b, 0x01, 0x55, 0x02, 0x04, 0x13, 0x07, 0x17, 1, 2, 3, 4, 5, 6, 7, 8, 0x1b, 0x00, 0x00}
	n, err = thriftStructLen(ok)
	require.NoError(t, err)
	require.Equal(t, len(ok), n)
}

func TestCheckTrailer(t *testing.T) {
	file := func(footer uint32, magic string) []byte {
		b := append([]byte("PAR1"), make([]byte, 20)...)
		b = binary.LittleEndian.AppendUint32(b, footer)
		return append(b, magic...)
	}
	n, err := checkTrailer(bytes.NewReader(file(20, "PAR1")), 32)
	require.NoError(t, err)
	require.EqualValues(t, 20, n)
	for _, c := range []struct {
		b    []byte
		want string
	}{
		{file(21, "PAR1"), "footer of 21 bytes"},
		{file(1<<31, "PAR1"), "footer of 2147483648 bytes"},
		{file(4, "PARE"), "magic"},
		{[]byte("PAR1PAR1"), "too short"},
	} {
		_, err := checkTrailer(bytes.NewReader(c.b), int64(len(c.b)))
		require.ErrorIs(t, err, ErrInvalid)
		require.ErrorContains(t, err, c.want)
	}
	// A reader that ends early.
	_, err = checkTrailer(bytes.NewReader(file(4, "PAR1")[:20]), 32)
	require.ErrorIs(t, err, ErrInvalid)
	// A footer whose first field is a list of 2^31 items.
	b := file(20, "PAR1")
	copy(b[4:], []byte{0x19, 0xf5, 0x80, 0x80, 0x80, 0x80, 0x08})
	err = checkFooter(bytes.NewReader(b), int64(len(b)))
	require.ErrorIs(t, err, ErrInvalid)
	require.ErrorContains(t, err, "footer")
	require.NoError(t, checkFooter(bytes.NewReader(file(20, "PAR1")), 32))
	require.Error(t, checkFooter(bytes.NewReader(file(21, "PAR1")), 32))
}

// dataPage returns a v1 data page header of n values whose uncompressed
// body is size bytes, compressed to csize.
func dataPage(n, size, csize int32) *format.PageHeader {
	h := &format.PageHeader{Type: format.DataPage, UncompressedPageSize: size, CompressedPageSize: csize}
	h.DataPageHeader.Valid = true
	h.DataPageHeader.V.NumValues = n
	h.DataPageHeader.V.Encoding = format.Plain
	h.DataPageHeader.V.DefinitionLevelEncoding = format.RLE
	h.DataPageHeader.V.RepetitionLevelEncoding = format.RLE
	return h
}

func reader(b []byte) func(off, n int64) ([]byte, error) {
	return func(off, n int64) ([]byte, error) {
		if off+n > int64(len(b)) {
			return nil, bytes.ErrTooLarge
		}
		return b[off : off+n], nil
	}
}

func compressWith(t *testing.T, codec format.CompressionCodec, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	switch codec {
	case format.Snappy:
		return snappy.Encode(nil, b)
	case format.Lz4Raw:
		out := make([]byte, lz4.CompressBlockBound(len(b)))
		n, err := lz4.CompressBlock(b, out, nil)
		require.NoError(t, err)
		return out[:n]
	case format.Zstd:
		e, err := zstd.NewWriter(nil)
		require.NoError(t, err)
		defer e.Close()
		return e.EncodeAll(b, nil)
	case format.Gzip:
		w := gzip.NewWriter(&buf)
		_, err := w.Write(b)
		require.NoError(t, err)
		require.NoError(t, w.Close())
	case format.Brotli:
		w := brotli.NewWriter(&buf)
		_, err := w.Write(b)
		require.NoError(t, err)
		require.NoError(t, w.Close())
	}
	return buf.Bytes()
}

func TestPageCheckerCodecs(t *testing.T) {
	// A plain page of 1,000 zero int64s without levels.
	body := make([]byte, 8000)
	for _, codec := range []format.CompressionCodec{format.Snappy, format.Lz4Raw, format.Zstd, format.Gzip, format.Brotli} {
		t.Run(codec.String(), func(t *testing.T) {
			c := &pageChecker{codec: codec, chunkValues: 1000, chunkBytes: 1 << 20}
			defer c.close()
			data := compressWith(t, codec, body)
			require.NoError(t, c.check(dataPage(1000, 8000, int32(len(data))), reader(data)))

			// The header claims more than the data holds.
			err := c.check(dataPage(1000, 8001, int32(len(data))), reader(data))
			require.ErrorIs(t, err, ErrInvalid)

			// The data holds more than the header claims: a small bomb.
			big := compressWith(t, codec, make([]byte, 64000))
			err = c.check(dataPage(1000, 8000, int32(len(big))), reader(big))
			require.ErrorIs(t, err, ErrInvalid)

			// Corrupt data.
			bad := bytes.Clone(data)
			for i := range bad {
				bad[i] ^= 0x5a
			}
			require.ErrorIs(t, c.check(dataPage(1000, 8000, int32(len(bad))), reader(bad)), ErrInvalid)
		})
	}

	// Snappy, LZ4 and gzip cannot expand past their ratio, so a claim past
	// it is refused before decoding.
	c := &pageChecker{codec: format.Lz4Raw, chunkValues: 10, chunkBytes: 1 << 30}
	err := c.check(dataPage(10, 1<<29, 10), reader(make([]byte, 10)))
	require.ErrorContains(t, err, "cannot decode to")

	// An uncompressed chunk is not decoded.
	c = &pageChecker{codec: format.Uncompressed, chunkValues: 10, chunkBytes: 100}
	require.NoError(t, c.check(dataPage(10, 80, 80), reader(make([]byte, 80))))
	// A codec parquet-go cannot decode.
	c = &pageChecker{codec: format.LZO, chunkValues: 10, chunkBytes: 100}
	require.ErrorIs(t, c.check(dataPage(10, 80, 8), reader(make([]byte, 8))), ErrUnsupported)
	// Data the file does not hold.
	c = &pageChecker{codec: format.Uncompressed, chunkValues: 10, chunkBytes: 100}
	require.ErrorIs(t, c.check(dataPage(10, 80, 80), reader(nil)), ErrInvalid)
}

func TestPageCheckerHeaders(t *testing.T) {
	c := &pageChecker{codec: format.Uncompressed, chunkValues: 100, chunkBytes: 1000, maxDef: 1, maxRep: 1}
	data := make([]byte, 1000)
	check := func(h *format.PageHeader) error { return c.check(h, reader(data)) }

	require.ErrorContains(t, check(dataPage(10, 1001, 10)), "uncompressed size 1001")
	require.ErrorContains(t, check(dataPage(101, 10, 10)), "101 values")
	require.ErrorContains(t, check(dataPage(-1, 10, 10)), "-1 values")
	big := &pageChecker{codec: format.Uncompressed, chunkValues: 1 << 40, chunkBytes: 1 << 40}
	require.ErrorIs(t, big.check(dataPage(maxPageValues+1, 10, 10), reader(data)), ErrUnsupported)
	require.ErrorIs(t, big.check(&format.PageHeader{Type: format.DataPage, UncompressedPageSize: maxPageBytes + 1}, reader(data)), ErrUnsupported)

	// v1 levels: a length past the page, and runs past the values.
	page := binary.LittleEndian.AppendUint32(nil, 900)
	require.ErrorContains(t, c.check(dataPage(10, int32(len(page)), int32(len(page))), reader(page)), "levels of 900 bytes")
	rle := []byte{0xfe, 0xff, 0xff, 0xff, 0x0f, 0x01} // a run of 2^31-1 ones
	page = append(binary.LittleEndian.AppendUint32(nil, uint32(len(rle))), rle...)
	require.ErrorContains(t, c.check(dataPage(10, int32(len(page)), int32(len(page))), reader(page)), "runs of more than 10")
	// Two valid level streams, then plain values.
	ok := []byte{0x14, 0x00}
	page = append(binary.LittleEndian.AppendUint32(nil, 2), ok...)
	page = append(page, binary.LittleEndian.AppendUint32(nil, 2)...)
	page = append(page, ok...)
	page = append(page, make([]byte, 80)...)
	require.NoError(t, c.check(dataPage(10, int32(len(page)), int32(len(page))), reader(page)))
	// Bit-packed levels.
	h := dataPage(10, 4, 4)
	h.DataPageHeader.V.RepetitionLevelEncoding = format.BitPacked
	h.DataPageHeader.V.DefinitionLevelEncoding = format.BitPacked
	require.NoError(t, c.check(h, reader(make([]byte, 4))))
	h = dataPage(100, 4, 4)
	h.DataPageHeader.V.RepetitionLevelEncoding = format.BitPacked
	require.ErrorContains(t, c.check(h, reader(make([]byte, 4))), "levels of 13 bytes")
	h.DataPageHeader.V.RepetitionLevelEncoding = format.DeltaBinaryPacked
	require.ErrorIs(t, c.check(h, reader(make([]byte, 4))), ErrUnsupported)

	// v2 pages.
	v2 := func(n, nulls, rows, rep, def, size, csize int32) *format.PageHeader {
		h := &format.PageHeader{Type: format.DataPageV2, UncompressedPageSize: size, CompressedPageSize: csize}
		h.DataPageHeaderV2.Valid = true
		h.DataPageHeaderV2.V = format.DataPageHeaderV2{NumValues: n, NumNulls: nulls, NumRows: rows,
			RepetitionLevelsByteLength: rep, DefinitionLevelsByteLength: def, Encoding: format.RLEDictionary}
		return h
	}
	page = append(append([]byte{}, ok...), ok...)
	page = append(page, 4, 0x14, 0x00)
	require.NoError(t, c.check(v2(10, 0, 10, 2, 2, int32(len(page)), int32(len(page))), reader(page)))
	require.ErrorContains(t, c.check(v2(10, 11, 10, 2, 2, 7, 7), reader(page)), "11 nulls")
	require.ErrorContains(t, c.check(v2(10, 0, 11, 2, 2, 7, 7), reader(page)), "11 rows")
	require.ErrorContains(t, c.check(v2(10, 0, 10, 5, 5, 7, 7), reader(page)), "levels of 10 bytes")
	require.ErrorContains(t, c.check(v2(10, 0, 10, -1, 0, 7, 7), reader(page)), "levels of")
	require.ErrorContains(t, c.check(v2(1000, 0, 10, 2, 2, 7, 7), reader(page)), "1000 values")
	bad := append(append([]byte{}, rle...), ok...)
	require.ErrorContains(t, c.check(v2(10, 0, 10, int32(len(rle)), 2, int32(len(bad)), int32(len(bad))), reader(bad)), "runs of more than")
	bad = append(append(append([]byte{}, ok...), rle...), 4)
	require.ErrorContains(t, c.check(v2(10, 0, 10, 2, int32(len(rle)), int32(len(bad)), int32(len(bad))), reader(bad)), "runs of more than")
	uncompressed := v2(10, 0, 10, 2, 2, int32(len(page)), int32(len(page)))
	uncompressed.DataPageHeaderV2.V.IsCompressed.Valid = true
	snappyChunk := &pageChecker{codec: format.Snappy, chunkValues: 100, chunkBytes: 1000}
	require.NoError(t, snappyChunk.check(uncompressed, reader(page)))
	require.NoError(t, check(&format.PageHeader{Type: format.DataPageV2}))
	require.NoError(t, check(&format.PageHeader{Type: format.DataPage}))

	// Dictionary pages.
	dict := func(n int32) *format.PageHeader {
		h := &format.PageHeader{Type: format.DictionaryPage, UncompressedPageSize: 8, CompressedPageSize: 8}
		h.DictionaryPageHeader.Valid = true
		h.DictionaryPageHeader.V.NumValues = n
		return h
	}
	require.NoError(t, check(dict(2)))
	require.ErrorContains(t, check(dict(73)), "dictionary of 73 values")
	require.ErrorContains(t, check(dict(-1)), "dictionary of -1 values")
	hugeDict := &format.PageHeader{Type: format.DictionaryPage, UncompressedPageSize: maxPageValues, CompressedPageSize: 8}
	hugeDict.DictionaryPageHeader.Valid = true
	hugeDict.DictionaryPageHeader.V.NumValues = maxPageValues + 1
	require.ErrorIs(t, big.check(hugeDict, reader(data)), ErrUnsupported)
	require.NoError(t, check(&format.PageHeader{Type: format.DictionaryPage}))
	require.NoError(t, check(&format.PageHeader{Type: format.IndexPage}))
	require.ErrorContains(t, check(&format.PageHeader{Type: 9}), "page of type 9")

	// parquet-go reads the first page of a chunk as the dictionary when it
	// has a dictionary header, whatever its type, so the header is checked
	// on every page. Here an LZ4 page that would make parquet-go's decoder
	// grow its buffer without end (testdata/fuzz/FuzzOpenAndRead/0284d1860fee778f).
	lz := &pageChecker{codec: format.Lz4Raw, chunkValues: 100, chunkBytes: 1000}
	for _, typ := range []format.PageType{format.DictionaryPage, format.DataPage, 9} {
		h := &format.PageHeader{Type: typ, UncompressedPageSize: 16, CompressedPageSize: 4}
		h.DictionaryPageHeader.Valid = true
		h.DictionaryPageHeader.V.NumValues = 2
		require.ErrorContains(t, lz.check(h, reader([]byte{0xf0, 0xff, 0xff, 0xff})), "LZ4_RAW data", typ)
	}
}

func TestCheckValues(t *testing.T) {
	require.NoError(t, checkValues(nil, format.RLEDictionary, 10))
	require.NoError(t, checkValues([]byte{3, 0x14, 0x05}, format.RLEDictionary, 10))
	require.ErrorContains(t, checkValues([]byte{33}, format.PlainDictionary, 10), "33 bits") //nolint:staticcheck // older writers use it
	require.ErrorContains(t, checkValues([]byte{3, 0x80, 0x01, 0x05}, format.RLEDictionary, 10), "runs of more than 10")
	// Bit-packed groups: one group of eight is fine for ten values, three
	// are not.
	require.NoError(t, checkValues([]byte{1, 0x03, 0xff}, format.RLEDictionary, 10))
	require.ErrorContains(t, checkValues([]byte{1, 0x07, 0xff, 0xff, 0xff}, format.RLEDictionary, 10), "runs of more than 10")
	require.ErrorContains(t, checkValues([]byte{1, 0xff, 0xff, 0xff, 0x0f}, format.RLEDictionary, 10), "groups")
	require.ErrorContains(t, checkValues([]byte{1, 0x80}, format.RLEDictionary, 10), "run header")
	// A bit-packed group of eight 3-bit values takes three bytes; the run
	// after it is counted too.
	require.NoError(t, checkValues([]byte{3, 0x03, 0, 0, 0, 0x04, 0x01}, format.RLEDictionary, 2))
	require.ErrorContains(t, checkValues([]byte{3, 0x03, 0, 0, 0, 0x14, 0x01}, format.RLEDictionary, 2), "runs of more than 2")
	// Booleans, with a length first.
	require.NoError(t, checkValues(nil, format.RLE, 10))
	require.NoError(t, checkValues([]byte{2, 0, 0, 0, 0x14, 0x01}, format.RLE, 10))
	require.ErrorContains(t, checkValues([]byte{9, 0, 0, 0, 0x80, 0x01, 0x01}, format.RLE, 10), "runs of more than 10")
	// Delta headers: block size, miniblocks, values, first value.
	for _, enc := range []format.Encoding{format.DeltaBinaryPacked, format.DeltaLengthByteArray, format.DeltaByteArray} {
		require.NoError(t, checkValues([]byte{0x80, 0x01, 0x04, 0x0a, 0x02}, enc, 10))
		require.NoError(t, checkValues(nil, enc, 0))
		require.ErrorContains(t, checkValues([]byte{0x80, 0x01, 0x04, 0x80, 0x80, 0x80, 0x80, 0x08}, enc, 10), "2147483648 values")
		require.ErrorContains(t, checkValues([]byte{0x80, 0x01}, enc, 10), "delta header")
	}
	require.NoError(t, checkValues([]byte(strings.Repeat("x", 10)), format.Plain, 10))
	require.NoError(t, checkValues([]byte(strings.Repeat("x", 10)), format.ByteStreamSplit, 10))
}
