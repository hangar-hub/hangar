package upstream_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/blob"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/repo"
	"github.com/jamesagarside/hangar/internal/upstream"
	"github.com/jamesagarside/hangar/internal/upstream/upstreamtest"
)

func noSleep(context.Context, time.Duration) error { return nil }

func newClient() *upstream.HTTPClient {
	return upstream.NewHTTPClient(upstream.HTTPOptions{Version: "test", Sleep: noSleep, BaseBackoff: time.Millisecond})
}

// seedRemote creates a model with a config.json, an LFS weights file and
// a GGUF file on the Fake.
func seedRemote(t *testing.T, f *upstreamtest.Fake, id model.RepoID, files map[string][]byte, lfs map[string][]byte) model.CommitSHA {
	t.Helper()
	ctx := context.Background()
	if _, err := f.Repos.Get(ctx, id); errors.Is(err, repo.ErrNotFound) {
		_, err := f.Repos.Create(ctx, repo.CreateParams{ID: id, Visibility: model.VisibilityPublic})
		require.NoError(t, err)
	}
	var ops []repo.Operation
	for p, c := range files {
		ops = append(ops, repo.Operation{Kind: repo.OpAdd, Path: p, Content: bytes.NewReader(c)})
	}
	for p, c := range lfs {
		obj, err := f.Repos.LFS.Put(ctx, c)
		require.NoError(t, err)
		ops = append(ops, repo.Operation{Kind: repo.OpAddLFS, Path: p, OID: obj.OID, Size: obj.Size})
	}
	res, err := f.Repos.Commit(ctx, repo.CommitParams{Repo: id, Summary: "add files", Operations: ops, Actor: model.System(), AllowMirror: true})
	require.NoError(t, err)
	return res.SHA
}

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

var tiny = model.RepoID{Type: model.RepoTypeModel, Namespace: "acme", Name: "tiny"}

func TestHTTPClientRepoInfoTreeDownload(t *testing.T) {
	ctx := context.Background()
	f := upstreamtest.NewFake(t)
	weights := bytes.Repeat([]byte("weights!"), 1000)
	commit := seedRemote(t, f, tiny, map[string][]byte{"config.json": []byte(`{"model_type":"bert"}`), "sub/notes.txt": []byte("hi")},
		map[string][]byte{"model.safetensors": weights})
	srv := upstreamtest.NewServer(t, f)
	up := srv.Upstream("test")
	c := newClient()

	info, err := c.RepoInfo(ctx, up, upstream.Credentials{}, tiny, "main")
	require.NoError(t, err)
	require.Equal(t, commit, info.SHA)
	require.Equal(t, tiny, info.ID)
	var lfs *upstream.Sibling
	for i := range info.Siblings {
		if info.Siblings[i].Path == "model.safetensors" {
			lfs = &info.Siblings[i]
		}
	}
	require.NotNil(t, lfs)
	require.Equal(t, sha(weights), lfs.LFS.OID)
	require.Equal(t, int64(len(weights)), lfs.Size)

	entries, err := c.Tree(ctx, up, upstream.Credentials{}, tiny, "main", "", true)
	require.NoError(t, err)
	paths := map[string]model.Entry{}
	for _, e := range entries {
		paths[e.Path] = e
	}
	require.Contains(t, paths, "sub")
	require.Equal(t, model.EntryDirectory, paths["sub"].Type)
	require.Contains(t, paths, "sub/notes.txt")
	require.Equal(t, sha(weights), paths["model.safetensors"].LFS.OID)
	require.Greater(t, srv.Requests("tree"), 1, "the tree was paged")

	rc, meta, err := c.Download(ctx, up, upstream.Credentials{Token: "secret-token"}, tiny, commit, "model.safetensors", nil)
	require.NoError(t, err)
	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	require.NoError(t, rc.Close())
	require.Equal(t, weights, got)
	require.Equal(t, sha(weights), meta.ETag)
	require.True(t, meta.LFS)
	require.Equal(t, int64(len(weights)), meta.Size)
	require.Equal(t, commit, meta.Commit)
	require.Zero(t, srv.AuthOnCDN(), "the token must not follow the redirect to another origin")

	rc, meta, err = c.Download(ctx, up, upstream.Credentials{}, tiny, commit, "model.safetensors", &blob.Range{Start: 100, End: -1})
	require.NoError(t, err)
	got, _ = io.ReadAll(rc)
	_ = rc.Close()
	require.Equal(t, weights[100:], got)
	require.Equal(t, int64(len(weights)), meta.Size)

	rc, meta, err = c.Download(ctx, up, upstream.Credentials{}, tiny, commit, "config.json", nil)
	require.NoError(t, err)
	got, _ = io.ReadAll(rc)
	_ = rc.Close()
	require.JSONEq(t, `{"model_type":"bert"}`, string(got))
	require.False(t, meta.LFS)

	_, _, err = c.Download(ctx, up, upstream.Credentials{}, tiny, commit, "missing.bin", nil)
	require.ErrorIs(t, err, upstream.ErrNotFound)
	_, err = c.RepoInfo(ctx, up, upstream.Credentials{}, model.RepoID{Type: model.RepoTypeModel, Namespace: "acme", Name: "nope"}, "")
	require.ErrorIs(t, err, upstream.ErrNotFound, "401 with RepoNotFound maps to not found")
	_, err = c.RepoInfo(ctx, up, upstream.Credentials{}, tiny, "no-such-branch")
	require.ErrorIs(t, err, upstream.ErrNotFound)
}

func TestHTTPClientGatedPrivateAndSearch(t *testing.T) {
	ctx := context.Background()
	f := upstreamtest.NewFake(t)
	f.RequireToken = "good"
	seedRemote(t, f, tiny, map[string][]byte{"a.txt": []byte("a")}, nil)
	priv := model.RepoID{Type: model.RepoTypeModel, Namespace: "acme", Name: "secret"}
	_, err := f.Repos.Create(ctx, repo.CreateParams{ID: priv, Visibility: model.VisibilityPrivate})
	require.NoError(t, err)
	gated := model.RepoID{Type: model.RepoTypeModel, Namespace: "acme", Name: "gated"}
	seedRemote(t, f, gated, map[string][]byte{"a.txt": []byte("a")}, nil)
	f.Gated = []string{gated.String()}
	srv := upstreamtest.NewServer(t, f)
	up := srv.Upstream("test")
	c := newClient()

	_, err = c.RepoInfo(ctx, up, upstream.Credentials{}, priv, "")
	require.ErrorIs(t, err, upstream.ErrNotFound)
	info, err := c.RepoInfo(ctx, up, upstream.Credentials{Token: "good"}, priv, "")
	require.NoError(t, err)
	require.True(t, info.Private)
	_, err = c.RepoInfo(ctx, up, upstream.Credentials{}, gated, "")
	require.ErrorIs(t, err, upstream.ErrGated)

	page, err := c.Search(ctx, up, upstream.Credentials{}, upstream.SearchQuery{Type: model.RepoTypeModel, Search: "acme", Limit: 1, Sort: "trending"})
	require.NoError(t, err)
	require.Len(t, page.Repos, 1)
	require.NotEmpty(t, page.NextCursor)
	page2, err := c.Search(ctx, up, upstream.Credentials{}, upstream.SearchQuery{Type: model.RepoTypeModel, Search: "acme", Limit: 1, Cursor: page.NextCursor})
	require.NoError(t, err)
	require.Len(t, page2.Repos, 1)
	require.Empty(t, page2.NextCursor, "two public Repos match, so the second page is the last")
	_, err = c.Search(ctx, up, upstream.Credentials{}, upstream.SearchQuery{Type: model.RepoTypeModel, Sort: "stars"})
	require.Error(t, err)
}

func TestHTTPClientRetriesAndRateLimits(t *testing.T) {
	ctx := context.Background()
	f := upstreamtest.NewFake(t)
	seedRemote(t, f, tiny, map[string][]byte{"a.txt": []byte("a")}, nil)
	srv := upstreamtest.NewServer(t, f)
	up := srv.Upstream("test")
	var waits []time.Duration
	c := upstream.NewHTTPClient(upstream.HTTPOptions{Version: "test", BaseBackoff: time.Second, MaxBackoff: time.Minute, MaxAttempts: 3,
		Sleep: func(_ context.Context, d time.Duration) error { waits = append(waits, d); return nil }})

	srv.Fail("/api/models/acme/tiny", http.StatusServiceUnavailable, 2, "")
	_, err := c.RepoInfo(ctx, up, upstream.Credentials{}, tiny, "")
	require.NoError(t, err, "two 503s then success")
	require.Len(t, waits, 2)
	for _, w := range waits {
		require.Positive(t, w)
		require.LessOrEqual(t, w, 2*time.Second, "full jitter stays under the exponential ceiling")
	}

	waits = nil
	srv.Fail("/api/models/acme/tiny", http.StatusTooManyRequests, 1, "7")
	_, err = c.RepoInfo(ctx, up, upstream.Credentials{}, tiny, "")
	require.NoError(t, err)
	require.Equal(t, []time.Duration{7 * time.Second}, waits, "Retry-After is honoured")

	srv.Fail("/api/models/acme/tiny", http.StatusTooManyRequests, 1, "3600")
	_, err = c.RepoInfo(ctx, up, upstream.Credentials{}, tiny, "")
	var rl *upstream.RateLimitError
	require.ErrorAs(t, err, &rl, "a Retry-After past the maximum backoff is returned, not waited for")
	require.Equal(t, time.Hour, rl.RetryAfter)

	srv.Fail("/api/models/acme/tiny", http.StatusBadGateway, 5, "")
	_, err = c.RepoInfo(ctx, up, upstream.Credentials{}, tiny, "")
	require.ErrorIs(t, err, upstream.ErrUnavailable)
}

func TestHTTPClientRedactsPresignedURLs(t *testing.T) {
	ctx := context.Background()
	// The Hub redirects to a CDN that drops the connection: the error
	// must not contain the presigned query string.
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hj, _ := w.(http.Hijacker)
		conn, _, _ := hj.Hijack()
		_ = conn.Close()
	}))
	defer cdn.Close()
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Repo-Commit", strings.Repeat("a", 40))
		http.Redirect(w, r, cdn.URL+"/blob/x?X-Amz-Credential=AKIDSECRET&X-Amz-Signature="+upstreamtest.PresignSecret, http.StatusFound)
	}))
	defer hub.Close()
	c := newClient()
	_, _, err := c.Download(ctx, upstream.Upstream{Name: "x", BaseURL: hub.URL}, upstream.Credentials{Token: "tok-secret"}, tiny,
		model.CommitSHA(strings.Repeat("a", 40)), "f.bin", nil)
	require.Error(t, err)
	require.ErrorIs(t, err, upstream.ErrUnavailable)
	require.NotContains(t, err.Error(), upstreamtest.PresignSecret)
	require.NotContains(t, err.Error(), "AKIDSECRET")
	require.NotContains(t, err.Error(), "tok-secret")
	require.Contains(t, err.Error(), "/blob/x")
}

func TestHTTPClientFetchPack(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Fatal("git is required")
	}
	for _, v2 := range []bool{true, false} {
		name := "v0"
		if v2 {
			name = "v2"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			f := upstreamtest.NewFake(t)
			first := seedRemote(t, f, tiny, map[string][]byte{"a.txt": []byte("a")}, map[string][]byte{"w.bin": []byte("weights")})
			require.NoError(t, f.Repos.CreateTag(ctx, repo.TagParams{Repo: tiny, Tag: "v1", Revision: "main", Message: "annotated", Actor: model.System()}))
			srv := upstreamtest.NewServer(t, f)
			srv.NoProtocolV2 = !v2
			up := srv.Upstream("test")
			c := newClient()

			res, err := c.FetchPack(ctx, up, upstream.Credentials{}, tiny, nil)
			require.NoError(t, err)
			require.NotNil(t, res.Pack)
			require.Equal(t, first, res.Refs["refs/heads/main"])
			require.Equal(t, first, res.Peeled["refs/tags/v1"], "annotated tags are peeled")
			require.NotEqual(t, first, res.Refs["refs/tags/v1"], "the ref names the tag object")
			// The pack indexes in a fresh repository and holds the Commit.
			dir := t.TempDir()
			gitRun(t, dir, nil, "init", "--bare", "-q")
			pack, err := io.ReadAll(res.Pack)
			require.NoError(t, err)
			require.NoError(t, res.Pack.Close())
			gitRun(t, dir, pack, "index-pack", "--stdin", "--fix-thin")
			gitRun(t, dir, nil, "cat-file", "-e", string(first))

			// With the tip as a have, nothing new comes down.
			res, err = c.FetchPack(ctx, up, upstream.Credentials{}, tiny, []model.CommitSHA{first, res.Refs["refs/tags/v1"]})
			require.NoError(t, err)
			require.Nil(t, res.Pack)

			// A new Commit comes down as a pack of only the new objects.
			second := seedRemote(t, f, tiny, map[string][]byte{"b.txt": []byte("b")}, nil)
			res, err = c.FetchPack(ctx, up, upstream.Credentials{}, tiny, []model.CommitSHA{first})
			require.NoError(t, err)
			require.NotNil(t, res.Pack)
			require.Equal(t, second, res.Refs["refs/heads/main"])
			pack, _ = io.ReadAll(res.Pack)
			_ = res.Pack.Close()
			gitRun(t, dir, pack, "index-pack", "--stdin", "--fix-thin")
			gitRun(t, dir, nil, "cat-file", "-e", string(second))
		})
	}
}

func gitRun(t *testing.T, dir string, stdin []byte, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
	return string(out)
}

func TestHTTPClientViewer(t *testing.T) {
	ctx := context.Background()
	f := upstreamtest.NewFake(t)
	srv := upstreamtest.NewServer(t, f)
	f.SetViewerResponse("/splits", map[string][]string{"dataset": {"acme/data"}}, `{"splits":[]}`)
	raw, err := newClient().Viewer(ctx, srv.Upstream("test"), upstream.Credentials{}, "/splits", map[string][]string{"dataset": {"acme/data"}})
	require.NoError(t, err)
	require.JSONEq(t, `{"splits":[]}`, string(raw))
	_, err = newClient().Viewer(ctx, srv.Upstream("test"), upstream.Credentials{}, "/../admin", nil)
	require.Error(t, err)
}
