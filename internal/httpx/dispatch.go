package httpx

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/jamesagarside/hangar/internal/telemetry"
)

// Protocols holds one handler per protocol that Hangar serves on its single
// host (docs/spec/02-architecture.md "Routing"). Each handler receives the
// full request path, except Viewer, which receives paths relative to the
// Dataset Viewer root (`/splits`, `/rows`, ...) whether the request came to
// `/datasets-server/...` or to the `datasets-server.<host>` hostname.
type Protocols struct {
	// Health serves /healthz and /readyz.
	Health http.Handler
	// Native serves /api/hangar/v1/...
	Native http.Handler
	// Hub serves the Hub HTTP API (/api/...), file downloads
	// (/{ns}/{name}/resolve/... and /datasets/...), and device-code login
	// (/oauth/device, /oauth/token).
	Hub http.Handler
	// Git serves git smart HTTP: info/refs, git-upload-pack and git-receive-pack.
	Git http.Handler
	// LFS serves the git-lfs API under /{ns}/{name}.git/info/lfs/...
	LFS http.Handler
	// Ollama serves the Ollama registry under /v2/...
	Ollama http.Handler
	// Xet serves the Xet CAS endpoints under /xet-cas/... and /v1/telemetry.
	Xet http.Handler
	// Viewer serves the Dataset Viewer API.
	Viewer http.Handler
	// Auth serves sign-in, sign-out and OIDC callbacks under /auth/...,
	// except the sign-in, invite and reset pages, which are UI routes.
	Auth http.Handler
	// UI serves /assets/... and every other path as the web UI.
	UI http.Handler
}

// Target is where Classify sends a request.
type Target int

// Targets.
const (
	TargetUI Target = iota
	TargetHealth
	TargetNative
	TargetHub
	TargetGit
	TargetLFS
	TargetOllama
	TargetXet
	TargetViewer
	TargetAuth
)

// Protocol returns the telemetry protocol name of the target.
func (t Target) Protocol() telemetry.Protocol {
	switch t {
	case TargetHealth:
		return telemetry.ProtocolHealth
	case TargetNative:
		return telemetry.ProtocolNative
	case TargetHub:
		return telemetry.ProtocolHub
	case TargetGit:
		return telemetry.ProtocolGit
	case TargetLFS:
		return telemetry.ProtocolLFS
	case TargetOllama:
		return telemetry.ProtocolOllama
	case TargetXet:
		return telemetry.ProtocolXet
	case TargetViewer:
		return telemetry.ProtocolViewer
	case TargetAuth:
		return telemetry.ProtocolNative
	default:
		return telemetry.ProtocolUI
	}
}

// ViewerPrefix is the path prefix of the Dataset Viewer on the main host.
const ViewerPrefix = "/datasets-server"

// Classify decides which protocol handles a request. viewerHost is
// `datasets-server.<public host>`; requests for that host always go to the
// Viewer.
func Classify(r *http.Request, viewerHost string) Target {
	if viewerHost != "" && strings.EqualFold(r.Host, viewerHost) {
		return TargetViewer
	}
	p := r.URL.EscapedPath()
	switch {
	case p == "/healthz" || p == "/readyz":
		return TargetHealth
	case p == "/api/hangar/v1" || strings.HasPrefix(p, "/api/hangar/v1/"):
		return TargetNative
	case p == "/api" || strings.HasPrefix(p, "/api/"), p == "/oauth/device", p == "/oauth/token":
		return TargetHub
	case strings.HasPrefix(p, "/v2/") || p == "/v2":
		return TargetOllama
	case strings.HasPrefix(p, "/xet-cas/") || p == "/v1/telemetry":
		return TargetXet
	case p == ViewerPrefix || strings.HasPrefix(p, ViewerPrefix+"/"):
		return TargetViewer
	case strings.HasPrefix(p, "/auth/"):
		if isAuthPage(r, p) {
			return TargetUI
		}
		return TargetAuth
	case strings.HasPrefix(p, "/assets/"):
		return TargetUI
	}
	return classifyRepoPath(p)
}

// isAuthPage reports whether the request is for one of the sign-in, invite,
// password reset or email confirmation pages, which the UI renders.
func isAuthPage(r *http.Request, p string) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	return p == "/auth/login" || strings.HasPrefix(p, "/auth/invite/") || strings.HasPrefix(p, "/auth/reset/") ||
		strings.HasPrefix(p, "/auth/email/")
}

// classifyRepoPath handles paths that start with a Repo ID, with or without
// the `datasets/` prefix: resolve downloads, git and git-lfs. Anything else
// is a UI route.
func classifyRepoPath(escaped string) Target {
	segs := strings.Split(strings.TrimPrefix(escaped, "/"), "/")
	if len(segs) > 0 && segs[0] == "datasets" {
		segs = segs[1:]
	}
	if len(segs) < 2 {
		return TargetUI
	}
	name := segs[1]
	rest := segs[2:]
	isGitName := strings.HasSuffix(name, ".git")
	if len(rest) >= 2 && rest[0] == "info" && rest[1] == "lfs" {
		return TargetLFS
	}
	if len(rest) == 2 && rest[0] == "info" && rest[1] == "refs" {
		return TargetGit
	}
	if len(rest) == 1 && (rest[0] == "git-upload-pack" || rest[0] == "git-receive-pack") {
		return TargetGit
	}
	if isGitName {
		return TargetGit
	}
	if len(rest) >= 1 && rest[0] == "resolve" {
		return TargetHub
	}
	// Single-segment Repo IDs, such as /gpt2/resolve/main/config.json, which
	// the Hub redirects to the canonical ID.
	if len(segs) >= 4 && segs[1] == "resolve" {
		return TargetHub
	}
	return TargetUI
}

// Dispatch returns a handler that routes every request to its protocol and
// records the protocol for logs, spans and error formatting. publicURL is
// server.public_url; it gives the `datasets-server.<host>` name.
func Dispatch(p Protocols, publicURL string) http.Handler {
	viewerHost := ""
	if u, err := url.Parse(publicURL); err == nil && u.Host != "" {
		viewerHost = "datasets-server." + u.Host
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		target := Classify(r, viewerHost)
		SetProtocol(r.Context(), target.Protocol())
		var h http.Handler
		switch target {
		case TargetHealth:
			h = p.Health
		case TargetNative:
			h = p.Native
		case TargetHub:
			h = p.Hub
		case TargetGit:
			h = p.Git
		case TargetLFS:
			h = p.LFS
		case TargetOllama:
			h = p.Ollama
		case TargetXet:
			h = p.Xet
		case TargetViewer:
			h = p.Viewer
			if !strings.EqualFold(r.Host, viewerHost) {
				r = stripPrefix(r, ViewerPrefix)
			}
		case TargetAuth:
			h = p.Auth
		default:
			h = p.UI
		}
		if h == nil {
			WriteError(w, r, http.StatusNotFound, "", "Not found.")
			return
		}
		h.ServeHTTP(w, r)
	})
}

func stripPrefix(r *http.Request, prefix string) *http.Request {
	r2 := r.Clone(r.Context())
	r2.URL.Path = strings.TrimPrefix(r.URL.Path, prefix)
	if r.URL.RawPath != "" {
		r2.URL.RawPath = strings.TrimPrefix(r.URL.RawPath, prefix)
	}
	if r2.URL.Path == "" {
		r2.URL.Path = "/"
	}
	return r2
}
