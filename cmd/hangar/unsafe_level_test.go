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
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/auth"
	"github.com/hangar-hub/hangar/internal/config"
	"github.com/hangar-hub/hangar/internal/model"
	"github.com/hangar-hub/hangar/internal/repo"
	"github.com/hangar-hub/hangar/internal/scan/scantest"
	"github.com/hangar-hub/hangar/internal/upstream"
)

// TestSuspiciousDownloadsFollowTheBlockLevel checks scan.block_level,
// decided by James on 2026-10-18, on every route that serves a Repo
// file's bytes (the list TestUnsafeDownloadGateCoversEveryRoute keeps):
// with scan.block_unsafe_downloads on, a pickle whose imports are on
// neither the allow nor the deny list downloads at level unsafe, the
// default, and is refused at level suspicious, where accept_risk=1 still
// downloads it through the Hub API.
func TestSuspiciousDownloadsFollowTheBlockLevel(t *testing.T) {
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
	suspicious := scantest.SklearnSuspicious()
	_, err = in.repos.Commit(ctx, repo.CommitParams{Repo: id, Revision: "main", Summary: "Add an sklearn model", Actor: p,
		Operations: []repo.Operation{{Kind: repo.OpAdd, Path: "model.pkl", Content: bytes.NewReader(suspicious)}}})
	require.NoError(t, err)
	// A suspicious pickle stored in git, not LFS, which a git clone sends.
	_, err = in.repos.Commit(ctx, repo.CommitParams{Repo: id, Revision: "main", Summary: "Track nothing with LFS", Actor: p,
		Operations: []repo.Operation{{Kind: repo.OpAdd, Path: ".gitattributes", Content: strings.NewReader("*.txt -text\n")}}})
	require.NoError(t, err)
	_, err = in.repos.Commit(ctx, repo.CommitParams{Repo: id, Revision: "main", Summary: "Add a pickle to git", Actor: p,
		Operations: []repo.Operation{{Kind: repo.OpAdd, Path: "in-git.pkl", Content: bytes.NewReader(suspicious)}}})
	require.NoError(t, err)
	r, err := in.repos.Get(ctx, id)
	require.NoError(t, err)
	head, err := in.repos.Resolve(ctx, id, "")
	require.NoError(t, err)
	e, err := in.repos.Entry(ctx, id, head, "model.pkl")
	require.NoError(t, err)
	require.NotNil(t, e.LFS)
	oid := e.LFS.OID
	res, err := in.scanner.ScanCommit(ctx, r.UUID, id, head)
	require.NoError(t, err)
	require.Equal(t, "suspicious", string(res.Status), "the scan finds sklearn imports on neither list")

	// A zip of the Repo, exported while blocking is off.
	resp, body := do0(t, p, token, http.MethodPost, pub+"/api/hangar/v1/repos/models/alice/m/zip", "")
	require.Equal(t, http.StatusAccepted, resp.StatusCode, body)
	var j struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &j), body)
	zipJob, err := uuid.Parse(j.ID)
	require.NoError(t, err)
	runJobs(t, in, zipJob)
	var zr repo.ZipResult
	job, err := in.queue.Get(ctx, zipJob)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(job.Result, &zr))

	do := func(method, url, body string, authed bool) (*http.Response, string) {
		t.Helper()
		req, err := http.NewRequest(method, url, strings.NewReader(body))
		require.NoError(t, err)
		if body != "" {
			req.Header.Set("Content-Type", "application/vnd.git-lfs+json")
			req.Header.Set("Accept", "application/vnd.git-lfs+json")
		}
		if authed {
			setToken(req, "alice", token)
		}
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return resp, string(raw)
	}
	lfsBatch := `{"operation":"download","transfers":["basic"],"objects":[{"oid":"` + oid + `","size":` + jsonInt(e.Size) + `}]}`
	resp, body = do(http.MethodPost, pub+"/alice/m.git/info/lfs/objects/batch", lfsBatch, true)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var batch struct {
		Objects []struct {
			Actions map[string]struct {
				Href string `json:"href"`
			} `json:"actions"`
		} `json:"objects"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &batch), body)
	require.Len(t, batch.Objects, 1, body)
	href := batch.Objects[0].Actions["download"].Href
	require.NotEmpty(t, href)
	// A Pull-through link for a Repo that is not a Mirror: the gate decides
	// it before the Mirror lookup, so it shows the level without an
	// Upstream.
	pullThroughHref, err := in.signer.Sign(pub+upstream.PullThroughPath+r.UUID.String()+"/"+oid+"?size="+jsonInt(e.Size),
		"pull-through:"+r.UUID.String()+":"+oid, time.Hour)
	require.NoError(t, err)

	on := true
	_, err = in.auth.Settings.Update(ctx, auth.SettingsPatch{ScanBlockUnsafeDownloads: &on}, model.System())
	require.NoError(t, err)
	setLevel := func(level string) {
		t.Helper()
		_, err := in.auth.Settings.Update(ctx, auth.SettingsPatch{ScanBlockLevel: &level}, model.System())
		require.NoError(t, err)
	}
	uploadPack := func() (*http.Response, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, pub+"/alice/m.git/git-upload-pack", strings.NewReader("0032want "+string(head)+"\n00000009done\n"))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/x-git-upload-pack-request")
		req.SetBasicAuth("alice", token)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return resp, string(raw)
	}

	// served checks each route serves the suspicious file; refused checks
	// each refuses it with the suspicious reason.
	type check struct{ served, refused func(t *testing.T) }
	const reason = "imports code that Hangar's security scan does not know to be safe"
	checks := map[string]check{
		routeResolve: {
			served: func(t *testing.T) {
				resp, body := do(http.MethodGet, pub+"/alice/m/resolve/main/model.pkl", "", false)
				require.Equal(t, http.StatusOK, resp.StatusCode, body)
				require.Equal(t, string(suspicious), body)
			},
			refused: func(t *testing.T) {
				resp, _ := do(http.MethodGet, pub+"/alice/m/resolve/main/model.pkl", "", false)
				require.Equal(t, http.StatusForbidden, resp.StatusCode)
				require.Contains(t, resp.Header.Get("X-Error-Message"), reason)
				require.Contains(t, resp.Header.Get("X-Error-Message"), "accept_risk=1")
				resp, body := do(http.MethodGet, pub+"/alice/m/resolve/main/model.pkl?accept_risk=1", "", false)
				require.Equal(t, http.StatusOK, resp.StatusCode, "accept_risk=1 downloads it anyway")
				require.Equal(t, string(suspicious), body)
			},
		},
		routeLFSBatch: {
			served: func(t *testing.T) {
				resp, body := do(http.MethodPost, pub+"/alice/m.git/info/lfs/objects/batch", lfsBatch, true)
				require.Equal(t, http.StatusOK, resp.StatusCode, body)
				require.Contains(t, body, `"download"`)
			},
			refused: func(t *testing.T) {
				resp, body := do(http.MethodPost, pub+"/alice/m.git/info/lfs/objects/batch", lfsBatch, true)
				require.Equal(t, http.StatusOK, resp.StatusCode, body)
				require.Contains(t, body, `"code":403`)
				require.Contains(t, body, "model.pkl "+reason)
				require.NotContains(t, body, `"download"`, "no download action is issued")
			},
		},
		routeLFSObject: {
			served: func(t *testing.T) {
				resp, body := do(http.MethodGet, href, "", false)
				require.Equal(t, http.StatusOK, resp.StatusCode, body)
				require.Equal(t, string(suspicious), body)
			},
			refused: func(t *testing.T) {
				resp, body := do(http.MethodGet, href, "", false)
				require.Equal(t, http.StatusForbidden, resp.StatusCode, body)
				require.Contains(t, body, reason)
			},
		},
		routeOllamaBlob: {
			served: func(t *testing.T) {
				resp, body := do(http.MethodGet, pub+"/v2/alice/m/blobs/sha256:"+oid, "", false)
				require.Equal(t, http.StatusOK, resp.StatusCode, body)
				require.Equal(t, string(suspicious), body)
			},
			refused: func(t *testing.T) {
				resp, body := do(http.MethodGet, pub+"/v2/alice/m/blobs/sha256:"+oid, "", false)
				require.Equal(t, http.StatusForbidden, resp.StatusCode, body)
				require.Contains(t, body, "DENIED")
			},
		},
		routeZipStart: {
			served: func(t *testing.T) {
				resp, body := do(http.MethodPost, pub+"/api/hangar/v1/repos/models/alice/m/zip", "", true)
				require.Equal(t, http.StatusAccepted, resp.StatusCode, body)
			},
			refused: func(t *testing.T) {
				resp, body := do(http.MethodPost, pub+"/api/hangar/v1/repos/models/alice/m/zip", "", true)
				require.Equal(t, http.StatusForbidden, resp.StatusCode, body)
				require.Contains(t, body, "cannot include it")
			},
		},
		routeZipDownload: {
			served: func(t *testing.T) {
				resp, body := do(http.MethodGet, zr.URL, "", false)
				require.Equal(t, http.StatusOK, resp.StatusCode, body)
				require.Equal(t, "application/zip", resp.Header.Get("Content-Type"))
			},
			refused: func(t *testing.T) {
				resp, body := do(http.MethodGet, zr.URL, "", false)
				require.Equal(t, http.StatusForbidden, resp.StatusCode, body)
				require.Contains(t, body, reason)
			},
		},
		routePullThrough: {
			served: func(t *testing.T) {
				resp, body := do(http.MethodGet, pullThroughHref, "", false)
				require.Equal(t, http.StatusNotFound, resp.StatusCode, "the gate allows it and the Repo is not a Mirror: %s", body)
			},
			refused: func(t *testing.T) {
				resp, body := do(http.MethodGet, pullThroughHref, "", false)
				require.Equal(t, http.StatusForbidden, resp.StatusCode, body)
				require.Contains(t, body, reason)
			},
		},
		routeGitUploadPack: {
			served: func(t *testing.T) {
				resp, body := uploadPack()
				require.Equal(t, http.StatusOK, resp.StatusCode, body)
			},
			refused: func(t *testing.T) {
				resp, body := uploadPack()
				require.Equal(t, http.StatusForbidden, resp.StatusCode, body)
				require.Contains(t, body, "in-git.pkl "+reason)
				require.Contains(t, body, "git clone and fetch")
			},
		},
	}
	require.Len(t, checks, len(unsafeDownloadRoutes), "every listed route has a check")

	// The default level, unset, then set explicitly.
	for _, level := range []string{"", "unsafe"} {
		name := "default level"
		if level != "" {
			setLevel(level)
			name = "level " + level
		}
		for _, route := range unsafeDownloadRoutes {
			t.Run(name+"/"+route, func(t *testing.T) { checks[route].served(t) })
		}
	}
	setLevel("suspicious")
	for _, route := range unsafeDownloadRoutes {
		t.Run("level suspicious/"+route, func(t *testing.T) { checks[route].refused(t) })
	}
	// Safe files still download.
	resp, _ = do(http.MethodGet, pub+"/alice/m/resolve/main/README.md", "", false)
	require.Equal(t, http.StatusOK, resp.StatusCode)
}
