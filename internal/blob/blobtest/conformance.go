package blobtest

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/blob"
)

// Capabilities says which optional behaviour a backend has, so the suite
// checks ErrNotSupported where it applies and the real behaviour otherwise.
type Capabilities struct {
	// Presign is true for backends that presign URLs (S3).
	Presign bool
	// MinPartSize is the smallest non-final multipart part the backend
	// accepts; S3 needs 5 MiB. Zero means any size.
	MinPartSize int64
}

// RunConformance runs the Blob store conformance suite. newStore must
// return an empty store for each call.
func RunConformance(t *testing.T, caps Capabilities, newStore func(t *testing.T) blob.Store) {
	t.Helper()
	ctx := context.Background()

	t.Run("PutGetStat", func(t *testing.T) {
		s := newStore(t)
		data := []byte("hello hangar")
		require.NoError(t, s.Put(ctx, "lfs/sha256/aa/bb/obj", bytes.NewReader(data), int64(len(data))))
		info, err := s.Stat(ctx, "lfs/sha256/aa/bb/obj")
		require.NoError(t, err)
		require.Equal(t, int64(len(data)), info.Size)
		require.Equal(t, "lfs/sha256/aa/bb/obj", info.Key)
		rc, info2, err := s.Get(ctx, "lfs/sha256/aa/bb/obj", nil)
		require.NoError(t, err)
		got, err := io.ReadAll(rc)
		require.NoError(t, err)
		require.NoError(t, rc.Close())
		require.Equal(t, data, got)
		require.Equal(t, info.Size, info2.Size)
	})

	t.Run("PutUnknownSizeAndOverwrite", func(t *testing.T) {
		s := newStore(t)
		require.NoError(t, s.Put(ctx, "tmp/x", strings.NewReader("first"), -1))
		require.NoError(t, s.Put(ctx, "tmp/x", strings.NewReader("second!"), -1))
		info, err := s.Stat(ctx, "tmp/x")
		require.NoError(t, err)
		require.Equal(t, int64(7), info.Size)
	})

	t.Run("PutSizeMismatchStoresNothing", func(t *testing.T) {
		s := newStore(t)
		err := s.Put(ctx, "tmp/short", strings.NewReader("abc"), 10)
		require.ErrorIs(t, err, blob.ErrSizeMismatch)
		_, err = s.Stat(ctx, "tmp/short")
		require.ErrorIs(t, err, blob.ErrNotFound)
	})

	t.Run("Ranges", func(t *testing.T) {
		s := newStore(t)
		require.NoError(t, s.Put(ctx, "tmp/r", strings.NewReader("0123456789"), 10))
		for _, tc := range []struct {
			rng  blob.Range
			want string
		}{
			{blob.Range{Start: 0, End: 0}, "0"},
			{blob.Range{Start: 2, End: 5}, "2345"},
			{blob.Range{Start: 7, End: -1}, "789"},
			{blob.Range{Start: 8, End: 100}, "89"},
		} {
			rc, info, err := s.Get(ctx, "tmp/r", &tc.rng)
			require.NoError(t, err)
			got, _ := io.ReadAll(rc)
			_ = rc.Close()
			require.Equal(t, tc.want, string(got))
			require.Equal(t, int64(10), info.Size, "Info describes the whole object")
		}
		_, _, err := s.Get(ctx, "tmp/r", &blob.Range{Start: 10, End: -1})
		require.ErrorIs(t, err, blob.ErrInvalidRange)
	})

	t.Run("NotFound", func(t *testing.T) {
		s := newStore(t)
		_, err := s.Stat(ctx, "nope/x")
		require.ErrorIs(t, err, blob.ErrNotFound)
		_, _, err = s.Get(ctx, "nope/x", nil)
		require.ErrorIs(t, err, blob.ErrNotFound)
		require.NoError(t, s.Delete(ctx, "nope/x"), "deleting a missing key is not an error")
		err = s.Copy(ctx, "nope/x", "nope/y")
		require.ErrorIs(t, err, blob.ErrNotFound)
	})

	t.Run("InvalidKeys", func(t *testing.T) {
		s := newStore(t)
		for _, k := range []string{"", "/abs", "a/../b", "a//b", "trailing/"} {
			require.ErrorIs(t, s.Put(ctx, k, strings.NewReader("x"), 1), blob.ErrInvalidKey, k)
		}
	})

	t.Run("DeleteAndList", func(t *testing.T) {
		s := newStore(t)
		for _, k := range []string{"git/r1/pack/b", "git/r1/pack/a", "git/r2/pack/c", "lfs/sha256/x"} {
			require.NoError(t, s.Put(ctx, k, strings.NewReader(k), int64(len(k))))
		}
		var keys []string
		require.NoError(t, s.List(ctx, "git/r1/", func(i blob.Info) error {
			keys = append(keys, i.Key)
			require.Equal(t, int64(len(i.Key)), i.Size)
			return nil
		}))
		require.Equal(t, []string{"git/r1/pack/a", "git/r1/pack/b"}, keys)

		stop := errors.New("stop")
		n := 0
		err := s.List(ctx, "", func(blob.Info) error { n++; return stop })
		require.ErrorIs(t, err, stop)
		require.Equal(t, 1, n)

		require.NoError(t, s.Delete(ctx, "git/r1/pack/a"))
		_, err = s.Stat(ctx, "git/r1/pack/a")
		require.ErrorIs(t, err, blob.ErrNotFound)
	})

	t.Run("Copy", func(t *testing.T) {
		s := newStore(t)
		require.NoError(t, s.Put(ctx, "tmp/src", strings.NewReader("payload"), 7))
		require.NoError(t, s.Copy(ctx, "tmp/src", "lfs/sha256/dst"))
		rc, _, err := s.Get(ctx, "lfs/sha256/dst", nil)
		require.NoError(t, err)
		got, _ := io.ReadAll(rc)
		_ = rc.Close()
		require.Equal(t, "payload", string(got))
	})

	t.Run("Multipart", func(t *testing.T) {
		s := newStore(t)
		partSize := max(caps.MinPartSize, 1024)
		p1 := randomBytes(t, int(partSize))
		p2 := randomBytes(t, int(partSize))
		p3 := randomBytes(t, 100)
		key := "lfs/sha256/mp/object"
		id, err := s.CreateMultipart(ctx, key)
		require.NoError(t, err)

		// Parts can arrive out of order and be retried.
		e2, err := s.PutPart(ctx, key, id, 2, bytes.NewReader(p2), int64(len(p2)))
		require.NoError(t, err)
		e1, err := s.PutPart(ctx, key, id, 1, bytes.NewReader(p1), int64(len(p1)))
		require.NoError(t, err)
		e1again, err := s.PutPart(ctx, key, id, 1, bytes.NewReader(p1), int64(len(p1)))
		require.NoError(t, err)
		require.Equal(t, e1, e1again)
		require.NotEmpty(t, e1)

		listed, err := s.ListParts(ctx, key, id)
		require.NoError(t, err)
		require.Len(t, listed, 2)
		require.Equal(t, 1, listed[0].Number)
		require.Equal(t, 2, listed[1].Number)

		_, err = s.Stat(ctx, key)
		require.ErrorIs(t, err, blob.ErrNotFound, "nothing is visible before completion")

		e3, err := s.PutPart(ctx, key, id, 3, bytes.NewReader(p3), int64(len(p3)))
		require.NoError(t, err)
		require.NoError(t, s.CompleteMultipart(ctx, key, id, []blob.Part{{Number: 1, ETag: e1}, {Number: 2, ETag: e2}, {Number: 3, ETag: e3}}))
		rc, info, err := s.Get(ctx, key, nil)
		require.NoError(t, err)
		got, _ := io.ReadAll(rc)
		_ = rc.Close()
		require.Equal(t, 2*partSize+100, info.Size)
		require.Equal(t, append(append(append([]byte{}, p1...), p2...), p3...), got)
	})

	t.Run("MultipartAbort", func(t *testing.T) {
		s := newStore(t)
		key := "lfs/sha256/mp/aborted"
		id, err := s.CreateMultipart(ctx, key)
		require.NoError(t, err)
		_, err = s.PutPart(ctx, key, id, 1, strings.NewReader("x"), 1)
		require.NoError(t, err)
		require.NoError(t, s.AbortMultipart(ctx, key, id))
		_, err = s.ListParts(ctx, key, id)
		require.ErrorIs(t, err, blob.ErrNotFound)
		_, err = s.Stat(ctx, key)
		require.ErrorIs(t, err, blob.ErrNotFound)
	})

	t.Run("Presign", func(t *testing.T) {
		s := newStore(t)
		require.NoError(t, s.Put(ctx, "lfs/sha256/p", strings.NewReader("x"), 1))
		getURL, getErr := s.PresignGet(ctx, "lfs/sha256/p", time.Hour, "model.safetensors")
		id, err := s.CreateMultipart(ctx, "lfs/sha256/q")
		require.NoError(t, err)
		partURL, partErr := s.PresignPart(ctx, "lfs/sha256/q", id, 1, time.Hour)
		put, putErr := s.PresignPut(ctx, "lfs/sha256/r", 1, nil, time.Hour)
		if !caps.Presign {
			require.ErrorIs(t, getErr, blob.ErrNotSupported)
			require.ErrorIs(t, partErr, blob.ErrNotSupported)
			require.ErrorIs(t, putErr, blob.ErrNotSupported)
			return
		}
		require.NoError(t, getErr)
		require.NoError(t, partErr)
		require.NoError(t, putErr)
		require.True(t, strings.HasPrefix(getURL, "http"))
		require.True(t, strings.HasPrefix(partURL, "http"))
		require.True(t, strings.HasPrefix(put.URL, "http"))
	})

	t.Run("ConcurrentPuts", func(t *testing.T) {
		s := newStore(t)
		var wg sync.WaitGroup
		errs := make(chan error, 16)
		for i := range 16 {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				data := bytes.Repeat([]byte{byte(i)}, 1000)
				errs <- s.Put(ctx, "tmp/same", bytes.NewReader(data), int64(len(data)))
			}(i)
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			require.NoError(t, err)
		}
		rc, _, err := s.Get(ctx, "tmp/same", nil)
		require.NoError(t, err)
		got, _ := io.ReadAll(rc)
		_ = rc.Close()
		require.Len(t, got, 1000)
		first := got[0]
		require.Equal(t, bytes.Repeat([]byte{first}, 1000), got, "readers never see a mix of two writes")
	})

	t.Run("ListOrder", func(t *testing.T) {
		s := newStore(t)
		var want []string
		for i := range 25 {
			k := fmt.Sprintf("xet/xorbs/%02d", 24-i)
			want = append(want, k)
			require.NoError(t, s.Put(ctx, k, strings.NewReader("x"), 1))
		}
		sort.Strings(want)
		var got []string
		require.NoError(t, s.List(ctx, "xet/xorbs/", func(i blob.Info) error { got = append(got, i.Key); return nil }))
		require.Equal(t, want, got)
	})
}

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	_, err := rand.Read(b)
	require.NoError(t, err)
	return b
}
