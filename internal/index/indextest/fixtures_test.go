package indextest_test

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/db"
	"github.com/jamesagarside/hangar/internal/db/dbtest"
	"github.com/jamesagarside/hangar/internal/index/gguf"
	"github.com/jamesagarside/hangar/internal/index/indextest"
	"github.com/jamesagarside/hangar/internal/index/safetensors"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/repo/repotest"
)

func TestFixtureHelpers(t *testing.T) {
	ctx := context.Background()
	fake := repotest.NewFake(t)
	id := model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "tiny"}
	r := indextest.CreateRepo(t, fake, id, model.VisibilityPublic)
	require.Equal(t, indextest.Owner("alice").NamespaceID, r.NamespaceID)
	st := indextest.Safetensors(t, map[string]string{"k": "v"}, safetensors.Tensor{Name: "w", DType: "F32", Shape: []int64{2}})
	g := indextest.LlamaGGUF(t, 2, "Q4_0", gguf.KV{Key: "extra", Value: "x"})
	sha := indextest.Commit(t, fake, id, map[string][]byte{"model.safetensors": st, "m.gguf": g, "a.txt": []byte("text")})
	e, err := fake.Entry(ctx, id, sha, "m.gguf")
	require.NoError(t, err)
	require.NotNil(t, e.LFS, "GGUF files are stored as LFS objects")
	e, err = fake.Entry(ctx, id, sha, "a.txt")
	require.NoError(t, err)
	require.Nil(t, e.LFS)
	rc, _, err := fake.OpenFile(ctx, id, sha, "m.gguf", nil)
	require.NoError(t, err)
	b, _ := io.ReadAll(rc)
	_ = rc.Close()
	f, err := gguf.Read(bytes.NewReader(b), int64(len(b)))
	require.NoError(t, err)
	require.Equal(t, "x", f.StringValue("extra"))
	require.Equal(t, indextest.Llama3Template, f.ChatTemplate())
	require.True(t, indextest.IsLFSPath("x.SAFETENSORS"))
	require.False(t, indextest.IsLFSPath("README.md"))

	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		indextest.InsertRepoRow(t, d, r)
		indextest.InsertUser(t, d, indextest.Owner("bob"))
		indextest.InsertUser(t, d, indextest.Owner("bob"))
		n, err := d.NewSelect().Model((*db.Repo)(nil)).Count(ctx)
		require.NoError(t, err)
		require.EqualValues(t, 1, n)
	})
}
