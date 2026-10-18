//go:build compat

package gitcompat

import (
	"testing"

	"github.com/google/uuid"

	"github.com/jamesagarside/hangar/internal/auth"
	"github.com/jamesagarside/hangar/internal/blob/blobtest"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/repo"
	"github.com/jamesagarside/hangar/internal/repo/repotest"
)

// The fake backend is repotest.Fake: real bare git repositories driven by
// the git binary. Both replicas share one Fake.
func init() {
	register("fake", func(t *testing.T, blobs *blobtest.Fake, usernames []string, authz auth.Authz) Backend {
		f := repotest.NewFake(t)
		f.LFS = repotest.NewFakeLFS(blobs)
		f.CheckLFSReferences = true
		f.CanReadFiles = repotest.CanReadFilesWith(authz)
		users := map[string]model.Principal{}
		for _, name := range usernames {
			users[name] = model.Principal{Kind: model.PrincipalUser, UserID: uuid.New(), NamespaceID: uuid.New(),
				Username: name, DisplayName: name, Email: name + "@example.com"}
		}
		return Backend{Replica: func(*testing.T) repo.Service { return f }, LFS: f.LFS, Users: users}
	})
}
