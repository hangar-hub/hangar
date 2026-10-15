package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/api"
	"github.com/jamesagarside/hangar/internal/auth"
	"github.com/jamesagarside/hangar/internal/config"
	"github.com/jamesagarside/hangar/internal/jobs"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/repo"
	"github.com/jamesagarside/hangar/internal/scan/scantest"
	"github.com/jamesagarside/hangar/internal/upstream"
	"github.com/jamesagarside/hangar/internal/upstream/upstreamtest"
)

// The routes that serve the bytes of a Repo file, by protocol.
// TestUnsafeDownloadGateCoversEveryRoute checks each one refuses a file the
// scan marked unsafe, or has not scanned yet, while
// scan.block_unsafe_downloads is on. The lists are not kept by hand alone:
// TestServingSurfaceIsClassified enumerates the router's protocols and
// signed-link routes, and TestFileByteReadersAreClassified enumerates every
// function in the binary that reads a Repo file's bytes, so a new download
// path fails until it is mapped to one of these routes and checked.
const (
	routeResolve     = "GET /{repo}/resolve/{rev}/{path}"
	routeLFSBatch    = "POST /{repo}.git/info/lfs/objects/batch (download)"
	routeLFSObject   = "GET /{repo}.git/info/lfs/objects/{oid} (signed)"
	routeOllamaBlob  = "GET /v2/{ns}/{name}/blobs/{digest}"
	routeZipStart    = "POST /api/hangar/v1/repos/{type}/{ns}/{name}/zip"
	routeZipDownload = "GET /api/hangar/v1/exports/{file} (signed)"
	routePullThrough = "GET /api/hangar/v1/pull-through/{repo}/{oid} (signed)"
)

var unsafeDownloadRoutes = []string{
	routeResolve, routeLFSBatch, routeLFSObject, routeOllamaBlob, routeZipStart, routeZipDownload, routePullThrough,
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

	// A Mirror pulled with history only, so its pickle downloads through
	// Pull-through.
	fake := upstreamtest.NewFake(t)
	upSrv := upstreamtest.NewServer(t, fake)
	upID := model.RepoID{Type: model.RepoTypeModel, Namespace: "acme", Name: "tiny"}
	upstreamtest.Seed(t, fake, upID, nil, map[string][]byte{"evil.pkl": scantest.OSSystemProtocol0()})
	_, err = in.registry.Create(ctx, upstream.UpstreamParams{Name: "fake", BaseURL: upSrv.URL, Enabled: true, NamespacePrefix: "fake-"}, model.System())
	require.NoError(t, err)
	on, everything, role := true, []string{"*"}, "user"
	_, err = in.auth.Settings.Update(ctx, auth.SettingsPatch{PullThroughEnabled: &on, PullThroughAllow: &everything, PullThroughMinRole: &role}, model.System())
	require.NoError(t, err)
	root, err := in.auth.Users.Create(ctx, auth.NewUser{Username: "root", Password: "a long enough password 1", IsAdmin: true}, model.System())
	require.NoError(t, err)
	pullID, _, err := in.pulls.StartPull(ctx, upstream.PullRequest{Upstream: "fake", Repo: upID, Selection: upstream.Selection{None: true},
		Now: true, Actor: root.Principal(model.AuthSession)})
	require.NoError(t, err)

	// A zip of the Repo, exported while blocking is off.
	zipJob := func() jobs.JobID {
		t.Helper()
		resp, body := do0(t, p, token, http.MethodPost, pub+"/api/hangar/v1/repos/models/alice/m/zip", "")
		require.Equal(t, http.StatusAccepted, resp.StatusCode, body)
		var j struct {
			ID string `json:"id"`
		}
		require.NoError(t, json.Unmarshal([]byte(body), &j), body)
		id, err := uuid.Parse(j.ID)
		require.NoError(t, err)
		return id
	}()
	runJobs(t, in, pullID, zipJob)
	var zr repo.ZipResult
	j, err := in.queue.Get(ctx, zipJob)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(j.Result, &zr))
	require.True(t, strings.HasPrefix(zr.URL, pub+api.ExportPath), "the FS Blob store cannot presign, so Hangar serves the zip: %s", zr.URL)
	mirror, err := in.repos.Get(ctx, model.RepoID{Type: model.RepoTypeModel, Namespace: "fake-acme", Name: "tiny"})
	require.NoError(t, err)
	pickle := scantest.OSSystemProtocol0()
	sum := sha256.Sum256(pickle)
	pullThroughHref, ok, err := in.pulls.PullThrough().DownloadURL(ctx, p, mirror, hex.EncodeToString(sum[:]), int64(len(pickle)))
	require.NoError(t, err)
	require.True(t, ok)
	require.True(t, strings.HasPrefix(pullThroughHref, pub+upstream.PullThroughPath), pullThroughHref)

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
		routeResolve: func() {
			resp, _ := do(http.MethodGet, pub+"/alice/m/resolve/main/evil.pkl", "", false)
			require.Equal(t, http.StatusForbidden, resp.StatusCode)
			require.Contains(t, resp.Header.Get("X-Error-Message"), "accept_risk=1")
			resp, _ = do(http.MethodGet, pub+"/alice/m/resolve/main/evil.pkl?accept_risk=1", "", false)
			require.Equal(t, http.StatusOK, resp.StatusCode, "accept_risk=1 downloads it anyway")
		},
		routeLFSBatch: func() {
			resp, body := do(http.MethodPost, pub+"/alice/m.git/info/lfs/objects/batch", lfsBatch, true)
			require.Equal(t, http.StatusOK, resp.StatusCode, body)
			require.Contains(t, body, `"code":403`)
			require.Contains(t, body, "evil.pkl is marked unsafe")
			require.NotContains(t, body, `"download"`, "no download action is issued")
		},
		routeLFSObject: func() {
			resp, body := do(http.MethodGet, href, "", false)
			require.Equal(t, http.StatusForbidden, resp.StatusCode, body)
			require.Contains(t, body, "marked unsafe")
		},
		routeOllamaBlob: func() {
			resp, body := do(http.MethodGet, pub+"/v2/alice/m/blobs/sha256:"+oid, "", false)
			require.Equal(t, http.StatusForbidden, resp.StatusCode, body)
			require.Contains(t, body, "DENIED")
		},
		routeZipStart: func() {
			resp, body := do(http.MethodPost, pub+"/api/hangar/v1/repos/models/alice/m/zip", "", true)
			require.Equal(t, http.StatusForbidden, resp.StatusCode, body)
			require.Contains(t, body, "cannot include it")
		},
		routeZipDownload: func() {
			resp, body := do(http.MethodGet, zr.URL, "", false)
			require.Equal(t, http.StatusForbidden, resp.StatusCode, body)
			require.Contains(t, body, "evil.pkl is marked unsafe")
			require.Contains(t, body, "cannot include it")
		},
		routePullThrough: func() {
			cdn := upSrv.Requests("cdn")
			resp, body := do(http.MethodGet, pullThroughHref, "", false)
			require.Equal(t, http.StatusForbidden, resp.StatusCode, body)
			require.Contains(t, body, "evil.pkl")
			require.Equal(t, cdn, upSrv.Requests("cdn"), "a refused download never reaches the Upstream")
		},
	}
	require.Len(t, checks, len(unsafeDownloadRoutes), "every listed route has a check")
	for _, route := range unsafeDownloadRoutes {
		t.Run(route, func(t *testing.T) { checks[route]() })
	}

	// Safe files still download everywhere.
	resp, _ := do(http.MethodGet, pub+"/alice/m/resolve/main/README.md", "", false)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	// Turned off again, the links issued earlier download.
	setBlock(false)
	resp, body := do(http.MethodGet, href, "", false)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, string(scantest.OSSystemProtocol0()), body)
	resp, body = do(http.MethodGet, zr.URL, "", false)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	require.Equal(t, "application/zip", resp.Header.Get("Content-Type"))
	resp, body = do(http.MethodGet, pullThroughHref, "", false)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	require.Equal(t, string(scantest.OSSystemProtocol0()), body)
}

// do0 sends a request with Basic credentials, for set-up before the
// test's own helper exists.
func do0(t *testing.T, p model.Principal, token, method, url, body string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	require.NoError(t, err)
	req.SetBasicAuth(p.Username, token)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp, string(raw)
}

// runJobs runs queued Jobs until each of ids has finished, and fails the
// test if one did not succeed.
func runJobs(t *testing.T, in *instance, ids ...jobs.JobID) {
	t.Helper()
	ctx := context.Background()
	w := in.newWorker()
	deadline := time.Now().Add(2 * time.Minute)
	for _, id := range ids {
		for {
			j, err := in.queue.Get(ctx, id)
			require.NoError(t, err)
			if j.Status == jobs.StatusSucceeded {
				break
			}
			require.NotContains(t, []jobs.Status{jobs.StatusFailed, jobs.StatusCancelled}, j.Status, "%s: %s", j.Kind, j.Error)
			require.True(t, time.Now().Before(deadline), "%s is still %s", j.Kind, j.Status)
			w.Tick(ctx)
			w.Wait()
		}
	}
}

func jsonInt(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}
