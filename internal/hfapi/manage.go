package hfapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/jamesagarside/hangar/internal/auth"
	"github.com/jamesagarside/hangar/internal/httpx"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/org"
	"github.com/jamesagarside/hangar/internal/repo"
)

// serveRepos routes /api/repos/{create,delete,move}.
func (h *Handler) serveRepos(w http.ResponseWriter, r *http.Request, p reqPath) {
	if p.len() != 1 {
		h.notFound(w, r)
		return
	}
	switch p.at(0) {
	case "create":
		h.route(w, r, "/api/repos/create", true, methods{http.MethodPost: h.createRepo})
	case "delete":
		h.route(w, r, "/api/repos/delete", true, methods{http.MethodDelete: h.deleteRepo})
	case "move":
		h.route(w, r, "/api/repos/move", true, methods{http.MethodPost: h.moveRepo})
	default:
		h.notFound(w, r)
	}
}

// repoURL is the Repo's web URL, which clients parse as
// {base}/[datasets/]{ns}/{name}.
func (h *Handler) repoURL(id model.RepoID) string {
	return h.d.Settings.PublicURL + "/" + id.Path()
}

// userError is an error whose text is a full sentence shown to users.
type userError string

func (e userError) Error() string { return string(e) }

// repoTypeParam parses the `type` field of the Repo management endpoints.
func repoTypeParam(s string) (model.RepoType, error) {
	switch strings.ToLower(s) {
	case "space", "spaces", "kernel", "kernels":
		return "", userError("Hangar stores models and datasets; it does not support Spaces or kernels.")
	}
	t, err := model.ParseRepoType(s)
	if err != nil {
		return "", userError("Unknown repo type " + s + ". Use model or dataset.")
	}
	return t, nil
}

// visibilityParam reads `visibility` or the legacy `private` boolean.
func visibilityParam(visibility string, private *bool) (model.Visibility, bool, error) {
	if visibility != "" {
		v, err := model.ParseVisibility(visibility)
		if err != nil {
			return "", false, userError("Unknown visibility " + visibility + ". Use public, internal or private.")
		}
		return v, true, nil
	}
	if private != nil {
		if *private {
			return model.VisibilityPrivate, true, nil
		}
		return model.VisibilityPublic, true, nil
	}
	return "", false, nil
}

// namespaceTarget looks up a Namespace for a permission check.
func (h *Handler) namespaceTarget(w http.ResponseWriter, r *http.Request, name string) (*auth.NamespaceTarget, bool) {
	ns, err := h.d.Orgs.Get(r.Context(), name)
	if err != nil {
		if errors.Is(err, org.ErrNotFound) {
			httpx.WriteHubError(w, r, &httpx.HubError{Status: http.StatusNotFound, Message: "Namespace " + name + " does not exist."})
			return nil, false
		}
		h.internalError(w, r, err)
		return nil, false
	}
	return &auth.NamespaceTarget{ID: ns.ID, Name: ns.Name, Kind: ns.Kind}, true
}

// createRepo serves POST /api/repos/create (contract §7.1). A 409 still
// carries the Repo URL, because `exist_ok=True` reads it.
func (h *Handler) createRepo(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p := httpx.Principal(ctx)
	var req struct {
		Name       string  `json:"name"`
		Org        *string `json:"organization"` //nolint:misspell // the Hub API field name.
		Visibility string  `json:"visibility"`
		Private    *bool   `json:"private"`
		Type       string  `json:"type"`
	}
	if !h.decodeJSON(w, r, &req) {
		return
	}
	if p.IsAnonymous() {
		h.writeForbidden(w, r, "")
		return
	}
	t, err := repoTypeParam(req.Type)
	if err != nil {
		h.writeBadRequest(w, r, err.Error())
		return
	}
	nsName := p.Username
	if req.Org != nil && *req.Org != "" {
		nsName = *req.Org
	}
	id, err := model.ParseRepoID(t, nsName+"/"+req.Name)
	if err != nil {
		h.writeBadRequest(w, r, errMessage(err))
		return
	}
	vis, _, err := visibilityParam(req.Visibility, req.Private)
	if err != nil {
		h.writeBadRequest(w, r, err.Error())
		return
	}
	if vis == "" {
		vis = model.VisibilityPublic
	}
	nt, ok := h.namespaceTarget(w, r, nsName)
	if !ok {
		return
	}
	if err := h.d.Authz.Can(ctx, p, auth.ActionCreateRepo, auth.Target{Namespace: nt}); err != nil {
		if errors.Is(err, auth.ErrForbidden) || errors.Is(err, auth.ErrNotFound) {
			h.writeForbidden(w, r, "You cannot create Repos in "+nsName+".")
			return
		}
		h.internalError(w, r, err)
		return
	}
	rp, err := h.d.Repos.Create(ctx, repo.CreateParams{ID: id, Visibility: vis, Actor: p})
	if err != nil {
		if errors.Is(err, repo.ErrExists) {
			httpx.WriteHubError(w, r, &httpx.HubError{
				Status: http.StatusConflict, Message: "You already created this " + string(t) + " repo",
				Extra: map[string]any{"url": h.repoURL(id)},
			})
			return
		}
		h.serviceError(w, r, id, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, map[string]any{"url": h.repoURL(rp.ID), "name": rp.ID.String(), "id": rp.UUID.String()})
}

// managedRepo loads a Repo named in a JSON body and checks action, without
// redirecting: the request path does not name the Repo.
func (h *Handler) managedRepo(w http.ResponseWriter, r *http.Request, id model.RepoID, action auth.Action) (*repo.Repo, bool) {
	ctx := r.Context()
	httpx.SetRepo(ctx, id)
	rp, err := h.d.Repos.Get(ctx, id)
	if err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			httpx.HubRepoNotFound(w, r, id)
			return nil, false
		}
		h.serviceError(w, r, id, err)
		return nil, false
	}
	p := httpx.Principal(ctx)
	if err := h.d.Authz.Can(ctx, p, auth.ActionRead, target(rp)); err != nil {
		h.denied(w, r, id, err)
		return nil, false
	}
	if err := h.d.Authz.Can(ctx, p, action, target(rp)); err != nil {
		h.denied(w, r, id, err)
		return nil, false
	}
	return rp, true
}

// deleteRepo serves DELETE /api/repos/delete (contract §7.2): a soft
// delete that can be restored within repos.restore_window.
func (h *Handler) deleteRepo(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string  `json:"name"`
		Org  *string `json:"organization"` //nolint:misspell // the Hub API field name.
		Type string  `json:"type"`
	}
	if !h.decodeJSON(w, r, &req) {
		return
	}
	t, err := repoTypeParam(req.Type)
	if err != nil {
		h.writeBadRequest(w, r, err.Error())
		return
	}
	ns := httpx.Principal(r.Context()).Username
	if req.Org != nil && *req.Org != "" {
		ns = *req.Org
	}
	id, err := model.ParseRepoID(t, ns+"/"+req.Name)
	if err != nil {
		httpx.HubRepoNotFound(w, r, model.RepoID{Type: t, Namespace: ns, Name: req.Name})
		return
	}
	// A write Token may delete Repos its user administers, as on the Hub
	// (auth.ActionHubAdmin).
	rp, ok := h.managedRepo(w, r, id, auth.ActionHubAdmin)
	if !ok {
		return
	}
	if err := h.d.Repos.Delete(r.Context(), rp.ID, httpx.Principal(r.Context())); err != nil {
		h.serviceError(w, r, rp.ID, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, map[string]any{})
}

// moveRepo serves POST /api/repos/move (contract §7.3b): a rename, a
// transfer to another Namespace, or both. The old ID keeps redirecting.
func (h *Handler) moveRepo(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p := httpx.Principal(ctx)
	var req struct {
		FromRepo string `json:"fromRepo"`
		ToRepo   string `json:"toRepo"`
		Type     string `json:"type"`
	}
	if !h.decodeJSON(w, r, &req) {
		return
	}
	t, err := repoTypeParam(req.Type)
	if err != nil {
		h.writeBadRequest(w, r, err.Error())
		return
	}
	from, err := model.ParseRepoID(t, req.FromRepo)
	if err != nil {
		h.writeBadRequest(w, r, errMessage(err))
		return
	}
	to, err := model.ParseRepoID(t, req.ToRepo)
	if err != nil {
		h.writeBadRequest(w, r, errMessage(err))
		return
	}
	// A rename within the Repo's Namespace needs what huggingface_hub's
	// move_repo needs on the Hub, a write Token (auth.ActionHubAdmin). A
	// transfer to another Namespace also needs auth.ActionAdmin, and so the
	// admin scope. The check uses the Namespace the Repo is in now, not the
	// one fromRepo names, which can be an old ID that redirects.
	rp, ok := h.managedRepo(w, r, from, auth.ActionHubAdmin)
	if !ok {
		return
	}
	cur := rp
	if !strings.EqualFold(cur.ID.Namespace, to.Namespace) {
		if !h.can(w, r, cur, auth.ActionAdmin) {
			return
		}
		nt, ok := h.namespaceTarget(w, r, to.Namespace)
		if !ok {
			return
		}
		if err := h.d.Authz.Can(ctx, p, auth.ActionCreateRepo, auth.Target{Namespace: nt}); err != nil {
			h.writeForbidden(w, r, "You cannot move Repos into "+to.Namespace+".")
			return
		}
		moved, err := h.d.Repos.Transfer(ctx, cur.ID, to.Namespace, p)
		if err != nil {
			h.serviceError(w, r, cur.ID, err)
			return
		}
		cur = moved
	}
	if cur.ID.Name != to.Name {
		renamed, err := h.d.Repos.Rename(ctx, cur.ID, to.Name, p)
		if err != nil {
			h.serviceError(w, r, cur.ID, err)
			return
		}
		cur = renamed
	}
	httpx.WriteJSON(w, r, http.StatusOK, map[string]any{"url": h.repoURL(cur.ID)})
}

// updateSettings serves PUT /api/{type}s/{repo}/settings (contract §7.3).
// It changes only visibility and gating, which a write Token may do on
// Repos its user administers, as on the Hub (auth.ActionHubAdmin).
func (h *Handler) updateSettings(w http.ResponseWriter, r *http.Request, ref repoRef) {
	rp, ok := h.load(w, r, ref, auth.ActionHubAdmin)
	if !ok {
		return
	}
	var req struct {
		Gated      json.RawMessage `json:"gated"`
		Visibility string          `json:"visibility"`
		Private    *bool           `json:"private"`
	}
	if !h.decodeJSON(w, r, &req) {
		return
	}
	var u repo.SettingsUpdate
	vis, set, err := visibilityParam(req.Visibility, req.Private)
	if err != nil {
		h.writeBadRequest(w, r, err.Error())
		return
	}
	if set {
		u.Visibility = &vis
	}
	if len(req.Gated) > 0 && string(req.Gated) != "null" {
		g := rp.Gating
		var mode any
		if err := json.Unmarshal(req.Gated, &mode); err != nil {
			h.writeBadRequest(w, r, "gated must be false, \"auto\" or \"manual\".")
			return
		}
		switch v := mode.(type) {
		case bool:
			if v {
				h.writeBadRequest(w, r, "gated must be false, \"auto\" or \"manual\".")
				return
			}
			g.Mode = ""
		case string:
			if v != "auto" && v != "manual" {
				h.writeBadRequest(w, r, "gated must be false, \"auto\" or \"manual\".")
				return
			}
			g.Mode = v
		default:
			h.writeBadRequest(w, r, "gated must be false, \"auto\" or \"manual\".")
			return
		}
		u.Gating = &g
	}
	if _, err := h.d.Repos.UpdateSettings(r.Context(), rp.ID, u, httpx.Principal(r.Context())); err != nil {
		h.serviceError(w, r, rp.ID, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, map[string]any{})
}
