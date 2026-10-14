package pq

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"math"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/snappy"
	"github.com/klauspost/compress/zstd"
	"github.com/parquet-go/parquet-go/encoding"
	"github.com/parquet-go/parquet-go/encoding/delta"
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
	// Delta headers: block size, miniblocks, values, first value. For
	// integers only the header is checked; the byte-array encodings are
	// checked in full by TestCheckDeltaByteArrays.
	require.NoError(t, checkValues([]byte{0x80, 0x01, 0x04, 0x0a, 0x02}, format.DeltaBinaryPacked, 10))
	for _, enc := range []format.Encoding{format.DeltaBinaryPacked, format.DeltaLengthByteArray, format.DeltaByteArray} {
		require.NoError(t, checkValues(nil, enc, 0))
		require.ErrorContains(t, checkValues([]byte{0x80, 0x01, 0x04, 0x80, 0x80, 0x80, 0x80, 0x08}, enc, 10), "2147483648 values")
		require.ErrorContains(t, checkValues([]byte{0x80, 0x01}, enc, 10), "delta header")
	}
	require.NoError(t, checkValues([]byte(strings.Repeat("x", 10)), format.Plain, 10))
	require.NoError(t, checkValues([]byte(strings.Repeat("x", 10)), format.ByteStreamSplit, 10))
}

// byteArrays returns the values and offsets parquet-go's byte-array
// encoders take.
func byteArrays(values ...string) ([]byte, []uint32) {
	data, offsets := []byte{}, []uint32{0}
	for _, v := range values {
		data = append(data, v...)
		offsets = append(offsets, uint32(len(data))) //nolint:gosec // test values are short
	}
	return data, offsets
}

// deltaStream encodes 32-bit integers as DELTA_BINARY_PACKED.
func deltaStream(t *testing.T, v ...int32) []byte {
	t.Helper()
	b, err := new(delta.BinaryPackedEncoding).EncodeInt32(nil, v)
	require.NoError(t, err)
	return b
}

func TestCheckDeltaByteArrays(t *testing.T) {
	// Valid pages from parquet-go's encoders, with more values than one
	// block of 128 holds.
	values := []string{"", "a", "ab", "abc", "b", "", "été"}
	for i := range 300 {
		values = append(values, strings.Repeat("x", i%40)+"y")
	}
	data, offsets := byteArrays(values...)
	dlba, err := new(delta.LengthByteArrayEncoding).EncodeByteArray(nil, data, offsets)
	require.NoError(t, err)
	dba, err := new(delta.ByteArrayEncoding).EncodeByteArray(nil, data, offsets)
	require.NoError(t, err)
	n := int64(len(values))
	require.NoError(t, checkValues(dlba, format.DeltaLengthByteArray, n))
	require.NoError(t, checkValues(dba, format.DeltaByteArray, n))
	lengths, rest, err := deltaInts(dlba, "lengths", n)
	require.NoError(t, err)
	require.Len(t, lengths, len(values))
	require.Equal(t, data, rest)

	// Value data cut short.
	require.ErrorContains(t, checkValues(dlba[:len(dlba)-1], format.DeltaLengthByteArray, n), "bytes of page data")
	require.ErrorContains(t, checkValues(dba[:len(dba)-1], format.DeltaByteArray, n), "bytes of page data")
	// More values than the page.
	require.ErrorContains(t, checkValues(dlba, format.DeltaLengthByteArray, n-1), "lengths: delta header of 307 values in a page of 306")
	require.ErrorContains(t, checkValues(dba, format.DeltaByteArray, n-1), "prefix lengths: delta header of 307 values")

	// Lengths that do not fit, each the only fault in its page.
	dlbaOf := func(lengths []int32, data string) []byte { return append(deltaStream(t, lengths...), data...) }
	dbaOf := func(prefixes, suffixes []int32, data string) []byte {
		return append(append(deltaStream(t, prefixes...), deltaStream(t, suffixes...)...), data...)
	}
	require.NoError(t, checkValues(dlbaOf([]int32{1, 2}, "abc"), format.DeltaLengthByteArray, 2))
	require.ErrorContains(t, checkValues(dlbaOf([]int32{1, -1}, "abc"), format.DeltaLengthByteArray, 2), "value 1 has a length of -1")
	require.ErrorContains(t, checkValues(dlbaOf([]int32{1, math.MaxInt32}, "abc"), format.DeltaLengthByteArray, 2), "in 3 bytes of page data")
	require.NoError(t, checkValues(dbaOf([]int32{0, 2, 3}, []int32{3, 1, 0}, "abcd"), format.DeltaByteArray, 3))
	require.ErrorContains(t, checkValues(dbaOf([]int32{1, 0}, []int32{1, 1}, "ab"), format.DeltaByteArray, 2), "value 0 has a prefix of 1 bytes after a value of 0")
	require.ErrorContains(t, checkValues(dbaOf([]int32{0, 2}, []int32{1, 1}, "ab"), format.DeltaByteArray, 2), "value 1 has a prefix of 2 bytes after a value of 1")
	require.ErrorContains(t, checkValues(dbaOf([]int32{0, -1}, []int32{1, 1}, "ab"), format.DeltaByteArray, 2), "prefix of -1 bytes")
	require.ErrorContains(t, checkValues(dbaOf([]int32{0, 0}, []int32{1, math.MaxInt32}, "ab"), format.DeltaByteArray, 2), "in 2 bytes of page data")
	require.ErrorContains(t, checkValues(dbaOf([]int32{0, 0}, []int32{1}, "ab"), format.DeltaByteArray, 2), "2 prefix lengths and 1 suffix lengths")
	require.ErrorContains(t, checkValues(deltaStream(t, 0, 0), format.DeltaByteArray, 2), "suffix lengths: delta header")

	// Prefixes that repeat a value build values larger than the page.
	// parquet-go allocates their total, so it is limited like a page.
	const long, repeats = 1024, maxPageBytes / 1024
	prefixes, suffixes := make([]int32, repeats+1), make([]int32, repeats+1)
	suffixes[0] = long
	for i := 1; i <= repeats; i++ {
		prefixes[i] = long
	}
	huge := dbaOf(prefixes, suffixes, strings.Repeat("z", long))
	require.ErrorIs(t, checkValues(huge, format.DeltaByteArray, repeats+1), ErrUnsupported)
	require.NoError(t, checkValues(dbaOf(prefixes[:repeats], suffixes[:repeats], strings.Repeat("z", long)), format.DeltaByteArray, repeats))

	// Faults in the integer streams themselves.
	header := []byte{0x80, 0x01, 0x04, 0x03, 0x00} // 128 per block, 4 miniblocks, 3 values, first 0
	for name, c := range map[string]struct {
		b    []byte
		want string
	}{
		"no first value":   {header[:4], "no first value"},
		"block size":       {[]byte{0x40, 0x04, 0x03, 0x00}, "delta blocks of 64 values in 4 miniblocks"},
		"no miniblocks":    {[]byte{0x80, 0x01, 0x00, 0x03, 0x00}, "in 0 miniblocks"},
		"miniblock size":   {[]byte{0x80, 0x01, 0x03, 0x03, 0x00}, "in 3 miniblocks"},
		"block too large":  {[]byte{0x80, 0x80, 0x08, 0x04, 0x03, 0x00}, "delta blocks of 131072"},
		"first value":      {[]byte{0x80, 0x01, 0x04, 0x01, 0x80, 0x80, 0x80, 0x80, 0x10}, "not a 32-bit integer"},
		"block header":     {append(bytes.Clone(header), 0x80), "delta block header"},
		"wide miniblock":   {append(bytes.Clone(header), 0x00, 33, 0, 0, 0), "33-bit deltas"},
		"missing values":   {append(bytes.Clone(header), 0x00), "2 of 3 values missing"},
		"no blocks at all": {header, "2 of 3 values missing"},
	} {
		_, _, err := deltaInts(c.b, "lengths", 10)
		require.ErrorIs(t, err, ErrInvalid, name)
		require.ErrorContains(t, err, c.want, name)
	}
	// A smallest delta of 1, then the first miniblock at 2 bits with deltas
	// of 1 above it. Bit widths of miniblocks after the last value are not
	// read.
	v, rest, err := deltaInts(append(bytes.Clone(header), 0x02, 0x02, 0xff, 0xff, 0xff, 0x55, 0, 0, 0, 0, 0, 0, 0, 'x'), "lengths", 10)
	require.NoError(t, err)
	require.Equal(t, []int32{0, 2, 4}, v)
	require.Equal(t, []byte("x"), rest)

	// A dictionary page is decoded with the encoding its header names.
	c := &pageChecker{codec: format.Uncompressed, chunkValues: 100, chunkBytes: 1000}
	page := dlbaOf([]int32{1, 900}, "ab")
	h := &format.PageHeader{Type: format.DictionaryPage, UncompressedPageSize: int32(len(page)), CompressedPageSize: int32(len(page))}
	h.DictionaryPageHeader.Valid = true
	h.DictionaryPageHeader.V = format.DictionaryPageHeader{NumValues: 2, Encoding: format.DeltaLengthByteArray}
	require.ErrorContains(t, c.check(h, reader(page)), "in 2 bytes of page data")
	h.DictionaryPageHeader.V.Encoding = format.PlainDictionary //nolint:staticcheck // older writers use it
	require.NoError(t, c.check(h, reader(page)))
}

// TestDeltaByteArrayChecksMatchParquetGo changes bytes of valid
// DELTA_BYTE_ARRAY and DELTA_LENGTH_BYTE_ARRAY pages at random. Whenever
// the checks accept a page, parquet-go must decode it, and to no more than
// its estimate, which the checks bound: they must read the streams as
// parquet-go reads them.
func TestDeltaByteArrayChecksMatchParquetGo(t *testing.T) {
	values := []string{"apple", "apricot", "banana", "band", "bandana", "", "c"}
	for i := range 200 {
		values = append(values, strings.Repeat("q", i%7)+string(rune('a'+i%26)))
	}
	data, offsets := byteArrays(values...)
	rng := rand.New(rand.NewPCG(3, 4)) //nolint:gosec // reproducible test input, not a secret
	for _, enc := range []interface {
		encoding.Encoding
		EncodeByteArray([]byte, []byte, []uint32) ([]byte, error)
	}{new(delta.LengthByteArrayEncoding), new(delta.ByteArrayEncoding)} {
		page, err := enc.EncodeByteArray(nil, data, offsets)
		require.NoError(t, err)
		accepted := 0
		for i := range 20000 {
			b := bytes.Clone(page)
			for range 1 + rng.IntN(3) {
				b[rng.IntN(len(b))] = byte(rng.IntN(256))
			}
			if checkValues(b, enc.Encoding(), int64(len(values))) != nil {
				continue
			}
			accepted++
			estimate := enc.EstimateDecodeByteArraySize(b)
			require.LessOrEqual(t, estimate, maxPageBytes, "%s mutation %d", enc, i)
			out, _, err := enc.DecodeByteArray(nil, b, nil)
			require.NoError(t, err, "%s mutation %d", enc, i)
			require.LessOrEqual(t, len(out), maxPageBytes, "%s mutation %d", enc, i)
		}
		require.Positive(t, accepted, enc)
	}
}

func TestDecimalArrow(t *testing.T) {
	a, err := decimalArrow("d", 38, 38)
	require.NoError(t, err)
	require.Equal(t, 38, a.Scale)
	_, err = decimalArrow("d", 76, 0)
	require.NoError(t, err)
	for _, c := range [][2]int64{{0, 0}, {5, -1}, {5, 6}, {18, 1 << 31}} {
		_, err := decimalArrow("d", c[0], c[1])
		require.ErrorIs(t, err, ErrInvalid, c)
	}
	_, err = decimalArrow("d", 77, 2)
	require.ErrorIs(t, err, ErrUnsupported)
}
