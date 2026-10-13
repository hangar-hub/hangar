package secrets_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/secrets"
)

func TestSealOpen(t *testing.T) {
	s, err := secrets.New(bytes.Repeat([]byte{1}, 32))
	require.NoError(t, err)
	c, err := s.Seal("hf_secret_token", "upstream_token")
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(c, "v1:"))
	require.NotContains(t, c, "hf_secret_token")
	c2, err := s.Seal("hf_secret_token", "upstream_token")
	require.NoError(t, err)
	require.NotEqual(t, c, c2, "random nonces")

	p, err := s.Open(c, "upstream_token")
	require.NoError(t, err)
	require.Equal(t, "hf_secret_token", p)

	_, err = s.Open(c, "webhook_secret")
	require.ErrorIs(t, err, secrets.ErrDecrypt, "purpose is bound")
	other, err := secrets.New(bytes.Repeat([]byte{2}, 32))
	require.NoError(t, err)
	_, err = other.Open(c, "upstream_token")
	require.ErrorIs(t, err, secrets.ErrDecrypt)
	for _, bad := range []string{"", "v2:abc", "v1:!!!", "v1:YQ"} {
		_, err = s.Open(bad, "x")
		require.ErrorIs(t, err, secrets.ErrDecrypt, bad)
	}
	_, err = secrets.New([]byte("short"))
	require.Error(t, err)
}
