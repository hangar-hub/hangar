package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/auth"
	"github.com/jamesagarside/hangar/internal/config"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/repo"
	"github.com/jamesagarside/hangar/internal/scan/scantest"
)

// unsafeDownloadRoutes lists every route that serves the bytes of a Repo
// file, by protocol. TestUnsafeDownloadGateCoversEveryRoute checks each one
// refuses a file the scan marked unsafe while scan.block_unsafe_downloads
// is on; a new download route must be added here. Pull-through serves
// through the Hub API and Ollama routes, and presigned redirects are issued
// only after these checks. git-upload-pack is not gated (scan.Gate).
var unsafeDownloadRoutes = []string{
	"GET /{repo}/resolve/{rev}/{path}",
	"POST /{repo}.git/info/lfs/objects/batch (download)",
	"GET /{repo}.git/info/lfs/objects/{oid} (signed)",
	"GET /v2/{ns}/{name}/blobs/{digest}",
	"POST /api/hangar/v1/repos/{type}/{ns}/{name}/zip",
}

func TestUnsafeDownloadGateCoversEveryRoute(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.NotFoundHandler())
	pub := "http://" + srv.Listener.Addr().String()
	in := newTestInstance(t, func(c *config.Config) {
		c.Server.PublicURL = pub
		c.Security.AnonymousRead = true
	})
	ctx := context.Background()
	h, err := newHandler(ctx, in, newHealth(in.db, in.blobs), false)
	require.NoError(t, err)
	srv.Config.Handler = h
	srv.Start()
	t.Cleanup(srv.Close)

	p := testUser(t, in, "alice")
	_, token, err := in.auth.TokenStore.Create(ctx, auth.NewToken{UserID: p.UserID, Name: "t", Scope: model.ScopeWrite})
	require.NoError(t, err)
	id := model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "m"}
	_, err = in.repos.Create(ctx, repo.CreateParams{ID: id, Actor: p, InitialReadme: true, Visibility: model.VisibilityPublic})
	require.NoError(t, err)
	_, err = in.repos.Commit(ctx, repo.CommitParams{Repo: id, Revision: "main", Summary: "Add a pickle", Actor: p,
		Operations: []repo.Operation{{Kind: repo.OpAdd, Path: "evil.pkl", Content: bytes.NewReader(scantest.OSSystemProtocol0())}}})
	require.NoError(t, err)
	r, err := in.repos.Get(ctx, id)
	require.NoError(t, err)
	head, err := in.repos.Resolve(ctx, id, "")
	require.NoError(t, err)
	e, err := in.repos.Entry(ctx, id, head, "evil.pkl")
	require.NoError(t, err)
	require.NotNil(t, e.LFS, "the Hub's default .gitattributes stores pickles in LFS")
	oid := e.LFS.OID
	res, err := in.scanner.ScanCommit(ctx, r.UUID, id, head)
	require.NoError(t, err)
	require.Equal(t, "unsafe", string(res.Status))

	do := func(method, url, body string, authed bool) (*http.Response, string) {
		t.Helper()
		req, err := http.NewRequest(method, url, strings.NewReader(body))
		require.NoError(t, err)
		if body != "" {
			req.Header.Set("Content-Type", "application/vnd.git-lfs+json")
			req.Header.Set("Accept", "application/vnd.git-lfs+json")
		}
		if authed {
			req.SetBasicAuth("alice", token)
		}
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return resp, string(raw)
	}
	setBlock := func(on bool) {
		t.Helper()
		_, err := in.auth.Settings.Update(ctx, auth.SettingsPatch{ScanBlockUnsafeDownloads: &on}, model.System())
		require.NoError(t, err)
	}
	lfsBatch := `{"operation":"download","transfers":["basic"],"objects":[{"oid":"` + oid + `","size":` + jsonInt(e.Size) + `}]}`
	signedHref := func() string {
		t.Helper()
		resp, body := do(http.MethodPost, pub+"/alice/m.git/info/lfs/objects/batch", lfsBatch, true)
		require.Equal(t, http.StatusOK, resp.StatusCode, body)
		var out struct {
			Objects []struct {
				Actions map[string]struct {
					Href string `json:"href"`
				} `json:"actions"`
			} `json:"objects"`
		}
		require.NoError(t, json.Unmarshal([]byte(body), &out), body)
		require.Len(t, out.Objects, 1, body)
		return out.Objects[0].Actions["download"].Href
	}

	// With the setting off every route serves the file. The signed link
	// is kept to check that links issued before an admin turns blocking on
	// are refused too.
	href := signedHref()
	require.NotEmpty(t, href)
	setBlock(true)

	checks := map[string]func(){
		unsafeDownloadRoutes[0]: func() {
			resp, _ := do(http.MethodGet, pub+"/alice/m/resolve/main/evil.pkl", "", false)
			require.Equal(t, http.StatusForbidden, resp.StatusCode)
			require.Contains(t, resp.Header.Get("X-Error-Message"), "accept_risk=1")
			resp, _ = do(http.MethodGet, pub+"/alice/m/resolve/main/evil.pkl?accept_risk=1", "", false)
			require.Equal(t, http.StatusOK, resp.StatusCode, "accept_risk=1 downloads it anyway")
		},
		unsafeDownloadRoutes[1]: func() {
			resp, body := do(http.MethodPost, pub+"/alice/m.git/info/lfs/objects/batch", lfsBatch, true)
			require.Equal(t, http.StatusOK, resp.StatusCode, body)
			require.Contains(t, body, `"code":403`)
			require.Contains(t, body, "evil.pkl is marked unsafe")
			require.NotContains(t, body, `"download"`, "no download action is issued")
		},
		unsafeDownloadRoutes[2]: func() {
			resp, body := do(http.MethodGet, href, "", false)
			require.Equal(t, http.StatusForbidden, resp.StatusCode, body)
			require.Contains(t, body, "marked unsafe")
		},
		unsafeDownloadRoutes[3]: func() {
			resp, body := do(http.MethodGet, pub+"/v2/alice/m/blobs/sha256:"+oid, "", false)
			require.Equal(t, http.StatusForbidden, resp.StatusCode, body)
			require.Contains(t, body, "DENIED")
		},
		unsafeDownloadRoutes[4]: func() {
			resp, body := do(http.MethodPost, pub+"/api/hangar/v1/repos/models/alice/m/zip", "", true)
			require.Equal(t, http.StatusForbidden, resp.StatusCode, body)
			require.Contains(t, body, "cannot include it")
		},
	}
	require.Len(t, checks, len(unsafeDownloadRoutes), "every listed route has a check")
	for _, route := range unsafeDownloadRoutes {
		t.Run(route, func(t *testing.T) { checks[route]() })
	}

	// Safe files still download everywhere.
	resp, _ := do(http.MethodGet, pub+"/alice/m/resolve/main/README.md", "", false)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	// Turned off again, the object downloads through git-lfs.
	setBlock(false)
	resp, body := do(http.MethodGet, href, "", false)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, string(scantest.OSSystemProtocol0()), body)
}

func jsonInt(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}
