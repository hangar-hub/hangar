package indextest_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/index"
	"github.com/jamesagarside/hangar/internal/index/indextest"
	"github.com/jamesagarside/hangar/internal/model"
)

func TestFakeIndexer(t *testing.T) {
	ctx := context.Background()
	f := indextest.NewFake()
	repoUUID := uuid.New()
	id := model.RepoID{Type: model.RepoTypeModel, Namespace: "ns", Name: "gguf"}
	commit := model.CommitSHA("0123456789012345678901234567890123456789")

	_, err := f.Get(ctx, repoUUID, commit)
	require.ErrorIs(t, err, index.ErrNotIndexed, "readers never wait for indexing")

	f.SetSummary(repoUUID, commit, &index.Summary{Formats: []string{"gguf"}, Architecture: "llama", ContextLength: 8192})
	s, err := f.Index(ctx, repoUUID, id, commit)
	require.NoError(t, err)
	require.Equal(t, "llama", s.Architecture)
	got, err := f.Get(ctx, repoUUID, commit)
	require.NoError(t, err)
	require.Equal(t, int64(8192), got.ContextLength)
	require.Equal(t, 1, f.IndexRuns())

	other := model.CommitSHA("9999999999999999999999999999999999999999")
	empty, err := f.Index(ctx, repoUUID, id, other)
	require.NoError(t, err)
	require.Empty(t, empty.Formats)

	f.SetHeader(id, commit, "model.safetensors", `{"__metadata__":{}}`)
	h, err := f.FileHeader(ctx, id, commit, "model.safetensors")
	require.NoError(t, err)
	require.JSONEq(t, `{"__metadata__":{}}`, string(h))
	_, err = f.FileHeader(ctx, id, commit, "README.md")
	require.ErrorIs(t, err, index.ErrNoHeader)

	f.SetFiles(id, commit, map[string]int64{"m-Q4_K_M.gguf": 10, "m-Q8_0.gguf": 20})
	groups, err := f.Quants(ctx, id, commit)
	require.NoError(t, err)
	require.Len(t, groups, 2)

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = f.Index(cancelled, repoUUID, id, commit)
	require.Error(t, err)
}
