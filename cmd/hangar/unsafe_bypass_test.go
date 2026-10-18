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

	"github.com/hangar-hub/hangar/internal/auth"
	"github.com/hangar-hub/hangar/internal/config"
	"github.com/hangar-hub/hangar/internal/model"
	"github.com/hangar-hub/hangar/internal/repo"
	"github.com/hangar-hub/hangar/internal/scan/scantest"
)

// TestUnsafeDownloadGateCatchesScannerBypasses is the end-to-end
// regression for the scanner bypasses of the phase 3 security review:
// each file runs code when a standard loader opens it, and before the fix
// the scan reported it safe or suspicious, or read nothing, so it
// downloaded with scan.block_unsafe_downloads on. Each is now refused
// through the Hub API, git-lfs and the Ollama registry.
// bypassServer is a full server with one public Repo, alice/bypasses,
// for the download gate's end-to-end tests.
type bypassServer struct {
	in    *instance
	pub   string
	token string
	actor model.Principal
	id    model.RepoID
}

func newBypassServer(t *testing.T) *bypassServer {
	t.Helper()
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
	return &bypassServer{in: in, pub: pub, token: token, actor: p, id: id}
}

// commit adds files to main and deletes paths from it.
func (b *bypassServer) commit(t *testing.T, files map[string][]byte, deletes ...string) model.CommitSHA {
	t.Helper()
	var ops []repo.Operation
	for path, content := range files {
		ops = append(ops, repo.Operation{Kind: repo.OpAdd, Path: path, Content: bytes.NewReader(content)})
	}
	for _, path := range deletes {
		ops = append(ops, repo.Operation{Kind: repo.OpDelete, Path: path})
	}
	res, err := b.in.repos.Commit(context.Background(), repo.CommitParams{Repo: b.id, Revision: "main", Summary: "update", Actor: b.actor, Operations: ops})
	require.NoError(t, err)
	return res.SHA
}

// scan scans each Commit, then turns scan.block_unsafe_downloads on.
func (b *bypassServer) scanAndBlock(t *testing.T, commits ...model.CommitSHA) {
	t.Helper()
	ctx := context.Background()
	r, err := b.in.repos.Get(ctx, b.id)
	require.NoError(t, err)
	for _, c := range commits {
		_, err = b.in.scanner.ScanCommit(ctx, r.UUID, b.id, c)
		require.NoError(t, err)
	}
	on := true
	_, err = b.in.auth.Settings.Update(ctx, auth.SettingsPatch{ScanBlockUnsafeDownloads: &on}, model.System())
	require.NoError(t, err)
}

func (b *bypassServer) do(t *testing.T, method, url, body string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	require.NoError(t, err)
	if body != "" {
		req.Header.Set("Content-Type", "application/vnd.git-lfs+json")
		req.Header.Set("Accept", "application/vnd.git-lfs+json")
	}
	setToken(req, "alice", b.token)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp, string(raw)
}

// requireRefused checks that the Hub API refuses revision/path and that
// git-lfs batch and the Ollama registry refuse its LFS object, each with
// 403 and, where the protocol carries a message, reason.
func (b *bypassServer) requireRefused(t *testing.T, revision, path string, oid string, size int64, reason string) {
	t.Helper()
	resp, _ := b.do(t, http.MethodGet, b.pub+"/alice/bypasses/resolve/"+revision+"/"+path, "")
	require.Equal(t, http.StatusForbidden, resp.StatusCode, "Hub API")
	require.Contains(t, resp.Header.Get("X-Error-Message"), reason)

	batch := `{"operation":"download","transfers":["basic"],"objects":[{"oid":"` + oid + `","size":` + jsonInt(size) + `}]}`
	resp, body := b.do(t, http.MethodPost, b.pub+"/alice/bypasses.git/info/lfs/objects/batch", batch)
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
	require.Contains(t, out.Objects[0].Error.Message, reason)
	require.Nil(t, out.Objects[0].Actions)

	resp, body = b.do(t, http.MethodGet, b.pub+"/v2/alice/bypasses/blobs/sha256:"+oid, "")
	require.Equal(t, http.StatusForbidden, resp.StatusCode, "Ollama: %s", body)
}

// TestUnsafeDownloadGateCatchesScannerBypasses is the end-to-end
// regression for the scanner bypasses of the phase 3 security review:
// each file runs code when a standard loader opens it, and before the fix
// the scan reported it safe or suspicious, or read nothing, so it
// downloaded with scan.block_unsafe_downloads on. Each is now refused
// through the Hub API, git-lfs and the Ollama registry.
func TestUnsafeDownloadGateCatchesScannerBypasses(t *testing.T) {
	b := newBypassServer(t)
	ctx := context.Background()
	in, id := b.in, b.id
	commit := func(files map[string][]byte, deletes ...string) model.CommitSHA {
		t.Helper()
		return b.commit(t, files, deletes...)
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
	b.scanAndBlock(t, older, head)

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
			b.requireRefused(t, string(tg.commit), tg.path, e.LFS.OID, e.Size, tg.reason)
		})
	}

	// The tip's file of the 6c object is not a pickle by name, but the
	// object is the old pickle's, so git-lfs and Ollama refuse it there
	// too, while the Hub API serves the path, which no loader unpickles.
	e, err := in.repos.Entry(ctx, id, head, "renamed.safetensors")
	require.NoError(t, err)
	resp, _ := b.do(t, http.MethodGet, b.pub+"/v2/alice/bypasses/blobs/sha256:"+e.LFS.OID, "")
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	resp, _ = b.do(t, http.MethodGet, b.pub+"/alice/bypasses/resolve/main/README.md", "")
	require.Equal(t, http.StatusOK, resp.StatusCode, "files that cannot run code still download")
}

// TestUnsafeDownloadGateRefusesObjectsHeldOnlyByATag is the tag case of
// security input 6c through the full server: a pickle that main no longer
// holds, but a lightweight or annotated tag still does, is refused by the
// Hub API on the tag, by git-lfs batch and by the Ollama registry.
func TestUnsafeDownloadGateRefusesObjectsHeldOnlyByATag(t *testing.T) {
	b := newBypassServer(t)
	ctx := context.Background()
	light := b.commit(t, map[string][]byte{"light.pkl": scantest.OSSystemProtocol0()})
	require.NoError(t, b.in.repos.CreateTag(ctx, repo.TagParams{Repo: b.id, Tag: "v1", Revision: string(light), Actor: b.actor}))
	annotated := b.commit(t, map[string][]byte{"annotated.pkl": scantest.SubprocessInst()}, "light.pkl")
	require.NoError(t, b.in.repos.CreateTag(ctx, repo.TagParams{Repo: b.id, Tag: "v2", Revision: string(annotated), Message: "release 2", Actor: b.actor}))
	tip := b.commit(t, nil, "annotated.pkl")
	b.scanAndBlock(t, light, annotated, tip)

	for _, tc := range []struct{ tag, path string }{{"v1", "light.pkl"}, {"v2", "annotated.pkl"}} {
		t.Run(tc.tag, func(t *testing.T) {
			sha, err := b.in.repos.Resolve(ctx, b.id, tc.tag)
			require.NoError(t, err)
			e, err := b.in.repos.Entry(ctx, b.id, sha, tc.path)
			require.NoError(t, err)
			require.NotNil(t, e.LFS, tc.path)
			_, err = b.in.repos.Entry(ctx, b.id, tip, tc.path)
			require.Error(t, err, "main no longer holds %s", tc.path)
			b.requireRefused(t, tc.tag, tc.path, e.LFS.OID, e.Size, "marked unsafe")
		})
	}
}
