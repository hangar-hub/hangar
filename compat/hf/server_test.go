//go:build compat

// Package hf holds the Hub client compatibility suite (compat/hf, pytest)
// and the Go test server it runs against until the integration lead wires
// internal/hfapi into cmd/hangar. The server is the Hub API from
// internal/hfapi over the fakes in internal/hfapi/hfapitest, or over the
// real repo.Service when built with the w2real tag (see README.md).
package hf

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/hfapi/hfapitest"
)

// EnvServeInfo names the file TestServeHub writes the endpoint and Tokens
// to. When it is set, TestServeHub serves until its standard input closes.
const EnvServeInfo = "HANGAR_HF_SERVE_INFO"

// MultipartThreshold is the lowered `lfs.multipart_threshold` the test
// server uses, the smallest the setting allows (one S3 part), so
// test_upload_over_multipart_threshold reaches huggingface_hub's multipart
// path with a small file.
const MultipartThreshold = 5 << 20

// serveInfo is what the pytest suite reads to reach the server.
type serveInfo struct {
	Endpoint string            `json:"endpoint"`
	Tokens   map[string]string `json:"tokens"`
	Org      string            `json:"org"`
	Backend  string            `json:"backend"`
	// MultipartThreshold is the size above which LFS uploads are multipart.
	MultipartThreshold int64 `json:"multipart_threshold"`
}

func startHub(t *testing.T) (*hfapitest.Env, serveInfo) {
	t.Helper()
	opts := hfapitest.Options{LFSMultipartThreshold: MultipartThreshold}
	backend := repoBackend(t, &opts)
	e := hfapitest.New(t, opts)
	info := serveInfo{
		Endpoint: e.URL, Tokens: map[string]string{}, Org: hfapitest.Org, Backend: backend,
		MultipartThreshold: MultipartThreshold,
	}
	for name, u := range e.Users {
		info.Tokens[name] = u.Token
	}
	return e, info
}

// TestServeHub starts the Hub API test server. Without HANGAR_HF_SERVE_INFO
// it checks the server answers and stops; with it, pytest drives it.
func TestServeHub(t *testing.T) {
	e, info := startHub(t)
	path := os.Getenv(EnvServeInfo)
	if path == "" {
		req, err := http.NewRequest(http.MethodGet, e.URL+"/api/whoami-v2", nil)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+info.Tokens["alice"])
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		require.Equal(t, http.StatusOK, resp.StatusCode)
		var who map[string]any
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&who))
		require.Equal(t, "alice", who["name"])
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
