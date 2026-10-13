package webhook_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/webhook"
)

func TestSignAndVerify(t *testing.T) {
	secret := []byte("s3cret")
	body := []byte(`{"event":"repo.created"}`)
	sig := webhook.Sign(secret, body)
	require.Regexp(t, `^sha256=[0-9a-f]{64}$`, sig)
	require.True(t, webhook.Verify(secret, body, sig))
	require.False(t, webhook.Verify([]byte("other"), body, sig))
	require.False(t, webhook.Verify(secret, []byte("{}"), sig))
	// Known value from: printf '%s' '{"event":"repo.created"}' | openssl dgst -sha256 -hmac s3cret
	require.Equal(t, "sha256=4ade32eb32bfb30749cba20bcd8b36e15b302dba429c22c25090fa67834c1c6e", sig)
	require.Len(t, webhook.AllEvents(), 16)
	require.Len(t, webhook.RetrySchedule, 5)
}
