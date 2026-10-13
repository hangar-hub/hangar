package xet_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/xet"
)

func TestValidHash(t *testing.T) {
	require.True(t, xet.ValidHash(strings.Repeat("0f", 32)))
	require.False(t, xet.ValidHash(strings.Repeat("0F", 32)))
	require.False(t, xet.ValidHash("abc"))
	require.Contains(t, xet.RejectMessage, "HF_HUB_DISABLE_XET=1")
}
