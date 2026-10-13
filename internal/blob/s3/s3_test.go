package s3_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/blob"
	"github.com/jamesagarside/hangar/internal/blob/blobtest"
	"github.com/jamesagarside/hangar/internal/blob/s3"
)

// s3Enabled reports whether the S3 tests run, as dbtest does for
// Postgres: `make test` turns them on, and a plain `go test` without Docker
// logs that they are not registered.
func s3Enabled(t *testing.T) bool {
	t.Helper()
	if !blobtest.S3Enabled() {
		t.Logf("S3 tests not registered: set %s=1 (needs Docker) or %s", blobtest.EnvMinIO, blobtest.EnvS3Endpoint)
		return false
	}
	return true
}

func TestConformanceMinIO(t *testing.T) {
	if !s3Enabled(t) {
		return
	}
	blobtest.RunConformance(t, blobtest.Capabilities{
		Presign:         true,
		MinPartSize:     blob.MinPartSize,
		PresignHTTP:     true,
		ChecksumPresign: true,
	}, func(t *testing.T) blob.Store { return blobtest.S3(t) })
}

func TestPutUnknownSizeStreamsInParts(t *testing.T) {
	if !s3Enabled(t) {
		return
	}
	ctx := context.Background()
	s := blobtest.S3(t)
	data := make([]byte, blob.DefaultPartSize+3<<20)
	_, err := rand.Read(data)
	require.NoError(t, err)
	// A reader without Len or Seek, as a streamed request body is.
	require.NoError(t, s.Put(ctx, "exports/big.zip", io.MultiReader(bytes.NewReader(data)), -1))
	rc, info, err := s.Get(ctx, "exports/big.zip", nil)
	require.NoError(t, err)
	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	require.NoError(t, rc.Close())
	require.Equal(t, int64(len(data)), info.Size)
	require.Equal(t, sha256.Sum256(data), sha256.Sum256(got))
}

func TestPutKnownSizeOverSinglePutLimit(t *testing.T) {
	if !s3Enabled(t) {
		return
	}
	ctx := context.Background()
	s := blobtest.S3(t)
	data := make([]byte, blob.DefaultPartSize+1)
	_, err := rand.Read(data)
	require.NoError(t, err)
	require.NoError(t, s.Put(ctx, "tmp/known", io.MultiReader(bytes.NewReader(data)), int64(len(data))))
	info, err := s.Stat(ctx, "tmp/known")
	require.NoError(t, err)
	require.Equal(t, int64(len(data)), info.Size)
	err = s.Put(ctx, "tmp/short", io.MultiReader(bytes.NewReader(data[:100])), int64(len(data)))
	require.ErrorIs(t, err, blob.ErrSizeMismatch)
	_, err = s.Stat(ctx, "tmp/short")
	require.ErrorIs(t, err, blob.ErrNotFound)
}

func TestRangedGetReportsWholeSize(t *testing.T) {
	if !s3Enabled(t) {
		return
	}
	ctx := context.Background()
	s := blobtest.S3(t)
	require.NoError(t, s.Put(ctx, "tmp/r", strings.NewReader("0123456789"), 10))
	rc, info, err := s.Get(ctx, "tmp/r", &blob.Range{Start: 3, End: 4})
	require.NoError(t, err)
	b, _ := io.ReadAll(rc)
	_ = rc.Close()
	require.Equal(t, "34", string(b))
	require.Equal(t, int64(10), info.Size)
}

func TestCheck(t *testing.T) {
	if !s3Enabled(t) {
		return
	}
	ctx := context.Background()
	require.NoError(t, blobtest.S3(t).Check(ctx))
	srv := blobtest.S3TestServer(t)
	opts := srv.Options()
	opts.Bucket = "hangar-missing-bucket"
	st, err := s3.New(opts)
	require.NoError(t, err)
	require.Error(t, st.Check(ctx))
	_, err = st.Stat(ctx, "x")
	require.Error(t, err)
	require.NotErrorIs(t, err, blob.ErrNotFound, "a missing bucket is a configuration error, not a missing object")
}

func TestPresignPutLimit(t *testing.T) {
	st, err := s3.New(s3.Options{Endpoint: "http://127.0.0.1:1", Bucket: "bucket", AccessKeyID: "a", SecretAccessKey: "b", Region: "us-east-1", UsePathStyle: true})
	require.NoError(t, err)
	ctx := context.Background()
	_, err = st.PresignPut(ctx, "lfs/x", blob.MaxSinglePut+1, nil, time.Hour)
	require.ErrorIs(t, err, blob.ErrNotSupported)
	sum := sha256.Sum256([]byte("x"))
	req, err := st.PresignPut(ctx, "lfs/x", 1, sum[:], time.Hour)
	require.NoError(t, err)
	require.Equal(t, "LXEWQrcmsEQBYnyp+6wy9chTD7GQPMTbAiWHF5IaSIE=", req.Headers["x-amz-checksum-sha256"], "base64, as S3 expects")
	require.Contains(t, req.URL, "x-amz-checksum-sha256", "the checksum header is signed")
	_, err = st.PresignPut(ctx, "lfs/x", 1, []byte{1}, time.Hour)
	require.Error(t, err)
	u, err := st.PresignGet(ctx, "lfs/x", time.Hour, "naïve model.gguf")
	require.NoError(t, err)
	require.Contains(t, u, "response-content-disposition=attachment")
	_, err = st.PresignPart(ctx, "lfs/x", "id", 0, time.Hour)
	require.Error(t, err)
}

func TestNewValidatesOptions(t *testing.T) {
	_, err := s3.New(s3.Options{})
	require.Error(t, err, "the bucket is required")
	_, err = s3.New(s3.Options{Bucket: "b", Endpoint: "ftp://x"})
	require.Error(t, err)
	_, err = s3.New(s3.Options{Bucket: "b", Endpoint: "http://"})
	require.Error(t, err)
	for _, ep := range []string{"", "s3.eu-west-2.amazonaws.com", "https://minio.example.com:9000", "http://localhost:9000"} {
		_, err = s3.New(s3.Options{Bucket: "b", Endpoint: ep})
		require.NoError(t, err, ep)
	}
}

func TestContentDisposition(t *testing.T) {
	require.Equal(t, "attachment; filename=model.gguf", s3.ContentDisposition("model.gguf"))
	require.Contains(t, s3.ContentDisposition("naïve.gguf"), "filename*=utf-8''na%C3%AFve.gguf")
}
