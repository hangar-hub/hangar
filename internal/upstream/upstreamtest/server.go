package upstreamtest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hangar-hub/hangar/internal/model"
	"github.com/hangar-hub/hangar/internal/repo"
	"github.com/hangar-hub/hangar/internal/upstream"
)

// PresignSecret is the fake signature the Server's CDN redirects carry, so
// tests can check that it never reaches errors or logs.
const PresignSecret = "presigned-secret-6d1f"

// Server is a Hugging Face Hub-compatible HTTP server over the Fake's
// Repos, for testing upstream.HTTPClient and the Pull machinery end to end.
// It serves repo info, search, tree (paged with absolute Link headers),
// resolve (LFS files redirect to a CDN on another origin, as huggingface.co
// does), the Dataset Viewer under /viewer, and git smart HTTP through the
// real `git http-backend`, protocol v0 and v2.
type Server struct {
	Fake *Fake
	// URL is the Hub's base URL.
	URL string
	// CDN serves LFS objects at /blob/<oid> on another origin.
	CDN *httptest.Server
	// TreePageSize is how many tree entries a page holds.
	TreePageSize int
	// NoProtocolV2 makes git requests ignore Git-Protocol, so clients
	// fall back to protocol v0.
	NoProtocolV2 bool

	hub *httptest.Server

	mu          sync.Mutex
	failures    []failure
	authOnCDN   int
	ranges      []string
	corruptNext map[string]int
	slowBytes   int64
	gate        chan struct{}
	requests    map[string]int
}

type failure struct {
	prefix     string
	status     int
	retryAfter string
	n          int
}

// NewServer starts a Server over f; it stops when the test ends.
func NewServer(t testing.TB, f *Fake) *Server {
	t.Helper()
	s := &Server{Fake: f, TreePageSize: 2, corruptNext: map[string]int{}, requests: map[string]int{}}
	s.CDN = httptest.NewServer(http.HandlerFunc(s.serveCDN))
	t.Cleanup(s.CDN.Close)
	s.hub = httptest.NewServer(http.HandlerFunc(s.serveHub))
	t.Cleanup(s.hub.Close)
	s.URL = s.hub.URL
	return s
}

// Upstream returns an upstream.Upstream for the Server.
func (s *Server) Upstream(name string) upstream.Upstream {
	return upstream.Upstream{Name: name, BaseURL: s.URL, ViewerURL: s.URL + "/viewer", Enabled: true}
}

// Fail makes the next n requests whose path starts with prefix answer
// status, with Retry-After when retryAfter is set.
func (s *Server) Fail(prefix string, status, n int, retryAfter string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failures = append(s.failures, failure{prefix: prefix, status: status, retryAfter: retryAfter, n: n})
}

// CorruptNext makes the next n CDN downloads of oid flip a byte.
func (s *Server) CorruptNext(oid string, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.corruptNext[oid] = n
}

// BreakAfter makes CDN downloads stop after n bytes of each response,
// until called with 0.
func (s *Server) BreakAfter(n int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.slowBytes = n
}

// Hold makes CDN downloads wait after their headers until Release.
func (s *Server) Hold() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gate = make(chan struct{})
}

// Release lets held CDN downloads continue.
func (s *Server) Release() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.gate != nil {
		close(s.gate)
		s.gate = nil
	}
}

// AuthOnCDN counts CDN requests that carried an Authorization header,
// which must never happen.
func (s *Server) AuthOnCDN() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.authOnCDN
}

// Ranges returns the Range headers the CDN received.
func (s *Server) Ranges() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.ranges...)
}

// Requests counts requests by kind: info, search, tree, resolve, cdn, git.
func (s *Server) Requests(kind string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests[kind]
}

func (s *Server) count(kind string) {
	s.mu.Lock()
	s.requests[kind]++
	s.mu.Unlock()
}

func (s *Server) injected(w http.ResponseWriter, r *http.Request) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.failures {
		f := &s.failures[i]
		if f.n > 0 && strings.HasPrefix(r.URL.Path, f.prefix) {
			f.n--
			if f.retryAfter != "" {
				w.Header().Set("Retry-After", f.retryAfter)
			}
			http.Error(w, "injected failure", f.status)
			return true
		}
	}
	return false
}

func credOf(r *http.Request) upstream.Credentials {
	if tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		return upstream.Credentials{Token: tok}
	}
	if _, pw, ok := r.BasicAuth(); ok {
		return upstream.Credentials{Token: pw}
	}
	return upstream.Credentials{}
}

func hubError(w http.ResponseWriter, err error, commit model.CommitSHA) {
	if commit != "" {
		w.Header().Set("X-Repo-Commit", string(commit))
	}
	switch {
	case errors.Is(err, upstream.ErrGated):
		w.Header().Set("X-Error-Code", "GatedRepo")
		w.WriteHeader(http.StatusForbidden)
	case errors.Is(err, repo.ErrEntryNotFound):
		w.Header().Set("X-Error-Code", "EntryNotFound")
		w.WriteHeader(http.StatusNotFound)
	case errors.Is(err, repo.ErrRevisionNotFound):
		w.Header().Set("X-Error-Code", "RevisionNotFound")
		w.WriteHeader(http.StatusNotFound)
	case errors.Is(err, upstream.ErrNotFound):
		w.Header().Set("X-Error-Code", "RepoNotFound")
		w.WriteHeader(http.StatusUnauthorized)
	default:
		w.WriteHeader(http.StatusInternalServerError)
	}
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

func (s *Server) serveHub(w http.ResponseWriter, r *http.Request) {
	if s.injected(w, r) {
		return
	}
	segs := strings.Split(strings.Trim(r.URL.EscapedPath(), "/"), "/")
	for i := range segs {
		segs[i], _ = url.PathUnescape(segs[i])
	}
	switch {
	case len(segs) >= 1 && segs[0] == "viewer":
		s.serveViewer(w, r, "/"+strings.Join(segs[1:], "/"))
	case len(segs) == 2 && segs[0] == "api" && (segs[1] == "models" || segs[1] == "datasets"):
		s.serveSearch(w, r, segs[1])
	case len(segs) >= 4 && segs[0] == "api" && (segs[1] == "models" || segs[1] == "datasets"):
		s.serveAPI(w, r, segs[1], segs[2:])
	default:
		t := model.RepoTypeModel
		if len(segs) > 0 && segs[0] == "datasets" {
			t, segs = model.RepoTypeDataset, segs[1:]
		}
		if len(segs) < 3 {
			http.NotFound(w, r)
			return
		}
		id, err := model.ParseRepoID(t, segs[0]+"/"+strings.TrimSuffix(segs[1], ".git"))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		switch segs[2] {
		case "resolve":
			s.serveResolve(w, r, id, segs[3:])
		case "info", "git-upload-pack":
			s.serveGit(w, r, id, strings.Join(segs[2:], "/"))
		default:
			http.NotFound(w, r)
		}
	}
}

func repoType(plural string) model.RepoType {
	if plural == "datasets" {
		return model.RepoTypeDataset
	}
	return model.RepoTypeModel
}

func (s *Server) serveSearch(w http.ResponseWriter, r *http.Request, plural string) {
	s.count("search")
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	page, err := s.Fake.Search(r.Context(), upstream.Upstream{}, credOf(r), upstream.SearchQuery{Type: repoType(plural), Search: q.Get("search"),
		Author: q.Get("author"), Filter: q["filter"], Limit: limit, Cursor: q.Get("cursor")})
	if err != nil {
		hubError(w, err, "")
		return
	}
	out := []map[string]any{}
	for _, rp := range page.Repos {
		out = append(out, map[string]any{"id": rp.ID.String(), "sha": rp.SHA, "private": rp.Private, "tags": rp.Tags, "downloads": 3, "likes": 1,
			"lastModified": rp.LastModified.UTC().Format(time.RFC3339), "gated": false})
	}
	if page.NextCursor != "" {
		nq := url.Values{}
		for k, v := range q {
			nq[k] = v
		}
		nq.Set("cursor", page.NextCursor)
		w.Header().Set("Link", "<"+s.URL+r.URL.Path+"?"+nq.Encode()+`>; rel="next"`)
	}
	_ = json.NewEncoder(w).Encode(out)
}

func (s *Server) serveAPI(w http.ResponseWriter, r *http.Request, plural string, rest []string) {
	id, err := model.ParseRepoID(repoType(plural), rest[0]+"/"+rest[1])
	if err != nil {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	cred := credOf(r)
	switch {
	case len(rest) == 2 || (len(rest) == 4 && rest[2] == "revision"):
		s.count("info")
		rev := ""
		if len(rest) == 4 {
			rev = rest[3]
		}
		info, err := s.Fake.RepoInfo(ctx, upstream.Upstream{}, cred, id, rev)
		if err != nil {
			hubError(w, err, "")
			return
		}
		sibs := []map[string]any{}
		for _, sb := range info.Siblings {
			m := map[string]any{"rfilename": sb.Path, "size": sb.Size}
			if sb.LFS != nil {
				m["lfs"] = map[string]any{"sha256": sb.LFS.OID, "size": sb.LFS.Size, "pointerSize": sb.LFS.PointerSize}
			}
			sibs = append(sibs, m)
		}
		gated := any(false)
		for _, g := range s.Fake.Gated {
			if g == id.String() {
				gated = "manual"
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": id.String(), "sha": info.SHA, "private": info.Private, "gated": gated, "tags": info.Tags,
			"lastModified": info.LastModified.UTC().Format(time.RFC3339), "siblings": sibs, "library_name": "transformers"})
	case len(rest) >= 4 && rest[2] == "tree":
		s.count("tree")
		entries, err := s.Fake.Tree(ctx, upstream.Upstream{}, cred, id, rest[3], strings.Join(rest[4:], "/"), r.URL.Query().Get("recursive") == "true")
		if err != nil {
			hubError(w, err, "")
			return
		}
		start, _ := strconv.Atoi(r.URL.Query().Get("cursor"))
		end := min(start+s.TreePageSize, len(entries))
		out := []map[string]any{}
		for _, e := range entries[start:end] {
			m := map[string]any{"type": string(e.Type), "oid": e.OID, "size": e.Size, "path": e.Path}
			if e.LFS != nil {
				m["lfs"] = map[string]any{"oid": e.LFS.OID, "size": e.LFS.Size, "pointerSize": e.LFS.PointerSize}
			}
			out = append(out, m)
		}
		if end < len(entries) {
			nq := r.URL.Query()
			nq.Set("cursor", strconv.Itoa(end))
			w.Header().Set("Link", "<"+s.URL+r.URL.EscapedPath()+"?"+nq.Encode()+`>; rel="next"`)
		}
		_ = json.NewEncoder(w).Encode(out)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) serveResolve(w http.ResponseWriter, r *http.Request, id model.RepoID, rest []string) {
	s.count("resolve")
	if len(rest) < 2 {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	cred := credOf(r)
	if _, err := s.Fake.access(ctx, cred, id); err != nil {
		hubError(w, err, "")
		return
	}
	sha, err := s.Fake.Repos.Resolve(ctx, id, rest[0])
	if err != nil {
		hubError(w, err, "")
		return
	}
	path := strings.Join(rest[1:], "/")
	e, err := s.Fake.Repos.Entry(ctx, id, sha, path)
	if err != nil || e.Type != model.EntryFile {
		if err == nil {
			err = repo.ErrEntryNotFound
		}
		hubError(w, err, sha)
		return
	}
	if err := s.Fake.slowFile(ctx); err != nil {
		return
	}
	w.Header().Set("X-Repo-Commit", string(sha))
	if e.LFS != nil {
		w.Header().Set("X-Linked-Etag", `"`+e.LFS.OID+`"`)
		w.Header().Set("X-Linked-Size", strconv.FormatInt(e.LFS.Size, 10))
		target := s.CDN.URL + "/blob/" + e.LFS.OID + "?X-Amz-Signature=" + PresignSecret
		http.Redirect(w, r, target, http.StatusFound)
		return
	}
	rc, _, err := s.Fake.Repos.OpenFile(ctx, id, sha, path, nil)
	if err != nil {
		hubError(w, err, sha)
		return
	}
	defer rc.Close()
	b, _ := io.ReadAll(rc)
	w.Header().Set("ETag", `"`+e.OID+`"`)
	http.ServeContent(w, r, path, time.Time{}, bytes.NewReader(b))
}

func (s *Server) serveCDN(w http.ResponseWriter, r *http.Request) {
	s.count("cdn")
	s.mu.Lock()
	if r.Header.Get("Authorization") != "" {
		s.authOnCDN++
	}
	if rg := r.Header.Get("Range"); rg != "" {
		s.ranges = append(s.ranges, rg)
	}
	gate, breakAfter := s.gate, s.slowBytes
	s.mu.Unlock()
	oid, ok := strings.CutPrefix(r.URL.Path, "/blob/")
	if !ok || r.URL.Query().Get("X-Amz-Signature") != PresignSecret {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	rc, obj, err := s.Fake.Repos.LFS.Open(r.Context(), oid, nil)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	b, _ := io.ReadAll(rc)
	_ = rc.Close()
	_ = obj
	// Like huggingface.co's CDN, answer with an ETag of the CDN's own that
	// looks like a SHA-256 but is not the object's.
	cdnTag := sha256.Sum256([]byte("cdn:" + oid))
	w.Header().Set("ETag", `"`+hex.EncodeToString(cdnTag[:])+`"`)
	s.mu.Lock()
	if s.corruptNext[oid] > 0 {
		s.corruptNext[oid]--
		b = append([]byte(nil), b...)
		b[len(b)/2] ^= 0xff
	}
	s.mu.Unlock()
	if gate != nil {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.Itoa(len(b)))
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		half := len(b) / 2
		_, _ = w.Write(b[:half])
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-gate
		_, _ = w.Write(b[half:])
		return
	}
	if breakAfter > 0 {
		bw := &breakingWriter{ResponseWriter: w, left: breakAfter}
		http.ServeContent(bw, r, oid, time.Time{}, bytes.NewReader(b))
		return
	}
	http.ServeContent(w, r, oid, time.Time{}, bytes.NewReader(b))
}

// breakingWriter stops writing after a number of bytes and aborts the
// connection, as a dropped download would.
type breakingWriter struct {
	http.ResponseWriter
	left int64
}

func (b *breakingWriter) Write(p []byte) (int, error) {
	if b.left <= 0 {
		panic(http.ErrAbortHandler)
	}
	if int64(len(p)) > b.left {
		_, _ = b.ResponseWriter.Write(p[:b.left])
		b.left = 0
		if f, ok := b.ResponseWriter.(http.Flusher); ok {
			f.Flush()
		}
		panic(http.ErrAbortHandler)
	}
	b.left -= int64(len(p))
	return b.ResponseWriter.Write(p)
}

func (s *Server) serveViewer(w http.ResponseWriter, r *http.Request, endpoint string) {
	body, err := s.Fake.Viewer(r.Context(), upstream.Upstream{}, credOf(r), endpoint, r.URL.Query())
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"not found"}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

func (s *Server) serveGit(w http.ResponseWriter, r *http.Request, id model.RepoID, rest string) {
	s.count("git")
	ctx := r.Context()
	if _, err := s.Fake.access(ctx, credOf(r), id); err != nil {
		w.Header().Set("WWW-Authenticate", `Basic realm="test"`)
		http.Error(w, "not found", http.StatusUnauthorized)
		return
	}
	dir, err := s.Fake.Repos.Dir(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	gitPath, err := exec.LookPath("git")
	if err != nil {
		http.Error(w, "git not installed", http.StatusInternalServerError)
		return
	}
	env := []string{"GIT_PROJECT_ROOT=" + filepath.Dir(dir), "GIT_HTTP_EXPORT_ALL=1", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull, "HOME=" + filepath.Dir(dir)}
	if s.NoProtocolV2 {
		r.Header.Del("Git-Protocol")
	} else if p := r.Header.Get("Git-Protocol"); p != "" {
		env = append(env, "GIT_PROTOCOL="+p)
	}
	h := &cgi.Handler{Path: gitPath, Args: []string{"http-backend"}, Env: env, Root: "/"}
	r2 := r.Clone(context.WithoutCancel(ctx))
	r2.URL.Path = "/" + filepath.Base(dir) + "/" + rest
	r2.URL.RawPath = ""
	r2.Header.Del("Authorization")
	h.ServeHTTP(w, r2)
}

// String describes the Server for test failure messages.
func (s *Server) String() string { return fmt.Sprintf("upstreamtest.Server(%s)", s.URL) }

// Seed commits regular files and LFS files to a Repo on f, creating it
// public if needed, and returns the Commit.
func Seed(t testing.TB, f *Fake, id model.RepoID, files, lfs map[string][]byte) model.CommitSHA {
	t.Helper()
	ctx := context.Background()
	if _, err := f.Repos.Get(ctx, id); errors.Is(err, repo.ErrNotFound) {
		if _, err := f.Repos.Create(ctx, repo.CreateParams{ID: id, Visibility: model.VisibilityPublic}); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	var ops []repo.Operation
	for p, c := range files {
		ops = append(ops, repo.Operation{Kind: repo.OpAdd, Path: p, Content: bytes.NewReader(c)})
	}
	for p, c := range lfs {
		obj, err := f.Repos.LFS.Put(ctx, c)
		if err != nil {
			t.Fatalf("store %s: %v", p, err)
		}
		ops = append(ops, repo.Operation{Kind: repo.OpAddLFS, Path: p, OID: obj.OID, Size: obj.Size})
	}
	res, err := f.Repos.Commit(ctx, repo.CommitParams{Repo: id, Summary: "add files", Operations: ops, Actor: model.System(), AllowMirror: true})
	if err != nil {
		t.Fatalf("commit to %s: %v", id, err)
	}
	return res.SHA
}
