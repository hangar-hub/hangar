package api

import (
	"net/http"

	"github.com/jamesagarside/hangar/internal/api/gen"
	"github.com/jamesagarside/hangar/internal/auth"
	"github.com/jamesagarside/hangar/internal/httpx"
	"github.com/jamesagarside/hangar/internal/model"
)

// GetMe implements GET /me. Session callers also get their CSRF token.
func (h *Identity) GetMe(w http.ResponseWriter, r *http.Request) {
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
				Detail: "A token can only create tokens with the same or a lower scope, and only when it is not restricted."})
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
		err = h.auth.Device.Approve(r.Context(), body.UserCode, p)
	} else {
		err = h.auth.Device.Deny(r.Context(), body.UserCode, p)
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
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
	k, err := h.auth.Ollama.Add(r.Context(), p.UserID, idStr(body.Name), body.PublicKey, p)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusCreated, idOllamaKeyOut(*k))
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
