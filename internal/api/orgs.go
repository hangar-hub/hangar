package api

import (
	"errors"
	"net/http"

	"github.com/jamesagarside/hangar/internal/api/gen"
	"github.com/jamesagarside/hangar/internal/auth"
	"github.com/jamesagarside/hangar/internal/httpx"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/org"
)

func idNamespaceOut(ns *org.Namespace, members int64) gen.Namespace {
	out := gen.Namespace{Name: ns.Name, Kind: gen.NamespaceKind(ns.Kind), DisplayName: ns.DisplayName,
		Description: idStrPtr(ns.Description), CreatedAt: &ns.CreatedAt}
	if ns.ReservedMirror {
		t := true
		out.ReservedMirror = &t
	}
	if ns.Kind == org.KindOrg {
		n := int(members)
		out.MemberCount = &n
	}
	if out.DisplayName == "" {
		out.DisplayName = ns.Name
	}
	return out
}

func idMemberOut(m org.Member) gen.Member {
	return gen.Member{Username: m.Username, DisplayName: idStrPtr(m.DisplayName), Role: gen.Role(m.Role), Source: gen.MemberSource(m.Source)}
}

// orgTarget loads an Organisation and checks the caller may manage it.
func (h *Identity) orgTarget(w http.ResponseWriter, r *http.Request, name string, action auth.Action) (*org.Namespace, model.Principal, bool) {
	p := httpx.Principal(r.Context())
	ns, err := h.orgs.Get(r.Context(), name)
	if err == nil && ns.Kind != org.KindOrg {
		err = org.ErrNotFound
	}
	if err != nil {
		h.fail(w, r, err)
		return nil, p, false
	}
	if err := h.auth.Authorizer.Can(r.Context(), p, action, auth.Target{Namespace: &auth.NamespaceTarget{ID: ns.ID, Name: ns.Name, Kind: ns.Kind}}); err != nil {
		h.fail(w, r, err)
		return nil, p, false
	}
	return ns, p, true
}

// CreateOrg implements POST /orgs. The creator becomes its admin.
func (h *Identity) CreateOrg(w http.ResponseWriter, r *http.Request) {
	p, ok := h.signedIn(w, r)
	if !ok || !h.needScope(w, r, p, model.ScopeWrite) {
		return
	}
	if p.Token != nil && !p.Token.Restrictions.IsZero() {
		httpx.WriteProblem(w, r, &httpx.Problem{Status: http.StatusForbidden, Detail: "A restricted token cannot create Organisations."})
		return
	}
	var body gen.OrgCreate
	if !h.decode(w, r, &body) {
		return
	}
	ns, err := h.orgs.CreateOrg(r.Context(), body.Name, idStr(body.DisplayName), p)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if body.Description != nil {
		ns, err = h.orgs.UpdateOrg(r.Context(), ns.Name, org.OrgUpdate{Description: body.Description}, p)
		if err != nil {
			h.fail(w, r, err)
			return
		}
	}
	httpx.WriteJSON(w, r, http.StatusCreated, idNamespaceOut(ns, 1))
}

// UpdateOrg implements PATCH /orgs/{ns}. Organisation admins only.
func (h *Identity) UpdateOrg(w http.ResponseWriter, r *http.Request, name gen.Ns) {
	ns, p, ok := h.orgTarget(w, r, name, auth.ActionManageNamespace)
	if !ok {
		return
	}
	var body gen.OrgUpdate
	if !h.decode(w, r, &body) {
		return
	}
	ns, err := h.orgs.UpdateOrg(r.Context(), ns.Name, org.OrgUpdate{DisplayName: body.DisplayName, Description: body.Description}, p)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	n, err := h.orgs.MemberCount(r.Context(), ns.ID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, idNamespaceOut(ns, n))
}

// DeleteOrg implements DELETE /orgs/{ns}: only an empty Organisation.
func (h *Identity) DeleteOrg(w http.ResponseWriter, r *http.Request, name gen.Ns) {
	ns, p, ok := h.orgTarget(w, r, name, auth.ActionManageNamespace)
	if !ok {
		return
	}
	if err := h.orgs.DeleteOrg(r.Context(), ns.Name, p); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ListMembers implements GET /orgs/{ns}/members. Membership is public, as
// on the Hub.
func (h *Identity) ListMembers(w http.ResponseWriter, r *http.Request, name gen.Ns) {
	ns, _, ok := h.orgTarget(w, r, name, auth.ActionRead)
	if !ok {
		return
	}
	members, err := h.orgs.Members(r.Context(), ns.Name)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := gen.MemberList{Items: make([]gen.Member, 0, len(members))}
	for _, m := range members {
		out.Items = append(out.Items, idMemberOut(m))
	}
	httpx.WriteJSON(w, r, http.StatusOK, out)
}

// GetMember implements GET /orgs/{ns}/members/{user}.
func (h *Identity) GetMember(w http.ResponseWriter, r *http.Request, name gen.Ns, user string) {
	ns, _, ok := h.orgTarget(w, r, name, auth.ActionRead)
	if !ok {
		return
	}
	m, err := h.orgs.Member(r.Context(), ns.Name, user)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, idMemberOut(*m))
}

// SetMember implements PUT /orgs/{ns}/members/{user}. Organisation admins
// only; the last admin cannot be demoted.
func (h *Identity) SetMember(w http.ResponseWriter, r *http.Request, name gen.Ns, user string) {
	ns, p, ok := h.orgTarget(w, r, name, auth.ActionManageNamespace)
	if !ok {
		return
	}
	var body gen.MemberSet
	if !h.decode(w, r, &body) {
		return
	}
	u, err := h.auth.Users.GetByUsername(r.Context(), user)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if err := h.orgs.SetMember(r.Context(), ns.Name, u.ID, model.Role(body.Role), org.SourceManual, p); err != nil {
		h.fail(w, r, err)
		return
	}
	m, err := h.orgs.Member(r.Context(), ns.Name, u.Username)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, idMemberOut(*m))
}

// RemoveMember implements DELETE /orgs/{ns}/members/{user}. Admins remove
// anyone; Members can leave on their own.
func (h *Identity) RemoveMember(w http.ResponseWriter, r *http.Request, name gen.Ns, user string) {
	p := httpx.Principal(r.Context())
	ns, err := h.orgs.Get(r.Context(), name)
	if err == nil && ns.Kind != org.KindOrg {
		err = org.ErrNotFound
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	u, err := h.auth.Users.GetByUsername(r.Context(), user)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	leaving := p.IsUser() && p.UserID == u.ID && p.MaxScope().Includes(model.ScopeWrite)
	if !leaving {
		if err := h.auth.Authorizer.Can(r.Context(), p, auth.ActionManageNamespace,
			auth.Target{Namespace: &auth.NamespaceTarget{ID: ns.ID, Name: ns.Name, Kind: ns.Kind}}); err != nil {
			h.fail(w, r, err)
			return
		}
	}
	if err := h.orgs.RemoveMember(r.Context(), ns.Name, u.ID, p); err != nil {
		if errors.Is(err, org.ErrNotFound) {
			err = auth.ErrNotFound
		}
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
