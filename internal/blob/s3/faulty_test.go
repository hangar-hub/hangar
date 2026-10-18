package s3_test

import (
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // S3 ETags are MD5; the fake computes them the same way.
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/blob"
	"github.com/hangar-hub/hangar/internal/blob/s3"
)

// fault is what faultyS3 does to an upload request instead of answering
// it normally.
type fault int

const (
	faultNone fault = iota
	// faultReset reads part of the body, then drops the connection, as a
	// peer or middlebox resetting the connection does.
	faultReset
	// faultStall reads part of the body, then stops reading and never
	// answers, as a link that stops passing packets does.
	faultStall
	// faultSlowResponse reads the whole body and never answers.
	faultSlowResponse
	// faultSlowDown answers 503 SlowDown after reading the body.
	faultSlowDown
	// faultDenied answers 403 AccessDenied, which is final.
	faultDenied
)

// faultyS3 is a minimal S3 server over HTTPS (so minio-go sends bodies
// unsigned and unchunked) that injects a fault into upload requests. It
// supports what the tests use: HEAD bucket and object, single PUT,
// multipart create, part PUT, complete and abort, and GET.
type faultyS3 struct {
	t   *testing.T
	srv *httptest.Server

	mu sync.Mutex
	// faults lists the faults for successive upload requests; after it
	// runs out, uploads succeed.
	faults []fault
	// always applies to every upload request when set.
	always fault
	// uploads counts upload requests (single and part PUTs).
	uploads int
	// headBucket counts HEAD bucket requests.
	headBucket int
	// bucketStatus is the status of HEAD bucket, 200 by default.
	bucketStatus int
	bucketRegion string
	objects      map[string][]byte
	parts        map[string]map[int][]byte
	nextID       int
	done         chan struct{}
}

func newFaultyS3(t *testing.T) *faultyS3 {
	t.Helper()
	f := &faultyS3{t: t, objects: map[string][]byte{}, parts: map[string]map[int][]byte{}, bucketStatus: http.StatusOK, done: make(chan struct{})}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(f.serve))
	t.Cleanup(func() {
		close(f.done)
		f.srv.Close()
	})
	return f
}

// options returns store options for the server with fast retries.
func (f *faultyS3) options(t *testing.T) s3.Options {
	return s3.Options{
		Endpoint: f.srv.URL, Region: "us-east-1", Bucket: "bucket", AccessKeyID: "a", SecretAccessKey: "b",
		UsePathStyle: true, Transport: f.srv.Client().Transport,
		RetryBackoff: time.Millisecond, StallTimeout: time.Second, SpoolDir: t.TempDir(),
	}
}

func (f *faultyS3) store(t *testing.T, opts s3.Options) *s3.Store {
	t.Helper()
	st, err := s3.New(opts)
	require.NoError(t, err)
	return st
}

func (f *faultyS3) setFaults(fs ...fault) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.faults = fs
}

func (f *faultyS3) count() (uploads, headBucket int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.uploads, f.headBucket
}

func (f *faultyS3) nextFault() fault {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.uploads++
	if f.always != faultNone {
		return f.always
	}
	if len(f.faults) == 0 {
		return faultNone
	}
	ft := f.faults[0]
	f.faults = f.faults[1:]
	return ft
}

func s3Error(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, "<Error><Code>%s</Code><Message>%s</Message></Error>", code, code)
}

func etagOf(b []byte) string {
	sum := md5.Sum(b) //nolint:gosec // S3 ETag.
	return `"` + hex.EncodeToString(sum[:]) + `"`
}

func (f *faultyS3) serve(w http.ResponseWriter, r *http.Request) {
	bucket, key, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	q := r.URL.Query()
	if bucket != "bucket" {
		s3Error(w, http.StatusNotFound, "NoSuchBucket")
		return
	}
	switch {
	case key == "" && r.Method == http.MethodHead:
		f.mu.Lock()
		f.headBucket++
		status, region := f.bucketStatus, f.bucketRegion
		f.mu.Unlock()
		if region != "" {
			w.Header().Set("x-amz-bucket-region", region)
		}
		w.WriteHeader(status)
	case r.Method == http.MethodHead:
		f.mu.Lock()
		obj, ok := f.objects[key]
		missing := f.bucketStatus == http.StatusNotFound
		f.mu.Unlock()
		if !ok || missing {
			// AWS answers HEAD with a bare 404 whether the object or the
			// bucket is missing.
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(obj)))
		w.Header().Set("ETag", etagOf(obj))
		w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodGet:
		f.mu.Lock()
		obj, ok := f.objects[key]
		f.mu.Unlock()
		if !ok {
			s3Error(w, http.StatusNotFound, "NoSuchKey")
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(obj)))
		w.Header().Set("ETag", etagOf(obj))
		w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
		_, _ = w.Write(obj)
	case r.Method == http.MethodPost && q.Has("uploads"):
		f.mu.Lock()
		f.nextID++
		id := "upload-" + strconv.Itoa(f.nextID)
		f.parts[id] = map[int][]byte{}
		f.mu.Unlock()
		_, _ = fmt.Fprintf(w, "<InitiateMultipartUploadResult><Bucket>bucket</Bucket><Key>%s</Key><UploadId>%s</UploadId></InitiateMultipartUploadResult>", key, id)
	case r.Method == http.MethodPost && q.Has("uploadId"):
		f.complete(w, r, key, q.Get("uploadId"))
	case r.Method == http.MethodDelete && q.Has("uploadId"):
		f.mu.Lock()
		delete(f.parts, q.Get("uploadId"))
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodDelete:
		f.mu.Lock()
		delete(f.objects, key)
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPut:
		f.put(w, r, key, q)
	default:
		s3Error(w, http.StatusNotImplemented, "NotImplemented")
	}
}

func (f *faultyS3) put(w http.ResponseWriter, r *http.Request, key string, q map[string][]string) {
	switch f.nextFault() {
	case faultReset:
		_, _ = io.CopyN(io.Discard, r.Body, r.ContentLength/2)
		conn, _, err := http.NewResponseController(w).Hijack()
		require.NoError(f.t, err)
		_ = conn.Close()
		return
	case faultStall:
		// Stop reading without closing: the client sees no progress. The
		// server only notices a client hanging up once it has read the
		// body, so hold the connection open here until the test ends.
		_, _ = io.CopyN(io.Discard, r.Body, r.ContentLength/2)
		conn, _, err := http.NewResponseController(w).Hijack()
		require.NoError(f.t, err)
		go func() {
			<-f.done
			_ = conn.Close()
		}()
		return
	case faultSlowResponse:
		_, _ = io.Copy(io.Discard, r.Body)
		f.hang(r)
		return
	case faultSlowDown:
		_, _ = io.Copy(io.Discard, r.Body)
		s3Error(w, http.StatusServiceUnavailable, "SlowDown")
		return
	case faultDenied:
		_, _ = io.Copy(io.Discard, r.Body)
		s3Error(w, http.StatusForbidden, "AccessDenied")
		return
	case faultNone:
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return
	}
	if int64(len(body)) != r.ContentLength {
		s3Error(w, http.StatusBadRequest, "IncompleteBody")
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if id := r.URL.Query().Get("uploadId"); id != "" {
		parts, ok := f.parts[id]
		if !ok {
			s3Error(w, http.StatusNotFound, "NoSuchUpload")
			return
		}
		n, _ := strconv.Atoi(r.URL.Query().Get("partNumber"))
		parts[n] = body
	} else {
		f.objects[key] = body
	}
	w.Header().Set("ETag", etagOf(body))
	w.WriteHeader(http.StatusOK)
}

// hang holds the request until the client gives up on it.
func (f *faultyS3) hang(r *http.Request) {
	select {
	case <-r.Context().Done():
	case <-time.After(30 * time.Second):
		f.t.Error("the client never gave up on a stalled request")
	}
}

func (f *faultyS3) complete(w http.ResponseWriter, r *http.Request, key, id string) {
	var req struct {
		Parts []struct {
			PartNumber int
			ETag       string
		} `xml:"Part"`
	}
	if err := xml.NewDecoder(r.Body).Decode(&req); err != nil {
		s3Error(w, http.StatusBadRequest, "MalformedXML")
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	parts, ok := f.parts[id]
	if !ok {
		s3Error(w, http.StatusNotFound, "NoSuchUpload")
		return
	}
	var obj []byte
	for _, p := range req.Parts {
		b, ok := parts[p.PartNumber]
		if !ok || strings.Trim(etagOf(b), `"`) != strings.Trim(p.ETag, `"`) {
			s3Error(w, http.StatusBadRequest, "InvalidPart")
			return
		}
		obj = append(obj, b...)
	}
	f.objects[key] = obj
	delete(f.parts, id)
	_, _ = fmt.Fprintf(w, "<CompleteMultipartUploadResult><Bucket>bucket</Bucket><Key>%s</Key><ETag>%s</ETag></CompleteMultipartUploadResult>", key, etagOf(obj))
}

// onceReader is a stream that cannot be rewound and fails the test if it
// is read past its end, so a retry that read the source again would show.
type onceReader struct {
	r    io.Reader
	read int
}

func (o *onceReader) Read(p []byte) (int, error) {
	n, err := o.r.Read(p)
	o.read += n
	return n, err
}

func randomPart(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7 + i/251)
	}
	return b
}

func spoolFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// TestPartUploadRetriesFaults sends a streamed part through each fault a
// slow or unreliable link produces and checks the retry stores the exact
// bytes, reads the source once and leaves no spool file behind.
func TestPartUploadRetriesFaults(t *testing.T) {
	ctx := context.Background()
	for name, ft := range map[string]fault{
		"ConnectionReset": faultReset,
		"StallMidBody":    faultStall,
		"NoResponse":      faultSlowResponse,
		"SlowDown":        faultSlowDown,
	} {
		t.Run(name, func(t *testing.T) {
			f := newFaultyS3(t)
			opts := f.options(t)
			st := f.store(t, opts)
			id, err := st.CreateMultipart(ctx, "lfs/k")
			require.NoError(t, err)
			data := randomPart(t, 4<<20)
			f.setFaults(ft, ft)
			src := &onceReader{r: io.MultiReader(bytes.NewReader(data))}
			etag, err := st.PutPart(ctx, "lfs/k", id, 1, src, int64(len(data)))
			require.NoError(t, err)
			require.Equal(t, strings.Trim(etagOf(data), `"`), etag)
			require.Equal(t, len(data), src.read, "the source is read once, whatever the retries")
			uploads, _ := f.count()
			require.Equal(t, 3, uploads, "two failed attempts and one that worked")
			require.Empty(t, spoolFiles(t, opts.SpoolDir), "the spool file is removed")
			require.NoError(t, st.CompleteMultipart(ctx, "lfs/k", id, []blob.Part{{Number: 1, ETag: etag}}))
			requireObject(t, st, "lfs/k", data)
		})
	}
}

// TestSeekablePartIsRewoundNotSpooled checks a seekable body is sent again
// by seeking, so it needs no spool directory.
func TestSeekablePartIsRewoundNotSpooled(t *testing.T) {
	ctx := context.Background()
	f := newFaultyS3(t)
	opts := f.options(t)
	opts.SpoolDir = "/nonexistent/spool"
	st := f.store(t, opts)
	id, err := st.CreateMultipart(ctx, "lfs/k")
	require.NoError(t, err)
	data := randomPart(t, 1<<20)
	f.setFaults(faultReset)
	r := bytes.NewReader(append(bytes.Clone(data), "next part"...))
	etag, err := st.PutPart(ctx, "lfs/k", id, 1, io.NewSectionReader(r, 0, int64(len(data))), int64(len(data)))
	require.NoError(t, err)
	require.Equal(t, strings.Trim(etagOf(data), `"`), etag)

	// A non-seekable body cannot be spooled there, and says so.
	f.setFaults(faultReset)
	_, err = st.PutPart(ctx, "lfs/k", id, 2, io.MultiReader(bytes.NewReader(data)), int64(len(data)))
	require.ErrorContains(t, err, "spool file")
}

func TestPutVerifiedMultipartSurvivesFaults(t *testing.T) {
	ctx := context.Background()
	f := newFaultyS3(t)
	opts := f.options(t)
	st := f.store(t, opts)
	data := randomPart(t, int(blob.DefaultPartSize)+4096)
	h := sha256.Sum256(data)
	sum := h[:]
	// Part 1 is reset, part 2 stalls; both are retried from the spool.
	f.setFaults(faultReset, faultNone, faultStall)
	src := &onceReader{r: io.MultiReader(bytes.NewReader(data))}
	require.NoError(t, blob.PutVerifiedMultipart(ctx, st, "lfs/sha256/big", src, int64(len(data)), sum))
	require.Equal(t, len(data), src.read)
	requireObject(t, st, "lfs/sha256/big", data)
	require.Empty(t, spoolFiles(t, opts.SpoolDir))

	// A single PUT is retried too.
	small := []byte("small object")
	f.setFaults(faultSlowDown, faultReset)
	require.NoError(t, st.Put(ctx, "tmp/small", bytes.NewReader(small), int64(len(small))))
	requireObject(t, st, "tmp/small", small)
}

func TestPartUploadGivesUp(t *testing.T) {
	ctx := context.Background()
	f := newFaultyS3(t)
	opts := f.options(t)
	opts.Retries = 2
	st := f.store(t, opts)
	id, err := st.CreateMultipart(ctx, "lfs/k")
	require.NoError(t, err)
	data := randomPart(t, 64<<10)

	f.always = faultSlowDown
	_, err = st.PutPart(ctx, "lfs/k", id, 1, io.MultiReader(bytes.NewReader(data)), int64(len(data)))
	require.ErrorContains(t, err, "gave up after 3 attempts")
	require.ErrorContains(t, err, "SlowDown")
	uploads, _ := f.count()
	require.Equal(t, 3, uploads, "the first attempt and two retries")
	require.Empty(t, spoolFiles(t, opts.SpoolDir))

	f.always = faultDenied
	_, err = st.PutPart(ctx, "lfs/k", id, 1, io.MultiReader(bytes.NewReader(data)), int64(len(data)))
	require.ErrorContains(t, err, "AccessDenied")
	after, _ := f.count()
	require.Equal(t, uploads+1, after, "a denied request is final")

	f.always = faultNone
	opts.Retries = -1
	once := f.store(t, opts)
	f.setFaults(faultSlowDown)
	_, err = once.PutPart(ctx, "lfs/k", id, 1, io.MultiReader(bytes.NewReader(data)), int64(len(data)))
	require.Error(t, err, "a negative Retries turns retries off")
	last, _ := f.count()
	require.Equal(t, after+1, last)
}

func TestPartUploadSourceProblemsAreFinal(t *testing.T) {
	ctx := context.Background()
	f := newFaultyS3(t)
	st := f.store(t, f.options(t))
	id, err := st.CreateMultipart(ctx, "lfs/k")
	require.NoError(t, err)
	data := randomPart(t, 64<<10)

	_, err = st.PutPart(ctx, "lfs/k", id, 1, io.MultiReader(bytes.NewReader(data[:100])), int64(len(data)))
	require.ErrorIs(t, err, blob.ErrSizeMismatch, "a short source is not retried")
	uploads, _ := f.count()
	require.Equal(t, 1, uploads)

	broken := io.MultiReader(bytes.NewReader(data[:1000]), errReader{})
	_, err = st.PutPart(ctx, "lfs/k", id, 1, broken, int64(len(data)))
	require.ErrorIs(t, err, errSourceBroken, "a failing source is not retried")
	uploads, _ = f.count()
	require.Equal(t, 2, uploads)

	_, err = st.PutPart(ctx, "lfs/k", id, 1, io.MultiReader(bytes.NewReader(data), strings.NewReader("x")), int64(len(data)))
	require.ErrorIs(t, err, blob.ErrSizeMismatch, "a longer source is refused")
}

func TestPartUploadStopsWhenCancelled(t *testing.T) {
	f := newFaultyS3(t)
	opts := f.options(t)
	opts.RetryBackoff = time.Hour
	st := f.store(t, opts)
	id, err := st.CreateMultipart(context.Background(), "lfs/k")
	require.NoError(t, err)
	f.setFaults(faultSlowDown)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = st.PutPart(ctx, "lfs/k", id, 1, strings.NewReader("data"), 4)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Less(t, time.Since(start), 10*time.Second, "the backoff wait ends with the context")
}

var errSourceBroken = errors.New("upstream connection reset")

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errSourceBroken }

func requireObject(t *testing.T, st *s3.Store, key string, want []byte) {
	t.Helper()
	rc, info, err := st.Get(context.Background(), key, nil)
	require.NoError(t, err)
	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	require.NoError(t, rc.Close())
	require.Equal(t, int64(len(want)), info.Size)
	require.True(t, bytes.Equal(want, got), "stored bytes match")
}

// TestCheckExplainsBucketProblems checks the start-up check and Stat
// against the answers AWS S3 gives: a bare 404 for HEAD in a missing
// bucket, 301 with the bucket's region, and 403.
func TestCheckExplainsBucketProblems(t *testing.T) {
	ctx := context.Background()
	f := newFaultyS3(t)
	st := f.store(t, f.options(t))

	require.NoError(t, st.Check(ctx))
	_, err := st.Stat(ctx, "missing")
	require.ErrorIs(t, err, blob.ErrNotFound)
	_, heads := f.count()
	require.Equal(t, 1, heads, "a bucket seen by Check is not checked again on a missing object")

	f.mu.Lock()
	f.bucketStatus = http.StatusNotFound
	f.mu.Unlock()
	missing := f.store(t, f.options(t))
	err = missing.Check(ctx)
	require.ErrorContains(t, err, `bucket "bucket" does not exist`)
	_, err = missing.Stat(ctx, "x")
	require.Error(t, err)
	require.NotErrorIs(t, err, blob.ErrNotFound, "a missing bucket is a configuration error, not a missing object")
	require.ErrorContains(t, err, "does not exist")

	f.mu.Lock()
	f.bucketStatus, f.bucketRegion = http.StatusMovedPermanently, "eu-west-2"
	f.mu.Unlock()
	err = f.store(t, f.options(t)).Check(ctx)
	require.ErrorContains(t, err, `bucket "bucket" is in region "eu-west-2", not "us-east-1": set storage.s3.region to "eu-west-2"`)

	f.mu.Lock()
	f.bucketStatus, f.bucketRegion = http.StatusForbidden, ""
	f.mu.Unlock()
	err = f.store(t, f.options(t)).Check(ctx)
	require.ErrorContains(t, err, `access to bucket "bucket" is denied`)
	coded, ok := errors.AsType[blob.CodedError](err)
	require.True(t, ok, "spans record the S3 error code")
	require.Equal(t, "s3.AccessDenied", coded.ErrorCode())
}
