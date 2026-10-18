package repo_test

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/blob"
	"github.com/jamesagarside/hangar/internal/db"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/repo"
)

// servePresigned makes the env's Blob store presign URLs and serves them
// with S3's signed-header rules (blobtest.Fake.ServeHTTP).
func servePresigned(t *testing.T, e *env) {
	t.Helper()
	srv := httptest.NewServer(e.blobs)
	t.Cleanup(srv.Close)
	e.blobs.PresignBaseURL = srv.URL + "/bucket"
}

// putURL sends body to a presigned upload with only the headers the URL
// lists, as git-lfs does, or none, as huggingface_hub does on a basic
// upload.
func putURL(t *testing.T, req blob.PresignedRequest, body []byte, withHeaders bool) int {
	t.Helper()
	r, err := http.NewRequest(http.MethodPut, req.URL, bytes.NewReader(body))
	require.NoError(t, err)
	if withHeaders {
		for k, v := range req.Headers {
			r.Header.Set(k, v)
		}
	}
	resp, err := http.DefaultClient.Do(r)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	return resp.StatusCode
}

func stagedUploads(t *testing.T, e *env, oid string) []db.Upload {
	t.Helper()
	var rows []db.Upload
	require.NoError(t, e.d.NewSelect().Model(&rows).Where("oid = ?", oid).Scan(e.ctx))
	return rows
}

// TestPresignedPutWorksWithoutHeaders is the regression test for bellatrix
// finding 1: huggingface_hub uploads LFS files of 10 to 64 MiB with one PUT
// and sends no headers, and the URL signed Content-Type and
// x-amz-checksum-sha256, so MinIO refused every such upload. The URL now
// signs nothing, writes to a staging key, and Verify checks the content
// before it reaches the object's key.
func TestPresignedPutWorksWithoutHeaders(t *testing.T) {
	forEach(t, func(t *testing.T, e *env) {
		servePresigned(t, e)
		for _, withHeaders := range []bool{false, true} {
			content := []byte("basic upload, headers sent: " + map[bool]string{false: "none", true: "the listed ones"}[withHeaders])
			oid := sha256Hex(content)
			req, err := e.lfs.PresignPut(e.ctx, oid, int64(len(content)), time.Hour, uuid.Nil)
			require.NoError(t, err)
			require.NotContains(t, req.Headers, "x-amz-checksum-sha256", "no checksum header to leave out")
			require.Equal(t, http.StatusOK, putURL(t, req, content, withHeaders))

			// Nothing reaches the object's key until Verify has checked it.
			_, ok := e.blobs.Bytes(model.LFSObjectKey(oid))
			require.False(t, ok, "the PUT writes to a staging key")
			_, err = e.lfs.Stat(e.ctx, oid)
			require.ErrorIs(t, err, repo.ErrNotFound)

			obj, err := e.lfs.Verify(e.ctx, oid, int64(len(content)), uuid.Nil)
			require.NoError(t, err)
			require.Equal(t, repo.LFSObject{OID: oid, Size: int64(len(content)), StorageKey: model.LFSObjectKey(oid), Verified: true}, obj)
			stored, ok := e.blobs.Bytes(model.LFSObjectKey(oid))
			require.True(t, ok)
			require.Equal(t, content, stored)
			require.Empty(t, stagedUploads(t, e, oid), "the staged object and its row are removed")
		}
		require.Zero(t, e.blobs.Calls("ServeRefused"))
	})
}

// TestPresignedPutCannotReplaceAVerifiedObject covers what signing the
// checksum used to guarantee: a client cannot store other bytes under an
// object's SHA-256, even one that holds a URL for it after the object has
// been verified, because PUTs go to a staging key and only a matching
// object is copied into place.
func TestPresignedPutCannotReplaceAVerifiedObject(t *testing.T) {
	forEach(t, func(t *testing.T, e *env) {
		servePresigned(t, e)
		content := []byte("the real weights")
		oid := sha256Hex(content)
		size := int64(len(content))
		honest, err := e.lfs.PresignPut(e.ctx, oid, size, time.Hour, uuid.Nil)
		require.NoError(t, err)
		late, err := e.lfs.PresignPut(e.ctx, oid, size, time.Hour, uuid.Nil)
		require.NoError(t, err)
		require.NotEqual(t, honest.URL, late.URL, "every URL has its own staging key")

		require.Equal(t, http.StatusOK, putURL(t, honest, content, false))
		_, err = e.lfs.Verify(e.ctx, oid, size, uuid.Nil)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, putURL(t, late, []byte("not the weights!"), false))
		_, err = e.lfs.Verify(e.ctx, oid, size, uuid.Nil)
		require.ErrorIs(t, err, repo.ErrChecksumMismatch, "the late PUT proves nothing")
		require.Empty(t, stagedUploads(t, e, oid), "and is discarded")
		obj, err := e.lfs.Stat(e.ctx, oid)
		require.NoError(t, err)
		require.True(t, obj.Verified)
		stored, _ := e.blobs.Bytes(model.LFSObjectKey(oid))
		require.Equal(t, content, stored, "the verified object is untouched")

		// A staged PUT nobody verifies goes when its upload expires.
		never, err := e.lfs.PresignPut(e.ctx, oid, size, time.Hour, uuid.Nil)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, putURL(t, never, []byte("not the weights!"), false))
		rows := stagedUploads(t, e, oid)
		require.Len(t, rows, 1)
		_, err = e.d.NewUpdate().Model((*db.Upload)(nil)).Set("expires_at = ?", time.Now().Add(-time.Minute)).Where("id = ?", rows[0].ID).Exec(e.ctx)
		require.NoError(t, err)
		n, err := e.lfs.ExpireUploads(e.ctx)
		require.NoError(t, err)
		require.Equal(t, 1, n)
		_, ok := e.blobs.Bytes(rows[0].StorageKey)
		require.False(t, ok)
		stored, _ = e.blobs.Bytes(model.LFSObjectKey(oid))
		require.Equal(t, content, stored)
	})
}

// TestPresignedPutVerifyRejectsWrongContent covers a PUT of other bytes, or
// none: Verify discards a wrong staged object and never records it.
func TestPresignedPutVerifyRejectsWrongContent(t *testing.T) {
	forEach(t, func(t *testing.T, e *env) {
		servePresigned(t, e)
		content := []byte("expected bytes")
		oid := sha256Hex(content)
		size := int64(len(content))

		_, err := e.lfs.Verify(e.ctx, oid, size, uuid.Nil)
		require.ErrorIs(t, err, repo.ErrNotFound, "nothing was uploaded")

		req, err := e.lfs.PresignPut(e.ctx, oid, size, time.Hour, uuid.Nil)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, putURL(t, req, []byte("other  bytes!!"), false))
		_, err = e.lfs.Verify(e.ctx, oid, size, uuid.Nil)
		require.ErrorIs(t, err, repo.ErrChecksumMismatch)
		require.Empty(t, stagedUploads(t, e, oid))
		_, ok := e.blobs.Bytes(model.LFSObjectKey(oid))
		require.False(t, ok)
		_, err = e.lfs.Stat(e.ctx, oid)
		require.ErrorIs(t, err, repo.ErrNotFound)

		// A good upload after a bad one is verified.
		req, err = e.lfs.PresignPut(e.ctx, oid, size, time.Hour, uuid.Nil)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, putURL(t, req, content, false))
		_, err = e.lfs.Verify(e.ctx, oid, size, uuid.Nil)
		require.NoError(t, err)

		_, err = e.lfs.PresignPut(e.ctx, strings.ToUpper(oid), size, time.Hour, uuid.Nil)
		require.ErrorIs(t, err, repo.ErrInvalid)
	})
}
