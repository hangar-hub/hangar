package s3

import (
	"errors"
	"net/http"
	"testing"

	"github.com/minio/minio-go/v7"
	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/blob"
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
