package scan_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/scan"
)

func TestWorst(t *testing.T) {
	require.Equal(t, scan.StatusUnsafe, scan.Worst(scan.StatusSafe, scan.StatusUnsafe))
	require.Equal(t, scan.StatusUnsafe, scan.Worst(scan.StatusUnsafe, scan.StatusSuspicious))
	require.Equal(t, scan.StatusSuspicious, scan.Worst(scan.StatusError, scan.StatusSuspicious))
	require.Equal(t, scan.StatusSafe, scan.Worst("", scan.StatusSafe))
}
