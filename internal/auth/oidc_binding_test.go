package auth_test

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/auth"
	"github.com/hangar-hub/hangar/internal/auth/authtest"
	"github.com/hangar-hub/hangar/internal/config"
	"github.com/hangar-hub/hangar/internal/db/dbtest"
	"github.com/hangar-hub/hangar/internal/httpx"
)

// fakeIdP is a minimal OpenID provider: discovery, a JWKS, and a token
// endpoint that checks PKCE and returns an RS256 ID token. Tests approve
// an authorisation request directly with approve, standing in for the
// user signing in at the provider. TestOIDCKeycloak covers a real one.
type fakeIdP struct {
	srv       *httptest.Server
	key       *rsa.PrivateKey
	exchanges atomic.Int32

	mu    sync.Mutex
	codes map[string]fakeGrant
}

type fakeGrant struct {
	clientID, redirectURI, challenge, nonce string
	claims                                  map[string]any
}

func newFakeIdP(t *testing.T) *fakeIdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	f := &fakeIdP{key: key, codes: map[string]fakeGrant{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		iss := f.srv.URL
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer": iss, "authorization_endpoint": iss + "/authorize", "token_endpoint": iss + "/token",
			"jwks_uri": iss + "/jwks", "id_token_signing_alg_values_supported": []string{"RS256"},
			"response_types_supported": []string{"code"}, "subject_types_supported": []string{"public"},
		})
	})
	mux.HandleFunc("GET /jwks", func(w http.ResponseWriter, _ *http.Request) {
		b64 := base64.RawURLEncoding.EncodeToString
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]any{{
			"kty": "RSA", "kid": "k1", "alg": "RS256", "use": "sig",
			"n": b64(key.N.Bytes()), "e": b64(big.NewInt(int64(key.E)).Bytes()),
		}}})
	})
	mux.HandleFunc("POST /token", f.token)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// approve signs a user in at the provider for authURL, as the browser
// would, and returns the state and code the provider redirects back with.
func (f *fakeIdP) approve(t *testing.T, authURL string, claims map[string]any) (state, code string) {
	t.Helper()
	u, err := url.Parse(authURL)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(authURL, f.srv.URL+"/authorize?"), authURL)
	q := u.Query()
	require.Equal(t, "S256", q.Get("code_challenge_method"))
	code = uuid.NewString()
	f.mu.Lock()
	f.codes[code] = fakeGrant{clientID: q.Get("client_id"), redirectURI: q.Get("redirect_uri"), challenge: q.Get("code_challenge"),
		nonce: q.Get("nonce"), claims: claims}
	f.mu.Unlock()
	return q.Get("state"), code
}

func (f *fakeIdP) token(w http.ResponseWriter, r *http.Request) {
	f.exchanges.Add(1)
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	g, ok := f.codes[r.PostForm.Get("code")]
	delete(f.codes, r.PostForm.Get("code"))
	f.mu.Unlock()
	sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
	if !ok || base64.RawURLEncoding.EncodeToString(sum[:]) != g.challenge || r.PostForm.Get("redirect_uri") != g.redirectURI {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
		return
	}
	now := time.Now()
	claims := map[string]any{"iss": f.srv.URL, "aud": g.clientID, "iat": now.Unix(), "exp": now.Add(time.Minute).Unix(), "nonce": g.nonce}
	for k, v := range g.claims {
		claims[k] = v
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "at", "token_type": "Bearer", "expires_in": 60, "id_token": f.sign(claims)})
}

func (f *fakeIdP) sign(claims map[string]any) string {
	b64 := base64.RawURLEncoding.EncodeToString
	payload, _ := json.Marshal(claims)
	signed := b64([]byte(`{"alg":"RS256","kid":"k1","typ":"JWT"}`)) + "." + b64(payload)
	sum := sha256.Sum256([]byte(signed))
	sig, err := rsa.SignPKCS1v15(rand.Reader, f.key, crypto.SHA256, sum[:])
	if err != nil {
		panic(err)
	}
	return signed + "." + b64(sig)
}

func cookieNamed(cs []*http.Cookie, name string) *http.Cookie {
	for _, c := range cs {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// TestOIDCBrowserBinding covers the browser binding of OIDC sign-in
// (security review finding 1): the state, nonce and PKCE verifier live on
// the server, so without the OIDCCookie a callback URL started in one
// browser could be finished in another. That allowed login CSRF, and a
// link URL forwarded to another user attached their identity, and their
// admin_groups and org_mappings, to the account that started the link.
func TestOIDCBrowserBinding(t *testing.T) {
	idp := newFakeIdP(t)
	newService := func(t *testing.T, publicURL string) *auth.Service {
		s, _ := authtest.NewService(t, dbtest.SQLite(t), authtest.RealOptions{Config: func(c *config.Config) {
			c.Server.PublicURL = publicURL
			c.OIDC.Providers = []config.OIDCProvider{{ID: "sso", Issuer: idp.srv.URL, ClientID: "hangar", ClientSecret: "secret",
				AutoCreateUsers: true, AdminGroups: []string{"admins"}}}
		}})
		return s
	}
	ctx := context.Background()
	victimClaims := map[string]any{"sub": "victim-sub", "preferred_username": "victim", "groups": []any{"admins"}}
	attackerClaims := map[string]any{"sub": "attacker-sub", "preferred_username": "attacker"}

	t.Run("service", func(t *testing.T) {
		s := newService(t, "http://hangar.test")
		flow, err := s.OIDC.Start(ctx, "sso", "/after", uuid.Nil)
		require.NoError(t, err)
		require.NotEmpty(t, flow.Binding)
		other, err := s.OIDC.Start(ctx, "sso", "/after", uuid.Nil)
		require.NoError(t, err)
		require.NotEqual(t, flow.Binding, other.Binding, "a new binding per sign-in")

		state, code := idp.approve(t, flow.AuthURL, attackerClaims)
		before := idp.exchanges.Load()
		for name, binding := range map[string]string{"no cookie": "", "another sign-in's cookie": other.Binding, "a made-up cookie": "x"} {
			_, err = s.OIDC.Callback(ctx, auth.OIDCCallback{Provider: "sso", State: state, Code: code, Binding: binding})
			require.ErrorIs(t, err, auth.ErrOIDCState, name)
		}
		require.Equal(t, before, idp.exchanges.Load(), "the code is never exchanged for the wrong browser")
		_, err = s.OIDC.Callback(ctx, auth.OIDCCallback{Provider: "sso", State: state, Code: code, Binding: flow.Binding})
		require.ErrorIs(t, err, auth.ErrOIDCState, "a refused callback uses up the state")

		flow, err = s.OIDC.Start(ctx, "sso", "/after", uuid.Nil)
		require.NoError(t, err)
		state, code = idp.approve(t, flow.AuthURL, attackerClaims)
		res, err := s.OIDC.Callback(ctx, auth.OIDCCallback{Provider: "sso", State: state, Code: code, Binding: flow.Binding})
		require.NoError(t, err)
		require.Equal(t, "attacker", res.User.Username)
		require.Equal(t, "/after", res.RedirectTo)

		// A link needs the session of the User who started it as well.
		alice := mustUser(t, s, "alice", false)
		bob := mustUser(t, s, "bob", false)
		for name, sessionUser := range map[string]uuid.UUID{"no session": uuid.Nil, "another User's session": bob.ID} {
			flow, err = s.OIDC.Start(ctx, "sso", "", alice.ID)
			require.NoError(t, err)
			state, code = idp.approve(t, flow.AuthURL, victimClaims)
			_, err = s.OIDC.Callback(ctx, auth.OIDCCallback{Provider: "sso", State: state, Code: code, Binding: flow.Binding, SessionUser: sessionUser})
			require.ErrorIs(t, err, auth.ErrOIDCState, name)
		}
		alice, err = s.Users.Get(ctx, alice.ID)
		require.NoError(t, err)
		require.Empty(t, alice.OIDCProviders)
		require.False(t, alice.IsAdmin)
		flow, err = s.OIDC.Start(ctx, "sso", "", alice.ID)
		require.NoError(t, err)
		state, code = idp.approve(t, flow.AuthURL, victimClaims)
		res, err = s.OIDC.Callback(ctx, auth.OIDCCallback{Provider: "sso", State: state, Code: code, Binding: flow.Binding, SessionUser: alice.ID})
		require.NoError(t, err)
		require.True(t, res.Linked)
		require.Equal(t, []string{"sso"}, res.User.OIDCProviders)
	})

	t.Run("HTTP", func(t *testing.T) {
		s := newService(t, "http://hangar.test")
		h := httpx.Authenticate(s, auth.WriteAuthError("http://hangar.test"))(s.Handler())
		call := func(method, target string, cookies []*http.Cookie, csrf string) *httptest.ResponseRecorder {
			req := httptest.NewRequest(method, "http://hangar.test"+target, nil)
			for _, c := range cookies {
				req.AddCookie(c)
			}
			if csrf != "" {
				req.Header.Set(auth.CSRFHeader, csrf)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req.WithContext(httpx.WithInfo(req.Context(), "req")))
			return rec
		}
		callback := func(state, code string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
			return call(http.MethodGet, "/auth/oidc/sso/callback?"+url.Values{"state": {state}, "code": {code}}.Encode(), cookies, "")
		}
		session := func(u *auth.User) (*http.Cookie, string) {
			sess, secret, err := s.Sessions.Create(ctx, u.ID, "", "")
			require.NoError(t, err)
			return &http.Cookie{Name: auth.SessionCookie, Value: secret}, sess.CSRFToken
		}

		// The start sets the binding cookie: HttpOnly, SameSite=Lax, only
		// for /auth/oidc/, for as long as the state lives.
		start := call(http.MethodGet, "/auth/oidc/sso?redirect=/alice/model", nil, "")
		require.Equal(t, http.StatusFound, start.Code)
		bind := cookieNamed(start.Result().Cookies(), auth.OIDCCookie)
		require.NotNil(t, bind)
		require.True(t, bind.HttpOnly)
		require.False(t, bind.Secure, "public_url is plain HTTP")
		require.Equal(t, http.SameSiteLaxMode, bind.SameSite)
		require.Equal(t, "/auth/oidc/", bind.Path)
		require.Equal(t, 600, bind.MaxAge)

		// Login CSRF: the attacker signs in at the provider as themself
		// and gets the victim's browser, which has no binding cookie or
		// its own, to open the callback URL.
		state, code := idp.approve(t, start.Header().Get("Location"), attackerClaims)
		victimStart := call(http.MethodGet, "/auth/oidc/sso", nil, "")
		victimBind := cookieNamed(victimStart.Result().Cookies(), auth.OIDCCookie)
		for name, cookies := range map[string][]*http.Cookie{"no cookie": nil, "the victim's own cookie": {victimBind}} {
			rec := callback(state, code, cookies...)
			require.Equal(t, "/auth/login?error=oidc_state", rec.Header().Get("Location"), name)
			require.Nil(t, cookieNamed(rec.Result().Cookies(), auth.SessionCookie), "%s: no session", name)
		}

		// The browser that started a sign-in finishes it, and the callback
		// clears the binding cookie.
		start = call(http.MethodGet, "/auth/oidc/sso?redirect=/alice/model", nil, "")
		bind = cookieNamed(start.Result().Cookies(), auth.OIDCCookie)
		state, code = idp.approve(t, start.Header().Get("Location"), attackerClaims)
		rec := callback(state, code, bind)
		require.Equal(t, "/alice/model", rec.Header().Get("Location"))
		require.NotNil(t, cookieNamed(rec.Result().Cookies(), auth.SessionCookie))
		cleared := cookieNamed(rec.Result().Cookies(), auth.OIDCCookie)
		require.NotNil(t, cleared)
		require.Empty(t, cleared.Value)
		require.Negative(t, cleared.MaxAge)
		require.Equal(t, "/auth/oidc/", cleared.Path)

		// Link forwarding: the attacker starts a link and sends the
		// authorize_url to a victim in an admin group, who signs in at the
		// provider. The victim's browser has no binding cookie; even with
		// one, the session must be the attacker's.
		attacker := mustUser(t, s, "mallory", false)
		victim := mustUser(t, s, "victor", false)
		attackerSession, attackerCSRF := session(attacker)
		victimSession, _ := session(victim)
		link := func() (string, *http.Cookie) {
			rec := call(http.MethodPost, "/auth/oidc/sso/link", []*http.Cookie{attackerSession}, attackerCSRF)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			var body struct {
				AuthorizeURL string `json:"authorize_url"`
			}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
			c := cookieNamed(rec.Result().Cookies(), auth.OIDCCookie)
			require.NotNil(t, c, "the link response binds the attacker's browser")
			return body.AuthorizeURL, c
		}
		for name, cookies := range map[string]func(*http.Cookie) []*http.Cookie{
			"victim's browser, signed in":     func(*http.Cookie) []*http.Cookie { return []*http.Cookie{victimSession} },
			"victim's browser, signed out":    func(*http.Cookie) []*http.Cookie { return nil },
			"binding, but the victim session": func(b *http.Cookie) []*http.Cookie { return []*http.Cookie{b, victimSession} },
			"binding, but no session":         func(b *http.Cookie) []*http.Cookie { return []*http.Cookie{b} },
		} {
			authURL, b := link()
			state, code := idp.approve(t, authURL, victimClaims)
			rec := callback(state, code, cookies(b)...)
			require.Equal(t, "/auth/login?error=oidc_state", rec.Header().Get("Location"), name)
		}
		got, err := s.Users.Get(ctx, attacker.ID)
		require.NoError(t, err)
		require.Empty(t, got.OIDCProviders, "the victim's identity is not linked to the attacker")
		require.False(t, got.IsAdmin, "the victim's admin group is not applied to the attacker")

		// The User who started the link finishes it in their own browser.
		authURL, b := link()
		state, code = idp.approve(t, authURL, victimClaims)
		rec = callback(state, code, b, attackerSession)
		require.Equal(t, "/settings?linked=sso", rec.Header().Get("Location"))
		got, err = s.Users.Get(ctx, attacker.ID)
		require.NoError(t, err)
		require.Equal(t, []string{"sso"}, got.OIDCProviders)
	})

	t.Run("Secure on HTTPS", func(t *testing.T) {
		s := newService(t, "https://hangar.test")
		h := httpx.Authenticate(s, auth.WriteAuthError("https://hangar.test"))(s.Handler())
		req := httptest.NewRequest(http.MethodGet, "https://hangar.test/auth/oidc/sso", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req.WithContext(httpx.WithInfo(req.Context(), "req")))
		bind := cookieNamed(rec.Result().Cookies(), auth.OIDCCookie)
		require.NotNil(t, bind)
		require.True(t, bind.Secure)
		require.True(t, bind.HttpOnly)
	})
}

