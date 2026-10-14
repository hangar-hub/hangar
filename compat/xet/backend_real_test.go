//go:build compat && xetserver && xetreal

package xet

import (
	"context"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/blob/fs"
	"github.com/jamesagarside/hangar/internal/db"
	"github.com/jamesagarside/hangar/internal/db/dbtest"
	"github.com/jamesagarside/hangar/internal/hfapi/hfapitest"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/org"
	"github.com/jamesagarside/hangar/internal/org/orgtest"
	"github.com/jamesagarside/hangar/internal/repo"
	"github.com/jamesagarside/hangar/internal/repo/repotest"
)

// dbOrgs answers Namespace lookups from the database the real Repo service
// uses, so both agree on Namespace and user IDs.
type dbOrgs struct {
	*orgtest.Fake
	ns *repotest.DBNamespaces
}

func (d dbOrgs) Get(ctx context.Context, name string) (*org.Namespace, error) {
	return d.ns.Get(ctx, name)
}
func (d dbOrgs) GetByID(ctx context.Context, id uuid.UUID) (*org.Namespace, error) {
	return d.ns.GetByID(ctx, id)
}
func (d dbOrgs) MembershipsOf(ctx context.Context, userID uuid.UUID) ([]org.Membership, error) {
	return d.ns.MembershipsOf(ctx, userID)
}

// currentBackend uses W2's internal/repo and W1's filesystem Blob store,
// with one database (SQLite, or Postgres with HANGAR_XET_DB=postgres) for
// Repos, LFS objects and the Xet tables, and one Blob store for git data,
// LFS objects and xorbs, as an Instance has.
func currentBackend(t *testing.T) backend {
	name := "internal/repo (W2), blob/fs (W1), SQLite"
	var d *db.DB
	if os.Getenv("HANGAR_XET_DB") == "postgres" {
		name = "internal/repo (W2), blob/fs (W1), Postgres"
		d = dbtest.Postgres(t)
	} else {
		d = dbtest.SQLite(t)
	}
	blobs, err := fs.New(t.TempDir())
	require.NoError(t, err)
	return backend{
		name: name,
		opts: func(o *hfapitest.Options) {
			real := repotest.NewReal(t, d, repotest.RealOptions{Blobs: blobs, Tweak: func(ro *repo.Options) { ro.AnonymousRead = true }})
			o.Repos, o.LFS = real.Store, real.LFS
			o.Orgs = dbOrgs{Fake: orgtest.NewFake(), ns: real.Namespaces}
			o.AddUser = func(t testing.TB, name string) model.Principal { return real.AddUser(t, name) }
			o.AddOrg = func(t testing.TB, name string, members map[uuid.UUID]model.Role) uuid.UUID {
				return real.AddOrg(t, name, members)
			}
		},
		db:    d,
		blobs: blobs,
	}
}
