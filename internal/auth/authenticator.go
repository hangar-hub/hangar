package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/google/uuid"

	"github.com/jamesagarside/hangar/internal/httpx"
	"github.com/jamesagarside/hangar/internal/model"
)

// ErrCSRF means a session-authenticated, state-changing request did not
// carry the session's CSRF token. It wraps ErrInvalidCredentials;
// WriteAuthError answers it with 403.
var ErrCSRF = fmt.Errorf("%w: missing or wrong %s header", ErrInvalidCredentials, CSRFHeader)

// Authenticate implements Authenticator and httpx.Authenticator:
//
//   - `Authorization: Bearer hgr_...`: a Token.
//   - `Authorization: Bearer <JWT>`: an Ollama registry token.
//   - `Authorization: Basic` with a Token as the password (any username): git.
//   - the `hangar_session` cookie: a UI session. Requests with a method
//     other than GET, HEAD or OPTIONS must also send the session's CSRF
//     token in X-CSRF-Token.
//
// With no credentials it returns an anonymous Principal and no error. A
// bad credential returns an error wrapping ErrInvalidCredentials, and also
// httpx.ErrInvalidCredentials.
func (s *Service) Authenticate(r *http.Request) (model.Principal, error) {
	p, err := s.authenticate(r)
	if err != nil {
		if errors.Is(err, ErrInvalidCredentials) {
			return model.Principal{}, fmt.Errorf("%w (%w)", err, httpx.ErrInvalidCredentials)
		}
		return model.Principal{}, err
	}
	return p, nil
}

func (s *Service) authenticate(r *http.Request) (model.Principal, error) {
	ctx := r.Context()
	if h := r.Header.Get("Authorization"); h != "" {
		scheme, cred, _ := strings.Cut(h, " ")
		cred = strings.TrimSpace(cred)
		switch strings.ToLower(scheme) {
		case "bearer":
			if cred == "" {
				return model.Principal{}, fmt.Errorf("%w: empty bearer token", ErrInvalidCredentials)
			}
			if strings.HasPrefix(cred, TokenPrefix) {
				return s.tokenPrincipal(ctx, cred)
			}
			if looksLikeJWT(cred) {
				return s.registryPrincipal(ctx, cred)
			}
			return model.Principal{}, fmt.Errorf("%w: unrecognised bearer token", ErrInvalidCredentials)
		case "basic":
			_, pw, ok := r.BasicAuth()
			if !ok || pw == "" {
				return model.Principal{}, fmt.Errorf("%w: malformed basic credentials", ErrInvalidCredentials)
			}
			return s.tokenPrincipal(ctx, pw)
		default:
			return model.Principal{}, fmt.Errorf("%w: unsupported Authorization scheme", ErrInvalidCredentials)
		}
	}
	c, _ := r.Cookie(SessionCookie)
	if c == nil || c.Value == "" {
		return model.Anonymous(), nil
	}
	sess, err := s.Sessions.lookup(ctx, c.Value)
	if err != nil {
		return model.Principal{}, err
	}
	u, err := s.Users.Get(ctx, sess.UserID)
	if err != nil {
		return model.Principal{}, fmt.Errorf("%w: unknown user", ErrInvalidCredentials)
	}
	if u.Disabled() {
		return model.Principal{}, fmt.Errorf("%w: %w", ErrInvalidCredentials, ErrDisabled)
	}
	if !safeMethod(r.Method) && !checkCSRF(r, sess) {
		return model.Principal{}, ErrCSRF
	}
	p := u.Principal(model.AuthSession)
	p.SessionID = sess.ID
	return p, nil
}

func safeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

func (s *Service) tokenPrincipal(ctx context.Context, secret string) (model.Principal, error) {
	p, err := s.TokenStore.authenticate(ctx, secret)
	if err != nil {
		s.countAttempt(ctx, model.AuthToken, "failure")
		return model.Principal{}, err
	}
	s.countAttempt(ctx, model.AuthToken, "success")
	return p, nil
}

// registryPrincipal verifies an Ollama registry token. The Principal can
// only read the one Repo the token names.
func (s *Service) registryPrincipal(ctx context.Context, token string) (model.Principal, error) {
	c, err := verifyJWT(s.jwtKey, token, s.publicURL(), s.now())
	if err != nil {
		return model.Principal{}, fmt.Errorf("%w: %w", ErrInvalidCredentials, err)
	}
	uid, err := uuid.Parse(c.Subject)
	if err != nil {
		return model.Principal{}, fmt.Errorf("%w: bad subject", ErrInvalidCredentials)
	}
	u, err := s.Users.Get(ctx, uid)
	if err != nil {
		return model.Principal{}, fmt.Errorf("%w: unknown user", ErrInvalidCredentials)
	}
	if u.Disabled() {
		return model.Principal{}, fmt.Errorf("%w: %w", ErrInvalidCredentials, ErrDisabled)
	}
	p := u.Principal(model.AuthOllamaKey)
	p.Token = &model.TokenInfo{Name: "ollama", Scope: model.ScopeRead, Restrictions: model.TokenRestrictions{Repos: []string{c.Repo}}}
	return p, nil
}

// WriteAuthError writes the response for an Authenticate error: 403 for a
// missing CSRF token, and the protocol's 401 otherwise. Pass it to
// httpx.Authenticate as onError. It classifies the request first, because
// authentication runs before the protocol dispatcher.
func WriteAuthError(publicURL string) func(w http.ResponseWriter, r *http.Request, err error) {
	viewerHost := ""
	if u, perr := url.Parse(publicURL); perr == nil && u.Host != "" {
		viewerHost = "datasets-server." + u.Host
	}
	return func(w http.ResponseWriter, r *http.Request, err error) {
		httpx.SetProtocol(r.Context(), httpx.Classify(r, viewerHost).Protocol())
		if errors.Is(err, ErrCSRF) {
			httpx.WriteError(w, r, http.StatusForbidden, "", "The request needs the X-CSRF-Token header from your session. Reload the page and try again.")
			return
		}
		if !errors.Is(err, ErrInvalidCredentials) {
			httpx.WriteError(w, r, http.StatusInternalServerError, "", "Authentication is unavailable. The request ID is "+httpx.RequestID(r.Context())+".")
			return
		}
		httpx.WriteUnauthorized(w, r)
	}
}
