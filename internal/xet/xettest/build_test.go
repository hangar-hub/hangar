package xettest_test

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/xet"
	"github.com/hangar-hub/hangar/internal/xet/xettest"
)

func TestSplitChunksAndBG4(t *testing.T) {
	data := []byte("0123456789")
	require.Equal(t, [][]byte{[]byte("0123"), []byte("4567"), []byte("89")}, xettest.SplitChunks(data, 4))
	require.Empty(t, xettest.SplitChunks(nil, 4))

	// Byte grouping puts byte k of every 4-byte word in group k; the
	// leftover bytes go to the first groups.
	require.Equal(t, []byte("0481592637"), xettest.BG4Split(data))
	for n := range 9 {
		in := bytes.Repeat([]byte{1, 2, 3, 4, 5, 6, 7}, n)[:n]
		x := xettest.BuildXorb([][]byte{append(in, 9)}, false, xet.CompressionBG4LZ4)
		idx, err := xet.ParseXorb(bytes.NewReader(x.Body), nil)
		require.NoError(t, err, "n=%d", n)
		require.Equal(t, x.Hash, idx.Hash())
	}
}

func TestEncodeChunk(t *testing.T) {
	data := bytes.Repeat([]byte("hangar "), 100)
	for _, s := range []xet.CompressionScheme{xet.CompressionNone, xet.CompressionLZ4, xet.CompressionBG4LZ4} {
		c := xettest.EncodeChunk(data, s)
		require.Equal(t, byte(s), c[4])
		idx, err := xet.ParseXorb(bytes.NewReader(c), nil)
		require.NoError(t, err)
		require.Equal(t, xet.ChunkHash(data), idx.Chunks[0].Hash)
	}
	require.Less(t, len(xettest.LZ4Frame(data)), len(data))
}

func TestBuildXorb(t *testing.T) {
	chunks := [][]byte{[]byte("a"), []byte("bb"), []byte("ccc")}
	x := xettest.BuildXorb(chunks, true, xet.CompressionNone, xet.CompressionLZ4)
	require.Equal(t, chunks, x.Data)
	require.Len(t, x.Chunks, 3)
	require.Contains(t, string(x.Body), "XETBLOB")
	idx, err := xet.ParseXorb(bytes.NewReader(x.Body), nil)
	require.NoError(t, err)
	require.Equal(t, x.Hash, idx.Hash())
}

func TestBuildUpload(t *testing.T) {
	a := bytes.Repeat([]byte("model weights "), 2000)
	b := []byte("small")
	u := xettest.BuildUpload([][]byte{a, b, {}}, xettest.UploadOptions{ChunkSize: 1000, ChunksPerXorb: 10})
	require.Len(t, u.Xorbs, 3, "28 + 1 chunks in xorbs of at most 10")
	require.Len(t, u.Shard.Files, 3)
	require.Len(t, u.Shard.Xorbs, 3)
	require.NoError(t, u.Shard.CheckUpload())
	sum := sha256.Sum256(a)
	require.Equal(t, hex.EncodeToString(sum[:]), u.Files[0].SHA256)
	require.Equal(t, hex.EncodeToString(sum[:]), u.Shard.Files[0].SHA256.String())
	require.Equal(t, len(a), u.Files[0].Size)
	require.True(t, u.Files[2].XetHash.IsZero(), "an empty file has the zero hash")

	sh, err := xet.ParseShard(u.ShardBytes())
	require.NoError(t, err)
	indexes := map[xet.Hash]*xet.XorbIndex{}
	for _, x := range u.Xorbs {
		idx, err := xet.ParseXorb(bytes.NewReader(x.Body), nil)
		require.NoError(t, err)
		indexes[x.Hash] = idx
	}
	for i := range sh.Files {
		require.NoError(t, xet.CheckFile(&sh.Files[i], func(h xet.Hash) (*xet.XorbIndex, error) { return indexes[h], nil }))
	}

	// Known xorbs whose chunks are exactly a file's are referenced, not sent.
	known := xettest.BuildXorb([][]byte{b}, false)
	again := xettest.BuildUpload([][]byte{b}, xettest.UploadOptions{ChunkSize: 1000, Known: []xettest.Xorb{
		xettest.BuildXorb([][]byte{[]byte("other")}, false), u.Xorbs[2], known,
	}})
	require.Empty(t, again.Xorbs)
	require.Equal(t, known.Hash, again.Shard.Files[0].Terms[0].Xorb)

	// Defaults: 64 KiB chunks and no xorb chunk limit below the protocol's.
	def := xettest.BuildUpload([][]byte{bytes.Repeat([]byte{7}, 200<<10)}, xettest.UploadOptions{})
	require.Len(t, def.Xorbs, 1)
	require.Len(t, def.Xorbs[0].Chunks, 4)
}

func TestIdleProxy(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				r := bufio.NewReader(c)
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					if line == "slow\n" {
						time.Sleep(500 * time.Millisecond)
					}
					_, _ = io.WriteString(c, "echo "+line)
				}
			}()
		}
	}()

	p, err := xettest.NewIdleProxy(ln.Addr().String(), 200*time.Millisecond)
	require.NoError(t, err)
	defer p.Close()
	c, err := net.Dial("tcp", p.Addr)
	require.NoError(t, err)
	defer c.Close()
	r := bufio.NewReader(c)
	_, _ = io.WriteString(c, "hello\n")
	line, err := r.ReadString('\n')
	require.NoError(t, err)
	require.Equal(t, "echo hello\n", line)
	require.Zero(t, p.Dropped.Load())

	// A server that stays silent longer than the idle timeout is cut off.
	_, _ = io.WriteString(c, "slow\n")
	_, err = r.ReadString('\n')
	require.Error(t, err)
	require.Eventually(t, func() bool { return p.Dropped.Load() == 1 }, time.Second, 10*time.Millisecond)

	// Connections to a target that is not listening close at once.
	dead, err := xettest.NewIdleProxy("127.0.0.1:1", time.Second)
	require.NoError(t, err)
	defer dead.Close()
	c2, err := net.Dial("tcp", dead.Addr)
	require.NoError(t, err)
	_, err = c2.Read(make([]byte, 1))
	require.Error(t, err)
	_ = c2.Close()
}
