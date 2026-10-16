package httpx

import "time"

// SetCheckTimeout shortens how long one run of a readiness check may take.
func SetCheckTimeout(h *Health, d time.Duration) { h.checkTimeout = d }

// SetTimeouts shortens the per-check and per-answer timeouts for tests.
func (h *Health) SetTimeouts(check, ready time.Duration) {
	h.checkTimeout, h.readyTimeout = check, ready
}
