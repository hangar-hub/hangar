package httpx

import "time"

// SetCheckTimeout shortens how long one run of a readiness check may take.
func SetCheckTimeout(h *Health, d time.Duration) { h.timeout = d }
