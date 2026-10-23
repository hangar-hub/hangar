package httpx

import (
	"context"
	"net/http"
	"net/url"
	"strings"
)

// RedactedSegment replaces a secret path segment in what telemetry sees.
const RedactedSegment = "{code}"

// RedactPath returns path with the segment after each of prefixes replaced
// by RedactedSegment, and whether it changed anything. A prefix such as
// "/auth/reset/" turns "/auth/reset/abc" into "/auth/reset/{code}" and
// keeps anything after the code.
func RedactPath(path string, prefixes []string) (string, bool) {
	for _, p := range prefixes {
		rest, ok := strings.CutPrefix(path, p)
		if !ok || rest == "" {
			continue
		}
		tail := ""
		if i := strings.IndexByte(rest, '/'); i >= 0 {
			tail = rest[i:]
		}
		return p + RedactedSegment + tail, true
	}
	return path, false
}

type originalURLKey struct{}

type originalURL struct {
	url        *url.URL
	requestURI string
}

// HideSecretPaths wraps a tracing middleware, such as otelhttp's, so it
// never sees the secret segment of paths that carry one: invite, password
// reset and email confirmation codes. otelhttp records the raw path as the
// url.path attribute of every server span, and anyone who can read the
// tracing backend could use an unused code. The tracing middleware gets a
// copy of the request whose path names the code as RedactedSegment; the
// handler inside it gets the request with the real path back.
func HideSecretPaths(trace func(http.Handler) http.Handler, prefixes ...string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		restore := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if orig, ok := r.Context().Value(originalURLKey{}).(originalURL); ok {
				r = r.WithContext(r.Context())
				r.URL, r.RequestURI = orig.url, orig.requestURI
			}
			next.ServeHTTP(w, r)
		})
		traced := trace(restore)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			redacted, ok := RedactPath(r.URL.Path, prefixes)
			if !ok {
				traced.ServeHTTP(w, r)
				return
			}
			hidden := r.WithContext(context.WithValue(r.Context(), originalURLKey{}, originalURL{url: r.URL, requestURI: r.RequestURI}))
			u := *r.URL
			u.Path, u.RawPath = redacted, ""
			hidden.URL = &u
			hidden.RequestURI = u.RequestURI()
			traced.ServeHTTP(w, hidden)
		})
	}
}
