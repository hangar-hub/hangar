//go:build compat

package lmstudio

import (
	"os"
	"testing"

	"github.com/hangar-hub/hangar/compat/harness"
)

// TestMain removes the hangar binary the harness builds for this package.
func TestMain(m *testing.M) { os.Exit(harness.Main(m)) }
