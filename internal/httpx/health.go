package httpx

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// checkTimeout bounds one run of a readiness check.
const checkTimeout = 5 * time.Second

// Check is one readiness check. It returns nil when healthy.
type Check struct {
	Name string
	Run  func(ctx context.Context) error
	// CacheFor reuses a result for this long, as the Blob store sentinel
	// check does (every 10 seconds). Zero runs the check every time.
	CacheFor time.Duration
}

// Health serves /healthz and /readyz (docs/spec/11-observability.md "Health").
type Health struct {
	checks   []Check
	draining atomic.Bool
	log      atomic.Pointer[slog.Logger]

	mu       sync.Mutex
	cache    map[string]cached
	inflight map[string]*flight
	now      func() time.Time
	timeout  time.Duration
}

type cached struct {
	err error
	at  time.Time
}

// flight is one run of a cached check that probes share. done is closed
// once err is set.
type flight struct {
	done chan struct{}
	err  error
}

// NewHealth returns a Health with the given readiness checks. Failed
// checks are logged to slog.Default until SetLogger names another logger.
func NewHealth(checks ...Check) *Health {
	return &Health{checks: checks, cache: map[string]cached{}, inflight: map[string]*flight{}, now: time.Now, timeout: checkTimeout}
}

// SetLogger sets the logger that failed readiness checks are written to.
func (h *Health) SetLogger(log *slog.Logger) { h.log.Store(log) }

func (h *Health) logger() *slog.Logger {
	if l := h.log.Load(); l != nil {
		return l
	}
	return slog.Default()
}

// SetDraining marks the process as shutting down: /healthz and /readyz
// both answer 503 so load balancers stop sending traffic.
func (h *Health) SetDraining() { h.draining.Store(true) }

// CheckResult is one failed check in the /readyz body.
type CheckResult struct {
	Name  string `json:"name"`
	Error string `json:"error"`
}

func (h *Health) run(ctx context.Context, c Check) error {
	if c.CacheFor <= 0 {
		ctx, cancel := context.WithTimeout(ctx, h.timeout)
		defer cancel()
		return c.Run(ctx)
	}
	return h.runCached(ctx, c)
}

// runCached answers from the cache while the last result is fresh.
// Otherwise it starts one run of the check, or joins the run already in
// progress, and waits for it or for ctx, whichever ends first. The run
// does not use ctx: a probe that gives up (kubelet waits 1 second) leaves
// the run to finish on its own timeout and cache a result for the next
// probe, instead of caching the probe's own context error. A run that
// hits its own timeout is not cached either, so the next probe tries again.
func (h *Health) runCached(ctx context.Context, c Check) error {
	h.mu.Lock()
	if r, ok := h.cache[c.Name]; ok && h.now().Sub(r.at) < c.CacheFor {
		h.mu.Unlock()
		return r.err
	}
	f, ok := h.inflight[c.Name]
	if !ok {
		f = &flight{done: make(chan struct{})}
		h.inflight[c.Name] = f
		go h.fly(context.WithoutCancel(ctx), c, f)
	}
	h.mu.Unlock()

	select {
	case <-f.done:
		return f.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (h *Health) fly(ctx context.Context, c Check, f *flight) {
	ctx, cancel := context.WithTimeout(ctx, h.timeout)
	defer cancel()
	err := c.Run(ctx)

	h.mu.Lock()
	if err == nil || ctx.Err() == nil {
		h.cache[c.Name] = cached{err: err, at: h.now()}
	}
	delete(h.inflight, c.Name)
	f.err = err
	close(f.done)
	h.mu.Unlock()
}

// Failures runs every check and returns the failures, logging each one.
func (h *Health) Failures(ctx context.Context) []CheckResult {
	failed := []CheckResult{}
	for _, c := range h.checks {
		if err := h.run(ctx, c); err != nil {
			failed = append(failed, CheckResult{Name: c.Name, Error: err.Error()})
			h.logger().WarnContext(ctx, "readiness check failed", "check", c.Name, "error", err.Error())
		}
	}
	return failed
}

// ServeHTTP implements http.Handler for /healthz and /readyz.
func (h *Health) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	SetRoute(r.Context(), r.URL.Path)
	w.Header().Set("Cache-Control", "no-store")
	switch r.URL.Path {
	case "/healthz":
		if h.draining.Load() {
			WriteJSON(w, r, http.StatusServiceUnavailable, map[string]string{"status": "shutting down"})
			return
		}
		WriteJSON(w, r, http.StatusOK, map[string]string{"status": "ok"})
	case "/readyz":
		if h.draining.Load() {
			WriteJSON(w, r, http.StatusServiceUnavailable, map[string]any{"status": "shutting down", "failed": []CheckResult{{Name: "shutdown", Error: "the process is shutting down"}}})
			return
		}
		if failed := h.Failures(r.Context()); len(failed) > 0 {
			WriteJSON(w, r, http.StatusServiceUnavailable, map[string]any{"status": "not ready", "failed": failed})
			return
		}
		WriteJSON(w, r, http.StatusOK, map[string]string{"status": "ready"})
	default:
		WriteError(w, r, http.StatusNotFound, "", "Not found.")
	}
}
