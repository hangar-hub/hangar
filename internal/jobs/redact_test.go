package jobs_test

import (
	"errors"
	"fmt"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/jobs"
)

func TestRedactURLs(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"no URL here", "no URL here"},
		{"https://example.com/a/b", "https://example.com/a/b"},
		{"https://example.com", "https://example.com"},
		{`Get "https://b.s3.amazonaws.com/k?X-Amz-Signature=abc": EOF`, `Get "https://b.s3.amazonaws.com/k?redacted": EOF`},
		{"put https://b/k?X-Amz-Signature=abc: timeout", "put https://b/k?redacted: timeout"},
		{"https://user:pass@host:9000/path", "https://host:9000/path"},
		{"https://token@host?q=1", "https://host?redacted"},
		{"http://host/p#frag", "http://host/p?redacted"},
		{"see (https://host/x?sig=1).", "see (https://host/x?redacted)."},
		{"a https://u:p@h1/x?s=1 and s3://h2/y?t=2", "a https://h1/x?redacted and s3://h2/y?redacted"},
		{"<https://h/x?sig=1>", "<https://h/x?redacted>"},
	} {
		require.Equal(t, tc.want, jobs.RedactURLs(tc.in), tc.in)
	}
}

func TestRedactError(t *testing.T) {
	require.Empty(t, jobs.RedactError(nil))
	err := fmt.Errorf("pull file: %w", &url.Error{Op: "Get", URL: "https://x:y@cdn.example/f?Expires=1&Signature=s", Err: errors.New("unexpected EOF")})
	require.Equal(t, `pull file: Get "https://cdn.example/f?redacted": unexpected EOF`, jobs.RedactError(err))
}
