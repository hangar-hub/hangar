// Package api holds the native API handlers. Each file implements part of
// the generated gen.ServerInterface on its own type; cmd/hangar composes
// them into one server.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	"github.com/jamesagarside/hangar/internal/api/gen"
	"github.com/jamesagarside/hangar/internal/auth"
	"github.com/jamesagarside/hangar/internal/httpx"
	"github.com/jamesagarside/hangar/internal/jobs"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/repo"
)

// JobRepos finds the Repo a Repo-scoped Job works on. repo.Service
// implements it.
type JobRepos interface {
	GetByUUID(ctx context.Context, id uuid.UUID) (*repo.Repo, error)
}

// Jobs serves /jobs: listing, progress, cancel and retry.
//
// Users see the Jobs they started; Instance admins see every Job. A
// Repo-scoped Job (jobs.Job.Repo set) is also checked against the caller's
// current access to that Repo through Authz, including their Token's
// restrictions: read_files to see it (its progress and result, such as a
// zip download URL, reveal the Repo's files), write to cancel or retry it,
// and admin while the Repo is deleted. So a Token restricted to one Repo
// never sees another Repo's Jobs, and a user who lost access to a private
// Repo no longer sees the Jobs they started on it. A credential with
// Token restrictions sees no Instance-wide Jobs, as its restrictions name
// only Repos and Namespaces.
type Jobs struct {
	Queue jobs.Queue
	// Authz and Repos are required: without them any request that reaches
	// a Repo-scoped Job fails with 500 rather than skip the check.
	Authz auth.Authz
	Repos JobRepos
}

// JobResource converts a Job to its native API form. The /pulls handlers
// use it too.
func JobResource(j *jobs.Job) gen.Job {
	out := gen.Job{
		Id: j.ID, Kind: string(j.Kind), Status: gen.JobStatus(j.Status), Attempts: j.Attempts, MaxAttempts: j.MaxAttempts,
		CreatedAt: j.CreatedAt, StartedAt: j.StartedAt, FinishedAt: j.FinishedAt, Progress: jobProgressResource(j.Progress),
	}
	if !j.RunAfter.IsZero() {
		ra := j.RunAfter
		out.RunAfter = &ra
	}
	if j.Error != "" {
		e := j.Error
		out.Error = &e
	}
	if j.CreatedBy != uuid.Nil {
		c := j.CreatedBy.String()
		out.CreatedBy = &c
	}
	if j.ParentID != uuid.Nil {
		p := j.ParentID
		out.ParentId = &p
	}
	if m := jobJSONObject(j.Payload); m != nil {
		out.Payload = &m
	}
	if m := jobJSONObject(j.Result); m != nil {
		out.Result = &m
	}
	return out
}

// resource is JobResource with the Repo's current ID in payload.repo when
// the payload does not name it, as dataset.convert and mirror.sync Jobs
// carry only the Repo UUID. The Jobs drawer titles Jobs by Repo. repos
// holds the Repos jobAccess read.
func (h *Jobs) resource(j *jobs.Job, repos map[uuid.UUID]*repo.Repo) gen.Job {
	out := JobResource(j)
	r := repos[j.Repo]
	if j.Repo == uuid.Nil || r == nil {
		return out
	}
	if out.Payload == nil {
		m := map[string]any{}
		out.Payload = &m
	}
	if _, ok := (*out.Payload)["repo"]; !ok {
		(*out.Payload)["repo"] = r.ID.String()
	}
	return out
}

func jobJSONObject(raw json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil || len(m) == 0 {
		return nil
	}
	return m
}

func jobProgressResource(p jobs.ProgressUpdate) gen.JobProgress {
	out := gen.JobProgress{
		BytesDone: p.BytesDone, BytesTotal: p.BytesTotal, FilesDone: p.FilesDone, FilesTotal: p.FilesTotal, Rate: float32(p.Rate),
	}
	if p.Rate > 0 && p.BytesTotal > p.BytesDone {
		eta := int(float64(p.BytesTotal-p.BytesDone) / p.Rate)
		out.EtaSeconds = &eta
	}
	if p.Message != "" {
		m := p.Message
		out.Message = &m
	}
	if len(p.Files) > 0 {
		files := make([]gen.FileProgress, len(p.Files))
		for i, f := range p.Files {
			files[i] = gen.FileProgress{Path: f.Path, BytesDone: f.BytesDone, BytesTotal: f.BytesTotal, Status: f.Status}
			if f.Error != "" {
				e := f.Error
				files[i].Error = &e
			}
		}
		out.Files = &files
	}
	return out
}

func platformProblem(w http.ResponseWriter, r *http.Request, status int, detail string) {
	httpx.WriteProblem(w, r, &httpx.Problem{Status: status, Detail: detail})
}

// platformFail logs err and writes a 500 with detail, which must not
// include err: it may name internal paths or hosts.
func platformFail(w http.ResponseWriter, r *http.Request, detail string, err error) {
	slog.ErrorContext(r.Context(), detail, "error", err, "method", r.Method, "request_id", httpx.RequestID(r.Context()))
	platformProblem(w, r, http.StatusInternalServerError, detail)
}

// signedIn returns the caller, or writes 401 for anonymous requests.
func jobsCaller(w http.ResponseWriter, r *http.Request) (model.Principal, bool) {
	p := httpx.Principal(r.Context())
	if p.IsAnonymous() {
		platformProblem(w, r, http.StatusUnauthorized, "Sign in to see Jobs.")
		return p, false
	}
	return p, true
}

// Outcomes of a Job access check other than access.
var (
	// errJobHidden means the caller may not know the Job exists: 404.
	errJobHidden = errors.New("job hidden")
	// errJobReadOnly means the caller may see the Job but not change it: 403.
	errJobReadOnly = errors.New("job read-only")
)

// jobAccess checks p's access to j: to see it, and with write to cancel or
// retry it. It returns nil, errJobHidden, errJobReadOnly, or an error
// reading the Repo or checking permissions. repos caches Repo lookups
// across the Jobs of one request.
func (h *Jobs) jobAccess(ctx context.Context, p model.Principal, j *jobs.Job, write bool, repos map[uuid.UUID]*repo.Repo) error {
	started := j.CreatedBy != uuid.Nil && j.CreatedBy == p.UserID
	if !started && !p.IsAdmin {
		return errJobHidden
	}
	unrestricted := p.Token == nil || p.Token.Restrictions.IsZero()
	if j.Repo == uuid.Nil {
		if !unrestricted {
			return errJobHidden
		}
		return nil
	}
	if h.Authz == nil || h.Repos == nil {
		return errors.New("the Jobs API has no Authz or Repos, so it cannot check access to Repo-scoped Jobs")
	}
	r, ok := repos[j.Repo]
	if !ok {
		var err error
		r, err = h.Repos.GetByUUID(ctx, j.Repo)
		if err != nil && !errors.Is(err, repo.ErrNotFound) {
			return fmt.Errorf("read the Repo of Job %s: %w", j.ID, err)
		}
		repos[j.Repo] = r
	}
	if r == nil {
		// The Repo was purged. Nobody can be checked against it, so only
		// Instance admins with an unrestricted credential see its Jobs.
		if p.IsAdmin && unrestricted {
			return nil
		}
		return errJobHidden
	}
	target := auth.Target{Repo: &auth.RepoTarget{
		ID: r.ID, UUID: r.UUID, NamespaceID: r.NamespaceID, Visibility: r.Visibility, Gated: r.Gating.Enabled(), Mirror: r.Mirror,
	}}
	see, change := auth.ActionReadFiles, auth.ActionWrite
	if r.DeletedAt != nil {
		see, change = auth.ActionAdmin, auth.ActionAdmin
	}
	if err := h.Authz.Can(ctx, p, see, target); err != nil {
		if jobAuthDenied(err) {
			return errJobHidden
		}
		return fmt.Errorf("check access to the Repo of Job %s: %w", j.ID, err)
	}
	if write {
		if err := h.Authz.Can(ctx, p, change, target); err != nil {
			if jobAuthDenied(err) {
				return errJobReadOnly
			}
			return fmt.Errorf("check access to the Repo of Job %s: %w", j.ID, err)
		}
	}
	return nil
}

func jobAuthDenied(err error) bool {
	return errors.Is(err, auth.ErrNotFound) || errors.Is(err, auth.ErrForbidden) || errors.Is(err, auth.ErrGated)
}

// jobListMaxPages bounds how many queue pages one listing reads while
// skipping Jobs the caller may not see. The response then carries the
// cursor where it stopped.
const jobListMaxPages = 10

// ListJobs implements gen.ServerInterface.
func (h *Jobs) ListJobs(w http.ResponseWriter, r *http.Request, params gen.ListJobsParams) {
	p, ok := jobsCaller(w, r)
	if !ok {
		return
	}
	f := jobs.Filter{Limit: jobs.DefaultListLimit}
	if params.Limit != nil {
		if *params.Limit < 1 || *params.Limit > jobs.MaxListLimit {
			platformProblem(w, r, http.StatusBadRequest, "limit must be between 1 and 500.")
			return
		}
		f.Limit = *params.Limit
	}
	if params.Cursor != nil {
		f.Cursor = *params.Cursor
		if _, err := uuid.Parse(f.Cursor); err != nil {
			platformProblem(w, r, http.StatusBadRequest, "The cursor is not valid. Use next_cursor from the previous page.")
			return
		}
	}
	if params.Kind != nil && *params.Kind != "" {
		if !knownJobKind(jobs.Kind(*params.Kind)) {
			platformProblem(w, r, http.StatusBadRequest, "Unknown Job kind "+*params.Kind+".")
			return
		}
		f.Kinds = []jobs.Kind{jobs.Kind(*params.Kind)}
	}
	if params.Status != nil {
		if !params.Status.Valid() {
			platformProblem(w, r, http.StatusBadRequest, "status must be pending, running, succeeded, failed or cancelled.")
			return
		}
		f.Statuses = []jobs.Status{jobs.Status(*params.Status)}
	}
	if !p.IsAdmin || (params.Mine != nil && *params.Mine) {
		f.CreatedBy = p.UserID
		if f.CreatedBy == uuid.Nil {
			httpx.WriteJSON(w, r, http.StatusOK, gen.JobList{Items: []gen.Job{}})
			return
		}
	}
	out, err := h.listVisible(r.Context(), p, f)
	if err != nil {
		platformFail(w, r, "Could not list Jobs.", err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, out)
}

// listVisible pages through the queue until it has f.Limit Jobs the caller
// may see, the queue runs out, or it has read jobListMaxPages pages. The
// cursor it returns is the last Job it examined, so the next page starts
// after the Jobs it skipped.
func (h *Jobs) listVisible(ctx context.Context, p model.Principal, f jobs.Filter) (gen.JobList, error) {
	out := gen.JobList{Items: []gen.Job{}}
	repos := map[uuid.UUID]*repo.Repo{}
	for page := 1; ; page++ {
		list, next, err := h.Queue.List(ctx, f)
		if err != nil {
			return out, err
		}
		for i, j := range list {
			switch err := h.jobAccess(ctx, p, j, false, repos); {
			case errors.Is(err, errJobHidden):
				continue
			case err != nil:
				return out, err
			}
			out.Items = append(out.Items, h.resource(j, repos))
			if len(out.Items) == f.Limit {
				if i < len(list)-1 || next != "" {
					c := j.ID.String()
					out.NextCursor = &c
				}
				return out, nil
			}
		}
		if next == "" {
			return out, nil
		}
		if page == jobListMaxPages {
			out.NextCursor = &next
			return out, nil
		}
		f.Cursor = next
	}
}

func knownJobKind(k jobs.Kind) bool {
	for _, known := range jobs.AllKinds() {
		if k == known {
			return true
		}
	}
	return false
}

// lookup returns a Job the caller may see and, with write, change, or
// writes 401, 403 or 404. A Job the caller may not see is 404 whatever
// their Token's scope; one they may see but not change is 403.
func (h *Jobs) lookup(w http.ResponseWriter, r *http.Request, id gen.ID, write bool, repos map[uuid.UUID]*repo.Repo) (model.Principal, *jobs.Job, bool) {
	p, ok := jobsCaller(w, r)
	if !ok {
		return p, nil, false
	}
	j, err := h.Queue.Get(r.Context(), id)
	if err == nil {
		err = h.jobAccess(r.Context(), p, j, false, repos)
	}
	if err == nil && write {
		if !jobsCanWrite(w, r, p) {
			return p, nil, false
		}
		err = h.jobAccess(r.Context(), p, j, true, repos)
	}
	switch {
	case errors.Is(err, jobs.ErrNotFound) || errors.Is(err, errJobHidden):
		platformProblem(w, r, http.StatusNotFound, "Job not found.")
		return p, nil, false
	case errors.Is(err, errJobReadOnly):
		platformProblem(w, r, http.StatusForbidden, "You need write access to this Job's Repo to cancel or retry the Job.")
		return p, nil, false
	case err != nil:
		platformFail(w, r, "Could not read the Job.", err)
		return p, nil, false
	}
	return p, j, true
}

// GetJob implements gen.ServerInterface.
func (h *Jobs) GetJob(w http.ResponseWriter, r *http.Request, id gen.ID) {
	repos := map[uuid.UUID]*repo.Repo{}
	if _, j, ok := h.lookup(w, r, id, false, repos); ok {
		httpx.WriteJSON(w, r, http.StatusOK, h.resource(j, repos))
	}
}

func jobsCanWrite(w http.ResponseWriter, r *http.Request, p model.Principal) bool {
	if !p.MaxScope().Includes(model.ScopeWrite) {
		platformProblem(w, r, http.StatusForbidden, "This token's scope does not allow changing Jobs. Use a token with write scope.")
		return false
	}
	return true
}

// CancelJob implements gen.ServerInterface.
func (h *Jobs) CancelJob(w http.ResponseWriter, r *http.Request, id gen.ID) {
	repos := map[uuid.UUID]*repo.Repo{}
	if _, _, ok := h.lookup(w, r, id, true, repos); !ok {
		return
	}
	switch err := h.Queue.Cancel(r.Context(), id); {
	case errors.Is(err, jobs.ErrNotActive):
		platformProblem(w, r, http.StatusConflict, "The Job has already finished.")
		return
	case errors.Is(err, jobs.ErrNotFound):
		platformProblem(w, r, http.StatusNotFound, "Job not found.")
		return
	case err != nil:
		platformFail(w, r, "Could not cancel the Job.", err)
		return
	}
	j, err := h.Queue.Get(r.Context(), id)
	if err != nil {
		platformFail(w, r, "Could not read the Job.", err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, h.resource(j, repos))
}

// RetryJob implements gen.ServerInterface. It answers with the new Job.
func (h *Jobs) RetryJob(w http.ResponseWriter, r *http.Request, id gen.ID) {
	repos := map[uuid.UUID]*repo.Repo{}
	if _, _, ok := h.lookup(w, r, id, true, repos); !ok {
		return
	}
	newID, err := h.Queue.Retry(r.Context(), id)
	switch {
	case errors.Is(err, jobs.ErrNotRetrying):
		platformProblem(w, r, http.StatusConflict, "Only failed or cancelled Jobs can be retried.")
		return
	case err != nil:
		platformFail(w, r, "Could not retry the Job.", err)
		return
	}
	j, err := h.Queue.Get(r.Context(), newID)
	if err != nil {
		platformFail(w, r, "Could not read the Job.", err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusAccepted, h.resource(j, repos))
}
