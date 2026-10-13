package httpx

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

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

	mu    sync.Mutex
	cache map[string]cached
	now   func() time.Time
}

type cached struct {
	err error
	at  time.Time
}

// NewHealth returns a Health with the given readiness checks.
func NewHealth(checks ...Check) *Health {
	return &Health{checks: checks, cache: map[string]cached{}, now: time.Now}
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
	if c.CacheFor > 0 {
		h.mu.Lock()
		if r, ok := h.cache[c.Name]; ok && h.now().Sub(r.at) < c.CacheFor {
			h.mu.Unlock()
			return r.err
		}
		h.mu.Unlock()
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	err := c.Run(ctx)
	if c.CacheFor > 0 {
		h.mu.Lock()
		h.cache[c.Name] = cached{err: err, at: h.now()}
		h.mu.Unlock()
	}
	return err
}

// Failures runs every check and returns the failures.
func (h *Health) Failures(ctx context.Context) []CheckResult {
	failed := []CheckResult{}
	for _, c := range h.checks {
		if err := h.run(ctx, c); err != nil {
			failed = append(failed, CheckResult{Name: c.Name, Error: err.Error()})
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
