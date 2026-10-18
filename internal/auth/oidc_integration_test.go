//go:build integration

package auth_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/hangar-hub/hangar/internal/audit"
	"github.com/hangar-hub/hangar/internal/audit/audittest"
	"github.com/hangar-hub/hangar/internal/auth"
	"github.com/hangar-hub/hangar/internal/auth/authtest"
	"github.com/hangar-hub/hangar/internal/config"
	"github.com/hangar-hub/hangar/internal/db"
	"github.com/hangar-hub/hangar/internal/db/dbtest"
	"github.com/hangar-hub/hangar/internal/httpx"
	"github.com/hangar-hub/hangar/internal/model"
	"github.com/hangar-hub/hangar/internal/org"
)

// keycloakImage is the Keycloak release the OIDC test runs against.
const keycloakImage = "quay.io/keycloak/keycloak:26.4.0"

const kcClientSecret = "hangar-test-secret"

// kcRealm is imported at start: a confidential client that requires PKCE
// (S256), a groups claim, and test users.
const kcRealm = `{
  "realm": "hangar",
  "enabled": true,
  "groups": [{"name": "ml-team"}, {"name": "hangar-admins"}],
  "clients": [{
    "clientId": "hangar",
    "enabled": true,
    "publicClient": false,
    "secret": "` + kcClientSecret + `",
    "standardFlowEnabled": true,
    "directAccessGrantsEnabled": false,
    "redirectUris": ["http://hangar.test/*"],
    "attributes": {"pkce.code.challenge.method": "S256"},
    "protocolMappers": [{
      "name": "groups",
      "protocol": "openid-connect",
      "protocolMapper": "oidc-group-membership-mapper",
      "config": {"full.path": "false", "id.token.claim": "true", "access.token.claim": "true", "userinfo.token.claim": "true", "claim.name": "groups"}
    }]
  }],
  "users": [
    {"username": "kc-alice", "enabled": true, "email": "alice@example.com", "emailVerified": true, "firstName": "Alice", "lastName": "Kay",
     "credentials": [{"type": "password", "value": "alice-password-1", "temporary": false}], "groups": ["/ml-team", "/hangar-admins"]},
    {"username": "kc-bob", "enabled": true, "email": "bob@example.com", "emailVerified": true, "firstName": "Bob", "lastName": "Kay",
     "credentials": [{"type": "password", "value": "bob-password-12", "temporary": false}]},
    {"username": "kc-carol", "enabled": true, "email": "carol@example.com", "emailVerified": true, "firstName": "Carol", "lastName": "Kay",
     "credentials": [{"type": "password", "value": "carol-password-1", "temporary": false}]}
  ]
}`

type keycloak struct {
	base string // http://host:port
}

func (k keycloak) issuer() string { return k.base + "/realms/hangar" }

func startKeycloak(t *testing.T) keycloak {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        keycloakImage,
			Cmd:          []string{"start-dev", "--import-realm"},
			ExposedPorts: []string{"8080/tcp"},
			Env: map[string]string{
				"KC_BOOTSTRAP_ADMIN_USERNAME": "admin",
				"KC_BOOTSTRAP_ADMIN_PASSWORD": "admin",
				"JAVA_OPTS_KC_HEAP":           "-Xms64m -Xmx512m",
			},
			Files: []testcontainers.ContainerFile{{
				Reader: strings.NewReader(kcRealm), ContainerFilePath: "/opt/keycloak/data/import/hangar-realm.json", FileMode: 0o644,
			}},
			WaitingFor: wait.ForHTTP("/realms/hangar/.well-known/openid-configuration").WithPort("8080/tcp").
				WithStartupTimeout(4 * time.Minute),
		},
		Started: true,
	})
	require.NoError(t, err, "start Keycloak")
	t.Cleanup(func() { _ = testcontainers.TerminateContainer(c) })
	host, err := c.Host(ctx)
	require.NoError(t, err)
	port, err := c.MappedPort(ctx, "8080/tcp")
	require.NoError(t, err)
	return keycloak{base: fmt.Sprintf("http://%s:%s", host, port.Port())}
}

var formAction = regexp.MustCompile(`<form[^>]*id="kc-form-login"[^>]*action="([^"]+)"`)

// browserLogin follows authURL like a browser, signs in at Keycloak and
// returns the state and code from the redirect to Hangar's callback.
func browserLogin(t *testing.T, authURL, username, password string) (state, code string) {
	t.Helper()
	jar, err := cookiejar.New(nil)
	require.NoError(t, err)
	var callback *url.URL
	client := &http.Client{Jar: jar, Timeout: 30 * time.Second, CheckRedirect: func(req *http.Request, _ []*http.Request) error {
		if req.URL.Host == "hangar.test" {
			callback = req.URL
			return http.ErrUseLastResponse
		}
		return nil
	}}
	resp, err := client.Get(authURL)
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	m := formAction.FindSubmatch(body)
	require.NotNil(t, m, "Keycloak login form: %s", body)
	resp, err = client.PostForm(html.UnescapeString(string(m[1])), url.Values{"username": {username}, "password": {password}, "credentialId": {""}})
	require.NoError(t, err)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	require.NotNil(t, callback, "expected a redirect to Hangar, got %d: %s", resp.StatusCode, body)
	require.Equal(t, "/auth/oidc/kc/callback", callback.Path)
	return callback.Query().Get("state"), callback.Query().Get("code")
}

// kcGroup adds a Keycloak user to a group, or removes them, through the
// admin API.
func kcGroup(t *testing.T, k keycloak, username, group string, add bool) {
	t.Helper()
	resp, err := http.PostForm(k.base+"/realms/master/protocol/openid-connect/token",
		url.Values{"grant_type": {"password"}, "client_id": {"admin-cli"}, "username": {"admin"}, "password": {"admin"}})
	require.NoError(t, err)
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&tok))
	resp.Body.Close()
	call := func(method, path string, v any) {
		req, _ := http.NewRequest(method, k.base+"/admin/realms/hangar"+path, nil)
		req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Less(t, resp.StatusCode, 300, "%s %s", method, path)
		if v != nil {
			require.NoError(t, json.NewDecoder(resp.Body).Decode(v))
		}
	}
	var users []struct{ ID string }
	call(http.MethodGet, "/users?exact=true&username="+url.QueryEscape(username), &users)
	require.Len(t, users, 1)
	var groups []struct{ ID, Name string }
	call(http.MethodGet, "/groups", &groups)
	for _, g := range groups {
		if g.Name == group {
			method := http.MethodDelete
			if add {
				method = http.MethodPut
			}
			call(method, "/users/"+users[0].ID+"/groups/"+g.ID, nil)
			return
		}
	}
	t.Fatalf("no group %s", group)
}

func TestOIDCKeycloak(t *testing.T) {
	kc := startKeycloak(t)
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		ctx := context.Background()
		rec := audittest.NewFake()
		s, orgs := authtest.NewService(t, d, authtest.RealOptions{Audit: rec, Config: func(c *config.Config) {
			provider := config.OIDCProvider{
				ID: "kc", Name: "Keycloak", Issuer: kc.issuer(), ClientID: "hangar", ClientSecret: kcClientSecret,
				Scopes: []string{"openid", "profile", "email"}, AdminGroups: []string{"hangar-admins"},
				OrgMappings:     []config.OrgMapping{{Group: "ml-team", Org: "ml", Role: "write"}},
				AutoCreateUsers: true,
			}
			c.OIDC.Providers = []config.OIDCProvider{provider}
		}})
		require.Equal(t, []auth.ProviderInfo{{ID: "kc", Name: "Keycloak", Issuer: kc.issuer(), LoginURL: "/auth/oidc/kc"}}, s.OIDC.Providers())
		require.Equal(t, "http://hangar.test/auth/oidc/kc/callback", s.OIDC.CallbackURL("kc"))
		// A local admin, so admin_groups can demote OIDC users later.
		mustUser(t, s, "root", true)
		// A local user with the same verified email as kc-carol.
		carol, err := s.Users.Create(ctx, auth.NewUser{Username: "carol", Email: "carol@example.com", EmailVerified: true}, model.System())
		require.NoError(t, err)
		// A local user holding kc-bob's preferred username.
		mustUser(t, s, "kc-bob", false)

		login := func(username, password string, link uuid.UUID) (*auth.CallbackResult, error) {
			authURL, err := s.OIDC.Start(ctx, "kc", "/after?x=1", link)
			require.NoError(t, err)
			u, err := url.Parse(authURL)
			require.NoError(t, err)
			q := u.Query()
			require.Equal(t, "S256", q.Get("code_challenge_method"), "PKCE")
			require.NotEmpty(t, q.Get("nonce"))
			require.Equal(t, "http://hangar.test/auth/oidc/kc/callback", q.Get("redirect_uri"))
			state, code := browserLogin(t, authURL, username, password)
			return s.OIDC.Callback(ctx, "kc", state, code, "10.0.0.5", "browser")
		}

		// First login creates the User just in time, with admin_groups and
		// org_mappings applied.
		res, err := login("kc-alice", "alice-password-1", uuid.Nil)
		require.NoError(t, err)
		alice := res.User
		require.Equal(t, "kc-alice", alice.Username)
		require.Equal(t, "alice@example.com", alice.Email)
		require.Equal(t, "Alice Kay", alice.DisplayName)
		require.True(t, alice.IsAdmin, "in hangar-admins")
		require.Equal(t, []string{"kc"}, alice.OIDCProviders)
		require.Equal(t, "/after?x=1", res.RedirectTo)
		m, err := orgs.Member(ctx, "ml", "kc-alice")
		require.NoError(t, err)
		require.Equal(t, model.RoleWrite, m.Role)
		require.Equal(t, org.SourceOIDC, m.Source)
		require.Contains(t, rec.Actions(), audit.LoginSucceeded)

		// Second login finds the same User; group changes are re-evaluated.
		kcGroup(t, kc, "kc-alice", "hangar-admins", false)
		kcGroup(t, kc, "kc-alice", "ml-team", false)
		t.Cleanup(func() {
			kcGroup(t, kc, "kc-alice", "hangar-admins", true)
			kcGroup(t, kc, "kc-alice", "ml-team", true)
		})
		res, err = login("kc-alice", "alice-password-1", uuid.Nil)
		require.NoError(t, err)
		require.Equal(t, alice.ID, res.User.ID)
		require.False(t, res.User.IsAdmin, "admin_groups are checked at every login")
		_, err = orgs.Member(ctx, "ml", "kc-alice")
		require.ErrorIs(t, err, org.ErrNotFound, "mapped Memberships go when the group goes")

		// A taken username gets a numeric suffix.
		res, err = login("kc-bob", "bob-password-12", uuid.Nil)
		require.NoError(t, err)
		require.Equal(t, "kc-bob-1", res.User.Username)

		// A local account with the same verified email is never linked
		// automatically; its owner links it after signing in.
		_, err = login("kc-carol", "carol-password-1", uuid.Nil)
		require.ErrorIs(t, err, auth.ErrOIDCLinkRequired)
		require.Equal(t, "oidc_link_required", auth.OIDCErrorCode(err))
		res, err = login("kc-carol", "carol-password-1", carol.ID)
		require.NoError(t, err)
		require.True(t, res.Linked)
		require.Equal(t, carol.ID, res.User.ID)
		res, err = login("kc-carol", "carol-password-1", uuid.Nil)
		require.NoError(t, err)
		require.Equal(t, carol.ID, res.User.ID, "signs in to the linked account")
		require.Contains(t, rec.Actions(), audit.IdentityLinked)
		// The identity cannot be linked to another account.
		_, err = login("kc-carol", "carol-password-1", alice.ID)
		require.ErrorIs(t, err, auth.ErrOIDCIdentityTaken)

		// State is single use and bound to the provider.
		authURL, err := s.OIDC.Start(ctx, "kc", "", uuid.Nil)
		require.NoError(t, err)
		state, code := browserLogin(t, authURL, "kc-alice", "alice-password-1")
		_, err = s.OIDC.Callback(ctx, "kc", state, code, "", "")
		require.NoError(t, err)
		_, err = s.OIDC.Callback(ctx, "kc", state, code, "", "")
		require.ErrorIs(t, err, auth.ErrOIDCState)
		_, err = s.OIDC.Callback(ctx, "kc", "made-up", code, "", "")
		require.ErrorIs(t, err, auth.ErrOIDCState)
		_, err = s.OIDC.Start(ctx, "nope", "", uuid.Nil)
		require.ErrorIs(t, err, auth.ErrOIDCUnknownProvider)

		// A code from a flow without the PKCE verifier is refused by
		// Keycloak: a tampered code fails the exchange.
		authURL, err = s.OIDC.Start(ctx, "kc", "", uuid.Nil)
		require.NoError(t, err)
		state, _ = browserLogin(t, authURL, "kc-alice", "alice-password-1")
		_, err = s.OIDC.Callback(ctx, "kc", state, "tampered-code", "", "")
		require.Error(t, err)
		require.Equal(t, audit.LoginFailed, rec.Actions()[len(rec.Actions())-1])

		// Disabled users cannot sign in with OIDC either.
		yes := true
		_, err = s.Users.Update(ctx, res.User.ID, auth.UserUpdate{Disabled: &yes}, model.System())
		require.NoError(t, err)
		_, err = login("kc-carol", "carol-password-1", uuid.Nil)
		require.ErrorIs(t, err, auth.ErrDisabled)
	})

	t.Run("through the HTTP handler", func(t *testing.T) {
		s, _ := authtest.NewService(t, dbtest.SQLite(t), authtest.RealOptions{Config: func(c *config.Config) {
			c.OIDC.Providers = []config.OIDCProvider{{ID: "kc", Issuer: kc.issuer(), ClientID: "hangar", ClientSecret: kcClientSecret,
				Scopes: []string{"openid", "profile", "email"}, AutoCreateUsers: true}}
		}})
		h := httpx.Authenticate(s, auth.WriteAuthError("http://hangar.test"))(s.Handler())
		serve := func(target string) *httptest.ResponseRecorder {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, target, nil)
			h.ServeHTTP(rec, req.WithContext(httpx.WithInfo(req.Context(), "r")))
			return rec
		}
		start := serve("http://hangar.test/auth/oidc/kc?redirect=/bob/model")
		require.Equal(t, http.StatusFound, start.Code)
		state, code := browserLogin(t, start.Header().Get("Location"), "kc-bob", "bob-password-12")
		done := serve("http://hangar.test/auth/oidc/kc/callback?" + url.Values{"state": {state}, "code": {code}}.Encode())
		require.Equal(t, http.StatusFound, done.Code)
		require.Equal(t, "/bob/model", done.Header().Get("Location"))
		var session *http.Cookie
		for _, c := range done.Result().Cookies() {
			if c.Name == auth.SessionCookie {
				session = c
			}
		}
		require.NotNil(t, session, "signing in sets the session cookie")
		req := httptest.NewRequest(http.MethodGet, "http://hangar.test/api/hangar/v1/me", nil)
		req.AddCookie(session)
		p, err := s.Authenticate(req)
		require.NoError(t, err)
		require.Equal(t, "kc-bob", p.Username)
	})

	t.Run("no auto-create", func(t *testing.T) {
		s, _ := authtest.NewService(t, dbtest.SQLite(t), authtest.RealOptions{Config: func(c *config.Config) {
			c.OIDC.Providers = []config.OIDCProvider{{ID: "kc", Issuer: kc.issuer(), ClientID: "hangar", ClientSecret: kcClientSecret}}
		}})
		authURL, err := s.OIDC.Start(context.Background(), "kc", "https://evil.example/", uuid.Nil)
		require.NoError(t, err)
		state, code := browserLogin(t, authURL, "kc-bob", "bob-password-12")
		_, err = s.OIDC.Callback(context.Background(), "kc", state, code, "", "")
		require.True(t, errors.Is(err, auth.ErrOIDCNoAccount), "%v", err)
	})
}
