package convert

import (
	"bytes"
	"compress/gzip"
	"encoding/hex"
	"errors"
	"io"
	"runtime"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/require"
)

func zstdStream(t *testing.T, data []byte) []byte {
	t.Helper()
	var b bytes.Buffer
	w, err := zstd.NewWriter(&b, zstd.WithWindowSize(1<<20), zstd.WithSingleSegment(false))
	require.NoError(t, err)
	_, err = w.Write(data)
	require.NoError(t, err)
	require.NoError(t, w.Close())
	return b.Bytes()
}

// TestDecompressRefusesLargeZstdWindows covers security review finding 7:
// the frame header chooses the window the decoder keeps in memory (twice
// over), and a 67 KB file asking for 512 MiB made the worker hold over 500
// MiB. Windows over maxZstdWindow are refused before anything is held.
func TestDecompressRefusesLargeZstdWindows(t *testing.T) {
	data := []byte(strings.Repeat(`{"text":"hello"}`+"\n", 1000))
	frame := zstdStream(t, data)
	// Magic number (4 bytes), then the frame header descriptor: no
	// single-segment flag, so a window descriptor follows.
	require.Equal(t, []byte{0x28, 0xb5, 0x2f, 0xfd}, frame[:4])
	require.Zero(t, frame[4]&0x20, "not single-segment")

	rc, inner, err := decompress("data/train.jsonl.zst", io.NopCloser(bytes.NewReader(frame)))
	require.NoError(t, err)
	require.Equal(t, "data/train.jsonl", inner)
	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	require.NoError(t, rc.Close())
	require.Equal(t, data, got, "an ordinary window decodes")

	for name, descriptor := range map[string]byte{"256 MiB": 18 << 3, "512 MiB": 19 << 3, "2 GiB": 21 << 3} {
		bomb := bytes.Clone(frame)
		bomb[5] = descriptor // windowLog = 10 + exponent (high 5 bits)
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		rc, _, err := decompress("data/train.jsonl.zst", io.NopCloser(bytes.NewReader(bomb)))
		require.NoError(t, err)
		_, err = io.ReadAll(rc)
		require.ErrorIs(t, err, ErrUnsupported, name)
		require.ErrorContains(t, err, "zstd window larger than 128 MiB", name)
		require.NoError(t, rc.Close())
		runtime.ReadMemStats(&after)
		require.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(64<<20), "%s: allocated %d bytes", name, after.TotalAlloc-before.TotalAlloc)
	}
}

// TestDecompressStopsAtTheByteLimit checks every decompressor stops a
// stream that inflates past maxDecompressedBytes with a clear error, and
// passes a stream that ends exactly at the limit.
func TestDecompressStopsAtTheByteLimit(t *testing.T) {
	old := maxDecompressedBytes
	maxDecompressedBytes = 4096
	t.Cleanup(func() { maxDecompressedBytes = old })

	gz := func(data []byte) []byte {
		var b bytes.Buffer
		w := gzip.NewWriter(&b)
		_, _ = w.Write(data)
		_ = w.Close()
		return b.Bytes()
	}
	// Go has no bzip2 writer: these are Python's bz2.compress of 4096
	// bytes and of 1 MiB of newlines.
	bzFixtures := map[int]string{
		4096:    "425a683931415926535972e523fb000008400080140008200030cc05536a41470f17724538509072e523fb",
		1 << 20: "425a6839314159265359e83257b3000808400080140008200030cc0529a60806c4201e2ee48a70a121d064af66",
	}
	bz := func(data []byte) []byte {
		b, err := hex.DecodeString(bzFixtures[len(data)])
		require.NoError(t, err)
		return b
	}
	zs := func(data []byte) []byte { return zstdStream(t, data) }
	for ext, compress := range map[string]func([]byte) []byte{".gz": gz, ".bz2": bz, ".zst": zs} {
		exact := bytes.Repeat([]byte("\n"), 4096)
		rc, _, err := decompress("train.jsonl"+ext, io.NopCloser(bytes.NewReader(compress(exact))))
		require.NoError(t, err, ext)
		got, err := io.ReadAll(rc)
		require.NoError(t, err, "%s: a stream of exactly the limit passes", ext)
		require.Len(t, got, 4096)

		bomb := bytes.Repeat([]byte("\n"), 1<<20)
		rc, _, err = decompress("train.jsonl"+ext, io.NopCloser(bytes.NewReader(compress(bomb))))
		require.NoError(t, err, ext)
		_, err = io.ReadAll(rc)
		require.True(t, errors.Is(err, ErrUnsupported), "%s: %v", ext, err)
		require.ErrorContains(t, err, "train.jsonl"+ext+" decompresses to more than 4096 bytes")
	}
}
