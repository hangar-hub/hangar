package httpx_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/httpx"
)

func TestHealth(t *testing.T) {
	var dbErr error
	calls := 0
	h := httpx.NewHealth(
		httpx.Check{Name: "database", Run: func(context.Context) error { return dbErr }},
		httpx.Check{Name: "blob", CacheFor: time.Hour, Run: func(context.Context) error { calls++; return nil }},
	)
	get := func(p string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		return rec
	}
	require.Equal(t, 200, get("/healthz").Code)
	require.Equal(t, 200, get("/readyz").Code)
	require.Equal(t, 200, get("/readyz").Code)
	require.Equal(t, 1, calls, "cached checks run once per CacheFor")

	dbErr = errors.New("connection refused")
	rec := get("/readyz")
	require.Equal(t, 503, rec.Code)
	require.JSONEq(t, `{"status":"not ready","failed":[{"name":"database","error":"connection refused"}]}`, rec.Body.String())
	require.Equal(t, 200, get("/healthz").Code, "liveness does not depend on dependencies")

	h.SetDraining()
	require.Equal(t, 503, get("/healthz").Code)
	require.Equal(t, 503, get("/readyz").Code)
	require.Equal(t, 404, get("/other").Code)
}
