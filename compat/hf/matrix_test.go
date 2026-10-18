//go:build compat

package hf

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/compat/harness"
)

// goMatrixDir holds the outcomes of the Go tests in this package that
// cover rows of the compatibility matrix. conftest.py merges them into
// compatibility-matrix.json when pytest writes it, then removes them.
const goMatrixDir = ".matrix-go"

// goMatrixResult is one Go test's outcome, as conftest.py reads it.
type goMatrixResult struct {
	Test      string   `json:"test"`
	Endpoints []string `json:"endpoints"`
	Outcome   string   `json:"outcome"`
}

// recordMatrix registers endpoints as compatibility matrix rows that t
// covers. With HANGAR_HF_WRITE_MATRIX=1, as for pytest, it writes t's
// outcome when t ends, for conftest.py to merge. Call it once t is sure to
// run: a test that returns early without running must not count as passed.
// scripts/check-hub-matrix.py reads the calls, so pass string literals.
func recordMatrix(t *testing.T, endpoints ...string) {
	t.Helper()
	if os.Getenv("HANGAR_HF_WRITE_MATRIX") != "1" {
		return
	}
	dir := filepath.Join(harness.RepoRoot(), "compat", "hf", goMatrixDir)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	t.Cleanup(func() {
		r := goMatrixResult{Test: t.Name(), Endpoints: endpoints, Outcome: "passed"}
		switch {
		case t.Skipped():
			return
		case t.Failed():
			r.Outcome = "failed"
		}
		b, err := json.Marshal(r)
		if err != nil {
			t.Errorf("encode the matrix result: %v", err)
			return
		}
		if err := os.WriteFile(filepath.Join(dir, t.Name()+".json"), b, 0o644); err != nil {
			t.Errorf("write the matrix result: %v", err)
		}
	})
}
