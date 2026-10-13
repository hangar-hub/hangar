package upstream

import (
	"errors"
	"net/url"
	"strings"
)

// redactURL returns raw without its query, fragment and user information.
// Upstream redirects lead to presigned URLs whose query strings are
// credentials, so URLs never reach errors, logs or spans in full.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		if i := strings.IndexAny(raw, "?#"); i >= 0 {
			return raw[:i]
		}
		return raw
	}
	u.User = nil
	u.RawQuery = ""
	u.ForceQuery = false
	u.Fragment = ""
	u.RawFragment = ""
	return u.String()
}

// redactedURLError is a *url.Error whose URL has been redacted. It keeps
// the operation and the underlying error, so errors.Is still works for
// context cancellation and timeouts.
type redactedURLError struct {
	op  string
	url string
	err error
}

func (e *redactedURLError) Error() string {
	return e.op + " " + `"` + e.url + `"` + ": " + e.err.Error()
}

func (e *redactedURLError) Unwrap() error { return e.err }

// Timeout reports whether the underlying error is a timeout.
func (e *redactedURLError) Timeout() bool {
	t, ok := e.err.(interface{ Timeout() bool })
	return ok && t.Timeout()
}

// redactErr replaces every *url.Error in err's chain with one whose URL
// has no query string. The HTTP client returns *url.Error for transport
// failures, and after a redirect its URL is the presigned target.
func redactErr(err error) error {
	if err == nil {
		return nil
	}
	var ue *url.Error
	if !errors.As(err, &ue) {
		return err
	}
	inner := redactErr(ue.Err)
	r := &redactedURLError{op: ue.Op, url: redactURL(ue.URL), err: inner}
	if err == error(ue) {
		return r
	}
	// The *url.Error is wrapped: rebuild the message without the URL.
	msg := strings.ReplaceAll(err.Error(), ue.URL, r.url)
	return &wrappedRedacted{msg: msg, err: r}
}

type wrappedRedacted struct {
	msg string
	err error
}

func (e *wrappedRedacted) Error() string { return e.msg }
func (e *wrappedRedacted) Unwrap() error { return e.err }
