package cli

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAPIErrorNamesTheRequestIDOfServerErrors(t *testing.T) {
	hdr := func(kv ...string) http.Header {
		h := http.Header{}
		for i := 0; i < len(kv); i += 2 {
			h.Set(kv[i], kv[i+1])
		}
		return h
	}
	for _, tc := range []struct {
		name   string
		status int
		header http.Header
		body   string
		want   string
	}{
		{"problem with request_id", 500, hdr(), `{"detail":"Internal server error.","request_id":"r1"}`,
			"Internal server error. (HTTP 500, request ID r1)"},
		{"Hub header", 500, hdr("X-Error-Message", "Internal server error.", "X-Request-Id", "r2"), ``,
			"Internal server error. (HTTP 500, request ID r2)"},
		{"message already names it", 500, hdr("X-Request-Id", "r3"), `{"detail":"Internal server error. The request ID is r3."}`,
			"Internal server error. The request ID is r3. (HTTP 500)"},
		{"no message", 502, hdr("X-Request-Id", "r4"), `<html>bad gateway</html>`,
			"the Instance answered 502 Bad Gateway (request ID r4)"},
		{"client errors stay short", 404, hdr("X-Request-Id", "r5"), `{"detail":"Repository not found."}`,
			"Repository not found. (HTTP 404)"},
		{"no ID", 500, hdr(), `{"detail":"Internal server error."}`,
			"Internal server error. (HTTP 500)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := apiError(tc.status, tc.header, []byte(tc.body))
			require.EqualError(t, err, tc.want)
		})
	}
}
