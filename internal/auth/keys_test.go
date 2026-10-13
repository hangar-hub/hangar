package auth_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"golang.org/x/crypto/ssh"

	"github.com/jamesagarside/hangar/internal/audit"
	"github.com/jamesagarside/hangar/internal/audit/audittest"
	"github.com/jamesagarside/hangar/internal/auth"
	"github.com/jamesagarside/hangar/internal/auth/authtest"
	"github.com/jamesagarside/hangar/internal/db"
	"github.com/jamesagarside/hangar/internal/db/dbtest"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/secrets"
)

// ollamaKey is an Ollama client key pair, generated in the test.
type ollamaKey struct {
	signer ssh.Signer
	pub    string // authorized_keys line, as in id_ed25519.pub
}

func newOllamaKey(t *testing.T) ollamaKey {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	signer, err := ssh.NewSignerFromKey(priv)
	require.NoError(t, err)
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey()))) + " me@laptop"
	return ollamaKey{signer: signer, pub: line}
}

// sign does what Ollama's auth.Sign does: `<base64 public key>:<base64
// signature>` over `GET,<url>,<base64 of hex sha256 of empty body>`.
func (k ollamaKey) sign(t *testing.T, fullURL string) string {
	t.Helper()
	sig, err := k.signer.Sign(rand.Reader, auth.OllamaSignedData(http.MethodGet, fullURL))
	require.NoError(t, err)
	parts := bytes.Split(ssh.MarshalAuthorizedKey(k.signer.PublicKey()), []byte(" "))
	return fmt.Sprintf("%s:%s", bytes.TrimSpace(parts[1]), base64.StdEncoding.EncodeToString(sig.Blob))
}

// realmURL builds the URL Ollama's registryChallenge.URL builds.
func realmURL(ts time.Time, nonce, repo string) string {
	v := url.Values{}
	v.Add("service", "hangar.test")
	v.Add("scope", "repository:"+repo+":pull")
	v.Add("ts", strconv.FormatInt(ts.Unix(), 10))
	v.Add("nonce", nonce)
	return "http://hangar.test/v2/token?" + v.Encode()
}

type repoMap map[string]*auth.RepoTarget

func (m repoMap) RepoTarget(_ context.Context, id model.RepoID) (*auth.RepoTarget, error) {
	if t, ok := m[id.String()]; ok {
		return t, nil
	}
	return nil, fmt.Errorf("%w: %s", auth.ErrNotFound, id)
}

func TestOllamaKeysAndRegistryTokens(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		ctx := context.Background()
		clock := authtest.NewClock(time.Now().UTC().Truncate(time.Second))
		rec := audittest.NewFake()
		s, orgs := authtest.NewService(t, d, authtest.RealOptions{Clock: clock, Audit: rec})
		alice := mustUser(t, s, "alice", false)
		bob := mustUser(t, s, "bob", false)
		aliceNS, err := orgs.Get(ctx, "alice")
		require.NoError(t, err)
		private := insertRepo(t, d, aliceNS.ID, "alice", "secret-gguf", model.VisibilityPrivate, false)
		s.Ollama.Repos = repoMap{"alice/secret-gguf": private}

		key := newOllamaKey(t)
		k, err := s.Ollama.Add(ctx, alice.ID, "", key.pub, alice.Principal(model.AuthSession))
		require.NoError(t, err)
		require.Equal(t, "me@laptop", k.Name, "the key comment names it")
		require.True(t, strings.HasPrefix(k.Fingerprint, "SHA256:"))
		require.False(t, strings.Contains(k.PublicKey, "me@laptop"))
		_, err = s.Ollama.Add(ctx, bob.ID, "", key.pub, bob.Principal(model.AuthSession))
		require.ErrorIs(t, err, auth.ErrConflict, "a key belongs to one user")
		for name, bad := range map[string]string{
			"garbage": "not a key",
			"rsa":     "ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAAAgQDHu9g5sAtGyoUwZ0ZGXUdhWtjMm8w3/F9BfZz6S0XyFQ== x",
		} {
			_, err = s.Ollama.Add(ctx, alice.ID, "", bad, alice.Principal(model.AuthSession))
			require.ErrorIs(t, err, auth.ErrInvalid, name)
		}
		keys, err := s.Ollama.List(ctx, alice.ID)
		require.NoError(t, err)
		require.Len(t, keys, 1)

		repo := model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "secret-gguf"}
		issue := func(k ollamaKey, ts time.Time, nonce string, id model.RepoID) (string, error) {
			u := realmURL(ts, nonce, id.String())
			tok, exp, err := s.Ollama.IssueRegistryToken(ctx, auth.OllamaTokenRequest{Method: http.MethodGet, URL: u, Authorization: k.sign(t, u), Repo: id})
			if err == nil {
				require.Equal(t, clock.Now().Add(5*time.Minute), exp)
			}
			return tok, err
		}
		tok, err := issue(key, clock.Now(), "nonce-1", repo)
		require.NoError(t, err)

		// The token authenticates as alice, for that Repo only.
		r := httptest.NewRequest(http.MethodGet, "http://hangar.test/v2/alice/secret-gguf/manifests/latest", nil)
		r.Header.Set("Authorization", "Bearer "+tok)
		p, err := s.Authenticate(r)
		require.NoError(t, err)
		require.Equal(t, alice.ID, p.UserID)
		require.Equal(t, model.AuthOllamaKey, p.Method)
		require.Equal(t, model.ScopeRead, p.MaxScope())
		require.NoError(t, s.Authorizer.Can(ctx, p, auth.ActionReadFiles, auth.Target{Repo: private}))
		require.ErrorIs(t, s.Authorizer.Can(ctx, p, auth.ActionWrite, auth.Target{Repo: private}), auth.ErrForbidden)
		other := insertRepo(t, d, aliceNS.ID, "alice", "other", model.VisibilityPrivate, false)
		require.ErrorIs(t, s.Authorizer.Can(ctx, p, auth.ActionRead, auth.Target{Repo: other}), auth.ErrNotFound)

		_, err = issue(key, clock.Now(), "nonce-1", repo)
		require.ErrorIs(t, err, auth.ErrInvalidCredentials, "a reused nonce is refused")
		_, err = issue(key, clock.Now().Add(-6*time.Minute), "nonce-2", repo)
		require.ErrorIs(t, err, auth.ErrInvalidCredentials, "a stale ts is refused")
		_, err = issue(key, clock.Now().Add(6*time.Minute), "nonce-3", repo)
		require.ErrorIs(t, err, auth.ErrInvalidCredentials, "a future ts is refused")
		_, err = issue(newOllamaKey(t), clock.Now(), "nonce-4", repo)
		require.ErrorIs(t, err, auth.ErrInvalidCredentials, "unregistered key")

		u := realmURL(clock.Now(), "nonce-5", repo.String())
		_, _, err = s.Ollama.IssueRegistryToken(ctx, auth.OllamaTokenRequest{URL: u + "&x=1", Authorization: key.sign(t, u), Repo: repo})
		require.ErrorIs(t, err, auth.ErrInvalidCredentials, "the signature covers the whole URL")
		_, _, err = s.Ollama.IssueRegistryToken(ctx, auth.OllamaTokenRequest{URL: u, Authorization: "garbage", Repo: repo})
		require.ErrorIs(t, err, auth.ErrInvalidCredentials)

		// bob's key cannot read alice's private Repo; unknown Repos are 404 too.
		bobKey := newOllamaKey(t)
		_, err = s.Ollama.Add(ctx, bob.ID, "bob's", bobKey.pub, bob.Principal(model.AuthSession))
		require.NoError(t, err)
		_, err = issue(bobKey, clock.Now(), "nonce-6", repo)
		require.ErrorIs(t, err, auth.ErrNotFound)
		_, err = issue(bobKey, clock.Now(), "nonce-7", model.RepoID{Type: model.RepoTypeModel, Namespace: "x", Name: "y"})
		require.ErrorIs(t, err, auth.ErrNotFound)

		// Nonces expire after 10 minutes.
		clock.Advance(11 * time.Minute)
		_, err = issue(key, clock.Now(), "nonce-1", repo)
		require.NoError(t, err)

		// Registry tokens expire after 5 minutes.
		clock.Advance(5 * time.Minute)
		_, err = s.Authenticate(r)
		require.ErrorIs(t, err, auth.ErrInvalidCredentials)

		// A forged token signed with another key is refused.
		parts := strings.Split(tok, ".")
		forged := parts[0] + "." + parts[1] + "." + base64.RawURLEncoding.EncodeToString(make([]byte, 32))
		r.Header.Set("Authorization", "Bearer "+forged)
		_, err = s.Authenticate(r)
		require.ErrorIs(t, err, auth.ErrInvalidCredentials)

		require.ErrorIs(t, s.Ollama.Delete(ctx, bob.ID, k.ID, bob.Principal(model.AuthSession)), auth.ErrNotFound, "only the owner removes a key")
		require.NoError(t, s.Ollama.Delete(ctx, alice.ID, k.ID, alice.Principal(model.AuthSession)))
		_, err = issue(key, clock.Now(), "nonce-8", repo)
		require.ErrorIs(t, err, auth.ErrInvalidCredentials, "removed keys stop working")
		require.Contains(t, rec.Actions(), audit.OllamaKeyChanged)
	})
}

func FuzzParseOllamaAuthorization(f *testing.F) {
	f.Add("AAAAC3NzaC1lZDI1NTE5AAAAIK0wmN/Cr3JXqmLW7u+g9pTh+wyqDHpSQEIQczXkVx9q:" + base64.StdEncoding.EncodeToString(make([]byte, 64)))
	f.Add("a:b")
	f.Add(":")
	f.Add("")
	f.Fuzz(func(t *testing.T, h string) {
		pub, sig, err := auth.ParseOllamaAuthorization(h)
		if err == nil {
			require.Equal(t, ssh.KeyAlgoED25519, pub.Type())
			require.Len(t, sig, 64)
		}
	})
}

func FuzzParseOllamaPublicKey(f *testing.F) {
	f.Add("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIK0wmN/Cr3JXqmLW7u+g9pTh+wyqDHpSQEIQczXkVx9q me@host")
	f.Add("ssh-rsa AAAA")
	f.Fuzz(func(t *testing.T, s string) {
		norm, fp, err := auth.ParseOllamaPublicKey(s)
		if err == nil {
			require.True(t, strings.HasPrefix(norm, "ssh-ed25519 "))
			require.True(t, strings.HasPrefix(fp, "SHA256:"))
		}
	})
}

func TestOllamaRequestURL(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "http://internal:8080/v2/token?service=x&scope=repository%3Aa%2Fb%3Apull&ts=1&nonce=n", nil)
	require.Equal(t, "https://hangar.example.com/v2/token?service=x&scope=repository%3Aa%2Fb%3Apull&ts=1&nonce=n",
		auth.OllamaRequestURL("https://hangar.example.com/", r))
}

func TestDeviceFlow(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		ctx := context.Background()
		clock := authtest.NewClock(start())
		s, _ := authtest.NewService(t, d, authtest.RealOptions{Clock: clock})
		alice := mustUser(t, s, "alice", false)
		aliceP := alice.Principal(model.AuthSession)

		dc, err := s.Device.Start(ctx, "hf-cli")
		require.NoError(t, err)
		require.Regexp(t, `^[BCDFGHJKLMNPQRSTVWXZ]{4}-[BCDFGHJKLMNPQRSTVWXZ]{4}$`, dc.UserCode)
		require.Equal(t, "http://hangar.test/settings/device", dc.VerificationURI)
		require.Equal(t, dc.VerificationURI+"?code="+dc.UserCode, dc.VerificationURIComplete)
		require.Equal(t, 5*time.Second, dc.Interval)

		_, err = s.Device.Poll(ctx, dc.DeviceCode)
		require.ErrorIs(t, err, auth.ErrAuthorizationPending)
		_, err = s.Device.Poll(ctx, dc.DeviceCode)
		require.ErrorIs(t, err, auth.ErrSlowDown, "polling faster than the interval")
		clock.Advance(5 * time.Second)
		_, err = s.Device.Poll(ctx, dc.DeviceCode)
		require.ErrorIs(t, err, auth.ErrAuthorizationPending)

		require.ErrorIs(t, s.Device.Approve(ctx, dc.UserCode, model.Anonymous()), auth.ErrForbidden)
		tokenP := aliceP
		tokenP.Token = &model.TokenInfo{Scope: model.ScopeAdmin}
		require.ErrorIs(t, s.Device.Approve(ctx, dc.UserCode, tokenP), auth.ErrForbidden, "approve in the UI, not with a token")
		require.ErrorIs(t, s.Device.Approve(ctx, "BBBB-BBBB", aliceP), auth.ErrNotFound)
		typed := strings.ToLower(strings.ReplaceAll(dc.UserCode, "-", ""))
		require.NoError(t, s.Device.Approve(ctx, typed, aliceP), "codes are accepted in any case, without the dash")
		require.ErrorIs(t, s.Device.Deny(ctx, dc.UserCode, aliceP), auth.ErrNotFound, "already answered")

		clock.Advance(5 * time.Second)
		secret, err := s.Device.Poll(ctx, dc.DeviceCode)
		require.NoError(t, err)
		r := httptest.NewRequest(http.MethodGet, "http://hangar.test/api/whoami-v2", nil)
		r.Header.Set("Authorization", "Bearer "+secret)
		p, err := s.Authenticate(r)
		require.NoError(t, err)
		require.Equal(t, alice.ID, p.UserID)
		require.Equal(t, model.ScopeWrite, p.Token.Scope)
		require.Equal(t, "device login (hf-cli)", p.Token.Name)
		clock.Advance(5 * time.Second)
		_, err = s.Device.Poll(ctx, dc.DeviceCode)
		require.ErrorIs(t, err, auth.ErrExpiredToken, "the secret is handed out once")

		dc2, err := s.Device.Start(ctx, uuid.NewString())
		require.NoError(t, err)
		require.NoError(t, s.Device.Deny(ctx, dc2.UserCode, aliceP))
		_, err = s.Device.Poll(ctx, dc2.DeviceCode)
		require.ErrorIs(t, err, auth.ErrAccessDenied)

		dc3, err := s.Device.Start(ctx, "")
		require.NoError(t, err)
		clock.Advance(16 * time.Minute)
		_, err = s.Device.Poll(ctx, dc3.DeviceCode)
		require.ErrorIs(t, err, auth.ErrExpiredToken)
		require.ErrorIs(t, s.Device.Approve(ctx, dc3.UserCode, aliceP), auth.ErrNotFound)
		_, err = s.Device.Poll(ctx, "unknown")
		require.ErrorIs(t, err, auth.ErrExpiredToken)
		clock.Advance(24 * time.Hour)
		n, err := s.Device.DeleteExpired(ctx)
		require.NoError(t, err)
		require.Equal(t, int64(3), n)
	})
}

type upstreamNames map[string]uuid.UUID

func (u upstreamNames) UpstreamID(_ context.Context, name string) (uuid.UUID, error) {
	if id, ok := u[name]; ok {
		return id, nil
	}
	return uuid.Nil, fmt.Errorf("%w: upstream %s", auth.ErrNotFound, name)
}

func TestUpstreamTokens(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		ctx := context.Background()
		hfID := db.NewID()
		_, err := d.NewInsert().Model(&db.Upstream{ID: hfID, Name: "huggingface", DisplayName: "Hugging Face", BaseURL: "https://huggingface.co",
			Kind: "hf", Enabled: true, Source: "config"}).Exec(ctx)
		require.NoError(t, err)
		rec := audittest.NewFake()
		s, _ := authtest.NewService(t, d, authtest.RealOptions{Audit: rec, Upstreams: upstreamNames{"huggingface": hfID}})
		alice := mustUser(t, s, "alice", false)
		ap := alice.Principal(model.AuthSession)

		st, err := s.UpstreamTokens.Status(ctx, alice.ID, "huggingface")
		require.NoError(t, err)
		require.False(t, st.Saved)
		_, err = s.UpstreamTokens.Status(ctx, alice.ID, "nope")
		require.ErrorIs(t, err, auth.ErrNotFound)

		st, err = s.UpstreamTokens.Set(ctx, alice.ID, "huggingface", "hf_secret_value", ap)
		require.NoError(t, err)
		require.True(t, st.Saved)
		require.NotNil(t, st.UpdatedAt)
		var row db.UpstreamToken
		require.NoError(t, d.NewSelect().Model(&row).Where("user_id = ?", alice.ID).Scan(ctx))
		require.NotContains(t, row.Ciphertext, "hf_secret_value", "stored encrypted")
		tok, ok, err := s.UpstreamTokens.Token(ctx, alice.ID, hfID)
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, "hf_secret_value", tok)
		_, err = s.UpstreamTokens.Set(ctx, alice.ID, "huggingface", "hf_second", ap)
		require.NoError(t, err, "replacing works")
		tok, _, _ = s.UpstreamTokens.Token(ctx, alice.ID, hfID)
		require.Equal(t, "hf_second", tok)
		_, err = s.UpstreamTokens.Set(ctx, alice.ID, "huggingface", "  ", ap)
		require.ErrorIs(t, err, auth.ErrInvalid)

		// Key rotation re-encrypts stored tokens in one transaction.
		key := make([]byte, 32)
		_, _ = rand.Read(key)
		newSealer, err := secrets.New(key)
		require.NoError(t, err)
		oldKey, err := s.Config().InstanceKeyBytes()
		require.NoError(t, err)
		oldSealer, err := secrets.New(oldKey)
		require.NoError(t, err)
		require.NoError(t, d.InTx(ctx, func(ctx context.Context, tx bun.Tx) error {
			n, err := auth.ReencryptUpstreamTokens(ctx, tx, oldSealer, newSealer)
			require.Equal(t, 1, n)
			return err
		}))
		require.NoError(t, d.NewSelect().Model(&row).Where("user_id = ?", alice.ID).Scan(ctx))
		plain, err := newSealer.Open(row.Ciphertext, "upstream_token")
		require.NoError(t, err)
		require.Equal(t, "hf_second", plain)
		require.Error(t, d.InTx(ctx, func(ctx context.Context, tx bun.Tx) error {
			_, err := auth.ReencryptUpstreamTokens(ctx, tx, oldSealer, newSealer)
			return err
		}), "the wrong old key fails")

		require.NoError(t, s.UpstreamTokens.Delete(ctx, alice.ID, "huggingface", ap))
		require.ErrorIs(t, s.UpstreamTokens.Delete(ctx, alice.ID, "huggingface", ap), auth.ErrNotFound)
		_, ok, err = s.UpstreamTokens.Token(ctx, alice.ID, hfID)
		require.NoError(t, err)
		require.False(t, ok)
		require.Contains(t, rec.Actions(), audit.UpstreamTokenChanged)
		for _, e := range rec.Events() {
			require.NotContains(t, fmt.Sprint(e.Data), "hf_", "tokens never reach the audit log")
		}
	})
}

func TestAccessRequests(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		ctx := context.Background()
		rec := audittest.NewFake()
		s, orgs := authtest.NewService(t, d, authtest.RealOptions{Audit: rec})
		owner := mustUser(t, s, "owner", false)
		ns, err := orgs.Get(ctx, "owner")
		require.NoError(t, err)
		target := insertRepo(t, d, ns.ID, "owner", "gated", model.VisibilityPublic, true)
		form := []auth.GatingField{{Name: "company", Type: "text", Required: true}, {Name: "agree", Type: "checkbox", Required: true}, {Name: "note", Type: "text"}}
		manual := auth.GatedRepo{UUID: target.UUID, ID: target.ID, Mode: auth.GatingManual, Fields: form}
		users := map[string]model.Principal{}
		for _, n := range []string{"ann", "ben", "cat"} {
			users[n] = mustUser(t, s, n, false).Principal(model.AuthSession)
		}

		_, err = s.AccessRequests.Request(ctx, manual, model.Anonymous(), nil)
		require.ErrorIs(t, err, auth.ErrForbidden)
		for name, f := range map[string]map[string]any{
			"missing":   {"company": "x"},
			"unchecked": {"company": "x", "agree": false},
			"unknown":   {"company": "x", "agree": true, "extra": "y"},
			"nested":    {"company": map[string]any{"a": 1}, "agree": true},
		} {
			_, err = s.AccessRequests.Request(ctx, manual, users["ann"], f)
			require.ErrorIs(t, err, auth.ErrInvalid, name)
		}
		fields := map[string]any{"company": "Acme", "agree": true}
		req, err := s.AccessRequests.Request(ctx, manual, users["ann"], fields)
		require.NoError(t, err)
		require.Equal(t, auth.AccessPending, req.Status)
		require.Equal(t, "ann", req.Username)
		again, err := s.AccessRequests.Request(ctx, manual, users["ann"], fields)
		require.NoError(t, err)
		require.Equal(t, req.ID, again.ID, "asking twice returns the same request")
		_, err = s.AccessRequests.Request(ctx, manual, users["ben"], fields)
		require.NoError(t, err)
		_, err = s.AccessRequests.Request(ctx, manual, users["cat"], fields)
		require.NoError(t, err)

		page, next, err := s.AccessRequests.List(ctx, target.UUID, "", "", 2)
		require.NoError(t, err)
		require.Len(t, page, 2)
		require.Equal(t, "ann", page[0].Username)
		page, next, err = s.AccessRequests.List(ctx, target.UUID, "", next, 2)
		require.NoError(t, err)
		require.Len(t, page, 1)
		require.Empty(t, next)
		_, _, err = s.AccessRequests.List(ctx, target.UUID, "maybe", "", 2)
		require.ErrorIs(t, err, auth.ErrInvalid)

		op := owner.Principal(model.AuthSession)
		decided, err := s.AccessRequests.Decide(ctx, manual, req.ID, auth.AccessRejected, op)
		require.NoError(t, err)
		require.Equal(t, auth.AccessRejected, decided.Status)
		require.Equal(t, "owner", decided.DecidedBy)
		require.NotNil(t, decided.DecidedAt)
		_, err = s.AccessRequests.Request(ctx, manual, users["ann"], fields)
		require.ErrorIs(t, err, auth.ErrConflict, "rejected users cannot ask again")
		_, err = s.AccessRequests.Decide(ctx, manual, req.ID, "maybe", op)
		require.ErrorIs(t, err, auth.ErrInvalid)
		_, err = s.AccessRequests.Decide(ctx, manual, uuid.New(), auth.AccessAccepted, op)
		require.ErrorIs(t, err, auth.ErrNotFound)
		pending, _, err := s.AccessRequests.List(ctx, target.UUID, auth.AccessPending, "", 10)
		require.NoError(t, err)
		require.Len(t, pending, 2)

		auto := insertRepo(t, d, ns.ID, "owner", "auto", model.VisibilityPublic, true)
		ar, err := s.AccessRequests.Request(ctx, auth.GatedRepo{UUID: auto.UUID, ID: auto.ID, Mode: auth.GatingAuto}, users["ann"], nil)
		require.NoError(t, err)
		require.Equal(t, auth.AccessAccepted, ar.Status, "auto mode accepts at once")
		ok, err := s.AccessRequests.Accepted(ctx, auto.UUID, users["ann"].UserID)
		require.NoError(t, err)
		require.True(t, ok)
		mine, err := s.AccessRequests.Mine(ctx, auto.UUID, users["ann"].UserID)
		require.NoError(t, err)
		require.Equal(t, ar.ID, mine.ID)
		_, err = s.AccessRequests.Request(ctx, auth.GatedRepo{UUID: auto.UUID, ID: auto.ID}, users["ben"], nil)
		require.ErrorIs(t, err, auth.ErrInvalid, "not gated")

		n := 0
		for _, a := range rec.Actions() {
			if a == audit.AccessRequestDecided {
				n++
			}
		}
		require.Equal(t, 2, n, "a manual decision and an automatic acceptance")
	})
}
