package repo

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/blob/blobtest"
	"github.com/jamesagarside/hangar/internal/db/dbtest"
	"github.com/jamesagarside/hangar/internal/model"
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
