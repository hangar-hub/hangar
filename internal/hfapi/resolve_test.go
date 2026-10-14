package hfapi_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/blob/blobtest"
	"github.com/jamesagarside/hangar/internal/hfapi"
	"github.com/jamesagarside/hangar/internal/hfapi/hfapitest"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/repo"
	"github.com/jamesagarside/hangar/internal/repo/repotest"
)

const configJSON = `{"model_type": "bert"}` + "\n"

func sha256hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func TestResolveRegularFile(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	id := modelID("alice", "tiny")
	_, sha := seed(t, e, "alice", id, model.VisibilityPublic, map[string]string{"config.json": configJSON, "sub/dir/ü.txt": "hello"})
	etag := `"` + model.GitBlobID([]byte(configJSON)) + `"`

	for _, method := range []string{http.MethodHead, http.MethodGet} {
		r := e.Request(t, method, "/alice/tiny/resolve/main/config.json", "alice", "")
		require.Equal(t, http.StatusOK, r.StatusCode)
		require.Equal(t, string(sha), r.Header.Get("X-Repo-Commit"))
		require.Equal(t, etag, r.Header.Get("ETag"))
		require.Equal(t, etag, r.Header.Get("X-Linked-Etag"))
		require.Equal(t, strconv.Itoa(len(configJSON)), r.Header.Get("Content-Length"))
		require.Equal(t, strconv.Itoa(len(configJSON)), r.Header.Get("X-Linked-Size"))
		require.Equal(t, "bytes", r.Header.Get("Accept-Ranges"))
		require.Empty(t, r.Header.Get("Content-Encoding"))
		require.Empty(t, r.Header.Get("X-Xet-Hash"))
		require.Empty(t, r.Header.Get("X-Xet-Refresh-Route"))
		require.Empty(t, r.Header.Get("Link"))
		require.Contains(t, r.Header.Get("Content-Disposition"), `filename="config.json"`)
		if method == http.MethodGet {
			require.Equal(t, configJSON, string(readBody(t, r)))
		}
	}
	// By full SHA, unicode paths, and with Accept-Encoding asking for gzip.
	r := e.Request(t, http.MethodGet, "/alice/tiny/resolve/"+string(sha)+"/sub/dir/%C3%BC.txt", "", "", "Accept-Encoding", "gzip")
	require.Equal(t, http.StatusOK, r.StatusCode)
	require.Empty(t, r.Header.Get("Content-Encoding"))
	require.Equal(t, "hello", string(readBody(t, r)))
}

// TestResolveNeverRendersUploads checks that files which a browser would
// render, such as HTML and SVG, download as attachments on Hangar's origin.
func TestResolveNeverRendersUploads(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	seed(t, e, "alice", modelID("alice", "xss"), model.VisibilityPublic, map[string]string{
		"page.html": "<script>alert(1)</script>", "image.svg": `<svg xmlns="http://www.w3.org/2000/svg" onload="alert(1)"/>`,
	})
	for _, f := range []string{"page.html", "image.svg"} {
		for _, method := range []string{http.MethodHead, http.MethodGet} {
			r := e.Request(t, method, "/alice/xss/resolve/main/"+f, "", "")
			require.Equal(t, http.StatusOK, r.StatusCode)
			require.Equal(t, "application/octet-stream", r.Header.Get("Content-Type"), f)
			require.Equal(t, "nosniff", r.Header.Get("X-Content-Type-Options"), f)
			require.True(t, strings.HasPrefix(r.Header.Get("Content-Disposition"), "attachment;"), f)
		}
	}
}

func TestResolveRanges(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	seed(t, e, "alice", modelID("alice", "r"), model.VisibilityPublic, map[string]string{"f.txt": "0123456789"})
	cases := []struct {
		rng, body, contentRange string
		status                  int
	}{
		{"bytes=2-5", "2345", "bytes 2-5/10", http.StatusPartialContent},
		{"bytes=7-", "789", "bytes 7-9/10", http.StatusPartialContent},
		{"bytes=-3", "789", "bytes 7-9/10", http.StatusPartialContent},
		{"bytes=8-100", "89", "bytes 8-9/10", http.StatusPartialContent},
		{"bytes=0-1,4-5", "0123456789", "", http.StatusOK},
		{"items=0-1", "0123456789", "", http.StatusOK},
		{"bytes=x-1", "0123456789", "", http.StatusOK},
		{"bytes=5-2", "0123456789", "", http.StatusOK},
		{"bytes=-x", "0123456789", "", http.StatusOK},
		{"bytes=0", "0123456789", "", http.StatusOK},
	}
	for _, c := range cases {
		r := e.Request(t, http.MethodGet, "/alice/r/resolve/main/f.txt", "", "", "Range", c.rng)
		require.Equal(t, c.status, r.StatusCode, c.rng)
		require.Equal(t, c.body, string(readBody(t, r)), c.rng)
		require.Equal(t, c.contentRange, r.Header.Get("Content-Range"), c.rng)
	}
	for _, rng := range []string{"bytes=10-", "bytes=-0", "bytes=99-100"} {
		r := e.Request(t, http.MethodGet, "/alice/r/resolve/main/f.txt", "", "", "Range", rng)
		require.Equal(t, http.StatusRequestedRangeNotSatisfiable, r.StatusCode, rng)
		require.Equal(t, "bytes */10", r.Header.Get("Content-Range"))
	}
}

func TestResolveLFSFile(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	weights := strings.Repeat("w", 1000)
	_, sha := seed(t, e, "alice", modelID("alice", "lfs"), model.VisibilityPublic, map[string]string{"model.safetensors": weights})
	r := e.Request(t, http.MethodHead, "/alice/lfs/resolve/main/model.safetensors", "", "")
	require.Equal(t, http.StatusOK, r.StatusCode)
	require.Equal(t, `"`+sha256hex(weights)+`"`, r.Header.Get("ETag"))
	require.Equal(t, `"`+sha256hex(weights)+`"`, r.Header.Get("X-Linked-Etag"))
	require.Equal(t, "1000", r.Header.Get("Content-Length"))
	require.Equal(t, string(sha), r.Header.Get("X-Repo-Commit"))
	r = e.Request(t, http.MethodGet, "/alice/lfs/resolve/main/model.safetensors", "", "", "Range", "bytes=0-9")
	require.Equal(t, http.StatusPartialContent, r.StatusCode)
	require.Equal(t, "wwwwwwwwww", string(readBody(t, r)))
}

func TestResolveRedirectMode(t *testing.T) {
	t.Parallel()
	store := blobtest.NewFake()
	store.PresignBaseURL = "https://blobs.example"
	var fake *repotest.Fake
	e := newEnv(t, func(o *hfapitest.Options) {
		fake = repotest.NewFake(t)
		fake.LFS = repotest.NewFakeLFS(store)
		o.Repos, o.LFS = fake, fake.LFS
		o.Settings = func(s *hfapi.Settings) { s.RedirectDownloads = true }
	})
	weights := strings.Repeat("x", 300)
	_, sha := seed(t, e, "alice", modelID("alice", "s3"), model.VisibilityPublic, map[string]string{"model.safetensors": weights, "config.json": configJSON})
	for _, method := range []string{http.MethodHead, http.MethodGet} {
		r := e.Request(t, method, "/alice/s3/resolve/main/model.safetensors", "", "")
		require.Equal(t, http.StatusFound, r.StatusCode)
		require.True(t, strings.HasPrefix(r.Header.Get("Location"), "https://blobs.example/"), r.Header.Get("Location"))
		require.Equal(t, string(sha), r.Header.Get("X-Repo-Commit"))
		require.Equal(t, "300", r.Header.Get("X-Linked-Size"))
		require.Equal(t, `"`+sha256hex(weights)+`"`, r.Header.Get("X-Linked-Etag"))
	}
	// Regular files are never redirected.
	r := e.Request(t, http.MethodGet, "/alice/s3/resolve/main/config.json", "", "")
	require.Equal(t, http.StatusOK, r.StatusCode)
}

func TestResolveErrors(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	_, sha := seed(t, e, "alice", modelID("alice", "pub"), model.VisibilityPublic, map[string]string{"config.json": configJSON, "dir/a.txt": "a"})
	seed(t, e, "alice", modelID("alice", "secret"), model.VisibilityPrivate, map[string]string{"config.json": configJSON})

	r := e.Request(t, http.MethodHead, "/alice/pub/resolve/main/missing.json", "", "")
	requireHubError(t, r, http.StatusNotFound, "EntryNotFound")
	require.Equal(t, string(sha), r.Header.Get("X-Repo-Commit"))
	requireHubError(t, e.Request(t, http.MethodGet, "/alice/pub/resolve/main/dir", "", ""), http.StatusNotFound, "EntryNotFound")
	requireHubError(t, e.Request(t, http.MethodGet, "/alice/pub/resolve/main/..%2Fx", "", ""), http.StatusNotFound, "EntryNotFound")
	requireHubError(t, e.Request(t, http.MethodGet, "/alice/pub/resolve/nope/config.json", "", ""), http.StatusNotFound, "RevisionNotFound")
	requireHubError(t, e.Request(t, http.MethodGet, "/alice/nothing/resolve/main/config.json", "", ""), http.StatusUnauthorized, "RepoNotFound")
	requireHubError(t, e.Request(t, http.MethodGet, "/alice/nothing/resolve/main/config.json", "bob", ""), http.StatusNotFound, "RepoNotFound")
	// Private Repos look missing to callers who cannot read them: 401 without a token, 404 with one.
	requireHubError(t, e.Request(t, http.MethodGet, "/alice/secret/resolve/main/config.json", "", ""), http.StatusUnauthorized, "RepoNotFound")
	requireHubError(t, e.Request(t, http.MethodGet, "/alice/secret/resolve/main/config.json", "bob", ""), http.StatusNotFound, "RepoNotFound")
	require.Equal(t, http.StatusOK, e.Request(t, http.MethodGet, "/alice/secret/resolve/main/config.json", "alice", "").StatusCode)
	// A bad token gets the message that stops clients reporting RepoNotFound.
	r = e.Request(t, http.MethodGet, "/alice/pub/resolve/main/config.json", "", "", "Authorization", "Bearer wrong")
	require.Equal(t, http.StatusUnauthorized, r.StatusCode)
	require.Equal(t, "Invalid credentials in Authorization header", r.Header.Get("X-Error-Message"))
	require.Empty(t, r.Header.Get("X-Error-Code"))
	// Wrong methods and unknown paths.
	require.Equal(t, http.StatusMethodNotAllowed, e.Request(t, http.MethodPost, "/alice/pub/resolve/main/config.json", "", "").StatusCode)
	require.Equal(t, http.StatusNotFound, e.Request(t, http.MethodGet, "/datasets/x/resolve/main/a", "", "").StatusCode)
}

func TestResolveGated(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	rp, _ := seed(t, e, "alice", modelID("alice", "gated"), model.VisibilityPublic, map[string]string{"config.json": configJSON})
	mode := "manual"
	_, err := e.Repos.UpdateSettings(context.Background(), rp.ID, repo.SettingsUpdate{Gating: &repo.Gating{Mode: mode}}, e.Users["alice"].Principal)
	require.NoError(t, err)
	e.Authz.AnonymousRead = true
	requireHubError(t, e.Request(t, http.MethodGet, "/alice/gated/resolve/main/config.json", "", ""), http.StatusUnauthorized, "GatedRepo")
	requireHubError(t, e.Request(t, http.MethodGet, "/alice/gated/resolve/main/config.json", "bob", ""), http.StatusForbidden, "GatedRepo")
	// Metadata stays readable.
	info := decode[map[string]any](t, e.Request(t, http.MethodGet, "/api/models/alice/gated", "bob", ""))
	require.Equal(t, "manual", info["gated"])
	e.Authz.Approve(e.Users["bob"].Principal.UserID, rp.UUID)
	require.Equal(t, http.StatusOK, e.Request(t, http.MethodGet, "/alice/gated/resolve/main/config.json", "bob", "").StatusCode)
}

func TestResolveRevisions(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	id := modelID("alice", "revs")
	seed(t, e, "alice", id, model.VisibilityPublic, map[string]string{"a.txt": "main"})
	ctx := context.Background()
	alice := e.Users["alice"].Principal
	require.NoError(t, e.Repos.CreateBranch(ctx, id, "feature/x", "main", alice))
	commitFiles(t, e, "alice", id, "feature/x", map[string]string{"a.txt": "feature"})
	require.NoError(t, e.Repos.CreateTag(ctx, repo.TagParams{Repo: id, Tag: "v1", Revision: "main", Actor: alice}))
	base, err := e.Repos.Resolve(ctx, id, "main")
	require.NoError(t, err)
	_, err = e.Repos.Commit(ctx, repo.CommitParams{Repo: id, Revision: "refs/pr/1", CreateFrom: base, Summary: "pr", Actor: alice,
		Operations: []repo.Operation{{Kind: repo.OpAdd, Path: "a.txt", Content: strings.NewReader("pr")}}})
	require.NoError(t, err)

	for rev, want := range map[string]string{
		"main": "main", "feature%2Fx": "feature", "v1": "main", "refs%2Fpr%2F1": "pr", "refs/pr/1": "pr",
	} {
		r := e.Request(t, http.MethodGet, "/alice/revs/resolve/"+rev+"/a.txt", "", "")
		require.Equal(t, http.StatusOK, r.StatusCode, rev)
		require.Equal(t, want, string(readBody(t, r)), rev)
	}
}

func TestResolveDatasetsAndDownloadCounting(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	m, _ := seed(t, e, "alice", modelID("alice", "counted"), model.VisibilityPublic, map[string]string{"config.json": configJSON, "tokenizer.json": "{}"})
	d, _ := seed(t, e, "alice", datasetID("alice", "data"), model.VisibilityPublic, map[string]string{"README.md": "# d", "data/train.csv": "a\n1\n", "data/test.csv": "a\n2\n"})
	counter := []string{"X-HF-Download-Counter", "1"}
	for _, p := range []string{"config.json", "tokenizer.json", "config.json"} {
		require.Equal(t, http.StatusOK, e.Request(t, http.MethodHead, "/alice/counted/resolve/main/"+p, "", "", counter...).StatusCode)
	}
	// A GET does not count again, nor does a HEAD without the header.
	e.Request(t, http.MethodGet, "/alice/counted/resolve/main/config.json", "", "", counter...)
	e.Request(t, http.MethodHead, "/alice/counted/resolve/main/config.json", "", "")
	n := sumDownloads(t, e, m.UUID)
	require.EqualValues(t, 2, n)

	for _, p := range []string{"README.md", "data/train.csv", "data/test.csv", "data/test.csv"} {
		r := e.Request(t, http.MethodHead, "/datasets/alice/data/resolve/main/"+p, "", "", counter...)
		require.Equal(t, http.StatusOK, r.StatusCode, p)
	}
	require.EqualValues(t, 2, sumDownloads(t, e, d.UUID), "only the first data file, data/test.csv, counts")
	info := decode[map[string]any](t, e.Request(t, http.MethodGet, "/api/datasets/alice/data", "", ""))
	require.EqualValues(t, 2, info["downloads"])
}

func sumDownloads(t *testing.T, e *hfapitest.Env, repoUUID [16]byte) int64 {
	t.Helper()
	days, err := e.Community.Downloads(context.Background(), repoUUID, time.Time{})
	require.NoError(t, err)
	var n int64
	for _, d := range days {
		n += d.Count
	}
	return n
}

func TestResolveSingleSegmentRedirect(t *testing.T) {
	t.Parallel()
	e := newEnv(t, func(o *hfapitest.Options) {
		o.Settings = func(s *hfapi.Settings) { s.DefaultNamespace = "acme" }
	})
	r := e.Request(t, http.MethodHead, "/gpt2/resolve/main/config.json?x=1", "", "")
	require.Equal(t, http.StatusTemporaryRedirect, r.StatusCode)
	require.Equal(t, "/acme/gpt2/resolve/main/config.json?x=1", r.Header.Get("Location"))
	r = e.Request(t, http.MethodGet, "/datasets/squad/resolve/main/a", "", "")
	require.Equal(t, "/datasets/acme/squad/resolve/main/a", r.Header.Get("Location"))
	r = e.Request(t, http.MethodGet, "/api/models/gpt2", "", "")
	require.Equal(t, "/api/models/acme/gpt2", r.Header.Get("Location"))
	r = e.Request(t, http.MethodGet, "/api/models/gpt2/revision/main", "", "")
	require.Equal(t, "/api/models/acme/gpt2/revision/main", r.Header.Get("Location"))
	// Without a default Namespace there is no redirect.
	e2 := newEnv(t)
	require.Equal(t, http.StatusNotFound, e2.Request(t, http.MethodGet, "/api/models/gpt2", "", "").StatusCode)
}

func TestResolveMovedRepo(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	seed(t, e, "alice", modelID("alice", "old"), model.VisibilityPublic, map[string]string{"config.json": configJSON})
	seed(t, e, "alice", modelID("alice", "hidden-old"), model.VisibilityPrivate, map[string]string{"config.json": configJSON})
	ctx := context.Background()
	_, err := e.Repos.Rename(ctx, modelID("alice", "old"), "new", e.Users["alice"].Principal)
	require.NoError(t, err)
	_, err = e.Repos.Rename(ctx, modelID("alice", "hidden-old"), "hidden-new", e.Users["alice"].Principal)
	require.NoError(t, err)

	r := e.Request(t, http.MethodHead, "/alice/old/resolve/main/config.json", "", "")
	require.Equal(t, http.StatusTemporaryRedirect, r.StatusCode)
	require.Equal(t, "/alice/new/resolve/main/config.json", r.Header.Get("Location"))
	r = e.Request(t, http.MethodPost, "/api/models/alice/old/preupload/main?create_pr=0", "alice", `{"files":[]}`)
	require.Equal(t, http.StatusTemporaryRedirect, r.StatusCode)
	require.Equal(t, "/api/models/alice/new/preupload/main?create_pr=0", r.Header.Get("Location"))
	// The new name of a private Repo stays hidden from callers who cannot read it.
	requireHubError(t, e.Request(t, http.MethodGet, "/api/models/alice/hidden-old", "bob", ""), http.StatusNotFound, "RepoNotFound")
	r = e.Request(t, http.MethodGet, "/api/models/alice/hidden-old", "alice", "")
	require.Equal(t, http.StatusTemporaryRedirect, r.StatusCode)
	require.Equal(t, "/api/models/alice/hidden-new", r.Header.Get("Location"))
}

// pullThrough is a fake hfapi.PullThrough.
type pullThrough struct {
	repoErr error
	content string
	fileErr error
	created *repo.Repo
}

func (p *pullThrough) Repo(_ context.Context, _ model.Principal, _ model.RepoID) (*repo.Repo, error) {
	return p.created, p.repoErr
}

func (p *pullThrough) File(_ context.Context, _ model.Principal, _ *repo.Repo, _ model.CommitSHA, _ model.Entry) (io.ReadCloser, error) {
	if p.fileErr != nil {
		return nil, p.fileErr
	}
	return io.NopCloser(strings.NewReader(p.content)), nil
}

func TestResolvePartialMirrorAndPullThrough(t *testing.T) {
	t.Parallel()
	pt := &pullThrough{repoErr: hfapi.ErrPullPending, fileErr: repo.ErrNotPresent}
	e := newEnv(t, func(o *hfapitest.Options) {
		o.Deps = func(d *hfapi.Deps) { d.PullThrough = pt }
	})
	weights := strings.Repeat("q", 64)
	rp, _ := seed(t, e, "alice", modelID("alice", "mirror"), model.VisibilityPublic, map[string]string{"model.gguf": weights})
	require.NoError(t, e.Repos.SetLFSPresence(context.Background(), rp.ID, []repo.LFSRef{{OID: sha256hex(weights), Size: 64, Present: false}}))

	r := e.Request(t, http.MethodHead, "/alice/mirror/resolve/main/model.gguf", "", "")
	require.Equal(t, http.StatusOK, r.StatusCode)
	require.Equal(t, "64", r.Header.Get("Content-Length"))
	r = e.Request(t, http.MethodGet, "/alice/mirror/resolve/main/model.gguf", "", "")
	require.Equal(t, http.StatusConflict, r.StatusCode)
	require.Contains(t, r.Header.Get("X-Error-Message"), "not in the arsenal")

	// A policy refusal, such as the Namespace's storage quota, is a 403
	// with the message.
	pt.fileErr = &repo.PolicyError{Message: "This change needs 64 B more than the storage quota allows."}
	r = e.Request(t, http.MethodGet, "/alice/mirror/resolve/main/model.gguf", "", "")
	require.Equal(t, http.StatusForbidden, r.StatusCode)
	require.Contains(t, r.Header.Get("X-Error-Message"), "storage quota")

	pt.fileErr, pt.content = nil, weights
	r = e.Request(t, http.MethodGet, "/alice/mirror/resolve/main/model.gguf", "", "")
	require.Equal(t, http.StatusOK, r.StatusCode)
	require.Equal(t, weights, string(readBody(t, r)))

	// A missing Repo asks Pull-through, which may still be planning.
	r = e.Request(t, http.MethodGet, "/api/models/upstream/thing", "", "")
	require.Equal(t, http.StatusServiceUnavailable, r.StatusCode)
	require.Equal(t, "10", r.Header.Get("Retry-After"))
	pt.repoErr = &repo.PolicyError{Message: "The Namespace is over its storage quota."}
	r = e.Request(t, http.MethodGet, "/api/models/upstream/thing", "bob", "")
	require.Equal(t, http.StatusForbidden, r.StatusCode)
	require.Contains(t, r.Header.Get("X-Error-Message"), "storage quota")
	pt.repoErr = repo.ErrNotFound
	requireHubError(t, e.Request(t, http.MethodGet, "/api/models/upstream/thing", "bob", ""), http.StatusNotFound, "RepoNotFound")
	pt.repoErr, pt.created = nil, rp
	require.Equal(t, http.StatusOK, e.Request(t, http.MethodGet, "/api/models/alice/mirror", "", "").StatusCode)
}
