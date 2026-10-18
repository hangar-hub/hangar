//go:build compat && !w2real

package hf

import (
	"testing"

	"github.com/hangar-hub/hangar/internal/hfapi/hfapitest"
)

// repoBackend leaves the Repo service as hfapitest's real-git fake.
func repoBackend(_ *testing.T, _ *hfapitest.Options) string { return "repotest.Fake" }
