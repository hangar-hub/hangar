// Package httpxtest provides stand-in protocol handlers for wiring and
// router tests. Each answers 501 in its protocol's error format, naming the
// protocol, so a request that reaches the wrong handler is easy to spot.
// Phase 0 mounts them in cmd/hangar until the workstreams' handlers exist;
// docs/spec/15-build-plan.md phase 2 replaces every one.
package httpxtest

import (
	"net/http"

	"github.com/jamesagarside/hangar/internal/httpx"
)

// Protocol returns a handler that answers 501 for the named protocol.
func Protocol(name string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpx.SetRoute(r.Context(), "/{"+name+"}")
		w.Header().Set("X-Hangar-Fake", name)
		httpx.WriteError(w, r, http.StatusNotImplemented, "", "The "+name+" handler is not wired into this build.")
	})
}

// Protocols returns a full httpx.Protocols of stand-ins, keeping any
// handler already set in base.
func Protocols(base httpx.Protocols) httpx.Protocols {
	fill := func(h *http.Handler, name string) {
		if *h == nil {
			*h = Protocol(name)
		}
	}
	fill(&base.Health, "health")
	fill(&base.Native, "native API")
	fill(&base.Hub, "Hub API")
	fill(&base.Git, "git")
	fill(&base.LFS, "git-lfs")
	fill(&base.Ollama, "Ollama registry")
	fill(&base.Xet, "Xet CAS")
	fill(&base.Viewer, "Dataset Viewer")
	fill(&base.Auth, "sign-in")
	fill(&base.UI, "web UI")
	return base
}
