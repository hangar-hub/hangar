package blobtest_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/blob"
	"github.com/jamesagarside/hangar/internal/blob/blobtest"
	blobfs "github.com/jamesagarside/hangar/internal/blob/fs"
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

// serveFake returns a Fake whose presigned URLs point at a test server
// running it.
func serveFake(t *testing.T) *blobtest.Fake {
	t.Helper()
	f := blobtest.NewFake()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	f.PresignBaseURL = srv.URL + "/bucket"
	return f
}

func TestFakeConformanceWithPresignedHTTP(t *testing.T) {
	blobtest.RunConformance(t, blobtest.Capabilities{Presign: true, PresignHTTP: true, ChecksumPresign: true},
		func(t *testing.T) blob.Store { return serveFake(t) })
}

func TestFakeServesPresignedURLsLikeS3(t *testing.T) {
	ctx := context.Background()
	f := serveFake(t)
	require.NoError(t, f.Put(ctx, "lfs/obj", strings.NewReader("hello"), 5))
	send := func(method, u string, body string, headers map[string]string) *http.Response {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, method, u, strings.NewReader(body))
		require.NoError(t, err)
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		_, _ = io.Copy(io.Discard, resp.Body)
		require.NoError(t, resp.Body.Close())
		return resp
	}

	get, err := f.PresignGet(ctx, "lfs/obj", time.Hour, "")
	require.NoError(t, err)
	resp := send(http.MethodHead, get, "", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "5", resp.Header.Get("Content-Length"))
	require.NotEmpty(t, resp.Header.Get("ETag"))
	require.Equal(t, http.StatusMethodNotAllowed, send(http.MethodDelete, get, "", nil).StatusCode)

	expired, err := f.PresignGet(ctx, "lfs/obj", -time.Minute, "")
	require.NoError(t, err)
	require.Equal(t, http.StatusForbidden, send(http.MethodGet, expired, "", nil).StatusCode, "an expired URL")
	unsigned := strings.Split(get, "?")[0]
	require.Equal(t, http.StatusForbidden, send(http.MethodGet, unsigned, "", nil).StatusCode, "a URL that was never signed")
	require.NoError(t, f.Delete(ctx, "lfs/obj"))
	require.Equal(t, http.StatusNotFound, send(http.MethodGet, get, "", nil).StatusCode, "an object deleted since signing")
	outside := strings.Replace(get, "/bucket/", "/other/", 1)
	require.Equal(t, http.StatusNotFound, send(http.MethodGet, outside, "", nil).StatusCode, "a path outside the bucket")

	sum := sha256.Sum256([]byte("checked"))
	put, err := f.PresignPut(ctx, "lfs/checked", 7, sum[:], time.Hour)
	require.NoError(t, err)
	require.Equal(t, base64.StdEncoding.EncodeToString(sum[:]), put.Headers["x-amz-checksum-sha256"], "base64, as S3 expects")
	require.Equal(t, http.StatusForbidden, send(http.MethodPut, put.URL, "checked", nil).StatusCode, "the signed checksum header is missing")
	require.Equal(t, http.StatusBadRequest, send(http.MethodPut, put.URL, "checkeD", put.Headers).StatusCode, "the content does not match")
	require.Equal(t, http.StatusOK, send(http.MethodPut, put.URL, "checked", put.Headers).StatusCode)
	b, ok := f.Bytes("lfs/checked")
	require.True(t, ok)
	require.Equal(t, "checked", string(b))
	plain, err := f.PresignPut(ctx, "lfs/plain", 3, nil, time.Hour)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, send(http.MethodPut, plain.URL, "abc", nil).StatusCode, "no checksum was signed")
	_, err = f.PresignPut(ctx, "lfs/x", 1, []byte{1, 2}, time.Hour)
	require.Error(t, err, "a checksum that is not a SHA-256")

	id, err := f.CreateMultipart(ctx, "lfs/multi")
	require.NoError(t, err)
	part, err := f.PresignPart(ctx, "lfs/multi", id, 1, time.Hour)
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, send(http.MethodPut, strings.Replace(part, "partNumber=1", "partNumber=x", 1), "p", nil).StatusCode)
	require.Equal(t, http.StatusBadRequest, send(http.MethodPut, strings.Replace(part, "partNumber=1", "partNumber=0", 1), "p", nil).StatusCode)
	require.Equal(t, http.StatusNotFound, send(http.MethodPut, strings.Replace(part, id, "gone", 1), "p", nil).StatusCode)
	require.Equal(t, 4, f.Calls("ServePut"))
	require.Equal(t, 2, f.Calls("ServeGet"))

	disabled := blobtest.NewFake()
	rec := httptest.NewRecorder()
	disabled.ServeHTTP(rec, httptest.NewRequestWithContext(ctx, http.MethodGet, "/bucket/lfs/obj", nil))
	require.Equal(t, http.StatusNotImplemented, rec.Code, "a Fake without presigning")
}

func TestRunStreamingPassesAStreamingStore(t *testing.T) {
	s, err := blobfs.New(filepath.Join(t.TempDir(), "blobs"))
	require.NoError(t, err)
	blobtest.RunStreaming(t, s, 16<<20, 8<<20)
}

// recordingTB records failures instead of failing the test, so a test can
// check that a checker fails.
type recordingTB struct {
	testing.TB
	mu     sync.Mutex
	failed bool
	msgs   []string
}

func (r *recordingTB) Helper() {}

func (r *recordingTB) Logf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.msgs = append(r.msgs, fmt.Sprintf(format, args...))
}

func (r *recordingTB) Errorf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failed = true
	r.msgs = append(r.msgs, fmt.Sprintf(format, args...))
}

func (r *recordingTB) FailNow() {
	r.mu.Lock()
	r.failed = true
	r.mu.Unlock()
	runtime.Goexit()
}

func (r *recordingTB) Fatalf(format string, args ...any) {
	r.Errorf(format, args...)
	r.FailNow()
}

func TestRunStreamingCatchesAStoreThatBuffers(t *testing.T) {
	// The Fake holds every object in memory, as a store that buffers would.
	rec := &recordingTB{TB: t}
	done := make(chan struct{})
	go func() {
		defer close(done)
		blobtest.RunStreaming(rec, blobtest.NewFake(), 16<<20, 8<<20)
	}()
	<-done
	require.True(t, rec.failed, "RunStreaming must fail a store that buffers objects")
	require.Contains(t, strings.Join(rec.msgs, "\n"), "PutVerified streams")
}

func TestS3Enabled(t *testing.T) {
	t.Setenv(blobtest.EnvS3Endpoint, "")
	t.Setenv(blobtest.EnvMinIO, "")
	require.False(t, blobtest.S3Enabled())
	t.Setenv(blobtest.EnvMinIO, "1")
	require.True(t, blobtest.S3Enabled())
	t.Setenv(blobtest.EnvMinIO, "")
	t.Setenv(blobtest.EnvS3Endpoint, "http://127.0.0.1:9000")
	require.True(t, blobtest.S3Enabled())
}

// TestS3Helpers checks the S3 test helpers against the test server. Like
// the S3 backend's tests, it is registered only when S3 tests are enabled
// (`make test` enables them).
func TestS3Helpers(t *testing.T) {
	if !blobtest.S3Enabled() {
		t.Logf("S3 tests not registered: set %s=1 (needs Docker) or %s", blobtest.EnvMinIO, blobtest.EnvS3Endpoint)
		return
	}
	ctx := context.Background()
	srv := blobtest.S3TestServer(t)
	require.Equal(t, srv, blobtest.S3TestServer(t), "one shared server per test process")
	opts := srv.Options()
	require.Equal(t, srv.Bucket, opts.Bucket)
	require.True(t, opts.UsePathStyle)
	require.Empty(t, opts.Prefix)

	var first blob.Store
	t.Run("store", func(t *testing.T) {
		first = blobtest.S3(t)
		require.NoError(t, first.Put(ctx, "a/b", strings.NewReader("x"), 1))
		second := blobtest.S3(t)
		var keys []string
		require.NoError(t, second.List(ctx, "", func(i blob.Info) error { keys = append(keys, i.Key); return nil }))
		require.Empty(t, keys, "each store has its own prefix")
	})
	var left []string
	require.NoError(t, first.List(ctx, "", func(i blob.Info) error { left = append(left, i.Key); return nil }))
	require.Empty(t, left, "the store's objects are removed when its test ends")
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
	sum := sha256.Sum256([]byte("hello"))
	req, err := f.PresignPut(ctx, "a/c", 5, sum[:], time.Hour)
	require.NoError(t, err)
	require.Equal(t, base64.StdEncoding.EncodeToString(sum[:]), req.Headers["x-amz-checksum-sha256"])

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
