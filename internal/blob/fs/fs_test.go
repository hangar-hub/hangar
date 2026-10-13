package fs_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/blob"
	"github.com/jamesagarside/hangar/internal/blob/blobtest"
	blobfs "github.com/jamesagarside/hangar/internal/blob/fs"
)

func newStore(t *testing.T) *blobfs.Store {
	t.Helper()
	s, err := blobfs.New(filepath.Join(t.TempDir(), "blobs"))
	require.NoError(t, err)
	return s
}

func TestConformance(t *testing.T) {
	blobtest.RunConformance(t, blobtest.Capabilities{}, func(t *testing.T) blob.Store { return newStore(t) })
}

func TestLayoutIsPlainFiles(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	oid := strings.Repeat("ab", 32)
	key := "lfs/sha256/ab/ab/" + oid
	require.NoError(t, s.Put(ctx, key, strings.NewReader("weights"), 7))
	// An admin can recover the object with standard tools.
	b, err := os.ReadFile(filepath.Join(s.Root(), "lfs", "sha256", "ab", "ab", oid))
	require.NoError(t, err)
	require.Equal(t, "weights", string(b))
	// Nothing is left in staging.
	entries, err := os.ReadDir(filepath.Join(s.Root(), ".staging"))
	require.NoError(t, err)
	require.Empty(t, entries)
}

func TestStagingIsNotAKey(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	require.ErrorIs(t, s.Put(ctx, ".staging/x", strings.NewReader("x"), 1), blob.ErrInvalidKey)
	require.NoError(t, s.Put(ctx, "a", strings.NewReader("x"), 1))
	var keys []string
	require.NoError(t, s.List(ctx, "", func(i blob.Info) error { keys = append(keys, i.Key); return nil }))
	require.Equal(t, []string{"a"}, keys)
}

func TestDeletePrunesEmptyDirectories(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	require.NoError(t, s.Put(ctx, "git/r1/pack/a", strings.NewReader("a"), 1))
	require.NoError(t, s.Put(ctx, "git/r2/pack/b", strings.NewReader("b"), 1))
	require.NoError(t, s.Delete(ctx, "git/r1/pack/a"))
	_, err := os.Stat(filepath.Join(s.Root(), "git", "r1"))
	require.ErrorIs(t, err, os.ErrNotExist)
	_, err = os.Stat(filepath.Join(s.Root(), "git", "r2", "pack", "b"))
	require.NoError(t, err)
	// A Put after the prune recreates the directories.
	require.NoError(t, s.Put(ctx, "git/r1/pack/c", strings.NewReader("c"), 1))
}

func TestPartETagRecoveredWithoutSidecar(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	id, err := s.CreateMultipart(ctx, "lfs/sha256/x")
	require.NoError(t, err)
	etag, err := s.PutPart(ctx, "lfs/sha256/x", id, 1, strings.NewReader("part one"), 8)
	require.NoError(t, err)
	// Simulate a crash between the part rename and the sidecar write.
	require.NoError(t, os.Remove(filepath.Join(s.Root(), "uploads", id, "part-00001.etag")))
	parts, err := s.ListParts(ctx, "lfs/sha256/x", id)
	require.NoError(t, err)
	require.Len(t, parts, 1)
	require.Equal(t, etag, parts[0].ETag)
	require.NoError(t, s.CompleteMultipart(ctx, "lfs/sha256/x", id, parts))
	_, err = os.Stat(filepath.Join(s.Root(), "uploads", id))
	require.ErrorIs(t, err, os.ErrNotExist, "completion removes the staged parts")
}

func TestMultipartKeyMustMatch(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	id, err := s.CreateMultipart(ctx, "lfs/sha256/x")
	require.NoError(t, err)
	_, err = s.PutPart(ctx, "lfs/sha256/other", id, 1, strings.NewReader("x"), 1)
	require.ErrorIs(t, err, blob.ErrNotFound)
	_, err = s.ListParts(ctx, "lfs/sha256/x", "not-a-uuid")
	require.ErrorIs(t, err, blob.ErrNotFound)
	_, err = s.PutPart(ctx, "lfs/sha256/x", id, 0, strings.NewReader("x"), 1)
	require.Error(t, err)
	require.ErrorIs(t, s.CompleteMultipart(ctx, "lfs/sha256/x", id, []blob.Part{{Number: 1, ETag: "x"}}), blob.ErrNotFound)
	require.Error(t, s.CompleteMultipart(ctx, "lfs/sha256/x", id, nil))
}

func TestPresignNotSupported(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	_, err := s.PresignGet(ctx, "a", time.Hour, "")
	require.ErrorIs(t, err, blob.ErrNotSupported)
}

func TestNewErrors(t *testing.T) {
	_, err := blobfs.New("")
	require.Error(t, err)
	file := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(file, []byte("x"), 0o600))
	_, err = blobfs.New(file)
	require.Error(t, err, "the root must be a directory")
}

func TestListStopsOnCancelledContext(t *testing.T) {
	s := newStore(t)
	require.NoError(t, s.Put(context.Background(), "a/b", strings.NewReader("x"), 1))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, s.List(ctx, "", func(blob.Info) error { return nil }), context.Canceled)
}

func TestStreamsLargeObjects(t *testing.T) {
	blobtest.RunStreaming(t, newStore(t), 48<<20, 16<<20)
}
