package ollama_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/auth/authtest"
	"github.com/jamesagarside/hangar/internal/db/dbtest"
	"github.com/jamesagarside/hangar/internal/httpx"
	"github.com/jamesagarside/hangar/internal/index/indextest"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/ollama"
	"github.com/jamesagarside/hangar/internal/repo/repotest"
	"github.com/jamesagarside/hangar/internal/scan"
	"github.com/jamesagarside/hangar/internal/scan/scantest"
)

// TestBlobBlocksUnsafeDownloads checks that the registry follows
// scan.block_unsafe_downloads for layers that are files of the Repo.
func TestBlobBlocksUnsafeDownloads(t *testing.T) {
	ctx := context.Background()
	repos := repotest.NewFake(t)
	authz := authtest.NewAuthz()
	authz.AnonymousRead = true
	authn := authtest.NewAuthenticator()
	scanner := scantest.NewFake(repos)
	block := true
	signer, err := httpx.NewSigner([]byte("0123456789abcdef0123456789abcdef"))
	require.NoError(t, err)
	h, err := ollama.New(ollama.Options{DB: dbtest.SQLite(t), Repos: repos, LFS: repos.LFS, Authz: authz, OllamaAuth: authtest.NewOllamaAuth(authn),
		Signer: signer, PublicURL: "http://hangar.test",
		Downloads: &scan.Gate{Block: func(context.Context) (bool, error) { return block, nil }, Results: scanner.Results, Repos: repos}})
	require.NoError(t, err)
	srv := httptest.NewServer(httpx.RequestIDs(httpx.Authenticate(ollama.ExemptTokenEndpoint(authn), nil)(httpx.Dispatch(httpx.Protocols{Ollama: h}, "http://hangar.test"))))
	t.Cleanup(srv.Close)

	id := model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "mixed"}
	r := indextest.CreateRepo(t, repos, id, model.VisibilityPublic)
	sha := indextest.Commit(t, repos, id, map[string][]byte{
		"evil.pkl":  scantest.OSSystemProtocol0(),
		"tiny.gguf": indextest.LlamaGGUF(t, 1, "F16"),
	})
	scanner.SetResult("evil.pkl", scan.StatusUnsafe, scan.Finding{Module: "os", Name: "system", Status: scan.StatusUnsafe})
	_, err = scanner.ScanCommit(ctx, r.UUID, id, sha)
	require.NoError(t, err)
	oid := func(p string) string {
		e, err := repos.Entry(ctx, id, sha, p)
		require.NoError(t, err)
		return e.LFS.OID
	}
	get := func(p string) *http.Response {
		resp, err := http.Get(srv.URL + "/v2/alice/mixed/blobs/sha256:" + oid(p))
		require.NoError(t, err)
		t.Cleanup(func() { _ = resp.Body.Close() })
		return resp
	}
	resp := get("evil.pkl")
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	require.Contains(t, errCode(t, resp), "DENIED: evil.pkl is marked unsafe")
	require.Equal(t, http.StatusOK, get("tiny.gguf").StatusCode)
	block = false
	require.Equal(t, http.StatusOK, get("evil.pkl").StatusCode)
}
