//go:build compat

package hf

import (
	"os"
	"testing"

	"github.com/jamesagarside/hangar/compat/harness"
)

// TestMain removes the hangar binary the harness builds for this package.
func TestMain(m *testing.M) { os.Exit(harness.Main(m)) }
