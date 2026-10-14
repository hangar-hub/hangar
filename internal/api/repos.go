// Package api implements the native API (docs/spec/10-native-api.md)
// against the server interface generated from api/openapi.yaml. Each file
// holds one area's handlers on its own type, so workstreams can build them
// separately; the server composes them into one gen.ServerInterface.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/jamesagarside/hangar/internal/api/gen"
	"github.com/jamesagarside/hangar/internal/auth"
	"github.com/jamesagarside/hangar/internal/blob"
	"github.com/jamesagarside/hangar/internal/collections"
	"github.com/jamesagarside/hangar/internal/community"
	"github.com/jamesagarside/hangar/internal/httpx"
	"github.com/jamesagarside/hangar/internal/index"
	"github.com/jamesagarside/hangar/internal/jobs"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/org"
	"github.com/jamesagarside/hangar/internal/repo"
	"github.com/jamesagarside/hangar/internal/scan"
)

// NamespaceSearcher finds Namespaces whose name or display name contains a
// query, for global search (BROWSE-6).
type NamespaceSearcher interface {
	SearchNamespaces(ctx context.Context, query string, limit int) ([]org.Namespace, error)
}

// RepoDeps are the services behind the Repo, upload, search and Namespace
// handlers. Index, Scanner and Community are optional: without them Repo
// responses leave out the index summary, scan status, likes and
// downloads.
type RepoDeps struct {
	Repos       repo.Service
	LFS         repo.LFSStore
	Authz       auth.Authz
	Orgs        org.Service
	Jobs        jobs.Queue
	Collections collections.Service
	// NamespaceSearch serves the namespaces part of /search.
	NamespaceSearch NamespaceSearcher
	Community       community.Service
	Index           index.Indexer
	Scanner         scan.Scanner
	// PublicURL is server.public_url, for Hangar-served part URLs.
	PublicURL string
	// UploadURLTTL is lfs.upload_url_ttl. Zero means 24 hours.
	UploadURLTTL time.Duration
	// InlineMax is the largest file a Commit may carry inline
	// (hfapi.regular_upload_max). Zero means 10 MiB.
	InlineMax int64
	// Facets asks List for facet counts; it needs the index.
	Facets bool
	Log    *slog.Logger
}

func (d *RepoDeps) validate() error {
	switch {
	case d.Repos == nil, d.LFS == nil, d.Authz == nil, d.Orgs == nil, d.Jobs == nil, d.Collections == nil, d.NamespaceSearch == nil:
		return errors.New("api: Repos, LFS, Authz, Orgs, Jobs, Collections and NamespaceSearch are required")
	case d.PublicURL == "":
		return errors.New("api: PublicURL is required")
	}
	if d.UploadURLTTL <= 0 {
		d.UploadURLTTL = 24 * time.Hour
	}
	if d.InlineMax <= 0 {
		d.InlineMax = 10 << 20
	}
	if d.Log == nil {
		d.Log = slog.New(slog.DiscardHandler)
	}
	d.PublicURL = strings.TrimSuffix(d.PublicURL, "/")
	return nil
}

// ReposAPI serves /repos: listing, settings, lifecycle, refs, Commits,
// trees and zip exports (REPO-1 to REPO-5, REPO-9, DOWN-5).
type ReposAPI struct{ d RepoDeps }

// NewReposAPI returns the Repo handlers.
func NewReposAPI(d RepoDeps) (*ReposAPI, error) {
	if err := d.validate(); err != nil {
		return nil, err
	}
	return &ReposAPI{d: d}, nil
}

// problem writes err as a problem detail. Errors the caller cannot act on
// are logged and answered with a generic message.
func (d *RepoDeps) problem(w http.ResponseWriter, r *http.Request, err error) {
	status := http.StatusInternalServerError
	var pe *repo.PolicyError
	switch {
	case errors.Is(err, repo.ErrNotFound), errors.Is(err, auth.ErrNotFound), errors.Is(err, org.ErrNotFound),
		errors.Is(err, repo.ErrRevisionNotFound), errors.Is(err, repo.ErrEntryNotFound), errors.Is(err, repo.ErrNotPresent),
		errors.Is(err, jobs.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, repo.ErrExists), errors.Is(err, repo.ErrRefExists), errors.Is(err, repo.ErrRefConflict):
		status = http.StatusConflict
	case errors.Is(err, repo.ErrParentMismatch):
		status = http.StatusPreconditionFailed
	case errors.Is(err, auth.ErrForbidden), errors.Is(err, auth.ErrGated), errors.Is(err, repo.ErrReadOnly), errors.Is(err, repo.ErrProtectedRef),
		errors.As(err, &pe):
		status = http.StatusForbidden
	case errors.Is(err, repo.ErrLFSObjectMissing), errors.Is(err, repo.ErrChecksumMismatch):
		status = http.StatusUnprocessableEntity
	case errors.Is(err, repo.ErrInvalid), errors.Is(err, model.ErrInvalidRepoID), errors.Is(err, model.ErrInvalidRepoType),
		errors.Is(err, model.ErrInvalidPath), errors.Is(err, model.ErrInvalidNamespace), errors.Is(err, blob.ErrInvalidRange),
		errors.Is(err, community.ErrInvalid), errors.Is(err, collections.ErrInvalid):
		status = http.StatusBadRequest
	}
	detail := err.Error()
	if status == http.StatusNotFound && (errors.Is(err, auth.ErrNotFound) || errors.Is(err, repo.ErrNotFound)) {
		detail = "Repository not found. If it is private, make sure you are signed in and have access to it."
	}
	if status == http.StatusInternalServerError {
		d.Log.ErrorContext(r.Context(), "native api request failed", "error", err.Error())
		detail = "Internal server error. The request ID identifies it in the server log."
	}
	// Anonymous callers get 401 for any missing Repo, so a private Repo
	// looks the same as one that does not exist.
	if status == http.StatusNotFound && httpx.Principal(r.Context()).IsAnonymous() && (errors.Is(err, auth.ErrNotFound) || errors.Is(err, repo.ErrNotFound)) {
		status = http.StatusUnauthorized
		detail = "Repository not found. If it is private, sign in to see it."
	}
	httpx.WriteProblem(w, r, &httpx.Problem{Status: status, Detail: detail})
}

func badRequest(w http.ResponseWriter, r *http.Request, param, detail string) {
	p := &httpx.Problem{Status: http.StatusBadRequest, Detail: detail}
	if param != "" {
		p.Errors = []httpx.ProblemError{{Parameter: param, Detail: detail}}
	}
	httpx.WriteProblem(w, r, p)
}

func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		badRequest(w, r, "", "The request body is not valid JSON for this endpoint: "+err.Error())
		return false
	}
	return true
}

// repoID builds a RepoID from path parameters.
func repoID(t gen.RepoType, ns, name string) (model.RepoID, error) {
	rt, err := model.ParseRepoType(string(t))
	if err != nil || (t != "models" && t != "datasets") {
		return model.RepoID{}, fmt.Errorf("%w: %q", model.ErrInvalidRepoType, t)
	}
	return model.RepoID{Type: rt, Namespace: ns, Name: name}, nil
}

func target(r *repo.Repo) auth.Target {
	return auth.Target{Repo: &auth.RepoTarget{ID: r.ID, UUID: r.UUID, NamespaceID: r.NamespaceID, Visibility: r.Visibility,
		Gated: r.Gating.Enabled(), Mirror: r.Mirror}}
}

// load finds a Repo and checks the caller may do action on it. Callers
// without read access get a not-found error, so private names do not leak.
func (d *RepoDeps) load(r *http.Request, t gen.RepoType, ns, name string, action auth.Action) (*repo.Repo, error) {
	id, err := repoID(t, ns, name)
	if err != nil {
		return nil, err
	}
	rp, err := d.Repos.Get(r.Context(), id)
	if err != nil {
		return nil, err
	}
	p := httpx.Principal(r.Context())
	if err := d.Authz.Can(r.Context(), p, auth.ActionRead, target(rp)); err != nil {
		return nil, err
	}
	if action != auth.ActionRead {
		if err := d.Authz.Can(r.Context(), p, action, target(rp)); err != nil {
			return nil, err
		}
	}
	httpx.SetRepo(r.Context(), rp.ID)
	return rp, nil
}

func str(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func i64(n int64) *int64 { return &n }

// toRepo converts a Repo, with the extras the caller is allowed to see.
func (d *RepoDeps) toRepo(ctx context.Context, p model.Principal, r *repo.Repo) gen.Repo {
	out := gen.Repo{
		Id: r.ID.String(), Type: gen.RepoTypeSingular(r.ID.Type), Namespace: r.ID.Namespace, Name: r.ID.Name,
		Visibility: gen.Visibility(r.Visibility), Description: str(r.Description), DefaultBranch: r.DefaultBranch,
		Licence: str(r.Licence), Mirror: r.Mirror, Gated: gen.RepoGated(r.Gating.Mode),
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt, SizeBytes: r.SizeBytes, LfsBytes: r.LFSBytes,
	}
	u := r.UUID
	out.Uuid = &u
	tags := append([]string{}, r.Tags...)
	out.Tags = &tags
	if !r.LastModified.IsZero() {
		lm := r.LastModified
		out.LastModified = &lm
	}
	if r.DeletedAt != nil {
		del := *r.DeletedAt
		out.DeletedAt = &del
	}
	if r.DeletedAt == nil {
		if sha, err := d.Repos.Resolve(ctx, r.ID, ""); err == nil {
			s := string(sha)
			out.Sha = &s
			d.addSummary(ctx, r, sha, &out)
		}
	}
	if d.Community != nil {
		if n, err := d.Community.Likes(ctx, r.UUID); err == nil {
			out.Likes = n
		}
		if days, err := d.Community.Downloads(ctx, r.UUID, time.Now().UTC().AddDate(0, 0, -30).Truncate(24*time.Hour)); err == nil {
			for _, day := range days {
				out.Downloads += day.Count
			}
		}
	}
	perms := gen.RepoPermissions{}
	can := func(a auth.Action) bool { return d.Authz.Can(ctx, p, a, target(r)) == nil }
	perms.Read = can(auth.ActionRead)
	perms.Write = can(auth.ActionWrite)
	perms.Admin = can(auth.ActionAdmin)
	perms.Discuss = can(auth.ActionDiscuss)
	if r.Mirror {
		pm := can(auth.ActionPullMirror)
		perms.PullMirror = &pm
	}
	out.Permissions = &perms
	return out
}

func (d *RepoDeps) addSummary(ctx context.Context, r *repo.Repo, sha model.CommitSHA, out *gen.Repo) {
	if d.Index == nil {
		return
	}
	sum, err := d.Index.Get(ctx, r.UUID, sha)
	if err != nil {
		return
	}
	s := gen.RepoSummary{Architecture: str(sum.Architecture), PipelineTag: str(sum.PipelineTag), LibraryName: str(sum.LibraryName)}
	if len(sum.Formats) > 0 {
		f := append([]string{}, sum.Formats...)
		s.Formats = &f
	}
	if sum.Parameters != nil {
		s.Parameters = i64(sum.Parameters.Total)
	}
	if sum.ContextLength > 0 {
		s.ContextLength = i64(sum.ContextLength)
	}
	var quants []string
	for _, q := range sum.Quantisations {
		quants = append(quants, q.Name)
	}
	if len(quants) > 0 {
		s.Quants = &quants
	}
	if d.Scanner != nil {
		if res, err := d.Scanner.Results(ctx, r.UUID, sha); err == nil {
			st := gen.RepoSummaryScanStatus(res.Status)
			s.ScanStatus = &st
		} else if errors.Is(err, scan.ErrNotScanned) {
			st := gen.RepoSummaryScanStatusPending
			s.ScanStatus = &st
		}
	}
	out.Summary = &s
}

func limitOf(l *gen.Limit) (int, error) {
	if l == nil {
		return 50, nil
	}
	if *l < 1 || *l > 500 {
		return 0, fmt.Errorf("limit must be between 1 and 500")
	}
	return *l, nil
}

func one(s *string) []string {
	if s == nil || *s == "" {
		return nil
	}
	return []string{*s}
}

// ListRepos implements gen.ServerInterface (BROWSE-1).
func (a *ReposAPI) ListRepos(w http.ResponseWriter, r *http.Request, params gen.ListReposParams) {
	limit, err := limitOf(params.Limit)
	if err != nil {
		badRequest(w, r, "limit", err.Error())
		return
	}
	p := httpx.Principal(r.Context())
	opts := repo.ListOptions{Viewer: p, Limit: limit, Sort: "updated", Desc: true, IncludeFacets: a.d.Facets,
		Namespaces: one(params.Ns), Tasks: one(params.Task), Libraries: one(params.Library), Quants: one(params.Quant),
		Licences: one(params.Licence), Modalities: one(params.Modality), Languages: one(params.Language),
		SizeCategories: one(params.SizeCategory), Mirror: params.Mirror}
	if params.Type != nil {
		t, err := model.ParseRepoType(string(*params.Type))
		if err != nil {
			badRequest(w, r, "type", err.Error())
			return
		}
		opts.Type = t
	}
	if params.Q != nil {
		opts.Search = *params.Q
	}
	if params.Format != nil {
		opts.Formats = *params.Format
	}
	for _, v := range []struct {
		dst  *int64
		src  *int64
		name string
	}{{&opts.ParamsMin, params.ParamsMin, "params_min"}, {&opts.ParamsMax, params.ParamsMax, "params_max"},
		{&opts.ContextMin, params.ContextMin, "context_min"}, {&opts.SizeMax, params.SizeMax, "size_max"}} {
		if v.src != nil {
			if *v.src < 0 {
				badRequest(w, r, v.name, v.name+" cannot be negative")
				return
			}
			*v.dst = *v.src
		}
	}
	if params.Sort != nil {
		opts.Sort = string(*params.Sort)
		opts.Desc = opts.Sort != "name"
	}
	if params.Cursor != nil {
		opts.Cursor = *params.Cursor
	}
	res, err := a.d.Repos.List(r.Context(), opts)
	if err != nil {
		a.d.problem(w, r, err)
		return
	}
	out := gen.RepoList{Items: make([]gen.Repo, 0, len(res.Repos)), NextCursor: str(res.NextCursor)}
	for _, rp := range res.Repos {
		out.Items = append(out.Items, a.d.toRepo(r.Context(), p, rp))
	}
	if res.Facets != nil {
		out.Facets = &res.Facets
	}
	httpx.WriteJSON(w, r, http.StatusOK, out)
}

// CreateRepo implements gen.ServerInterface (REPO-1).
func (a *ReposAPI) CreateRepo(w http.ResponseWriter, r *http.Request) {
	var body gen.RepoCreate
	if !decodeJSON(w, r, &body) {
		return
	}
	p := httpx.Principal(r.Context())
	if p.IsAnonymous() {
		httpx.WriteProblem(w, r, &httpx.Problem{Status: http.StatusUnauthorized, Detail: "Sign in to create a Repo."})
		return
	}
	t, err := model.ParseRepoType(string(body.Type))
	if err != nil {
		badRequest(w, r, "", err.Error())
		return
	}
	ns, err := a.d.Orgs.Get(r.Context(), body.Namespace)
	if err != nil {
		if errors.Is(err, org.ErrNotFound) {
			httpx.WriteProblem(w, r, &httpx.Problem{Status: http.StatusNotFound, Detail: "Namespace " + body.Namespace + " does not exist."})
			return
		}
		a.d.problem(w, r, err)
		return
	}
	if err := a.d.Authz.Can(r.Context(), p, auth.ActionCreateRepo, auth.Target{Namespace: &auth.NamespaceTarget{ID: ns.ID, Name: ns.Name, Kind: ns.Kind}}); err != nil {
		httpx.WriteProblem(w, r, &httpx.Problem{Status: http.StatusForbidden, Detail: "You cannot create Repos in " + ns.Name + "."})
		return
	}
	params := repo.CreateParams{ID: model.RepoID{Type: t, Namespace: ns.Name, Name: body.Name}, Actor: p}
	if body.Visibility != nil {
		params.Visibility = model.Visibility(*body.Visibility)
	}
	if body.Description != nil {
		params.Description = *body.Description
	}
	if body.Licence != nil {
		params.Licence = *body.Licence
	}
	params.InitialReadme = body.Readme != nil && *body.Readme
	created, err := a.d.Repos.Create(r.Context(), params)
	if err != nil {
		a.d.problem(w, r, err)
		return
	}
	w.Header().Set("Location", a.d.PublicURL+"/api/hangar/v1/repos/"+created.ID.Type.Plural()+"/"+created.ID.String())
	httpx.WriteJSON(w, r, http.StatusCreated, a.d.toRepo(r.Context(), p, created))
}

// GetRepo implements gen.ServerInterface. A renamed Repo answers with its
// new ID, so clients can follow the redirect (REPO-4).
func (a *ReposAPI) GetRepo(w http.ResponseWriter, r *http.Request, pType gen.RepoType, ns gen.Ns, name gen.Name) {
	rp, err := a.d.load(r, pType, ns, name, auth.ActionRead)
	if err != nil {
		a.d.problem(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, a.d.toRepo(r.Context(), httpx.Principal(r.Context()), rp))
}

// UpdateRepo implements gen.ServerInterface (REPO-9).
func (a *ReposAPI) UpdateRepo(w http.ResponseWriter, r *http.Request, pType gen.RepoType, ns gen.Ns, name gen.Name) {
	rp, err := a.d.load(r, pType, ns, name, auth.ActionAdmin)
	if err != nil {
		a.d.problem(w, r, err)
		return
	}
	var body gen.RepoUpdate
	if !decodeJSON(w, r, &body) {
		return
	}
	u := repo.SettingsUpdate{Description: body.Description, DefaultBranch: body.DefaultBranch, Tags: body.Tags, Licence: body.Licence}
	if body.Visibility != nil {
		v := model.Visibility(*body.Visibility)
		u.Visibility = &v
	}
	if body.Gating != nil {
		g := repo.Gating{}
		if body.Gating.Mode != nil {
			g.Mode = string(*body.Gating.Mode)
		}
		if body.Gating.Prompt != nil {
			g.Prompt = *body.Gating.Prompt
		}
		if body.Gating.Fields != nil {
			for _, f := range *body.Gating.Fields {
				g.Fields = append(g.Fields, repo.GatingField{Name: f.Name, Type: string(f.Type), Required: f.Required != nil && *f.Required})
			}
		}
		u.Gating = &g
	}
	updated, err := a.d.Repos.UpdateSettings(r.Context(), rp.ID, u, httpx.Principal(r.Context()))
	if err != nil {
		a.d.problem(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, a.d.toRepo(r.Context(), httpx.Principal(r.Context()), updated))
}

// DeleteRepo implements gen.ServerInterface (REPO-5).
func (a *ReposAPI) DeleteRepo(w http.ResponseWriter, r *http.Request, pType gen.RepoType, ns gen.Ns, name gen.Name) {
	rp, err := a.d.load(r, pType, ns, name, auth.ActionAdmin)
	if err != nil {
		a.d.problem(w, r, err)
		return
	}
	if err := a.d.Repos.Delete(r.Context(), rp.ID, httpx.Principal(r.Context())); err != nil {
		a.d.problem(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// RestoreRepo implements gen.ServerInterface. Restoring needs admin on the
// Namespace, because the deleted Repo itself is not readable.
func (a *ReposAPI) RestoreRepo(w http.ResponseWriter, r *http.Request, pType gen.RepoType, ns gen.Ns, name gen.Name) {
	id, err := repoID(pType, ns, name)
	if err != nil {
		a.d.problem(w, r, err)
		return
	}
	p := httpx.Principal(r.Context())
	n, err := a.d.Orgs.Get(r.Context(), ns)
	if err != nil {
		a.d.problem(w, r, repo.ErrNotFound)
		return
	}
	if err := a.d.Authz.Can(r.Context(), p, auth.ActionAdmin, auth.Target{Namespace: &auth.NamespaceTarget{ID: n.ID, Name: n.Name, Kind: n.Kind}}); err != nil {
		a.d.problem(w, r, repo.ErrNotFound)
		return
	}
	restored, err := a.d.Repos.Restore(r.Context(), id, p)
	if err != nil {
		a.d.problem(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, a.d.toRepo(r.Context(), p, restored))
}

// RenameRepo implements gen.ServerInterface (REPO-4).
func (a *ReposAPI) RenameRepo(w http.ResponseWriter, r *http.Request, pType gen.RepoType, ns gen.Ns, name gen.Name) {
	rp, err := a.d.load(r, pType, ns, name, auth.ActionAdmin)
	if err != nil {
		a.d.problem(w, r, err)
		return
	}
	var body gen.RenameRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	renamed, err := a.d.Repos.Rename(r.Context(), rp.ID, body.Name, httpx.Principal(r.Context()))
	if err != nil {
		a.d.problem(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, a.d.toRepo(r.Context(), httpx.Principal(r.Context()), renamed))
}

// TransferRepo implements gen.ServerInterface (REPO-4). The caller needs
// admin on the Repo and the right to create Repos in the new Namespace.
func (a *ReposAPI) TransferRepo(w http.ResponseWriter, r *http.Request, pType gen.RepoType, ns gen.Ns, name gen.Name) {
	rp, err := a.d.load(r, pType, ns, name, auth.ActionAdmin)
	if err != nil {
		a.d.problem(w, r, err)
		return
	}
	var body gen.TransferRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	p := httpx.Principal(r.Context())
	dst, err := a.d.Orgs.Get(r.Context(), body.Namespace)
	if err != nil {
		if errors.Is(err, org.ErrNotFound) {
			badRequest(w, r, "", "Namespace "+body.Namespace+" does not exist.")
			return
		}
		a.d.problem(w, r, err)
		return
	}
	if err := a.d.Authz.Can(r.Context(), p, auth.ActionCreateRepo, auth.Target{Namespace: &auth.NamespaceTarget{ID: dst.ID, Name: dst.Name, Kind: dst.Kind}}); err != nil {
		httpx.WriteProblem(w, r, &httpx.Problem{Status: http.StatusForbidden, Detail: "You cannot create Repos in " + dst.Name + "."})
		return
	}
	moved, err := a.d.Repos.Transfer(r.Context(), rp.ID, dst.Name, p)
	if err != nil {
		a.d.problem(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, a.d.toRepo(r.Context(), p, moved))
}

func toRefs(in []repo.Ref) []gen.Ref {
	out := make([]gen.Ref, 0, len(in))
	for _, r := range in {
		out = append(out, gen.Ref{Name: model.ShortRefName(r.Name), Ref: r.Name, TargetCommit: string(r.Target)})
	}
	return out
}

// ListRepoRefs implements gen.ServerInterface (REPO-3).
func (a *ReposAPI) ListRepoRefs(w http.ResponseWriter, r *http.Request, pType gen.RepoType, ns gen.Ns, name gen.Name) {
	rp, err := a.d.load(r, pType, ns, name, auth.ActionRead)
	if err != nil {
		a.d.problem(w, r, err)
		return
	}
	refs, err := a.d.Repos.Refs(r.Context(), rp.ID)
	if err != nil {
		a.d.problem(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, gen.Refs{Branches: toRefs(refs.Branches), Tags: toRefs(refs.Tags),
		Converts: toRefs(refs.Converts), PullRequests: toRefs(refs.PullRequests)})
}

func toSignature(s repo.Signature) gen.Signature {
	return gen.Signature{Name: s.Name, Email: s.Email, Date: s.When.UTC()}
}

func toCommitInfo(c repo.CommitInfo) gen.CommitInfo {
	parents := make([]string, 0, len(c.Parents))
	for _, p := range c.Parents {
		parents = append(parents, string(p))
	}
	return gen.CommitInfo{Sha: string(c.SHA), Parents: parents, Author: toSignature(c.Author), Committer: toSignature(c.Committer),
		Title: c.Title, Message: c.Message}
}

// ListCommits implements gen.ServerInterface (REPO-3). The cursor is the
// number of Commits already listed.
func (a *ReposAPI) ListCommits(w http.ResponseWriter, r *http.Request, pType gen.RepoType, ns gen.Ns, name gen.Name, params gen.ListCommitsParams) {
	rp, err := a.d.load(r, pType, ns, name, auth.ActionRead)
	if err != nil {
		a.d.problem(w, r, err)
		return
	}
	limit, err := limitOf(params.Limit)
	if err != nil {
		badRequest(w, r, "limit", err.Error())
		return
	}
	skip := 0
	if params.Cursor != nil && *params.Cursor != "" {
		skip, err = strconv.Atoi(*params.Cursor)
		if err != nil || skip < 0 {
			badRequest(w, r, "cursor", "The cursor is not valid.")
			return
		}
	}
	opts := repo.LogOpts{Skip: skip, Limit: limit + 1}
	if params.Path != nil {
		opts.Path = *params.Path
	}
	rev := ""
	if params.Rev != nil {
		rev = *params.Rev
	}
	commits, err := a.d.Repos.Log(r.Context(), rp.ID, rev, opts)
	if err != nil {
		a.d.problem(w, r, err)
		return
	}
	out := gen.CommitList{Items: []gen.CommitInfo{}}
	if len(commits) > limit {
		commits = commits[:limit]
		out.NextCursor = str(strconv.Itoa(skip + limit))
	}
	for _, c := range commits {
		out.Items = append(out.Items, toCommitInfo(c))
	}
	httpx.WriteJSON(w, r, http.StatusOK, out)
}

func toChange(c repo.FileChange) gen.FileChange {
	out := gen.FileChange{Path: c.Path, Status: gen.FileChangeStatus(c.Status), Lfs: c.LFS, Binary: c.Binary,
		OldPath: str(c.OldPath), OldOid: str(c.OldOID), NewOid: str(c.NewOID)}
	if c.OldOID != "" {
		out.OldSize = i64(c.OldSize)
	}
	if c.NewOID != "" {
		out.NewSize = i64(c.NewSize)
	}
	return out
}

// GetCommit implements gen.ServerInterface (REPO-3).
func (a *ReposAPI) GetCommit(w http.ResponseWriter, r *http.Request, pType gen.RepoType, ns gen.Ns, name gen.Name, sha gen.SHA) {
	rp, err := a.d.load(r, pType, ns, name, auth.ActionRead)
	if err != nil {
		a.d.problem(w, r, err)
		return
	}
	c, err := a.d.Repos.GetCommit(r.Context(), rp.ID, model.CommitSHA(strings.ToLower(sha)))
	if err != nil {
		a.d.problem(w, r, err)
		return
	}
	changes, err := a.d.Repos.Diff(r.Context(), rp.ID, c.SHA)
	if err != nil {
		a.d.problem(w, r, err)
		return
	}
	info := toCommitInfo(*c)
	out := gen.CommitDetail{Sha: info.Sha, Parents: info.Parents, Author: info.Author, Committer: info.Committer, Title: info.Title,
		Message: info.Message, Changes: make([]gen.FileChange, 0, len(changes))}
	for _, ch := range changes {
		out.Changes = append(out.Changes, toChange(ch))
	}
	httpx.WriteJSON(w, r, http.StatusOK, out)
}

// GetCommitFileDiff implements gen.ServerInterface. A patch shows file
// content, so gated Repos need an approved access request.
func (a *ReposAPI) GetCommitFileDiff(w http.ResponseWriter, r *http.Request, pType gen.RepoType, ns gen.Ns, name gen.Name, sha gen.SHA, params gen.GetCommitFileDiffParams) {
	rp, err := a.d.load(r, pType, ns, name, auth.ActionReadFiles)
	if err != nil {
		a.d.problem(w, r, err)
		return
	}
	fd, err := a.d.Repos.FileDiff(r.Context(), rp.ID, model.CommitSHA(strings.ToLower(sha)), params.Path)
	if err != nil {
		a.d.problem(w, r, err)
		return
	}
	c := toChange(fd.FileChange)
	httpx.WriteJSON(w, r, http.StatusOK, gen.FileDiff{Path: c.Path, OldPath: c.OldPath, Status: gen.FileDiffStatus(c.Status), OldOid: c.OldOid,
		NewOid: c.NewOid, OldSize: c.OldSize, NewSize: c.NewSize, Lfs: c.Lfs, Binary: c.Binary, Patch: str(fd.Patch)})
}

// GetTreeRoot implements gen.ServerInterface.
func (a *ReposAPI) GetTreeRoot(w http.ResponseWriter, r *http.Request, pType gen.RepoType, ns gen.Ns, name gen.Name, rev gen.Rev, params gen.GetTreeRootParams) {
	a.tree(w, r, pType, ns, name, rev, "", params.Recursive, params.Cursor, params.Limit)
}

// GetTree implements gen.ServerInterface. A file path answers with that
// file as the only item.
func (a *ReposAPI) GetTree(w http.ResponseWriter, r *http.Request, pType gen.RepoType, ns gen.Ns, name gen.Name, rev gen.Rev, p gen.FilePath, params gen.GetTreeParams) {
	a.tree(w, r, pType, ns, name, rev, p, params.Recursive, params.Cursor, params.Limit)
}

func (a *ReposAPI) tree(w http.ResponseWriter, r *http.Request, pType gen.RepoType, ns, name, rev, p string, recursive *bool, cursor *string, limitP *int) {
	rp, err := a.d.load(r, pType, ns, name, auth.ActionRead)
	if err != nil {
		a.d.problem(w, r, err)
		return
	}
	limit, err := limitOf(limitP)
	if err != nil {
		badRequest(w, r, "limit", err.Error())
		return
	}
	sha, err := a.d.Repos.Resolve(r.Context(), rp.ID, rev)
	if err != nil {
		a.d.problem(w, r, err)
		return
	}
	entry, err := a.d.Repos.Entry(r.Context(), rp.ID, sha, p)
	if err != nil {
		a.d.problem(w, r, err)
		return
	}
	out := gen.TreeListing{Commit: string(sha), Items: []gen.TreeEntry{}}
	var entries []model.Entry
	if entry.Type == model.EntryFile {
		entries, err = a.d.Repos.Entries(r.Context(), rp.ID, sha, []string{entry.Path}, true)
	} else {
		opts := repo.TreeOpts{Recursive: recursive != nil && *recursive, Expand: true, Limit: limit + 1}
		if cursor != nil {
			opts.After = *cursor
		}
		entries, err = a.d.Repos.Tree(r.Context(), rp.ID, sha, entry.Path, opts)
		if len(entries) > limit {
			entries = entries[:limit]
			out.NextCursor = str(entries[limit-1].Path)
		}
	}
	if err != nil {
		a.d.problem(w, r, err)
		return
	}
	scans := a.d.scanStatuses(r.Context(), rp, sha)
	for _, e := range entries {
		te := gen.TreeEntry{Path: e.Path, Type: gen.TreeEntryType(e.Type), Size: e.Size, Oid: e.OID, Present: e.Present}
		if e.LFS != nil {
			te.Lfs = &struct {
				Oid         string `json:"oid"`
				PointerSize int    `json:"pointer_size"`
				Size        int64  `json:"size"`
			}{Oid: e.LFS.OID, PointerSize: e.LFS.PointerSize, Size: e.LFS.Size}
		}
		if e.LastCommit != nil {
			te.LastCommit = &gen.LastCommit{Sha: string(e.LastCommit.SHA), Title: e.LastCommit.Title, Date: e.LastCommit.Date}
		}
		if st, ok := scans[e.Path]; ok && e.Type == model.EntryFile {
			s := st
			te.ScanStatus = &s
		}
		out.Items = append(out.Items, te)
	}
	httpx.WriteJSON(w, r, http.StatusOK, out)
}

// scanStatuses returns each scanned file's status, or nothing when no
// scanner is wired or the Commit is not scanned yet.
func (d *RepoDeps) scanStatuses(ctx context.Context, rp *repo.Repo, sha model.CommitSHA) map[string]gen.TreeEntryScanStatus {
	out := map[string]gen.TreeEntryScanStatus{}
	if d.Scanner == nil {
		return out
	}
	res, err := d.Scanner.Results(ctx, rp.UUID, sha)
	if err != nil {
		return out
	}
	for _, f := range res.Files {
		out[f.Path] = gen.TreeEntryScanStatus(f.Status)
	}
	return out
}

// toJob converts a Job for the native API.
func toJob(j *jobs.Job) gen.Job {
	out := gen.Job{Id: j.ID, Kind: string(j.Kind), Status: gen.JobStatus(j.Status), Attempts: j.Attempts, MaxAttempts: j.MaxAttempts,
		CreatedAt: j.CreatedAt, StartedAt: j.StartedAt, FinishedAt: j.FinishedAt, Error: str(j.Error),
		Progress: gen.JobProgress{BytesDone: j.Progress.BytesDone, BytesTotal: j.Progress.BytesTotal, FilesDone: j.Progress.FilesDone,
			FilesTotal: j.Progress.FilesTotal, Rate: float32(j.Progress.Rate), Message: str(j.Progress.Message)}}
	if !j.RunAfter.IsZero() {
		ra := j.RunAfter
		out.RunAfter = &ra
	}
	if j.CreatedBy != uuid.Nil {
		out.CreatedBy = str(j.CreatedBy.String())
	}
	if len(j.Result) > 0 {
		var m map[string]any
		if json.Unmarshal(j.Result, &m) == nil {
			out.Result = &m
		}
	}
	return out
}

// StartZipExport implements gen.ServerInterface (DOWN-5): it starts a
// zip.export Job, whose result carries the download URL.
func (a *ReposAPI) StartZipExport(w http.ResponseWriter, r *http.Request, pType gen.RepoType, ns gen.Ns, name gen.Name, params gen.StartZipExportParams) {
	rp, err := a.d.load(r, pType, ns, name, auth.ActionReadFiles)
	if err != nil {
		a.d.problem(w, r, err)
		return
	}
	rev := ""
	if params.Rev != nil {
		rev = *params.Rev
	}
	sha, err := a.d.Repos.Resolve(r.Context(), rp.ID, rev)
	if err != nil {
		a.d.problem(w, r, err)
		return
	}
	dir := ""
	if params.Path != nil {
		dir = *params.Path
	}
	e, err := a.d.Repos.Entry(r.Context(), rp.ID, sha, dir)
	if err != nil {
		a.d.problem(w, r, err)
		return
	}
	if e.Type != model.EntryDirectory {
		badRequest(w, r, "path", "The path is a file. Download it directly, or choose a folder.")
		return
	}
	p := httpx.Principal(r.Context())
	id, err := a.d.Jobs.Enqueue(r.Context(), jobs.NewJob{Kind: jobs.KindZipExport, CreatedBy: p.UserID,
		DedupeKey: rp.UUID.String() + ":" + string(sha) + ":" + e.Path + ":" + p.UserID.String(),
		Payload:   repo.ZipJob{RepoUUID: rp.UUID, Commit: sha, Path: e.Path}})
	if err != nil {
		a.d.problem(w, r, err)
		return
	}
	j, err := a.d.Jobs.Get(r.Context(), id)
	if err != nil {
		a.d.problem(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusAccepted, toJob(j))
}

// ExportPath is where ExportHandler serves zip exports, under the public URL.
const ExportPath = "/api/hangar/v1/exports/"

// ExportURL returns repo.JobOptions.ExportURL: a presigned URL when the
// Blob store can presign, otherwise a Hangar URL signed with signer that
// ExportHandler serves.
func ExportURL(publicURL string, blobs blob.Store, signer *httpx.Signer) func(ctx context.Context, key, filename string, ttl time.Duration) (string, error) {
	publicURL = strings.TrimSuffix(publicURL, "/")
	return func(ctx context.Context, key, filename string, ttl time.Duration) (string, error) {
		u, err := blobs.PresignGet(ctx, key, ttl, filename)
		if err == nil {
			return u, nil
		}
		if !errors.Is(err, blob.ErrNotSupported) {
			return "", err
		}
		raw := publicURL + ExportPath + url.PathEscape(path.Base(key)) + "?filename=" + url.QueryEscape(filename)
		return signer.Sign(raw, "export:"+key, ttl)
	}
}

// ExportHandler serves zip exports at ExportPath from signed URLs made by
// ExportURL. Mount it outside the generated router, because the URL
// authorises itself.
func ExportHandler(blobs blob.Store, signer *httpx.Signer) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpx.SetRoute(r.Context(), ExportPath+"{file}")
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			httpx.WriteProblem(w, r, &httpx.Problem{Status: http.StatusMethodNotAllowed})
			return
		}
		file := strings.TrimPrefix(r.URL.Path, ExportPath)
		key := blob.PrefixExports + file
		scope, err := signer.Verify(r.URL)
		if err != nil || scope != "export:"+key || blob.ValidateKey(key) != nil || strings.Contains(file, "/") {
			httpx.WriteProblem(w, r, &httpx.Problem{Status: http.StatusForbidden, Detail: "The download link is invalid or has expired. Start the download again."})
			return
		}
		rc, info, err := blobs.Get(r.Context(), key, nil)
		if err != nil {
			if errors.Is(err, blob.ErrNotFound) {
				httpx.WriteProblem(w, r, &httpx.Problem{Status: http.StatusNotFound, Detail: "The export has expired. Start the download again."})
				return
			}
			httpx.WriteProblem(w, r, &httpx.Problem{Status: http.StatusInternalServerError})
			return
		}
		defer rc.Close()
		filename := r.URL.Query().Get("filename")
		if filename == "" {
			filename = file
		}
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Length", strconv.FormatInt(info.Size, 10))
		w.Header().Set("Content-Disposition", "attachment; filename*=UTF-8''"+url.PathEscape(path.Base(filename)))
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			_, _ = io.Copy(w, rc)
		}
	})
}
