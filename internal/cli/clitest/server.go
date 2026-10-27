package clitest

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"golang.org/x/crypto/ssh"

	"github.com/hangar-hub/hangar/internal/api/gen"
	"github.com/hangar-hub/hangar/internal/model"
	hangarrepo "github.com/hangar-hub/hangar/internal/repo"
	"github.com/hangar-hub/hangar/internal/sshsig"
)

// server implements the native API operations the CLI uses. The rest
// answer 501 through gen.Unimplemented.
type server struct {
	gen.Unimplemented
	i *Instance
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func problem(w http.ResponseWriter, status int, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(gen.Problem{Type: "about:blank", Title: http.StatusText(status), Status: status, Detail: &detail})
}

func repoID(t gen.RepoTypePlural, ns, name string) model.RepoID {
	rt, _ := model.ParseRepoType(string(t))
	return model.RepoID{Type: rt, Namespace: ns, Name: name}
}

// authed answers 401 and returns false without a known Token.
func (s *server) authed(w http.ResponseWriter, r *http.Request) bool {
	if s.i.user(r) == "" {
		problem(w, http.StatusUnauthorized, "Sign in or send a Token.")
		return false
	}
	return true
}

// lookup finds a Repo the caller can see. Call with the lock held.
func (s *server) lookup(w http.ResponseWriter, r *http.Request, id model.RepoID) *Repo {
	repo := s.i.repos[repoKey(id)]
	if repo == nil || repo.Visibility != gen.Public && r.Header.Get("Authorization") == "" {
		problem(w, http.StatusNotFound, fmt.Sprintf("Repo %s not found.", id.Path()))
		return nil
	}
	return repo
}

// resolveRev returns the Commit for a revision name or SHA. Lock held.
func (repo *Repo) resolveRev(rev string) *commit {
	if sha, ok := repo.refs[rev]; ok {
		return repo.commits[sha]
	}
	return repo.commits[rev]
}

func (s *server) apiRepo(repo *Repo) gen.Repo {
	var size, lfs, present int64
	head := repo.commits[repo.refs[repo.DefaultBranch]]
	var quants []string
	if head != nil {
		seen := map[string]bool{}
		for _, f := range head.files {
			// As on the server, size_bytes is git data plus the LFS objects
			// held, and lfs_bytes is the LFS objects held.
			if f.LFS {
				if s.i.present(f) {
					size += int64(len(f.Content))
					lfs += int64(len(f.Content))
					present += int64(len(f.Content))
				}
			} else {
				size += int64(len(f.Content))
				present += int64(len(f.Content))
			}
			if q := quantOf(f.Path); q != "" && !seen[q] {
				seen[q] = true
				quants = append(quants, q)
			}
		}
		sort.Strings(quants)
	}
	now := time.Now().UTC()
	out := gen.Repo{Id: repo.ID.String(), Type: gen.RepoTypeSingular(repo.ID.Type), Namespace: repo.ID.Namespace, Name: repo.ID.Name,
		Visibility: repo.Visibility, DefaultBranch: repo.DefaultBranch, Mirror: repo.Mirror, Gated: "", CreatedAt: now, UpdatedAt: now,
		SizeBytes: size, LfsBytes: lfs, PresentBytes: &present}
	if head != nil {
		out.Sha = &head.sha
	}
	if len(quants) > 0 {
		out.Summary = &gen.RepoSummary{Quants: &quants, Formats: &[]string{"gguf"}}
	}
	if repo.Mirror {
		out.MirrorStatus = &gen.MirrorSummary{Upstream: repo.Upstream, UpstreamRepo: repo.ID.String(), Status: "synced"}
	}
	return out
}

func (s *server) GetMe(w http.ResponseWriter, r *http.Request) {
	u := s.i.user(r)
	if u == "" {
		// As Hangar does: nobody signed in is not an error.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, http.StatusOK, gen.Me{Id: uuid.New(), Username: u, DisplayName: strings.ToUpper(u[:1]) + u[1:],
		Orgs: []gen.OrgMembership{{Name: "acme", Role: gen.RoleAdmin}}})
}

func (s *server) ListRepos(w http.ResponseWriter, r *http.Request, params gen.ListReposParams) {
	s.i.mu.Lock()
	defer s.i.mu.Unlock()
	items := []gen.Repo{}
	for _, repo := range s.i.repos {
		if params.Type != nil && repo.ID.Type.Plural() != string(*params.Type) {
			continue
		}
		if params.Q != nil && !strings.Contains(strings.ToLower(repo.ID.String()), strings.ToLower(*params.Q)) {
			continue
		}
		ar := s.apiRepo(repo)
		if params.Quant != nil {
			if ar.Summary == nil || !containsFold(*ar.Summary.Quants, *params.Quant) {
				continue
			}
		}
		items = append(items, ar)
	}
	sort.Slice(items, func(a, b int) bool { return items[a].Id < items[b].Id })
	writeJSON(w, http.StatusOK, gen.RepoList{Items: items})
}

func containsFold(list []string, s string) bool {
	for _, v := range list {
		if strings.EqualFold(v, s) {
			return true
		}
	}
	return false
}

func (s *server) CreateRepo(w http.ResponseWriter, r *http.Request) {
	if !s.authed(w, r) {
		return
	}
	var body gen.RepoCreate
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		problem(w, http.StatusBadRequest, err.Error())
		return
	}
	t, _ := model.ParseRepoType(string(body.Type))
	id := model.RepoID{Type: t, Namespace: body.Namespace, Name: body.Name}
	if err := id.Validate(); err != nil {
		problem(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	s.i.mu.Lock()
	defer s.i.mu.Unlock()
	if s.i.repos[repoKey(id)] != nil {
		problem(w, http.StatusConflict, "A Repo with this ID exists.")
		return
	}
	repo := s.i.newRepo(id, false, "")
	if body.Visibility != nil {
		repo.Visibility = *body.Visibility
	}
	// A new Repo's first Commit holds the Hub's default .gitattributes, as on
	// a real Instance.
	s.i.commitFiles(repo, "main", []File{{Path: ".gitattributes", Content: hangarrepo.DefaultGitAttributes(id.Type)}})
	writeJSON(w, http.StatusCreated, s.apiRepo(repo))
}

func (s *server) GetRepo(w http.ResponseWriter, r *http.Request, t gen.RepoType, ns, name string) {
	s.i.mu.Lock()
	defer s.i.mu.Unlock()
	repo := s.lookup(w, r, repoID(t, ns, name))
	if repo == nil {
		return
	}
	writeJSON(w, http.StatusOK, s.apiRepo(repo))
}

func (s *server) entry(f File) gen.TreeEntry {
	e := gen.TreeEntry{Path: f.Path, Type: gen.File, Present: s.i.present(f)}
	if f.LFS {
		ptr := f.pointer()
		e.Oid, e.Size = gitBlobID(ptr), int64(len(ptr))
		e.Lfs = &struct {
			Oid         string `json:"oid"`
			PointerSize int    `json:"pointer_size"`
			Size        int64  `json:"size"`
		}{Oid: f.sha256(), PointerSize: len(ptr), Size: int64(len(f.Content))}
		return e
	}
	e.Oid, e.Size = gitBlobID(f.Content), int64(len(f.Content))
	return e
}

func (s *server) tree(w http.ResponseWriter, r *http.Request, t gen.RepoType, ns, name, rev, dir string, recursive bool, cursor *string, limit *int) {
	s.i.mu.Lock()
	defer s.i.mu.Unlock()
	repo := s.lookup(w, r, repoID(t, ns, name))
	if repo == nil {
		return
	}
	c := repo.resolveRev(rev)
	if c == nil {
		problem(w, http.StatusNotFound, fmt.Sprintf("Revision %s not found.", rev))
		return
	}
	var items []gen.TreeEntry
	dirs := map[string]bool{}
	prefix := ""
	if dir != "" {
		prefix = strings.TrimSuffix(dir, "/") + "/"
	}
	for _, f := range c.files {
		if !strings.HasPrefix(f.Path, prefix) {
			continue
		}
		rest := strings.TrimPrefix(f.Path, prefix)
		if !recursive && strings.Contains(rest, "/") {
			d := prefix + rest[:strings.IndexByte(rest, '/')]
			if !dirs[d] {
				dirs[d] = true
				items = append(items, gen.TreeEntry{Path: d, Type: gen.Directory, Present: true})
			}
			continue
		}
		items = append(items, s.entry(f))
	}
	sort.Slice(items, func(a, b int) bool { return items[a].Path < items[b].Path })
	start := 0
	if cursor != nil {
		start, _ = strconv.Atoi(*cursor)
	}
	n := 50
	if limit != nil {
		n = *limit
	}
	end := min(start+n, len(items))
	start = min(start, end)
	out := gen.TreeListing{Commit: c.sha, Items: items[start:end]}
	if end < len(items) {
		next := strconv.Itoa(end)
		out.NextCursor = &next
	}
	if out.Items == nil {
		out.Items = []gen.TreeEntry{}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) GetTreeRoot(w http.ResponseWriter, r *http.Request, t gen.RepoType, ns, name, rev string, p gen.GetTreeRootParams) {
	s.tree(w, r, t, ns, name, rev, "", deref(p.Recursive), p.Cursor, p.Limit)
}

func (s *server) GetTree(w http.ResponseWriter, r *http.Request, t gen.RepoType, ns, name, rev, dir string, p gen.GetTreeParams) {
	s.tree(w, r, t, ns, name, rev, dir, deref(p.Recursive), p.Cursor, p.Limit)
}

func deref[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}

// quantGroups groups GGUF files by quant, with projectors.
func (s *server) quantGroups(files map[string]File) []gen.QuantGroup {
	groups := map[string]*gen.QuantGroup{}
	var projectors []string
	for _, f := range files {
		if strings.HasPrefix(path.Base(f.Path), "mmproj-") && path.Ext(f.Path) == ".gguf" {
			projectors = append(projectors, f.Path)
			continue
		}
		q := quantOf(f.Path)
		if q == "" {
			continue
		}
		g := groups[q]
		if g == nil {
			g = &gen.QuantGroup{Name: q, Present: true}
			groups[q] = g
		}
		g.Files = append(g.Files, f.Path)
		g.Size += int64(len(f.Content))
		g.Present = g.Present && s.i.present(f)
	}
	sort.Strings(projectors)
	out := make([]gen.QuantGroup, 0, len(groups))
	for _, g := range groups {
		sort.Strings(g.Files)
		if len(projectors) > 0 {
			g.Projectors = &projectors
		}
		out = append(out, *g)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Name < out[b].Name })
	return out
}

func (s *server) ListQuants(w http.ResponseWriter, r *http.Request, t gen.RepoType, ns, name string, p gen.ListQuantsParams) {
	s.i.mu.Lock()
	defer s.i.mu.Unlock()
	repo := s.lookup(w, r, repoID(t, ns, name))
	if repo == nil {
		return
	}
	rev := repo.DefaultBranch
	if p.Rev != nil {
		rev = *p.Rev
	}
	c := repo.resolveRev(rev)
	if c == nil {
		problem(w, http.StatusNotFound, "Revision not found.")
		return
	}
	writeJSON(w, http.StatusOK, gen.QuantList{Commit: c.sha, Items: s.quantGroups(c.files)})
}

func (s *server) GetDependencies(w http.ResponseWriter, r *http.Request, t gen.RepoType, ns, name string) {
	s.i.mu.Lock()
	defer s.i.mu.Unlock()
	repo := s.lookup(w, r, repoID(t, ns, name))
	if repo == nil {
		return
	}
	d := repo.Dependencies
	if d.Needs == nil {
		d.Needs = []gen.Dependency{}
	}
	if d.NeededBy == nil {
		d.NeededBy = []gen.Dependency{}
	}
	writeJSON(w, http.StatusOK, d)
}

func (s *server) ListDatasetConfigs(w http.ResponseWriter, r *http.Request, ns, name string, _ gen.ListDatasetConfigsParams) {
	s.i.mu.Lock()
	defer s.i.mu.Unlock()
	repo := s.lookup(w, r, repoID(gen.Datasets, ns, name))
	if repo == nil {
		return
	}
	cfgs := repo.Configs
	if cfgs == nil {
		cfgs = []gen.DatasetConfig{}
	}
	writeJSON(w, http.StatusOK, gen.DatasetConfigs{Commit: repo.refs[repo.DefaultBranch], FromCard: false, Configs: cfgs})
}

// lfsRules returns the LFS rules of a Repo's .gitattributes at a branch,
// falling back to the Hub's default for the Repo type, as the real upload
// plan and Commit endpoint do (internal/api/uploads.go).
func (s *server) lfsRules(r *Repo, branch string) *hangarrepo.LFSRules {
	var files map[string]File
	if c := r.commits[r.refs[branch]]; c != nil {
		files = c.files
	}
	return lfsRulesOf(r.ID.Type, files)
}

// lfsRulesOf parses the .gitattributes among files, or the Hub's default.
func lfsRulesOf(t model.RepoType, files map[string]File) *hangarrepo.LFSRules {
	if f, ok := files[".gitattributes"]; ok {
		return hangarrepo.ParseLFSRules(f.Content)
	}
	return hangarrepo.ParseLFSRules(hangarrepo.DefaultGitAttributes(t))
}

func (s *server) PlanUploads(w http.ResponseWriter, r *http.Request, t gen.RepoType, ns, name string) {
	if !s.authed(w, r) {
		return
	}
	var body gen.UploadPlanRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		problem(w, http.StatusBadRequest, err.Error())
		return
	}
	id := repoID(t, ns, name)
	s.i.mu.Lock()
	defer s.i.mu.Unlock()
	repo := s.lookup(w, r, id)
	if repo == nil {
		return
	}
	branch := repo.DefaultBranch
	if body.Revision != nil && *body.Revision != "" {
		branch = *body.Revision
	}
	rules := s.lfsRules(repo, branch)
	plan := gen.UploadPlan{Files: []gen.UploadPlanItem{}}
	for _, f := range body.Files {
		item := gen.UploadPlanItem{Path: f.Path}
		switch {
		case !rules.IsLFS(f.Path) && f.Size <= s.i.InlineMax:
			item.Action = gen.Inline
		case s.i.objects[f.Sha256] != nil:
			item.Action = gen.Exists
		default:
			if repo.Mirror && !s.mirrorWants(repo, f.Sha256) {
				problem(w, http.StatusForbidden, "Mirrors are read-only; only objects the Mirror is missing can be uploaded.")
				return
			}
			item.Action = gen.Upload
			up := s.findUpload(id, f.Sha256)
			if up == nil {
				up = &upload{id: uuid.New(), repo: id, sha: f.Sha256, size: f.Size, parts: map[int][]byte{}}
				s.i.uploads[up.id] = up
			}
			n := int((f.Size + s.i.PartSize - 1) / s.i.PartSize)
			if n == 0 {
				n = 1
			}
			urls := make([]string, n)
			expires := "never"
			if s.i.Presign && s.i.stalePlans > 0 {
				s.i.stalePlans--
				expires = "past"
			}
			for k := range urls {
				if s.i.Presign {
					urls[k] = fmt.Sprintf("%s/presigned/%s/%d?sig=fake&expires=%s", s.i.PresignBase, up.id, k+1, expires)
				} else {
					urls[k] = fmt.Sprintf("%s/api/hangar/v1/repos/%s/%s/%s/uploads/%s/parts/%d", s.i.URL, t, url.PathEscape(ns), url.PathEscape(name), up.id, k+1)
				}
			}
			item.UploadId, item.PartUrls, item.PartSize, item.Presigned = &up.id, &urls, &s.i.PartSize, &s.i.Presign
		}
		plan.Files = append(plan.Files, item)
	}
	writeJSON(w, http.StatusOK, plan)
}

// mirrorWants reports whether a Mirror references an object it lacks.
func (s *server) mirrorWants(repo *Repo, sha string) bool {
	for _, c := range repo.commits {
		for _, f := range c.files {
			if f.LFS && f.sha256() == sha {
				return true
			}
		}
	}
	return false
}

func (s *server) findUpload(id model.RepoID, sha string) *upload {
	for _, u := range s.i.uploads {
		if u.repo == id && u.sha == sha && !u.done {
			return u
		}
	}
	return nil
}

func (s *server) status(up *upload) gen.UploadStatus {
	st := gen.UploadStatus{Id: up.id, Sha256: up.sha, Size: up.size, PartSize: &s.i.PartSize, State: gen.Uploading, Parts: []gen.UploadedPart{}}
	n := int((up.size + s.i.PartSize - 1) / s.i.PartSize)
	st.PartCount = n
	nums := make([]int, 0, len(up.parts))
	for k := range up.parts {
		nums = append(nums, k)
	}
	sort.Ints(nums)
	for _, k := range nums {
		size := int64(len(up.parts[k]))
		st.Parts = append(st.Parts, gen.UploadedPart{Number: k, Etag: etagOf(up.parts[k]), Size: &size})
	}
	if up.done {
		st.State = gen.Verified
	}
	return st
}

func etagOf(b []byte) string {
	s := sha256.Sum256(b)
	return `"` + hex.EncodeToString(s[:8]) + `"`
}

func (s *server) GetUpload(w http.ResponseWriter, r *http.Request, _ gen.RepoType, _, _ string, id gen.ID) {
	if !s.authed(w, r) {
		return
	}
	s.i.mu.Lock()
	defer s.i.mu.Unlock()
	up := s.i.uploads[id]
	if up != nil {
		if content, ok := s.i.landing[up.sha]; ok {
			// Another upload of the same object completed first.
			delete(s.i.landing, up.sha)
			s.i.objects[up.sha] = content
			delete(s.i.uploads, up.id)
			up = nil
		}
	}
	if up == nil {
		problem(w, http.StatusNotFound, "The upload does not exist or has expired. Plan the upload again.")
		return
	}
	writeJSON(w, http.StatusOK, s.status(up))
}

// resetPart reads half of a part's body and closes the connection without
// an answer, if ResetPart asked for it, and reports whether it did.
func (i *Instance) resetPart(w http.ResponseWriter, r *http.Request, id uuid.UUID, n int) bool {
	i.mu.Lock()
	up := i.uploads[id]
	reset := false
	if up != nil {
		key := fmt.Sprintf("%s/%d", up.sha, n)
		if f := i.failedParts[key]; f.times > 0 && f.reset {
			f.times--
			i.failedParts[key] = f
			reset = true
		}
	}
	i.mu.Unlock()
	if !reset {
		return false
	}
	_, _ = io.CopyN(io.Discard, r.Body, max(r.ContentLength/2, 1))
	hangUp(w)
	return true
}

// hangUp closes the connection without an answer.
func hangUp(w http.ResponseWriter) {
	conn, _, err := http.NewResponseController(w).Hijack()
	if err != nil {
		panic(http.ErrAbortHandler)
	}
	_ = conn.Close()
}

// storePart stores a part, or fails it if FailPart or FailPartLast asked
// to.
func (i *Instance) storePart(w http.ResponseWriter, r *http.Request, id uuid.UUID, n int, body []byte) {
	i.mu.Lock()
	defer i.mu.Unlock()
	up := i.uploads[id]
	if up == nil {
		problem(w, http.StatusNotFound, "Upload not found.")
		return
	}
	key := fmt.Sprintf("%s/%d", up.sha, n)
	if f := i.failedParts[key]; f.times > 0 && !f.reset {
		f.times--
		i.failedParts[key] = f
		if f.afterOthers {
			i.waitForOtherParts(r, up, n)
		}
		status := f.status
		if status == 0 {
			status = http.StatusInternalServerError
		}
		problem(w, status, "Injected part failure.")
		return
	}
	up.parts[n] = body
	i.partPuts++
	close(i.partStored)
	i.partStored = make(chan struct{})
	w.Header().Set("ETag", etagOf(body))
	size := int64(len(body))
	writeJSON(w, http.StatusOK, gen.UploadedPart{Number: n, Etag: etagOf(body), Size: &size})
}

// waitForOtherParts returns once every part of up other than n is stored,
// the request ends or PartFailureWait has passed. i.mu is held on entry and
// on return.
func (i *Instance) waitForOtherParts(r *http.Request, up *upload, n int) {
	total := int((up.size + i.PartSize - 1) / i.PartSize)
	timeout := time.NewTimer(PartFailureWait)
	defer timeout.Stop()
	for {
		others := len(up.parts)
		if _, ok := up.parts[n]; ok {
			others--
		}
		if others >= total-1 {
			return
		}
		stored := i.partStored
		i.mu.Unlock()
		select {
		case <-stored:
			i.mu.Lock()
		case <-r.Context().Done():
			i.mu.Lock()
			return
		case <-timeout.C:
			i.mu.Lock()
			return
		}
	}
}

func (s *server) PutUploadPart(w http.ResponseWriter, r *http.Request, _ gen.RepoType, _, _ string, id gen.ID, n int) {
	if !s.authed(w, r) {
		return
	}
	if s.i.resetPart(w, r, id, n) {
		return
	}
	body, ok := readBody(r)
	if !ok {
		problem(w, http.StatusBadRequest, "Incomplete part body.")
		return
	}
	s.i.storePart(w, r, id, n, body)
}

// presignedObject serves an LFS object at a presigned URL, with Range
// support.
// failDownload answers a download that FailDownload asked to fail, and
// reports whether it did. It counts every download.
func (i *Instance) failDownload(w http.ResponseWriter, file string, content []byte) bool {
	i.mu.Lock()
	i.downloads[file]++
	f, fail := i.failedDownloads[file]
	fail = fail && f.times > 0
	if fail {
		f.times--
		i.failedDownloads[file] = f
	}
	i.mu.Unlock()
	if !fail {
		return false
	}
	if f.status != 0 {
		problem(w, f.status, "Injected download failure.")
		return true
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(content)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(content[:len(content)/2])
	_ = http.NewResponseController(w).Flush()
	hangUp(w)
	return true
}

func (i *Instance) presignedObject(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "" || r.URL.Query().Get("sig") != "fake" {
		problem(w, http.StatusForbidden, "Presigned URLs take no Authorization header.")
		return
	}
	sha := chi.URLParam(r, "sha")
	i.mu.Lock()
	obj, ok := i.objects[sha]
	i.mu.Unlock()
	if !ok {
		problem(w, http.StatusNotFound, "No such object.")
		return
	}
	http.ServeContent(w, r, sha, time.Time{}, bytes.NewReader(obj))
}

func (i *Instance) presignedPart(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "" || r.URL.Query().Get("sig") != "fake" {
		problem(w, http.StatusForbidden, "Presigned URLs take no Authorization header.")
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	n, nerr := strconv.Atoi(chi.URLParam(r, "n"))
	if err != nil || nerr != nil {
		problem(w, http.StatusBadRequest, "Bad part URL.")
		return
	}
	if r.URL.Query().Get("expires") == "past" {
		problem(w, http.StatusForbidden, "Request has expired.")
		return
	}
	if i.resetPart(w, r, id, n) {
		return
	}
	body, ok := readBody(r)
	if !ok {
		problem(w, http.StatusBadRequest, "Incomplete part body.")
		return
	}
	i.storePart(w, r, id, n, body)
}

func (s *server) CompleteUpload(w http.ResponseWriter, r *http.Request, _ gen.RepoType, _, _ string, id gen.ID) {
	if !s.authed(w, r) {
		return
	}
	var body gen.UploadComplete
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		problem(w, http.StatusBadRequest, err.Error())
		return
	}
	if d := s.i.takeSlowCompletion(id); d > 0 {
		t := time.NewTimer(d)
		select {
		case <-t.C:
		case <-r.Context().Done():
		}
		t.Stop()
	}
	s.i.mu.Lock()
	defer s.i.mu.Unlock()
	up := s.i.uploads[id]
	if up == nil {
		problem(w, http.StatusNotFound, "Upload not found.")
		return
	}
	if f := s.i.failedCompletes[up.sha]; f.times > 0 {
		f.times--
		s.i.failedCompletes[up.sha] = f
		if f.retryAfter != "" {
			w.Header().Set("Retry-After", f.retryAfter)
		}
		problem(w, http.StatusServiceUnavailable, "The request failed for a reason that may pass. Nothing was lost; send the same request again.")
		return
	}
	var all bytes.Buffer
	for k, p := range body.Parts {
		if p.Number != k+1 || etagOf(up.parts[p.Number]) != p.Etag {
			problem(w, http.StatusBadRequest, fmt.Sprintf("Part %d is missing or its ETag does not match.", p.Number))
			return
		}
		all.Write(up.parts[p.Number])
	}
	sum := sha256.Sum256(all.Bytes())
	if hex.EncodeToString(sum[:]) != up.sha || int64(all.Len()) != up.size {
		problem(w, http.StatusUnprocessableEntity, "The SHA-256 or size does not match.")
		return
	}
	s.i.objects[up.sha] = all.Bytes()
	up.done = true
	// Like Hangar, the completed upload is removed, so a later request for
	// it answers 404.
	delete(s.i.uploads, up.id)
	if n := s.i.droppedCompletes[up.sha]; n > 0 {
		s.i.droppedCompletes[up.sha] = n - 1
		hangUp(w)
		return
	}
	writeJSON(w, http.StatusOK, s.status(up))
}

func (s *server) CreateCommit(w http.ResponseWriter, r *http.Request, t gen.RepoType, ns, name string) {
	if !s.authed(w, r) {
		return
	}
	var body gen.CommitRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		problem(w, http.StatusBadRequest, err.Error())
		return
	}
	s.i.mu.Lock()
	defer s.i.mu.Unlock()
	repo := s.lookup(w, r, repoID(t, ns, name))
	if repo == nil {
		return
	}
	if repo.Mirror {
		problem(w, http.StatusForbidden, "Mirrors are read-only.")
		return
	}
	branch := repo.DefaultBranch
	if body.Branch != nil && *body.Branch != "" {
		branch = *body.Branch
	}
	// Like the real Commit endpoint, a Commit goes only to a branch that
	// exists; the default branch of a Repo with no Commits yet is the
	// exception.
	if _, ok := repo.refs[branch]; !ok && branch != repo.DefaultBranch {
		problem(w, http.StatusNotFound, "revision not found: "+branch)
		return
	}
	files := map[string]File{}
	parent := repo.commits[repo.refs[branch]]
	if parent == nil {
		parent = repo.commits[repo.refs[repo.DefaultBranch]]
	}
	if parent != nil {
		for p, f := range parent.files {
			files[p] = f
		}
	}
	// Like the real Commit endpoint, an inline file that .gitattributes puts
	// in LFS is stored as an LFS object.
	rules := lfsRulesOf(repo.ID.Type, files)
	for _, op := range body.Operations {
		switch op.Op {
		case gen.Add:
			b, err := base64.StdEncoding.DecodeString(deref(op.ContentBase64))
			if err != nil {
				problem(w, http.StatusBadRequest, "Bad base64.")
				return
			}
			f := File{Path: op.Path, Content: b, LFS: rules.IsLFS(op.Path)}
			if f.LFS {
				s.i.objects[f.sha256()] = b
			}
			files[op.Path] = f
		case gen.AddLfs:
			obj := s.i.objects[deref(op.Sha256)]
			if obj == nil || int64(len(obj)) != deref(op.Size) {
				problem(w, http.StatusBadRequest, fmt.Sprintf("LFS object for %s is not stored.", op.Path))
				return
			}
			files[op.Path] = File{Path: op.Path, Content: obj, LFS: true}
		case gen.Delete:
			delete(files, op.Path)
		default:
			problem(w, http.StatusBadRequest, "Unsupported operation.")
			return
		}
	}
	list := make([]File, 0, len(files))
	for _, f := range files {
		list = append(list, f)
	}
	before := repo.refs[branch]
	sha := s.i.commitFiles(repo, branch, list)
	resp := gen.CommitResponse{Sha: sha, Ref: "refs/heads/" + branch}
	if sha == before {
		resp.NoOp = ptr(true)
	}
	if deref(body.CreatePr) {
		resp.PullRequest = ptr(1)
	}
	writeJSON(w, http.StatusCreated, resp)
}

func ptr[T any](v T) *T { return &v }

func (s *server) arsenal(id model.RepoID) (gen.ArsenalState, *Repo) {
	repo := s.i.repos[repoKey(id)]
	if repo == nil {
		return gen.ArsenalStateNone, nil
	}
	c := repo.commits[repo.refs[repo.DefaultBranch]]
	if c == nil {
		return gen.ArsenalStateNone, repo
	}
	for _, f := range c.files {
		if !s.i.present(f) {
			return gen.ArsenalStatePartial, repo
		}
	}
	return gen.ArsenalStateFull, repo
}

func (s *server) Discover(w http.ResponseWriter, r *http.Request, t gen.RepoType, p gen.DiscoverParams) {
	s.i.mu.Lock()
	defer s.i.mu.Unlock()
	items := []gen.DiscoverItem{}
	for _, u := range s.i.upstream {
		if u.ID.Type.Plural() != string(t) {
			continue
		}
		if p.Search != nil && !strings.Contains(strings.ToLower(u.ID.String()), strings.ToLower(*p.Search)) {
			continue
		}
		st, _ := s.arsenal(u.ID)
		items = append(items, gen.DiscoverItem{Id: u.ID.String(), Type: gen.RepoTypeSingular(u.ID.Type), Arsenal: st, Downloads: ptr(int64(42))})
	}
	sort.Slice(items, func(a, b int) bool { return items[a].Id < items[b].Id })
	writeJSON(w, http.StatusOK, gen.DiscoverList{Items: items})
}

func (s *server) GetDiscoverDetail(w http.ResponseWriter, r *http.Request, t gen.RepoType, ns, name string, _ gen.GetDiscoverDetailParams) {
	id := repoID(t, ns, name)
	s.i.mu.Lock()
	defer s.i.mu.Unlock()
	u := s.i.upstream[repoKey(id)]
	if u == nil {
		problem(w, http.StatusNotFound, "Not on the Upstream.")
		return
	}
	st, _ := s.arsenal(id)
	d := gen.DiscoverDetail{Id: id.String(), Type: gen.RepoTypeSingular(id.Type), Arsenal: st, Files: []gen.DiscoverFile{}, Dependencies: []gen.Dependency{}}
	files := map[string]File{}
	for _, f := range u.Revisions["main"] {
		files[f.Path] = f
		d.Files = append(d.Files, gen.DiscoverFile{Path: f.Path, Size: int64(len(f.Content)), Lfs: f.LFS, Present: f.LFS && s.i.objects[f.sha256()] != nil || !f.LFS && st != gen.ArsenalStateNone})
		d.TotalSize += int64(len(f.Content))
	}
	if q := s.quantGroups(files); len(q) > 0 {
		d.Quants = &q
	}
	writeJSON(w, http.StatusOK, d)
}

func (s *server) newJob(user string, payload map[string]any, total int64, finish func() error) gen.Job {
	j := &job{j: gen.Job{Id: uuid.New(), Kind: "pull", Status: gen.JobStatusPending, MaxAttempts: 5,
		Progress: gen.JobProgress{BytesTotal: total}, CreatedAt: time.Now().UTC(), Payload: &payload, CreatedBy: &user},
		finish: finish, created: time.Now()}
	s.i.jobs[j.j.Id] = j
	s.i.jobOrder = append(s.i.jobOrder, j.j.Id)
	return j.j
}

// pullFn returns the work a Pull does when its Job finishes: commit the
// upstream history at each Revision into the Mirror and, as Hangar does,
// store the selected LFS objects of the Revision pulled only. A Commit SHA
// names a Revision added with AddUpstreamRevision under that SHA, or else
// the Commit of main. Like Hangar, a Pull of paths that do not exist at its
// Revision fails. Lock held when called.
func (s *server) pullFn(u *UpstreamRepo, rev string, sel *gen.Selection) (func() error, int64) {
	target := "main"
	revs := []string{"main"}
	if _, known := u.Revisions[rev]; rev != "" && rev != "main" && (known || !model.IsFullSHA(rev)) {
		target = rev
		revs = append(revs, rev)
	}
	parquet := sel != nil && deref(sel.ParquetOnly)
	if parquet {
		revs = append(revs, "refs/convert/parquet")
	}
	take := func(rv string, f File) bool {
		if !f.LFS {
			return false
		}
		if parquet {
			return rv == "refs/convert/parquet" && parquetSelected(sel, f)
		}
		return rv == target && selected(sel, f)
	}
	var total int64
	for _, rv := range revs {
		for _, f := range u.Revisions[rv] {
			if take(rv, f) {
				total += int64(len(f.Content))
			}
		}
	}
	return func() error {
		if s.i.FailPulls != "" {
			return fmt.Errorf("%s", s.i.FailPulls)
		}
		if !parquet && sel != nil && sel.Paths != nil {
			have := map[string]bool{}
			for _, f := range u.Revisions[target] {
				have[f.Path] = true
			}
			for _, p := range *sel.Paths {
				if !have[p] {
					return fmt.Errorf("invalid selection: %s does not exist at %s", p, target)
				}
			}
		}
		repo := s.i.repos[repoKey(u.ID)]
		if repo == nil {
			repo = s.i.newRepo(u.ID, true, "huggingface")
		}
		for _, rv := range revs {
			files, ok := u.Revisions[rv]
			if !ok {
				continue
			}
			for _, f := range files {
				if take(rv, f) {
					s.i.objects[f.sha256()] = f.Content
				}
			}
			if model.IsFullSHA(rv) {
				s.i.commitAt(repo, rv, files)
				continue
			}
			sha := s.i.commitFiles(repo, rv, files)
			if rv == "main" && target == "main" && model.IsFullSHA(rev) {
				repo.refs[rev] = sha
			}
		}
		return nil
	}, total
}

// parquetSelected applies Config and Split selections to Parquet export
// files laid out as <config>/<split>/<shard>.parquet.
func parquetSelected(sel *gen.Selection, f File) bool {
	parts := strings.Split(f.Path, "/")
	if len(parts) < 3 {
		return false
	}
	if sel.Configs != nil && !containsFold(*sel.Configs, parts[0]) {
		return false
	}
	if sel.Splits != nil && !containsFold(*sel.Splits, parts[1]) {
		return false
	}
	return true
}

func (s *server) StartPull(w http.ResponseWriter, r *http.Request) {
	if !s.authed(w, r) {
		return
	}
	var body gen.PullCreate
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		problem(w, http.StatusBadRequest, err.Error())
		return
	}
	t, _ := model.ParseRepoType(string(body.Type))
	id, err := model.ParseRepoID(t, body.Repo)
	if err != nil {
		problem(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	s.i.mu.Lock()
	defer s.i.mu.Unlock()
	u := s.i.upstream[repoKey(id)]
	if u == nil {
		problem(w, http.StatusNotFound, fmt.Sprintf("%s is not on %s.", id.Path(), body.Upstream))
		return
	}
	fn, total := s.pullFn(u, deref(body.Revision), body.Selection)
	writeJSON(w, http.StatusAccepted, s.newJob(s.i.Users[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")], map[string]any{"repo": id.String()}, total, fn))
}

func (s *server) PullMoreIntoMirror(w http.ResponseWriter, r *http.Request, t gen.RepoType, ns, name string, params gen.PullMoreIntoMirrorParams) {
	if !s.authed(w, r) {
		return
	}
	var sel gen.Selection
	if err := json.NewDecoder(r.Body).Decode(&sel); err != nil {
		problem(w, http.StatusBadRequest, err.Error())
		return
	}
	id := repoID(t, ns, name)
	s.i.mu.Lock()
	defer s.i.mu.Unlock()
	repo := s.lookup(w, r, id)
	if repo == nil {
		return
	}
	if !repo.Mirror {
		problem(w, http.StatusConflict, "Not a Mirror.")
		return
	}
	u := s.i.upstream[repoKey(id)]
	fn, total := s.pullFn(u, deref(params.Revision), &sel)
	writeJSON(w, http.StatusAccepted, s.newJob("alice", map[string]any{"repo": id.String()}, total, fn))
}

// advance moves a Job on by one poll. Lock held.
func (s *server) advance(j *job) {
	if j.j.Status == gen.JobStatusCancelled || j.j.Status == gen.JobStatusSucceeded || j.j.Status == gen.JobStatusFailed {
		return
	}
	j.polls++
	if j.polls < s.i.JobPolls {
		j.j.Status = gen.JobStatusRunning
		j.j.Progress.BytesDone = j.j.Progress.BytesTotal / 2
		j.j.Progress.Rate = 1 << 20
		j.j.Progress.EtaSeconds = ptr(1)
		return
	}
	j.j.Attempts = 1
	if err := j.finish(); err != nil {
		j.j.Status = gen.JobStatusFailed
		j.j.Error = ptr(err.Error())
		return
	}
	j.j.Status = gen.JobStatusSucceeded
	j.j.Progress.BytesDone = j.j.Progress.BytesTotal
}

func (s *server) GetJob(w http.ResponseWriter, r *http.Request, id gen.ID) {
	if !s.authed(w, r) {
		return
	}
	s.i.mu.Lock()
	defer s.i.mu.Unlock()
	j := s.i.jobs[id]
	if j == nil {
		problem(w, http.StatusNotFound, "Job not found.")
		return
	}
	s.advance(j)
	writeJSON(w, http.StatusOK, j.j)
}

func (s *server) GetPull(w http.ResponseWriter, r *http.Request, id gen.ID) {
	s.GetJob(w, r, id)
}

func (s *server) ListJobs(w http.ResponseWriter, r *http.Request, p gen.ListJobsParams) {
	if !s.authed(w, r) {
		return
	}
	s.i.mu.Lock()
	defer s.i.mu.Unlock()
	items := []gen.Job{}
	for k := len(s.i.jobOrder) - 1; k >= 0; k-- {
		j := s.i.jobs[s.i.jobOrder[k]].j
		if p.Status != nil && j.Status != *p.Status || p.Kind != nil && j.Kind != *p.Kind {
			continue
		}
		items = append(items, j)
	}
	writeJSON(w, http.StatusOK, gen.JobList{Items: items})
}

func (s *server) CancelJob(w http.ResponseWriter, r *http.Request, id gen.ID) {
	if !s.authed(w, r) {
		return
	}
	s.i.mu.Lock()
	defer s.i.mu.Unlock()
	j := s.i.jobs[id]
	if j == nil {
		problem(w, http.StatusNotFound, "Job not found.")
		return
	}
	if j.j.Status == gen.JobStatusPending || j.j.Status == gen.JobStatusRunning {
		j.j.Status = gen.JobStatusCancelled
	}
	writeJSON(w, http.StatusOK, j.j)
}

func (s *server) ListMyOllamaKeys(w http.ResponseWriter, r *http.Request) {
	if !s.authed(w, r) {
		return
	}
	writeJSON(w, http.StatusOK, gen.OllamaKeyList{Items: s.i.OllamaKeys()})
}

// ollamaKeyNamespace is the signature namespace of Ollama key challenges,
// as auth.OllamaKeyNamespace.
const ollamaKeyNamespace = "hangar-ollama-key"

func (s *server) CreateOllamaKeyChallenge(w http.ResponseWriter, r *http.Request) {
	if !s.authed(w, r) {
		return
	}
	c := "hangar-ollama-key:clitest:" + uuid.NewString()
	s.i.mu.Lock()
	s.i.keyChallenges[c] = true
	s.i.mu.Unlock()
	writeJSON(w, http.StatusOK, gen.OllamaKeyChallenge{Challenge: c, Namespace: ollamaKeyNamespace, ExpiresAt: time.Now().Add(15 * time.Minute).UTC()})
}

func (s *server) AddMyOllamaKey(w http.ResponseWriter, r *http.Request) {
	if !s.authed(w, r) {
		return
	}
	var body gen.OllamaKeyCreate
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		problem(w, http.StatusBadRequest, err.Error())
		return
	}
	pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(body.PublicKey))
	if err != nil {
		problem(w, http.StatusUnprocessableEntity, "Not an ssh-ed25519 public key.")
		return
	}
	s.i.mu.Lock()
	issued := s.i.keyChallenges[body.Challenge]
	s.i.mu.Unlock()
	if !issued || sshsig.Verify(body.Signature, pub, ollamaKeyNamespace, []byte(body.Challenge)) != nil {
		problem(w, http.StatusUnprocessableEntity, "Sign a new challenge with the private key of this Ollama key.")
		return
	}
	k := gen.OllamaKey{Id: uuid.New(), Name: body.Name, Fingerprint: ssh.FingerprintSHA256(pub), CreatedAt: time.Now().UTC()}
	s.i.mu.Lock()
	s.i.ollamaKeys = append(s.i.ollamaKeys, k)
	s.i.mu.Unlock()
	writeJSON(w, http.StatusCreated, k)
}

func (i *Instance) oauthDevice(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil || r.PostForm.Get("client_id") == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_request"})
		return
	}
	i.mu.Lock()
	code := "dev-" + uuid.NewString()
	userCode := fmt.Sprintf("ABCD-%04d", len(i.devices)+1)
	i.devices[code] = &device{userCode: userCode}
	i.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"device_code": code, "user_code": userCode, "verification_uri": i.URL + "/settings/device",
		"verification_uri_complete": i.URL + "/settings/device?code=" + userCode, "interval": 1, "expires_in": 60,
	})
}

func (i *Instance) oauthToken(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_request"})
		return
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	d := i.devices[r.PostForm.Get("device_code")]
	switch {
	case d == nil:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "expired_token"})
	case d.denied:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "access_denied"})
	case d.approved || i.DeviceAutoApprove:
		tok := "hgr_device_" + strings.ReplaceAll(uuid.NewString(), "-", "")
		i.Users[tok] = "alice"
		delete(i.devices, r.PostForm.Get("device_code"))
		writeJSON(w, http.StatusOK, map[string]any{"access_token": tok, "token_type": "bearer"})
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "authorization_pending"})
	}
}

// DenyDevice denies a pending device login by user code.
func (i *Instance) DenyDevice(userCode string) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	for _, d := range i.devices {
		if d.userCode == userCode {
			d.denied = true
			return true
		}
	}
	return false
}

// resolve serves Hub file downloads with the headers huggingface_hub reads
// and Range support.
func (i *Instance) resolve(t model.RepoType) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := model.RepoID{Type: t, Namespace: chi.URLParam(r, "ns"), Name: chi.URLParam(r, "name")}
		rev, _ := url.PathUnescape(chi.URLParam(r, "rev"))
		file, _ := url.PathUnescape(chi.URLParam(r, "*"))
		i.mu.Lock()
		repo := i.repos[repoKey(id)]
		var c *commit
		if repo != nil {
			c = repo.resolveRev(rev)
		}
		var f File
		ok := false
		if c != nil {
			f, ok = c.files[file]
		}
		present := ok && i.present(f)
		redirect := i.RedirectDownloads
		base := i.PresignBase
		i.mu.Unlock()
		if repo == nil || c == nil {
			w.Header().Set("X-Error-Code", "RepoNotFound")
			w.Header().Set("X-Error-Message", "Repository not found")
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("X-Repo-Commit", c.sha)
		if !ok || !present {
			w.Header().Set("X-Error-Code", "EntryNotFound")
			w.Header().Set("X-Error-Message", "Entry not found")
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		etag := gitBlobID(f.Content)
		if f.LFS {
			etag = f.sha256()
		}
		w.Header().Set("ETag", `"`+etag+`"`)
		w.Header().Set("X-Linked-Etag", `"`+etag+`"`)
		if i.failDownload(w, file, f.Content) {
			return
		}
		if redirect && f.LFS {
			w.Header().Set("X-Linked-Size", strconv.Itoa(len(f.Content)))
			http.Redirect(w, r, base+"/presigned-objects/"+etag+"?sig=fake", http.StatusFound)
			return
		}
		http.ServeContent(w, r, path.Base(file), time.Time{}, bytes.NewReader(f.Content))
	}
}
