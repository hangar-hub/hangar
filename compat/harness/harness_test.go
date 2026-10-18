//go:build compat

package harness_test

import (
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/compat/harness"
)

func TestStartServesHealthAndStops(t *testing.T) {
	inst := harness.Start(t, harness.Options{})
	resp, err := http.Get(inst.URL + "/healthz")
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.JSONEq(t, `{"status":"ok"}`, string(body))
	require.Contains(t, inst.Logs(), "hangar started")

	require.NoError(t, inst.Stop(30*time.Second))
	_, err = http.Get(inst.URL + "/healthz")
	require.Error(t, err, "the port is closed after Stop")
}
