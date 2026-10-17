package httpx

import (
	"context"
	"errors"
	"net/http"
)

// StatusClientClosedRequest is the status recorded when the client went
// away before the answer (499, as nginx uses), so a cancelled request is not
// counted or logged as a server error.
const StatusClientClosedRequest = 499

// ClientClosedMessage is the message written with StatusClientClosedRequest.
// Nobody reads it; it is there for access logs and metrics.
const ClientClosedMessage = "The client closed the request."

// ClientGone reports whether err is the result of the client closing the
// request: the request context ended and the error says so. An error that
// wraps context.Canceled while the client is still connected (for example
// a server-side timeout of its own) is not the client's doing.
func ClientGone(r *http.Request, err error) bool {
	return r.Context().Err() != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded))
}
