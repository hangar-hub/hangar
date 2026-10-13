package httpx

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Segments splits the escaped request path on '/' and unescapes each
// segment, so a revision such as `refs%2Fpr%2F1` stays one segment
// (docs/spec/03-hf-api.md, rule 1). The leading slash gives no segment.
func Segments(r *http.Request) ([]string, error) {
	return SplitEscapedPath(r.URL.EscapedPath())
}

// SplitEscapedPath is Segments for an escaped path string.
func SplitEscapedPath(escaped string) ([]string, error) {
	escaped = strings.TrimPrefix(escaped, "/")
	if escaped == "" {
		return nil, nil
	}
	raw := strings.Split(escaped, "/")
	out := make([]string, len(raw))
	for i, s := range raw {
		u, err := url.PathUnescape(s)
		if err != nil {
			return nil, err
		}
		out[i] = u
	}
	return out, nil
}

// ParseBool parses the booleans Hub clients send: true, false, True, False,
// 1 and 0, in any case. ok is false for anything else.
func ParseBool(s string) (v, ok bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "true", "1":
		return true, true
	case "false", "0":
		return false, true
	default:
		return false, false
	}
}

// QueryBool reads a boolean query parameter, returning def when it is
// absent or malformed.
func QueryBool(q url.Values, key string, def bool) bool {
	if !q.Has(key) {
		return def
	}
	v, ok := ParseBool(q.Get(key))
	if !ok {
		return def
	}
	return v
}

// QueryList returns every value of key, accepting both `key=a&key=b` and
// `key[]=a`, and comma-separated values.
func QueryList(q url.Values, key string) []string {
	var out []string
	for _, k := range []string{key, key + "[]"} {
		for _, v := range q[k] {
			for _, part := range strings.Split(v, ",") {
				if part = strings.TrimSpace(part); part != "" {
					out = append(out, part)
				}
			}
		}
	}
	return out
}

// QueryInt reads an integer query parameter, clamped to [min, max], with def
// when absent or malformed.
func QueryInt(q url.Values, key string, def, minimum, maximum int) int {
	v, err := strconv.Atoi(q.Get(key))
	if err != nil {
		return def
	}
	return max(minimum, min(maximum, v))
}

// AbsoluteURL joins Hangar's public URL with an escaped path and query.
// Link headers must be absolute and on server.public_url, because the Hub
// client follows them verbatim (docs/research/hf-hub-api-contract.md §3.1).
func AbsoluteURL(publicURL, escapedPath string, q url.Values) string {
	u := strings.TrimRight(publicURL, "/") + "/" + strings.TrimLeft(escapedPath, "/")
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	return u
}

// SetLinkNext sets `Link: <url>; rel="next"`.
func SetLinkNext(w http.ResponseWriter, absoluteURL string) {
	w.Header().Set("Link", "<"+absoluteURL+">; rel=\"next\"")
}
