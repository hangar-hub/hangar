package cli

import (
	"context"
	"errors"
	"math/rand/v2"
	"net/http"
	"time"
)

// transferAttempts is how many times the CLI sends one part upload or one
// file download before it gives up. A home link that drops a connection
// now and then costs a resend of one part, not the whole command.
const transferAttempts = 6

// maxTransferBackoff caps the wait between two attempts.
const maxTransferBackoff = 30 * time.Second

// transportError is a request that failed before an answer arrived, or
// whose answer broke off: a connection that failed, timed out or was reset.
// The request may succeed if it is sent again.
type transportError struct{ err error }

func (e *transportError) Error() string { return e.err.Error() }
func (e *transportError) Unwrap() error { return e.err }

// transferBackoff returns the wait before attempt+1: exponential from one
// second, capped at maxTransferBackoff, with jitter so parallel parts that
// failed together do not retry together.
func transferBackoff(attempt int) time.Duration {
	d := min(time.Second<<min(max(attempt-1, 0), 6), maxTransferBackoff)
	return d/2 + rand.N(d/2+1) //nolint:gosec // jitter, not a secret
}

// transient reports whether a failed transfer may succeed if it is sent
// again: the connection failed or broke off, or the server or a proxy
// answered 408, 425, 429, 500, 502, 503 or 504. A cancelled command is
// never transient.
func transient(ctx context.Context, err error) bool {
	if err == nil || ctx.Err() != nil || errors.Is(err, context.Canceled) {
		return false
	}
	var ae *APIError
	if errors.As(err, &ae) {
		switch ae.Status {
		case http.StatusRequestTimeout, http.StatusTooEarly, http.StatusTooManyRequests, http.StatusInternalServerError,
			http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			return true
		}
		return false
	}
	var te *transportError
	return errors.As(err, &te)
}

// retryWait is how long to wait before sending a failed transfer again:
// what a 429 or 503 asked for in Retry-After, or transferBackoff.
func retryWait(err error, attempt int) time.Duration {
	var ae *APIError
	if errors.As(err, &ae) && ae.RetryAfter > 0 {
		return ae.RetryAfter
	}
	return transferBackoff(attempt)
}
