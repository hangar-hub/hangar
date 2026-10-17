package main

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
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

// TestUnsafeDownloadGateCatchesScannerBypasses is the end-to-end
// regression for the scanner bypasses of the phase 3 security review:
// each file runs code when a standard loader opens it, and before the fix
// the scan reported it safe or suspicious, or read nothing, so it
// downloaded with scan.block_unsafe_downloads on. Each is now refused
// through the Hub API, git-lfs and the Ollama registry.
func TestUnsafeDownloadGateCatchesScannerBypasses(t *testing.T) {
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
	id := model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "bypasses"}
	_, err = in.repos.Create(ctx, repo.CreateParams{ID: id, Actor: p, InitialReadme: true, Visibility: model.VisibilityPublic})
	require.NoError(t, err)
	commit := func(files map[string][]byte, deletes ...string) model.CommitSHA {
		t.Helper()
		var ops []repo.Operation
		for path, content := range files {
			ops = append(ops, repo.Operation{Kind: repo.OpAdd, Path: path, Content: bytes.NewReader(content)})
		}
		for _, path := range deletes {
			ops = append(ops, repo.Operation{Kind: repo.OpDelete, Path: path})
		}
		res, err := in.repos.Commit(ctx, repo.CommitParams{Repo: id, Revision: "main", Summary: "update", Actor: p, Operations: ops})
		require.NoError(t, err)
		return res.SHA
	}
	evil := scantest.OSSystemProtocol0()
	var gz bytes.Buffer
	gw := gzip.NewWriter(&gz)
	_, _ = gw.Write(evil)
	_ = gw.Close()
	var npz bytes.Buffer
	zw := zip.NewWriter(&npz)
	fw, _ := zw.Create("a.npy")
	_, _ = fw.Write(scantest.NumpyObjectArray(evil))
	_ = zw.Close()

	// 6c: an older Commit holds the pickle's bytes as old.pkl; the tip
	// holds the same LFS object under a name that does not run code.
	older := commit(map[string][]byte{"old.pkl": evil})
	commit(map[string][]byte{"renamed.safetensors": evil}, "old.pkl")
	files := map[string][]byte{
		// 6a: an .xml beside a .bin made the scanner skip it.
		"pytorch_model.bin": evil,
		"pytorch_model.xml": []byte("<net/>"),
		// joblib.load skips array data that pickle.load reads as opcodes.
		"model.joblib": scantest.JoblibHiddenOSSystem(),
		// Compressed and archived names the scanner did not read.
		"model.joblib.gz": gz.Bytes(),
		"arrays.npz":      npz.Bytes(),
		// 6b: a file the scan cannot read in full.
		"model.pkl.xz": {0xfd, '7', 'z', 'X', 'Z', 0, 1, 2},
	}
	head := commit(files)
	r, err := in.repos.Get(ctx, id)
	require.NoError(t, err)
	for _, c := range []model.CommitSHA{older, head} {
		_, err = in.scanner.ScanCommit(ctx, r.UUID, id, c)
		require.NoError(t, err)
	}

	on := true
	_, err = in.auth.Settings.Update(ctx, auth.SettingsPatch{ScanBlockUnsafeDownloads: &on}, model.System())
	require.NoError(t, err)

	do := func(method, url, body string) (*http.Response, string) {
		t.Helper()
		req, err := http.NewRequest(method, url, strings.NewReader(body))
		require.NoError(t, err)
		if body != "" {
			req.Header.Set("Content-Type", "application/vnd.git-lfs+json")
			req.Header.Set("Accept", "application/vnd.git-lfs+json")
		}
		req.SetBasicAuth("alice", token)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return resp, string(raw)
	}
	type target struct {
		commit model.CommitSHA
		path   string
		reason string
	}
	targets := []target{
		{head, "pytorch_model.bin", "marked unsafe"},
		{head, "model.joblib", "marked unsafe"},
		{head, "model.joblib.gz", "marked unsafe"},
		{head, "arrays.npz", "marked unsafe"},
		{head, "model.pkl.xz", "could not be read in full"},
		{older, "old.pkl", "marked unsafe"},
	}
	for _, tg := range targets {
		t.Run(tg.path, func(t *testing.T) {
			e, err := in.repos.Entry(ctx, id, tg.commit, tg.path)
			require.NoError(t, err)
			require.NotNil(t, e.LFS, tg.path)
			oid := e.LFS.OID

			resp, _ := do(http.MethodGet, pub+"/alice/bypasses/resolve/"+string(tg.commit)+"/"+tg.path, "")
			require.Equal(t, http.StatusForbidden, resp.StatusCode, "Hub API")
			require.Contains(t, resp.Header.Get("X-Error-Message"), tg.reason)

			batch := `{"operation":"download","transfers":["basic"],"objects":[{"oid":"` + oid + `","size":` + jsonInt(e.Size) + `}]}`
			resp, body := do(http.MethodPost, pub+"/alice/bypasses.git/info/lfs/objects/batch", batch)
			require.Equal(t, http.StatusOK, resp.StatusCode, body)
			var out struct {
				Objects []struct {
					Actions map[string]any `json:"actions"`
					Error   struct {
						Code    int    `json:"code"`
						Message string `json:"message"`
					} `json:"error"`
				} `json:"objects"`
			}
			require.NoError(t, json.Unmarshal([]byte(body), &out), body)
			require.Len(t, out.Objects, 1)
			require.Equal(t, http.StatusForbidden, out.Objects[0].Error.Code, "git-lfs batch: %s", body)
			require.Contains(t, out.Objects[0].Error.Message, tg.reason)
			require.Nil(t, out.Objects[0].Actions)

			resp, body = do(http.MethodGet, pub+"/v2/alice/bypasses/blobs/sha256:"+oid, "")
			require.Equal(t, http.StatusForbidden, resp.StatusCode, "Ollama: %s", body)
		})
	}

	// The tip's file of the 6c object is not a pickle by name, but the
	// object is the old pickle's, so git-lfs and Ollama refuse it there
	// too, while the Hub API serves the path, which no loader unpickles.
	e, err := in.repos.Entry(ctx, id, head, "renamed.safetensors")
	require.NoError(t, err)
	resp, _ := do(http.MethodGet, pub+"/v2/alice/bypasses/blobs/sha256:"+e.LFS.OID, "")
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	resp, _ = do(http.MethodGet, pub+"/alice/bypasses/resolve/main/README.md", "")
	require.Equal(t, http.StatusOK, resp.StatusCode, "files that cannot run code still download")
}
