package api

import (
	"bytes"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/jamesagarside/hangar/internal/api/gen"
	"github.com/jamesagarside/hangar/internal/auth"
	"github.com/jamesagarside/hangar/internal/blob"
	"github.com/jamesagarside/hangar/internal/community"
	"github.com/jamesagarside/hangar/internal/httpx"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/repo"
)

// UploadsAPI serves uploads and Commits from the browser and the CLI
// (UP-3, UP-4 and UP-6): plan, upload parts straight to the Blob store or
// through Hangar, complete, then commit.
type UploadsAPI struct{ d RepoDeps }

// NewUploadsAPI returns the upload handlers.
func NewUploadsAPI(d RepoDeps) (*UploadsAPI, error) {
	if err := d.validate(); err != nil {
		return nil, err
	}
	return &UploadsAPI{d: d}, nil
}

// maxPlanFiles is the most files one plan may cover (the OpenAPI limit).
const maxPlanFiles = 1000

// PlanUploads implements gen.ServerInterface. Files that .gitattributes
// leaves out of LFS and that are small enough go inline in the Commit;
// stored objects are skipped (UP-6); the rest get a multipart upload.
func (a *UploadsAPI) PlanUploads(w http.ResponseWriter, r *http.Request, pType gen.RepoType, ns gen.Ns, name gen.Name) {
	rp, err := a.d.load(r, pType, ns, name, auth.ActionWrite)
	if err != nil {
		a.d.problem(w, r, err)
		return
	}
	var body gen.UploadPlanRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	if len(body.Files) > maxPlanFiles {
		badRequest(w, r, "", "Plan at most 1,000 files at a time.")
		return
	}
	rules, err := a.lfsRules(r, rp, body.Revision)
	if err != nil {
		a.d.problem(w, r, err)
		return
	}
	oids := make([]string, 0, len(body.Files))
	for i, f := range body.Files {
		clean, err := model.CleanRepoPath(f.Path)
		if err != nil || clean == "" {
			badRequest(w, r, "", "files["+strconv.Itoa(i)+"].path is not a valid path in a Repo.")
			return
		}
		if !model.IsSHA256(f.Sha256) || f.Size < 0 {
			badRequest(w, r, "", "files["+strconv.Itoa(i)+"] needs a lower-case sha256 and a size of 0 or more.")
			return
		}
		body.Files[i].Path = clean
		oids = append(oids, f.Sha256)
	}
	stored, err := a.d.LFS.StatMany(r.Context(), oids)
	if err != nil {
		a.d.problem(w, r, err)
		return
	}
	p := httpx.Principal(r.Context())
	out := gen.UploadPlan{Files: make([]gen.UploadPlanItem, 0, len(body.Files))}
	for _, f := range body.Files {
		item := gen.UploadPlanItem{Path: f.Path}
		switch {
		case !rules.IsLFS(f.Path) && f.Size <= a.d.InlineMax:
			item.Action = "inline"
		case stored[f.Sha256].Verified && stored[f.Sha256].Size == f.Size:
			item.Action = "exists"
		default:
			ps := repo.PartSize(f.Size)
			// Resume continues the caller's upload of the same object in
			// this Repo, so planning again after a reload reuses its parts.
			u, err := a.d.LFS.CreateUpload(r.Context(), repo.UploadParams{OID: f.Sha256, Size: f.Size, RepoUUID: rp.UUID, UserID: p.UserID,
				Multipart: true, PartSize: ps, TTL: a.d.UploadURLTTL, Resume: true})
			if err != nil {
				a.d.problem(w, r, err)
				return
			}
			urls, presigned, err := a.partURLs(r, rp, u)
			if err != nil {
				a.d.problem(w, r, err)
				return
			}
			item.Action = "upload"
			item.UploadId = &u.ID
			item.PartSize = &ps
			item.PartUrls = &urls
			item.Presigned = &presigned
		}
		out.Files = append(out.Files, item)
	}
	httpx.WriteJSON(w, r, http.StatusOK, out)
}

// lfsRules reads .gitattributes at the revision, falling back to the
// Hub's defaults when the file or the branch does not exist.
func (a *UploadsAPI) lfsRules(r *http.Request, rp *repo.Repo, revision *string) (*repo.LFSRules, error) {
	rev := ""
	if revision != nil {
		rev = *revision
	}
	sha, err := a.d.Repos.Resolve(r.Context(), rp.ID, rev)
	if errors.Is(err, repo.ErrRevisionNotFound) {
		return repo.ParseLFSRules(repo.DefaultGitAttributes(rp.ID.Type)), nil
	}
	if err != nil {
		return nil, err
	}
	rc, e, err := a.d.Repos.OpenFile(r.Context(), rp.ID, sha, ".gitattributes", nil)
	if errors.Is(err, repo.ErrEntryNotFound) || errors.Is(err, repo.ErrNotPresent) {
		return repo.ParseLFSRules(repo.DefaultGitAttributes(rp.ID.Type)), nil
	}
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, max(e.Size, 0)+1))
	if err != nil {
		return nil, err
	}
	return repo.ParseLFSRules(b), nil
}

// partURLs returns presigned part URLs, or Hangar part URLs when the Blob
// store cannot presign.
func (a *UploadsAPI) partURLs(r *http.Request, rp *repo.Repo, u *repo.Upload) ([]string, bool, error) {
	count := max(u.PartCount, 1)
	urls := make([]string, 0, count)
	for n := 1; n <= count; n++ {
		url, err := a.d.LFS.PresignPart(r.Context(), u.ID, n, a.d.UploadURLTTL)
		if errors.Is(err, blob.ErrNotSupported) {
			urls = urls[:0]
			base := a.d.PublicURL + "/api/hangar/v1/repos/" + rp.ID.Type.Plural() + "/" + rp.ID.String() + "/uploads/" + u.ID.String() + "/parts/"
			for m := 1; m <= count; m++ {
				urls = append(urls, base+strconv.Itoa(m))
			}
			return urls, false, nil
		}
		if err != nil {
			return nil, false, err
		}
		urls = append(urls, url)
	}
	return urls, true, nil
}

// upload loads an upload and checks it belongs to the Repo in the path.
func (a *UploadsAPI) upload(r *http.Request, rp *repo.Repo, id gen.ID) (*repo.Upload, error) {
	owner, err := a.d.LFS.UploadRepo(r.Context(), id)
	if err != nil {
		return nil, err
	}
	if owner != rp.UUID {
		return nil, repo.ErrNotFound
	}
	return a.d.LFS.GetUpload(r.Context(), id)
}

func toStatus(u *repo.Upload, verified bool) gen.UploadStatus {
	out := gen.UploadStatus{Id: u.ID, Sha256: u.OID, Size: u.Size, PartCount: u.PartCount, Parts: []gen.UploadedPart{}, State: "uploading"}
	if u.PartSize > 0 {
		out.PartSize = i64(u.PartSize)
	}
	if !u.ExpiresAt.IsZero() {
		ea := u.ExpiresAt
		out.ExpiresAt = &ea
	}
	for _, p := range u.Done {
		out.Parts = append(out.Parts, gen.UploadedPart{Number: p.Number, Etag: p.ETag, Size: i64(p.Size)})
	}
	if verified {
		out.State = "verified"
	}
	return out
}

// GetUpload implements gen.ServerInterface: the parts done so far, so a
// client can resume.
func (a *UploadsAPI) GetUpload(w http.ResponseWriter, r *http.Request, pType gen.RepoType, ns gen.Ns, name gen.Name, id gen.ID) {
	rp, err := a.d.load(r, pType, ns, name, auth.ActionWrite)
	if err != nil {
		a.d.problem(w, r, err)
		return
	}
	u, err := a.upload(r, rp, id)
	if err != nil {
		a.uploadProblem(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, toStatus(u, false))
}

func (a *UploadsAPI) uploadProblem(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, repo.ErrNotFound) {
		httpx.WriteProblem(w, r, &httpx.Problem{Status: http.StatusNotFound, Detail: "The upload does not exist or has expired. Plan the upload again."})
		return
	}
	a.d.problem(w, r, err)
}

// PutUploadPart implements gen.ServerInterface. The body streams into the
// Blob store; it is never held whole in memory.
func (a *UploadsAPI) PutUploadPart(w http.ResponseWriter, r *http.Request, pType gen.RepoType, ns gen.Ns, name gen.Name, id gen.ID, n int) {
	rp, err := a.d.load(r, pType, ns, name, auth.ActionWrite)
	if err != nil {
		a.d.problem(w, r, err)
		return
	}
	if _, err := a.upload(r, rp, id); err != nil {
		a.uploadProblem(w, r, err)
		return
	}
	size := r.ContentLength
	etag, err := a.d.LFS.PutPart(r.Context(), id, n, r.Body, size)
	if err != nil {
		a.uploadProblem(w, r, err)
		return
	}
	w.Header().Set("ETag", `"`+strings.Trim(etag, `"`)+`"`)
	out := gen.UploadedPart{Number: n, Etag: etag}
	if size >= 0 {
		out.Size = i64(size)
	}
	httpx.WriteJSON(w, r, http.StatusOK, out)
}

// CompleteUpload implements gen.ServerInterface: it assembles the parts and
// checks the SHA-256 and size before the object becomes visible.
func (a *UploadsAPI) CompleteUpload(w http.ResponseWriter, r *http.Request, pType gen.RepoType, ns gen.Ns, name gen.Name, id gen.ID) {
	rp, err := a.d.load(r, pType, ns, name, auth.ActionWrite)
	if err != nil {
		a.d.problem(w, r, err)
		return
	}
	var body gen.UploadComplete
	if !decodeJSON(w, r, &body) {
		return
	}
	u, err := a.upload(r, rp, id)
	if err != nil {
		a.uploadProblem(w, r, err)
		return
	}
	parts := make([]blob.Part, 0, len(body.Parts))
	for _, p := range body.Parts {
		bp := blob.Part{Number: p.Number, ETag: strings.Trim(p.Etag, `"`)}
		if p.Size != nil {
			bp.Size = *p.Size
		}
		parts = append(parts, bp)
	}
	obj, err := a.d.LFS.CompleteUpload(r.Context(), id, parts)
	if err != nil {
		a.uploadProblem(w, r, err)
		return
	}
	u.Done = parts
	u.OID, u.Size = obj.OID, obj.Size
	httpx.WriteJSON(w, r, http.StatusOK, toStatus(u, true))
}

// CreateCommit implements gen.ServerInterface. With create_pr, the Commit
// goes to a new pull request ref, which only needs the discuss permission.
func (a *UploadsAPI) CreateCommit(w http.ResponseWriter, r *http.Request, pType gen.RepoType, ns gen.Ns, name gen.Name) {
	var body gen.CommitRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	createPR := body.CreatePr != nil && *body.CreatePr
	action := auth.ActionWrite
	if createPR {
		action = auth.ActionDiscuss
	}
	rp, err := a.d.load(r, pType, ns, name, action)
	if err != nil {
		a.d.problem(w, r, err)
		return
	}
	p := httpx.Principal(r.Context())
	params := repo.CommitParams{Repo: rp.ID, Summary: body.Summary, Actor: p}
	if body.Description != nil {
		params.Description = *body.Description
	}
	if body.Branch != nil {
		params.Revision = *body.Branch
	}
	if body.ParentCommit != nil {
		params.ParentCommit = model.CommitSHA(*body.ParentCommit)
	}
	for i, op := range body.Operations {
		o := repo.Operation{Kind: repo.OpKind(op.Op), Path: op.Path}
		switch o.Kind {
		case repo.OpAdd:
			content := ""
			if op.ContentBase64 != nil {
				content = *op.ContentBase64
			}
			b, err := base64.StdEncoding.DecodeString(content)
			if err != nil {
				badRequest(w, r, "", "operations["+strconv.Itoa(i)+"].content_base64 is not valid base64.")
				return
			}
			o.Content = bytes.NewReader(b)
		case repo.OpAddLFS:
			if op.Sha256 != nil {
				o.OID = *op.Sha256
			}
			if op.Size != nil {
				o.Size = *op.Size
			}
		case repo.OpCopy:
			if op.SrcPath != nil {
				o.SrcPath = *op.SrcPath
			}
			if op.SrcRevision != nil {
				o.SrcRevision = *op.SrcRevision
			}
		}
		params.Operations = append(params.Operations, o)
	}
	if err := repo.ValidateCommitParams(params); err != nil {
		a.d.problem(w, r, err)
		return
	}
	out := gen.CommitResponse{}
	if createPR {
		if a.d.Community == nil {
			httpx.WriteProblem(w, r, &httpx.Problem{Status: http.StatusServiceUnavailable, Detail: "Pull requests are not available on this server."})
			return
		}
		base := params.Revision
		if base == "" {
			base = rp.DefaultBranch
		}
		from, err := a.d.Repos.Resolve(r.Context(), rp.ID, base)
		if err != nil {
			a.d.problem(w, r, err)
			return
		}
		pr, err := a.d.Community.Create(r.Context(), community.CreateParams{RepoUUID: rp.UUID, Repo: rp.ID, Title: body.Summary,
			Description: params.Description, PullRequest: true, Base: base, Author: p})
		if err != nil {
			a.d.problem(w, r, err)
			return
		}
		params.Revision, params.CreateFrom, params.ParentCommit = pr.PRRef, from, ""
		num := pr.Num
		out.PullRequest = &num
	}
	res, err := a.d.Repos.Commit(r.Context(), params)
	if err != nil {
		a.d.problem(w, r, err)
		return
	}
	out.Sha, out.Ref = string(res.SHA), res.Ref
	if res.NoOp {
		noop := true
		out.NoOp = &noop
	}
	httpx.WriteJSON(w, r, http.StatusCreated, out)
}
