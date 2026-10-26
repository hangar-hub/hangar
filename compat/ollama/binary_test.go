//go:build compat

package ollama_test

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"

	"github.com/hangar-hub/hangar/compat/harness"
	"github.com/hangar-hub/hangar/internal/db/dbtest"
	"github.com/hangar-hub/hangar/internal/model"
	"github.com/hangar-hub/hangar/internal/sshsig"
)

// binary is a real Hangar Instance started by compat/harness: the binary
// built from ./cmd/hangar, or HANGAR_BINARY, with every real service
// wired. Ollama reaches it through a reverse proxy in the test process
// that keeps the Host header and records each request, so the scenarios
// can check what Ollama sent.
type binary struct {
	*recorder
	inst   *harness.Instance
	listen string
	token  string
}

// startBinarySQLite runs the binary on a temporary SQLite database.
func startBinarySQLite(t *testing.T, listen string) target {
	return startBinary(t, listen)
}

// startBinaryPostgres runs the binary on a fresh database on the test
// Postgres server (dbtest: a container, or HANGAR_TEST_POSTGRES_DSN).
func startBinaryPostgres(t *testing.T, listen string) target {
	return startBinary(t, listen, "database.driver=postgres", "database.dsn="+dbtest.PostgresDSN(t))
}

func startBinary(t *testing.T, listen string, set ...string) target {
	t.Helper()
	secret := make([]byte, 16)
	_, err := rand.Read(secret)
	require.NoError(t, err)
	password := hex.EncodeToString(secret)
	inst := harness.Start(t, harness.Options{
		Set: append([]string{"security.anonymous_read=true", "security.bootstrap_admin.username=" + owner}, set...),
		Env: []string{"HANGAR_SECURITY_BOOTSTRAP_ADMIN_PASSWORD=" + password},
	})
	b := &binary{recorder: &recorder{}, inst: inst}
	b.token = b.createToken(t, password)

	upstream, err := url.Parse(inst.URL)
	require.NoError(t, err)
	proxy := &httputil.ReverseProxy{Rewrite: func(pr *httputil.ProxyRequest) {
		pr.SetURL(upstream)
		// Hangar builds the token realm and blob URLs from the Host Ollama
		// used, so the proxy must not replace it.
		pr.Out.Host = pr.In.Host
	}}
	ln, err := net.Listen("tcp", listen)
	require.NoError(t, err)
	srv := &http.Server{Handler: b.wrap(proxy), ReadHeaderTimeout: 30 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	b.listen = ln.Addr().String()
	return b
}

func (b *binary) addr() string { return b.listen }

// createToken signs in as the bootstrap admin with a password, the way
// the web UI does, and creates a write Token.
func (b *binary) createToken(t *testing.T, password string) string {
	t.Helper()
	jar, err := cookiejar.New(nil)
	require.NoError(t, err)
	c := &http.Client{Jar: jar, Timeout: 30 * time.Second}
	var me struct {
		CSRFToken string `json:"csrf_token"`
	}
	b.call(t, c, http.MethodPost, "/api/hangar/v1/auth/login", nil,
		map[string]string{"username": owner, "password": password}, http.StatusOK, &me)
	require.NotEmpty(t, me.CSRFToken, "the login response carries a CSRF token")
	var tok struct {
		Secret string `json:"secret"`
	}
	b.call(t, c, http.MethodPost, "/api/hangar/v1/me/tokens", map[string]string{"X-CSRF-Token": me.CSRFToken},
		map[string]string{"name": "compat ollama", "scope": "write"}, http.StatusCreated, &tok)
	require.NotEmpty(t, tok.Secret)
	return tok.Secret
}

// call sends a JSON request to the Instance and decodes the response.
// Errors name the status and body, never the request headers.
func (b *binary) call(t *testing.T, c *http.Client, method, path string, headers map[string]string, body any, want int, out any) {
	t.Helper()
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	req, err := http.NewRequest(method, b.inst.URL+path, bytes.NewReader(raw))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	require.Equal(t, want, resp.StatusCode, "%s %s: %s", method, path, got)
	if out != nil {
		require.NoError(t, json.Unmarshal(got, out), "%s %s: %s", method, path, got)
	}
}

// createRepo uploads files with `hangar push --create`, which goes through
// the native upload API, the Blob store and the Repo service.
func (b *binary) createRepo(t *testing.T, id model.RepoID, vis model.Visibility, files map[string][]byte) {
	t.Helper()
	home := t.TempDir()
	src := filepath.Join(home, "src")
	for p, content := range files {
		full := filepath.Join(src, filepath.FromSlash(p))
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, content, 0o644))
	}
	args := []string{"push", src, id.String(), "--create", "--message", "Add the compat fixtures"}
	if vis == model.VisibilityPrivate {
		args = append(args, "--private")
	}
	cmd := exec.Command(harness.Binary(t), args...)
	cmd.Env = append(os.Environ(), "HOME="+home, "XDG_CONFIG_HOME="+filepath.Join(home, ".config"),
		"HANGAR_CLI_URL="+b.inst.URL, "HANGAR_CLI_TOKEN="+b.token, "HANGAR_CLI_NO_KEYCHAIN=1")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "hangar push: %s", out)
}

// registerKey registers an Ollama key for the bootstrap admin through the
// native API, as Settings, Ollama keys and `hangar ollama key register` do:
// it signs a challenge with the key's private half, then adds the key.
func (b *binary) registerKey(t *testing.T, authorizedKey string, privateKey []byte) {
	t.Helper()
	c := &http.Client{Timeout: 30 * time.Second}
	auth := map[string]string{"Authorization": "Bearer " + b.token}
	var ch struct {
		Challenge string `json:"challenge"`
		Namespace string `json:"namespace"`
	}
	b.call(t, c, http.MethodPost, "/api/hangar/v1/me/ollama-keys/challenge", auth, struct{}{}, http.StatusOK, &ch)
	signer, err := ssh.ParsePrivateKey(privateKey)
	require.NoError(t, err)
	sig, err := sshsig.Sign(signer, ch.Namespace, []byte(ch.Challenge))
	require.NoError(t, err)
	b.call(t, c, http.MethodPost, "/api/hangar/v1/me/ollama-keys", auth,
		map[string]string{"name": "compat ollama", "public_key": authorizedKey, "challenge": ch.Challenge, "signature": sig},
		http.StatusCreated, nil)
}
