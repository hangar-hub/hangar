package upstream

import (
	"testing"
	"time"
)

// SetTestTimeout changes the connection test bound for one test.
func SetTestTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	old := testTimeout
	testTimeout = d
	t.Cleanup(func() { testTimeout = old })
}
