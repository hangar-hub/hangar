package api

import (
	"net/http"

	"github.com/jamesagarside/hangar/internal/api/gen"
	"github.com/jamesagarside/hangar/internal/auth"
	"github.com/jamesagarside/hangar/internal/httpx"
	"github.com/jamesagarside/hangar/internal/model"
)

func idAdminUserOut(u *auth.User) gen.AdminUser {
	out := gen.AdminUser{Id: u.ID, Username: u.Username, DisplayName: idStrPtr(u.DisplayName), Email: idStrPtr(u.Email),
		IsAdmin: u.IsAdmin, Disabled: u.Disabled(), LastLoginAt: u.LastLoginAt, CreatedAt: u.CreatedAt,
		PendingEmail: idStrPtr(u.PendingEmail)}
	if len(u.OIDCProviders) > 0 {
		ps := append([]string(nil), u.OIDCProviders...)
		out.OidcProviders = &ps
	}
	return out
}

// AdminListUsers implements GET /admin/users.
func (h *Identity) AdminListUsers(w http.ResponseWriter, r *http.Request, params gen.AdminListUsersParams) {
	if _, ok := h.instanceAdmin(w, r); !ok {
		return
	}
	cursor := ""
	if params.Cursor != nil {
		cursor = *params.Cursor
	}
	users, next, err := h.auth.Users.List(r.Context(), idStr(params.Q), cursor, idLimit(params.Limit))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := gen.AdminUserList{Items: make([]gen.AdminUser, 0, len(users)), NextCursor: idNextCursor(next)}
	for _, u := range users {
		out.Items = append(out.Items, idAdminUserOut(u))
	}
	httpx.WriteJSON(w, r, http.StatusOK, out)
}

// AdminListOrgs implements GET /admin/orgs: every Organisation, for the
// admin Organisations page.
func (h *Identity) AdminListOrgs(w http.ResponseWriter, r *http.Request, params gen.AdminListOrgsParams) {
	if _, ok := h.instanceAdmin(w, r); !ok {
		return
	}
	cursor := ""
	if params.Cursor != nil {
		cursor = *params.Cursor
	}
	orgs, next, err := h.orgs.ListOrgs(r.Context(), idStr(params.Q), cursor, idLimit(params.Limit))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := gen.AdminOrgList{Items: make([]gen.Namespace, 0, len(orgs)), NextCursor: idNextCursor(next)}
	for i := range orgs {
		n, err := h.orgs.MemberCount(r.Context(), orgs[i].ID)
		if err != nil {
			h.fail(w, r, err)
			return
		}
		out.Items = append(out.Items, idNamespaceOut(&orgs[i], n))
	}
	httpx.WriteJSON(w, r, http.StatusOK, out)
}

// AdminCreateUser implements POST /admin/users. Without a password the
// user signs in with OIDC, or the admin sends a reset link.
func (h *Identity) AdminCreateUser(w http.ResponseWriter, r *http.Request) {
	p, ok := h.instanceAdmin(w, r)
	if !ok {
		return
	}
	var body gen.AdminUserCreate
	if !h.decode(w, r, &body) {
		return
	}
	// An admin vouches for the email they give, so it is verified.
	nu := auth.NewUser{Username: body.Username, Email: idStr(body.Email), EmailVerified: idStr(body.Email) != "",
		DisplayName: idStr(body.DisplayName), Password: idStr(body.Password)}
	if body.IsAdmin != nil {
		nu.IsAdmin = *body.IsAdmin
	}
	u, err := h.auth.Users.Create(r.Context(), nu, p)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusCreated, idAdminUserOut(u))
}

// AdminUpdateUser implements PATCH /admin/users/{id}.
func (h *Identity) AdminUpdateUser(w http.ResponseWriter, r *http.Request, id gen.ID) {
	p, ok := h.instanceAdmin(w, r)
	if !ok {
		return
	}
	var body gen.AdminUserUpdate
	if !h.decode(w, r, &body) {
		return
	}
	u, err := h.auth.Users.Update(r.Context(), id, auth.UserUpdate{Disabled: body.Disabled, IsAdmin: body.IsAdmin,
		Email: body.Email, DisplayName: body.DisplayName}, p)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, idAdminUserOut(u))
}

// AdminCreateResetLink implements POST /admin/users/{id}/reset-link.
func (h *Identity) AdminCreateResetLink(w http.ResponseWriter, r *http.Request, id gen.ID) {
	p, ok := h.instanceAdmin(w, r)
	if !ok {
		return
	}
	link, err := h.auth.CreateResetLink(r.Context(), id, p)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusCreated, gen.Link{Url: link.URL, ExpiresAt: link.ExpiresAt})
}

// AdminCreateEmailLink implements POST /admin/users/{id}/email-link: the
// link that confirms a user's pending email, for the admin to send to it.
func (h *Identity) AdminCreateEmailLink(w http.ResponseWriter, r *http.Request, id gen.ID) {
	p, ok := h.instanceAdmin(w, r)
	if !ok {
		return
	}
	var body gen.EmailLinkCreate
	if !h.decode(w, r, &body) {
		return
	}
	link, err := h.auth.CreateEmailLink(r.Context(), id, body.Email, p)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusCreated, gen.EmailLink{Url: link.URL, ExpiresAt: link.ExpiresAt, Email: link.Email})
}

// AdminCreateInvite implements POST /admin/invites.
func (h *Identity) AdminCreateInvite(w http.ResponseWriter, r *http.Request) {
	p, ok := h.instanceAdmin(w, r)
	if !ok {
		return
	}
	var body gen.InviteCreate
	if !h.decode(w, r, &body) {
		return
	}
	params := auth.InviteParams{Email: idStr(body.Email), Org: idStr(body.Org)}
	if body.Role != nil {
		params.Role = model.Role(*body.Role)
	}
	if body.IsAdmin != nil {
		params.MakeAdmin = *body.IsAdmin
	}
	link, err := h.auth.CreateInvite(r.Context(), params, p)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusCreated, gen.Link{Url: link.URL, ExpiresAt: link.ExpiresAt})
}
