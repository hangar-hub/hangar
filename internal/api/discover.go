package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/google/uuid"

	"github.com/jamesagarside/hangar/internal/api/gen"
	"github.com/jamesagarside/hangar/internal/auth"
	"github.com/jamesagarside/hangar/internal/httpx"
	"github.com/jamesagarside/hangar/internal/jobs"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/repo"
	"github.com/jamesagarside/hangar/internal/upstream"
	"github.com/jamesagarside/hangar/internal/upstream/deps"
)

// Upstreams serves Discover, Pulls, Mirrors, Dependencies and the admin
// Upstream endpoints (W6): discover.go, pulls.go, mirrors.go and
// admin_upstreams.go.
type Upstreams struct {
	Service *upstream.Service
	Client  upstream.Client
	Repos   repo.Service
	Authz   auth.Authz
	Queue   jobs.Queue
	Log     *slog.Logger
}

func (h *Upstreams) log() *slog.Logger {
	if h.Log != nil {
		return h.Log
	}
	return slog.Default()
}

func upProblem(w http.ResponseWriter, r *http.Request, status int, detail string) {
	httpx.WriteProblem(w, r, &httpx.Problem{Status: status, Detail: detail})
}

// upFail answers an error from the upstream package. Messages of known
// errors are safe to show; unknown errors are logged and hidden.
func (h *Upstreams) upFail(w http.ResponseWriter, r *http.Request, err error) {
	var rl *upstream.RateLimitError
	switch {
	case clientGone(r, err):
		writeClientGone(w, r, h.log(), err)
	case errors.As(err, &rl):
		w.Header().Set("Retry-After", strconv.Itoa(max(1, int(rl.RetryAfter.Seconds()))))
		upProblem(w, r, http.StatusServiceUnavailable, "The Upstream is rate limiting Hangar. Try again later.")
	case errors.Is(err, upstream.ErrUpstreamNotFound):
		upProblem(w, r, http.StatusNotFound, "Upstream not found.")
	case errors.Is(err, upstream.ErrUpstreamDisabled):
		upProblem(w, r, http.StatusConflict, "That Upstream is disabled.")
	case errors.Is(err, upstream.ErrInvalidSelection), errors.Is(err, upstream.ErrUpstreamInvalid):
		upProblem(w, r, http.StatusBadRequest, err.Error())
	case errors.Is(err, upstream.ErrConflict), errors.Is(err, upstream.ErrUpstreamExists), errors.Is(err, upstream.ErrUpstreamConfigured),
		errors.Is(err, upstream.ErrUpstreamInUse):
		upProblem(w, r, http.StatusConflict, err.Error())
	case errors.Is(err, upstream.ErrOverrideNotAllowed):
		upProblem(w, r, http.StatusForbidden, "Pulling outside the pull windows is turned off on this Instance.")
	case errors.Is(err, upstream.ErrNotMirror):
		upProblem(w, r, http.StatusNotFound, "This Repo is not a Mirror.")
	case errors.Is(err, auth.ErrNotFound), errors.Is(err, repo.ErrNotFound):
		upProblem(w, r, http.StatusNotFound, "Repo not found.")
	case errors.Is(err, auth.ErrForbidden):
		upProblem(w, r, http.StatusForbidden, err.Error())
	case errors.Is(err, upstream.ErrGated):
		upProblem(w, r, http.StatusForbidden, "The upstream Repo is gated. Request access on the Upstream, then save your token for it in Settings > Upstream tokens.")
	case errors.Is(err, upstream.ErrUnauthorized):
		upProblem(w, r, http.StatusForbidden, "The Upstream rejected the credentials. Check your token for it in Settings > Upstream tokens.")
	case errors.Is(err, upstream.ErrNotFound):
		upProblem(w, r, http.StatusNotFound, "Not found on the Upstream, or the Upstream does not show it to your credentials.")
	case errors.Is(err, upstream.ErrUnavailable):
		h.log().WarnContext(r.Context(), "upstream unavailable", "error", err.Error(), "request_id", httpx.RequestID(r.Context()))
		upProblem(w, r, http.StatusBadGateway, "The Upstream is not answering. Try again later.")
	default:
		h.log().ErrorContext(r.Context(), "upstream request failed", "error", err.Error(), "request_id", httpx.RequestID(r.Context()))
		upProblem(w, r, http.StatusInternalServerError, "The request failed.")
	}
}

// upSignedIn returns the caller, or writes 401.
func upSignedIn(w http.ResponseWriter, r *http.Request) (model.Principal, bool) {
	p := httpx.Principal(r.Context())
	if p.IsAnonymous() {
		upProblem(w, r, http.StatusUnauthorized, "Sign in to search Upstreams and pull into the arsenal.")
		return p, false
	}
	return p, true
}

func upRepoType(t gen.RepoType) (model.RepoType, bool) {
	switch t {
	case gen.Models:
		return model.RepoTypeModel, true
	case gen.Datasets:
		return model.RepoTypeDataset, true
	}
	return "", false
}

// upLoadRepo reads a Repo the caller can see, or writes 404.
func (h *Upstreams) upLoadRepo(w http.ResponseWriter, r *http.Request, pType gen.RepoType, ns, name string) (*repo.Repo, model.Principal, bool) {
	p := httpx.Principal(r.Context())
	t, ok := upRepoType(pType)
	if !ok {
		upProblem(w, r, http.StatusNotFound, "Repo not found.")
		return nil, p, false
	}
	id := model.RepoID{Type: t, Namespace: ns, Name: name}
	httpx.SetRepo(r.Context(), id)
	rp, err := h.Repos.Get(r.Context(), id)
	if err == nil {
		err = h.Authz.Can(r.Context(), p, auth.ActionRead, auth.Target{Repo: &auth.RepoTarget{ID: rp.ID, UUID: rp.UUID, NamespaceID: rp.NamespaceID,
			Visibility: rp.Visibility, Gated: rp.Gating.Enabled(), Mirror: rp.Mirror}})
	}
	if err != nil {
		if errors.Is(err, repo.ErrNotFound) || errors.Is(err, auth.ErrNotFound) || errors.Is(err, repo.ErrInvalid) || errors.Is(err, auth.ErrForbidden) {
			if p.IsAnonymous() {
				upProblem(w, r, http.StatusUnauthorized, "Repo not found. Sign in if it is private.")
			} else {
				upProblem(w, r, http.StatusNotFound, "Repo not found.")
			}
			return nil, p, false
		}
		h.upFail(w, r, err)
		return nil, p, false
	}
	return rp, p, true
}

func upUpstreamResource(u upstream.Upstream) gen.Upstream {
	out := gen.Upstream{Id: u.ID, Name: u.Name, DisplayName: u.DisplayName, BaseUrl: u.BaseURL, Enabled: u.Enabled}
	tok := u.HasInstanceToken
	out.HasInstanceToken = &tok
	if u.NamespacePrefix != "" {
		p := u.NamespacePrefix
		out.NamespacePrefix = &p
	}
	if v := u.DatasetViewerURL(); v != "" {
		out.ViewerUrl = &v
	}
	if u.Source != "" {
		src := gen.UpstreamSource(u.Source)
		out.Source = &src
	}
	return out
}

// ListUpstreams implements gen.ServerInterface: the enabled Upstreams.
func (h *Upstreams) ListUpstreams(w http.ResponseWriter, r *http.Request) {
	if _, ok := upSignedIn(w, r); !ok {
		return
	}
	list, err := h.Service.Registry().List(r.Context(), true)
	if err != nil {
		h.upFail(w, r, err)
		return
	}
	out := gen.UpstreamList{Items: []gen.Upstream{}}
	for _, u := range list {
		res := upUpstreamResource(u)
		res.HasInstanceToken = nil
		out.Items = append(out.Items, res)
	}
	httpx.WriteJSON(w, r, http.StatusOK, out)
}

func ptr[T any](v T) *T { return &v }

// Discover implements gen.ServerInterface (BROWSE-2, DATA-7).
func (h *Upstreams) Discover(w http.ResponseWriter, r *http.Request, pType gen.RepoType, params gen.DiscoverParams) {
	p, ok := upSignedIn(w, r)
	if !ok {
		return
	}
	t, ok := upRepoType(pType)
	if !ok {
		upProblem(w, r, http.StatusBadRequest, "type must be models or datasets.")
		return
	}
	q := upstream.SearchQuery{Type: t, Limit: 50}
	if params.Limit != nil {
		if *params.Limit < 1 || *params.Limit > 100 {
			upProblem(w, r, http.StatusBadRequest, "limit must be between 1 and 100.")
			return
		}
		q.Limit = *params.Limit
	}
	if params.Search != nil {
		q.Search = *params.Search
	}
	if params.Author != nil {
		q.Author = *params.Author
	}
	if params.Filter != nil {
		q.Filter = *params.Filter
	}
	if params.Sort != nil {
		if !params.Sort.Valid() {
			upProblem(w, r, http.StatusBadRequest, "sort must be trending, downloads, likes or lastModified.")
			return
		}
		q.Sort = string(*params.Sort)
	}
	if params.Cursor != nil {
		q.Cursor = *params.Cursor
	}
	name := ""
	if params.Upstream != nil {
		name = *params.Upstream
	}
	page, err := h.Service.Discover(r.Context(), p, name, q)
	if err != nil {
		h.upFail(w, r, err)
		return
	}
	out := gen.DiscoverList{Items: []gen.DiscoverItem{}}
	if page.NextCursor != "" {
		out.NextCursor = ptr(page.NextCursor)
	}
	for _, it := range page.Items {
		item := gen.DiscoverItem{Id: it.ID.String(), Type: gen.RepoTypeSingular(it.ID.Type), Arsenal: gen.ArsenalState(it.Arsenal),
			Downloads: ptr(it.Downloads), Likes: ptr(it.Likes), Private: ptr(it.Private), Tags: ptr(it.Tags)}
		if it.Tags == nil {
			item.Tags = ptr([]string{})
		}
		if !it.LastModified.IsZero() {
			item.LastModified = ptr(it.LastModified)
		}
		if it.Gated != "" {
			item.Gated = ptr(it.Gated)
		}
		if it.PipelineTag != "" {
			item.PipelineTag = ptr(it.PipelineTag)
		}
		if it.LibraryName != "" {
			item.LibraryName = ptr(it.LibraryName)
		}
		if !it.SHA.IsZero() {
			item.Sha = ptr(string(it.SHA))
		}
		if !it.LocalSHA.IsZero() {
			item.LocalSha = ptr(string(it.LocalSHA))
		}
		out.Items = append(out.Items, item)
	}
	httpx.WriteJSON(w, r, http.StatusOK, out)
}

func upDependency(d deps.Dependency, state upstream.ArsenalState) gen.Dependency {
	out := gen.Dependency{Repo: d.Target.String(), Type: gen.RepoTypeSingular(d.Target.Type), Kind: gen.DependencyKind(d.Kind), Required: d.Required,
		Arsenal: gen.ArsenalState(state)}
	if d.Revision != "" {
		out.Revision = ptr(d.Revision)
	}
	if d.SourcePath != "" {
		out.SourcePath = ptr(d.SourcePath)
	}
	if d.SourceKey != "" {
		out.SourceKey = ptr(d.SourceKey)
	}
	return out
}

// GetDiscoverDetail implements gen.ServerInterface.
func (h *Upstreams) GetDiscoverDetail(w http.ResponseWriter, r *http.Request, pType gen.RepoType, ns gen.Ns, name gen.Name, params gen.GetDiscoverDetailParams) {
	p, ok := upSignedIn(w, r)
	if !ok {
		return
	}
	t, ok := upRepoType(pType)
	if !ok {
		upProblem(w, r, http.StatusBadRequest, "type must be models or datasets.")
		return
	}
	id := model.RepoID{Type: t, Namespace: ns, Name: name}
	if err := id.Validate(); err != nil {
		upProblem(w, r, http.StatusBadRequest, err.Error())
		return
	}
	up, rev := "", ""
	if params.Upstream != nil {
		up = *params.Upstream
	}
	if params.Rev != nil {
		rev = *params.Rev
	}
	det, err := h.Service.DiscoverDetail(r.Context(), p, up, id, rev)
	if err != nil {
		h.upFail(w, r, err)
		return
	}
	info := det.Info
	out := gen.DiscoverDetail{Id: info.ID.String(), Type: gen.RepoTypeSingular(t), Arsenal: gen.ArsenalState(det.Arsenal), Sha: ptr(string(info.SHA)),
		Files: []gen.DiscoverFile{}, Dependencies: []gen.Dependency{}, TotalSize: det.TotalSize, Downloads: ptr(info.Downloads), Likes: ptr(info.Likes),
		Private: ptr(info.Private), Tags: ptr(info.Tags)}
	if info.Tags == nil {
		out.Tags = ptr([]string{})
	}
	if !info.LastModified.IsZero() {
		out.LastModified = ptr(info.LastModified)
	}
	if info.Gated != "" {
		out.Gated = ptr(info.Gated)
	}
	if info.PipelineTag != "" {
		out.PipelineTag = ptr(info.PipelineTag)
	}
	if info.LibraryName != "" {
		out.LibraryName = ptr(info.LibraryName)
	}
	if info.CardData != nil {
		out.CardData = ptr(info.CardData)
	}
	if det.Card != "" {
		out.Card = ptr(det.Card)
	}
	if !det.LocalSHA.IsZero() {
		out.LocalSha = ptr(string(det.LocalSHA))
	}
	for _, f := range det.Files {
		out.Files = append(out.Files, gen.DiscoverFile{Path: f.Path, Size: f.Size, Lfs: f.LFS, Present: f.Present})
	}
	if len(det.Quants) > 0 {
		qs := make([]gen.QuantGroup, 0, len(det.Quants))
		for _, q := range det.Quants {
			g := gen.QuantGroup{Name: q.Name, Files: q.Files, Size: q.Size, Present: q.Present}
			if len(q.Projectors) > 0 {
				g.Projectors = ptr(q.Projectors)
			}
			qs = append(qs, g)
		}
		out.Quants = &qs
	}
	for _, d := range det.Dependencies {
		out.Dependencies = append(out.Dependencies, upDependency(d.Dependency, d.Arsenal))
	}
	if len(det.Configs) > 0 {
		cs := make([]gen.DatasetConfig, 0, len(det.Configs))
		for i, c := range det.Configs {
			dc := gen.DatasetConfig{Config: c.Config, Default: i == 0, Splits: []gen.DatasetSplit{}}
			for _, s := range c.Splits {
				dc.Splits = append(dc.Splits, gen.DatasetSplit{Split: s.Split, NumRows: s.NumRows, NumBytesParquet: s.NumBytes, Status: gen.DatasetSplitStatusReady})
			}
			cs = append(cs, dc)
		}
		out.Configs = &cs
	}
	if len(det.Preview) > 0 {
		var m map[string]any
		if json.Unmarshal(det.Preview, &m) == nil {
			out.Preview = &m
		}
	}
	httpx.WriteJSON(w, r, http.StatusOK, out)
}

// GetDependencies implements gen.ServerInterface: what the Repo needs and
// which Repos the caller can see need it (PULL-11).
func (h *Upstreams) GetDependencies(w http.ResponseWriter, r *http.Request, pType gen.RepoType, ns gen.Ns, name gen.Name) {
	rp, p, ok := h.upLoadRepo(w, r, pType, ns, name)
	if !ok {
		return
	}
	g, err := h.Service.Graph(r.Context(), p, rp)
	if err != nil {
		h.upFail(w, r, err)
		return
	}
	out := gen.Dependencies{Needs: []gen.Dependency{}, NeededBy: []gen.Dependency{}}
	for _, d := range g.Needs {
		out.Needs = append(out.Needs, upDependency(d.Dependency, d.Arsenal))
	}
	for _, d := range g.NeededBy {
		dep := upDependency(d.Dependency, d.Arsenal)
		dep.Repo, dep.Type = d.Repo.String(), gen.RepoTypeSingular(d.Repo.Type)
		out.NeededBy = append(out.NeededBy, dep)
	}
	httpx.WriteJSON(w, r, http.StatusOK, out)
}

// upJobResource converts a Job to its native API form (as api.JobResource
// on W1's branch does).
func upJobResource(j *jobs.Job) gen.Job {
	out := gen.Job{Id: j.ID, Kind: string(j.Kind), Status: gen.JobStatus(j.Status), Attempts: j.Attempts, MaxAttempts: j.MaxAttempts,
		CreatedAt: j.CreatedAt, StartedAt: j.StartedAt, FinishedAt: j.FinishedAt}
	pr := j.Progress
	out.Progress = gen.JobProgress{BytesDone: pr.BytesDone, BytesTotal: pr.BytesTotal, FilesDone: pr.FilesDone, FilesTotal: pr.FilesTotal, Rate: float32(pr.Rate)}
	if pr.Rate > 0 && pr.BytesTotal > pr.BytesDone {
		eta := int(float64(pr.BytesTotal-pr.BytesDone) / pr.Rate)
		out.Progress.EtaSeconds = &eta
	}
	if pr.Message != "" {
		out.Progress.Message = ptr(pr.Message)
	}
	if len(pr.Files) > 0 {
		files := make([]gen.FileProgress, len(pr.Files))
		for i, f := range pr.Files {
			files[i] = gen.FileProgress{Path: f.Path, BytesDone: f.BytesDone, BytesTotal: f.BytesTotal, Status: f.Status}
			if f.Error != "" {
				files[i].Error = ptr(f.Error)
			}
		}
		out.Progress.Files = &files
	}
	if !j.RunAfter.IsZero() {
		out.RunAfter = ptr(j.RunAfter)
	}
	if j.Error != "" {
		out.Error = ptr(j.Error)
	}
	if j.CreatedBy != uuid.Nil {
		out.CreatedBy = ptr(j.CreatedBy.String())
	}
	if j.ParentID != uuid.Nil {
		out.ParentId = ptr(j.ParentID)
	}
	for _, kv := range []struct {
		raw json.RawMessage
		dst **map[string]any
	}{{j.Payload, &out.Payload}, {j.Result, &out.Result}} {
		var m map[string]any
		if len(kv.raw) > 0 && json.Unmarshal(kv.raw, &m) == nil && len(m) > 0 {
			*kv.dst = &m
		}
	}
	return out
}
