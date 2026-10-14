package xet_test

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/xet"
	"github.com/jamesagarside/hangar/internal/xet/xettest"
)

// Reference vectors from the Hugging Face dataset
// xet-team/xet-spec-reference-files (Apache-2.0), in testdata/reference.
const (
	refDir       = "testdata/reference"
	refFile      = "Electric_Vehicle_Population_Data_20250917.csv"
	refFileHash  = "118a53328412787fee04011dcf82fdc4acf3a4a1eddec341c910d30a306aaf97"
	refXorbHash  = "eea25d6ee393ccae385820daed127b96ef0ea034dfb7cf6da3a950ce334b7632"
	refRangeHash = "d81c11b1fc9bc2a25587108c675bbfe65ca2e5d350b0cd92c58329fcc8444178"
	// refSHA256 is the CSV's SHA-256, from the dataset's LFS metadata.
	refSHA256 = "f41255b252f776125f2b657136654e4d4d5d2ccf8ef4db0ec186bd1981b69734"
)

func readRef(t testing.TB, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(refDir, name))
	require.NoError(t, err)
	return b
}

// refChunks parses the .chunks list: "<hash> <length>" per line.
func refChunks(t testing.TB) []xet.Chunk {
	t.Helper()
	var out []xet.Chunk
	sc := bufio.NewScanner(bytes.NewReader(readRef(t, refFile+".chunks")))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 0 {
			continue
		}
		require.Len(t, f, 2)
		h, err := xet.ParseHash(f[0])
		require.NoError(t, err)
		n, err := strconv.ParseUint(f[1], 10, 64)
		require.NoError(t, err)
		out = append(out, xet.Chunk{Hash: h, Size: n})
	}
	require.NoError(t, sc.Err())
	return out
}

func mustHash(t testing.TB, s string) xet.Hash {
	t.Helper()
	h, err := xet.ParseHash(s)
	require.NoError(t, err)
	return h
}

func TestReferenceChunkHashes(t *testing.T) {
	for _, name := range []string{
		"b10aa1dc71c61661de92280c41a188aabc47981739b785724a099945d8dc5ce4",
		"26255591fa803b6baf25d88c315b8a6f5153d5bcfdf18ec5ef526264e0ccc907",
		"099cb228194fe640e36a6c7d274ee5ed3a714ccd557a0951d9b6b43a7292b5d1",
	} {
		data := readRef(t, name+".chunk")
		require.Equal(t, name, xet.ChunkHash(data).String())
	}
}

func TestReferenceAggregateHashes(t *testing.T) {
	chunks := refChunks(t)
	require.Len(t, chunks, 796)
	require.Equal(t, strings.TrimSpace(string(readRef(t, refFile+".xet-file-hash"))), refFileHash)
	require.Equal(t, strings.TrimSpace(string(readRef(t, refFile+".xet-xorb-hash"))), refXorbHash)
	require.Equal(t, refFileHash, xet.FileHash(chunks).String())
	require.Equal(t, refXorbHash, xet.XorbHash(chunks).String())
	hashes := make([]xet.Hash, len(chunks))
	for i, c := range chunks {
		hashes[i] = c.Hash
	}
	require.Equal(t, refRangeHash, xet.RangeHash(hashes).String())
	require.Equal(t, refRangeHash, strings.TrimSpace(string(readRef(t, refXorbHash+".xorb.range-hash"))))

	// The first chunk of the list is the first .chunk file.
	require.Equal(t, "b10aa1dc71c61661de92280c41a188aabc47981739b785724a099945d8dc5ce4", chunks[0].Hash.String())
}

// refIndex builds the index Hangar would store for the reference xorb,
// from the chunk list: the offsets are those of uncompressed chunks.
func refIndex(t testing.TB) *xet.XorbIndex {
	t.Helper()
	idx := &xet.XorbIndex{}
	var off uint32
	for _, c := range refChunks(t) {
		idx.Chunks = append(idx.Chunks, xet.XorbChunk{Hash: c.Hash, Offset: off, CompressedSize: uint32(c.Size), Size: uint32(c.Size)})
		off += 8 + uint32(c.Size)
	}
	idx.DataSize = int64(off)
	return idx
}

func TestReferenceShards(t *testing.T) {
	idx := refIndex(t)
	index := func(h xet.Hash) (*xet.XorbIndex, error) {
		if h.String() == refXorbHash {
			return idx, nil
		}
		return nil, nil
	}
	for _, name := range []string{".shard.verification-no-footer", ".shard.verification"} {
		t.Run(name, func(t *testing.T) {
			b := readRef(t, refFile+name)
			sh, err := xet.ParseShard(b)
			require.NoError(t, err)
			require.Len(t, sh.Files, 1)
			f := sh.Files[0]
			require.Equal(t, refFileHash, f.Hash.String())
			require.NotZero(t, f.Flags&xet.FileFlagVerification)
			require.Len(t, f.Verification, len(f.Terms))
			for _, term := range f.Terms {
				require.Equal(t, refXorbHash, term.Xorb.String())
			}
			require.Len(t, sh.Xorbs, 1)
			require.Equal(t, refXorbHash, sh.Xorbs[0].Hash.String())
			require.Len(t, sh.Xorbs[0].Chunks, 796)
			require.NoError(t, xet.CheckFile(&f, index))
			require.NoError(t, xet.CheckXorb(&sh.Xorbs[0], idx))
			if name == ".shard.verification-no-footer" {
				require.Zero(t, sh.FooterSize)
				// Serialising the parsed shard gives the same bytes.
				require.Equal(t, b, xet.MarshalShard(sh))
			} else {
				require.NotZero(t, sh.FooterSize)
			}
			// The metadata extension carries the CSV's SHA-256 in hash form.
			require.NotNil(t, f.SHA256)
			require.Equal(t, refSHA256, f.SHA256.String())
			require.NotEqual(t, refSHA256, hexOf(f.SHA256[:]))

			// A wrong verification hash is caught.
			bad := f
			bad.Verification = append([]xet.Hash(nil), f.Verification...)
			bad.Verification[0][0] ^= 1
			require.ErrorIs(t, xet.CheckFile(&bad, index), xet.ErrBadShard)
		})
	}

	// The base shard has no verification entries, so uploads refuse it.
	sh, err := xet.ParseShard(readRef(t, refFile+".shard"))
	require.NoError(t, err)
	require.Len(t, sh.Files, 1)
	require.Zero(t, sh.Files[0].Flags&xet.FileFlagVerification)
	require.ErrorIs(t, sh.CheckUpload(), xet.ErrBadShard)

	// The dedupe shard lists the xorb with HMAC'd chunk hashes and no files.
	sh, err = xet.ParseShard(readRef(t, refFile+".shard.dedupe"))
	require.NoError(t, err)
	require.Empty(t, sh.Files)
	require.Len(t, sh.Xorbs, 1)
	require.ErrorIs(t, xet.CheckXorb(&sh.Xorbs[0], idx), xet.ErrBadShard)
}

func hexOf(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 0, 2*len(b))
	for _, c := range b {
		out = append(out, digits[c>>4], digits[c&15])
	}
	return string(out)
}

func TestHashStringForms(t *testing.T) {
	h := mustHash(t, refXorbHash)
	require.Equal(t, refXorbHash, h.String())
	// Each 8-byte word prints byte-reversed.
	require.Equal(t, byte(0xae), h[0], "the word eea25d6ee393ccae is stored little-endian")
	_, err := xet.ParseHash(strings.ToUpper(refXorbHash))
	require.Error(t, err)
	_, err = xet.ParseHash("abc")
	require.Error(t, err)
	require.True(t, xet.Hash{}.IsZero())

	// One chunk: the xorb hash is the chunk hash, with no internal node.
	c := xet.Chunk{Hash: xet.ChunkHash([]byte("hello")), Size: 5}
	require.Equal(t, c.Hash, xet.XorbHash([]xet.Chunk{c}))
	require.NotEqual(t, c.Hash, xet.FileHash([]xet.Chunk{c}))
	require.True(t, xet.XorbHash(nil).IsZero())
	require.True(t, xet.FileHash(nil).IsZero())

	// Trees of every small size, including levels with a one-node tail
	// group, are deterministic and differ from each other.
	seen := map[xet.Hash]int{}
	var chunks []xet.Chunk
	for n := 1; n <= 40; n++ {
		chunks = append(chunks, xet.Chunk{Hash: xet.ChunkHash([]byte{byte(n)}), Size: uint64(n)})
		h := xet.XorbHash(chunks)
		require.Equal(t, h, xet.XorbHash(append([]xet.Chunk(nil), chunks...)))
		_, dup := seen[h]
		require.False(t, dup, "n=%d", n)
		seen[h] = n
	}
}

func randomBytes(r *rand.Rand, n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(r.Uint32())
	}
	return b
}

// textBytes is compressible.
func textBytes(n int, seed int) []byte {
	var b bytes.Buffer
	for i := 0; b.Len() < n; i++ {
		b.WriteString("row " + strconv.Itoa(i*seed) + ", model hangar, quantisation q4_0;\n")
	}
	return b.Bytes()[:n]
}

func TestParseXorbSchemes(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	var chunks [][]byte
	// BG4 lengths with n%4 in {0,1,2,3}, random and compressible data,
	// a maximum-size chunk and a one-byte chunk.
	for _, n := range []int{4096, 4097, 4098, 4099, 1, xet.MaxChunkSize} {
		chunks = append(chunks, textBytes(n, n), randomBytes(r, n))
	}
	for _, footer := range []bool{false, true} {
		x := xettest.BuildXorb(chunks, footer, xet.CompressionNone, xet.CompressionLZ4, xet.CompressionBG4LZ4)
		var data bytes.Buffer
		idx, err := xet.ParseXorb(bytes.NewReader(x.Body), &data)
		require.NoError(t, err)
		require.Equal(t, x.Hash, idx.Hash())
		require.Len(t, idx.Chunks, len(chunks))
		require.Equal(t, int64(data.Len()), idx.DataSize)
		if footer {
			require.Less(t, data.Len(), len(x.Body))
			require.Equal(t, x.Body[:data.Len()], data.Bytes())
		} else {
			require.Equal(t, x.Body, data.Bytes())
		}
		var raw uint64
		for _, c := range chunks {
			raw += uint64(len(c))
		}
		require.Equal(t, raw, idx.RawSize())

		// The stored index round-trips, with the payload sizes recovered.
		back, err := xet.UnmarshalIndex(append(idx.MarshalIndex(), 0xAA, 0xBB))
		require.NoError(t, err)
		require.Equal(t, idx, back)
	}

	// One chunk: the xorb hash equals the chunk hash.
	one := xettest.BuildXorb([][]byte{[]byte("only")}, false)
	idx, err := xet.ParseXorb(bytes.NewReader(one.Body), nil)
	require.NoError(t, err)
	require.Equal(t, xet.ChunkHash([]byte("only")), idx.Hash())
}

func TestParseXorbRejects(t *testing.T) {
	good := xettest.BuildXorb([][]byte{textBytes(1000, 1), textBytes(2000, 2)}, false, xet.CompressionLZ4)
	hdr := func(version byte, comp uint32, scheme xet.CompressionScheme, size uint32) []byte {
		b := xet.ChunkHeader{Version: version, CompressedSize: comp, Scheme: scheme, Size: size}.Marshal()
		return b[:]
	}
	lz := xettest.LZ4Frame(textBytes(1000, 3))
	cases := map[string][]byte{
		"empty":             nil,
		"truncated header":  good.Body[:5],
		"truncated payload": good.Body[:len(good.Body)-3],
		"bad version":       append(hdr(1, 3, xet.CompressionNone, 3), "abc"...),
		"bad scheme":        append(hdr(0, 3, 99, 3), "abc"...),
		"compressed too big": append(hdr(0, xet.MaxCompressedChunkSize+1, xet.CompressionLZ4, 10),
			make([]byte, 16)...),
		"chunk too big":        append(hdr(0, 3, xet.CompressionLZ4, xet.MaxChunkSize+1), "abc"...),
		"zero length chunk":    hdr(0, 0, xet.CompressionNone, 0),
		"none length mismatch": append(hdr(0, 3, xet.CompressionNone, 4), "abc"...),
		"lz4 decodes to more":  append(hdr(0, uint32(len(lz)), xet.CompressionLZ4, 999), lz...),
		"lz4 decodes to fewer": append(hdr(0, uint32(len(lz)), xet.CompressionLZ4, 1001), lz...),
		"lz4 garbage":          append(hdr(0, 5, xet.CompressionLZ4, 10), "hello"...),
		"lz4 trailing bytes":   append(hdr(0, uint32(len(lz)+2), xet.CompressionLZ4, 1000), append(lz, 1, 2)...),
		"bg4 garbage":          append(hdr(0, 5, xet.CompressionBG4LZ4, 10), "hello"...),
		"trailing partial":     append(append([]byte(nil), good.Body...), 0, 0, 0),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := xet.ParseXorb(bytes.NewReader(body), nil)
			require.ErrorIs(t, err, xet.ErrBadXorb)
		})
	}

	// A decompression bomb: a tiny frame of zeros that expands far past the
	// declared length is refused without decoding it all.
	bomb := xettest.LZ4Frame(make([]byte, 64<<20))
	_, err := xet.ParseXorb(bytes.NewReader(append(hdr(0, uint32(min(len(bomb), xet.MaxCompressedChunkSize)), xet.CompressionLZ4, 100), bomb...)), nil)
	require.ErrorIs(t, err, xet.ErrBadXorb)

	// More than 8192 chunks.
	var many bytes.Buffer
	for range xet.MaxXorbChunks + 1 {
		many.Write(xettest.EncodeChunk([]byte{1}, xet.CompressionNone))
	}
	_, err = xet.ParseXorb(&many, nil)
	require.ErrorIs(t, err, xet.ErrBadXorb)

	// More than 64 MiB of uncompressed data.
	var big bytes.Buffer
	chunk := xettest.EncodeChunk(make([]byte, xet.MaxChunkSize), xet.CompressionLZ4)
	for range xet.MaxXorbBytes/xet.MaxChunkSize + 1 {
		big.Write(chunk)
	}
	_, err = xet.ParseXorb(&big, nil)
	require.ErrorIs(t, err, xet.ErrBadXorb)
}

func TestUnmarshalIndexRejects(t *testing.T) {
	x := xettest.BuildXorb([][]byte{[]byte("abc"), []byte("defg")}, false)
	idx, err := xet.ParseXorb(bytes.NewReader(x.Body), nil)
	require.NoError(t, err)
	b := idx.MarshalIndex()
	for name, in := range map[string][]byte{
		"short":     b[:10],
		"magic":     append([]byte("NOTMAGIC"), b[8:]...),
		"truncated": b[:len(b)-1],
		"zero":      func() []byte { c := bytes.Clone(b); binary.LittleEndian.PutUint32(c[8:], 0); return c }(),
		"too many":  func() []byte { c := bytes.Clone(b); binary.LittleEndian.PutUint32(c[8:], 9000); return c }(),
		"bad size":  func() []byte { c := bytes.Clone(b); binary.LittleEndian.PutUint64(c[16:], 3); return c }(),
	} {
		_, err := xet.UnmarshalIndex(in)
		require.Error(t, err, name)
	}
}

func TestBuildUploadAndCheck(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 4))
	files := [][]byte{randomBytes(r, 300_000), textBytes(70_000, 7), {}, randomBytes(r, 10)}
	u := xettest.BuildUpload(files, xettest.UploadOptions{ChunkSize: 16 << 10, ChunksPerXorb: 5,
		Schemes: []xet.CompressionScheme{xet.CompressionLZ4, xet.CompressionBG4LZ4, xet.CompressionNone}})
	require.Greater(t, len(u.Xorbs), 2, "the files cross xorb boundaries")
	require.NoError(t, u.Shard.CheckUpload())
	parsed, err := xet.ParseShard(u.ShardBytes())
	require.NoError(t, err)
	require.Equal(t, u.ShardBytes(), xet.MarshalShard(parsed))

	indexes := map[xet.Hash]*xet.XorbIndex{}
	for _, x := range u.Xorbs {
		idx, err := xet.ParseXorb(bytes.NewReader(x.Body), nil)
		require.NoError(t, err)
		require.Equal(t, x.Hash, idx.Hash())
		indexes[x.Hash] = idx
	}
	index := func(h xet.Hash) (*xet.XorbIndex, error) { return indexes[h], nil }
	for i := range parsed.Files {
		require.NoError(t, xet.CheckFile(&parsed.Files[i], index))
	}
	for i := range parsed.Xorbs {
		require.NoError(t, xet.CheckXorb(&parsed.Xorbs[i], indexes[parsed.Xorbs[i].Hash]))
	}
	require.Len(t, parsed.XorbHashes(), len(u.Xorbs))
	require.Equal(t, int64(300_000), parsed.Files[0].Size())

	// Mismatches the checks must catch.
	f := parsed.Files[0]
	f.Terms = append([]xet.Term(nil), f.Terms...)
	f.Terms[0].UnpackedBytes++
	require.ErrorIs(t, xet.CheckFile(&f, index), xet.ErrBadShard)
	f = parsed.Files[0]
	f.Hash[0] ^= 1
	require.ErrorIs(t, xet.CheckFile(&f, index), xet.ErrBadShard)
	f = parsed.Files[0]
	f.Terms = append([]xet.Term(nil), f.Terms...)
	f.Terms[0].End = 1000
	require.ErrorIs(t, xet.CheckFile(&f, index), xet.ErrBadShard)
	f.Terms[0].Xorb = xet.Hash{9}
	require.Error(t, xet.CheckFile(&f, index))

	x := parsed.Xorbs[0]
	x.Chunks = append([]xet.ShardChunk(nil), x.Chunks...)
	x.Chunks[1].Offset++
	require.ErrorIs(t, xet.CheckXorb(&x, indexes[x.Hash]), xet.ErrBadShard)
	x = parsed.Xorbs[0]
	x.RawBytes++
	require.ErrorIs(t, xet.CheckXorb(&x, indexes[x.Hash]), xet.ErrBadShard)
	x = parsed.Xorbs[0]
	x.Chunks = x.Chunks[1:]
	require.ErrorIs(t, xet.CheckXorb(&x, indexes[x.Hash]), xet.ErrBadShard)
}

func TestParseShardRejects(t *testing.T) {
	u := xettest.BuildUpload([][]byte{textBytes(5000, 1)}, xettest.UploadOptions{ChunkSize: 1000})
	good := u.ShardBytes()
	set := func(off int, v uint64) []byte {
		c := bytes.Clone(good)
		binary.LittleEndian.PutUint64(c[off:], v)
		return c
	}
	set32 := func(off int, v uint32) []byte {
		c := bytes.Clone(good)
		binary.LittleEndian.PutUint32(c[off:], v)
		return c
	}
	for name, b := range map[string][]byte{
		"empty":            nil,
		"bad tag":          append([]byte("x"), good[1:]...),
		"bad version":      set(32, 3),
		"footer too big":   set(40, uint64(len(good)+1)),
		"footer truncated": set(40, 48),
		"no bookend":       good[:len(good)-48],
		"trailing bytes":   append(bytes.Clone(good), make([]byte, 48)...),
		"partial record":   append(bytes.Clone(good), 1),
		"huge term count":  set32(48+36, 1<<31),
		"huge chunk count": set32(len(good)-48*(len(u.Shard.Xorbs[0].Chunks)+2)+36, 1<<30),
		"too large":        make([]byte, xet.MaxShardSize+1),
	} {
		_, err := xet.ParseShard(b)
		require.Error(t, err, name)
	}

	// Upload rules.
	sh, err := xet.ParseShard(good)
	require.NoError(t, err)
	sh.Files[0].SHA256 = nil
	require.ErrorIs(t, sh.CheckUpload(), xet.ErrBadShard)
	sh, _ = xet.ParseShard(good)
	sh.Files[0].Terms[0].End = sh.Files[0].Terms[0].Start
	require.ErrorIs(t, sh.CheckUpload(), xet.ErrBadShard)
	sh, _ = xet.ParseShard(good)
	sh.Files[0].Terms = nil
	require.ErrorIs(t, sh.CheckUpload(), xet.ErrBadShard)
	sh, _ = xet.ParseShard(good)
	sh.Xorbs[0].Chunks = nil
	require.ErrorIs(t, sh.CheckUpload(), xet.ErrBadShard)
}

func TestShardEventJSON(t *testing.T) {
	for want, e := range map[string]xet.ShardEvent{
		`{"type":"validating","verified":0,"total":0}`:        {Type: xet.EventValidating},
		`{"type":"validating","verified":3,"total":7}`:        {Type: xet.EventValidating, Verified: 3, Total: 7},
		`{"type":"committing","stage":"uploading"}`:           {Type: xet.EventCommitting, Stage: xet.StageUploading},
		`{"type":"result"}`:                                   {Type: xet.EventResult, Result: 1},
		`{"type":"error","message":"boom","retryable":false}`: {Type: xet.EventError, Message: "boom"},
		`{"type":"error","message":"again","retryable":true}`: {Type: xet.EventError, Message: "again", Retryable: true},
	} {
		b, err := e.MarshalJSON()
		require.NoError(t, err)
		require.JSONEq(t, want, string(b))
	}
	_, err := xet.ShardEvent{Type: "nope"}.MarshalJSON()
	require.Error(t, err)
}
