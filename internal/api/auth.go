// Package api implements the native API (/api/hangar/v1), the generated
// gen.ServerInterface from api/openapi.yaml. Each workstream implements its
// part on its own handler type; the integration wiring combines them.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/hangar-hub/hangar/internal/api/gen"
	"github.com/hangar-hub/hangar/internal/audit"
	"github.com/hangar-hub/hangar/internal/auth"
	"github.com/hangar-hub/hangar/internal/httpx"
	"github.com/hangar-hub/hangar/internal/model"
	"github.com/hangar-hub/hangar/internal/org"
	"github.com/hangar-hub/hangar/internal/repo"
)

// IdentityRepos finds a Repo by ID, following renames. repo.Service has it.
type IdentityRepos interface {
	Get(ctx context.Context, id model.RepoID) (*repo.Repo, error)
}

// IdentityOptions configures NewIdentity.
type IdentityOptions struct {
	Auth  *auth.Service
	Orgs  *org.DBService
	Audit audit.Log
	// Repos finds Repos for access requests and collaborators.
	Repos IdentityRepos
	Log   *slog.Logger
}

// Identity implements the identity and access part of the native API
// (workstream W5): sessions, Tokens, keys, Organisations, Members,
// collaborators, access requests, users, Instance settings and the audit
// log. It implements IdentityAPI.
type Identity struct {
	auth  *auth.Service
	orgs  *org.DBService
	audit audit.Log
	repos IdentityRepos
	log   *slog.Logger
}

// NewIdentity returns the identity handlers.
func NewIdentity(o IdentityOptions) *Identity {
	log := o.Log
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Identity{auth: o.Auth, orgs: o.Orgs, audit: o.Audit, repos: o.Repos, log: log}
}

// IdentityAPI lists the gen.ServerInterface operations Identity serves.
type IdentityAPI interface {
	GetMe(w http.ResponseWriter, r *http.Request)
	UpdateMe(w http.ResponseWriter, r *http.Request)
	ChangeMyPassword(w http.ResponseWriter, r *http.Request)
	Login(w http.ResponseWriter, r *http.Request)
	Logout(w http.ResponseWriter, r *http.Request)
	ListAuthProviders(w http.ResponseWriter, r *http.Request)
	ListMySessions(w http.ResponseWriter, r *http.Request)
	RevokeMySession(w http.ResponseWriter, r *http.Request, id gen.ID)
	ListMyTokens(w http.ResponseWriter, r *http.Request)
	CreateMyToken(w http.ResponseWriter, r *http.Request)
	RevokeMyToken(w http.ResponseWriter, r *http.Request, id gen.ID)
	DecideDeviceLogin(w http.ResponseWriter, r *http.Request)
	ListMyOllamaKeys(w http.ResponseWriter, r *http.Request)
	AddMyOllamaKey(w http.ResponseWriter, r *http.Request)
	DeleteMyOllamaKey(w http.ResponseWriter, r *http.Request, id gen.ID)
	GetMyUpstreamToken(w http.ResponseWriter, r *http.Request, upstream string)
	SetMyUpstreamToken(w http.ResponseWriter, r *http.Request, upstream string)
	DeleteMyUpstreamToken(w http.ResponseWriter, r *http.Request, upstream string)
	CreateOrg(w http.ResponseWriter, r *http.Request)
	UpdateOrg(w http.ResponseWriter, r *http.Request, ns gen.Ns)
	DeleteOrg(w http.ResponseWriter, r *http.Request, ns gen.Ns)
	ListMembers(w http.ResponseWriter, r *http.Request, ns gen.Ns)
	GetMember(w http.ResponseWriter, r *http.Request, ns gen.Ns, user string)
	SetMember(w http.ResponseWriter, r *http.Request, ns gen.Ns, user string)
	RemoveMember(w http.ResponseWriter, r *http.Request, ns gen.Ns, user string)
	ListCollaborators(w http.ResponseWriter, r *http.Request, pType gen.RepoType, ns gen.Ns, name gen.Name)
	SetCollaborators(w http.ResponseWriter, r *http.Request, pType gen.RepoType, ns gen.Ns, name gen.Name)
	ListAccessRequests(w http.ResponseWriter, r *http.Request, pType gen.RepoType, ns gen.Ns, name gen.Name, params gen.ListAccessRequestsParams)
	RequestAccess(w http.ResponseWriter, r *http.Request, pType gen.RepoType, ns gen.Ns, name gen.Name)
	DecideAccessRequest(w http.ResponseWriter, r *http.Request, pType gen.RepoType, ns gen.Ns, name gen.Name, id gen.ID)
	AdminListUsers(w http.ResponseWriter, r *http.Request, params gen.AdminListUsersParams)
	AdminCreateUser(w http.ResponseWriter, r *http.Request)
	AdminUpdateUser(w http.ResponseWriter, r *http.Request, id gen.ID)
	AdminCreateResetLink(w http.ResponseWriter, r *http.Request, id gen.ID)
	AdminCreateEmailLink(w http.ResponseWriter, r *http.Request, id gen.ID)
	AdminCreateInvite(w http.ResponseWriter, r *http.Request)
	AdminGetSettings(w http.ResponseWriter, r *http.Request)
	AdminUpdateSettings(w http.ResponseWriter, r *http.Request)
	AdminListTokens(w http.ResponseWriter, r *http.Request, params gen.AdminListTokensParams)
	AdminRevokeToken(w http.ResponseWriter, r *http.Request, id gen.ID)
	AdminListAudit(w http.ResponseWriter, r *http.Request, params gen.AdminListAuditParams)
}

var _ IdentityAPI = (*Identity)(nil)

// Login implements POST /auth/login: password sign-in that sets the
// session cookie.
func (h *Identity) Login(w http.ResponseWriter, r *http.Request) {
	var body gen.LoginRequest
	if !h.decode(w, r, &body) {
		return
	}
	u, err := h.auth.Login.Password(r.Context(), auth.LoginAttempt{
		Username: body.Username, Password: body.Password, IP: httpx.ClientIP(r), UserAgent: r.UserAgent()})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	sess, err := h.auth.StartSession(r.Context(), w, r, u)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.writeMe(w, r, u.Principal(model.AuthSession), sess.CSRFToken)
}

// Logout implements POST /auth/logout.
func (h *Identity) Logout(w http.ResponseWriter, r *http.Request) {
	p := httpx.Principal(r.Context())
	h.auth.EndSession(r.Context(), w, r)
	if p.IsUser() {
		h.auth.RecordLogout(r.Context(), p, httpx.ClientIP(r), r.UserAgent())
	}
	w.WriteHeader(http.StatusNoContent)
}

// ListAuthProviders implements GET /auth/providers.
func (h *Identity) ListAuthProviders(w http.ResponseWriter, r *http.Request) {
	st, err := h.auth.Settings.Get(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := gen.AuthProviders{LocalLogin: st.LocalLogin, Registration: gen.AuthProvidersRegistration(st.Registration),
		UiExternalImages: st.UIExternalImages}
	out.Providers = []struct {
		Id       string `json:"id"`
		LoginUrl string `json:"login_url"`
		Name     string `json:"name"`
	}{}
	for _, p := range h.auth.OIDC.Providers() {
		out.Providers = append(out.Providers, struct {
			Id       string `json:"id"`
			LoginUrl string `json:"login_url"`
			Name     string `json:"name"`
		}{Id: p.ID, LoginUrl: p.LoginURL, Name: p.Name})
	}
	httpx.WriteJSON(w, r, http.StatusOK, out)
}

// writeMe writes the Me schema for p.
func (h *Identity) writeMe(w http.ResponseWriter, r *http.Request, p model.Principal, csrf string) {
	me, err := h.auth.Me(r.Context(), p, csrf)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, me)
}

// decode reads a JSON body into v, writing a 400 problem on failure.
func (h *Identity) decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(v); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			httpx.WriteProblem(w, r, &httpx.Problem{Status: http.StatusRequestEntityTooLarge, Detail: "The request body is too large."})
			return false
		}
		httpx.WriteProblem(w, r, &httpx.Problem{Status: http.StatusBadRequest, Detail: "The request body is not valid JSON for this operation: " + err.Error()})
		return false
	}
	return true
}

// signedIn returns the caller, writing 401 when the request is anonymous.
func (h *Identity) signedIn(w http.ResponseWriter, r *http.Request) (model.Principal, bool) {
	p := httpx.Principal(r.Context())
	if !p.IsUser() {
		httpx.WriteProblem(w, r, &httpx.Problem{Status: http.StatusUnauthorized, Detail: "Sign in to do this."})
		return p, false
	}
	return p, true
}

// needScope checks a Token's scope for /me changes, writing 403.
func (h *Identity) needScope(w http.ResponseWriter, r *http.Request, p model.Principal, s model.Scope) bool {
	if !p.MaxScope().Includes(s) {
		httpx.WriteProblem(w, r, &httpx.Problem{Status: http.StatusForbidden,
			Detail: fmt.Sprintf("The Token's %s scope does not allow this; it needs %s.", p.MaxScope(), s)})
		return false
	}
	return true
}

// instanceAdmin checks the caller is an Instance admin, writing 401 or 403.
func (h *Identity) instanceAdmin(w http.ResponseWriter, r *http.Request) (model.Principal, bool) {
	p, ok := h.signedIn(w, r)
	if !ok {
		return p, false
	}
	if err := h.auth.Authorizer.Can(r.Context(), p, auth.ActionInstanceAdmin, auth.Target{}); err != nil {
		h.fail(w, r, err)
		return p, false
	}
	return p, true
}

// fail maps service errors to problem details.
func (h *Identity) fail(w http.ResponseWriter, r *http.Request, err error) {
	if clientGone(r, err) {
		writeClientGone(w, r, h.log, err)
		return
	}
	p := httpx.Principal(r.Context())
	status := http.StatusInternalServerError
	detail := ""
	var te *auth.ThrottledError
	switch {
	case errors.As(err, &te):
		w.Header().Set("Retry-After", strconv.Itoa(int(te.RetryAfter.Round(time.Second).Seconds())))
		status, detail = http.StatusTooManyRequests, te.Message()
	case errors.Is(err, auth.ErrLoginFailed):
		status, detail = http.StatusUnauthorized, "Wrong username or password."
	case errors.Is(err, auth.ErrInvalidCredentials):
		status, detail = http.StatusUnauthorized, "Sign in to do this."
	case errors.Is(err, auth.ErrLocalLoginDisabled), errors.Is(err, auth.ErrRegistrationClosed), errors.Is(err, auth.ErrDisabled):
		status, detail = http.StatusForbidden, idSentence(err)
	case errors.Is(err, auth.ErrGated):
		status, detail = http.StatusForbidden, "This Repo is gated. Request access on the Repo page."
	case errors.Is(err, auth.ErrForbidden):
		if p.IsAnonymous() {
			status, detail = http.StatusUnauthorized, "Sign in to do this."
		} else {
			status, detail = http.StatusForbidden, idSentence(err)
		}
	case errors.Is(err, auth.ErrNotFound), errors.Is(err, org.ErrNotFound), errors.Is(err, repo.ErrNotFound):
		status, detail = http.StatusNotFound, idSentence(err)
	case errors.Is(err, auth.ErrInvalid), errors.Is(err, org.ErrInvalid), errors.Is(err, auth.ErrInvalidSettings):
		status, detail = http.StatusUnprocessableEntity, idSentence(err)
	case errors.Is(err, auth.ErrConflict), errors.Is(err, org.ErrExists), errors.Is(err, org.ErrLastAdmin),
		errors.Is(err, org.ErrNotEmpty), errors.Is(err, auth.ErrLastInstanceAdmin):
		status, detail = http.StatusConflict, idSentence(err)
	case errors.Is(err, audit.ErrBadCursor):
		status, detail = http.StatusBadRequest, "The cursor is not valid."
	}
	if status == http.StatusInternalServerError {
		h.log.ErrorContext(r.Context(), "native API request failed", "route", idRouteName(r), "error", err)
		detail = "Internal server error. The request ID is " + httpx.RequestID(r.Context()) + "."
	}
	httpx.WriteProblem(w, r, &httpx.Problem{Status: status, Detail: detail})
}

func idRouteName(r *http.Request) string {
	if i := httpx.Info(r.Context()); i != nil {
		return i.Route()
	}
	return ""
}

// idSentence turns an error into user-facing text: first letter upper case,
// ending with a full stop.
func idSentence(err error) string {
	s := err.Error()
	if s == "" {
		return s
	}
	if s[0] >= 'a' && s[0] <= 'z' {
		s = string(s[0]-'a'+'A') + s[1:]
	}
	if last := s[len(s)-1]; last != '.' && last != '?' && last != '!' {
		s += "."
	}
	return s
}

// idLimit returns a page size from the Limit parameter.
func idLimit(l *gen.Limit) int {
	if l == nil || *l <= 0 {
		return 50
	}
	return min(*l, 500)
}

func idStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func idStrPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func idNextCursor(s string) *string { return idStrPtr(s) }
