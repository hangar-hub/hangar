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
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/hangar-hub/hangar/internal/api"
	"github.com/hangar-hub/hangar/internal/api/gen"
	"github.com/hangar-hub/hangar/internal/auth"
	"github.com/hangar-hub/hangar/internal/blob"
	"github.com/hangar-hub/hangar/internal/config"
	"github.com/hangar-hub/hangar/internal/db"
	"github.com/hangar-hub/hangar/internal/httpx"
	"github.com/hangar-hub/hangar/internal/jobs"
	"github.com/hangar-hub/hangar/internal/ollama"
	"github.com/hangar-hub/hangar/internal/repo"
	"github.com/hangar-hub/hangar/internal/telemetry"
	"github.com/hangar-hub/hangar/internal/upstream"
	"github.com/hangar-hub/hangar/internal/web"
	"github.com/hangar-hub/hangar/internal/xet"
)

// newHealth builds the readiness checks: database reachable, migrations at
// the expected version, and the Blob store sentinel readable, followed by
// any extra checks, such as the start-up check that serve adds.
func newHealth(d *db.DB, blobs blob.Store, extra ...httpx.Check) *httpx.Health {
	return newHealthFor(d, func() blob.Store { return blobs }, extra...)
}

// newHealthFor is newHealth for a Blob store that may not be open yet:
// until blobs returns one, the blob_store check fails.
func newHealthFor(d *db.DB, blobs func() blob.Store, extra ...httpx.Check) *httpx.Health {
	checks := []httpx.Check{
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
			s := blobs()
			if s == nil {
				// Not open yet. The startup check keeps /readyz at 503 and
				// says why; failing here instead would cache the failure
				// for CacheFor after the store opens.
				return nil
			}
			_, err := s.Stat(ctx, blob.SentinelKey)
			return err
		}},
	}
	return httpx.NewHealth(append(checks, extra...)...)
}

// startupCheck fails until started is set, which serve does once the full
// handler is installed, so /readyz answers 503 while the Instance is still
// being built and no load balancer sends API traffic to the health-only
// handler.
func startupCheck(started *atomic.Bool) httpx.Check {
	return startupCheckWaiting(started, nil)
}

// startupCheckWaiting is startupCheck that also names what start-up is
// waiting for while waiting holds a reason, such as a Blob store that does
// not answer yet.
func startupCheckWaiting(started *atomic.Bool, waiting *atomic.Pointer[string]) httpx.Check {
	return httpx.Check{Name: "startup", Run: func(context.Context) error {
		if started.Load() {
			return nil
		}
		if waiting != nil {
			if w := waiting.Load(); w != nil {
				return fmt.Errorf("the server is still starting: %s", *w)
			}
		}
		return errors.New("the server is still starting")
	}}
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
		UI:     web.Handler(web.Options{ConnectSrc: in.presignOrigins(ctx), DocsURL: cfg.UI.DocsURL}),
	}
	if in.viewer != nil {
		protocols.Viewer = in.viewer.Handler()
	}
	dispatch := httpx.Dispatch(protocols, cfg.Server.PublicURL)
	// The Ollama token endpoint takes an Ollama key signature, which the
	// Ollama handler verifies; WriteAuthError classifies the protocol first,
	// so bad credentials get each client's own challenge. A stale session
	// cookie makes the request anonymous, and Middleware clears it.
	authed := in.auth.Middleware(ollama.ExemptTokenEndpoint(in.auth), auth.WriteAuthError(cfg.Server.PublicURL))(dispatch)

	direct := directRoutes(in)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if casHost != "" && strings.EqualFold(r.Host, casHost) {
			httpx.SetProtocol(r.Context(), telemetry.ProtocolXet)
			cas.ServeHTTP(w, r)
			return
		}
		p := r.URL.EscapedPath()
		for _, d := range direct {
			if strings.HasPrefix(p, d.Prefix) {
				httpx.SetProtocol(r.Context(), d.Protocol)
				httpx.SetRoute(r.Context(), d.Route)
				d.Handler.ServeHTTP(w, r)
				return
			}
		}
		if httpx.Classify(r, viewerHost) == httpx.TargetXet {
			dispatch.ServeHTTP(w, r)
			return
		}
		authed.ServeHTTP(w, r)
	}), nil
}

// directRoute is a path prefix served before authentication and protocol
// dispatch, because its URLs carry their own credential: a signature.
type directRoute struct {
	Prefix string
	// Route is the route template for the access log and the span name.
	Route    string
	Protocol telemetry.Protocol
	Handler  http.Handler
}

// directRoutes returns the signed-link routes: zip exports and git-lfs
// Pull-through downloads. Both serve file bytes, so both check
// scan.block_unsafe_downloads when the link is used.
func directRoutes(in *instance) []directRoute {
	return []directRoute{
		{Prefix: api.ExportPath, Route: api.ExportPath + "{file}", Protocol: telemetry.ProtocolNative,
			Handler: api.ExportHandler(in.blobs, in.signer, exportCheck(in))},
		{Prefix: upstream.PullThroughPath, Route: upstream.PullThroughPath + "{repo}/{oid}", Protocol: telemetry.ProtocolLFS,
			Handler: in.pulls.PullThrough().Handler()},
	}
}

// exportCheck runs the unsafe-download gate again when a signed zip export
// link is used, because a link issued before an admin turned blocking on,
// or before the scan finished, must not serve a refused file. It finds the
// zip.export Job from the export's key and checks the folder of the Commit
// it exported, as StartZipExport did.
func exportCheck(in *instance) api.ExportCheck {
	return func(ctx context.Context, key string) error {
		if on, err := in.unsafeGate.Block(ctx); err != nil || !on {
			return err
		}
		name, ok := strings.CutPrefix(key, blob.PrefixExports)
		if !ok {
			return fmt.Errorf("export key %q is outside %s", key, blob.PrefixExports)
		}
		jobID, err := uuid.Parse(strings.TrimSuffix(name, ".zip"))
		if err != nil || repo.ExportKey(jobID) != key {
			return fmt.Errorf("export key %q does not name a zip.export Job", key)
		}
		j, err := in.queue.Get(ctx, jobID)
		if err != nil {
			return fmt.Errorf("zip.export Job of %s: %w", key, err)
		}
		var pl repo.ZipJob
		if j.Kind != jobs.KindZipExport {
			return fmt.Errorf("job %s is %s, not %s", jobID, j.Kind, jobs.KindZipExport)
		}
		if err := j.DecodePayload(&pl); err != nil {
			return err
		}
		r, err := in.repos.GetByUUID(ctx, pl.RepoUUID)
		if err != nil {
			return err
		}
		return in.unsafeGate.CheckTree(ctx, r.UUID, r.ID, pl.Commit, pl.Path)
	}
}
