package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"

	"github.com/google/uuid"

	"github.com/jamesagarside/hangar/internal/httpx"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/telemetry"
)

// ErrCSRF means a session-authenticated, state-changing request did not
// carry the session's CSRF token. It wraps ErrInvalidCredentials;
// WriteAuthError answers it with 403.
var ErrCSRF = fmt.Errorf("%w: missing or wrong %s header", ErrInvalidCredentials, CSRFHeader)

// Authenticate implements Authenticator and httpx.Authenticator:
//
//   - `Authorization: Bearer hgr_...`: a Token.
//   - `Authorization: Bearer <JWT>`: an Ollama registry token.
//   - `Authorization: Basic` with a Token as the password (any username),
//     on git and git-lfs routes only. Other routes answer 401, except the
//     static UI and health routes, which ignore it.
//   - the `hangar_session` cookie: a UI session. Requests with a method
//     other than GET, HEAD or OPTIONS must also send the session's CSRF
//     token in X-CSRF-Token, except the calls that start a new session.
//
// A request with a method other than GET, HEAD or OPTIONS that carries
// Basic credentials or a valid session cookie is refused with
// ErrCrossOrigin when the browser marks it as cross-origin (see
// originGuard). Browsers add both credentials on their own, so this stops
// other sites from using them.
//
// With no credentials, or only a stale session cookie, it returns an
// anonymous Principal and no error. A bad Authorization header returns an
// error wrapping ErrInvalidCredentials, and also httpx.ErrInvalidCredentials.
//
// Any other error means the credentials could not be checked. When the
// request context has ended (the client closed the request) it is logged at
// DEBUG; otherwise it is a failure of the authentication store and is
// logged at ERROR with the request ID, because WriteAuthError answers it
// with a 500 that names only that ID.
func (s *Service) Authenticate(r *http.Request) (model.Principal, error) {
	p, err := s.authenticate(r)
	if err == nil {
		return p, nil
	}
	if errors.Is(err, ErrInvalidCredentials) {
		return model.Principal{}, fmt.Errorf("%w (%w)", err, httpx.ErrInvalidCredentials)
	}
	ctx := r.Context()
	if ctx.Err() != nil {
		s.log.DebugContext(ctx, "client closed the request during authentication",
			"error", err.Error(), "request_id", httpx.RequestID(ctx))
	} else {
		s.log.ErrorContext(ctx, "authentication store unavailable",
			"error", err.Error(), "method", r.Method, "request_id", httpx.RequestID(ctx))
	}
	return model.Principal{}, err
}

func (s *Service) authenticate(r *http.Request) (model.Principal, error) {
	ctx := r.Context()
	h := r.Header.Get("Authorization")
	if scheme, _, _ := strings.Cut(h, " "); strings.EqualFold(scheme, "basic") {
		// A browser replays cached Basic credentials on its own, so they
		// are accepted only where git and git-lfs need them, and never on
		// a cross-origin state-changing request.
		allowed, ignore := basicAllowed(httpx.Classify(r, s.viewerHost))
		switch {
		case ignore:
			h = ""
		case !allowed:
			return model.Principal{}, ErrBasicNotAccepted
		default:
			if err := s.origins.check(r); err != nil {
				return model.Principal{}, err
			}
		}
	}
	if h != "" {
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
	if isSignInCall(r) {
		// Signing in from another site would sign the browser in to an
		// account the other site chose.
		if err := s.origins.check(r); err != nil {
			return model.Principal{}, err
		}
	}
	c, _ := r.Cookie(SessionCookie)
	if c == nil || c.Value == "" {
		return model.Anonymous(), nil
	}
	// The browser sends the session cookie on every request, so a stale one
	// (unknown, expired, revoked, or for a missing or disabled User) makes
	// the request anonymous instead of failing it. Otherwise the browser
	// could not even load the sign-in page until the user cleared cookies.
	sess, err := s.Sessions.lookup(ctx, c.Value)
	if errors.Is(err, ErrInvalidCredentials) {
		return staleSession(ctx), nil
	}
	if err != nil {
		return model.Principal{}, err
	}
	u, err := s.Users.Get(ctx, sess.UserID)
	if errors.Is(err, ErrNotFound) {
		return staleSession(ctx), nil
	}
	if err != nil {
		return model.Principal{}, err
	}
	if u.Disabled() {
		return staleSession(ctx), nil
	}
	// Defence in depth on top of SameSite=Lax and the CSRF header.
	if err := s.origins.check(r); err != nil {
		return model.Principal{}, err
	}
	if !safeMethod(r.Method) && !checkCSRF(r, sess) {
		if isSignInCall(r) {
			// Signing in again replaces the session, so it needs nothing
			// from the current one.
			return model.Anonymous(), nil
		}
		return model.Principal{}, ErrCSRF
	}
	p := u.Principal(model.AuthSession)
	p.SessionID = sess.ID
	return p, nil
}

// isSignInCall reports whether r is one of the calls that start a new
// session: password sign-in, registration, accepting an invite and
// resetting a password. They run as anonymous when the request's session
// cookie comes without the CSRF header, so a signed-in browser can sign in
// again. SameSite=Lax keeps other sites from posting them with the cookie.
func isSignInCall(r *http.Request) bool {
	if r.Method != http.MethodPost {
		return false
	}
	p := r.URL.Path
	switch {
	case p == "/api/hangar/v1/auth/login", p == "/auth/register":
		return true
	case strings.HasPrefix(p, "/auth/invite/"):
		return !strings.Contains(strings.TrimPrefix(p, "/auth/invite/"), "/")
	case strings.HasPrefix(p, "/auth/reset/"):
		return !strings.Contains(strings.TrimPrefix(p, "/auth/reset/"), "/")
	}
	return false
}

// staleKey holds a *staleFlag in a request context set up by Middleware.
type staleKey struct{}

// staleFlag records that a request carried a stale session cookie.
type staleFlag struct{ set atomic.Bool }

// staleSession marks the request's session cookie as stale, so Middleware
// clears it, and returns the anonymous Principal.
func staleSession(ctx context.Context) model.Principal {
	if f, ok := ctx.Value(staleKey{}).(*staleFlag); ok {
		f.set.Store(true)
	}
	return model.Anonymous()
}

// Middleware is httpx.Authenticate with one addition: when the request's
// session cookie is stale, the response clears the session and CSRF
// cookies, so the browser stops sending them. authn is usually s, perhaps
// wrapped; onError is as for httpx.Authenticate.
func (s *Service) Middleware(authn httpx.Authenticator, onError func(w http.ResponseWriter, r *http.Request, err error)) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		authed := httpx.Authenticate(authn, onError)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if f, ok := r.Context().Value(staleKey{}).(*staleFlag); ok && f.set.Load() {
				s.ClearCookies(w)
			}
			next.ServeHTTP(w, r)
		}))
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			authed.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), staleKey{}, &staleFlag{})))
		})
	}
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
// missing CSRF token, the protocol's 401 for a bad credential, 499 when the
// client closed the request before its credentials were checked, and 500
// when the authentication store failed. Pass it to httpx.Authenticate as
// onError. It classifies the request first, because authentication runs
// before the protocol dispatcher.
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
		if errors.Is(err, ErrCrossOrigin) {
			httpx.WriteError(w, r, http.StatusForbidden, "", "Hangar refuses changes sent from another site with your browser's credentials. Make the change from Hangar itself, or send a Token as a Bearer token.")
			return
		}
		if errors.Is(err, ErrBasicNotAccepted) {
			httpx.WriteError(w, r, http.StatusUnauthorized, "", "HTTP Basic credentials are accepted only by git and git-lfs. Send the Token in an Authorization: Bearer header.")
			return
		}
		if !errors.Is(err, ErrInvalidCredentials) && r.Context().Err() != nil {
			// Nobody reads this answer; it is written for access logs and
			// metrics.
			writeClientClosed(w, r)
			return
		}
		if !errors.Is(err, ErrInvalidCredentials) {
			httpx.WriteError(w, r, http.StatusInternalServerError, "", "Authentication is unavailable. The request ID is "+httpx.RequestID(r.Context())+".")
			return
		}
		httpx.WriteUnauthorized(w, r)
	}
}

// writeClientClosed answers a request the client closed, in the format of
// the request's protocol.
func writeClientClosed(w http.ResponseWriter, r *http.Request) {
	const msg = httpx.ClientClosedMessage
	switch httpx.ProtocolOf(r.Context()) {
	case telemetry.ProtocolNative, telemetry.ProtocolUI, telemetry.ProtocolUnknown, telemetry.ProtocolHealth:
		httpx.WriteProblem(w, r, &httpx.Problem{Status: httpx.StatusClientClosedRequest, Title: "Client Closed Request", Detail: msg})
	default:
		httpx.WriteError(w, r, httpx.StatusClientClosedRequest, "", msg)
	}
}
