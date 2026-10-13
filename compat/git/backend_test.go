//go:build compat

package gitcompat

import (
	"sort"
	"testing"

	"github.com/jamesagarside/hangar/internal/blob/blobtest"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/repo"
)

// Backend is a Repo service implementation under test.
type Backend struct {
	// Replica returns the repo.Service of one web replica. Every replica
	// shares the backend's database and Blob store.
	Replica func(t *testing.T) repo.Service
	// LFS is the LFS store, shared by every replica.
	LFS repo.LFSStore
}

// BackendFactory builds a backend over blobs. users are the Users whose
// personal Namespaces the suite's Repos live in.
type BackendFactory func(t *testing.T, blobs *blobtest.Fake, users []model.Principal) Backend

var backends = map[string]BackendFactory{}

// register adds a backend; each backend file registers itself in init.
func register(name string, f BackendFactory) { backends[name] = f }

// forEachBackend runs fn once per registered backend.
func forEachBackend(t *testing.T, fn func(t *testing.T, name string, f BackendFactory)) {
	t.Helper()
	names := make([]string, 0, len(backends))
	for n := range backends {
		names = append(names, n)
	}
	sort.Strings(names)
	if len(names) == 0 {
		t.Fatal("no Repo service backend is registered")
	}
	for _, n := range names {
		t.Run(n, func(t *testing.T) { fn(t, n, backends[n]) })
	}
}
