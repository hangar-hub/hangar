package web_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/httpx"
	"github.com/jamesagarside/hangar/internal/web"
)

func serve(h http.Handler, method, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, nil)
	req = req.WithContext(httpx.WithInfo(req.Context(), "rid"))
	h.ServeHTTP(rec, req)
	return rec
}

func TestServesIndexForUIRoutesWithCSP(t *testing.T) {
	h := web.Handler(web.Options{
		ConnectSrc: []string{"https://s3.example.com"},
		FS: fstest.MapFS{
			"index.html":               {Data: []byte("<!doctype html><div id=root></div>")},
			"assets/index-AbCdEf12.js": {Data: []byte("console.log(1)")},
			"assets/hangar.svg":        {Data: []byte("<svg/>")},
		},
	})
	for _, p := range []string{"/", "/arsenal", "/ns/name/tree/main/a/b", "/datasets/ns/d/viewer/default/train"} {
		rec := serve(h, http.MethodGet, p)
		require.Equal(t, 200, rec.Code, p)
		require.Contains(t, rec.Body.String(), "id=root")
		csp := rec.Header().Get("Content-Security-Policy")
		require.Contains(t, csp, "script-src 'self'")
		require.NotContains(t, csp, "unsafe-inline")
		require.Contains(t, csp, "connect-src 'self' https://s3.example.com")
		require.Equal(t, "no-cache", rec.Header().Get("Cache-Control"))
	}
	rec := serve(h, http.MethodGet, "/assets/index-AbCdEf12.js")
	require.Equal(t, 200, rec.Code)
	require.Contains(t, rec.Header().Get("Cache-Control"), "immutable")
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
