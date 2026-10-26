package api

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/hangar-hub/hangar/internal/api/gen"
	"github.com/hangar-hub/hangar/internal/auth"
	"github.com/hangar-hub/hangar/internal/httpx"
	"github.com/hangar-hub/hangar/internal/model"
)

// GetMe implements GET /me. Session callers also get their CSRF token.
// Nobody signed in gets 204, not 401: the web UI asks on every page load,
// and a 401 shows as an error in the browser console before anyone has
// signed in (bellatrix finding 6). Bad credentials never get here; the
// authentication middleware answers them with 401.
func (h *Identity) GetMe(w http.ResponseWriter, r *http.Request) {
	if !httpx.Principal(r.Context()).IsUser() {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	p, ok := h.signedIn(w, r)
	if !ok {
		return
	}
	csrf := ""
	if sess, err := h.auth.CurrentSession(r.Context(), p); err == nil {
		csrf = sess.CSRFToken
	}
	h.writeMe(w, r, p, csrf)
}

// UpdateMe implements PATCH /me: the signed-in user changes their display
// name or email, with the admin user editor's checks. Token callers need
// the write scope, as for the other /me changes.
func (h *Identity) UpdateMe(w http.ResponseWriter, r *http.Request) {
	p, ok := h.signedIn(w, r)
	if !ok || !h.needScope(w, r, p, model.ScopeWrite) {
		return
	}
	var body gen.MeUpdate
	if !h.decode(w, r, &body) {
		return
	}
	if _, err := h.auth.UpdateProfile(r.Context(), p, auth.ProfileUpdate{DisplayName: body.DisplayName, Email: body.Email},
		httpx.ClientIP(r), r.UserAgent()); err != nil {
		h.fail(w, r, err)
		return
	}
	csrf := ""
	if sess, err := h.auth.CurrentSession(r.Context(), p); err == nil {
		csrf = sess.CSRFToken
	}
	h.writeMe(w, r, p, csrf)
}

// ChangeMyPassword implements POST /me/password. Only a web UI session may
// change a password; see auth.Service.ChangePassword.
func (h *Identity) ChangeMyPassword(w http.ResponseWriter, r *http.Request) {
	p, ok := h.signedIn(w, r)
	if !ok {
		return
	}
	if p.Method != model.AuthSession {
		h.fail(w, r, auth.ErrPasswordNeedsSession)
		return
	}
	var body gen.PasswordChange
	if !h.decode(w, r, &body) {
		return
	}
	err := h.auth.ChangePassword(r.Context(), p, auth.PasswordChange{Current: body.CurrentPassword, New: body.NewPassword,
		IP: httpx.ClientIP(r), UserAgent: r.UserAgent()})
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, auth.ErrWrongPassword):
		httpx.WriteProblem(w, r, &httpx.Problem{Status: http.StatusForbidden, Detail: "The current password is wrong.",
			Errors: []httpx.ProblemError{{Pointer: "/current_password", Detail: "The current password is wrong."}}})
	case errors.Is(err, auth.ErrWeakPassword):
		// "weak password: use at least 10 characters" becomes "Use at
		// least 10 characters.", next to the new password field.
		_, reason, _ := strings.Cut(err.Error(), auth.ErrWeakPassword.Error()+": ")
		detail := idSentence(errors.New(reason))
		httpx.WriteProblem(w, r, &httpx.Problem{Status: http.StatusUnprocessableEntity, Detail: detail,
			Errors: []httpx.ProblemError{{Pointer: "/new_password", Detail: detail}}})
	default:
		h.fail(w, r, err)
	}
}

// ListMySessions implements GET /me/sessions.
func (h *Identity) ListMySessions(w http.ResponseWriter, r *http.Request) {
	p, ok := h.signedIn(w, r)
	if !ok {
		return
	}
	list, err := h.auth.Sessions.List(r.Context(), p.UserID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := gen.SessionList{Items: make([]gen.Session, 0, len(list))}
	for _, s := range list {
		out.Items = append(out.Items, gen.Session{Id: s.ID, CreatedAt: s.CreatedAt, ExpiresAt: s.ExpiresAt, LastSeenAt: s.LastSeenAt,
			Current: s.ID == p.SessionID, Ip: idStrPtr(s.IP), UserAgent: idStrPtr(s.UserAgent)})
	}
	httpx.WriteJSON(w, r, http.StatusOK, out)
}

// RevokeMySession implements DELETE /me/sessions/{id}. Revoking the
// current session also clears its cookies.
func (h *Identity) RevokeMySession(w http.ResponseWriter, r *http.Request, id gen.ID) {
	p, ok := h.signedIn(w, r)
	if !ok || !h.needScope(w, r, p, model.ScopeWrite) {
		return
	}
	if err := h.auth.Sessions.Revoke(r.Context(), p.UserID, id); err != nil {
		h.fail(w, r, err)
		return
	}
	if id == p.SessionID {
		h.auth.ClearCookies(w)
	}
	w.WriteHeader(http.StatusNoContent)
}

func idTokenOut(t auth.Token) gen.Token {
	out := gen.Token{Id: t.ID, Name: t.Name, Prefix: t.Prefix, Scope: gen.Scope(t.Scope), CreatedAt: t.CreatedAt,
		ExpiresAt: t.ExpiresAt, LastUsedAt: t.LastUsedAt, Username: idStrPtr(t.Username)}
	if !t.Restrictions.IsZero() {
		r := gen.TokenRestrictions{}
		if len(t.Restrictions.Namespaces) > 0 {
			ns := append([]string(nil), t.Restrictions.Namespaces...)
			r.Namespaces = &ns
		}
		if len(t.Restrictions.Repos) > 0 {
			rs := append([]string(nil), t.Restrictions.Repos...)
			r.Repos = &rs
		}
		out.Restrictions = &r
	}
	return out
}

// ListMyTokens implements GET /me/tokens.
func (h *Identity) ListMyTokens(w http.ResponseWriter, r *http.Request) {
	p, ok := h.signedIn(w, r)
	if !ok {
		return
	}
	list, err := h.auth.TokenStore.List(r.Context(), p.UserID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := gen.TokenList{Items: make([]gen.Token, 0, len(list))}
	for _, t := range list {
		out.Items = append(out.Items, idTokenOut(t))
	}
	httpx.WriteJSON(w, r, http.StatusOK, out)
}

// CreateMyToken implements POST /me/tokens. A Token can only create a
// Token no wider than itself: same or lower scope, and only when it is not
// restricted.
func (h *Identity) CreateMyToken(w http.ResponseWriter, r *http.Request) {
	p, ok := h.signedIn(w, r)
	if !ok || !h.needScope(w, r, p, model.ScopeWrite) {
		return
	}
	var body gen.TokenCreate
	if !h.decode(w, r, &body) {
		return
	}
	nt := auth.NewToken{UserID: p.UserID, Name: body.Name, Scope: model.Scope(body.Scope), ExpiresAt: body.ExpiresAt}
	if body.Restrictions != nil {
		if body.Restrictions.Namespaces != nil {
			nt.Restrictions.Namespaces = *body.Restrictions.Namespaces
		}
		if body.Restrictions.Repos != nil {
			nt.Restrictions.Repos = *body.Restrictions.Repos
		}
	}
	if p.Token != nil {
		if !p.Token.Restrictions.IsZero() || !p.Token.Scope.Includes(nt.Scope) {
			httpx.WriteProblem(w, r, &httpx.Problem{Status: http.StatusForbidden,
				Detail: "A Token can only create Tokens with the same or a lower scope, and only when it is not restricted."})
			return
		}
		// An expiry narrows a Token like a restriction does, so a Token
		// cannot outlive the one that created it.
		if exp := p.Token.ExpiresAt; exp != nil && (nt.ExpiresAt == nil || nt.ExpiresAt.After(*exp)) {
			httpx.WriteProblem(w, r, &httpx.Problem{Status: http.StatusForbidden,
				Detail: "A Token that expires can only create Tokens that expire no later than it does. Set expires_at to " +
					exp.UTC().Format(time.RFC3339) + " or earlier."})
			return
		}
	}
	tok, secret, err := h.auth.TokenStore.CreateAs(r.Context(), nt, p)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	t := idTokenOut(*tok)
	httpx.WriteJSON(w, r, http.StatusCreated, gen.TokenCreated{Id: t.Id, Name: t.Name, Prefix: t.Prefix, Scope: t.Scope,
		Restrictions: t.Restrictions, CreatedAt: t.CreatedAt, ExpiresAt: t.ExpiresAt, Username: t.Username, Secret: secret})
}

// RevokeMyToken implements DELETE /me/tokens/{id}.
func (h *Identity) RevokeMyToken(w http.ResponseWriter, r *http.Request, id gen.ID) {
	p, ok := h.signedIn(w, r)
	if !ok || !h.needScope(w, r, p, model.ScopeWrite) {
		return
	}
	// Users revoke their own Tokens here; admins use DELETE /admin/tokens/{id}.
	self := p
	self.IsAdmin = false
	if err := h.auth.TokenStore.Revoke(r.Context(), id, self); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// DecideDeviceLogin implements POST /me/device: the signed-in user
// approves or denies the code that `hf auth login` shows.
func (h *Identity) DecideDeviceLogin(w http.ResponseWriter, r *http.Request) {
	p, ok := h.signedIn(w, r)
	if !ok {
		return
	}
	var body gen.DeviceDecision
	if !h.decode(w, r, &body) {
		return
	}
	var err error
	if body.Approve {
		// A device login gets read access unless the user chooses write
		// (05-auth.md "Device login").
		scope := model.ScopeRead
		if body.Scope != nil && *body.Scope == gen.DeviceDecisionScopeWrite {
			scope = model.ScopeWrite
		} else if body.Scope != nil && !body.Scope.Valid() {
			httpx.WriteProblem(w, r, &httpx.Problem{Status: http.StatusBadRequest, Detail: "scope must be read or write."})
			return
		}
		err = h.auth.Device.Approve(r.Context(), body.UserCode, scope, p)
	} else {
		err = h.auth.Device.Deny(r.Context(), body.UserCode, p)
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// LookupDeviceLogin implements POST /me/device/lookup: it describes a
// pending device login (client, time and IP address), so the user can
// check it is theirs before answering it.
func (h *Identity) LookupDeviceLogin(w http.ResponseWriter, r *http.Request) {
	p, ok := h.signedIn(w, r)
	if !ok {
		return
	}
	var body gen.DeviceLookup
	if !h.decode(w, r, &body) {
		return
	}
	req, err := h.auth.Device.Lookup(r.Context(), body.UserCode, p)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, gen.DeviceRequest{UserCode: req.UserCode, ClientId: req.ClientID, ClientIp: req.ClientIP,
		RequestedAt: req.RequestedAt, ExpiresAt: req.ExpiresAt, TokenLifetimeDays: int(auth.DeviceTokenTTL / (24 * time.Hour))})
}

func idOllamaKeyOut(k auth.OllamaKey) gen.OllamaKey {
	return gen.OllamaKey{Id: k.ID, Name: idStrPtr(k.Name), Fingerprint: k.Fingerprint, CreatedAt: k.CreatedAt, LastUsedAt: k.LastUsedAt}
}

// ListMyOllamaKeys implements GET /me/ollama-keys.
func (h *Identity) ListMyOllamaKeys(w http.ResponseWriter, r *http.Request) {
	p, ok := h.signedIn(w, r)
	if !ok {
		return
	}
	keys, err := h.auth.Ollama.List(r.Context(), p.UserID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := gen.OllamaKeyList{Items: make([]gen.OllamaKey, 0, len(keys))}
	for _, k := range keys {
		out.Items = append(out.Items, idOllamaKeyOut(k))
	}
	httpx.WriteJSON(w, r, http.StatusOK, out)
}

// AddMyOllamaKey implements POST /me/ollama-keys.
func (h *Identity) AddMyOllamaKey(w http.ResponseWriter, r *http.Request) {
	p, ok := h.signedIn(w, r)
	if !ok || !h.needScope(w, r, p, model.ScopeWrite) {
		return
	}
	var body gen.OllamaKeyCreate
	if !h.decode(w, r, &body) {
		return
	}
	k, err := h.auth.Ollama.Add(r.Context(), p.UserID, idStr(body.Name), body.PublicKey, body.Challenge, body.Signature, p)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusCreated, idOllamaKeyOut(*k))
}

// CreateOllamaKeyChallenge implements POST /me/ollama-keys/challenge: a
// challenge the user signs with the Ollama key they are about to add, to
// prove they hold it.
func (h *Identity) CreateOllamaKeyChallenge(w http.ResponseWriter, r *http.Request) {
	p, ok := h.signedIn(w, r)
	if !ok || !h.needScope(w, r, p, model.ScopeWrite) {
		return
	}
	c, err := h.auth.Ollama.Challenge(r.Context(), p.UserID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	httpx.WriteJSON(w, r, http.StatusOK, gen.OllamaKeyChallenge{Challenge: c.Challenge, Namespace: c.Namespace, ExpiresAt: c.ExpiresAt})
}

// DeleteMyOllamaKey implements DELETE /me/ollama-keys/{id}.
func (h *Identity) DeleteMyOllamaKey(w http.ResponseWriter, r *http.Request, id gen.ID) {
	p, ok := h.signedIn(w, r)
	if !ok || !h.needScope(w, r, p, model.ScopeWrite) {
		return
	}
	if err := h.auth.Ollama.Delete(r.Context(), p.UserID, id, p); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func idUpstreamStatusOut(s auth.UpstreamTokenStatus) gen.UpstreamTokenStatus {
	return gen.UpstreamTokenStatus{Upstream: s.Upstream, Saved: s.Saved, UpdatedAt: s.UpdatedAt}
}

// GetMyUpstreamToken implements GET /me/upstream-tokens/{upstream}. The
// token itself is never returned.
func (h *Identity) GetMyUpstreamToken(w http.ResponseWriter, r *http.Request, upstream string) {
	p, ok := h.signedIn(w, r)
	if !ok {
		return
	}
	st, err := h.auth.UpstreamTokens.Status(r.Context(), p.UserID, upstream)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, idUpstreamStatusOut(st))
}

// SetMyUpstreamToken implements PUT /me/upstream-tokens/{upstream}.
func (h *Identity) SetMyUpstreamToken(w http.ResponseWriter, r *http.Request, upstream string) {
	p, ok := h.signedIn(w, r)
	if !ok || !h.needScope(w, r, p, model.ScopeWrite) {
		return
	}
	var body gen.UpstreamTokenSet
	if !h.decode(w, r, &body) {
		return
	}
	st, err := h.auth.UpstreamTokens.Set(r.Context(), p.UserID, upstream, body.Token, p)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, idUpstreamStatusOut(st))
}

// DeleteMyUpstreamToken implements DELETE /me/upstream-tokens/{upstream}.
func (h *Identity) DeleteMyUpstreamToken(w http.ResponseWriter, r *http.Request, upstream string) {
	p, ok := h.signedIn(w, r)
	if !ok || !h.needScope(w, r, p, model.ScopeWrite) {
		return
	}
	if err := h.auth.UpstreamTokens.Delete(r.Context(), p.UserID, upstream, p); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
