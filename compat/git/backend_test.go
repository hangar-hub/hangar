//go:build compat

package gitcompat

import (
	"sort"
	"testing"

	"github.com/hangar-hub/hangar/internal/auth"
	"github.com/hangar-hub/hangar/internal/blob/blobtest"
	"github.com/hangar-hub/hangar/internal/model"
	"github.com/hangar-hub/hangar/internal/repo"
)

// Backend is a Repo service implementation under test.
type Backend struct {
	// Replica returns the repo.Service of one web replica. Every replica
	// shares the backend's database and Blob store.
	Replica func(t *testing.T) repo.Service
	// LFS is the LFS store, shared by every replica.
	LFS repo.LFSStore
	// Users maps each requested username to a Principal whose personal
	// Namespace exists in the backend.
	Users map[string]model.Principal
}

// BackendFactory builds a backend over blobs, with a User and personal
// Namespace for each of usernames. Commits may reference LFS objects in
// Repos whose files authz lets the author read (repo.Service.Referable).
type BackendFactory func(t *testing.T, blobs *blobtest.Fake, usernames []string, authz auth.Authz) Backend

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
