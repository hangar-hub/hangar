package jobs

import (
	"regexp"
	"strings"
)

// schemePattern matches the start of an absolute URL in error text: a
// scheme followed by "://". redactURLAt then finds where that URL ends.
var schemePattern = regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*://`)

// redactedQuery replaces a URL's query string and fragment.
const redactedQuery = "?redacted"

// closers maps a character that can open a quoted or bracketed URL to the
// character that closes it.
var closers = map[byte]byte{'"': '"', '\'': '\'', '`': '`', '<': '>', '(': ')', '[': ']', '{': '}'}

// trailing is punctuation that ends a sentence or a wrapping error ("...:
// dial tcp") rather than belonging to an unquoted URL.
const trailing = ".,;:!?)]}>\"'`"

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
//
// A URL ends at whitespace, so a quote, angle bracket or other punctuation
// inside a query string cannot end the match early and leave the rest of a
// signature in the text. When the URL follows an opening quote or bracket,
// as *url.Error quotes its URL, it ends at the last matching closer before
// that whitespace instead.
func RedactURLs(s string) string {
	var b strings.Builder
	last := 0
	for last < len(s) {
		loc := schemePattern.FindStringIndex(s[last:])
		if loc == nil {
			break
		}
		start := last + loc[0]
		end := urlEnd(s, start, last+loc[1])
		b.WriteString(s[last:start])
		b.WriteString(redactURL(s[start:end]))
		last = end
	}
	b.WriteString(s[last:])
	return b.String()
}

// urlEnd returns the index just past the URL that starts at start, whose
// "://" ends at body.
func urlEnd(s string, start, body int) int {
	token := body
	for token < len(s) && !isSpace(s[token]) {
		token++
	}
	if start == 0 {
		return trimTrailing(s, body, token)
	}
	closer, ok := closers[s[start-1]]
	if !ok {
		return trimTrailing(s, body, token)
	}
	// The last closer before whitespace ends the URL, so a closer inside
	// the query is redacted with it. Ending too late only redacts more.
	if i := strings.LastIndexByte(s[body:token], closer); i >= 0 {
		return body + i
	}
	if closer == '"' {
		// A Go-quoted string (%q, as *url.Error writes it) can hold a
		// URL with a space in it; it ends at the next unescaped quote on
		// the line.
		for i := token; i < len(s) && s[i] != '\n'; i++ {
			switch s[i] {
			case '\\':
				i++
			case '"':
				return i
			}
		}
	}
	return trimTrailing(s, body, token)
}

// trimTrailing drops sentence punctuation from the end of s[body:end].
func trimTrailing(s string, body, end int) int {
	for end > body && strings.IndexByte(trailing, s[end-1]) >= 0 {
		end--
	}
	return end
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\v' || c == '\f'
}

func redactURL(raw string) string {
	i := strings.Index(raw, "://")
	scheme, rest := raw[:i+3], raw[i+3:]
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
	return scheme + authority + tail
}
