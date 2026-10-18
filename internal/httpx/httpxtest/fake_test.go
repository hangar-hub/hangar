package httpxtest_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/httpx"
	"github.com/hangar-hub/hangar/internal/httpx/httpxtest"
)

func TestStandInsAnswerInTheirProtocolFormat(t *testing.T) {
	ui := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ui")) })
	h := httpx.RequestIDs(httpx.Dispatch(httpxtest.Protocols(httpx.Protocols{UI: ui}), "http://localhost:8080"))
	for path, want := range map[string]string{
		"/api/models/a/b":           "Hub API",
		"/api/hangar/v1/me":         "native API",
		"/a/b.git/info/refs":        "git",
		"/a/b.git/info/lfs/objects": "git-lfs",
		"/v2/a/b/manifests/latest":  "Ollama registry",
		"/xet-cas/v1/xorbs/x/y":     "Xet CAS",
		"/datasets-server/splits":   "Dataset Viewer",
		"/auth/oidc/x/callback":     "sign-in",
		"/healthz":                  "health",
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		require.Equal(t, http.StatusNotImplemented, rec.Code, path)
		require.Equal(t, want, rec.Header().Get("X-Hangar-Fake"), path)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/arsenal", nil))
	require.Equal(t, "ui", rec.Body.String(), "handlers already set are kept")
}
