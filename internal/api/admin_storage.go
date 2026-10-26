package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/hangar-hub/hangar/internal/api/gen"
	"github.com/hangar-hub/hangar/internal/audit"
	"github.com/hangar-hub/hangar/internal/auth"
	"github.com/hangar-hub/hangar/internal/gc"
	"github.com/hangar-hub/hangar/internal/httpx"
	"github.com/hangar-hub/hangar/internal/jobs"
	"github.com/hangar-hub/hangar/internal/model"
)

// AdminStorage serves GET /admin/storage, PUT /admin/quotas/{ns} and
// POST /admin/gc (OPS-4). Every endpoint needs an Instance admin.
type AdminStorage struct {
	Authz      auth.Authz
	Accounting *gc.Accounting
	Queue      jobs.Queue
	Audit      audit.Recorder
}

// platformAdmin returns the caller if they are an Instance admin, and
// otherwise writes 401 or 403.
func platformAdmin(w http.ResponseWriter, r *http.Request, authz auth.Authz) (model.Principal, bool) {
	p := httpx.Principal(r.Context())
	if p.IsAnonymous() {
		platformProblem(w, r, http.StatusUnauthorized, "Sign in as an Instance admin.")
		return p, false
	}
	if err := authz.Can(r.Context(), p, auth.ActionInstanceAdmin, auth.Target{}); err != nil {
		if errors.Is(err, auth.ErrForbidden) || errors.Is(err, auth.ErrNotFound) {
			platformProblem(w, r, http.StatusForbidden, "Only Instance admins can do this, with a session or an admin-scope Token.")
			return p, false
		}
		platformFail(w, r, "Could not check permissions.", err)
		return p, false
	}
	return p, true
}

// AdminGetStorage implements gen.ServerInterface.
func (h *AdminStorage) AdminGetStorage(w http.ResponseWriter, r *http.Request) {
	if _, ok := platformAdmin(w, r, h.Authz); !ok {
		return
	}
	u, err := h.Accounting.Usage(r.Context())
	if err != nil {
		platformFail(w, r, "Could not compute storage usage.", err)
		return
	}
	out := gen.StorageUsage{
		TotalLfsBytes: u.TotalLFSBytes, TotalGitBytes: u.TotalGitBytes, XetCacheBytes: u.XetCacheBytes,
		Namespaces: make([]gen.NamespaceUsage, 0, len(u.Namespaces)),
	}
	for _, n := range u.Namespaces {
		nu := gen.NamespaceUsage{Namespace: n.Namespace, LfsBytes: n.LFSBytes, GitBytes: n.GitBytes, RepoCount: n.RepoCount}
		if n.QuotaBytes > 0 {
			q := n.QuotaBytes
			nu.QuotaBytes = &q
		}
		out.Namespaces = append(out.Namespaces, nu)
	}
	last, _, err := h.Queue.List(r.Context(), jobs.Filter{Kinds: []jobs.Kind{jobs.KindGC}, Statuses: []jobs.Status{jobs.StatusSucceeded}, Limit: 1})
	if err != nil {
		platformFail(w, r, "Could not read the last garbage collection.", err)
		return
	}
	if len(last) == 1 && last[0].FinishedAt != nil {
		out.LastGcAt = last[0].FinishedAt
	}
	httpx.WriteJSON(w, r, http.StatusOK, out)
}

// AdminSetQuota implements gen.ServerInterface. A limit of 0 removes the quota.
func (h *AdminStorage) AdminSetQuota(w http.ResponseWriter, r *http.Request, ns gen.Ns) {
	p, ok := platformAdmin(w, r, h.Authz)
	if !ok {
		return
	}
	var body gen.Quota
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		httpx.WriteProblem(w, r, &httpx.Problem{Status: http.StatusBadRequest, Detail: "The body must be a JSON object with bytes_limit.",
			Errors: []httpx.ProblemError{{Pointer: "/bytes_limit", Detail: "Give the quota in bytes, or 0 to remove it."}}})
		return
	}
	if body.BytesLimit < 0 {
		httpx.WriteProblem(w, r, &httpx.Problem{Status: http.StatusBadRequest, Detail: "The quota cannot be negative.",
			Errors: []httpx.ProblemError{{Pointer: "/bytes_limit", Detail: "Use 0 to remove the quota."}}})
		return
	}
	id, name, err := h.Accounting.NamespaceByName(r.Context(), ns)
	if errors.Is(err, gc.ErrNamespaceNotFound) {
		platformProblem(w, r, http.StatusNotFound, "Namespace "+ns+" not found.")
		return
	}
	if err != nil {
		platformFail(w, r, "Could not read the Namespace.", err)
		return
	}
	previous, err := h.Accounting.Quota(r.Context(), id)
	if err != nil {
		platformFail(w, r, "Could not read the quota.", err)
		return
	}
	if err := h.Accounting.SetQuota(r.Context(), id, body.BytesLimit); err != nil {
		platformFail(w, r, "Could not set the quota.", err)
		return
	}
	if h.Audit != nil {
		if err := h.Audit.Record(r.Context(), audit.Event{
			Action: audit.QuotaChanged, Actor: p, TargetType: "namespace", TargetID: name,
			IP: httpx.ClientIP(r), UserAgent: r.UserAgent(),
			Data: map[string]any{"bytes_limit": body.BytesLimit, "previous_bytes_limit": previous},
		}); err != nil {
			platformFail(w, r, "The quota is set, but the audit event could not be recorded.", err)
			return
		}
	}
	httpx.WriteJSON(w, r, http.StatusOK, gen.Quota{BytesLimit: body.BytesLimit})
}

// AdminRunGC implements gen.ServerInterface. It starts a full gc run, or
// joins the one already pending or running.
func (h *AdminStorage) AdminRunGC(w http.ResponseWriter, r *http.Request) {
	p, ok := platformAdmin(w, r, h.Authz)
	if !ok {
		return
	}
	id, err := gc.Start(r.Context(), h.Queue, gc.ScopeAll, p.UserID)
	if err != nil {
		platformFail(w, r, "Could not start garbage collection.", err)
		return
	}
	j, err := h.Queue.Get(r.Context(), id)
	if err != nil {
		platformFail(w, r, "Could not read the garbage collection Job.", err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusAccepted, JobResource(j))
}
