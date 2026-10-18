package s3

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/blob"
)

func TestUnknownSizePartSize(t *testing.T) {
	require.Equal(t, blob.DefaultPartSize, unknownSizePartSize(1))
	require.Equal(t, blob.DefaultPartSize, unknownSizePartSize(2000))
	require.Equal(t, 2*blob.DefaultPartSize, unknownSizePartSize(2001))
	require.Equal(t, 8*blob.DefaultPartSize, unknownSizePartSize(10000))
	var total int64
	for n := 1; n <= blob.MaxParts; n++ {
		total += unknownSizePartSize(n)
	}
	require.Greater(t, total, int64(700)<<30, "unknown-size uploads reach over 700 GiB")
}

func TestTotalFromContentRange(t *testing.T) {
	n, ok := totalFromContentRange("bytes 3-4/10")
	require.True(t, ok)
	require.Equal(t, int64(10), n)
	_, ok = totalFromContentRange("")
	require.False(t, ok)
	_, ok = totalFromContentRange("bytes 3-4/*")
	require.False(t, ok)
}

func TestMapErrKeepsS3CodeForTelemetry(t *testing.T) {
	resp := minio.ErrorResponse{Code: "AccessDenied", StatusCode: http.StatusForbidden, Message: "Access Denied.", Key: "lfs/sha256/ab/ab/secret"}
	err := mapErr(resp, "lfs/sha256/ab/ab/secret")
	coded, ok := errors.AsType[blob.CodedError](err)
	require.True(t, ok, "an unmapped S3 error carries its code")
	require.Equal(t, "s3.AccessDenied", coded.ErrorCode())
	got, ok := errors.AsType[minio.ErrorResponse](err)
	require.True(t, ok, "the response stays reachable")
	require.Equal(t, resp.Code, got.Code)
	require.Equal(t, resp.Error(), err.Error())

	err = mapErr(minio.ErrorResponse{Code: "NoSuchBucket", StatusCode: http.StatusNotFound}, "x")
	coded, ok = errors.AsType[blob.CodedError](err)
	require.True(t, ok)
	require.Equal(t, "s3.NoSuchBucket", coded.ErrorCode())
	require.Contains(t, err.Error(), "bucket does not exist")

	require.ErrorIs(t, mapErr(minio.ErrorResponse{Code: "NoSuchKey"}, "x"), blob.ErrNotFound)
	plain := errors.New("connection reset")
	require.Same(t, plain, mapErr(plain, "x"), "errors other than S3 responses pass through")
	noCode := minio.ErrorResponse{StatusCode: http.StatusInternalServerError}
	_, ok = errors.AsType[blob.CodedError](mapErr(noCode, "x"))
	require.False(t, ok, "a response with no code has nothing to record")
	require.NoError(t, mapErr(nil, "x"))
}

// TestProxyModesRefusePresigning checks that the proxy upload and
// download modes make the store refuse to presign, so every caller falls
// back to URLs that Hangar serves. No request reaches the endpoint.
func TestProxyModesRefusePresigning(t *testing.T) {
	ctx := t.Context()
	s, err := New(Options{Endpoint: "http://minio.invalid:9000", Bucket: "hangar", AccessKeyID: "k", SecretAccessKey: "s", Region: "us-east-1",
		UsePathStyle: true, ProxyUploads: true, ProxyDownloads: true})
	require.NoError(t, err)
	_, err = s.PresignPut(ctx, "lfs/sha256/ab/cd/x", 10, nil, time.Minute)
	require.ErrorIs(t, err, blob.ErrNotSupported)
	_, err = s.PresignPart(ctx, "lfs/sha256/ab/cd/x", "upload", 1, time.Minute)
	require.ErrorIs(t, err, blob.ErrNotSupported)
	_, err = s.PresignGet(ctx, "lfs/sha256/ab/cd/x", time.Minute, "x.bin")
	require.ErrorIs(t, err, blob.ErrNotSupported)

	// Without them the store presigns locally, also with no request.
	s, err = New(Options{Endpoint: "http://minio.invalid:9000", Bucket: "hangar", AccessKeyID: "k", SecretAccessKey: "s", Region: "us-east-1",
		UsePathStyle: true})
	require.NoError(t, err)
	pr, err := s.PresignPut(ctx, "lfs/sha256/ab/cd/x", 10, nil, time.Minute)
	require.NoError(t, err)
	require.Contains(t, pr.URL, "minio.invalid:9000")
	u, err := s.PresignPart(ctx, "lfs/sha256/ab/cd/x", "upload", 1, time.Minute)
	require.NoError(t, err)
	require.Contains(t, u, "partNumber=1")
	u, err = s.PresignGet(ctx, "lfs/sha256/ab/cd/x", time.Minute, "x.bin")
	require.NoError(t, err)
	require.Contains(t, u, "X-Amz-Signature")
}

func TestRetryReason(t *testing.T) {
	require.Equal(t, "stalled", retryReason(fmt.Errorf("upload: %w", errStalled)))
	require.Equal(t, "s3.SlowDown", retryReason(minio.ErrorResponse{Code: "SlowDown", StatusCode: 503}))
	require.Equal(t, "http.502", retryReason(minio.ErrorResponse{StatusCode: 502}))
	require.Equal(t, "connection", retryReason(&url.Error{Op: "Put", URL: "https://h/k?uploadId=1", Err: errors.New("Connection closed by foreign host")}))
	require.Equal(t, "timeout", retryReason(&url.Error{Op: "Put", URL: "https://h/k", Err: timeoutErr{}}))
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "net/http: timeout awaiting response headers" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

func TestRetryable(t *testing.T) {
	ctx := context.Background()
	require.True(t, retryable(ctx, errStalled))
	require.True(t, retryable(ctx, minio.ErrorResponse{Code: "RequestTimeout", StatusCode: 400}))
	require.True(t, retryable(ctx, minio.ErrorResponse{Code: "InternalError", StatusCode: 500}))
	require.True(t, retryable(ctx, minio.ErrorResponse{StatusCode: 503}))
	require.True(t, retryable(ctx, &url.Error{Op: "Put", URL: "https://h", Err: errors.New("use of closed network connection")}))
	require.False(t, retryable(ctx, minio.ErrorResponse{Code: "AccessDenied", StatusCode: 403}))
	require.False(t, retryable(ctx, minio.ErrorResponse{Code: "NoSuchUpload", StatusCode: 404}))
	require.False(t, retryable(ctx, minio.ErrorResponse{Code: "EntityTooSmall", StatusCode: 400}))
	require.False(t, retryable(ctx, blob.ErrSizeMismatch))
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	require.False(t, retryable(cancelled, errors.New("connection reset")))
}

func TestBackoff(t *testing.T) {
	for n := range 10 {
		ceiling := min(time.Second<<n, maxRetryBackoff)
		for range 50 {
			d := backoff(time.Second, n)
			require.GreaterOrEqual(t, d, ceiling/2)
			require.LessOrEqual(t, d, ceiling)
		}
	}
}
