package api

import (
	"net/http"

	"github.com/hangar-hub/hangar/internal/api/gen"
	"github.com/hangar-hub/hangar/internal/auth"
	"github.com/hangar-hub/hangar/internal/httpx"
	"github.com/hangar-hub/hangar/internal/model"
	"github.com/hangar-hub/hangar/internal/upstream"
)

// upAdmin returns the caller if they are an Instance admin, or writes 401
// or 403. Only Instance admins manage Upstreams (05-auth.md).
func (h *Upstreams) upAdmin(w http.ResponseWriter, r *http.Request) (model.Principal, bool) {
	p := httpx.Principal(r.Context())
	if p.IsAnonymous() {
		upProblem(w, r, http.StatusUnauthorized, "Sign in as an Instance admin.")
		return p, false
	}
	if err := h.Authz.Can(r.Context(), p, auth.ActionInstanceAdmin, auth.Target{}); err != nil {
		upProblem(w, r, http.StatusForbidden, "Only Instance admins can manage Upstreams.")
		return p, false
	}
	return p, true
}

// AdminListUpstreams implements gen.ServerInterface.
func (h *Upstreams) AdminListUpstreams(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.upAdmin(w, r); !ok {
		return
	}
	list, err := h.Service.Registry().List(r.Context(), false)
	if err != nil {
		h.upFail(w, r, err)
		return
	}
	out := gen.UpstreamList{Items: []gen.Upstream{}}
	for _, u := range list {
		out.Items = append(out.Items, upUpstreamResource(u))
	}
	httpx.WriteJSON(w, r, http.StatusOK, out)
}

// AdminCreateUpstream implements gen.ServerInterface.
func (h *Upstreams) AdminCreateUpstream(w http.ResponseWriter, r *http.Request) {
	p, ok := h.upAdmin(w, r)
	if !ok {
		return
	}
	var body gen.UpstreamCreate
	if !upDecode(w, r, &body) {
		return
	}
	params := upstream.UpstreamParams{Name: body.Name, BaseURL: body.BaseUrl, Enabled: true}
	if body.DisplayName != nil {
		params.DisplayName = *body.DisplayName
	}
	if body.Enabled != nil {
		params.Enabled = *body.Enabled
	}
	if body.ViewerUrl != nil {
		params.ViewerURL = *body.ViewerUrl
	}
	if body.NamespacePrefix != nil {
		params.NamespacePrefix = *body.NamespacePrefix
	}
	if body.InstanceToken != nil {
		params.InstanceToken = *body.InstanceToken
	}
	u, err := h.Service.Registry().Create(r.Context(), params, p)
	if err != nil {
		h.upFail(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusCreated, upUpstreamResource(*u))
}

// AdminGetUpstream implements gen.ServerInterface.
func (h *Upstreams) AdminGetUpstream(w http.ResponseWriter, r *http.Request, id gen.ID) {
	if _, ok := h.upAdmin(w, r); !ok {
		return
	}
	u, err := h.Service.Registry().Get(r.Context(), id)
	if err != nil {
		h.upFail(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, upUpstreamResource(*u))
}

// AdminUpdateUpstream implements gen.ServerInterface.
func (h *Upstreams) AdminUpdateUpstream(w http.ResponseWriter, r *http.Request, id gen.ID) {
	p, ok := h.upAdmin(w, r)
	if !ok {
		return
	}
	var body gen.UpstreamUpdate
	if !upDecode(w, r, &body) {
		return
	}
	u, err := h.Service.Registry().Update(r.Context(), id, upstream.UpstreamUpdate{DisplayName: body.DisplayName, BaseURL: body.BaseUrl,
		ViewerURL: body.ViewerUrl, Enabled: body.Enabled, NamespacePrefix: body.NamespacePrefix, InstanceToken: body.InstanceToken}, p)
	if err != nil {
		h.upFail(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, upUpstreamResource(*u))
}

// AdminDeleteUpstream implements gen.ServerInterface.
func (h *Upstreams) AdminDeleteUpstream(w http.ResponseWriter, r *http.Request, id gen.ID) {
	p, ok := h.upAdmin(w, r)
	if !ok {
		return
	}
	if err := h.Service.Registry().Delete(r.Context(), id, p); err != nil {
		h.upFail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// AdminTestUpstream implements gen.ServerInterface: one search with the
// Instance token, timed. Failures are reported in the result, not as errors.
func (h *Upstreams) AdminTestUpstream(w http.ResponseWriter, r *http.Request, id gen.ID) {
	if _, ok := h.upAdmin(w, r); !ok {
		return
	}
	ctx := r.Context()
	u, err := h.Service.Registry().Get(ctx, id)
	if err != nil {
		h.upFail(w, r, err)
		return
	}
	tok, err := h.Service.Registry().InstanceToken(ctx, id)
	if err != nil {
		h.upFail(w, r, err)
		return
	}
	lat, err := upstream.Test(ctx, h.Client, *u, upstream.Credentials{Token: tok})
	ms := int(lat.Milliseconds())
	out := gen.CheckResult{Name: ptr(u.Name), LatencyMs: &ms, Ok: err == nil, Message: "The Upstream answered a search."}
	if err != nil {
		out.Message = "The Upstream did not answer a search: " + err.Error()
	}
	httpx.WriteJSON(w, r, http.StatusOK, out)
}
