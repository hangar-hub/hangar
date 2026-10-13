// Package web serves the embedded web UI: hashed assets with long cache
// lifetimes, and index.html for every UI route so the client-side router
// can render it, with a strict Content Security Policy
// (docs/spec/09-web-ui.md).
package web

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"regexp"
	"strings"

	"github.com/jamesagarside/hangar/internal/httpx"
)

//go:embed all:dist
var dist embed.FS

// Options configures the UI handler.
type Options struct {
	// ConnectSrc adds origins the UI may call, such as the S3 presign host
	// for direct uploads.
	ConnectSrc []string
	// FS replaces the embedded build, for tests.
	FS fs.FS
}

var hashedAsset = regexp.MustCompile(`-[A-Za-z0-9_-]{8,}\.[a-z0-9]+$`)

// CSP returns the Content Security Policy for UI pages: no inline scripts,
// API calls to the same origin plus connectSrc.
func CSP(connectSrc []string) string {
	connect := append([]string{"'self'"}, connectSrc...)
	return strings.Join([]string{
		"default-src 'self'",
		"script-src 'self'",
		"style-src 'self'",
		"img-src 'self' data: blob: https:",
		"media-src 'self' blob:",
		"font-src 'self'",
		"connect-src " + strings.Join(connect, " "),
		"worker-src 'self' blob:",
		"object-src 'none'",
		"base-uri 'self'",
		"form-action 'self'",
		"frame-ancestors 'none'",
	}, "; ")
}

// Handler returns the UI handler.
func Handler(opts Options) http.Handler {
	root := opts.FS
	if root == nil {
		sub, err := fs.Sub(dist, "dist")
		if err != nil {
			panic(err) // the embed directive guarantees dist exists
		}
		root = sub
	}
	files := http.FS(root)
	csp := CSP(opts.ConnectSrc)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpx.SetRoute(r.Context(), "/{ui}")
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			httpx.WriteError(w, r, http.StatusMethodNotAllowed, "", "Method not allowed.")
			return
		}
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		if strings.HasPrefix(r.URL.Path, "/assets/") {
			httpx.SetRoute(r.Context(), "/assets/{file}")
			name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
			if !strings.HasPrefix(name, "assets/") {
				httpx.WriteError(w, r, http.StatusNotFound, "", "Asset not found.")
				return
			}
			if f, err := root.Open(name); err == nil {
				_ = f.Close()
				if hashedAsset.MatchString(name) {
					h.Set("Cache-Control", "public, max-age=31536000, immutable")
				} else {
					h.Set("Cache-Control", "public, max-age=3600")
				}
				http.FileServer(files).ServeHTTP(w, r)
				return
			}
			httpx.WriteError(w, r, http.StatusNotFound, "", "Asset not found.")
			return
		}
		index, err := fs.ReadFile(root, "index.html")
		if err != nil {
			httpx.WriteError(w, r, http.StatusServiceUnavailable, "", "The web UI is not built into this binary. Run make build.")
			return
		}
		h.Set("Content-Security-Policy", csp)
		h.Set("Cache-Control", "no-cache")
		h.Set("Content-Type", "text/html; charset=utf-8")
		h.Set("X-Frame-Options", "DENY")
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			_, _ = w.Write(index)
		}
	})
}
