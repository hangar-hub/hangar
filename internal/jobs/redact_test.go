package jobs_test

import (
	"errors"
	"fmt"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/jobs"
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
		// A quote or angle bracket inside a query string must not end the
		// URL early and leave the rest of the signature behind.
		{
			"get https://bucket.s3.amazonaws.com/k?X-Amz-Credential=AKIA'SECRET&X-Amz-Signature=deadbeef: 403",
			"get https://bucket.s3.amazonaws.com/k?redacted: 403",
		},
		{"https://h/k?sig=abc<def>ghi&X-Amz-Signature=deadbeef", "https://h/k?redacted"},
		{`Put "https://h/k?a=\"b&X-Amz-Signature=deadbeef": EOF`, `Put "https://h/k?redacted": EOF`},
		{"put 'https://h/k?a='b&sig=deadbeef' failed", "put 'https://h/k?redacted' failed"},
		{"see <https://h/k?a=<b>&sig=deadbeef>.", "see <https://h/k?redacted>."},
		{"(https://h/k?a=(b)&sig=deadbeef)", "(https://h/k?redacted)"},
		{"`https://h/k?a=`b&sig=deadbeef`", "`https://h/k?redacted`"},
		{`Get "https://h/a b?sig=deadbeef": EOF`, `Get "https://h/a b?redacted": EOF`},
		{`"https://user:pa\"ss@host/p": EOF`, `"https://host/p": EOF`},
		{"is it at https://h/x?", "is it at https://h/x?"},
		{"line one https://h/x?sig=deadbeef\nline two", "line one https://h/x?redacted\nline two"},
	} {
		require.Equal(t, tc.want, jobs.RedactURLs(tc.in), tc.in)
	}
}

// TestRedactURLsNeverLeaksTheQuery puts every punctuation character at the
// start of a query value, with the URL bare, quoted by *url.Error and
// bracketed, and checks no part of the signature after it survives.
func TestRedactURLsNeverLeaksTheQuery(t *testing.T) {
	const secret = "SECRETSIG"
	for c := byte('!'); c <= '~'; c++ {
		if c >= '0' && c <= '9' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' {
			continue
		}
		u := "https://bucket.s3.amazonaws.com/k?X-Amz-Credential=AKIA" + string(c) + "x&X-Amz-Signature=" + secret
		for _, text := range []string{
			"put " + u + ": 403",
			u,
			(&url.Error{Op: "Put", URL: u, Err: errors.New("EOF")}).Error(),
			"<" + u + ">",
			"(" + u + ").",
			"'" + u + "'",
			"[" + u + "]",
		} {
			got := jobs.RedactURLs(text)
			require.NotContains(t, got, secret, "%q became %q", text, got)
			require.Contains(t, got, "https://bucket.s3.amazonaws.com/k?redacted", text)
		}
	}
}

func TestRedactError(t *testing.T) {
	require.Empty(t, jobs.RedactError(nil))
	err := fmt.Errorf("pull file: %w", &url.Error{Op: "Get", URL: "https://x:y@cdn.example/f?Expires=1&Signature=s", Err: errors.New("unexpected EOF")})
	require.Equal(t, `pull file: Get "https://cdn.example/f?redacted": unexpected EOF`, jobs.RedactError(err))
}
