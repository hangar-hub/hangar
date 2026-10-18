package hfapi_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/model"
)

// TestCommitsCannotReferenceObjectsOfPrivateRepos covers the decision
// James made on 2026-10-18 ("check at Commit") through the Hub API: bob
// knows the SHA-256 and size of a file in alice's private Repo, but a
// commit or a create_pr commit with an lfsFile line for it is refused as
// for an object nobody uploaded, without its size, and opens no pull
// request; the git-lfs batch asks bob to upload it. alice, who can read
// the Repo, may reference it in another Repo without uploading it.
func TestCommitsCannotReferenceObjectsOfPrivateRepos(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	secret := "alice's private weights"
	oid := sha256hex(secret)
	seed(t, e, "alice", modelID("alice", "private"), model.VisibilityPrivate, map[string]string{"w.safetensors": secret})
	bobRepo, bobBase := seed(t, e, "bob", modelID("bob", "grab"), model.VisibilityPublic, map[string]string{"a.txt": "a"})
	unknown := sha256hex("nobody uploaded this")

	refusal := func(o string, size int, pr bool) string {
		t.Helper()
		path := "/api/models/bob/grab/commit/main"
		if pr {
			path += "?create_pr=1"
		}
		r := e.Request(t, http.MethodPost, path, "bob", ndjson(t, header("Grab", string(bobBase)), lfsLine("w.safetensors", o, size)))
		require.Equal(t, http.StatusBadRequest, r.StatusCode)
		msg := string(readBody(t, r))
		require.NotContains(t, msg, "23 bytes")
		return strings.ReplaceAll(msg, o, "<oid>")
	}
	for _, pr := range []bool{false, true} {
		for _, size := range []int{len(secret), -1, len(secret) + 1} {
			require.Equal(t, refusal(unknown, size, pr), refusal(oid, size, pr), "create_pr=%v size %d", pr, size)
		}
	}
	requireNoPullRequests(t, e, bobRepo)

	// The batch answers the stored object as it would a missing one.
	batch := `{"operation":"upload","transfers":["basic"],"objects":[{"oid":"` + oid + `","size":23}]}`
	r := e.Request(t, http.MethodPost, "/bob/grab.git/info/lfs/objects/batch", "bob", batch, "Content-Type", "application/vnd.git-lfs+json")
	require.Equal(t, http.StatusOK, r.StatusCode)
	require.Contains(t, string(readBody(t, r)), `"upload"`)

	// alice can read her Repo, so she references it in another one.
	_, aliceBase := seed(t, e, "alice", modelID("alice", "public"), model.VisibilityPublic, map[string]string{"a.txt": "a"})
	r = e.Request(t, http.MethodPost, "/api/models/alice/public/commit/main", "alice",
		ndjson(t, header("Reuse", string(aliceBase)), lfsLine("w.safetensors", oid, len(secret))))
	require.Equal(t, http.StatusOK, r.StatusCode, string(readBody(t, r)))
	r = e.Request(t, http.MethodPost, "/alice/public.git/info/lfs/objects/batch", "alice", batch, "Content-Type", "application/vnd.git-lfs+json")
	require.NotContains(t, string(readBody(t, r)), `"upload"`, "alice is told the object exists")
}
