package jobs

import (
	"regexp"
	"strings"
)

// urlPattern matches an absolute URL in error text: a scheme, "://", and
// everything up to whitespace, a quote or an angle bracket. *url.Error
// quotes its URL, so the closing quote ends the match there.
var urlPattern = regexp.MustCompile("[A-Za-z][A-Za-z0-9+.-]*://[^\\s\"'<>`]+")

// redactedQuery replaces a URL's query string and fragment.
const redactedQuery = "?redacted"

// RedactError returns err's text with the user information, query string
// and fragment of every URL in it removed. A presigned URL carries its
// signature in the query, and an Upstream or webhook URL can carry a
// password in its user information, so the worker passes every handler
// error through RedactError before it records the error on a span, logs it
// or stores it in the Job's error column, which GET /jobs/{id} returns
// (docs/spec/11-observability.md: never record presigned URLs).
func RedactError(err error) string {
	if err == nil {
		return ""
	}
	return RedactURLs(err.Error())
}

// RedactURLs removes the user information, query string and fragment of
// every absolute URL in s, keeping the scheme, host and path, and adds
// "?redacted" where a query or fragment was removed.
func RedactURLs(s string) string {
	return urlPattern.ReplaceAllStringFunc(s, redactURL)
}

func redactURL(raw string) string {
	// Punctuation that ends a sentence or a wrapping error ("...: dial tcp")
	// is not part of the URL; keep it after the redacted URL.
	trimmed := strings.TrimRight(raw, ".,;:)]}")
	suffix := raw[len(trimmed):]
	i := strings.Index(trimmed, "://")
	scheme, rest := trimmed[:i+3], trimmed[i+3:]
	end := strings.IndexAny(rest, "/?#")
	if end < 0 {
		end = len(rest)
	}
	authority, tail := rest[:end], rest[end:]
	if at := strings.LastIndexByte(authority, '@'); at >= 0 {
		authority = authority[at+1:]
	}
	if q := strings.IndexAny(tail, "?#"); q >= 0 {
		tail = tail[:q] + redactedQuery
	}
	return scheme + authority + tail + suffix
}
