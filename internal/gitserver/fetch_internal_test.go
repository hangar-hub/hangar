package gitserver

import (
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestFetchRequest checks which upload-pack requests the unsafe-download
// gate checks: every protocol v0 request and a v2 fetch send a pack; a v2
// ls-refs only lists refs. The whole body is passed on either way.
func TestFetchRequest(t *testing.T) {
	cases := map[string]bool{
		"0032want 0123456789012345678901234567890123456789\n00000009done\n": true,
		"0012command=fetch\n0001000ddeepen 1\n0000":                         true,
		"0014command=ls-refs\n0000":                                         false,
		"":                                                                  true,
	}
	for body, want := range cases {
		in, fetch := fetchRequest(strings.NewReader(body))
		require.Equal(t, want, fetch, "%q", body)
		got, err := io.ReadAll(in)
		require.NoError(t, err)
		require.Equal(t, body, string(got))
	}
}
