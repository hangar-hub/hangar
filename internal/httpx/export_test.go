package httpx

import "time"

// SetTimeouts shortens the per-check and per-answer timeouts for tests.
func (h *Health) SetTimeouts(check, ready time.Duration) {
	h.checkTimeout, h.readyTimeout = check, ready
}
