package api_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/api"
	"github.com/jamesagarside/hangar/internal/api/gen"
	"github.com/jamesagarside/hangar/internal/blob"
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
		return repo.LFSObject{}, fmt.Errorf("%w: repo: store lfs object: %w", repo.ErrRetryable, io.ErrUnexpectedEOF)
	}
	return f.LFSStore.CompleteUpload(ctx, id, parts)
}

// TestCompleteUploadAnswersRetryable checks that a completion that kept its
// upload answers 503 with Retry-After, not a 500 a client gives up on, and
// that sending it again stores the object.
func TestCompleteUploadAnswersRetryable(t *testing.T) {
	t.Parallel()
	f := newFixture(t, func(d *api.RepoDeps) { d.LFS = &failingCompletion{LFSStore: d.LFS} })
	f.user("alice")
	f.createRepo("alice", gen.RepoCreate{Type: "model", Namespace: "alice", Name: "big"})
	base := "/repos/models/alice/big"
	weights := bytes.Repeat([]byte("w"), 100)
	var p gen.UploadPlan
	f.expect(http.StatusOK, http.MethodPost, "alice", base+"/uploads", gen.UploadPlanRequest{Files: []gen.UploadFile{
		{Path: "model.safetensors", Size: int64(len(weights)), Sha256: sum256(weights)},
	}}).json(t, &p)
	up := p.Files[0]
	var part gen.UploadedPart
	f.expect(http.StatusOK, http.MethodPut, "alice", (*up.PartUrls)[0], weights).json(t, &part)
	complete := base + "/uploads/" + up.UploadId.String() + "/complete"
	body := gen.UploadComplete{Parts: []gen.UploadedPart{part}}

	r := f.expect(http.StatusServiceUnavailable, http.MethodPost, "alice", complete, body)
	require.Equal(t, "5", r.header.Get("Retry-After"))
	require.Contains(t, string(r.body), "send the same request again")
	require.NotContains(t, string(r.body), "unexpected EOF", "internal detail stays in the log")

	var status gen.UploadStatus
	f.expect(http.StatusOK, http.MethodPost, "alice", complete, body).json(t, &status)
	require.Equal(t, gen.UploadStatusState("verified"), status.State)
}
