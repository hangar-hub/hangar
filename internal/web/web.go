// Package web serves the embedded web UI: hashed assets with long cache
// lifetimes, and index.html for every UI route so the client-side router
// can render it, with a strict Content Security Policy
// (docs/spec/09-web-ui.md).
package web

import (
	"bytes"
	"crypto/rand"
	"embed"
	"encoding/base64"
	"io/fs"
	"net/http"
	"path"
	"regexp"
	"strings"

	"github.com/hangar-hub/hangar/internal/httpx"
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

// NonceMeta is the name of the meta element that carries the per-response
// style nonce to the UI, which hands it to the libraries that add style
// elements at run time (modal windows lock page scrolling that way).
const NonceMeta = "csp-nonce"

// CSP returns the Content Security Policy for UI pages: no inline scripts,
// stylesheets from the same origin plus style elements carrying nonce, and
// API calls to the same origin plus connectSrc. An empty nonce allows no
// style elements.
func CSP(connectSrc []string, nonce string) string {
	connect := append([]string{"'self'"}, connectSrc...)
	style := "style-src 'self'"
	if nonce != "" {
		style += " 'nonce-" + nonce + "'"
	}
	return strings.Join([]string{
		"default-src 'self'",
		"script-src 'self'",
		style,
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

// PermissionsPolicy turns off the browser features the UI never uses.
const PermissionsPolicy = "camera=(), microphone=(), geolocation=(), payment=(), usb=(), serial=(), hid=(), bluetooth=()"

// newNonce returns 128 random bits, base64 encoded, for one response.
func newNonce() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b) // crypto/rand.Read never fails on supported platforms
	return base64.StdEncoding.EncodeToString(b)
}

// withNonce adds the nonce meta element at the start of the head.
func withNonce(index []byte, nonce string) []byte {
	meta := []byte(`<meta name="` + NonceMeta + `" content="` + nonce + `" />`)
	i := bytes.Index(index, []byte("<head>"))
	if i < 0 {
		return append(meta, index...)
	}
	i += len("<head>")
	out := make([]byte, 0, len(index)+len(meta))
	out = append(out, index[:i]...)
	out = append(out, meta...)
	return append(out, index[i:]...)
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
		nonce := newNonce()
		h.Set("Content-Security-Policy", CSP(opts.ConnectSrc, nonce))
		h.Set("Cache-Control", "no-cache")
		h.Set("Content-Type", "text/html; charset=utf-8")
		h.Set("X-Frame-Options", "DENY")
		// No other window keeps a handle on the UI, and the page asks for
		// no device features.
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Permissions-Policy", PermissionsPolicy)
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			_, _ = w.Write(withNonce(index, nonce))
		}
	})
}
