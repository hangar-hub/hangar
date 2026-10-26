//go:build compat

package ollama_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"

	"github.com/hangar-hub/hangar/internal/auth"
	"github.com/hangar-hub/hangar/internal/auth/authtest"
	"github.com/hangar-hub/hangar/internal/db/dbtest"
	"github.com/hangar-hub/hangar/internal/httpx"
	"github.com/hangar-hub/hangar/internal/index/indextest"
	"github.com/hangar-hub/hangar/internal/model"
	"github.com/hangar-hub/hangar/internal/ollama"
	"github.com/hangar-hub/hangar/internal/repo/repotest"
)

// keyAuth verifies Ollama key signatures the way Ollama's registry auth
// signs them, for the in-process Instance. W5's auth.Service is the
// production implementation, which the binary targets use.
type keyAuth struct {
	authn *authtest.Authenticator
	mu    sync.Mutex
	keys  map[string]model.Principal
	seen  map[string]bool
}

func (k *keyAuth) register(authorizedKey string, p model.Principal) {
	k.mu.Lock()
	defer k.mu.Unlock()
	fields := strings.Fields(authorizedKey)
	k.keys[fields[1]] = p
}

func (k *keyAuth) IssueRegistryToken(_ context.Context, req auth.OllamaTokenRequest) (string, time.Time, error) {
	pub, sig, ok := strings.Cut(req.Authorization, ":")
	if !ok {
		return "", time.Time{}, auth.ErrInvalidCredentials
	}
	k.mu.Lock()
	p, known := k.keys[pub]
	k.mu.Unlock()
	if !known {
		return "", time.Time{}, auth.ErrInvalidCredentials
	}
	raw, err := base64.StdEncoding.DecodeString(pub)
	if err != nil {
		return "", time.Time{}, auth.ErrInvalidCredentials
	}
	key, err := ssh.ParsePublicKey(raw)
	if err != nil {
		return "", time.Time{}, auth.ErrInvalidCredentials
	}
	blob, err := base64.StdEncoding.DecodeString(sig)
	if err != nil {
		return "", time.Time{}, auth.ErrInvalidCredentials
	}
	empty := sha256.Sum256(nil)
	data := fmt.Sprintf("%s,%s,%s", req.Method, req.URL, base64.StdEncoding.EncodeToString([]byte(hex.EncodeToString(empty[:]))))
	if err := key.Verify([]byte(data), &ssh.Signature{Format: key.Type(), Blob: blob}); err != nil {
		return "", time.Time{}, auth.ErrInvalidCredentials
	}
	u, err := url.Parse(req.URL)
	if err != nil {
		return "", time.Time{}, auth.ErrInvalidCredentials
	}
	ts, err := strconv.ParseInt(u.Query().Get("ts"), 10, 64)
	if err != nil || time.Since(time.Unix(ts, 0)).Abs() > 5*time.Minute {
		return "", time.Time{}, auth.ErrInvalidCredentials
	}
	nonce := u.Query().Get("nonce")
	k.mu.Lock()
	reused := k.seen[nonce]
	k.seen[nonce] = true
	k.mu.Unlock()
	if nonce == "" || reused {
		return "", time.Time{}, auth.ErrInvalidCredentials
	}
	if !strings.EqualFold(req.Repo.Namespace, p.Username) {
		return "", time.Time{}, auth.ErrNotFound
	}
	token := "ollama-" + sha([]byte(req.URL))
	p.Method = model.AuthOllamaKey
	p.Token = &model.TokenInfo{Name: "ollama", Scope: model.ScopeRead,
		Restrictions: model.TokenRestrictions{Repos: []string{string(req.Repo.Type) + ":" + req.Repo.String()}}}
	k.authn.Add(token, p)
	return token, time.Now().Add(5 * time.Minute), nil
}

// inProcess serves the Ollama registry from the test process, over the
// Repo service fake and a SQLite database. It checks internal/ollama on
// its own; the binary targets check it as cmd/hangar wires it.
type inProcess struct {
	*recorder
	listen string
	repos  *repotest.Fake
	keys   *keyAuth
}

func startInProcess(t *testing.T, listen string) target {
	t.Helper()
	in := &inProcess{repos: repotest.NewFake(t), recorder: &recorder{}}
	authn := authtest.NewAuthenticator()
	in.keys = &keyAuth{authn: authn, keys: map[string]model.Principal{}, seen: map[string]bool{}}
	authz := authtest.NewAuthz()
	authz.AnonymousRead = true
	signer, err := httpx.NewSigner([]byte("compat-ollama-instance-key-0123456789"))
	require.NoError(t, err)
	h, err := ollama.New(ollama.Options{DB: dbtest.SQLite(t), Repos: in.repos, LFS: in.repos.LFS, Authz: authz,
		OllamaAuth: in.keys, Signer: signer, PublicURL: "http://" + listen})
	require.NoError(t, err)
	ln, err := net.Listen("tcp", listen)
	require.NoError(t, err)
	handler := in.wrap(httpx.Dispatch(httpx.Protocols{Ollama: h}, "http://"+listen))
	srv := &http.Server{Handler: httpx.RequestIDs(httpx.Authenticate(ollama.ExemptTokenEndpoint(authn), nil)(handler)),
		ReadHeaderTimeout: 30 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	in.listen = ln.Addr().String()
	return in
}

func (in *inProcess) addr() string { return in.listen }

func (in *inProcess) createRepo(t *testing.T, id model.RepoID, vis model.Visibility, files map[string][]byte) {
	t.Helper()
	indextest.CreateRepo(t, in.repos, id, vis)
	indextest.Commit(t, in.repos, id, files)
}

// registerKey registers the key with the fake OllamaAuth, which needs no
// proof that owner holds it.
func (in *inProcess) registerKey(t *testing.T, authorizedKey string, _ []byte) {
	t.Helper()
	in.keys.register(authorizedKey, indextest.Owner(owner))
}
