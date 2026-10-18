package importer_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/api"
	"github.com/hangar-hub/hangar/internal/api/gen"
	"github.com/hangar-hub/hangar/internal/auth/authtest"
	"github.com/hangar-hub/hangar/internal/blob"
	"github.com/hangar-hub/hangar/internal/db"
	"github.com/hangar-hub/hangar/internal/db/dbtest"
	"github.com/hangar-hub/hangar/internal/httpx"
	"github.com/hangar-hub/hangar/internal/jobs"
	"github.com/hangar-hub/hangar/internal/model"
	"github.com/hangar-hub/hangar/internal/repo"
	"github.com/hangar-hub/hangar/internal/upstream"
	"github.com/hangar-hub/hangar/internal/upstream/importer"
	"github.com/hangar-hub/hangar/internal/upstream/upstreamtest"
)

// fakeTarget is an importer.Target over a Harness, running Jobs inline.
type fakeTarget struct {
	h    *upstreamtest.Harness
	user model.Principal
}

func (f *fakeTarget) runPull(id jobs.JobID) error {
	f.h.Run(20)
	j := f.h.Job(id)
	if j.Status == jobs.StatusSucceeded {
		return nil
	}
	var st map[string]any
	_ = json.Unmarshal(j.Result, &st)
	if st["reason"] == "upstream_not_found" {
		return fmt.Errorf("%w: %s", importer.ErrUpstreamMissing, j.Error)
	}
	return errors.New(j.Error)
}

func (f *fakeTarget) PullHistory(ctx context.Context, up string, id model.RepoID, revision string) error {
	jid, _, err := f.h.Service.StartPull(ctx, upstream.PullRequest{Upstream: up, Repo: id, Revision: revision, Selection: upstream.Selection{None: true}, Actor: f.user})
	if err != nil {
		return err
	}
	return f.runPull(jid)
}

func (f *fakeTarget) PullFiles(ctx context.Context, id model.RepoID, paths []string) error {
	r, err := f.h.Repos.Get(ctx, id)
	if err != nil {
		return err
	}
	jid, err := f.h.Service.PullMore(ctx, f.user, r, upstream.Selection{Paths: paths}, false)
	if err != nil {
		return err
	}
	return f.runPull(jid)
}

func (f *fakeTarget) Tree(ctx context.Context, id model.RepoID, revision string) ([]model.Entry, error) {
	sha, err := f.h.Repos.Resolve(ctx, id, revision)
	if err != nil {
		return nil, err
	}
	return f.h.Repos.Tree(ctx, id, sha, "", repo.TreeOpts{Recursive: true})
}

func (f *fakeTarget) Store(ctx context.Context, _ model.RepoID, file importer.File) error {
	src, err := os.Open(file.Local)
	if err != nil {
		return err
	}
	defer src.Close()
	_, err = f.h.LFS.Ingest(ctx, file.SHA256, file.Size, src)
	return err
}

func (f *fakeTarget) CreateRepo(ctx context.Context, id model.RepoID) error {
	actor := f.user
	if ns, err := f.h.Orgs.Get(ctx, id.Namespace); err == nil {
		actor.NamespaceID = ns.ID
	} else {
		actor.NamespaceID = uuid.New()
	}
	_, err := f.h.Repos.Create(ctx, repo.CreateParams{ID: id, Actor: actor})
	if errors.Is(err, repo.ErrExists) {
		return nil
	}
	return err
}

func (f *fakeTarget) Commit(ctx context.Context, id model.RepoID, summary string, files []importer.File) error {
	var ops []repo.Operation
	for _, file := range files {
		if file.Size < 64 {
			b, err := os.ReadFile(file.Local)
			if err != nil {
				return err
			}
			ops = append(ops, repo.Operation{Kind: repo.OpAdd, Path: file.Path, Content: bytes.NewReader(b)})
			continue
		}
		if err := f.Store(ctx, id, file); err != nil {
			return err
		}
		ops = append(ops, repo.Operation{Kind: repo.OpAddLFS, Path: file.Path, OID: file.SHA256, Size: file.Size})
	}
	_, err := f.h.Repos.Commit(ctx, repo.CommitParams{Repo: id, Summary: summary, Operations: ops, Actor: f.user})
	return err
}

func sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func write(t *testing.T, path string, b []byte) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, b, 0o644))
}

// hfCache writes a cache entry the way huggingface_hub does: blobs named
// by ETag, snapshot files as relative symlinks, refs/main.
func hfCache(t *testing.T, root, kind string, id model.RepoID, commit model.CommitSHA, files map[string][]byte, lfs map[string]bool) {
	t.Helper()
	dir := filepath.Join(root, kind+"--"+id.Namespace+"--"+id.Name)
	write(t, filepath.Join(dir, "refs", "main"), []byte(commit))
	for p, c := range files {
		etag := sum(c)
		if !lfs[p] {
			etag = model.GitBlobID(c)
		}
		write(t, filepath.Join(dir, "blobs", etag), c)
		link := filepath.Join(dir, "snapshots", string(commit), p)
		require.NoError(t, os.MkdirAll(filepath.Dir(link), 0o755))
		rel, err := filepath.Rel(filepath.Dir(link), filepath.Join(dir, "blobs", etag))
		require.NoError(t, err)
		require.NoError(t, os.Symlink(rel, link))
	}
}

type world struct {
	h       *upstreamtest.Harness
	alice   model.Principal
	tiny    model.RepoID
	commit  model.CommitSHA
	weights []byte
	q4, q8  []byte
	gguf    model.RepoID
}

func newWorld(t *testing.T, d *db.DB) *world {
	w := &world{h: upstreamtest.NewHarness(t, d, upstreamtest.HarnessOptions{})}
	w.alice = w.h.User("alice")
	w.tiny = model.RepoID{Type: model.RepoTypeModel, Namespace: "acme", Name: "tiny"}
	w.weights = bytes.Repeat([]byte("cached weights "), 100)
	w.commit = w.h.Seed(w.tiny, map[string][]byte{"config.json": []byte(`{"a":1}`)}, map[string][]byte{"model.safetensors": w.weights, "other.bin": []byte("not cached")})
	w.gguf = model.RepoID{Type: model.RepoTypeModel, Namespace: "bartowski", Name: "Tiny-GGUF"}
	w.q4, w.q8 = []byte("q4 gguf bytes"), []byte("q8 gguf bytes")
	w.h.Seed(w.gguf, map[string][]byte{"README.md": []byte("# tiny")}, map[string][]byte{"Tiny-Q4_K_M.gguf": w.q4, "Tiny-Q8_0.gguf": w.q8})
	return w
}

func (w *world) check(t *testing.T, id model.RepoID, path string, want []byte) {
	t.Helper()
	sha, err := w.h.Repos.Resolve(w.h.Ctx, id, "")
	require.NoError(t, err)
	rc, _, err := w.h.Repos.OpenFile(w.h.Ctx, id, sha, path, nil)
	require.NoError(t, err, path)
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	require.Equal(t, want, got)
}

func (w *world) present(t *testing.T, id model.RepoID, path string) bool {
	t.Helper()
	sha, err := w.h.Repos.Resolve(w.h.Ctx, id, "")
	require.NoError(t, err)
	e, err := w.h.Repos.Entry(w.h.Ctx, id, sha, path)
	require.NoError(t, err)
	return e.Present
}

// importAll runs every kind of import through target and checks the
// Arsenal. No LFS object is downloaded from the Upstream.
func importAll(t *testing.T, w *world, target importer.Target) {
	ctx := context.Background()
	root := t.TempDir()

	// A Hugging Face cache holding config.json and model.safetensors of acme/tiny.
	cache := filepath.Join(root, "hub")
	hfCache(t, cache, "models", w.tiny, w.commit, map[string][]byte{"config.json": []byte(`{"a":1}`), "model.safetensors": w.weights},
		map[string]bool{"model.safetensors": true})
	// A cached Repo the Upstream does not have.
	hfCache(t, cache, "models", model.RepoID{Type: model.RepoTypeModel, Namespace: "gone", Name: "away"}, model.CommitSHA(strings.Repeat("a", 40)),
		map[string][]byte{"x.bin": []byte("x")}, map[string]bool{"x.bin": true})
	var lines []string
	im := importer.New(importer.Options{Target: target, Progress: func(s string) { lines = append(lines, s) }})
	cdn := w.h.Server.Requests("cdn")
	rep, err := im.FromHFCache(ctx, cache)
	require.NoError(t, err)
	require.Len(t, rep.Results, 2)
	require.Equal(t, 1, rep.Failed(), "%+v", rep.Results)
	byRepo := map[string]importer.Result{}
	for _, r := range rep.Results {
		byRepo[r.Repo.String()] = r
	}
	tinyRes := byRepo["acme/tiny"]
	require.NoError(t, tinyRes.Err)
	require.Equal(t, "mirror", tinyRes.Kind)
	require.Equal(t, 1, tinyRes.Files)
	require.ErrorIs(t, byRepo["gone/away"].Err, importer.ErrUpstreamMissing)
	head, err := w.h.Repos.Resolve(ctx, w.tiny, "main")
	require.NoError(t, err)
	require.Equal(t, w.commit, head, "the import plans a Mirror with the upstream Commit")
	require.True(t, w.present(t, w.tiny, "model.safetensors"))
	require.False(t, w.present(t, w.tiny, "other.bin"), "files not in the cache stay not in the arsenal")
	w.check(t, w.tiny, "model.safetensors", w.weights)
	require.Equal(t, cdn, w.h.Server.Requests("cdn"), "nothing is downloaded from the Upstream")
	require.NotEmpty(t, lines)

	// Offline, the cache becomes local Repos with one Commit.
	offline := filepath.Join(root, "offline")
	local := model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "cached"}
	hfCache(t, offline, "models", local, model.CommitSHA(strings.Repeat("b", 40)), map[string][]byte{"config.json": []byte(`{}`), "w.bin": w.weights},
		map[string]bool{"w.bin": true})
	rep, err = importer.New(importer.Options{Target: target, Offline: true}).FromHFCache(ctx, offline)
	require.NoError(t, err)
	require.Len(t, rep.Results, 1)
	require.NoError(t, rep.Results[0].Err)
	require.Equal(t, "local", rep.Results[0].Kind)
	w.check(t, local, "w.bin", w.weights)

	// LM Studio: bartowski/Tiny-GGUF holds the Q4 quant under the upstream
	// name; another folder is not on the Upstream and becomes a local Repo.
	lms := filepath.Join(root, "lmstudio")
	write(t, filepath.Join(lms, "bartowski", "Tiny-GGUF", "Tiny-Q4_K_M.gguf"), w.q4)
	write(t, filepath.Join(lms, "someone", "private-tune", "tune-Q4_0.gguf"), []byte("my own quant"))
	rep, err = importer.New(importer.Options{Target: target, Namespace: "alice"}).FromLMStudio(ctx, lms)
	require.NoError(t, err)
	require.Len(t, rep.Results, 2)
	for _, r := range rep.Results {
		require.NoError(t, r.Err, r.Repo.String())
	}
	require.True(t, w.present(t, w.gguf, "Tiny-Q4_K_M.gguf"))
	require.False(t, w.present(t, w.gguf, "Tiny-Q8_0.gguf"))
	w.check(t, w.gguf, "Tiny-Q4_K_M.gguf", w.q4)
	w.check(t, model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "private-tune"}, "tune-Q4_0.gguf", []byte("my own quant"))
	require.Equal(t, cdn, w.h.Server.Requests("cdn"))

	// A directory.
	dir := filepath.Join(root, "dir")
	write(t, filepath.Join(dir, "sub", "weights.bin"), w.weights)
	write(t, filepath.Join(dir, "README.md"), []byte("# mine"))
	write(t, filepath.Join(dir, ".git", "HEAD"), []byte("ref: x"))
	// A link out of the folder is never followed (it used to upload the
	// file it named); a link within it is.
	write(t, filepath.Join(root, "secret"), []byte("PRIVATE KEY"))
	require.NoError(t, os.Symlink(filepath.Join(root, "secret"), filepath.Join(dir, "config.json")))
	require.NoError(t, os.Symlink("README.md", filepath.Join(dir, "README-copy.md")))
	mine := model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "from-dir"}
	res := importer.New(importer.Options{Target: target}).FromDir(ctx, dir, mine, "")
	require.NoError(t, res.Err)
	require.Equal(t, 3, res.Files, ".git and the link out of the folder are skipped")
	w.check(t, mine, "sub/weights.bin", w.weights)
	res = importer.New(importer.Options{Target: target}).FromDir(ctx, filepath.Join(root, "empty-missing"), mine, "")
	require.Error(t, res.Err)

	// Ollama.
	ol := filepath.Join(root, "ollama")
	gguf := bytes.Repeat([]byte("GGUF"), 50)
	tmpl := []byte("{{ .Prompt }}")
	write(t, filepath.Join(ol, "blobs", "sha256-"+sum(gguf)), gguf)
	write(t, filepath.Join(ol, "blobs", "sha256-"+sum(tmpl)), tmpl)
	man, _ := json.Marshal(map[string]any{"layers": []map[string]any{
		{"mediaType": "application/vnd.ollama.image.model", "digest": "sha256:" + sum(gguf), "size": len(gguf)},
		{"mediaType": "application/vnd.ollama.image.template", "digest": "sha256:" + sum(tmpl), "size": len(tmpl)},
		{"mediaType": "application/vnd.ollama.image.unknown", "digest": "sha256:" + sum(tmpl), "size": len(tmpl)}}})
	write(t, filepath.Join(ol, "manifests", "registry.ollama.ai", "library", "qwen3", "0.6b"), man)
	rep, err = importer.New(importer.Options{Target: target, Namespace: "alice"}).FromOllama(ctx, ol)
	require.NoError(t, err)
	require.Len(t, rep.Results, 1)
	require.NoError(t, rep.Results[0].Err)
	olID := model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "qwen3"}
	require.Equal(t, olID, rep.Results[0].Repo)
	w.check(t, olID, "qwen3-0.6b.gguf", gguf)
	w.check(t, olID, "template", tmpl)
}

func TestImportWithFakeTarget(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		w := newWorld(t, d)
		importAll(t, w, &fakeTarget{h: w.h, user: w.alice})
	})
}

// nativeStub serves the native API endpoints the HTTPTarget uses: W6's
// real Pull and Mirror handlers, and test versions of W2's Repo, upload
// and Commit endpoints over the Harness fakes.
type nativeStub struct {
	*api.Upstreams
	stubUploads
}

type stubUploads struct {
	gen.Unimplemented
	h    *upstreamtest.Harness
	user model.Principal

	mu      sync.Mutex
	uploads map[uuid.UUID]stubUpload
}

type stubUpload struct {
	oid  string
	size int64
	key  string
	mid  string
}

func (s *stubUploads) CreateRepo(w http.ResponseWriter, r *http.Request) {
	var body gen.RepoCreate
	_ = json.NewDecoder(r.Body).Decode(&body)
	t, _ := model.ParseRepoType(string(body.Type))
	ft := fakeTarget{h: s.h, user: s.user}
	id := model.RepoID{Type: t, Namespace: body.Namespace, Name: body.Name}
	if _, err := s.h.Repos.Get(r.Context(), id); err == nil {
		httpx.WriteProblem(w, r, &httpx.Problem{Status: http.StatusConflict, Detail: "exists"})
		return
	}
	if err := ft.CreateRepo(r.Context(), id); err != nil {
		httpx.WriteProblem(w, r, &httpx.Problem{Status: http.StatusBadRequest, Detail: err.Error()})
		return
	}
	w.WriteHeader(http.StatusCreated)
	_, _ = w.Write([]byte(`{}`))
}

func (s *stubUploads) PlanUploads(w http.ResponseWriter, r *http.Request, _ gen.RepoType, _ gen.Ns, _ gen.Name) {
	var body gen.UploadPlanRequest
	_ = json.NewDecoder(r.Body).Decode(&body)
	out := gen.UploadPlan{}
	for _, f := range body.Files {
		item := gen.UploadPlanItem{Path: f.Path}
		switch {
		case f.Size < 64 && !strings.HasSuffix(f.Path, ".bin") && !strings.HasSuffix(f.Path, ".gguf") && !strings.HasSuffix(f.Path, ".safetensors"):
			item.Action = "inline"
		default:
			if o, err := s.h.LFS.Stat(r.Context(), f.Sha256); err == nil && o.Verified {
				item.Action = "exists"
				break
			}
			id := uuid.New()
			key := "uploads/" + id.String()
			mid, err := s.h.LFS.Store.CreateMultipart(r.Context(), key)
			if err != nil {
				httpx.WriteProblem(w, r, &httpx.Problem{Status: 500, Detail: err.Error()})
				return
			}
			s.mu.Lock()
			if s.uploads == nil {
				s.uploads = map[uuid.UUID]stubUpload{}
			}
			s.uploads[id] = stubUpload{oid: f.Sha256, size: f.Size, key: key, mid: mid}
			s.mu.Unlock()
			ps := int64(100)
			var urls []string
			for n := int64(1); (n-1)*ps < max(f.Size, 1); n++ {
				urls = append(urls, "http://"+r.Host+"/api/hangar/v1/repos/x/y/z/uploads/"+id.String()+"/parts/"+strconv.FormatInt(n, 10))
			}
			item.Action, item.UploadId, item.PartSize, item.PartUrls = "upload", &id, &ps, &urls
		}
		out.Files = append(out.Files, item)
	}
	httpx.WriteJSON(w, r, http.StatusOK, out)
}

func (s *stubUploads) PutUploadPart(w http.ResponseWriter, r *http.Request, _ gen.RepoType, _ gen.Ns, _ gen.Name, id gen.ID, n int) {
	if r.Header.Get("Authorization") == "" {
		httpx.WriteProblem(w, r, &httpx.Problem{Status: http.StatusUnauthorized, Detail: "no token"})
		return
	}
	s.mu.Lock()
	u := s.uploads[id]
	s.mu.Unlock()
	etag, err := s.h.LFS.Store.PutPart(r.Context(), u.key, u.mid, n, r.Body, r.ContentLength)
	if err != nil {
		httpx.WriteProblem(w, r, &httpx.Problem{Status: 500, Detail: err.Error()})
		return
	}
	w.Header().Set("ETag", etag)
	httpx.WriteJSON(w, r, http.StatusOK, gen.UploadedPart{Number: n, Etag: etag})
}

func (s *stubUploads) CompleteUpload(w http.ResponseWriter, r *http.Request, _ gen.RepoType, _ gen.Ns, _ gen.Name, id gen.ID) {
	var body gen.UploadComplete
	_ = json.NewDecoder(r.Body).Decode(&body)
	s.mu.Lock()
	u := s.uploads[id]
	s.mu.Unlock()
	var parts []blob.Part
	for _, p := range body.Parts {
		parts = append(parts, blob.Part{Number: p.Number, ETag: p.Etag})
	}
	ctx := r.Context()
	if err := s.h.LFS.Store.CompleteMultipart(ctx, u.key, u.mid, parts); err != nil {
		httpx.WriteProblem(w, r, &httpx.Problem{Status: 500, Detail: err.Error()})
		return
	}
	rc, _, err := s.h.LFS.Store.Get(ctx, u.key, nil)
	if err == nil {
		_, err = s.h.LFS.Ingest(ctx, u.oid, u.size, rc)
		_ = rc.Close()
	}
	if err != nil {
		httpx.WriteProblem(w, r, &httpx.Problem{Status: http.StatusBadRequest, Detail: err.Error()})
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, map[string]any{})
}

func (s *stubUploads) CreateCommit(w http.ResponseWriter, r *http.Request, pType gen.RepoType, ns gen.Ns, name gen.Name) {
	var body gen.CommitRequest
	_ = json.NewDecoder(r.Body).Decode(&body)
	t := model.RepoTypeModel
	if pType == gen.Datasets {
		t = model.RepoTypeDataset
	}
	var ops []repo.Operation
	for _, op := range body.Operations {
		switch op.Op {
		case gen.Add:
			b, _ := base64.StdEncoding.DecodeString(*op.ContentBase64)
			ops = append(ops, repo.Operation{Kind: repo.OpAdd, Path: op.Path, Content: bytes.NewReader(b)})
		case gen.AddLfs:
			ops = append(ops, repo.Operation{Kind: repo.OpAddLFS, Path: op.Path, OID: *op.Sha256, Size: *op.Size})
		}
	}
	res, err := s.h.Repos.Commit(r.Context(), repo.CommitParams{Repo: model.RepoID{Type: t, Namespace: ns, Name: name}, Summary: body.Summary, Operations: ops, Actor: s.user})
	if err != nil {
		httpx.WriteProblem(w, r, &httpx.Problem{Status: http.StatusBadRequest, Detail: err.Error()})
		return
	}
	httpx.WriteJSON(w, r, http.StatusCreated, map[string]string{"sha": string(res.SHA)})
}

func TestImportWithHTTPTarget(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		w := newWorld(t, d)
		h := w.h
		authn := authtest.NewAuthenticator()
		authn.Add("alice-token", w.alice)
		stub := nativeStub{Upstreams: &api.Upstreams{Service: h.Service, Client: h.Client, Repos: h.Repos, Authz: h.Authz, Queue: h.Queue},
			stubUploads: stubUploads{h: h, user: w.alice}}
		native := gen.HandlerWithOptions(&stub, gen.ChiServerOptions{BaseURL: "/api/hangar/v1"})
		// The Instance's Hub API serves trees of local Repos.
		local := upstreamtest.NewFake(t)
		local.Repos = h.Repos.Fake
		hub := upstreamtest.NewServer(t, local)
		hubURL, _ := http.NewRequest(http.MethodGet, hub.URL, nil)
		mux := http.NewServeMux()
		mux.Handle("/api/hangar/v1/", httpx.RequestIDs(httpx.Authenticate(authn, nil)(native)))
		mux.Handle("/", http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
			r2 := r.Clone(r.Context())
			r2.URL.Scheme, r2.URL.Host, r2.RequestURI = "http", hubURL.URL.Host, ""
			resp, err := http.DefaultTransport.RoundTrip(r2)
			if err != nil {
				rw.WriteHeader(http.StatusBadGateway)
				return
			}
			defer resp.Body.Close()
			for k, v := range resp.Header {
				rw.Header()[k] = v
			}
			rw.WriteHeader(resp.StatusCode)
			_, _ = io.Copy(rw, resp.Body)
		}))
		srv := httptest.NewServer(mux)
		defer srv.Close()
		hub.URL = srv.URL // pagination links name the Instance

		// Pulls run in the background, as a worker would.
		ctx, cancel := context.WithCancel(context.Background())
		var wg sync.WaitGroup
		wg.Go(func() {
			for ctx.Err() == nil {
				h.Queue.RunPending(ctx)
				h.Advance(10 * 60 * 1e9)
			}
		})
		defer func() { cancel(); wg.Wait() }()
		target := &importer.HTTPTarget{BaseURL: srv.URL, Token: "alice-token", PollInterval: 10 * 1e6, Version: "test"}
		importAll(t, w, target)

		// Errors from the Instance carry its message.
		err := target.PullFiles(ctx, model.RepoID{Type: model.RepoTypeModel, Namespace: "nobody", Name: "nothing"}, []string{"x"})
		require.ErrorContains(t, err, "404")
	})
}
