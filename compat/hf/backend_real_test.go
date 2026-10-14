//go:build compat && w2real

package hf

import (
	"context"
	"os"
	"testing"

	"github.com/google/uuid"

	"github.com/jamesagarside/hangar/internal/db"
	"github.com/jamesagarside/hangar/internal/db/dbtest"
	"github.com/jamesagarside/hangar/internal/hfapi/hfapitest"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/org"
	"github.com/jamesagarside/hangar/internal/org/orgtest"
	"github.com/jamesagarside/hangar/internal/repo"
	"github.com/jamesagarside/hangar/internal/repo/repotest"
)

// dbOrgs answers Namespace lookups from the database the real Repo
// service uses, so both agree on Namespace and user IDs.
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

// repoBackend uses W2's internal/repo on SQLite, or on Postgres with
// HANGAR_HF_DB=postgres.
func repoBackend(t *testing.T, o *hfapitest.Options) string {
	open, name := func(t testing.TB) *db.DB { return dbtest.SQLite(t) }, "sqlite"
	if os.Getenv("HANGAR_HF_DB") == "postgres" {
		open, name = func(t testing.TB) *db.DB { return dbtest.Postgres(t) }, "postgres"
	}
	real := repotest.NewReal(t, open(t), repotest.RealOptions{Tweak: func(ro *repo.Options) { ro.AnonymousRead = true }})
	o.Repos, o.LFS = real.Store, real.LFS
	o.Orgs = dbOrgs{Fake: orgtest.NewFake(), ns: real.Namespaces}
	o.AddUser = func(t testing.TB, name string) model.Principal { return real.AddUser(t, name) }
	o.AddOrg = func(t testing.TB, name string, members map[uuid.UUID]model.Role) uuid.UUID {
		return real.AddOrg(t, name, members)
	}
	return "internal/repo (W2) on " + name
}
