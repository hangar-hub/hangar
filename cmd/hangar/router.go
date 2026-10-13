package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/jamesagarside/hangar/internal/api/gen"
	"github.com/jamesagarside/hangar/internal/auth/authtest"
	"github.com/jamesagarside/hangar/internal/blob"
	"github.com/jamesagarside/hangar/internal/config"
	"github.com/jamesagarside/hangar/internal/db"
	"github.com/jamesagarside/hangar/internal/httpx"
	"github.com/jamesagarside/hangar/internal/httpx/httpxtest"
	"github.com/jamesagarside/hangar/internal/web"
)

// deps are the services the router needs. Phase 0 of the build plan wires
// fakes for everything except the database, health and the UI; phase 2
// replaces them with the real implementations.
type deps struct {
	cfg   *config.Config
	log   *slog.Logger
	db    *db.DB
	blobs blob.Store
	authn httpx.Authenticator
}

// newHealth builds the readiness checks: database reachable, migrations at
// the expected version, and the Blob store sentinel readable.
func newHealth(d deps) *httpx.Health {
	return httpx.NewHealth(
		httpx.Check{Name: "database", Run: func(ctx context.Context) error {
			if err := d.db.PingContext(ctx); err != nil {
				return fmt.Errorf("database unreachable: %w", err)
			}
			st, err := d.db.Schema(ctx)
			if err != nil {
				return err
			}
			if !st.UpToDate() {
				return fmt.Errorf("database schema is at %q; this binary expects %q (run hangar admin migrate)", st.Current, st.Expected)
			}
			return nil
		}},
		httpx.Check{Name: "blob_store", CacheFor: 10 * time.Second, Run: func(ctx context.Context) error {
			_, err := d.blobs.Stat(ctx, blob.SentinelKey)
			return err
		}},
	)
}

// ensureSentinel writes the Blob store sentinel that /readyz checks.
func ensureSentinel(ctx context.Context, s blob.Store) error {
	if _, err := s.Stat(ctx, blob.SentinelKey); err == nil {
		return nil
	} else if !errors.Is(err, blob.ErrNotFound) {
		return err
	}
	const body = "hangar"
	return s.Put(ctx, blob.SentinelKey, strings.NewReader(body), int64(len(body)))
}

// newHandler assembles the middleware chain and the protocol dispatcher
// for the routes in docs/spec/02-architecture.md.
func newHandler(d deps, health *httpx.Health) (http.Handler, error) {
	trusted := make([]netip.Prefix, 0, len(d.cfg.Server.TrustedProxies))
	for _, p := range d.cfg.Server.TrustedProxies {
		prefix, err := config.ParsePrefix(p)
		if err != nil {
			return nil, err
		}
		trusted = append(trusted, prefix)
	}
	native := gen.HandlerWithOptions(gen.Unimplemented{}, gen.ChiServerOptions{
		BaseURL: "/api/hangar/v1",
		// Record the route template for the access log and the span name.
		Middlewares: []gen.MiddlewareFunc{func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if rc := chi.RouteContext(r.Context()); rc != nil {
					httpx.SetRoute(r.Context(), rc.RoutePattern())
				}
				next.ServeHTTP(w, r)
			})
		}},
		ErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			httpx.WriteProblem(w, r, &httpx.Problem{Status: http.StatusBadRequest, Detail: err.Error()})
		},
	})
	protocols := httpxtest.Protocols(httpx.Protocols{
		Health: health,
		Native: http.MaxBytesHandler(native, d.cfg.Server.MaxRequestBody.Int64()),
		UI:     web.Handler(web.Options{}),
	})
	h := httpx.Dispatch(protocols, d.cfg.Server.PublicURL)
	h = httpx.Authenticate(d.authn, nil)(h)
	h = httpx.AccessLog(d.log)(h)
	h = httpx.Recover(d.log)(h)
	h = httpx.TrustedProxies(trusted)(h)
	h = otelhttp.NewHandler(h, "http", otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string { return r.Method }))
	h = httpx.RequestIDs(h)
	return h, nil
}

// stubAuthenticator is the phase 0 Authenticator: every request is anonymous
// until internal/auth is wired in phase 2.
func stubAuthenticator() httpx.Authenticator { return authtest.NewAuthenticator() }
