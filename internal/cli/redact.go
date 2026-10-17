package cli

import (
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// redactedQuery replaces the query string of a URL in an error message.
const redactedQuery = "?[redacted]"

// urlInText matches an absolute URL in an error message, up to the first
// space, quote or angle bracket.
var urlInText = regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*://[^\s"'<>]+`)

// redactURL keeps the scheme, host and path of a URL and removes its user
// information, query string and fragment. Presigned URLs carry their
// signature in the query string, so what is left is safe to print.
func redactURL(raw string) string {
	scheme, rest, ok := strings.Cut(raw, "://")
	if !ok {
		return raw
	}
	suffix := ""
	if i := strings.IndexAny(rest, "?#"); i >= 0 {
		rest, suffix = rest[:i], redactedQuery
	}
	authority, path := rest, ""
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		authority, path = rest[:i], rest[i:]
	}
	if i := strings.LastIndexByte(authority, '@'); i >= 0 {
		authority = authority[i+1:]
	}
	return scheme + "://" + authority + path + suffix
}

// redactURLs applies redactURL to every URL in s.
func redactURLs(s string) string {
	return urlInText.ReplaceAllStringFunc(s, redactURL)
}

// redactedError is an HTTP client error with its URLs redacted.
type redactedError struct {
	msg string
	err error
}

func (e *redactedError) Error() string { return e.msg }
func (e *redactedError) Unwrap() error { return e.err }

// redactURLError takes an error from an HTTP client and returns one whose
// message has no user information, query strings or fragments in its URLs,
// so presigned URLs and their signatures never reach the output. The
// *url.Error itself is left out of the chain, so nothing can print its full
// URL later; the error it wraps stays, so errors.Is still finds
// context.Canceled and context.DeadlineExceeded.
func redactURLError(err error) error {
	if err == nil {
		return nil
	}
	inner := err
	var ue *url.Error
	if errors.As(err, &ue) {
		inner = ue.Err
	}
	return &redactedError{msg: redactURLs(err.Error()), err: inner}
}

// redactingDoer sends requests with an HTTP client and redacts the URLs in
// its errors. A redirect to a presigned URL puts that URL in the error, so
// every request the CLI sends goes through it.
type redactingDoer struct {
	hc *http.Client
}

// Do sends req. Like http.Client.Do, it can return a response with an error
// when a redirect fails; the response body is then already closed.
func (d redactingDoer) Do(req *http.Request) (*http.Response, error) {
	resp, err := d.hc.Do(req) // #nosec G704 -- the Instance the user chose, and URLs it hands out.
	if err != nil {
		return resp, &transportError{err: redactURLError(err)}
	}
	return resp, nil
}
