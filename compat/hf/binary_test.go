//go:build compat

package hf

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/compat/harness"
	"github.com/jamesagarside/hangar/internal/hfapi/hfapitest"
)

// TestServeBinary runs the suite's Hub against the real hangar binary
// (compat/harness) instead of the Go test server: HANGAR_HF_BACKEND=binary
// in conftest.py. It seeds the users, Tokens and Organisation the suite
// expects through the native API: alice (admin scope, admin of acme), bob
// (write scope, read Member of acme), carol (read scope), and the bootstrap
// admin as admin. The Xet CAS endpoints are served, so HANGAR_HF_XET_UPLOADS=1
// runs the upload tests through hf_xet.
func TestServeBinary(t *testing.T) {
	in := harness.Start(t, harness.Options{Set: []string{
		"security.anonymous_read=true",
		"lfs.multipart_threshold=" + strconv.Itoa(MultipartThreshold),
	}})
	admin := in.AdminToken(t)
	tokens := map[string]string{"admin": admin}
	for name, scope := range map[string]string{"alice": "admin", "bob": "write", "carol": "read"} {
		tokens[name] = in.ScopedToken(t, name, in.AddUser(t, admin, name), scope)
	}
	in.MustAPI(t, http.StatusCreated, http.MethodPost, "/orgs", tokens["alice"], map[string]string{"name": hfapitest.Org})
	in.MustAPI(t, http.StatusOK, http.MethodPut, "/orgs/"+hfapitest.Org+"/members/bob", tokens["alice"], map[string]string{"role": "read"})

	info := serveInfo{Endpoint: in.URL, Tokens: tokens, Org: hfapitest.Org, Backend: "hangar binary", MultipartThreshold: MultipartThreshold}
	path := os.Getenv(EnvServeInfo)
	if path == "" {
		code, body := in.API(t, http.MethodGet, "/me", tokens["alice"], nil)
		require.Equal(t, http.StatusOK, code, string(body))
		return
	}
	b, err := json.Marshal(info)
	require.NoError(t, err)
	tmp := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".tmp")
	require.NoError(t, os.WriteFile(tmp, b, 0o600))
	require.NoError(t, os.Rename(tmp, path))
	// Serve until the parent closes standard input.
	_, _ = io.Copy(io.Discard, bufio.NewReader(os.Stdin))
}
