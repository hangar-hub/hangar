package ollama_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/auth"
	"github.com/hangar-hub/hangar/internal/auth/authtest"
	"github.com/hangar-hub/hangar/internal/community/communitytest"
	"github.com/hangar-hub/hangar/internal/db/dbtest"
	"github.com/hangar-hub/hangar/internal/httpx"
	"github.com/hangar-hub/hangar/internal/index/gguf"
	"github.com/hangar-hub/hangar/internal/index/indextest"
	"github.com/hangar-hub/hangar/internal/model"
	"github.com/hangar-hub/hangar/internal/ollama"
	"github.com/hangar-hub/hangar/internal/repo"
	"github.com/hangar-hub/hangar/internal/repo/repotest"
)

const mit = "MIT License\n\nPermission is hereby granted, free of charge, to any person obtaining a copy of this software.\n"

// recordingAuth wraps the fake OllamaAuth and remembers what it was asked.
type recordingAuth struct {
	*authtest.OllamaAuth
	mu   sync.Mutex
	reqs []auth.OllamaTokenRequest
}

func (a *recordingAuth) IssueRegistryToken(ctx context.Context, req auth.OllamaTokenRequest) (string, time.Time, error) {
	a.mu.Lock()
	a.reqs = append(a.reqs, req)
	a.mu.Unlock()
	return a.OllamaAuth.IssueRegistryToken(ctx, req)
}

type pullThrough struct {
	paths []string
	err   error
}

func (p *pullThrough) PullFiles(_ context.Context, _ *repo.Repo, _ model.CommitSHA, paths []string) error {
	p.paths = append(p.paths, paths...)
	return p.err
}

type env struct {
	srv       *httptest.Server
	repos     *repotest.Fake
	auth      *recordingAuth
	authz     *authtest.Authz
	downloads *communitytest.Fake
	pull      *pullThrough
	public    *repo.Repo
	alice     model.Principal
}

func newEnv(t *testing.T, withPullThrough bool, configure ...func(*ollama.Options)) *env {
	t.Helper()
	d := dbtest.SQLite(t)
	e := &env{repos: repotest.NewFake(t), authz: authtest.NewAuthz(), downloads: communitytest.NewFake(), pull: &pullThrough{}}
	e.authz.AnonymousRead = true
	authn := authtest.NewAuthenticator()
	e.auth = &recordingAuth{OllamaAuth: authtest.NewOllamaAuth(authn)}
	e.alice = indextest.Owner("alice")
	e.auth.AddKey("alice-key:signature", e.alice)
	signer, err := httpx.NewSigner([]byte("0123456789abcdef0123456789abcdef"))
	require.NoError(t, err)
	o := ollama.Options{DB: d, Repos: e.repos, LFS: e.repos.LFS, Authz: e.authz, OllamaAuth: e.auth, Signer: signer,
		PublicURL: "http://hangar.test", Community: e.downloads, Window: 64 << 10}
	if withPullThrough {
		o.PullThrough = e.pull
	}
	for _, c := range configure {
		c(&o)
	}
	h, err := ollama.New(o)
	require.NoError(t, err)
	_, err = ollama.New(ollama.Options{})
	require.Error(t, err)
	e.srv = httptest.NewServer(httpx.RequestIDs(httpx.Authenticate(ollama.ExemptTokenEndpoint(authn), nil)(httpx.Dispatch(httpx.Protocols{Ollama: h}, "http://hangar.test"))))
	t.Cleanup(e.srv.Close)

	id := model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "Tiny-GGUF"}
	e.public = indextest.CreateRepo(t, e.repos, id, model.VisibilityPublic)
	sha := indextest.Commit(t, e.repos, id, map[string][]byte{
		"tiny-Q4_K_M.gguf":          indextest.LlamaGGUF(t, 15, "Q4_K"),
		"tiny-Q8_0.gguf":            indextest.LlamaGGUF(t, 7, "Q8_0"),
		"tiny-F16.gguf":             indextest.LlamaGGUF(t, 1, "F16"),
		"mmproj-tiny-f16.gguf":      indextest.GGUF(t, []gguf.KV{{Key: "general.architecture", Value: "clip"}}, gguf.EncodeTensor{Name: "v", Type: "F16", Dims: []uint64{8}}),
		"split-00001-of-00002.gguf": indextest.LlamaGGUF(t, 1, "F16"),
		"split-00002-of-00002.gguf": indextest.LlamaGGUF(t, 1, "F16"),
		"LICENSE":                   []byte(mit),
		"system":                    []byte("You are terse."),
	})
	f16, err := e.repos.Entry(context.Background(), id, sha, "tiny-F16.gguf")
	require.NoError(t, err)
	require.NoError(t, e.repos.SetLFSPresence(context.Background(), id, []repo.LFSRef{{OID: f16.LFS.OID, Size: f16.Size, Present: false}}))
	return e
}

func (e *env) get(t *testing.T, method, path string, header map[string]string) *http.Response {
	t.Helper()
	u := path
	if !strings.HasPrefix(path, "http") {
		u = e.srv.URL + path
	}
	req, err := http.NewRequest(method, u, nil)
	require.NoError(t, err)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func body(t *testing.T, r *http.Response) []byte {
	t.Helper()
	b, err := io.ReadAll(r.Body)
	require.NoError(t, err)
	return b
}

func errCode(t *testing.T, r *http.Response) string {
	t.Helper()
	var out struct {
		Errors []struct{ Code, Message string } `json:"errors"`
	}
	require.NoError(t, json.Unmarshal(body(t, r), &out))
	require.Len(t, out.Errors, 1)
	return out.Errors[0].Code + ": " + out.Errors[0].Message
}

func (e *env) manifest(t *testing.T, tag string, header map[string]string) ollama.Manifest {
	t.Helper()
	resp := e.get(t, "GET", "/v2/alice/tiny-gguf/manifests/"+tag, header)
	b := body(t, resp)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(b))
	require.Equal(t, ollama.MediaTypeManifest, resp.Header.Get("Content-Type"))
	sum := sha256.Sum256(b)
	require.Equal(t, "sha256:"+hex.EncodeToString(sum[:]), resp.Header.Get("Docker-Content-Digest"))
	var m ollama.Manifest
	require.NoError(t, json.Unmarshal(b, &m))
	return m
}

func layer(m ollama.Manifest, mediaType string) *ollama.Layer {
	for i := range m.Layers {
		if m.Layers[i].MediaType == mediaType {
			return &m.Layers[i]
		}
	}
	return nil
}

func TestManifestForQuantsAndLatest(t *testing.T) {
	e := newEnv(t, false)
	ctx := context.Background()
	sha, err := e.repos.Resolve(ctx, e.public.ID, "main")
	require.NoError(t, err)
	entry := func(p string) model.Entry {
		en, err := e.repos.Entry(ctx, e.public.ID, sha, p)
		require.NoError(t, err)
		return en
	}

	m := e.manifest(t, "latest", nil)
	require.Equal(t, "sha256:"+entry("tiny-Q4_K_M.gguf").LFS.OID, layer(m, ollama.MediaTypeModel).Digest, "latest is Q4_K_M")
	require.Equal(t, "sha256:"+entry("mmproj-tiny-f16.gguf").LFS.OID, layer(m, ollama.MediaTypeProjector).Digest)
	require.NotNil(t, layer(m, ollama.MediaTypeTemplate), "the llama3 chat template matches an Ollama template")
	require.NotNil(t, layer(m, ollama.MediaTypeParams), "the matched template's stop tokens become params")
	require.NotNil(t, layer(m, ollama.MediaTypeSystem))
	require.NotNil(t, layer(m, ollama.MediaTypeLicense))
	require.Equal(t, "q8_0 picked by name", func() string {
		m8 := e.manifest(t, "q8_0", nil)
		if layer(m8, ollama.MediaTypeModel).Digest == "sha256:"+entry("tiny-Q8_0.gguf").LFS.OID {
			return "q8_0 picked by name"
		}
		return "wrong file"
	}())
	byFile := e.manifest(t, "tiny-Q8_0.gguf", nil)
	require.Equal(t, "sha256:"+entry("tiny-Q8_0.gguf").LFS.OID, layer(byFile, ollama.MediaTypeModel).Digest)

	// The config blob is deterministic and served from ollama_blobs.
	again := e.manifest(t, "latest", nil)
	require.Equal(t, m.Config.Digest, again.Config.Digest)
	resp := e.get(t, "GET", "/v2/alice/tiny-gguf/blobs/"+m.Config.Digest, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var cfg ollama.Config
	require.NoError(t, json.Unmarshal(body(t, resp), &cfg))
	require.Equal(t, "gguf", cfg.ModelFormat)
	require.Equal(t, "llama", cfg.ModelFamily)
	require.Equal(t, "Q4_K_M", cfg.FileType)
	require.Equal(t, "512", cfg.ModelType)
	require.Len(t, cfg.RootFS.DiffIDs, len(m.Layers))

	// Template layer content is Ollama's llama3-instruct template.
	tmpl := e.get(t, "GET", "/v2/alice/tiny-gguf/blobs/"+layer(m, ollama.MediaTypeTemplate).Digest, nil)
	require.Contains(t, string(body(t, tmpl)), "<|start_header_id|>")

	days, err := e.downloads.Downloads(ctx, e.public.UUID, time.Now().Add(-time.Hour))
	require.NoError(t, err)
	require.Equal(t, int64(4), days[0].Count, "each manifest GET counts a download")
}

func TestManifestErrors(t *testing.T) {
	e := newEnv(t, false)
	resp := e.get(t, "GET", "/v2/alice/tiny-gguf/manifests/Q2_K", nil)
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
	require.Contains(t, errCode(t, resp), "MANIFEST_UNKNOWN")

	resp = e.get(t, "GET", "/v2/alice/tiny-gguf/manifests/F16", nil)
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
	msg := errCode(t, resp)
	require.Contains(t, msg, "MANIFEST_UNKNOWN")
	require.Contains(t, msg, "hangar pull alice/Tiny-GGUF:F16")

	resp = e.get(t, "GET", "/v2/alice/tiny-gguf/manifests/split", nil)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	require.Contains(t, errCode(t, resp), "cannot pull split GGUF models")

	for _, m := range []string{"PUT", "POST", "PATCH", "DELETE"} {
		resp = e.get(t, m, "/v2/alice/tiny-gguf/manifests/latest", nil)
		require.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode, m)
		require.Contains(t, errCode(t, resp), "hangar import --from-ollama")
	}
	resp = e.get(t, "POST", "/v2/alice/tiny-gguf/blobs/uploads/", nil)
	require.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)

	resp = e.get(t, "GET", "/v2/", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp = e.get(t, "GET", "/v2/alice/tiny-gguf/tags/list", nil)
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
	resp = e.get(t, "GET", "/v2/alice/tiny-gguf/blobs/sha256:nothex", nil)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	resp = e.get(t, "GET", "/v2/alice/tiny-gguf/blobs/sha256:"+strings.Repeat("a", 64), nil)
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
	require.Contains(t, errCode(t, resp), "BLOB_UNKNOWN")
}

func TestPullThroughStartsAPull(t *testing.T) {
	e := newEnv(t, true)
	resp := e.get(t, "GET", "/v2/alice/tiny-gguf/manifests/F16", nil)
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	require.Equal(t, "30", resp.Header.Get("Retry-After"))
	require.Equal(t, []string{"tiny-F16.gguf"}, e.pull.paths, "only files that are not present are pulled")
	e.pull.err = errors.New("upstream unavailable")
	resp = e.get(t, "GET", "/v2/alice/tiny-gguf/manifests/F16", nil)
	require.Equal(t, http.StatusNotFound, resp.StatusCode, "a failed Pull-through falls back to the not-in-arsenal answer")
}

func TestBlobsStreamWithSignedRanges(t *testing.T) {
	e := newEnv(t, false)
	m := e.manifest(t, "Q8_0", nil)
	model := layer(m, ollama.MediaTypeModel)

	head := e.get(t, "HEAD", "/v2/alice/tiny-gguf/blobs/"+model.Digest, nil)
	require.Equal(t, http.StatusOK, head.StatusCode)
	require.Equal(t, fmt.Sprint(model.Size), head.Header.Get("Content-Length"))
	require.Equal(t, model.Digest, head.Header.Get("Docker-Content-Digest"))
	require.Equal(t, "nosniff", head.Header.Get("X-Content-Type-Options"), "a browser never renders a blob")
	require.Equal(t, "application/octet-stream", head.Header.Get("Content-Type"))

	// Ollama reads the direct URL from Location and downloads parts from
	// it without an Authorization header.
	full := e.get(t, "GET", "/v2/alice/tiny-gguf/blobs/"+model.Digest, nil)
	require.Equal(t, http.StatusOK, full.StatusCode)
	content := body(t, full)
	sum := sha256.Sum256(content)
	require.Equal(t, model.Digest, "sha256:"+hex.EncodeToString(sum[:]))
	loc, err := full.Location()
	require.NoError(t, err)
	require.Equal(t, strings.TrimPrefix(e.srv.URL, "http://"), loc.Host, "the direct URL stays on the request host")

	part := e.get(t, "GET", loc.String(), map[string]string{"Range": "bytes=10-19"})
	require.Equal(t, http.StatusPartialContent, part.StatusCode)
	require.Equal(t, content[10:20], body(t, part))
	require.Equal(t, fmt.Sprintf("bytes 10-19/%d", len(content)), part.Header.Get("Content-Range"))
	suffix := e.get(t, "GET", loc.String(), map[string]string{"Range": "bytes=-5"})
	require.Equal(t, content[len(content)-5:], body(t, suffix))
	open := e.get(t, "GET", loc.String(), map[string]string{"Range": "bytes=5-"})
	require.Equal(t, content[5:], body(t, open))
	bad := e.get(t, "GET", loc.String(), map[string]string{"Range": "bytes=999999-"})
	require.Equal(t, http.StatusRequestedRangeNotSatisfiable, bad.StatusCode)
	bad = e.get(t, "GET", loc.String(), map[string]string{"Range": "bytes=1-2,4-5"})
	require.Equal(t, http.StatusRequestedRangeNotSatisfiable, bad.StatusCode)

	// A signature for another blob or a tampered one is refused.
	q := loc.Query()
	q.Set("scope", "ollama-blob:alice/tiny-gguf:"+m.Config.Digest)
	loc.RawQuery = q.Encode()
	refused := e.get(t, "GET", loc.String(), nil)
	require.Equal(t, http.StatusUnauthorized, refused.StatusCode)

	// Small layers come from ollama_blobs, with ranges too.
	lic := layer(m, ollama.MediaTypeLicense)
	r := e.get(t, "GET", "/v2/alice/tiny-gguf/blobs/"+lic.Digest, map[string]string{"Range": "bytes=0-2"})
	require.Equal(t, http.StatusPartialContent, r.StatusCode)
	require.Equal(t, "MIT", string(body(t, r)))
}

// presigningLFS is an LFS store whose Blob store presigns downloads, as S3
// does.
type presigningLFS struct {
	repo.LFSStore
	mu  sync.Mutex
	ttl time.Duration
}

func (p *presigningLFS) PresignGet(_ context.Context, oid string, ttl time.Duration, _ string) (string, error) {
	p.mu.Lock()
	p.ttl = ttl
	p.mu.Unlock()
	return "https://bucket.example.test/lfs/" + oid + "?X-Amz-Signature=x", nil
}

// TestBlobsRedirectToTheBucket: with downloads in S3 redirect mode, a GET
// of a layer stored as an LFS object answers 307 to a presigned URL, so
// ollama pull takes the bytes straight from the bucket. HEAD, which Ollama
// reads the size from, and small layers kept in the database are served
// by Hangar.
func TestBlobsRedirectToTheBucket(t *testing.T) {
	var lfs *presigningLFS
	e := newEnv(t, false, func(o *ollama.Options) {
		lfs = &presigningLFS{LFSStore: o.LFS}
		o.LFS, o.RedirectDownloads, o.PresignTTL = lfs, true, 2*time.Hour
	})
	m := e.manifest(t, "Q8_0", nil)
	model := layer(m, ollama.MediaTypeModel)
	oid := strings.TrimPrefix(model.Digest, "sha256:")

	resp := e.get(t, "GET", "/v2/alice/tiny-gguf/blobs/"+model.Digest, nil)
	require.Equal(t, http.StatusTemporaryRedirect, resp.StatusCode)
	require.Equal(t, "https://bucket.example.test/lfs/"+oid+"?X-Amz-Signature=x", resp.Header.Get("Location"))
	require.Equal(t, model.Digest, resp.Header.Get("Docker-Content-Digest"))
	require.Empty(t, body(t, resp))
	require.Equal(t, 2*time.Hour, lfs.ttl)

	head := e.get(t, "HEAD", "/v2/alice/tiny-gguf/blobs/"+model.Digest, nil)
	require.Equal(t, http.StatusOK, head.StatusCode, "HEAD is answered here: a presigned GET URL does not answer HEAD")
	require.Equal(t, fmt.Sprint(model.Size), head.Header.Get("Content-Length"))

	lic := layer(m, ollama.MediaTypeLicense)
	r := e.get(t, "GET", "/v2/alice/tiny-gguf/blobs/"+lic.Digest, nil)
	require.Equal(t, http.StatusOK, r.StatusCode, "layers kept in the database are served")
	require.Equal(t, mit, string(body(t, r)))

	// A Blob store that cannot presign (the filesystem) serves the bytes.
	e = newEnv(t, false, func(o *ollama.Options) { o.RedirectDownloads = true })
	m = e.manifest(t, "Q8_0", nil)
	resp = e.get(t, "GET", "/v2/alice/tiny-gguf/blobs/"+layer(m, ollama.MediaTypeModel).Digest, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestPrivateReposUseTheOllamaKeyFlow(t *testing.T) {
	e := newEnv(t, false)
	id := model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "secret"}
	indextest.CreateRepo(t, e.repos, id, model.VisibilityPrivate)
	indextest.Commit(t, e.repos, id, map[string][]byte{"secret-Q4_0.gguf": indextest.LlamaGGUF(t, 2, "Q4_0")})

	resp := e.get(t, "GET", "/v2/alice/secret/manifests/latest", nil)
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	challenge := resp.Header.Get("WWW-Authenticate")
	host := strings.TrimPrefix(e.srv.URL, "http://")
	require.Equal(t, fmt.Sprintf(`Bearer realm="http://%s/v2/token",service="%s",scope="repository:alice/secret:pull"`, host, host), challenge)
	missing := e.get(t, "GET", "/v2/alice/nothing/manifests/latest", nil)
	require.Equal(t, http.StatusUnauthorized, missing.StatusCode, "missing and private Repos look the same to anonymous callers")

	// Ollama calls the realm with service, scope, ts and nonce, signed.
	v := url.Values{"service": {host}, "scope": {"repository:alice/secret:pull"}, "ts": {"1760000000"}, "nonce": {"abc"}}
	realm := e.srv.URL + "/v2/token?" + v.Encode()
	tok := e.get(t, "GET", realm, map[string]string{"Authorization": "alice-key:signature"})
	b := body(t, tok)
	require.Equal(t, http.StatusOK, tok.StatusCode, string(b))
	var out struct {
		Token string `json:"token"`
	}
	require.NoError(t, json.Unmarshal(b, &out))
	require.NotEmpty(t, out.Token)
	require.Len(t, e.auth.reqs, 1)
	require.Equal(t, realm, e.auth.reqs[0].URL, "the URL OllamaAuth verifies is exactly the signed realm URL")
	require.Equal(t, "alice/secret", e.auth.reqs[0].Repo.String())

	m := e.get(t, "GET", "/v2/alice/secret/manifests/latest", map[string]string{"Authorization": "Bearer " + out.Token})
	require.Equal(t, http.StatusOK, m.StatusCode)

	bad := e.get(t, "GET", realm, map[string]string{"Authorization": "unknown:sig"})
	require.Equal(t, http.StatusUnauthorized, bad.StatusCode)
	none := e.get(t, "GET", realm, nil)
	require.Equal(t, http.StatusUnauthorized, none.StatusCode)
	wrongScope := e.get(t, "GET", e.srv.URL+"/v2/token?scope=repository:alice/secret:push", map[string]string{"Authorization": "alice-key:signature"})
	require.Equal(t, http.StatusBadRequest, wrongScope.StatusCode)
	badName := e.get(t, "GET", e.srv.URL+"/v2/token?scope=repository:nope:pull", map[string]string{"Authorization": "alice-key:signature"})
	require.Equal(t, http.StatusBadRequest, badName.StatusCode)
	post := e.get(t, "POST", realm, nil)
	require.Equal(t, http.StatusMethodNotAllowed, post.StatusCode)

	// A signed-in user without access gets 404, not a challenge.
	bobToken := "bob-token"
	e.auth.Authenticator.Add(bobToken, indextest.Owner("bob"))
	hidden := e.get(t, "GET", "/v2/alice/secret/manifests/latest", map[string]string{"Authorization": "Bearer " + bobToken})
	require.Equal(t, http.StatusNotFound, hidden.StatusCode)
	require.Contains(t, errCode(t, hidden), "NAME_UNKNOWN")
}

// TestGeneratedLayersStayWithTheirRepo covers security review finding 12:
// generated layers were served by digest through any readable Repo, so a
// user who once read a private Repo's manifest could keep reading its
// layers, such as a GGUF file stored in git, through a public Repo after
// losing access. A generated layer is now served only through a Repo it
// was built from.
func TestGeneratedLayersStayWithTheirRepo(t *testing.T) {
	e := newEnv(t, false)
	id := model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "secret"}
	indextest.CreateRepo(t, e.repos, id, model.VisibilityPrivate)
	indextest.Commit(t, e.repos, id, map[string][]byte{
		"secret-Q4_0.gguf": indextest.LlamaGGUF(t, 2, "Q4_0"),
		"LICENSE":          []byte("Secret licence: do not share."),
	})
	const token = "alice-token"
	e.auth.Authenticator.Add(token, e.alice)
	aliceAuth := map[string]string{"Authorization": "Bearer " + token}
	resp := e.get(t, "GET", "/v2/alice/secret/manifests/latest", aliceAuth)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var m ollama.Manifest
	require.NoError(t, json.Unmarshal(body(t, resp), &m))
	lic := layer(m, ollama.MediaTypeLicense)
	require.NotNil(t, lic)

	own := e.get(t, "GET", "/v2/alice/secret/blobs/"+lic.Digest, aliceAuth)
	require.Equal(t, http.StatusOK, own.StatusCode)
	require.Equal(t, "Secret licence: do not share.", string(body(t, own)))

	for _, digest := range []string{lic.Digest, m.Config.Digest} {
		through := e.get(t, "GET", "/v2/alice/tiny-gguf/blobs/"+digest, nil)
		require.Equal(t, http.StatusNotFound, through.StatusCode, "a private Repo's layer is not served through a public Repo")
		require.Contains(t, errCode(t, through), "BLOB_UNKNOWN")
	}

	// The public Repo's own generated layers still work.
	pub := e.manifest(t, "Q8_0", nil)
	r := e.get(t, "GET", "/v2/alice/tiny-gguf/blobs/"+layer(pub, ollama.MediaTypeLicense).Digest, nil)
	require.Equal(t, http.StatusOK, r.StatusCode)
}

func TestMatchTemplateAndHumanNumber(t *testing.T) {
	tmpl, ok, err := ollama.MatchTemplate(indextest.Llama3Template)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "llama3-instruct", tmpl.Name)
	require.Contains(t, string(tmpl.Params), "<|eot_id|>")
	_, ok, err = ollama.MatchTemplate("")
	require.NoError(t, err)
	require.False(t, ok)
	_, ok, err = ollama.MatchTemplate(strings.Repeat("nothing like a chat template ", 20))
	require.NoError(t, err)
	require.False(t, ok)
	near, ok, err := ollama.MatchTemplate(strings.Replace(indextest.Llama3Template, "bos_token", "bos_tok", 1))
	require.NoError(t, err)
	require.True(t, ok, "small edits still match")
	require.Equal(t, "llama3-instruct", near.Name)

	for n, want := range map[uint64]string{12: "12", 1500: "2K", 135_000_000: "135M", 1_230_000: "1.23M", 3_210_000_000: "3.2B", 8_000_000_000: "8B"} {
		require.Equal(t, want, ollama.HumanNumber(n), n)
	}
}
