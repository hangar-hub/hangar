package repotest_test

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/db"
	"github.com/hangar-hub/hangar/internal/db/dbtest"
	"github.com/hangar-hub/hangar/internal/model"
	"github.com/hangar-hub/hangar/internal/repo"
	"github.com/hangar-hub/hangar/internal/repo/repotest"
)

func TestRealServiceRoundTrip(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		ctx := context.Background()
		r := repotest.NewReal(t, d)
		bob := r.AddUser(t, "bob")
		orgID := r.AddOrg(t, "team", map[uuid.UUID]model.Role{bob.UserID: model.RoleWrite})
		ms, err := r.Namespaces.MembershipsOf(ctx, bob.UserID)
		require.NoError(t, err)
		require.Len(t, ms, 1)
		require.Equal(t, orgID, ms[0].Org.ID)
		require.Equal(t, model.RoleWrite, ms[0].Role)

		id := model.RepoID{Type: model.RepoTypeDataset, Namespace: "team", Name: "data"}
		created, err := r.Create(ctx, repo.CreateParams{ID: id, Actor: bob})
		require.NoError(t, err)
		require.Equal(t, id, created.ID)
		res, err := r.Commit(ctx, repo.CommitParams{Repo: id, Summary: "add", Actor: bob,
			Operations: []repo.Operation{{Kind: repo.OpAdd, Path: "data.csv", Content: strings.NewReader("a,b\n")}}})
		require.NoError(t, err)
		rc, e, err := r.OpenFile(ctx, id, res.SHA, "data.csv", nil)
		require.NoError(t, err)
		b, _ := io.ReadAll(rc)
		rc.Close()
		require.Equal(t, "a,b\n", string(b))
		require.Equal(t, model.GitBlobID(b), e.OID)

		_, err = r.Namespaces.Get(ctx, "nobody")
		require.Error(t, err)
		_, err = r.Namespaces.GetByID(ctx, uuid.New())
		require.Error(t, err)
		ns, err := r.Namespaces.Get(ctx, "BOB")
		require.NoError(t, err)
		require.Equal(t, bob.UserID, ns.UserID)
	})
}

func TestFakeLFSUploadRepo(t *testing.T) {
	ctx := context.Background()
	f := repotest.NewFake(t)
	repoUUID := uuid.New()
	u, err := f.LFS.CreateUpload(ctx, repo.UploadParams{OID: strings.Repeat("a", 64), Size: 1, RepoUUID: repoUUID})
	require.NoError(t, err)
	got, err := f.LFS.UploadRepo(ctx, u.ID)
	require.NoError(t, err)
	require.Equal(t, repoUUID, got)
	_, err = f.LFS.UploadRepo(ctx, uuid.New())
	require.ErrorIs(t, err, repo.ErrNotFound)

	id := model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "m"}
	alice := model.Principal{Kind: model.PrincipalUser, UserID: uuid.New(), NamespaceID: uuid.New(), Username: "alice"}
	_, err = f.Create(ctx, repo.CreateParams{ID: id, Actor: alice})
	require.NoError(t, err)
	require.NoError(t, f.SetMirrorState(ctx, id, true))
	r, err := f.Get(ctx, id)
	require.NoError(t, err)
	require.True(t, r.Mirror)
	n, err := f.CountByNamespace(ctx, alice.NamespaceID, alice)
	require.NoError(t, err)
	require.Equal(t, 1, n)
}
