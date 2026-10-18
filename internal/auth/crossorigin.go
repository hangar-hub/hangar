package auth

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/hangar-hub/hangar/internal/httpx"
)

// ErrCrossOrigin means a state-changing request came from another site
// with a credential the browser adds on its own: the session cookie or
// cached HTTP Basic credentials. It wraps ErrInvalidCredentials;
// WriteAuthError answers it with 403.
var ErrCrossOrigin = fmt.Errorf("%w: cross-origin request with browser credentials", ErrInvalidCredentials)

// ErrBasicNotAccepted means HTTP Basic credentials were sent to a route
// other than git or git-lfs. docs/spec/05-auth.md accepts a Token as a
// Basic password only for git and git-lfs; everything else takes it as a
// Bearer token. It wraps ErrInvalidCredentials; WriteAuthError answers it
// with 401.
var ErrBasicNotAccepted = fmt.Errorf("%w: basic credentials are only accepted by git and git-lfs", ErrInvalidCredentials)

// originGuard rejects cross-origin browser requests with unsafe methods.
// It follows net/http.CrossOriginProtection: Sec-Fetch-Site decides when
// the browser sends it (only `same-origin` and `none` pass); otherwise an
// Origin header must name the request's own host or server.public_url.
// Requests with neither header, which is what git, git-lfs,
// huggingface_hub, the hangar CLI and Ollama send, pass.
type originGuard struct{ cop *http.CrossOriginProtection }

func newOriginGuard(publicURL string) (*originGuard, error) {
	cop := http.NewCrossOriginProtection()
	if u, err := url.Parse(strings.TrimRight(publicURL, "/")); err == nil && u.Scheme != "" && u.Host != "" {
		if err := cop.AddTrustedOrigin(u.Scheme + "://" + u.Host); err != nil {
			return nil, fmt.Errorf("auth: trusted origin from server.public_url: %w", err)
		}
	}
	return &originGuard{cop: cop}, nil
}

// check returns ErrCrossOrigin for a cross-origin request with an unsafe
// method, and nil for everything else.
func (g *originGuard) check(r *http.Request) error {
	if g == nil || safeMethod(r.Method) {
		return nil
	}
	if err := g.cop.Check(r); err != nil {
		return ErrCrossOrigin
	}
	return nil
}

// basicAllowed reports whether a request may authenticate with HTTP Basic
// credentials: git and git-lfs routes may. Static UI and health routes
// ignore them instead, because a browser that once signed in to a git URL
// may send them with page loads.
func basicAllowed(target httpx.Target) (allowed, ignore bool) {
	switch target {
	case httpx.TargetGit, httpx.TargetLFS:
		return true, false
	case httpx.TargetUI, httpx.TargetHealth:
		return false, true
	default:
		return false, false
	}
}
