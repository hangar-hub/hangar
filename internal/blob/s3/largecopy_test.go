package s3_test

import (
	"context"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/blob"
	"github.com/jamesagarside/hangar/internal/blob/s3"
)

// sizedS3 is a minimal S3 server whose objects have a size and an ETag but
// no bytes, so a test can copy objects of many gigabytes without storing
// them. It answers HEAD, multipart create, UploadPartCopy, complete and
// abort, and CopyObject, and records every copy request's source range.
type sizedS3 struct {
	t   *testing.T
	srv *httptest.Server

	mu      sync.Mutex
	objects map[string]sizedObject
	uploads map[string]map[int]copyRange
	singles []string // CopyObject requests, by source
	nextID  int
}

type sizedObject struct {
	size int64
	etag string
}

// copyRange is one UploadPartCopy: the source byte range and the ETag the
// request required the source to have.
type copyRange struct {
	src        string
	start, end int64
	ifMatch    string
}

func newSizedS3(t *testing.T) *sizedS3 {
	t.Helper()
	f := &sizedS3{t: t, objects: map[string]sizedObject{}, uploads: map[string]map[int]copyRange{}}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *sizedS3) store(t *testing.T) *s3.Store {
	t.Helper()
	st, err := s3.New(s3.Options{
		Endpoint: f.srv.URL, Region: "us-east-1", Bucket: "bucket", AccessKeyID: "a", SecretAccessKey: "b",
		UsePathStyle: true, Transport: f.srv.Client().Transport, RetryBackoff: time.Millisecond, SpoolDir: t.TempDir(),
	})
	require.NoError(t, err)
	return st
}

func (f *sizedS3) serve(w http.ResponseWriter, r *http.Request) {
	bucket, key, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	q := r.URL.Query()
	if bucket != "bucket" {
		s3Error(w, http.StatusNotFound, "NoSuchBucket")
		return
	}
	switch {
	case key == "" && r.Method == http.MethodHead:
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodHead:
		f.mu.Lock()
		obj, ok := f.objects[key]
		f.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if m := r.Header.Get("If-Match"); m != "" && m != obj.etag {
			w.WriteHeader(http.StatusPreconditionFailed)
			return
		}
		w.Header().Set("Content-Length", strconv.FormatInt(obj.size, 10))
		w.Header().Set("ETag", obj.etag)
		w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodPost && q.Has("uploads"):
		f.mu.Lock()
		f.nextID++
		id := "upload-" + strconv.Itoa(f.nextID)
		f.uploads[id] = map[int]copyRange{}
		f.mu.Unlock()
		_, _ = fmt.Fprintf(w, "<InitiateMultipartUploadResult><Bucket>bucket</Bucket><Key>%s</Key><UploadId>%s</UploadId></InitiateMultipartUploadResult>", key, id)
	case r.Method == http.MethodPut && r.Header.Get("X-Amz-Copy-Source") != "":
		f.copy(w, r, key, q)
	case r.Method == http.MethodPost && q.Has("uploadId"):
		f.complete(w, r, key, q.Get("uploadId"))
	case r.Method == http.MethodDelete && q.Has("uploadId"):
		f.mu.Lock()
		delete(f.uploads, q.Get("uploadId"))
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	default:
		s3Error(w, http.StatusNotImplemented, "NotImplemented")
	}
}

func (f *sizedS3) copy(w http.ResponseWriter, r *http.Request, key string, q url.Values) {
	srcPath, err := url.PathUnescape(r.Header.Get("X-Amz-Copy-Source"))
	if err != nil {
		s3Error(w, http.StatusBadRequest, "InvalidArgument")
		return
	}
	_, src, _ := strings.Cut(strings.TrimPrefix(srcPath, "/"), "/")
	f.mu.Lock()
	defer f.mu.Unlock()
	obj, ok := f.objects[src]
	if !ok {
		s3Error(w, http.StatusNotFound, "NoSuchKey")
		return
	}
	ifMatch := r.Header.Get("X-Amz-Copy-Source-If-Match")
	if ifMatch != "" && strings.Trim(ifMatch, `"`) != strings.Trim(obj.etag, `"`) {
		s3Error(w, http.StatusPreconditionFailed, "PreconditionFailed")
		return
	}
	id := q.Get("uploadId")
	if id == "" {
		// CopyObject: S3 refuses a source over 5 GiB.
		if obj.size > 5<<30 {
			s3Error(w, http.StatusBadRequest, "InvalidRequest")
			return
		}
		f.singles = append(f.singles, src)
		f.objects[key] = sizedObject{size: obj.size, etag: `"copy-of-` + strings.Trim(obj.etag, `"`) + `"`}
		_, _ = fmt.Fprintf(w, "<CopyObjectResult><ETag>%s</ETag><LastModified>%s</LastModified></CopyObjectResult>",
			f.objects[key].etag, time.Now().UTC().Format(time.RFC3339))
		return
	}
	parts, ok := f.uploads[id]
	if !ok {
		s3Error(w, http.StatusNotFound, "NoSuchUpload")
		return
	}
	cr := copyRange{src: src, start: 0, end: obj.size - 1, ifMatch: ifMatch}
	if rng := r.Header.Get("X-Amz-Copy-Source-Range"); rng != "" {
		a, b, ok := strings.Cut(strings.TrimPrefix(rng, "bytes="), "-")
		start, err1 := strconv.ParseInt(a, 10, 64)
		end, err2 := strconv.ParseInt(b, 10, 64)
		if !ok || err1 != nil || err2 != nil || start > end || end >= obj.size {
			s3Error(w, http.StatusBadRequest, "InvalidRange")
			return
		}
		cr.start, cr.end = start, end
	}
	// S3 copies at most 5 GiB per UploadPartCopy.
	if cr.end-cr.start+1 > 5<<30 {
		s3Error(w, http.StatusBadRequest, "InvalidRequest")
		return
	}
	n, _ := strconv.Atoi(q.Get("partNumber"))
	parts[n] = cr
	_, _ = fmt.Fprintf(w, "<CopyPartResult><ETag>\"part-%d\"</ETag><LastModified>%s</LastModified></CopyPartResult>",
		n, time.Now().UTC().Format(time.RFC3339))
}

func (f *sizedS3) complete(w http.ResponseWriter, r *http.Request, key, id string) {
	var req struct {
		Parts []struct{ PartNumber int } `xml:"Part"`
	}
	if err := xml.NewDecoder(r.Body).Decode(&req); err != nil {
		s3Error(w, http.StatusBadRequest, "MalformedXML")
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	parts, ok := f.uploads[id]
	if !ok {
		s3Error(w, http.StatusNotFound, "NoSuchUpload")
		return
	}
	var size int64
	for _, p := range req.Parts {
		cr, ok := parts[p.PartNumber]
		if !ok {
			s3Error(w, http.StatusBadRequest, "InvalidPart")
			return
		}
		size += cr.end - cr.start + 1
	}
	f.objects[key] = sizedObject{size: size, etag: fmt.Sprintf(`"assembled-%d"`, len(req.Parts))}
	_, _ = fmt.Fprintf(w, "<CompleteMultipartUploadResult><Bucket>bucket</Bucket><Key>%s</Key><ETag>%s</ETag></CompleteMultipartUploadResult>",
		key, f.objects[key].etag)
}

// ranges returns every UploadPartCopy made, in part order, across uploads.
func (f *sizedS3) ranges() []copyRange {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []copyRange
	for _, parts := range f.uploads {
		nums := make([]int, 0, len(parts))
		for n := range parts {
			nums = append(nums, n)
		}
		sort.Ints(nums)
		for _, n := range nums {
			out = append(out, parts[n])
		}
	}
	return out
}

// TestCopyMultiGigabyteObjects covers Copy, which completing an upload
// uses to move a verified object into place, for the object sizes
// bellatrix finding 13 was about: just over 2 GiB, and over the 5 GiB that
// S3 copies in one request. Each copy is split into ranged UploadPartCopy
// requests of storage.s3 copy part size (512 MiB) that cover every byte
// once and each require the source's ETag, so no request copies gigabytes
// and a source that changes mid-copy fails it.
func TestCopyMultiGigabyteObjects(t *testing.T) {
	t.Parallel()
	const partSize = 512 << 20
	for _, size := range []int64{2<<30 + 12345, 6<<30 + 12345, 5 << 30} {
		t.Run(strconv.FormatInt(size, 10), func(t *testing.T) {
			t.Parallel()
			f := newSizedS3(t)
			st := f.store(t)
			f.objects["tmp/uploads/big"] = sizedObject{size: size, etag: `"source-etag"`}

			require.NoError(t, st.Copy(context.Background(), "tmp/uploads/big", "lfs/sha256/big"))
			require.Empty(t, f.singles, "no single CopyObject request")
			got := f.ranges()
			require.Len(t, got, int((size+partSize-1)/partSize))
			next := int64(0)
			for i, cr := range got {
				require.Equal(t, "tmp/uploads/big", cr.src)
				require.Equal(t, next, cr.start, "part %d starts where the last ended", i+1)
				require.LessOrEqual(t, cr.end-cr.start+1, int64(partSize), "part %d", i+1)
				require.Equal(t, "source-etag", strings.Trim(cr.ifMatch, `"`), "part %d requires the source's ETag", i+1)
				next = cr.end + 1
			}
			require.Equal(t, size, next, "the parts cover the whole object")
			info, err := st.Stat(context.Background(), "lfs/sha256/big")
			require.NoError(t, err)
			require.Equal(t, size, info.Size)
		})
	}
}

// TestCopyRefusesASourceThatChanges checks that a multi-gigabyte copy
// whose source is replaced after Copy looked at it fails rather than
// assembling parts of two objects.
func TestCopyRefusesASourceThatChanges(t *testing.T) {
	t.Parallel()
	f := newSizedS3(t)
	st := f.store(t)
	f.objects["tmp/uploads/big"] = sizedObject{size: 3 << 30, etag: `"first"`}
	inner := f.srv.Config.Handler
	var once sync.Once
	f.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Amz-Copy-Source") != "" {
			once.Do(func() {
				f.mu.Lock()
				f.objects["tmp/uploads/big"] = sizedObject{size: 3 << 30, etag: `"second"`}
				f.mu.Unlock()
			})
		}
		inner.ServeHTTP(w, r)
	})
	err := st.Copy(context.Background(), "tmp/uploads/big", "lfs/sha256/big")
	require.Error(t, err)
	_, err = st.Stat(context.Background(), "lfs/sha256/big")
	require.ErrorIs(t, err, blob.ErrNotFound)
}
