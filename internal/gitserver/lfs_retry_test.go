package gitserver_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/blob"
	"github.com/jamesagarside/hangar/internal/gitserver/gitservertest"
	"github.com/jamesagarside/hangar/internal/repo"
)

// failingCompletion fails the first completion the way a Blob store read
// that keeps breaking does, keeping the upload.
type failingCompletion struct {
	repo.LFSStore
	failed atomic.Bool
}

func (f *failingCompletion) CompleteUpload(ctx context.Context, id uuid.UUID, parts []blob.Part) (repo.LFSObject, error) {
	if f.failed.CompareAndSwap(false, true) {
		return repo.LFSObject{}, fmt.Errorf("%w: %w", repo.ErrRetryable, io.ErrUnexpectedEOF)
	}
	return f.LFSStore.CompleteUpload(ctx, id, parts)
}

// TestLFSMultipartCompletionRetryable checks that a completion that kept its
// upload answers 503 with Retry-After, and that sending it again succeeds.
func TestLFSMultipartCompletionRetryable(t *testing.T) {
	t.Parallel()
	f := newFixture(t, func(o *gitservertest.Options, _ *fixture) { o.LFS = &failingCompletion{LFSStore: o.LFS} })
	content, oid := randomContent(t, (1<<20)+300<<10)
	_, b := f.batch(t, publicID, tokAlice, batchReq("upload", []string{"multipart"}, obj(oid, len(content))))
	require.Equal(t, "multipart", b.Transfer)
	up := b.Objects[0].Actions["upload"]
	var etags []map[string]any
	for n := 1; n <= (len(content)+262143)/262144; n++ {
		lo, hi := (n-1)*262144, min(n*262144, len(content))
		resp := call(t, http.MethodPut, &lfsAction{Href: up.Header[fmt.Sprintf("%05d", n)]}, content[lo:hi])
		require.Equal(t, http.StatusOK, resp.StatusCode)
		etags = append(etags, map[string]any{"partNumber": n, "etag": resp.Header.Get("ETag")})
	}
	done, err := json.Marshal(map[string]any{"oid": oid, "parts": etags})
	require.NoError(t, err)
	resp := call(t, http.MethodPost, &lfsAction{Href: up.Href}, done)
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	require.Equal(t, "5", resp.Header.Get("Retry-After"))
	require.Equal(t, http.StatusOK, call(t, http.MethodPost, &lfsAction{Href: up.Href}, done).StatusCode)
	stored, err := f.repos.LFS.Stat(context.Background(), oid)
	require.NoError(t, err)
	require.True(t, stored.Verified)
}
