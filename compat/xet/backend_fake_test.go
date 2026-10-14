//go:build compat && xetserver && !xetreal

package xet

import (
	"testing"

	"github.com/jamesagarside/hangar/internal/blob/blobtest"
	"github.com/jamesagarside/hangar/internal/db/dbtest"
	"github.com/jamesagarside/hangar/internal/hfapi/hfapitest"
)

// currentBackend keeps hfapitest's real-git Repo fake and stores xorbs in
// memory, with the Xet tables on a temporary SQLite database.
func currentBackend(t *testing.T) backend {
	return backend{
		name:  "repotest.Fake, blobtest.Fake, SQLite",
		opts:  func(*hfapitest.Options) {},
		db:    dbtest.SQLite(t),
		blobs: blobtest.NewFake(),
	}
}
