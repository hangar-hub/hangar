package hfapi_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/hfapi"
	"github.com/jamesagarside/hangar/internal/hfapi/hfapitest"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/scan"
)

// TestResolveBlocksUnscannedPickles checks that the unsafe-download gate
// fails closed: with blocking on, a file that can run code downloads only
// once its scan has found it safe.
func TestResolveBlocksUnscannedPickles(t *testing.T) {
	t.Parallel()
	e := newEnv(t, func(o *hfapitest.Options) {
		o.Deps = func(d *hfapi.Deps) {
			d.BlockUnsafe = func(context.Context) (bool, error) { return true, nil }
		}
	})
	rp, sha := seed(t, e, "alice", modelID("alice", "fresh"), model.VisibilityPublic, map[string]string{
		"weights.pkl": "cos\nsystem\n.", "config.json": configJSON,
	})
	get := func(path string) *http.Response {
		t.Helper()
		return e.Request(t, http.MethodGet, "/alice/fresh/resolve/main/"+path, "", "")
	}
	r := get("weights.pkl")
	require.Equal(t, http.StatusForbidden, r.StatusCode, "a pickle that is not scanned yet does not download")
	require.Contains(t, r.Header.Get("X-Error-Message"), "has not been scanned")
	require.Contains(t, r.Header.Get("X-Error-Message"), "accept_risk=1")
	require.Equal(t, http.StatusOK, get("config.json").StatusCode, "files that cannot run code are not held back")
	require.Equal(t, http.StatusOK, get("weights.pkl?accept_risk=1").StatusCode)

	e.Scan.SetResult("weights.pkl", scan.StatusSafe)
	_, err := e.Scan.ScanCommit(context.Background(), rp.UUID, rp.ID, sha)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, get("weights.pkl").StatusCode, "a scanned safe pickle downloads")
}
