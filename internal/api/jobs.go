// Package api holds the native API handlers. Each file implements part of
// the generated gen.ServerInterface on its own type; cmd/hangar composes
// them into one server.
package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	"github.com/jamesagarside/hangar/internal/api/gen"
	"github.com/jamesagarside/hangar/internal/httpx"
	"github.com/jamesagarside/hangar/internal/jobs"
	"github.com/jamesagarside/hangar/internal/model"
)

// Jobs serves /jobs: listing, progress, cancel and retry. Users see the
// Jobs they started; Instance admins see every Job.
type Jobs struct {
	Queue jobs.Queue
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

// visible reports whether p may see the Job: its creator or an Instance admin.
func jobVisible(p model.Principal, j *jobs.Job) bool {
	return p.IsAdmin || (j.CreatedBy != uuid.Nil && j.CreatedBy == p.UserID)
}

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
	list, next, err := h.Queue.List(r.Context(), f)
	if err != nil {
		platformFail(w, r, "Could not list Jobs.", err)
		return
	}
	out := gen.JobList{Items: make([]gen.Job, 0, len(list))}
	for _, j := range list {
		out.Items = append(out.Items, JobResource(j))
	}
	if next != "" {
		out.NextCursor = &next
	}
	httpx.WriteJSON(w, r, http.StatusOK, out)
}

func knownJobKind(k jobs.Kind) bool {
	for _, known := range jobs.AllKinds() {
		if k == known {
			return true
		}
	}
	return false
}

// lookup returns a Job the caller may see, or writes 401 or 404.
func (h *Jobs) lookup(w http.ResponseWriter, r *http.Request, id gen.ID) (model.Principal, *jobs.Job, bool) {
	p, ok := jobsCaller(w, r)
	if !ok {
		return p, nil, false
	}
	j, err := h.Queue.Get(r.Context(), id)
	if errors.Is(err, jobs.ErrNotFound) || (err == nil && !jobVisible(p, j)) {
		platformProblem(w, r, http.StatusNotFound, "Job not found.")
		return p, nil, false
	}
	if err != nil {
		platformFail(w, r, "Could not read the Job.", err)
		return p, nil, false
	}
	return p, j, true
}

// GetJob implements gen.ServerInterface.
func (h *Jobs) GetJob(w http.ResponseWriter, r *http.Request, id gen.ID) {
	if _, j, ok := h.lookup(w, r, id); ok {
		httpx.WriteJSON(w, r, http.StatusOK, JobResource(j))
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
	p, _, ok := h.lookup(w, r, id)
	if !ok || !jobsCanWrite(w, r, p) {
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
	httpx.WriteJSON(w, r, http.StatusOK, JobResource(j))
}

// RetryJob implements gen.ServerInterface. It answers with the new Job.
func (h *Jobs) RetryJob(w http.ResponseWriter, r *http.Request, id gen.ID) {
	p, _, ok := h.lookup(w, r, id)
	if !ok || !jobsCanWrite(w, r, p) {
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
	httpx.WriteJSON(w, r, http.StatusAccepted, JobResource(j))
}
