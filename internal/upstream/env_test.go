package upstream_test

import (
	"testing"

	"github.com/hangar-hub/hangar/internal/db"
	"github.com/hangar-hub/hangar/internal/db/dbtest"
	"github.com/hangar-hub/hangar/internal/model"
	"github.com/hangar-hub/hangar/internal/upstream/upstreamtest"
)

func newEnv(t *testing.T, d *db.DB, o upstreamtest.HarnessOptions) *upstreamtest.Harness {
	t.Helper()
	return upstreamtest.NewHarness(t, d, o)
}

func forEachDB(t *testing.T, fn func(t *testing.T, d *db.DB)) {
	dbtest.ForEachDriver(t, fn)
}

func lfsIDs(es []model.Entry) map[string]bool {
	out := map[string]bool{}
	for _, e := range es {
		if e.LFS != nil {
			out[e.Path] = e.Present
		}
	}
	return out
}
