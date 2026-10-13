// Package clitest is a fake Hangar Instance for testing the hangar client
// commands. It serves the parts of the native API (through the generated
// server in internal/api/gen, so requests and answers follow
// api/openapi.yaml), the Hub file downloads and the device-code login that
// internal/cli uses, over an httptest.Server, with in-memory Repos, a fake
// Upstream catalogue and Pull Jobs that finish after a few polls.
package clitest

import (
	"bytes"
	"crypto/sha1" // #nosec G505 -- git object IDs are SHA-1.
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/jamesagarside/hangar/internal/api/gen"
	"github.com/jamesagarside/hangar/internal/model"
)

// Token is the write Token of the user "alice" that New registers.
const Token = "hgr_alice_test_token"

// File is a file in a Repo or an upstream Repo.
type File struct {
	Path    string
	Content []byte
	// LFS stores the file as an LFS object. Files over LFSThreshold, and
	// GGUF, safetensors and bin files, are LFS automatically.
	LFS bool
}

func (f File) sha256() string {
	s := sha256.Sum256(f.Content)
	return hex.EncodeToString(s[:])
}

// gitBlobID returns the git object ID of content.
func gitBlobID(content []byte) string {
	h := sha1.New() // #nosec G401 -- git object IDs are SHA-1.
	fmt.Fprintf(h, "blob %d\x00", len(content))
	h.Write(content)
	return hex.EncodeToString(h.Sum(nil))
}

// pointer returns the LFS pointer file for f.
func (f File) pointer() []byte {
	return []byte(fmt.Sprintf("version https://git-lfs.github.com/spec/v1\noid sha256:%s\nsize %d\n", f.sha256(), len(f.Content)))
}

// commit is a snapshot of a Repo's files.
type commit struct {
	sha   string
	files map[string]File
}

// Repo is a Repo in the fake Arsenal.
type Repo struct {
	ID            model.RepoID
	Visibility    gen.Visibility
	Mirror        bool
	Upstream      string
	DefaultBranch string
	refs          map[string]string // revision name -> Commit SHA
	commits       map[string]*commit
	// Configs is returned by the dataset Configs endpoint.
	Configs []gen.DatasetConfig
	// Dependencies is returned by the Dependencies endpoint.
	Dependencies gen.Dependencies
}

// UpstreamRepo is a Repo on the fake Upstream, by Revision.
type UpstreamRepo struct {
	ID        model.RepoID
	Revisions map[string][]File
}

type upload struct {
	id    uuid.UUID
	repo  model.RepoID
	sha   string
	size  int64
	parts map[int][]byte
	done  bool
}

type job struct {
	j       gen.Job
	polls   int
	finish  func() error
	created time.Time
}

// Instance is a running fake Instance.
type Instance struct {
	// URL is the base URL.
	URL string
	srv *httptest.Server

	mu sync.Mutex
	// Users maps Tokens to usernames.
	Users map[string]string
	// LFSThreshold makes files over this size LFS objects. Default 1 KiB.
	LFSThreshold int64
	// InlineMax is the largest non-LFS file the upload plan sends inline.
	InlineMax int64
	// PartSize is the multipart part size. Default 64 KiB.
	PartSize int64
	// Presign makes part URLs presigned (no Authorization needed).
	Presign bool
	// JobPolls is how many polls a Job takes to finish. Default 2.
	JobPolls int
	// FailPulls makes Pulls fail with this message.
	FailPulls string
	// DeviceAutoApprove approves device logins on the first poll.
	DeviceAutoApprove bool

	repos       map[string]*Repo
	upstream    map[string]*UpstreamRepo
	objects     map[string][]byte
	uploads     map[uuid.UUID]*upload
	jobs        map[uuid.UUID]*job
	jobOrder    []uuid.UUID
	ollamaKeys  []gen.OllamaKey
	devices     map[string]*device
	failedParts map[string]int
	// Requests counts requests by "METHOD path-template".
	Requests map[string]int
	partPuts int
}

type device struct {
	userCode string
	approved bool
	denied   bool
}

// New starts a fake Instance with the user alice and closes it when the
// test ends.
func New(t testing.TB) *Instance {
	t.Helper()
	i := &Instance{
		Users:        map[string]string{Token: "alice"},
		LFSThreshold: 1 << 10,
		InlineMax:    1 << 10,
		PartSize:     64 << 10,
		JobPolls:     2,
		repos:        map[string]*Repo{},
		upstream:     map[string]*UpstreamRepo{},
		objects:      map[string][]byte{},
		uploads:      map[uuid.UUID]*upload{},
		jobs:         map[uuid.UUID]*job{},
		devices:      map[string]*device{},
		failedParts:  map[string]int{},
		Requests:     map[string]int{},
	}
	r := chi.NewRouter()
	r.Use(i.count)
	r.Post("/oauth/device", i.oauthDevice)
	r.Post("/oauth/token", i.oauthToken)
	r.Put("/presigned/{id}/{n}", i.presignedPart)
	r.Get("/datasets/{ns}/{name}/resolve/{rev}/*", i.resolve(model.RepoTypeDataset))
	r.Get("/{ns}/{name}/resolve/{rev}/*", i.resolve(model.RepoTypeModel))
	gen.HandlerWithOptions(&server{i: i}, gen.ChiServerOptions{BaseURL: "/api/hangar/v1", BaseRouter: r})
	i.srv = httptest.NewServer(r)
	i.URL = i.srv.URL
	t.Cleanup(i.srv.Close)
	return i
}

func (i *Instance) count(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)
		pattern := chi.RouteContext(r.Context()).RoutePattern()
		i.mu.Lock()
		i.Requests[r.Method+" "+pattern]++
		i.mu.Unlock()
	})
}

func repoKey(id model.RepoID) string { return id.Path() }

// AddUpstream adds a Repo to the fake Upstream with files on `main`.
func (i *Instance) AddUpstream(id model.RepoID, files ...File) *UpstreamRepo {
	i.mu.Lock()
	defer i.mu.Unlock()
	u := &UpstreamRepo{ID: id, Revisions: map[string][]File{"main": i.classify(files)}}
	i.upstream[repoKey(id)] = u
	return u
}

// AddUpstreamRevision adds files at another Revision of an upstream Repo,
// such as refs/convert/parquet.
func (i *Instance) AddUpstreamRevision(id model.RepoID, rev string, files ...File) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.upstream[repoKey(id)].Revisions[rev] = i.classify(files)
}

// classify marks large and weight files as LFS.
func (i *Instance) classify(files []File) []File {
	out := make([]File, len(files))
	for k, f := range files {
		ext := path.Ext(f.Path)
		if int64(len(f.Content)) > i.LFSThreshold || ext == ".gguf" || ext == ".safetensors" || ext == ".bin" || ext == ".parquet" {
			f.LFS = true
		}
		out[k] = f
	}
	return out
}

// AddRepo creates a local Repo with one Commit on main holding files, all
// present.
func (i *Instance) AddRepo(id model.RepoID, files ...File) *Repo {
	i.mu.Lock()
	defer i.mu.Unlock()
	r := i.newRepo(id, false, "")
	fs := i.classify(files)
	for _, f := range fs {
		if f.LFS {
			i.objects[f.sha256()] = f.Content
		}
	}
	i.commitFiles(r, "main", fs)
	return r
}

func (i *Instance) newRepo(id model.RepoID, mirror bool, upstream string) *Repo {
	r := &Repo{ID: id, Visibility: gen.Public, Mirror: mirror, Upstream: upstream, DefaultBranch: "main",
		refs: map[string]string{}, commits: map[string]*commit{}}
	i.repos[repoKey(id)] = r
	return r
}

// commitFiles records a Commit of exactly files on a revision and returns its SHA.
func (i *Instance) commitFiles(r *Repo, rev string, files []File) string {
	m := map[string]File{}
	h := sha1.New() // #nosec G401 -- a stand-in Commit SHA.
	sorted := append([]File{}, files...)
	sort.Slice(sorted, func(a, b int) bool { return sorted[a].Path < sorted[b].Path })
	fmt.Fprintf(h, "%s\x00%s\x00", r.ID.Path(), rev)
	for _, f := range sorted {
		m[f.Path] = f
		fmt.Fprintf(h, "%s\x00%s\x00", f.Path, f.sha256())
	}
	sha := hex.EncodeToString(h.Sum(nil))
	r.commits[sha] = &commit{sha: sha, files: m}
	r.refs[rev] = sha
	return sha
}

// Repo returns a Repo in the Arsenal, or nil.
func (i *Instance) Repo(id model.RepoID) *Repo {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.repos[repoKey(id)]
}

// Head returns the Commit a revision of a Repo points at.
func (i *Instance) Head(id model.RepoID, rev string) string {
	i.mu.Lock()
	defer i.mu.Unlock()
	if r := i.repos[repoKey(id)]; r != nil {
		return r.refs[rev]
	}
	return ""
}

// Files returns a Repo's files at a revision, by path.
func (i *Instance) Files(id model.RepoID, rev string) map[string][]byte {
	i.mu.Lock()
	defer i.mu.Unlock()
	r := i.repos[repoKey(id)]
	if r == nil {
		return nil
	}
	c := r.commits[r.refs[rev]]
	if c == nil {
		return nil
	}
	out := map[string][]byte{}
	for p, f := range c.files {
		out[p] = f.Content
	}
	return out
}

// HasObject reports whether the Instance holds an LFS object.
func (i *Instance) HasObject(sha string) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	_, ok := i.objects[sha]
	return ok
}

// Present reports whether a file of a Repo is in the Arsenal.
func (i *Instance) Present(id model.RepoID, rev, p string) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	r := i.repos[repoKey(id)]
	if r == nil || r.commits[r.refs[rev]] == nil {
		return false
	}
	f, ok := r.commits[r.refs[rev]].files[p]
	return ok && i.present(f)
}

func (i *Instance) present(f File) bool {
	if !f.LFS {
		return true
	}
	_, ok := i.objects[f.sha256()]
	return ok
}

// FailPart makes the next n uploads of part number n of a file fail, to
// test resumed uploads. key is "<sha256>/<part>".
func (i *Instance) FailPart(sha string, part, times int) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.failedParts[fmt.Sprintf("%s/%d", sha, part)] = times
}

// PartPuts returns how many upload parts were stored.
func (i *Instance) PartPuts() int {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.partPuts
}

// OllamaKeys returns the registered Ollama keys.
func (i *Instance) OllamaKeys() []gen.OllamaKey {
	i.mu.Lock()
	defer i.mu.Unlock()
	return append([]gen.OllamaKey{}, i.ollamaKeys...)
}

// AddOllamaKey registers a key fingerprint for the user.
func (i *Instance) AddOllamaKey(fingerprint string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.ollamaKeys = append(i.ollamaKeys, gen.OllamaKey{Id: uuid.New(), Fingerprint: fingerprint, CreatedAt: time.Now()})
}

// RequestCount returns how many requests matched a route, such as
// "POST /api/hangar/v1/pulls".
func (i *Instance) RequestCount(route string) int {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.Requests[route]
}

// ApproveDevice approves a pending device login by user code.
func (i *Instance) ApproveDevice(userCode string) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	for _, d := range i.devices {
		if d.userCode == userCode {
			d.approved = true
			return true
		}
	}
	return false
}

// quantRe finds a GGUF quant token in a file name.
var quantRe = regexp.MustCompile(`(?i)(?:^|[-_.])(IQ\d_[A-Z]+|Q\d_K_[SML]|Q\d_K|Q\d_\d|BF16|F16|F32)(?:[-_.]|$)`)

// quantOf returns a GGUF file's quant name, upper case.
func quantOf(p string) string {
	if path.Ext(p) != ".gguf" || strings.HasPrefix(path.Base(p), "mmproj-") {
		return ""
	}
	m := quantRe.FindStringSubmatch(path.Base(p))
	if m == nil {
		return ""
	}
	return strings.ToUpper(m[1])
}

// fnmatch matches like Python's fnmatch: `*` matches `/` too.
func fnmatch(pattern, name string) bool {
	if strings.HasSuffix(pattern, "/") {
		pattern += "*"
	}
	re := "^" + strings.ReplaceAll(strings.ReplaceAll(regexp.QuoteMeta(pattern), `\*`, ".*"), `\?`, ".") + "$"
	ok, _ := regexp.MatchString(re, name)
	return ok
}

// selected reports whether a Pull selection picks a file.
func selected(sel *gen.Selection, f File) bool {
	if sel == nil {
		return true
	}
	if sel.Paths != nil {
		// Explicit paths; an empty list selects no files (history only).
		for _, p := range *sel.Paths {
			if p == f.Path {
				return true
			}
		}
		return false
	}
	if sel.Quants != nil {
		q := quantOf(f.Path)
		ok := strings.HasPrefix(path.Base(f.Path), "mmproj-")
		for _, want := range *sel.Quants {
			if strings.EqualFold(q, want) {
				ok = true
			}
		}
		if !ok {
			return false
		}
	}
	if sel.Include != nil && len(*sel.Include) > 0 {
		ok := false
		for _, p := range *sel.Include {
			ok = ok || fnmatch(p, f.Path)
		}
		if !ok {
			return false
		}
	}
	if sel.Exclude != nil {
		for _, p := range *sel.Exclude {
			if fnmatch(p, f.Path) {
				return false
			}
		}
	}
	return true
}

func (i *Instance) user(r *http.Request) string {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return ""
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.Users[tok]
}

// Close stops the server early.
func (i *Instance) Close() { i.srv.Close() }

// readBody reads a whole request body, failing on a short or broken read
// as an object store does.
func readBody(r *http.Request) ([]byte, bool) {
	var b bytes.Buffer
	if _, err := b.ReadFrom(r.Body); err != nil {
		return nil, false
	}
	return b.Bytes(), r.ContentLength < 0 || int64(b.Len()) == r.ContentLength
}
