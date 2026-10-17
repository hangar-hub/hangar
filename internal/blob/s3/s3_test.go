package s3_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/blob"
	"github.com/jamesagarside/hangar/internal/blob/blobtest"
	"github.com/jamesagarside/hangar/internal/blob/s3"
)

func TestConformanceMinIO(t *testing.T) {
	blobtest.RequireS3(t)
	blobtest.RunConformance(t, blobtest.Capabilities{
		Presign:         true,
		MinPartSize:     blob.MinPartSize,
		PresignHTTP:     true,
		ChecksumPresign: true,
	}, func(t *testing.T) blob.Store { return blobtest.S3(t) })
}

func TestPutUnknownSizeStreamsInParts(t *testing.T) {
	blobtest.RequireS3(t)
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
	blobtest.RequireS3(t)
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
	blobtest.RequireS3(t)
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
	blobtest.RequireS3(t)
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
	require.Equal(t, blob.SafeContentType, req.Headers["Content-Type"])
	signed := signedHeaders(t, req.URL)
	require.Contains(t, signed, "content-type", "with a checksum, the Content-Type is signed too")
	require.Contains(t, signed, "x-amz-checksum-sha256")
	plain, err := st.PresignPut(ctx, "lfs/x", 1, nil, time.Hour)
	require.NoError(t, err)
	require.Equal(t, blob.SafeContentType, plain.Headers["Content-Type"])
	require.NotContains(t, signedHeaders(t, plain.URL), "content-type",
		"without a checksum nothing is signed: huggingface_hub sends no headers on a basic upload")
	for _, name := range []string{"naïve model.gguf", ""} {
		u, err := st.PresignGet(ctx, "lfs/x", time.Hour, name)
		require.NoError(t, err)
		q := mustQuery(t, u)
		require.Equal(t, blob.SafeContentType, q.Get("response-content-type"), "every presigned GET forces the type")
		require.Equal(t, blob.ContentDisposition(name), q.Get("response-content-disposition"))
		require.True(t, strings.HasPrefix(q.Get("response-content-disposition"), "attachment"))
	}
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

func TestStreamsLargeObjects(t *testing.T) {
	blobtest.RequireS3(t)
	// 48 MiB is three 16 MiB parts, each streamed without a part buffer.
	blobtest.RunStreaming(t, blobtest.S3(t), 48<<20, 16<<20)
}

func mustQuery(t *testing.T, raw string) url.Values {
	t.Helper()
	u, err := url.Parse(raw)
	require.NoError(t, err)
	return u.Query()
}

func signedHeaders(t *testing.T, raw string) []string {
	t.Helper()
	return strings.Split(mustQuery(t, raw).Get("X-Amz-SignedHeaders"), ";")
}

// TestStoredContentTypeIsPinned checks that every write Hangar makes stores
// application/octet-stream, including a server-side copy of an object an
// uploader stored as text/html, so no stored object carries a type a
// browser would render.
func TestStoredContentTypeIsPinned(t *testing.T) {
	blobtest.RequireS3(t)
	ctx := context.Background()
	srv := blobtest.S3TestServer(t)
	opts := srv.Options()
	opts.Prefix = fmt.Sprintf("ctype-%d", time.Now().UnixNano())
	st, err := s3.New(opts)
	require.NoError(t, err)
	require.NoError(t, st.EnsureBucket(ctx, srv.Region))
	t.Cleanup(func() {
		_ = st.List(context.Background(), "", func(i blob.Info) error { return st.Delete(context.Background(), i.Key) })
	})
	host, secure := strings.TrimPrefix(strings.TrimPrefix(srv.Endpoint, "http://"), "https://"), strings.HasPrefix(srv.Endpoint, "https://")
	raw, err := minio.New(host, &minio.Options{Creds: credentials.NewStaticV4(srv.AccessKey, srv.SecretKey, ""), Secure: secure, Region: srv.Region, BucketLookup: minio.BucketLookupPath})
	require.NoError(t, err)
	stored := func(key string) string {
		t.Helper()
		oi, err := raw.StatObject(ctx, srv.Bucket, opts.Prefix+"/"+key, minio.StatObjectOptions{})
		require.NoError(t, err, key)
		return oi.ContentType
	}

	html := []byte("<html><script>alert(document.domain)</script></html>")
	require.NoError(t, st.Put(ctx, "tmp/put", bytes.NewReader(html), int64(len(html))))
	sum := sha256.Sum256(html)
	require.NoError(t, st.PutVerified(ctx, "lfs/sha256/verified", bytes.NewReader(html), int64(len(html)), sum[:]))
	id, err := st.CreateMultipart(ctx, "lfs/sha256/multipart")
	require.NoError(t, err)
	etag, err := st.PutPart(ctx, "lfs/sha256/multipart", id, 1, bytes.NewReader(html), int64(len(html)))
	require.NoError(t, err)
	require.NoError(t, st.CompleteMultipart(ctx, "lfs/sha256/multipart", id, []blob.Part{{Number: 1, ETag: etag}}))

	// An uploader with a plain presigned PUT can choose the stored type;
	// a copy of that object must not keep it.
	_, err = raw.PutObject(ctx, srv.Bucket, opts.Prefix+"/uploads/evil", bytes.NewReader(html), int64(len(html)), minio.PutObjectOptions{ContentType: "text/html"})
	require.NoError(t, err)
	require.Equal(t, "text/html", stored("uploads/evil"))
	require.NoError(t, st.Copy(ctx, "uploads/evil", "lfs/sha256/copied"))

	for _, key := range []string{"tmp/put", "lfs/sha256/verified", "lfs/sha256/multipart", "lfs/sha256/copied"} {
		require.Equal(t, blob.SafeContentType, stored(key), key)
	}
}

// flakyTransport breaks the first attempt of every part upload halfway
// through its body, as a reset connection on a poor link does.
type flakyTransport struct {
	base http.RoundTripper
	mu   sync.Mutex
	seen map[string]bool
	hits int
}

func (f *flakyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	q := req.URL.Query()
	if req.Method == http.MethodPut && q.Has("partNumber") {
		id := req.URL.Path + "#" + q.Get("uploadId") + "#" + q.Get("partNumber")
		f.mu.Lock()
		first := !f.seen[id]
		f.seen[id] = true
		if first {
			f.hits++
		}
		f.mu.Unlock()
		if first && req.Body != nil {
			req = req.Clone(req.Context())
			req.Body = &breakingBody{r: req.Body, left: req.ContentLength / 2}
		}
	}
	return f.base.RoundTrip(req)
}

type breakingBody struct {
	r    io.ReadCloser
	left int64
}

func (b *breakingBody) Read(p []byte) (int, error) {
	if b.left <= 0 {
		return 0, errors.New("injected: connection reset by peer")
	}
	n, err := b.r.Read(p[:min(int64(len(p)), b.left)])
	b.left -= int64(n)
	return n, err
}

func (b *breakingBody) Close() error { return b.r.Close() }

// TestPartRetriesOnMinIO breaks the first attempt of every part against a
// real S3 server, which checks each retried request's signature and
// length, and checks streamed uploads still stream and store the right
// bytes.
func TestPartRetriesOnMinIO(t *testing.T) {
	blobtest.RequireS3(t)
	srv := blobtest.S3TestServer(t)
	if !strings.HasPrefix(srv.Endpoint, "http://") {
		t.Skipf("fault injection needs a plain HTTP test server; %s is not one (scripts/with-test-minio.sh starts one)", srv.Endpoint)
	}
	base, err := minio.DefaultTransport(false)
	require.NoError(t, err)
	flaky := &flakyTransport{base: base, seen: map[string]bool{}}
	opts := srv.Options()
	opts.Prefix = fmt.Sprintf("flaky-%d", time.Now().UnixNano())
	opts.Transport = flaky
	opts.RetryBackoff = 10 * time.Millisecond
	opts.SpoolDir = t.TempDir()
	st, err := s3.New(opts)
	require.NoError(t, err)
	require.NoError(t, st.EnsureBucket(context.Background(), srv.Region))
	t.Cleanup(func() {
		_ = st.List(context.Background(), "", func(i blob.Info) error { return st.Delete(context.Background(), i.Key) })
	})
	blobtest.RunStreaming(t, st, 48<<20, 16<<20)
	flaky.mu.Lock()
	hits := flaky.hits
	flaky.mu.Unlock()
	require.GreaterOrEqual(t, hits, 6, "every part of both uploads failed once")
	entries, err := os.ReadDir(opts.SpoolDir)
	require.NoError(t, err)
	require.Empty(t, entries, "no spool file is left behind")
}

// TestCopyLargeObjectsInParts checks that Copy splits an object larger
// than the copy part size into several server-side copy requests, so no
// single request copies gigabytes (bellatrix finding 13b), and that the
// copy has the source's bytes and the safe Content-Type.
func TestCopyLargeObjectsInParts(t *testing.T) {
	blobtest.RequireS3(t)
	ctx := context.Background()
	srv := blobtest.S3TestServer(t)
	opts := srv.Options()
	opts.Prefix = "copy-" + strings.ReplaceAll(uuid.NewString(), "-", "")
	opts.CopyPartSize = blob.MinPartSize
	st, err := s3.New(opts)
	require.NoError(t, err)
	require.NoError(t, st.EnsureBucket(ctx, srv.Region))
	t.Cleanup(func() {
		_ = st.List(context.Background(), "", func(i blob.Info) error { return st.Delete(context.Background(), i.Key) })
	})

	data := make([]byte, 2*blob.MinPartSize+12345)
	_, err = rand.Read(data)
	require.NoError(t, err)
	require.NoError(t, st.Put(ctx, "tmp/uploads/big", bytes.NewReader(data), int64(len(data))))
	require.NoError(t, st.Copy(ctx, "tmp/uploads/big", "lfs/sha256/big"))
	rc, info, err := st.Get(ctx, "lfs/sha256/big", nil)
	require.NoError(t, err)
	got, err := io.ReadAll(rc)
	require.NoError(t, rc.Close())
	require.NoError(t, err)
	require.Equal(t, int64(len(data)), info.Size)
	require.True(t, bytes.Equal(data, got), "the copy has the source's bytes")
	require.True(t, strings.HasSuffix(info.ETag, "-3"), "a multipart copy of three parts, not one request: ETag %s", info.ETag)

	require.ErrorIs(t, st.Copy(ctx, "tmp/uploads/missing", "lfs/sha256/x"), blob.ErrNotFound)
	_, err = s3.New(s3.Options{Bucket: "b", CopyPartSize: 1 << 20})
	require.Error(t, err, "a copy part size below S3's minimum part size")
}
