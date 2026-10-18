//go:build compat

package gitcompat

import (
	"testing"

	"github.com/hangar-hub/hangar/internal/auth"
	"github.com/hangar-hub/hangar/internal/blob/blobtest"
	"github.com/hangar-hub/hangar/internal/db"
	"github.com/hangar-hub/hangar/internal/db/dbtest"
	"github.com/hangar-hub/hangar/internal/model"
	"github.com/hangar-hub/hangar/internal/repo"
	"github.com/hangar-hub/hangar/internal/repo/repotest"
)

// The real backends are internal/repo (repo.Store, gitstore and repo.LFS)
// on SQLite and on Postgres. Each replica is its own repo.Store with its own
// pack cache, sharing the database and the Blob store, as web replicas do.
func init() {
	real := func(open func(testing.TB) *db.DB) BackendFactory {
		return func(t *testing.T, blobs *blobtest.Fake, usernames []string, authz auth.Authz) Backend {
			d := open(t)
			first := repotest.NewReal(t, d, repotest.RealOptions{Blobs: blobs, Tweak: readFiles(authz)})
			users := map[string]model.Principal{}
			for _, name := range usernames {
				users[name] = first.AddUser(t, name)
			}
			lfs := first.LFS
			replica := func(t *testing.T) repo.Service {
				return repotest.NewReal(t, d, repotest.RealOptions{Blobs: blobs, Tweak: readFiles(authz)}).Store
			}
			return Backend{Replica: func(t *testing.T) repo.Service {
				if first != nil {
					r := first.Store
					first = nil
					return r
				}
				return replica(t)
			}, LFS: lfs, Users: users}
		}
	}
	register("real-sqlite", real(func(t testing.TB) *db.DB { return dbtest.SQLite(t) }))
	// Postgres needs HANGAR_TEST_POSTGRES=1 (make sets it) or HANGAR_TEST_POSTGRES_DSN.
	if dbtest.PostgresEnabled() {
		register("real-postgres", real(func(t testing.TB) *db.DB { return dbtest.Postgres(t) }))
	}
}

// readFiles wires authz into repo.Options.CanReadFiles, as the server does.
func readFiles(authz auth.Authz) func(*repo.Options) {
	return func(o *repo.Options) { o.CanReadFiles = repotest.CanReadFilesWith(authz) }
}
