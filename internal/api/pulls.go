package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/hangar-hub/hangar/internal/api/gen"
	"github.com/hangar-hub/hangar/internal/httpx"
	"github.com/hangar-hub/hangar/internal/jobs"
	"github.com/hangar-hub/hangar/internal/model"
	"github.com/hangar-hub/hangar/internal/upstream"
)

// upSelection converts the API Selection. An explicit empty `paths`
// list with nothing else selects no files: a history-only Pull.
func upSelection(s *gen.Selection) upstream.Selection {
	if s == nil {
		return upstream.Selection{}
	}
	if s.Paths != nil && len(*s.Paths) == 0 && s.Include == nil && s.Quants == nil && s.Configs == nil && s.Splits == nil &&
		(s.ParquetOnly == nil || !*s.ParquetOnly) {
		out := upstream.Selection{None: true}
		return out
	}
	val := func(p *[]string) []string {
		if p == nil {
			return nil
		}
		return *p
	}
	out := upstream.Selection{Paths: val(s.Paths), Include: val(s.Include), Exclude: val(s.Exclude), Quants: val(s.Quants),
		Configs: val(s.Configs), Splits: val(s.Splits)}
	if s.ParquetOnly != nil {
		out.ParquetOnly = *s.ParquetOnly
	}
	return out
}

func upSelectionResource(s upstream.Selection) gen.Selection {
	out := gen.Selection{}
	set := func(dst **[]string, v []string) {
		if len(v) > 0 {
			c := append([]string(nil), v...)
			*dst = &c
		}
	}
	set(&out.Paths, s.Paths)
	set(&out.Include, s.Include)
	set(&out.Exclude, s.Exclude)
	set(&out.Quants, s.Quants)
	set(&out.Configs, s.Configs)
	set(&out.Splits, s.Splits)
	if s.ParquetOnly {
		out.ParquetOnly = ptr(true)
	}
	if s.None {
		out.Paths = ptr([]string{})
	}
	return out
}

func upDecode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		upProblem(w, r, http.StatusBadRequest, "The request body is not valid JSON for this endpoint.")
		return false
	}
	return true
}

func (h *Upstreams) writeJob(w http.ResponseWriter, r *http.Request, status int, id jobs.JobID) {
	j, err := h.Queue.Get(r.Context(), id)
	if err != nil {
		h.upFail(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, status, upJobResource(j))
}

// StartPull implements gen.ServerInterface: Pull a Repo, or some of its
// files, into the Arsenal (PULL-2). The answer is the pull Job.
func (h *Upstreams) StartPull(w http.ResponseWriter, r *http.Request) {
	p, ok := upSignedIn(w, r)
	if !ok {
		return
	}
	var body gen.PullCreate
	if !upDecode(w, r, &body) {
		return
	}
	t, err := model.ParseRepoType(string(body.Type))
	if err != nil {
		upProblem(w, r, http.StatusBadRequest, "type must be model or dataset.")
		return
	}
	id, err := model.ParseRepoID(t, body.Repo)
	if err != nil {
		upProblem(w, r, http.StatusBadRequest, err.Error())
		return
	}
	req := upstream.PullRequest{Upstream: body.Upstream, Repo: id, Selection: upSelection(body.Selection), Actor: p}
	if body.Revision != nil {
		req.Revision = *body.Revision
	}
	if body.Track != nil {
		req.Track = *body.Track
	}
	if body.Now != nil {
		req.Now = *body.Now
	}
	jobID, _, err := h.Service.StartPull(r.Context(), req)
	if err != nil {
		h.upFail(w, r, err)
		return
	}
	h.writeJob(w, r, http.StatusAccepted, jobID)
}

// GetPull implements gen.ServerInterface: a Pull with per-file progress.
// Users see the Pulls they started; Instance admins see every Pull.
func (h *Upstreams) GetPull(w http.ResponseWriter, r *http.Request, id gen.ID) {
	p, ok := upSignedIn(w, r)
	if !ok {
		return
	}
	j, err := h.Queue.Get(r.Context(), id)
	if errors.Is(err, jobs.ErrNotFound) || (err == nil && (j.Kind != jobs.KindPull || !upCanSeeJob(p, j))) {
		upProblem(w, r, http.StatusNotFound, "Pull not found.")
		return
	}
	if err != nil {
		h.upFail(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, upJobResource(j))
}

// upCanSeeJob reports whether p may see the Job: its creator or an Instance admin.
func upCanSeeJob(p model.Principal, j *jobs.Job) bool {
	return p.IsAdmin || (j.CreatedBy != uuid.Nil && j.CreatedBy == p.UserID)
}
