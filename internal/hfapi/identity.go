package hfapi

import (
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"

	"github.com/google/uuid"

	"github.com/hangar-hub/hangar/internal/auth"
	"github.com/hangar-hub/hangar/internal/httpx"
)

// whoami serves GET /api/whoami-v2 (contract §6.2) for `hf auth login`
// and `hf auth whoami`.
func (h *Handler) whoami(w http.ResponseWriter, r *http.Request) {
	p := httpx.Principal(r.Context())
	if p.IsAnonymous() {
		httpx.WriteHubError(w, r, &httpx.HubError{Status: http.StatusUnauthorized, Message: "Invalid username or password."})
		return
	}
	orgs := []map[string]any{}
	if p.UserID != uuid.Nil {
		ms, err := h.d.Orgs.MembershipsOf(r.Context(), p.UserID)
		if err != nil {
			h.internalError(w, r, err)
			return
		}
		for _, m := range ms {
			orgs = append(orgs, map[string]any{
				"type": "org", "id": m.Org.ID.String(), "name": m.Org.Name,
				"fullname": orDefault(m.Org.DisplayName, m.Org.Name), "roleInOrg": string(m.Role),
			})
		}
	}
	authInfo := map[string]any{"type": "session"}
	if p.Token != nil {
		authInfo = map[string]any{
			"type":        "access_token",
			"accessToken": map[string]any{"displayName": p.Token.Name, "role": string(p.Token.Scope)},
		}
	}
	httpx.WriteJSON(w, r, http.StatusOK, map[string]any{
		"type":          "user",
		"id":            p.UserID.String(),
		"name":          p.Username,
		"fullname":      orDefault(p.DisplayName, p.Username),
		"email":         p.Email,
		"emailVerified": p.Email != "",
		"canPay":        false,
		"isPro":         false,
		"isAdmin":       p.IsAdmin,
		"orgs":          orgs,
		"auth":          authInfo,
	})
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// serveOAuth routes the device-code login endpoints that interactive
// `hf auth login` uses (contract §6.3), backed by Hangar Tokens.
func (h *Handler) serveOAuth(w http.ResponseWriter, r *http.Request, p reqPath) {
	switch {
	case p.len() == 1 && p.at(0) == "device":
		h.route(w, r, "/oauth/device", true, methods{http.MethodPost: h.deviceStart})
	case p.len() == 1 && p.at(0) == "token":
		h.route(w, r, "/oauth/token", true, methods{http.MethodPost: h.deviceToken})
	default:
		h.notFound(w, r)
	}
}

// oauthError writes an RFC 6749 error response.
func oauthError(w http.ResponseWriter, r *http.Request, status int, code, desc string) {
	httpx.WriteJSON(w, r, status, map[string]string{"error": code, "error_description": desc})
}

func (h *Handler) readForm(r *http.Request) (url.Values, error) {
	ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	b, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if ct != "" && ct != "application/x-www-form-urlencoded" {
		return nil, errors.New("send the request as application/x-www-form-urlencoded")
	}
	return url.ParseQuery(string(b))
}

// deviceStart serves POST /oauth/device (RFC 8628 section 3.1).
func (h *Handler) deviceStart(w http.ResponseWriter, r *http.Request) {
	form, err := h.readForm(r)
	if err != nil {
		oauthError(w, r, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	clientID := form.Get("client_id")
	if clientID == "" {
		oauthError(w, r, http.StatusBadRequest, "invalid_request", "client_id is required.")
		return
	}
	dc, err := h.d.Devices.Start(r.Context(), clientID)
	if err != nil {
		h.internalError(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, map[string]any{
		"device_code":               dc.DeviceCode,
		"user_code":                 dc.UserCode,
		"verification_uri":          dc.VerificationURI,
		"verification_uri_complete": orDefault(dc.VerificationURIComplete, dc.VerificationURI),
		"interval":                  int(dc.Interval.Seconds()),
		"expires_in":                int(dc.ExpiresIn.Seconds()),
	})
}

// deviceToken serves POST /oauth/token for the device-code grant. Hangar
// Tokens do not expire unless their owner sets an expiry, so the response
// has no refresh token and the client never tries to refresh.
func (h *Handler) deviceToken(w http.ResponseWriter, r *http.Request) {
	form, err := h.readForm(r)
	if err != nil {
		oauthError(w, r, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if gt := form.Get("grant_type"); gt != "urn:ietf:params:oauth:grant-type:device_code" {
		oauthError(w, r, http.StatusBadRequest, "unsupported_grant_type", "Hangar supports only the device code grant here. Sign in again with `hf auth login`.")
		return
	}
	secret, err := h.d.Devices.Poll(r.Context(), form.Get("device_code"))
	switch {
	case err == nil:
		w.Header().Set("Cache-Control", "no-store")
		httpx.WriteJSON(w, r, http.StatusOK, map[string]any{"access_token": secret, "token_type": "bearer"})
	case errors.Is(err, auth.ErrAuthorizationPending):
		oauthError(w, r, http.StatusBadRequest, "authorization_pending", "The user has not approved the request yet.")
	case errors.Is(err, auth.ErrSlowDown):
		oauthError(w, r, http.StatusBadRequest, "slow_down", "Poll less often.")
	case errors.Is(err, auth.ErrExpiredToken):
		oauthError(w, r, http.StatusBadRequest, "expired_token", "The device code has expired. Start again.")
	case errors.Is(err, auth.ErrAccessDenied):
		oauthError(w, r, http.StatusBadRequest, "access_denied", "The user denied the request.")
	case errors.Is(err, auth.ErrNotFound), errors.Is(err, auth.ErrInvalidCredentials):
		oauthError(w, r, http.StatusBadRequest, "invalid_grant", "The device code is not valid.")
	default:
		h.internalError(w, r, err)
	}
}
