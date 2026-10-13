//go:build compat

package gitcompat

import (
	"testing"

	"github.com/jamesagarside/hangar/internal/blob/blobtest"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/repo"
	"github.com/jamesagarside/hangar/internal/repo/repotest"
)

// The fake backend is repotest.Fake: real bare git repositories driven by
// the git binary. Both replicas share one Fake.
func init() {
	register("fake", func(t *testing.T, blobs *blobtest.Fake, _ []model.Principal) Backend {
		f := repotest.NewFake(t)
		f.LFS = repotest.NewFakeLFS(blobs)
		return Backend{Replica: func(*testing.T) repo.Service { return f }, LFS: f.LFS}
	})
}
