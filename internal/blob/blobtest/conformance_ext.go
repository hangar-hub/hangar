package blobtest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/blob"
)

// runExtended covers verified writes, key ordering, overwrite and part
// retry semantics, and real presigned URLs.
func runExtended(t *testing.T, caps Capabilities, newStore func(t *testing.T) blob.Store) {
	t.Helper()
	ctx := context.Background()

	t.Run("PutVerified", func(t *testing.T) {
		s := newStore(t)
		data := randomBytes(t, 4096)
		sum := sha256.Sum256(data)
		require.NoError(t, blob.PutVerified(ctx, s, "lfs/sha256/v/ok", bytes.NewReader(data), int64(len(data)), sum[:]))
		requireContent(t, s, "lfs/sha256/v/ok", data)

		bad := sha256.Sum256([]byte("something else"))
		err := blob.PutVerified(ctx, s, "lfs/sha256/v/bad", bytes.NewReader(data), int64(len(data)), bad[:])
		require.ErrorIs(t, err, blob.ErrChecksumMismatch)
		requireMissing(t, s, "lfs/sha256/v/bad")

		err = blob.PutVerified(ctx, s, "lfs/sha256/v/short", bytes.NewReader(data[:100]), int64(len(data)), sum[:])
		require.ErrorIs(t, err, blob.ErrSizeMismatch)
		requireMissing(t, s, "lfs/sha256/v/short")

		err = blob.PutVerified(ctx, s, "lfs/sha256/v/long", bytes.NewReader(append(bytes.Clone(data), 'x')), int64(len(data)), sum[:])
		require.ErrorIs(t, err, blob.ErrSizeMismatch)
		requireMissing(t, s, "lfs/sha256/v/long")

		empty := sha256.Sum256(nil)
		require.NoError(t, blob.PutVerified(ctx, s, "lfs/sha256/v/empty", bytes.NewReader(nil), 0, empty[:]))
		requireContent(t, s, "lfs/sha256/v/empty", []byte{})

		require.Error(t, blob.PutVerified(ctx, s, "lfs/sha256/v/x", bytes.NewReader(data), -1, sum[:]), "the size must be known")
		require.Error(t, blob.PutVerified(ctx, s, "lfs/sha256/v/x", bytes.NewReader(data), 1, []byte{1}), "the sum must be a SHA-256")
	})

	t.Run("PutVerifiedMultipart", func(t *testing.T) {
		s := newStore(t)
		// Two parts at the default part size.
		data := randomBytes(t, int(blob.DefaultPartSize)+1024)
		sum := sha256.Sum256(data)
		require.NoError(t, blob.PutVerifiedMultipart(ctx, s, "lfs/sha256/m/ok", bytes.NewReader(data), int64(len(data)), sum[:]))
		requireContent(t, s, "lfs/sha256/m/ok", data)

		data[len(data)-1] ^= 0xff
		err := blob.PutVerifiedMultipart(ctx, s, "lfs/sha256/m/bad", bytes.NewReader(data), int64(len(data)), sum[:])
		require.ErrorIs(t, err, blob.ErrChecksumMismatch)
		requireMissing(t, s, "lfs/sha256/m/bad")

		err = blob.PutVerifiedMultipart(ctx, s, "lfs/sha256/m/short", bytes.NewReader(data[:1000]), int64(len(data)), sum[:])
		require.ErrorIs(t, err, blob.ErrSizeMismatch)
		requireMissing(t, s, "lfs/sha256/m/short")
	})

	t.Run("EmptyObject", func(t *testing.T) {
		s := newStore(t)
		require.NoError(t, s.Put(ctx, "tmp/empty", bytes.NewReader(nil), 0))
		info, err := s.Stat(ctx, "tmp/empty")
		require.NoError(t, err)
		require.Zero(t, info.Size)
		requireContent(t, s, "tmp/empty", []byte{})
		_, _, err = s.Get(ctx, "tmp/empty", &blob.Range{Start: 0, End: -1})
		require.ErrorIs(t, err, blob.ErrInvalidRange)
	})

	t.Run("ListKeyOrderWithSeparators", func(t *testing.T) {
		s := newStore(t)
		for _, k := range []string{"a/b/x", "a/b-c/x", "a/b.c", "a/b0", "a/bz/y/z"} {
			require.NoError(t, s.Put(ctx, k, strings.NewReader("1"), 1))
		}
		require.Equal(t, []string{"a/b-c/x", "a/b.c", "a/b/x", "a/b0", "a/bz/y/z"}, listKeys(t, s, "a/"))
		require.Equal(t, []string{"a/b-c/x", "a/b.c", "a/b/x", "a/b0", "a/bz/y/z"}, listKeys(t, s, "a/b"))
		require.Equal(t, []string{"a/b/x"}, listKeys(t, s, "a/b/"))
		require.Equal(t, []string{"a/bz/y/z"}, listKeys(t, s, "a/bz/y"))
		require.Empty(t, listKeys(t, s, "nothing/here/"))
	})

	t.Run("StatPrefixIsNotAnObject", func(t *testing.T) {
		s := newStore(t)
		require.NoError(t, s.Put(ctx, "git/r1/pack/a", strings.NewReader("a"), 1))
		_, err := s.Stat(ctx, "git/r1")
		require.ErrorIs(t, err, blob.ErrNotFound)
		_, _, err = s.Get(ctx, "git/r1/pack", nil)
		require.ErrorIs(t, err, blob.ErrNotFound)
	})

	t.Run("ModTime", func(t *testing.T) {
		s := newStore(t)
		before := time.Now().Add(-time.Minute)
		require.NoError(t, s.Put(ctx, "tmp/m", strings.NewReader("m"), 1))
		info, err := s.Stat(ctx, "tmp/m")
		require.NoError(t, err)
		require.True(t, info.ModTime.After(before), "ModTime %s is recent", info.ModTime)
		require.True(t, info.ModTime.Before(time.Now().Add(time.Minute)))
	})

	t.Run("CopyOverwritesAndIsIndependent", func(t *testing.T) {
		s := newStore(t)
		require.NoError(t, s.Put(ctx, "tmp/src", strings.NewReader("new"), 3))
		require.NoError(t, s.Put(ctx, "tmp/dst", strings.NewReader("old!"), 4))
		require.NoError(t, s.Copy(ctx, "tmp/src", "tmp/dst"))
		requireContent(t, s, "tmp/dst", []byte("new"))
		require.NoError(t, s.Delete(ctx, "tmp/src"))
		requireContent(t, s, "tmp/dst", []byte("new"))
		require.NoError(t, s.Put(ctx, "tmp/dst", strings.NewReader("replaced"), 8))
		requireMissing(t, s, "tmp/src")
	})

	t.Run("MultipartPartRetryWithNewContent", func(t *testing.T) {
		s := newStore(t)
		partSize := max(caps.MinPartSize, 1024)
		key := "lfs/sha256/mp/retry"
		id, err := s.CreateMultipart(ctx, key)
		require.NoError(t, err)
		old := randomBytes(t, int(partSize))
		eOld, err := s.PutPart(ctx, key, id, 1, bytes.NewReader(old), int64(len(old)))
		require.NoError(t, err)
		fresh := randomBytes(t, int(partSize))
		eNew, err := s.PutPart(ctx, key, id, 1, bytes.NewReader(fresh), int64(len(fresh)))
		require.NoError(t, err)
		require.NotEqual(t, eOld, eNew)
		listed, err := s.ListParts(ctx, key, id)
		require.NoError(t, err)
		require.Len(t, listed, 1)
		require.Equal(t, strings.Trim(eNew, `"`), strings.Trim(listed[0].ETag, `"`))
		require.Equal(t, partSize, listed[0].Size)
		tail := []byte("tail")
		e2, err := s.PutPart(ctx, key, id, 2, bytes.NewReader(tail), int64(len(tail)))
		require.NoError(t, err)
		require.Error(t, s.CompleteMultipart(ctx, key, id, []blob.Part{{Number: 1, ETag: eOld}, {Number: 2, ETag: e2}}), "a stale ETag is refused")
		require.NoError(t, s.CompleteMultipart(ctx, key, id, []blob.Part{{Number: 1, ETag: eNew}, {Number: 2, ETag: e2}}))
		requireContent(t, s, key, append(fresh, tail...))
	})

	t.Run("MultipartUnknownUpload", func(t *testing.T) {
		s := newStore(t)
		_, err := s.ListParts(ctx, "lfs/sha256/mp/none", "00000000-0000-0000-0000-000000000000")
		require.ErrorIs(t, err, blob.ErrNotFound)
		_, err = s.PutPart(ctx, "lfs/sha256/mp/none", "00000000-0000-0000-0000-000000000000", 1, strings.NewReader("x"), 1)
		require.Error(t, err)
	})

	t.Run("CancelledContext", func(t *testing.T) {
		s := newStore(t)
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		require.Error(t, s.Put(cctx, "tmp/cancelled", strings.NewReader("x"), 1))
		requireMissing(t, s, "tmp/cancelled")
	})

	t.Run("ListUploads", func(t *testing.T) {
		s := newStore(t)
		ul, ok := s.(blob.UploadLister)
		if !caps.ListUploads {
			// Garbage collection then relies on upload rows alone.
			if ok {
				err := ul.ListUploads(ctx, func(blob.IncompleteUpload) error { return nil })
				require.ErrorIs(t, err, blob.ErrNotSupported, "%T lists uploads; set Capabilities.ListUploads", s)
			}
			return
		}
		require.True(t, ok, "%T does not implement blob.UploadLister", s)
		before := time.Now().Add(-time.Minute)
		id, err := s.CreateMultipart(ctx, "lfs/sha256/inc/one")
		require.NoError(t, err)
		_, err = s.PutPart(ctx, "lfs/sha256/inc/one", id, 1, strings.NewReader("x"), 1)
		require.NoError(t, err)
		find := func() *blob.IncompleteUpload {
			var found *blob.IncompleteUpload
			require.NoError(t, ul.ListUploads(ctx, func(u blob.IncompleteUpload) error {
				if u.UploadID == id {
					found = &u
				}
				return nil
			}))
			return found
		}
		u := find()
		require.NotNil(t, u)
		require.Equal(t, "lfs/sha256/inc/one", u.Key)
		require.True(t, u.Initiated.After(before))
		require.NoError(t, s.AbortMultipart(ctx, "lfs/sha256/inc/one", id))
		require.Nil(t, find())
	})

	t.Run("ServedDownloadsAreAttachments", func(t *testing.T) {
		s := newStore(t)
		key := "lfs/sha256/xss/page"
		require.NoError(t, s.Put(ctx, key, bytes.NewReader(xssPage), int64(len(xssPage))))
		d := blob.Downloads{Store: s, Mode: blob.DownloadProxy}
		serve := func(t *testing.T, method, filename string, headers map[string]string) *httptest.ResponseRecorder {
			t.Helper()
			req := httptest.NewRequestWithContext(ctx, method, "/download", nil)
			for k, v := range headers {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			// A caller's Content-Type, such as one guessed from the file
			// name, never survives.
			rec.Header().Set("Content-Type", "text/html")
			require.NoError(t, d.Serve(rec, req, key, filename))
			return rec
		}

		for _, name := range []string{"page.html", "image.svg", ""} {
			rec := serve(t, http.MethodGet, name, nil)
			require.Equal(t, http.StatusOK, rec.Code)
			require.Equal(t, xssPage, rec.Body.Bytes())
			requireAttachment(t, rec.Header(), name)
			require.Equal(t, "nosniff", rec.Header().Get("X-Content-Type-Options"))
			require.Equal(t, strconv.Itoa(len(xssPage)), rec.Header().Get("Content-Length"))
		}

		rec := serve(t, http.MethodGet, "page.html", map[string]string{"Range": "bytes=6-13"})
		require.Equal(t, http.StatusPartialContent, rec.Code)
		require.Equal(t, string(xssPage[6:14]), rec.Body.String())
		require.Equal(t, fmt.Sprintf("bytes 6-13/%d", len(xssPage)), rec.Header().Get("Content-Range"))
		requireAttachment(t, rec.Header(), "page.html")

		rec = serve(t, http.MethodHead, "page.html", nil)
		require.Equal(t, http.StatusOK, rec.Code)
		require.Zero(t, rec.Body.Len())
		require.Equal(t, strconv.Itoa(len(xssPage)), rec.Header().Get("Content-Length"))
		requireAttachment(t, rec.Header(), "page.html")

		rec = serve(t, http.MethodGet, "page.html", map[string]string{"Range": "bytes=999-"})
		require.Equal(t, http.StatusRequestedRangeNotSatisfiable, rec.Code)
		require.Equal(t, fmt.Sprintf("bytes */%d", len(xssPage)), rec.Header().Get("Content-Range"))

		missing := httptest.NewRecorder()
		err := d.Serve(missing, httptest.NewRequestWithContext(ctx, http.MethodGet, "/download", nil), "lfs/sha256/xss/missing", "x")
		require.ErrorIs(t, err, blob.ErrNotFound)
		require.False(t, missing.Flushed)
		require.Empty(t, missing.Header(), "nothing is written for a missing object, so the caller answers in its own format")
	})

	if caps.PresignHTTP {
		runPresignHTTP(t, caps, newStore)
	}
}

// xssPage is an upload a browser would run as script if it were served
// inline.
var xssPage = []byte("<html><script>alert(document.domain)</script></html>")

// requireAttachment checks the headers that stop a browser rendering a
// download: application/octet-stream and an attachment disposition.
func requireAttachment(t *testing.T, h http.Header, filename string) {
	t.Helper()
	require.Equal(t, blob.SafeContentType, h.Get("Content-Type"))
	require.Equal(t, blob.ContentDisposition(filename), h.Get("Content-Disposition"))
	require.True(t, strings.HasPrefix(h.Get("Content-Disposition"), "attachment"))
}

func runPresignHTTP(t *testing.T, caps Capabilities, newStore func(t *testing.T) blob.Store) {
	ctx := context.Background()
	client := &http.Client{Timeout: time.Minute}
	do := func(t *testing.T, method, url string, body []byte, headers map[string]string) httpResult {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
		require.NoError(t, err)
		req.ContentLength = int64(len(body))
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := client.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		return httpResult{StatusCode: resp.StatusCode, Header: resp.Header, Body: b}
	}

	t.Run("PresignGetHTTP", func(t *testing.T) {
		s := newStore(t)
		require.NoError(t, s.Put(ctx, "lfs/sha256/pg/obj", strings.NewReader("presigned bytes"), 15))
		u, err := s.PresignGet(ctx, "lfs/sha256/pg/obj", 10*time.Minute, "model.safetensors")
		require.NoError(t, err)
		resp := do(t, http.MethodGet, u, nil, nil)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.Equal(t, "presigned bytes", string(resp.Body))
		require.Contains(t, resp.Header.Get("Content-Disposition"), "model.safetensors")

		// A range request through the presigned URL, as hf_hub_download resumes.
		resp = do(t, http.MethodGet, u, nil, map[string]string{"Range": "bytes=10-"})
		require.Equal(t, http.StatusPartialContent, resp.StatusCode)
		require.Equal(t, "bytes", string(resp.Body))
	})

	t.Run("PresignPutHTTP", func(t *testing.T) {
		s := newStore(t)
		data := []byte("uploaded through a presigned PUT")
		req, err := s.PresignPut(ctx, "lfs/sha256/pp/plain", int64(len(data)), nil, 10*time.Minute)
		require.NoError(t, err)
		resp := do(t, http.MethodPut, req.URL, data, req.Headers)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		requireContent(t, s, "lfs/sha256/pp/plain", data)

		sum := sha256.Sum256(data)
		req, err = s.PresignPut(ctx, "lfs/sha256/pp/checked", int64(len(data)), sum[:], 10*time.Minute)
		require.NoError(t, err)
		resp = do(t, http.MethodPut, req.URL, data, req.Headers)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		requireContent(t, s, "lfs/sha256/pp/checked", data)

		if caps.ChecksumPresign {
			req, err = s.PresignPut(ctx, "lfs/sha256/pp/corrupt", int64(len(data)), sum[:], 10*time.Minute)
			require.NoError(t, err)
			corrupt := bytes.Clone(data)
			corrupt[0] ^= 0xff
			resp = do(t, http.MethodPut, req.URL, corrupt, req.Headers)
			require.GreaterOrEqual(t, resp.StatusCode, 400, "the store checks the signed SHA-256")
			requireMissing(t, s, "lfs/sha256/pp/corrupt")
		}
	})

	t.Run("PresignedDownloadsAreAttachments", func(t *testing.T) {
		s := newStore(t)
		// An uploader with a plain presigned PUT chooses the stored
		// Content-Type; downloads must not serve it.
		req, err := s.PresignPut(ctx, "lfs/sha256/xss/plain", int64(len(xssPage)), nil, 10*time.Minute)
		require.NoError(t, err)
		require.Equal(t, blob.SafeContentType, req.Headers["Content-Type"])
		resp := do(t, http.MethodPut, req.URL, xssPage, map[string]string{"Content-Type": "text/html"})
		require.Equal(t, http.StatusOK, resp.StatusCode)
		for _, name := range []string{"page.html", ""} {
			u, err := s.PresignGet(ctx, "lfs/sha256/xss/plain", 10*time.Minute, name)
			require.NoError(t, err)
			resp = do(t, http.MethodGet, u, nil, nil)
			require.Equal(t, http.StatusOK, resp.StatusCode)
			require.Equal(t, xssPage, resp.Body)
			requireAttachment(t, resp.Header, name)
		}

		// A presigned PUT that carries a checksum signs its Content-Type:
		// sending another one is refused and stores nothing.
		sum := sha256.Sum256(xssPage)
		req, err = s.PresignPut(ctx, "lfs/sha256/xss/signed", int64(len(xssPage)), sum[:], 10*time.Minute)
		require.NoError(t, err)
		headers := map[string]string{}
		for k, v := range req.Headers {
			headers[k] = v
		}
		headers["Content-Type"] = "image/svg+xml"
		resp = do(t, http.MethodPut, req.URL, xssPage, headers)
		require.GreaterOrEqual(t, resp.StatusCode, 400, "the Content-Type is signed")
		requireMissing(t, s, "lfs/sha256/xss/signed")
		resp = do(t, http.MethodPut, req.URL, xssPage, req.Headers)
		require.Equal(t, http.StatusOK, resp.StatusCode)

		// Parts uploaded through presigned URLs cannot set the type either.
		key := "lfs/sha256/xss/multipart"
		id, err := s.CreateMultipart(ctx, key)
		require.NoError(t, err)
		u, err := s.PresignPart(ctx, key, id, 1, 10*time.Minute)
		require.NoError(t, err)
		resp = do(t, http.MethodPut, u, xssPage, map[string]string{"Content-Type": "text/html"})
		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.NoError(t, s.CompleteMultipart(ctx, key, id, []blob.Part{{Number: 1, ETag: resp.Header.Get("ETag")}}))
		u, err = s.PresignGet(ctx, key, 10*time.Minute, "page.html")
		require.NoError(t, err)
		resp = do(t, http.MethodGet, u, nil, nil)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		requireAttachment(t, resp.Header, "page.html")
	})

	t.Run("PresignPartHTTP", func(t *testing.T) {
		s := newStore(t)
		key := "lfs/sha256/pm/obj"
		id, err := s.CreateMultipart(ctx, key)
		require.NoError(t, err)
		p1 := randomBytes(t, int(max(caps.MinPartSize, 1024)))
		p2 := []byte("last part")
		var parts []blob.Part
		for i, body := range [][]byte{p1, p2} {
			u, err := s.PresignPart(ctx, key, id, i+1, 10*time.Minute)
			require.NoError(t, err)
			resp := do(t, http.MethodPut, u, body, nil)
			require.Equal(t, http.StatusOK, resp.StatusCode)
			parts = append(parts, blob.Part{Number: i + 1, ETag: resp.Header.Get("ETag")})
		}
		listed, err := s.ListParts(ctx, key, id)
		require.NoError(t, err)
		require.Len(t, listed, 2)
		require.NoError(t, s.CompleteMultipart(ctx, key, id, parts))
		requireContent(t, s, key, append(bytes.Clone(p1), p2...))
	})
}

type httpResult struct {
	StatusCode int
	Header     http.Header
	Body       []byte
}

func listKeys(t *testing.T, s blob.Store, prefix string) []string {
	t.Helper()
	var keys []string
	require.NoError(t, s.List(context.Background(), prefix, func(i blob.Info) error {
		keys = append(keys, i.Key)
		return nil
	}))
	return keys
}

func requireContent(t *testing.T, s blob.Store, key string, want []byte) {
	t.Helper()
	rc, info, err := s.Get(context.Background(), key, nil)
	require.NoError(t, err, key)
	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	require.NoError(t, rc.Close())
	require.Equal(t, int64(len(want)), info.Size)
	require.True(t, bytes.Equal(want, got), "content of %s differs", key)
}

func requireMissing(t *testing.T, s blob.Store, key string) {
	t.Helper()
	_, err := s.Stat(context.Background(), key)
	require.ErrorIs(t, err, blob.ErrNotFound, key)
}
