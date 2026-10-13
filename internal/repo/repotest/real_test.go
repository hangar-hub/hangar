package repotest_test

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/db"
	"github.com/jamesagarside/hangar/internal/db/dbtest"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/repo"
	"github.com/jamesagarside/hangar/internal/repo/repotest"
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
