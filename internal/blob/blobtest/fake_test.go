package blobtest_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/blob"
	"github.com/jamesagarside/hangar/internal/blob/blobtest"
)

func TestFakeConformanceLikeFilesystem(t *testing.T) {
	blobtest.RunConformance(t, blobtest.Capabilities{}, func(*testing.T) blob.Store { return blobtest.NewFake() })
}

func TestFakeConformanceLikeS3(t *testing.T) {
	blobtest.RunConformance(t, blobtest.Capabilities{Presign: true}, func(*testing.T) blob.Store {
		f := blobtest.NewFake()
		f.PresignBaseURL = "https://blobs.example.com/bucket"
		return f
	})
}

func TestFakeHelpers(t *testing.T) {
	ctx := context.Background()
	f := blobtest.NewFake()
	f.PresignBaseURL = "https://blobs.example.com"
	f.MaxPresignPut = 10
	require.NoError(t, f.Put(ctx, "a/b", strings.NewReader("hi"), 2))
	require.Equal(t, []string{"a/b"}, f.Keys())
	b, ok := f.Bytes("a/b")
	require.True(t, ok)
	require.Equal(t, "hi", string(b))
	_, ok = f.Bytes("missing")
	require.False(t, ok)
	require.Equal(t, 1, f.Calls("Put"))

	_, err := f.PresignPut(ctx, "a/c", 11, nil, time.Hour)
	require.ErrorIs(t, err, blob.ErrNotSupported, "over the single PUT limit")
	req, err := f.PresignPut(ctx, "a/c", 5, []byte{0xab}, time.Hour)
	require.NoError(t, err)
	require.Equal(t, "ab", req.Headers["x-amz-checksum-sha256"])

	u, err := f.PresignGet(ctx, "a/b", time.Hour, "x.bin")
	require.NoError(t, err)
	require.Contains(t, u, "response-content-disposition")

	id, err := f.CreateMultipart(ctx, "m/x")
	require.NoError(t, err)
	_, err = f.PutPart(ctx, "m/x", id, 0, strings.NewReader("x"), 1)
	require.Error(t, err)
	_, err = f.PutPart(ctx, "m/x", id, 1, strings.NewReader("x"), 2)
	require.ErrorIs(t, err, blob.ErrSizeMismatch)
	etag, err := f.PutPart(ctx, "m/x", id, 1, strings.NewReader("x"), 1)
	require.NoError(t, err)
	require.Error(t, f.CompleteMultipart(ctx, "m/x", id, nil))
	require.Error(t, f.CompleteMultipart(ctx, "m/x", id, []blob.Part{{Number: 1, ETag: `"wrong"`}}))
	require.ErrorIs(t, f.CompleteMultipart(ctx, "m/x", id, []blob.Part{{Number: 2, ETag: etag}}), blob.ErrNotFound)
	require.ErrorIs(t, f.CompleteMultipart(ctx, "other/key", id, []blob.Part{{Number: 1, ETag: etag}}), blob.ErrNotFound)
	_, err = f.PresignPart(ctx, "m/x", "nope", 1, time.Hour)
	require.ErrorIs(t, err, blob.ErrNotFound)
	require.ErrorIs(t, f.AbortMultipart(ctx, "m/x", "nope"), blob.ErrNotFound)
}
