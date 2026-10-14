package cli

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/cli/clitest"
	"github.com/jamesagarside/hangar/internal/model"
)

// signature is the secret part of the presigned URLs in these tests.
const signature = "X-Amz-Signature=5ec4e7"

func TestRedactURLs(t *testing.T) {
	for in, want := range map[string]string{
		`Put "https://minio:9000/bucket/key?X-Amz-Credential=a&` + signature + `": EOF`: `Put "https://minio:9000/bucket/key?[redacted]": EOF`,
		`Get "https://user:secret@example.com/a/b#frag": refused`:                       `Get "https://example.com/a/b?[redacted]": refused`,
		`failed to parse Location header "http://h/x?sig=1": bad`:                       `failed to parse Location header "http://h/x?[redacted]": bad`,
		"reach hangar.example.com: dial tcp 10.0.0.1:443: i/o timeout":                  "reach hangar.example.com: dial tcp 10.0.0.1:443: i/o timeout",
		"http://a@b?c then https://d/e":                                                 "http://b?[redacted] then https://d/e",
		"no scheme ://x?y":                                                              "no scheme ://x?y",
	} {
		require.Equal(t, want, redactURLs(in), in)
	}
	require.NoError(t, redactURLError(nil))
}

// closedURL returns the URL of a server that is no longer listening.
func closedURL(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()
	return srv.URL
}

func TestRedactingDoerKeepsTheErrorChain(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/o?"+signature, nil)
	require.NoError(t, err)
	resp, err := redactingDoer{newHTTPClient()}.Do(req)
	if resp != nil {
		_ = resp.Body.Close()
	}
	require.ErrorIs(t, err, context.Canceled)
	require.NotContains(t, err.Error(), signature)
	require.Contains(t, err.Error(), "/o?[redacted]")
}

// requireNoSignature checks a failed command printed no presigned URL.
func requireNoSignature(t *testing.T, r result) {
	t.Helper()
	require.Error(t, r.err)
	for _, s := range []string{r.err.Error(), r.stdout, r.stderr} {
		require.NotContains(t, s, "sig=fake")
	}
	require.Contains(t, r.err.Error(), "?[redacted]")
}

func TestPushDoesNotPrintPresignedPartURLs(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.inst.Presign = true
	e.inst.PresignBase = closedURL(t)
	dir := filepath.Join(e.home, "model")
	writeTree(t, dir, map[string][]byte{"model.safetensors": blobOf("weights", 100<<10)})

	r := e.run("push", dir, "acme/leak", "--create")
	require.Equal(t, ExitFailure, r.code)
	requireNoSignature(t, r)
	require.Contains(t, r.err.Error(), "send the part")
	require.Contains(t, r.err.Error(), "/presigned/")
}

func TestPullFollowsPresignedDownloadRedirects(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.inst.RedirectDownloads = true
	id := mustID(t, model.RepoTypeModel, "acme/redirected")
	weights := blobOf("redirect", 100<<10)
	e.inst.AddRepo(id, clitest.File{Path: "model.safetensors", Content: weights}, clitest.File{Path: "README.md", Content: []byte("# R\n")})

	to := filepath.Join(e.home, "out")
	e.ok("pull", "acme/redirected", "--to", to)
	got, err := os.ReadFile(filepath.Join(to, "model.safetensors"))
	require.NoError(t, err)
	require.Equal(t, weights, got)
	require.Equal(t, 1, e.inst.RequestCount("GET /presigned-objects/{sha}"), "the LFS file came from the presigned URL")
}

func TestPullDoesNotPrintPresignedDownloadURLs(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.inst.RedirectDownloads = true
	e.inst.PresignBase = closedURL(t)
	id := mustID(t, model.RepoTypeModel, "acme/redirected")
	e.inst.AddRepo(id, clitest.File{Path: "model.safetensors", Content: blobOf("redirect", 100<<10)})

	r := e.run("pull", "acme/redirected", "--to", filepath.Join(e.home, "out"))
	require.Equal(t, ExitFailure, r.code)
	requireNoSignature(t, r)
	require.True(t, strings.HasPrefix(r.err.Error(), "model.safetensors") || strings.Contains(r.err.Error(), "reach "), r.err.Error())
	require.False(t, errors.Is(r.err, context.Canceled))
}

func TestCheckRedirectDropsTheTokenAcrossOrigins(t *testing.T) {
	first, err := http.NewRequest(http.MethodGet, "https://hangar.example.com/acme/m/resolve/main/w.gguf", nil)
	require.NoError(t, err)
	for target, keep := range map[string]bool{
		"https://hangar.example.com/acme/m2/resolve/main/w.gguf": true,
		"https://s3.hangar.example.com/bucket/key?" + signature:  false,
		"http://hangar.example.com/acme/m/resolve/main/w.gguf":   false,
		"https://minio.internal:9000/bucket/key?" + signature:    false,
	} {
		req, err := http.NewRequest(http.MethodGet, target, nil)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer hf_secret")
		require.NoError(t, checkRedirect(req, []*http.Request{first}))
		require.Equal(t, keep, req.Header.Get("Authorization") != "", target)
	}
	via := make([]*http.Request, maxRedirects)
	for i := range via {
		via[i] = first
	}
	require.EqualError(t, checkRedirect(first, via), "stopped after 10 redirects")
}
