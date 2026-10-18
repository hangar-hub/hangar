package repo

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/blob/blobtest"
	"github.com/hangar-hub/hangar/internal/db/dbtest"
	"github.com/hangar-hub/hangar/internal/model"
)

// TestIngestStreamsLargeObjects covers the path for objects over the
// streaming threshold: a multipart upload at the final key that completes
// only when the SHA-256 matches.
func TestIngestStreamsLargeObjects(t *testing.T) {
	ctx := context.Background()
	blobs := blobtest.NewFake()
	l := NewLFS(dbtest.SQLite(t), blobs)
	l.streamThreshold = 10
	content := bytes.Repeat([]byte("abcdefghij"), 4<<20/10)
	sum := sha256.Sum256(content)
	oid := hex.EncodeToString(sum[:])
	obj, err := l.Ingest(ctx, oid, int64(len(content)), bytes.NewReader(content))
	require.NoError(t, err)
	require.True(t, obj.Verified)
	got, ok := blobs.Bytes(model.LFSObjectKey(oid))
	require.True(t, ok)
	require.Equal(t, content, got)
	require.Positive(t, blobs.Calls("CompleteMultipart"))

	wrong := strings.Repeat("a", 64)
	_, err = l.Ingest(ctx, wrong, int64(len(content)), bytes.NewReader(content))
	require.ErrorIs(t, err, ErrChecksumMismatch)
	_, ok = blobs.Bytes(model.LFSObjectKey(wrong))
	require.False(t, ok, "a mismatched stream never becomes visible")
	_, err = l.Ingest(ctx, wrong, 20, strings.NewReader(strings.Repeat("x", 25)))
	require.ErrorIs(t, err, ErrChecksumMismatch, "a body longer than its size is refused")
	_, err = l.Ingest(ctx, wrong, 20, strings.NewReader("short"))
	require.ErrorIs(t, err, ErrChecksumMismatch)
}

// TestBlobCacheEvictsLeastRecentlyUsed checks the blob cache's bound: a blob
// larger than the cache is not kept, a repeated add changes nothing, and
// adding past the limit drops the blobs read longest ago.
func TestBlobCacheEvictsLeastRecentlyUsed(t *testing.T) {
	t.Parallel()
	c := newBlobCache(10)
	c.add("huge", make([]byte, 11))
	_, ok := c.get("huge")
	require.False(t, ok)

	c.add("a", []byte("aaaa"))
	c.add("b", []byte("bbbb"))
	c.add("a", []byte("AAAA"))
	got, ok := c.get("a")
	require.True(t, ok)
	require.Equal(t, "aaaa", string(got))

	// a was read after b, so b goes first.
	c.add("c", []byte("cccc"))
	_, ok = c.get("b")
	require.False(t, ok)
	for _, oid := range []string{"a", "c"} {
		_, ok = c.get(oid)
		require.True(t, ok, oid)
	}
	require.Equal(t, int64(8), c.size)
}
