//go:build xetref

package xet_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/db/dbtest"
	"github.com/hangar-hub/hangar/internal/xet"
)

// The reference xorb is 14 MiB, too large to commit, so this test runs only
// with the xetref build tag:
//
//	go test -tags xetref -run TestReferenceXorb ./internal/xet
//
// It reads the xorb from HANGAR_XET_REFERENCE_XORB, or from the compat
// fixture cache, downloading it from huggingface.co on first use.
const refXorbURL = "https://huggingface.co/datasets/xet-team/xet-spec-reference-files/resolve/main/" + refXorbHash + ".xorb"

func referenceXorb(t *testing.T) []byte {
	t.Helper()
	path := os.Getenv("HANGAR_XET_REFERENCE_XORB")
	if path == "" {
		path = filepath.Join("..", "..", "compat", ".fixtures", "xet-spec-reference-files", refXorbHash+".xorb")
	}
	if b, err := os.ReadFile(path); err == nil {
		return b
	}
	resp, err := http.Get(refXorbURL)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, b, 0o644))
	return b
}

// TestReferenceXorb parses the xorb xet-core produced for the reference
// CSV, then runs the server path end to end: store the xorb, accept the
// reference upload shard, and check the reassembled CSV's SHA-256.
func TestReferenceXorb(t *testing.T) {
	file := referenceXorb(t)
	// The published file ends with a 4-byte remnant, "XETB", after the last
	// chunk: not a whole footer, and not something xet-core's own chunk
	// reader (try_read_chunk_header) accepts either. The parser stays
	// strict, so the remnant is checked and removed here.
	_, err := xet.ParseXorb(bytes.NewReader(file), nil)
	require.ErrorIs(t, err, xet.ErrBadXorb)
	require.True(t, bytes.HasSuffix(file, []byte("XETB")))
	body := file[:len(file)-4]
	idx, err := xet.ParseXorb(bytes.NewReader(body), nil)
	require.NoError(t, err)
	require.Equal(t, refXorbHash, idx.Hash().String())
	chunks := refChunks(t)
	require.Len(t, idx.Chunks, len(chunks))
	for i, c := range chunks {
		require.Equal(t, c.Hash, idx.Chunks[i].Hash, "chunk %d", i)
		require.Equal(t, c.Size, uint64(idx.Chunks[i].Size), "chunk %d", i)
	}
	t.Logf("reference xorb: %d bytes, %d chunks, %d uncompressed bytes, %d bytes of chunk data",
		len(body), len(idx.Chunks), idx.RawSize(), idx.DataSize)

	e := newEnv(t, dbtest.SQLite(t))
	ctx := context.Background()
	inserted, err := e.svc.PutXorb(ctx, e.claims, refXorbHash, bytes.NewReader(body), int64(len(body)))
	require.NoError(t, err)
	require.True(t, inserted)
	shard := readRef(t, refFile+".shard.verification-no-footer")
	res, err := e.svc.AcceptShard(ctx, e.claims, bytes.NewReader(shard), int64(len(shard)), nil)
	require.NoError(t, err)
	require.Len(t, res.Files, 1)
	require.Equal(t, refSHA256, res.Files[0].OID)
	require.Equal(t, refFileHash, res.Files[0].XetHash)
	require.Equal(t, int64(63527244), res.Files[0].Size)
	require.Equal(t, refSHA256, sha(e.objectBytes(t, refSHA256)))
}
