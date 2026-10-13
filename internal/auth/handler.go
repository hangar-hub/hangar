package auth

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/jamesagarside/hangar/internal/httpx"
	"github.com/jamesagarside/hangar/internal/model"
)

// Handler serves the browser sign-in endpoints under /auth/ that are not
// part of the native API (httpx.Protocols.Auth):
//
//	GET  /auth/oidc/{provider}?redirect=/path   start OIDC sign-in (302 to the provider)
//	GET  /auth/oidc/{provider}/callback        finish it (302 to the redirect, or to
//	                                           /auth/login?error=<code>)
//	POST /auth/oidc/{provider}/link            signed-in users link a provider;
//	                                           answers {"authorize_url": "..."}
//	DELETE /auth/oidc/{provider}/link          unlink a provider
//	POST /auth/register                        {"username","password","email","display_name"}
//	POST /auth/invite/{code}                   the same body; accepts an invite
//	POST /auth/reset/{code}                    {"password"}; sets a new password
//
// The register, invite and reset calls sign the user in and answer with
// the Me schema. GET /auth/login, /auth/invite/{code} and /auth/reset/{code}
// are UI pages and never reach this handler. State-changing calls made
// with a session need the X-CSRF-Token header, like the native API.
func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /auth/oidc/{provider}", s.route("/auth/oidc/{provider}", s.oidcStart))
	mux.HandleFunc("GET /auth/oidc/{provider}/callback", s.route("/auth/oidc/{provider}/callback", s.oidcCallback))
	mux.HandleFunc("POST /auth/oidc/{provider}/link", s.route("/auth/oidc/{provider}/link", s.oidcLink))
	mux.HandleFunc("DELETE /auth/oidc/{provider}/link", s.route("/auth/oidc/{provider}/link", s.oidcUnlink))
	mux.HandleFunc("POST /auth/register", s.route("/auth/register", s.register))
	mux.HandleFunc("POST /auth/invite/{code}", s.route("/auth/invite/{code}", s.acceptInvite))
	mux.HandleFunc("POST /auth/reset/{code}", s.route("/auth/reset/{code}", s.resetPassword))
	return mux
}

// route records the route template for logs and spans; codes and states
// in paths and queries never reach the access log.
func (s *Service) route(tmpl string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		httpx.SetRoute(r.Context(), tmpl)
		w.Header().Set("Cache-Control", "no-store")
		h(w, r)
	}
}

func (s *Service) loginError(w http.ResponseWriter, r *http.Request, code string) {
	http.Redirect(w, r, "/auth/login?error="+url.QueryEscape(code), http.StatusFound)
}

func (s *Service) oidcStart(w http.ResponseWriter, r *http.Request) {
	authURL, err := s.OIDC.Start(r.Context(), r.PathValue("provider"), r.URL.Query().Get("redirect"), uuid.Nil)
	if err != nil {
		s.log.WarnContext(r.Context(), "start OIDC sign-in", "provider", r.PathValue("provider"), "error", err)
		s.loginError(w, r, OIDCErrorCode(err))
		return
	}
	http.Redirect(w, r, authURL, http.StatusFound) //nolint:gosec // The URL is the configured provider's authorisation endpoint; the user-supplied redirect is stored in oidc_states after SafeRedirect.
}

func (s *Service) oidcCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if e := q.Get("error"); e != "" {
		// The provider refused, for example when the user cancelled.
		s.loginError(w, r, "oidc_denied")
		return
	}
	res, err := s.OIDC.Callback(r.Context(), r.PathValue("provider"), q.Get("state"), q.Get("code"), httpx.ClientIP(r), r.UserAgent())
	if err != nil {
		s.log.WarnContext(r.Context(), "OIDC sign-in failed", "provider", r.PathValue("provider"), "reason", OIDCErrorCode(err), "error", err)
		s.loginError(w, r, OIDCErrorCode(err))
		return
	}
	if res.Linked {
		http.Redirect(w, r, "/settings?linked="+url.QueryEscape(r.PathValue("provider")), http.StatusFound)
		return
	}
	if _, err := s.StartSession(r.Context(), w, r, res.User); err != nil {
		s.log.ErrorContext(r.Context(), "start session", "error", err)
		s.loginError(w, r, "oidc_failed")
		return
	}
	http.Redirect(w, r, res.RedirectTo, http.StatusFound)
}

// sessionUser returns the signed-in session user, writing 401 or 403.
func (s *Service) sessionUser(w http.ResponseWriter, r *http.Request) (model.Principal, bool) {
	p := httpx.Principal(r.Context())
	if !p.IsUser() {
		httpx.WriteProblem(w, r, &httpx.Problem{Status: http.StatusUnauthorized, Detail: "Sign in to do this."})
		return p, false
	}
	if p.Method != model.AuthSession {
		httpx.WriteProblem(w, r, &httpx.Problem{Status: http.StatusForbidden, Detail: "Link sign-in providers in the web UI."})
		return p, false
	}
	return p, true
}

func (s *Service) oidcLink(w http.ResponseWriter, r *http.Request) {
	p, ok := s.sessionUser(w, r)
	if !ok {
		return
	}
	authURL, err := s.OIDC.Start(r.Context(), r.PathValue("provider"), "/settings", p.UserID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, map[string]string{"authorize_url": authURL})
}

func (s *Service) oidcUnlink(w http.ResponseWriter, r *http.Request) {
	p, ok := s.sessionUser(w, r)
	if !ok {
		return
	}
	if err := s.OIDC.Unlink(r.Context(), p.UserID, r.PathValue("provider"), p); err != nil {
		s.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// registrationBody is the body of /auth/register and /auth/invite/{code}.
type registrationBody struct {
	Username    string `json:"username"`
	Password    string `json:"password"`
	Email       string `json:"email"`
	DisplayName string `json:"display_name"`
}

func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		httpx.WriteProblem(w, r, &httpx.Problem{Status: http.StatusBadRequest, Detail: "The request body is not valid JSON."})
		return false
	}
	return true
}

func (s *Service) register(w http.ResponseWriter, r *http.Request) {
	s.createAccount(w, r, "")
}

func (s *Service) acceptInvite(w http.ResponseWriter, r *http.Request) {
	s.createAccount(w, r, r.PathValue("code"))
}

func (s *Service) createAccount(w http.ResponseWriter, r *http.Request, invite string) {
	var body registrationBody
	if !decodeBody(w, r, &body) {
		return
	}
	u, err := s.Register(r.Context(), Registration{Username: body.Username, Password: body.Password, Email: body.Email,
		DisplayName: body.DisplayName, InviteCode: invite, IP: httpx.ClientIP(r), UserAgent: r.UserAgent()})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.signInAndRespond(w, r, u, http.StatusCreated)
}

func (s *Service) resetPassword(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Password string `json:"password"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	u, err := s.ResetPassword(r.Context(), r.PathValue("code"), body.Password, httpx.ClientIP(r), r.UserAgent())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if u.Disabled() {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	s.signInAndRespond(w, r, u, http.StatusOK)
}

func (s *Service) signInAndRespond(w http.ResponseWriter, r *http.Request, u *User, status int) {
	sess, err := s.StartSession(r.Context(), w, r, u)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	me, err := s.Me(r.Context(), u.Principal(model.AuthSession), sess.CSRFToken)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, status, me)
}

// writeError maps errors from this package to problem details.
func (s *Service) writeError(w http.ResponseWriter, r *http.Request, err error) {
	status, detail := http.StatusInternalServerError, ""
	var te *ThrottledError
	switch {
	case errors.As(err, &te):
		w.Header().Set("Retry-After", strconv.Itoa(int(te.RetryAfter.Round(time.Second).Seconds())))
		status, detail = http.StatusTooManyRequests, te.Error()
	case errors.Is(err, ErrRegistrationClosed), errors.Is(err, ErrForbidden), errors.Is(err, ErrDisabled):
		status = http.StatusForbidden
	case errors.Is(err, ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, ErrInvalid):
		status = http.StatusUnprocessableEntity
	case errors.Is(err, ErrConflict):
		status = http.StatusConflict
	case errors.Is(err, ErrInvalidCredentials):
		status = http.StatusUnauthorized
	}
	if status == http.StatusInternalServerError {
		s.log.ErrorContext(r.Context(), "sign-in request failed", "error", err)
		detail = "Internal server error. The request ID is " + httpx.RequestID(r.Context()) + "."
	} else if detail == "" {
		detail = userText(err)
	}
	httpx.WriteProblem(w, r, &httpx.Problem{Status: status, Detail: detail})
}

// userText turns an error into a sentence for the UI.
func userText(err error) string {
	s := err.Error()
	if s == "" {
		return s
	}
	s = strings.ToUpper(s[:1]) + s[1:]
	if !strings.HasSuffix(s, ".") {
		s += "."
	}
	return s
}
