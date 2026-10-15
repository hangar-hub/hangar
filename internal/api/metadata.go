package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/google/uuid"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/jamesagarside/hangar/internal/api/gen"
	"github.com/jamesagarside/hangar/internal/audit"
	"github.com/jamesagarside/hangar/internal/auth"
	"github.com/jamesagarside/hangar/internal/collections"
	"github.com/jamesagarside/hangar/internal/community"
	"github.com/jamesagarside/hangar/internal/httpx"
	"github.com/jamesagarside/hangar/internal/index"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/org"
	"github.com/jamesagarside/hangar/internal/repo"
	"github.com/jamesagarside/hangar/internal/scan"
	"github.com/jamesagarside/hangar/internal/webhook"
)

// WebhookStore is what the webhook handlers need from webhook.Service.
type WebhookStore interface {
	Create(ctx context.Context, p webhook.CreateParams) (*webhook.Webhook, error)
	Get(ctx context.Context, id openapi_types.UUID) (*webhook.Webhook, error)
	List(ctx context.Context, o webhook.ListOptions) ([]webhook.Webhook, error)
	Update(ctx context.Context, id openapi_types.UUID, u webhook.Update) (*webhook.Webhook, error)
	Delete(ctx context.Context, id openapi_types.UUID) error
	Deliveries(ctx context.Context, webhookID openapi_types.UUID, cursor string, limit int) ([]webhook.Delivery, string, error)
	LastDelivery(ctx context.Context, webhookID openapi_types.UUID) (*webhook.Delivery, error)
	Redeliver(ctx context.Context, webhookID, deliveryID openapi_types.UUID) (*webhook.Delivery, error)
	Test(ctx context.Context, webhookID openapi_types.UUID, actor model.Principal) (*webhook.Delivery, error)
}

var _ WebhookStore = (*webhook.Service)(nil)

// MetaDeps are the services behind the metadata, community, Collection and
// webhook handlers (META-1 to META-3, REPO-6, REPO-7, BROWSE-5, EVT-1).
type MetaDeps struct {
	Repos       repo.Service
	Authz       auth.Authz
	Orgs        org.Service
	Index       index.Indexer
	Scanner     scan.Scanner
	Community   community.Service
	Collections collections.Service
	Webhooks    WebhookStore
	// Audit records webhook changes; optional.
	Audit audit.Recorder
	Log   *slog.Logger
}

func (d *MetaDeps) init() error {
	if d.Repos == nil || d.Authz == nil || d.Orgs == nil {
		return errors.New("api: Repos, Authz and Orgs are required")
	}
	if d.Log == nil {
		d.Log = slog.New(slog.DiscardHandler)
	}
	return nil
}

// MetadataAPI serves a Revision's index, file headers, GGUF quant groups,
// likes and download statistics.
type MetadataAPI struct{ d MetaDeps }

// NewMetadataAPI returns the metadata handlers. Index and Community are
// required.
func NewMetadataAPI(d MetaDeps) (*MetadataAPI, error) {
	if err := d.init(); err != nil {
		return nil, err
	}
	if d.Index == nil || d.Community == nil {
		return nil, errors.New("api: Index and Community are required")
	}
	return &MetadataAPI{d: d}, nil
}

// metaProblem answers an error as RFC 9457 problem details. Missing or
// unreadable Repos look the same, and anonymous callers get 401 for them.
func (d *MetaDeps) metaProblem(w http.ResponseWriter, r *http.Request, err error) {
	status := http.StatusInternalServerError
	detail := err.Error()
	switch {
	case errors.Is(err, repo.ErrNotFound), errors.Is(err, auth.ErrNotFound):
		status = http.StatusNotFound
		detail = "Repository not found. If it is private, make sure you are signed in and have access to it."
		if httpx.Principal(r.Context()).IsAnonymous() {
			status = http.StatusUnauthorized
			detail = "Repository not found. If it is private, sign in to see it."
		}
	case errors.Is(err, repo.ErrRevisionNotFound), errors.Is(err, repo.ErrEntryNotFound), errors.Is(err, community.ErrNotFound),
		errors.Is(err, collections.ErrNotFound), errors.Is(err, webhook.ErrNotFound), errors.Is(err, org.ErrNotFound),
		errors.Is(err, index.ErrNoHeader):
		status = http.StatusNotFound
	case errors.Is(err, repo.ErrNotPresent):
		status = http.StatusNotFound
		detail = "This file is not in the arsenal yet. Pull it from the Upstream to read it."
	case errors.Is(err, auth.ErrGated):
		status = http.StatusForbidden
		detail = "This Repo is gated. Request access to read its files."
	case errors.Is(err, auth.ErrForbidden), errors.Is(err, collections.ErrForbidden), errors.Is(err, repo.ErrReadOnly),
		errors.Is(err, repo.ErrProtectedRef):
		status = http.StatusForbidden
	case errors.Is(err, collections.ErrExists), errors.Is(err, community.ErrConflict), errors.Is(err, repo.ErrRefConflict),
		errors.Is(err, repo.ErrParentMismatch):
		status = http.StatusConflict
	case errors.Is(err, community.ErrNotOpen), errors.Is(err, community.ErrNotPullRequest):
		status = http.StatusUnprocessableEntity
	case errors.Is(err, community.ErrInvalid), errors.Is(err, collections.ErrInvalid), errors.Is(err, webhook.ErrInvalid),
		errors.Is(err, repo.ErrInvalid), errors.Is(err, model.ErrInvalidRepoID), errors.Is(err, model.ErrInvalidRepoType),
		errors.Is(err, model.ErrInvalidPath):
		status = http.StatusBadRequest
	}
	if status == http.StatusInternalServerError {
		d.Log.ErrorContext(r.Context(), "native api request failed", "error", err.Error())
		detail = "Internal server error. The request ID identifies it in the server log."
	}
	httpx.WriteProblem(w, r, &httpx.Problem{Status: status, Detail: detail})
}

func metaBadRequest(w http.ResponseWriter, r *http.Request, param, detail string) {
	p := &httpx.Problem{Status: http.StatusBadRequest, Detail: detail}
	if param != "" {
		p.Errors = []httpx.ProblemError{{Parameter: param, Detail: detail}}
	}
	httpx.WriteProblem(w, r, p)
}

func metaDecode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		metaBadRequest(w, r, "", "The request body is not valid JSON for this endpoint: "+err.Error())
		return false
	}
	return true
}

func metaRepoID(t gen.RepoType, ns, name string) (model.RepoID, error) {
	switch t {
	case gen.Models:
		return model.RepoID{Type: model.RepoTypeModel, Namespace: ns, Name: name}, nil
	case gen.Datasets:
		return model.RepoID{Type: model.RepoTypeDataset, Namespace: ns, Name: name}, nil
	}
	return model.RepoID{}, model.ErrInvalidRepoType
}

func metaTarget(r *repo.Repo) auth.Target {
	return auth.Target{Repo: &auth.RepoTarget{ID: r.ID, UUID: r.UUID, NamespaceID: r.NamespaceID, Visibility: r.Visibility,
		Gated: r.Gating.Enabled(), Mirror: r.Mirror}}
}

// metaLoad finds a live Repo the caller can read and may do action on.
func (d *MetaDeps) metaLoad(r *http.Request, t gen.RepoType, ns, name string, action auth.Action) (*repo.Repo, error) {
	id, err := metaRepoID(t, ns, name)
	if err != nil {
		return nil, err
	}
	rp, err := d.Repos.Get(r.Context(), id)
	if err != nil {
		return nil, err
	}
	if rp.DeletedAt != nil {
		return nil, repo.ErrNotFound
	}
	p := httpx.Principal(r.Context())
	if err := d.Authz.Can(r.Context(), p, auth.ActionRead, metaTarget(rp)); err != nil {
		return nil, err
	}
	if action != auth.ActionRead {
		if err := d.Authz.Can(r.Context(), p, action, metaTarget(rp)); err != nil {
			return nil, err
		}
	}
	httpx.SetRepo(r.Context(), rp.ID)
	return rp, nil
}

// metaResolve resolves an optional Revision; empty means the default branch.
func (d *MetaDeps) metaResolve(ctx context.Context, rp *repo.Repo, rev *string) (model.CommitSHA, error) {
	revision := rp.DefaultBranch
	if rev != nil && *rev != "" {
		revision = *rev
	}
	return d.Repos.Resolve(ctx, rp.ID, revision)
}

func metaLimit(l *int, def int) (int, bool) {
	if l == nil {
		return def, true
	}
	return *l, *l >= 1 && *l <= 500
}

// GetRepoMetadata implements gen.ServerInterface: the index of a Revision,
// or status pending while it is being indexed (META-1).
func (a *MetadataAPI) GetRepoMetadata(w http.ResponseWriter, r *http.Request, pType gen.RepoType, ns gen.Ns, name gen.Name, params gen.GetRepoMetadataParams) {
	rp, err := a.d.metaLoad(r, pType, ns, name, auth.ActionRead)
	if err != nil {
		a.d.metaProblem(w, r, err)
		return
	}
	sha, err := a.d.metaResolve(r.Context(), rp, params.Rev)
	if err != nil {
		a.d.metaProblem(w, r, err)
		return
	}
	out := gen.RepoMetadata{Commit: string(sha), Status: gen.RepoMetadataStatusPending}
	sum, err := a.d.Index.Get(r.Context(), rp.UUID, sha)
	switch {
	case errors.Is(err, index.ErrNotIndexed):
		httpx.WriteJSON(w, r, http.StatusOK, out)
		return
	case err != nil:
		a.d.metaProblem(w, r, err)
		return
	}
	raw, err := json.Marshal(sum)
	if err != nil {
		a.d.metaProblem(w, r, err)
		return
	}
	var m map[string]interface{}
	if err := json.Unmarshal(raw, &m); err != nil {
		a.d.metaProblem(w, r, err)
		return
	}
	out.Status, out.Summary = gen.RepoMetadataStatusReady, &m
	if at, ok := a.d.Index.(interface {
		IndexedAt(context.Context, openapi_types.UUID, model.CommitSHA) (time.Time, error)
	}); ok {
		if t, err := at.IndexedAt(r.Context(), rp.UUID, sha); err == nil {
			t = t.UTC()
			out.IndexedAt = &t
		}
	}
	httpx.WriteJSON(w, r, http.StatusOK, out)
}

// GetFileHeader implements gen.ServerInterface: a safetensors or GGUF
// header, read with ranged reads (BROWSE-4).
func (a *MetadataAPI) GetFileHeader(w http.ResponseWriter, r *http.Request, pType gen.RepoType, ns gen.Ns, name gen.Name, rev gen.Rev, p gen.FilePath) {
	rp, err := a.d.metaLoad(r, pType, ns, name, auth.ActionReadFiles)
	if err != nil {
		a.d.metaProblem(w, r, err)
		return
	}
	clean, err := model.CleanRepoPath(p)
	if err != nil || clean == "" {
		metaBadRequest(w, r, "path", "The path is not a valid file path.")
		return
	}
	sha, err := a.d.metaResolve(r.Context(), rp, &rev)
	if err != nil {
		a.d.metaProblem(w, r, err)
		return
	}
	format := gen.Safetensors
	switch strings.ToLower(path.Ext(clean)) {
	case ".safetensors":
	case ".gguf":
		format = gen.Gguf
	default:
		httpx.WriteProblem(w, r, &httpx.Problem{Status: http.StatusNotFound, Detail: "Only safetensors and GGUF files have a header to show."})
		return
	}
	raw, err := a.d.Index.FileHeader(r.Context(), rp.ID, sha, clean)
	if err != nil {
		a.d.metaProblem(w, r, err)
		return
	}
	var header map[string]interface{}
	if err := json.Unmarshal(raw, &header); err != nil {
		a.d.metaProblem(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, gen.FileHeader{Format: format, Header: header})
}

// GetFileScan implements gen.ServerInterface: one file's scan result with
// the imports the pickle scanner found (META-2), for the file page.
func (a *MetadataAPI) GetFileScan(w http.ResponseWriter, r *http.Request, pType gen.RepoType, ns gen.Ns, name gen.Name, rev gen.Rev, p gen.FilePath) {
	rp, err := a.d.metaLoad(r, pType, ns, name, auth.ActionReadFiles)
	if err != nil {
		a.d.metaProblem(w, r, err)
		return
	}
	clean, err := model.CleanRepoPath(p)
	if err != nil || clean == "" {
		metaBadRequest(w, r, "path", "The path is not a valid file path.")
		return
	}
	sha, err := a.d.metaResolve(r.Context(), rp, &rev)
	if err != nil {
		a.d.metaProblem(w, r, err)
		return
	}
	e, err := a.d.Repos.Entry(r.Context(), rp.ID, sha, clean)
	if err == nil && e.Type != model.EntryFile {
		err = repo.ErrEntryNotFound
	}
	if err != nil {
		a.d.metaProblem(w, r, err)
		return
	}
	out := gen.FileScan{Path: clean, Status: gen.FileScanStatus(scan.StatusSafe), Findings: []gen.ScanFinding{}}
	if a.d.Scanner == nil {
		out.Status = "pending"
		httpx.WriteJSON(w, r, http.StatusOK, out)
		return
	}
	res, err := a.d.Scanner.Results(r.Context(), rp.UUID, sha)
	switch {
	case errors.Is(err, scan.ErrNotScanned):
		out.Status = "pending"
	case err != nil:
		a.d.metaProblem(w, r, err)
		return
	default:
		for _, f := range res.Files {
			if f.Path != clean {
				continue
			}
			out.Status = gen.FileScanStatus(f.Status)
			if f.Error != "" {
				msg := f.Error
				out.Error = &msg
			}
			for _, fd := range f.Findings {
				out.Findings = append(out.Findings, gen.ScanFinding{Module: optString(fd.Module), Name: optString(fd.Name),
					Opcode: optString(fd.Opcode), Status: gen.ScanFindingStatus(fd.Status), Detail: optString(fd.Detail)})
			}
		}
	}
	httpx.WriteJSON(w, r, http.StatusOK, out)
}

func optString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// ListQuants implements gen.ServerInterface: GGUF files grouped by
// quantisation, with whether each group is in the arsenal (PULL-4).
func (a *MetadataAPI) ListQuants(w http.ResponseWriter, r *http.Request, pType gen.RepoType, ns gen.Ns, name gen.Name, params gen.ListQuantsParams) {
	rp, err := a.d.metaLoad(r, pType, ns, name, auth.ActionRead)
	if err != nil {
		a.d.metaProblem(w, r, err)
		return
	}
	sha, err := a.d.metaResolve(r.Context(), rp, params.Rev)
	if err != nil {
		a.d.metaProblem(w, r, err)
		return
	}
	groups, err := a.d.Index.Quants(r.Context(), rp.ID, sha)
	if err != nil {
		a.d.metaProblem(w, r, err)
		return
	}
	var paths []string
	for _, g := range groups {
		paths = append(paths, g.Files...)
		paths = append(paths, g.Projectors...)
	}
	present := map[string]bool{}
	if len(paths) > 0 {
		entries, err := a.d.Repos.Entries(r.Context(), rp.ID, sha, paths, false)
		if err != nil {
			a.d.metaProblem(w, r, err)
			return
		}
		for _, e := range entries {
			present[e.Path] = e.Present
		}
	}
	out := gen.QuantList{Commit: string(sha), Items: []gen.QuantGroup{}}
	for _, g := range groups {
		item := gen.QuantGroup{Name: g.Name, Files: append([]string{}, g.Files...), Size: g.Size, Present: true}
		for _, f := range g.Files {
			if !present[f] {
				item.Present = false
			}
		}
		if len(g.Projectors) > 0 {
			proj := append([]string{}, g.Projectors...)
			item.Projectors = &proj
		}
		out.Items = append(out.Items, item)
	}
	httpx.WriteJSON(w, r, http.StatusOK, out)
}

func (a *MetadataAPI) like(w http.ResponseWriter, r *http.Request, pType gen.RepoType, ns gen.Ns, name gen.Name, like bool) {
	rp, err := a.d.metaLoad(r, pType, ns, name, auth.ActionDiscuss)
	if err != nil {
		a.d.metaProblem(w, r, err)
		return
	}
	p := httpx.Principal(r.Context())
	if p.UserID == uuid.Nil {
		httpx.WriteProblem(w, r, &httpx.Problem{Status: http.StatusForbidden, Detail: "Only users can like a Repo."})
		return
	}
	if like {
		err = a.d.Community.Like(r.Context(), p.UserID, rp.UUID)
	} else {
		err = a.d.Community.Unlike(r.Context(), p.UserID, rp.UUID)
	}
	if err != nil {
		a.d.metaProblem(w, r, err)
		return
	}
	n, err := a.d.Community.Likes(r.Context(), rp.UUID)
	if err != nil {
		a.d.metaProblem(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, gen.LikeStatus{Liked: like, Likes: n})
}

// LikeRepo implements gen.ServerInterface (REPO-7).
func (a *MetadataAPI) LikeRepo(w http.ResponseWriter, r *http.Request, pType gen.RepoType, ns gen.Ns, name gen.Name) {
	a.like(w, r, pType, ns, name, true)
}

// UnlikeRepo implements gen.ServerInterface.
func (a *MetadataAPI) UnlikeRepo(w http.ResponseWriter, r *http.Request, pType gen.RepoType, ns gen.Ns, name gen.Name) {
	a.like(w, r, pType, ns, name, false)
}

// GetRepoStats implements gen.ServerInterface: downloads per day for the
// last `days` days, today included, with zero for days without any.
func (a *MetadataAPI) GetRepoStats(w http.ResponseWriter, r *http.Request, pType gen.RepoType, ns gen.Ns, name gen.Name, params gen.GetRepoStatsParams) {
	days := 30
	if params.Days != nil {
		days = *params.Days
	}
	if days < 1 || days > 365 {
		metaBadRequest(w, r, "days", "days must be between 1 and 365.")
		return
	}
	rp, err := a.d.metaLoad(r, pType, ns, name, auth.ActionRead)
	if err != nil {
		a.d.metaProblem(w, r, err)
		return
	}
	now := time.Now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	since := today.AddDate(0, 0, -(days - 1))
	counts, err := a.d.Community.Downloads(r.Context(), rp.UUID, since)
	if err != nil {
		a.d.metaProblem(w, r, err)
		return
	}
	byDay := map[string]int64{}
	for _, c := range counts {
		byDay[c.Day.UTC().Format(time.DateOnly)] += c.Count
	}
	out := gen.RepoStats{}
	for day := since; !day.After(today); day = day.AddDate(0, 0, 1) {
		n := byDay[day.Format(time.DateOnly)]
		out.Total += n
		out.Days = append(out.Days, struct {
			Count int64              `json:"count"`
			Day   openapi_types.Date `json:"day"`
		}{Count: n, Day: openapi_types.Date{Time: day}})
	}
	httpx.WriteJSON(w, r, http.StatusOK, out)
}
