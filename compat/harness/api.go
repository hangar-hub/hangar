package harness

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"testing"
)

// AdminUsername is the bootstrap admin of every Instance Start runs.
const AdminUsername = "hangar-admin"

// adminPassword is the bootstrap admin password of every Instance Start
// runs, so suites can sign in and seed the Instance through its API.
const adminPassword = "compat-admin-password-1" // #nosec G101 -- a test Instance on loopback.

// apiBase is the native API prefix.
const apiBase = "/api/hangar/v1"

// API calls the native API with a Token (empty for none) and a JSON body
// (nil for none), and returns the status and the response body.
func (i *Instance) API(t testing.TB, method, path, token string, body any) (int, []byte) {
	t.Helper()
	return i.call(t, http.DefaultClient, method, path, token, "", body)
}

func (i *Instance) call(t testing.TB, client *http.Client, method, path, token, csrf string, body any) (int, []byte) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode %s %s: %v", method, path, err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, i.URL+apiBase+path, rd)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if csrf != "" {
		req.Header.Set("X-CSRF-Token", csrf)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

// MustAPI is API that fails the test unless the status is want.
func (i *Instance) MustAPI(t testing.TB, want int, method, path, token string, body any) []byte {
	t.Helper()
	code, out := i.API(t, method, path, token, body)
	if code != want {
		t.Fatalf("%s %s answered %d, want %d: %s", method, path, code, want, out)
	}
	return out
}

// Token signs in with a password and creates a write Token for the user.
func (i *Instance) Token(t testing.TB, username, password string) string {
	t.Helper()
	return i.token(t, username, password, "write")
}

// ScopedToken signs in with a password and creates a Token of the scope
// (read, write or admin) for the user.
func (i *Instance) ScopedToken(t testing.TB, username, password, scope string) string {
	t.Helper()
	return i.token(t, username, password, scope)
}

func (i *Instance) token(t testing.TB, username, password, scope string) string {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	code, out := i.call(t, client, http.MethodPost, "/auth/login", "", "", map[string]string{"username": username, "password": password})
	if code != http.StatusOK {
		t.Fatalf("sign in as %s answered %d: %s", username, code, out)
	}
	var me struct {
		CSRF string `json:"csrf_token"`
	}
	if err := json.Unmarshal(out, &me); err != nil || me.CSRF == "" {
		t.Fatalf("sign in as %s returned no CSRF token: %s", username, out)
	}
	code, out = i.call(t, client, http.MethodPost, "/me/tokens", "", me.CSRF, map[string]string{"name": "compat", "scope": scope})
	if code != http.StatusCreated && code != http.StatusOK {
		t.Fatalf("create a Token for %s answered %d: %s", username, code, out)
	}
	var tok struct {
		Secret string `json:"secret"`
	}
	if err := json.Unmarshal(out, &tok); err != nil || !strings.HasPrefix(tok.Secret, "hgr_") {
		t.Fatalf("create a Token for %s returned no secret", username)
	}
	return tok.Secret
}

// AdminToken returns an admin-scoped Token of the bootstrap admin.
func (i *Instance) AdminToken(t testing.TB) string {
	t.Helper()
	return i.token(t, AdminUsername, adminPassword, "admin")
}

// CreateUser creates a local user with the admin Token and returns a write
// Token of the new user.
func (i *Instance) CreateUser(t testing.TB, adminToken, username string) string {
	t.Helper()
	return i.Token(t, username, i.AddUser(t, adminToken, username))
}

// AddUser creates a local user with the admin Token and returns the
// user's password.
func (i *Instance) AddUser(t testing.TB, adminToken, username string) string {
	t.Helper()
	password := "compat-password-for-" + username
	i.MustAPI(t, http.StatusCreated, http.MethodPost, "/admin/users", adminToken,
		map[string]any{"username": username, "password": password, "email": username + "@example.com"})
	return password
}

// CommitFiles creates a Repo of the token's user, or of another Namespace
// it can write to, and commits files to main through the native API. The
// Repo's .gitattributes decides which files are stored in LFS.
func (i *Instance) CommitFiles(t testing.TB, token, repoType, namespace, name, visibility string, files map[string][]byte) {
	t.Helper()
	i.MustAPI(t, http.StatusCreated, http.MethodPost, "/repos", token,
		map[string]any{"type": repoType, "namespace": namespace, "name": name, "visibility": visibility})
	ops := make([]map[string]any, 0, len(files))
	for p, b := range files {
		ops = append(ops, map[string]any{"op": "add", "path": p, "content_base64": base64Std(b)})
	}
	i.MustAPI(t, http.StatusCreated, http.MethodPost, fmt.Sprintf("/repos/%ss/%s/%s/commits", repoType, namespace, name), token,
		map[string]any{"summary": "Add files", "operations": ops})
}

func base64Std(b []byte) string { return base64.StdEncoding.EncodeToString(b) }
