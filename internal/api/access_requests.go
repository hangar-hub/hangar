package api

import (
	"context"
	"fmt"
	"net/http"

	"github.com/hangar-hub/hangar/internal/api/gen"
	"github.com/hangar-hub/hangar/internal/auth"
	"github.com/hangar-hub/hangar/internal/httpx"
	"github.com/hangar-hub/hangar/internal/model"
	"github.com/hangar-hub/hangar/internal/org"
	"github.com/hangar-hub/hangar/internal/repo"
)

// AuthRepoTarget describes a Repo for a permission check.
func AuthRepoTarget(r *repo.Repo) *auth.RepoTarget {
	return &auth.RepoTarget{ID: r.ID, UUID: r.UUID, NamespaceID: r.NamespaceID, Visibility: r.Visibility,
		Gated: r.Gating.Enabled(), Mirror: r.Mirror}
}

// AuthRepoResolver adapts a IdentityRepos (repo.Service) to auth.RepoResolver, for
// the Ollama registry token endpoint.
type AuthRepoResolver struct{ Repos IdentityRepos }

var _ auth.RepoResolver = AuthRepoResolver{}

// RepoTarget implements auth.RepoResolver.
func (t AuthRepoResolver) RepoTarget(ctx context.Context, id model.RepoID) (*auth.RepoTarget, error) {
	r, err := t.Repos.Get(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", auth.ErrNotFound, err)
	}
	return AuthRepoTarget(r), nil
}

// repoFor loads a Repo and checks the caller may do action on it. Callers
// who cannot read it get 404.
func (h *Identity) repoFor(w http.ResponseWriter, r *http.Request, typ gen.RepoType, ns, name string, action auth.Action) (*repo.Repo, model.Principal, bool) {
	p := httpx.Principal(r.Context())
	rt, err := model.ParseRepoType(string(typ))
	if err != nil {
		h.fail(w, r, fmt.Errorf("%w: %w", auth.ErrInvalid, err))
		return nil, p, false
	}
	id := model.RepoID{Type: rt, Namespace: ns, Name: name}
	httpx.SetRepo(r.Context(), id)
	rp, err := h.repos.Get(r.Context(), id)
	if err != nil {
		h.fail(w, r, err)
		return nil, p, false
	}
	if err := h.auth.Authorizer.Can(r.Context(), p, action, auth.Target{Repo: AuthRepoTarget(rp)}); err != nil {
		h.fail(w, r, err)
		return nil, p, false
	}
	return rp, p, true
}

func idGatedRepo(rp *repo.Repo) auth.GatedRepo {
	g := auth.GatedRepo{UUID: rp.UUID, ID: rp.ID, Mode: rp.Gating.Mode}
	for _, f := range rp.Gating.Fields {
		g.Fields = append(g.Fields, auth.GatingField{Name: f.Name, Type: f.Type, Required: f.Required})
	}
	return g
}

func idAccessRequestOut(a auth.AccessRequest) gen.AccessRequest {
	fields := a.Fields
	return gen.AccessRequest{Id: a.ID, Username: a.Username, Status: gen.AccessRequestStatus(a.Status), Fields: &fields,
		CreatedAt: a.CreatedAt, DecidedAt: a.DecidedAt, DecidedBy: idStrPtr(a.DecidedBy)}
}

// ListAccessRequests implements GET .../access-requests (Repo admins).
func (h *Identity) ListAccessRequests(w http.ResponseWriter, r *http.Request, typ gen.RepoType, ns gen.Ns, name gen.Name, params gen.ListAccessRequestsParams) {
	rp, _, ok := h.repoFor(w, r, typ, ns, name, auth.ActionAdmin)
	if !ok {
		return
	}
	status := ""
	if params.Status != nil {
		status = string(*params.Status)
	}
	cursor := ""
	if params.Cursor != nil {
		cursor = *params.Cursor
	}
	list, next, err := h.auth.AccessRequests.List(r.Context(), rp.UUID, status, cursor, idLimit(params.Limit))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := gen.AccessRequestList{Items: make([]gen.AccessRequest, 0, len(list)), NextCursor: idNextCursor(next)}
	for _, a := range list {
		out.Items = append(out.Items, idAccessRequestOut(a))
	}
	httpx.WriteJSON(w, r, http.StatusOK, out)
}

// RequestAccess implements POST .../access-requests: a signed-in user asks
// for access to a gated Repo. In auto mode the answer is immediate.
func (h *Identity) RequestAccess(w http.ResponseWriter, r *http.Request, typ gen.RepoType, ns gen.Ns, name gen.Name) {
	if _, ok := h.signedIn(w, r); !ok {
		return
	}
	rp, p, ok := h.repoFor(w, r, typ, ns, name, auth.ActionRead)
	if !ok {
		return
	}
	var body gen.AccessRequestCreate
	if !h.decode(w, r, &body) {
		return
	}
	fields := map[string]any{}
	if body.Fields != nil {
		fields = *body.Fields
	}
	a, err := h.auth.AccessRequests.Request(r.Context(), idGatedRepo(rp), p, fields)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusCreated, idAccessRequestOut(*a))
}

// DecideAccessRequest implements PATCH .../access-requests/{id} (Repo
// admins).
func (h *Identity) DecideAccessRequest(w http.ResponseWriter, r *http.Request, typ gen.RepoType, ns gen.Ns, name gen.Name, id gen.ID) {
	rp, p, ok := h.repoFor(w, r, typ, ns, name, auth.ActionAdmin)
	if !ok {
		return
	}
	var body gen.AccessRequestDecision
	if !h.decode(w, r, &body) {
		return
	}
	a, err := h.auth.AccessRequests.Decide(r.Context(), idGatedRepo(rp), id, string(body.Status), p)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, idAccessRequestOut(*a))
}

// ListCollaborators implements GET .../collaborators (Repo admins).
func (h *Identity) ListCollaborators(w http.ResponseWriter, r *http.Request, typ gen.RepoType, ns gen.Ns, name gen.Name) {
	rp, _, ok := h.repoFor(w, r, typ, ns, name, auth.ActionAdmin)
	if !ok {
		return
	}
	h.writeCollaborators(w, r, rp)
}

func (h *Identity) writeCollaborators(w http.ResponseWriter, r *http.Request, rp *repo.Repo) {
	cs, err := h.orgs.Collaborators(r.Context(), rp.UUID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := gen.CollaboratorList{Items: make([]gen.Collaborator, 0, len(cs))}
	for _, c := range cs {
		out.Items = append(out.Items, gen.Collaborator{Username: c.Username, Role: gen.CollaboratorRole(c.Role)})
	}
	httpx.WriteJSON(w, r, http.StatusOK, out)
}

// SetCollaborators implements PUT .../collaborators (Repo admins): the
// list replaces the current collaborators.
func (h *Identity) SetCollaborators(w http.ResponseWriter, r *http.Request, typ gen.RepoType, ns gen.Ns, name gen.Name) {
	rp, p, ok := h.repoFor(w, r, typ, ns, name, auth.ActionAdmin)
	if !ok {
		return
	}
	var body gen.CollaboratorList
	if !h.decode(w, r, &body) {
		return
	}
	if len(body.Items) > 1000 {
		h.fail(w, r, fmt.Errorf("%w: at most 1000 collaborators", auth.ErrInvalid))
		return
	}
	cs := make([]org.Collaborator, 0, len(body.Items))
	for _, c := range body.Items {
		u, err := h.auth.Users.GetByUsername(r.Context(), c.Username)
		if err != nil {
			h.fail(w, r, fmt.Errorf("%w: user %s does not exist", auth.ErrInvalid, c.Username))
			return
		}
		cs = append(cs, org.Collaborator{UserID: u.ID, Role: model.Role(c.Role)})
	}
	if err := h.orgs.SetCollaborators(r.Context(), rp.UUID, cs, p); err != nil {
		h.fail(w, r, err)
		return
	}
	h.writeCollaborators(w, r, rp)
}
