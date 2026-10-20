package web_test

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/httpx"
	"github.com/hangar-hub/hangar/internal/web"
)

func serve(h http.Handler, method, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, nil)
	req = req.WithContext(httpx.WithInfo(req.Context(), "rid"))
	h.ServeHTTP(rec, req)
	return rec
}

var nonceMeta = regexp.MustCompile(`<meta name="csp-nonce" content="([A-Za-z0-9+/=]{24})" />`)

func TestServesIndexForUIRoutesWithCSP(t *testing.T) {
	h := web.Handler(web.Options{
		ConnectSrc: []string{"https://s3.example.com"},
		FS: fstest.MapFS{
			"index.html":               {Data: []byte("<!doctype html><html><head><title>Hangar</title></head><div id=root></div>")},
			"assets/index-AbCdEf12.js": {Data: []byte("console.log(1)")},
			"assets/hangar.svg":        {Data: []byte("<svg/>")},
		},
	})
	seen := map[string]bool{}
	for _, p := range []string{"/", "/arsenal", "/models", "/datasets", "/ns/name/tree/main/a/b", "/datasets/ns/d/viewer/default/train"} {
		rec := serve(h, http.MethodGet, p)
		require.Equal(t, 200, rec.Code, p)
		body := rec.Body.String()
		require.Contains(t, body, "id=root")
		m := nonceMeta.FindStringSubmatch(body)
		require.NotNil(t, m, "the page carries the style nonce: %s", body)
		require.True(t, strings.HasPrefix(body, `<!doctype html><html><head><meta name="csp-nonce"`))
		nonce := m[1]
		require.False(t, seen[nonce], "every response gets a new nonce")
		seen[nonce] = true
		csp := rec.Header().Get("Content-Security-Policy")
		require.Contains(t, csp, "script-src 'self';")
		require.Contains(t, csp, "style-src 'self' 'nonce-"+nonce+"';")
		require.NotContains(t, csp, "unsafe-inline")
		require.NotContains(t, csp, "unsafe-eval")
		require.Contains(t, csp, "connect-src 'self' https://s3.example.com")
		require.Contains(t, csp, "frame-ancestors 'none'")
		require.Equal(t, "no-cache", rec.Header().Get("Cache-Control"))
		require.Equal(t, "DENY", rec.Header().Get("X-Frame-Options"))
		require.Equal(t, "same-origin", rec.Header().Get("Cross-Origin-Opener-Policy"))
		require.Equal(t, web.PermissionsPolicy, rec.Header().Get("Permissions-Policy"))
		require.Contains(t, rec.Header().Get("Permissions-Policy"), "camera=()")
		require.Equal(t, "nosniff", rec.Header().Get("X-Content-Type-Options"))
		require.Equal(t, "strict-origin-when-cross-origin", rec.Header().Get("Referrer-Policy"))
	}
	rec := serve(h, http.MethodGet, "/assets/index-AbCdEf12.js")
	require.Equal(t, 200, rec.Code)
	require.Contains(t, rec.Header().Get("Cache-Control"), "immutable")
	require.Empty(t, rec.Header().Get("Content-Security-Policy"))
	rec = serve(h, http.MethodGet, "/assets/hangar.svg")
	require.Equal(t, "public, max-age=3600", rec.Header().Get("Cache-Control"))
	rec = serve(h, http.MethodGet, "/assets/../index.html")
	require.Equal(t, 404, rec.Code)
	require.Equal(t, 404, serve(h, http.MethodGet, "/assets/missing.js").Code)
	rec = serve(h, http.MethodHead, "/arsenal")
	require.Equal(t, 200, rec.Code)
	require.Empty(t, rec.Body.String())
	require.Equal(t, 405, serve(h, http.MethodPost, "/arsenal").Code)
}

func TestNonceWithoutHead(t *testing.T) {
	h := web.Handler(web.Options{FS: fstest.MapFS{"index.html": {Data: []byte("<div id=root></div>")}}})
	rec := serve(h, http.MethodGet, "/")
	require.Regexp(t, `^<meta name="csp-nonce" content="[^"]+" /><meta name="docs-url" content="" /><div id=root></div>$`, rec.Body.String())
}

func TestDocsURLMeta(t *testing.T) {
	index := fstest.MapFS{"index.html": {Data: []byte("<head></head>")}}
	rec := serve(web.Handler(web.Options{FS: index, DocsURL: "https://docs.internal/hangar/"}), http.MethodGet, "/")
	require.Contains(t, rec.Body.String(), `<meta name="docs-url" content="https://docs.internal/hangar/" />`)

	rec = serve(web.Handler(web.Options{FS: index, DocsURL: `https://docs.internal/?a="b"&c`}), http.MethodGet, "/")
	require.Contains(t, rec.Body.String(), `content="https://docs.internal/?a=&#34;b&#34;&amp;c"`, "the URL cannot break out of the attribute")

	rec = serve(web.Handler(web.Options{FS: index}), http.MethodGet, "/")
	require.Contains(t, rec.Body.String(), `<meta name="docs-url" content="" />`, "empty removes the UI's docs links")
}

func TestCSPWithoutNonce(t *testing.T) {
	csp := web.CSP(nil, "")
	require.Contains(t, csp, "style-src 'self';")
	require.Contains(t, csp, "connect-src 'self';")
}

func TestMissingBuild(t *testing.T) {
	h := web.Handler(web.Options{FS: fstest.MapFS{}})
	rec := serve(h, http.MethodGet, "/")
	require.Equal(t, 503, rec.Code)
	require.True(t, strings.Contains(rec.Body.String(), "make build"))
}

func TestEmbeddedBuildLoads(t *testing.T) {
	// The embedded dist always exists (it holds at least .gitkeep).
	rec := serve(web.Handler(web.Options{}), http.MethodGet, "/")
	require.Contains(t, []int{200, 503}, rec.Code)
}
