package httpx_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/hangar-hub/hangar/internal/httpx"
)

func TestRedactPath(t *testing.T) {
	prefixes := []string{"/auth/invite/", "/auth/reset/"}
	for _, c := range []struct {
		in, want string
		changed  bool
	}{
		{"/auth/invite/abc123", "/auth/invite/{code}", true},
		{"/auth/reset/abc123/extra", "/auth/reset/{code}/extra", true},
		{"/auth/invite/", "/auth/invite/", false},
		{"/auth/login", "/auth/login", false},
		{"/alice/auth/invite/abc", "/alice/auth/invite/abc", false},
	} {
		got, changed := httpx.RedactPath(c.in, prefixes)
		require.Equal(t, c.want, got, c.in)
		require.Equal(t, c.changed, changed, c.in)
	}
}

// TestHideSecretPathsKeepsCodesOffSpans covers security review finding 6:
// otelhttp records the raw path as url.path, which put invite, reset and
// email confirmation codes on every span for those links.
func TestHideSecretPathsKeepsCodesOffSpans(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	var seen []string
	h := httpx.HideSecretPaths(func(next http.Handler) http.Handler {
		return otelhttp.NewHandler(next, "http", otelhttp.WithTracerProvider(tp))
	}, "/auth/invite/", "/auth/reset/", "/auth/email/")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.Path+" "+r.RequestURI)
		w.WriteHeader(http.StatusNoContent)
	}))

	const code = "secret-code-4f2a9c"
	for _, target := range []string{"/auth/invite/" + code, "/auth/reset/" + code + "?x=1", "/auth/email/" + code, "/alice/model"} {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, target, nil))
		require.Equal(t, http.StatusNoContent, rr.Code)
	}
	require.Equal(t, []string{
		"/auth/invite/" + code + " /auth/invite/" + code,
		"/auth/reset/" + code + " /auth/reset/" + code + "?x=1",
		"/auth/email/" + code + " /auth/email/" + code,
		"/alice/model /alice/model",
	}, seen, "the handler gets the real path")

	spans := rec.Ended()
	require.Len(t, spans, 4)
	paths := map[string]bool{}
	for _, s := range spans {
		require.NotContains(t, s.Name(), code)
		for _, a := range s.Attributes() {
			require.NotContains(t, a.Value.String(), code, "attribute %s", a.Key)
			if a.Key == attribute.Key("url.path") {
				paths[a.Value.AsString()] = true
			}
		}
	}
	require.Equal(t, map[string]bool{"/auth/invite/{code}": true, "/auth/reset/{code}": true, "/auth/email/{code}": true, "/alice/model": true}, paths)
}
