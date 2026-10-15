package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/jamesagarside/hangar/internal/api"
	"github.com/jamesagarside/hangar/internal/api/gen"
	"github.com/jamesagarside/hangar/internal/auth"
	"github.com/jamesagarside/hangar/internal/blob"
	"github.com/jamesagarside/hangar/internal/config"
	"github.com/jamesagarside/hangar/internal/db"
	"github.com/jamesagarside/hangar/internal/httpx"
	"github.com/jamesagarside/hangar/internal/ollama"
	"github.com/jamesagarside/hangar/internal/telemetry"
	"github.com/jamesagarside/hangar/internal/upstream"
	"github.com/jamesagarside/hangar/internal/web"
	"github.com/jamesagarside/hangar/internal/xet"
)

// newHealth builds the readiness checks: database reachable, migrations at
// the expected version, the Blob store sentinel readable, and any extra
// checks.
func newHealth(d *db.DB, blobs blob.Store, extra ...httpx.Check) *httpx.Health {
	return httpx.NewHealth(append([]httpx.Check{
		{Name: "database", Run: func(ctx context.Context) error {
			if err := d.PingContext(ctx); err != nil {
				return fmt.Errorf("database unreachable: %w", err)
			}
			st, err := d.Schema(ctx)
			if err != nil {
				return err
			}
			if !st.UpToDate() {
				return fmt.Errorf("database schema is at %q; this binary expects %q (run hangar admin migrate)", st.Current, st.Expected)
			}
			return nil
		}},
		{Name: "blob_store", CacheFor: 10 * time.Second, Run: func(ctx context.Context) error {
			_, err := blobs.Stat(ctx, blob.SentinelKey)
			return err
		}},
	}, extra...)...)
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

// uploadPartPath matches native API upload part PUTs, which stream into
// the Blob store and so have no body limit (parts are 16 MiB to 512 MiB).
var uploadPartPath = regexp.MustCompile(`^/api/hangar/v1/repos/[^/]+/[^/]+/[^/]+/uploads/[^/]+/parts/[0-9]+$`)

// newHandler assembles the middleware chain and the protocol dispatcher
// for the routes in docs/spec/02-architecture.md. A worker process serves
// only the health endpoints.
func newHandler(ctx context.Context, in *instance, health *httpx.Health, workerOnly bool) (http.Handler, error) {
	cfg := in.cfg
	trusted := make([]netip.Prefix, 0, len(cfg.Server.TrustedProxies))
	for _, p := range cfg.Server.TrustedProxies {
		prefix, err := config.ParsePrefix(p)
		if err != nil {
			return nil, err
		}
		trusted = append(trusted, prefix)
	}
	viewerHost := ""
	if u, err := url.Parse(cfg.Server.PublicURL); err == nil && u.Host != "" {
		viewerHost = "datasets-server." + u.Host
	}

	var h http.Handler
	if workerOnly {
		h = httpx.Dispatch(httpx.Protocols{Health: health}, cfg.Server.PublicURL)
	} else {
		var err error
		if h, err = protocolHandler(ctx, in, health, viewerHost); err != nil {
			return nil, err
		}
	}
	h = httpx.AccessLog(in.log)(h)
	h = httpx.Recover(in.log)(h)
	h = httpx.TrustedProxies(trusted)(h)
	h = otelhttp.NewHandler(h, "http", otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string { return r.Method }))
	h = httpx.RequestIDs(h)
	return h, nil
}

// protocolHandler serves every protocol on one host. Requests are
// authenticated before dispatch, except where the URL or the protocol
// carries its own credential: signed export and Pull-through links, and
// Xet CAS requests, whose bearer is a CAS token the Xet handler verifies.
func protocolHandler(ctx context.Context, in *instance, health *httpx.Health, viewerHost string) (http.Handler, error) {
	cfg := in.cfg
	srv, err := newNativeServer(in)
	if err != nil {
		return nil, err
	}
	native := gen.HandlerWithOptions(srv, gen.ChiServerOptions{
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
	limited := http.MaxBytesHandler(native, cfg.Server.MaxRequestBody.Int64())
	nativeLimited := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && uploadPartPath.MatchString(r.URL.EscapedPath()) {
			native.ServeHTTP(w, r)
			return
		}
		limited.ServeHTTP(w, r)
	})

	casURL := in.casURL
	cas, err := xet.NewHandler(in.xet, xet.HandlerOptions{CASURL: casURL, Log: in.log})
	if err != nil {
		return nil, err
	}
	casHost := ""
	if u, err := url.Parse(casURL); err == nil && u.Host != "" {
		if pu, err := url.Parse(cfg.Server.PublicURL); err == nil && !strings.EqualFold(u.Host, pu.Host) {
			casHost = u.Host
		}
	}

	protocols := httpx.Protocols{
		Health: health,
		Native: nativeLimited,
		Hub:    in.hub,
		Git:    in.gitServer.Git(),
		LFS:    in.gitServer.LFS(),
		Ollama: in.ollama,
		Xet:    cas,
		Auth:   in.auth.Handler(),
		UI:     web.Handler(web.Options{ConnectSrc: in.presignOrigins(ctx)}),
	}
	if in.viewer != nil {
		protocols.Viewer = in.viewer.Handler()
	}
	dispatch := httpx.Dispatch(protocols, cfg.Server.PublicURL)
	// The Ollama token endpoint takes an Ollama key signature, which the
	// Ollama handler verifies; WriteAuthError classifies the protocol first,
	// so bad credentials get each client's own challenge.
	authed := httpx.Authenticate(ollama.ExemptTokenEndpoint(in.auth), auth.WriteAuthError(cfg.Server.PublicURL))(dispatch)

	exports := api.ExportHandler(in.blobs, in.signer)
	pullThrough := in.pulls.PullThrough().Handler()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.EscapedPath()
		switch {
		case casHost != "" && strings.EqualFold(r.Host, casHost):
			httpx.SetProtocol(r.Context(), telemetry.ProtocolXet)
			cas.ServeHTTP(w, r)
		case strings.HasPrefix(p, api.ExportPath):
			httpx.SetProtocol(r.Context(), telemetry.ProtocolNative)
			exports.ServeHTTP(w, r)
		case strings.HasPrefix(p, upstream.PullThroughPath):
			httpx.SetProtocol(r.Context(), telemetry.ProtocolLFS)
			pullThrough.ServeHTTP(w, r)
		case httpx.Classify(r, viewerHost) == httpx.TargetXet:
			dispatch.ServeHTTP(w, r)
		default:
			authed.ServeHTTP(w, r)
		}
	}), nil
}
