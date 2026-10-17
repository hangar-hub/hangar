package api_test

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/api"
	"github.com/jamesagarside/hangar/internal/api/gen"
	"github.com/jamesagarside/hangar/internal/audit"
	"github.com/jamesagarside/hangar/internal/auth"
	"github.com/jamesagarside/hangar/internal/auth/authtest"
	"github.com/jamesagarside/hangar/internal/config"
	"github.com/jamesagarside/hangar/internal/db"
	"github.com/jamesagarside/hangar/internal/db/dbtest"
	"github.com/jamesagarside/hangar/internal/httpx"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/org"
	"github.com/jamesagarside/hangar/internal/repo"
)

const password = "correct horse battery staple 42"

// dbRepos reads repos rows, which the tests insert directly; in
// production repo.Service owns them.
type dbRepos struct {
	d    *db.DB
	orgs *org.DBService
}

func (r dbRepos) Get(ctx context.Context, id model.RepoID) (*repo.Repo, error) {
	ns, err := r.orgs.Get(ctx, id.Namespace)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", repo.ErrNotFound, id)
	}
	var row db.Repo
	err = r.d.NewSelect().Model(&row).Where("namespace_id = ? AND lower(name) = lower(?) AND type = ? AND deleted_at IS NULL", ns.ID, id.Name, string(id.Type)).Scan(ctx)
	if db.IsNotFound(err) {
		return nil, fmt.Errorf("%w: %s", repo.ErrNotFound, id)
	}
	if err != nil {
		return nil, err
	}
	g := repo.Gating{}
	if m, ok := row.Gating["mode"].(string); ok {
		g.Mode = m
	}
	if fs, ok := row.Gating["fields"].([]any); ok {
		for _, f := range fs {
			if fm, ok := f.(map[string]any); ok {
				req, _ := fm["required"].(bool)
				g.Fields = append(g.Fields, repo.GatingField{Name: fmt.Sprint(fm["name"]), Type: fmt.Sprint(fm["type"]), Required: req})
			}
		}
	}
	return &repo.Repo{UUID: row.ID, ID: model.RepoID{Type: id.Type, Namespace: ns.Name, Name: row.Name}, NamespaceID: ns.ID,
		Visibility: model.Visibility(row.Visibility), Gating: g}, nil
}

// server combines Identity with gen.Unimplemented for everything else, as
// the integration wiring will.
type server struct {
	*api.Identity
	fallback
}

type fallback struct{ gen.Unimplemented }

type env struct {
	t    *testing.T
	d    *db.DB
	svc  *auth.Service
	orgs *org.DBService
	srv  *httptest.Server
}

func newEnv(t *testing.T, d *db.DB) *env { return newEnvWith(t, d, nil) }

func newEnvWith(t *testing.T, d *db.DB, upstreams auth.UpstreamLookup) *env {
	t.Helper()
	rec := audit.NewDB(d, nil)
	svc, orgs := authtest.NewService(t, d, authtest.RealOptions{Audit: rec, Upstreams: upstreams, Config: func(c *config.Config) {
		c.Server.PublicURL = "http://hangar.test"
		c.OIDC.Providers = []config.OIDCProvider{{ID: "sso", Name: "Company SSO", Issuer: "https://sso.example.com", ClientID: "hangar"}}
	}})
	id := api.NewIdentity(api.IdentityOptions{Auth: svc, Orgs: orgs, Audit: rec, Repos: dbRepos{d: d, orgs: orgs}})
	native := gen.HandlerWithOptions(server{Identity: id}, gen.ChiServerOptions{
		BaseURL: "/api/hangar/v1",
		Middlewares: []gen.MiddlewareFunc{func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if rc := chi.RouteContext(r.Context()); rc != nil {
					httpx.SetRoute(r.Context(), rc.RoutePattern())
				}
				next.ServeHTTP(w, r)
			})
		}},
		ErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			httpx.WriteProblem(w, r, &httpx.Problem{Status: http.StatusBadRequest, Detail: err.Error()})
		},
	})
	h := httpx.Dispatch(httpx.Protocols{Native: native, Auth: svc.Handler()}, "http://hangar.test")
	h = httpx.Authenticate(svc, auth.WriteAuthError("http://hangar.test"))(h)
	h = httpx.RequestIDs(h)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &env{t: t, d: d, svc: svc, orgs: orgs, srv: srv}
}

// client is a browser-like or token client.
type client struct {
	e      *env
	http   *http.Client
	csrf   string
	bearer string
}

func (e *env) anon() *client {
	jar, _ := cookiejar.New(nil)
	return &client{e: e, http: &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

func (e *env) user(name string, admin bool) *auth.User {
	u, err := e.svc.Users.Create(context.Background(), auth.NewUser{Username: name, Password: password, IsAdmin: admin}, model.System())
	require.NoError(e.t, err)
	return u
}

// login signs in through the API and returns a session client.
func (e *env) login(name string) *client {
	c := e.anon()
	var me auth.MeView
	c.do(http.MethodPost, "/api/hangar/v1/auth/login", map[string]string{"username": name, "password": password}, http.StatusOK, &me)
	require.NotEmpty(e.t, me.CSRFToken)
	c.csrf = me.CSRFToken
	return c
}

func (c *client) do(method, path string, body any, want int, out any) *http.Response {
	c.e.t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		require.NoError(c.e.t, err)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.e.srv.URL+path, rd)
	require.NoError(c.e.t, err)
	req.Header.Set("Content-Type", "application/json")
	if c.csrf != "" {
		req.Header.Set(auth.CSRFHeader, c.csrf)
	}
	if c.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+c.bearer)
	}
	resp, err := c.http.Do(req)
	require.NoError(c.e.t, err)
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	require.Equal(c.e.t, want, resp.StatusCode, "%s %s: %s", method, path, raw)
	if want >= 400 {
		require.Contains(c.e.t, resp.Header.Get("Content-Type"), "problem+json", "%s %s", method, path)
	}
	if out != nil {
		require.NoError(c.e.t, json.Unmarshal(raw, out), string(raw))
	}
	return resp
}

func insertRepo(t *testing.T, d *db.DB, nsID uuid.UUID, name string, vis model.Visibility, gating map[string]any) {
	t.Helper()
	if gating == nil {
		gating = map[string]any{}
	}
	_, err := d.NewInsert().Model(&db.Repo{ID: db.NewID(), NamespaceID: nsID, Name: name, Type: "model", Visibility: string(vis),
		DefaultBranch: "main", HeadRef: "refs/heads/main", Tags: []string{}, Settings: map[string]any{}, Gating: gating}).Exec(context.Background())
	require.NoError(t, err)
}

func TestSessionsTokensAndKeys(t *testing.T) {
	t.Parallel()
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		e := newEnv(t, d)
		e.user("alice", false)
		anon := e.anon()

		// Nobody signed in is not an error for /me (bellatrix finding 6),
		// so the UI's check before sign-in leaves the console clean.
		anon.do(http.MethodGet, "/api/hangar/v1/me", nil, http.StatusNoContent, nil)
		anon.do(http.MethodGet, "/api/hangar/v1/me/tokens", nil, http.StatusUnauthorized, nil)
		var providers gen.AuthProviders
		anon.do(http.MethodGet, "/api/hangar/v1/auth/providers", nil, http.StatusOK, &providers)
		require.True(t, providers.LocalLogin)
		require.Equal(t, gen.AuthProvidersRegistrationClosed, providers.Registration)
		require.Len(t, providers.Providers, 1)
		require.Equal(t, "/auth/oidc/sso", providers.Providers[0].LoginUrl)
		require.True(t, providers.UiExternalImages, "ui.external_images is on by default and readable by everyone")

		anon.do(http.MethodPost, "/api/hangar/v1/auth/login", map[string]string{"username": "alice", "password": "wrong-password-1"}, http.StatusUnauthorized, nil)
		anon.do(http.MethodPost, "/api/hangar/v1/auth/login", "not an object", http.StatusBadRequest, nil)
		alice := e.login("alice")
		var me gen.Me
		alice.do(http.MethodGet, "/api/hangar/v1/me", nil, http.StatusOK, &me)
		require.Equal(t, "alice", me.Username)
		require.NotNil(t, me.CsrfToken)
		require.Empty(t, me.Orgs)

		// CSRF is enforced on state-changing calls.
		csrf := alice.csrf
		alice.csrf = ""
		alice.do(http.MethodPost, "/api/hangar/v1/me/tokens", map[string]any{"name": "x", "scope": "read"}, http.StatusForbidden, nil)
		alice.csrf = csrf

		var created gen.TokenCreated
		alice.do(http.MethodPost, "/api/hangar/v1/me/tokens", map[string]any{"name": "laptop", "scope": "write",
			"restrictions": map[string]any{"namespaces": []string{"alice"}}}, http.StatusCreated, &created)
		require.True(t, strings.HasPrefix(created.Secret, "hgr_"))
		require.Equal(t, gen.Scope("write"), created.Scope)
		require.Equal(t, []string{"alice"}, *created.Restrictions.Namespaces)
		alice.do(http.MethodPost, "/api/hangar/v1/me/tokens", map[string]any{"name": "", "scope": "read"}, http.StatusUnprocessableEntity, nil)

		tok := &client{e: e, http: http.DefaultClient, bearer: created.Secret}
		var tokMe gen.Me
		tok.do(http.MethodGet, "/api/hangar/v1/me", nil, http.StatusOK, &tokMe)
		require.Nil(t, tokMe.CsrfToken, "token callers get no CSRF token")
		tok.do(http.MethodPost, "/api/hangar/v1/me/tokens", map[string]any{"name": "child", "scope": "read"}, http.StatusForbidden, nil)
		var tokens gen.TokenList
		alice.do(http.MethodGet, "/api/hangar/v1/me/tokens", nil, http.StatusOK, &tokens)
		require.Len(t, tokens.Items, 1)
		require.Equal(t, created.Prefix, tokens.Items[0].Prefix)
		raw, _ := json.Marshal(tokens)
		require.NotContains(t, string(raw), created.Secret, "the secret is shown once")

		var unrestricted gen.TokenCreated
		alice.do(http.MethodPost, "/api/hangar/v1/me/tokens", map[string]any{"name": "ci", "scope": "write"}, http.StatusCreated, &unrestricted)
		tok2 := &client{e: e, http: http.DefaultClient, bearer: unrestricted.Secret}
		tok2.do(http.MethodPost, "/api/hangar/v1/me/tokens", map[string]any{"name": "wider", "scope": "admin"}, http.StatusForbidden, nil)
		tok2.do(http.MethodPost, "/api/hangar/v1/me/tokens", map[string]any{"name": "narrower", "scope": "read"}, http.StatusCreated, nil)
		readTok := &client{e: e, http: http.DefaultClient}
		var rt gen.TokenCreated
		alice.do(http.MethodPost, "/api/hangar/v1/me/tokens", map[string]any{"name": "ro", "scope": "read"}, http.StatusCreated, &rt)
		readTok.bearer = rt.Secret
		readTok.do(http.MethodPost, "/api/hangar/v1/me/ollama-keys", map[string]any{"public_key": "x"}, http.StatusForbidden, nil)

		alice.do(http.MethodDelete, "/api/hangar/v1/me/tokens/"+created.Id.String(), nil, http.StatusNoContent, nil)
		tok.do(http.MethodGet, "/api/hangar/v1/me", nil, http.StatusUnauthorized, nil)
		alice.do(http.MethodDelete, "/api/hangar/v1/me/tokens/"+uuid.NewString(), nil, http.StatusNotFound, nil)
		alice.do(http.MethodDelete, "/api/hangar/v1/me/tokens/not-a-uuid", nil, http.StatusBadRequest, nil)

		// Sessions.
		other := e.login("alice")
		var sessions gen.SessionList
		alice.do(http.MethodGet, "/api/hangar/v1/me/sessions", nil, http.StatusOK, &sessions)
		require.Len(t, sessions.Items, 2)
		var otherID uuid.UUID
		for _, s := range sessions.Items {
			if !s.Current {
				otherID = s.Id
			}
		}
		alice.do(http.MethodDelete, "/api/hangar/v1/me/sessions/"+otherID.String(), nil, http.StatusNoContent, nil)
		// The revoked session is signed out.
		other.do(http.MethodGet, "/api/hangar/v1/me", nil, http.StatusNoContent, nil)
		other.do(http.MethodGet, "/api/hangar/v1/me/sessions", nil, http.StatusUnauthorized, nil)

		// Ollama keys.
		key := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIK0wmN/Cr3JXqmLW7u+g9pTh+wyqDHpSQEIQczXkVx9q me@laptop"
		var k gen.OllamaKey
		alice.do(http.MethodPost, "/api/hangar/v1/me/ollama-keys", map[string]any{"public_key": key}, http.StatusCreated, &k)
		require.Equal(t, "me@laptop", *k.Name)
		alice.do(http.MethodPost, "/api/hangar/v1/me/ollama-keys", map[string]any{"public_key": key}, http.StatusConflict, nil)
		alice.do(http.MethodPost, "/api/hangar/v1/me/ollama-keys", map[string]any{"public_key": "nope"}, http.StatusUnprocessableEntity, nil)
		var keys gen.OllamaKeyList
		alice.do(http.MethodGet, "/api/hangar/v1/me/ollama-keys", nil, http.StatusOK, &keys)
		require.Len(t, keys.Items, 1)
		alice.do(http.MethodDelete, "/api/hangar/v1/me/ollama-keys/"+k.Id.String(), nil, http.StatusNoContent, nil)

		// Upstream tokens: no Upstream lookup is wired in this test, so
		// every name is unknown.
		alice.do(http.MethodGet, "/api/hangar/v1/me/upstream-tokens/huggingface", nil, http.StatusNotFound, nil)
		alice.do(http.MethodPut, "/api/hangar/v1/me/upstream-tokens/huggingface", map[string]any{"token": "hf_x"}, http.StatusNotFound, nil)

		// Device login answers.
		dc, err := e.svc.Device.Start(context.Background(), "hf")
		require.NoError(t, err)
		alice.do(http.MethodPost, "/api/hangar/v1/me/device", map[string]any{"user_code": dc.UserCode, "approve": true}, http.StatusNoContent, nil)
		alice.do(http.MethodPost, "/api/hangar/v1/me/device", map[string]any{"user_code": dc.UserCode, "approve": false}, http.StatusNotFound, nil)
		anon.do(http.MethodPost, "/api/hangar/v1/me/device", map[string]any{"user_code": dc.UserCode, "approve": true}, http.StatusUnauthorized, nil)

		// Logout ends the session.
		alice.do(http.MethodPost, "/api/hangar/v1/auth/logout", nil, http.StatusNoContent, nil)
		alice.csrf = ""
		alice.do(http.MethodGet, "/api/hangar/v1/me", nil, http.StatusNoContent, nil)
		alice.do(http.MethodGet, "/api/hangar/v1/me/sessions", nil, http.StatusUnauthorized, nil)
	})
}

func TestOrganisationEndpoints(t *testing.T) {
	t.Parallel()
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		e := newEnv(t, d)
		e.user("alice", false)
		e.user("bob", false)
		e.user("carol", false)
		alice, bob, anon := e.login("alice"), e.login("bob"), e.anon()

		anon.do(http.MethodPost, "/api/hangar/v1/orgs", map[string]any{"name": "acme"}, http.StatusUnauthorized, nil)
		var ns gen.Namespace
		alice.do(http.MethodPost, "/api/hangar/v1/orgs", map[string]any{"name": "acme", "display_name": "Acme", "description": "Rockets."}, http.StatusCreated, &ns)
		require.Equal(t, gen.NamespaceKindOrg, ns.Kind)
		require.Equal(t, "Rockets.", *ns.Description)
		alice.do(http.MethodPost, "/api/hangar/v1/orgs", map[string]any{"name": "acme"}, http.StatusConflict, nil)
		alice.do(http.MethodPost, "/api/hangar/v1/orgs", map[string]any{"name": "api"}, http.StatusUnprocessableEntity, nil)

		var m gen.Member
		alice.do(http.MethodPut, "/api/hangar/v1/orgs/acme/members/bob", map[string]any{"role": "write"}, http.StatusOK, &m)
		require.Equal(t, gen.Role("write"), m.Role)
		require.Equal(t, gen.MemberSourceManual, m.Source)
		bob.do(http.MethodPut, "/api/hangar/v1/orgs/acme/members/carol", map[string]any{"role": "read"}, http.StatusForbidden, nil)
		anon.do(http.MethodGet, "/api/hangar/v1/orgs/acme/members/bob", nil, http.StatusOK, &m)
		require.Equal(t, "bob", m.Username)
		anon.do(http.MethodGet, "/api/hangar/v1/orgs/acme/members/carol", nil, http.StatusNotFound, nil)
		anon.do(http.MethodGet, "/api/hangar/v1/orgs/nope/members", nil, http.StatusNotFound, nil)
		anon.do(http.MethodGet, "/api/hangar/v1/orgs/alice/members", nil, http.StatusNotFound, nil)
		var list gen.MemberList
		anon.do(http.MethodGet, "/api/hangar/v1/orgs/acme/members", nil, http.StatusOK, &list)
		require.Len(t, list.Items, 2)
		alice.do(http.MethodPut, "/api/hangar/v1/orgs/acme/members/nobody", map[string]any{"role": "read"}, http.StatusNotFound, nil)
		alice.do(http.MethodPut, "/api/hangar/v1/orgs/acme/members/bob", map[string]any{"role": "owner"}, http.StatusUnprocessableEntity, nil)
		alice.do(http.MethodPut, "/api/hangar/v1/orgs/acme/members/alice", map[string]any{"role": "read"}, http.StatusConflict, nil)

		var me gen.Me
		bob.do(http.MethodGet, "/api/hangar/v1/me", nil, http.StatusOK, &me)
		require.Equal(t, "acme", me.Orgs[0].Name)

		bob.do(http.MethodPatch, "/api/hangar/v1/orgs/acme", map[string]any{"display_name": "Nope"}, http.StatusForbidden, nil)
		alice.do(http.MethodPatch, "/api/hangar/v1/orgs/acme", map[string]any{"display_name": "Acme Ltd"}, http.StatusOK, &ns)
		require.Equal(t, "Acme Ltd", ns.DisplayName)
		require.Equal(t, 2, *ns.MemberCount)

		bob.do(http.MethodDelete, "/api/hangar/v1/orgs/acme/members/bob", nil, http.StatusNoContent, nil)
		bob.do(http.MethodDelete, "/api/hangar/v1/orgs/acme/members/alice", nil, http.StatusForbidden, nil)
		alice.do(http.MethodDelete, "/api/hangar/v1/orgs/acme/members/alice", nil, http.StatusConflict, nil)

		acme, err := e.orgs.Get(context.Background(), "acme")
		require.NoError(t, err)
		insertRepo(t, d, acme.ID, "rocket", model.VisibilityPublic, nil)
		alice.do(http.MethodDelete, "/api/hangar/v1/orgs/acme", nil, http.StatusConflict, nil)
		_, err = d.NewDelete().Model((*db.Repo)(nil)).Where("namespace_id = ?", acme.ID).Exec(context.Background())
		require.NoError(t, err)
		bob.do(http.MethodDelete, "/api/hangar/v1/orgs/acme", nil, http.StatusForbidden, nil)
		alice.do(http.MethodDelete, "/api/hangar/v1/orgs/acme", nil, http.StatusNoContent, nil)
	})
}

func TestRepoAccessEndpoints(t *testing.T) {
	t.Parallel()
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		ctx := context.Background()
		e := newEnv(t, d)
		owner := e.user("owner", false)
		e.user("bob", false)
		e.user("carol", false)
		o, bob, carol, anon := e.login("owner"), e.login("bob"), e.login("carol"), e.anon()
		insertRepo(t, d, owner.NamespaceID, "gated", model.VisibilityPublic, map[string]any{"mode": "manual",
			"fields": []any{map[string]any{"name": "company", "type": "text", "required": true}}})
		insertRepo(t, d, owner.NamespaceID, "secret", model.VisibilityPrivate, nil)
		base := "/api/hangar/v1/repos/models/owner/"

		anon.do(http.MethodPost, base+"gated/access-requests", map[string]any{}, http.StatusUnauthorized, nil)
		bob.do(http.MethodPost, base+"gated/access-requests", map[string]any{"fields": map[string]any{}}, http.StatusUnprocessableEntity, nil)
		var ar gen.AccessRequest
		bob.do(http.MethodPost, base+"gated/access-requests", map[string]any{"fields": map[string]any{"company": "Acme"}}, http.StatusCreated, &ar)
		require.Equal(t, gen.AccessRequestStatusPending, ar.Status)
		bob.do(http.MethodPost, base+"secret/access-requests", map[string]any{}, http.StatusNotFound, nil)
		bob.do(http.MethodPost, base+"nope/access-requests", map[string]any{}, http.StatusNotFound, nil)

		bob.do(http.MethodGet, base+"gated/access-requests", nil, http.StatusForbidden, nil)
		var list gen.AccessRequestList
		o.do(http.MethodGet, base+"gated/access-requests?status=pending", nil, http.StatusOK, &list)
		require.Len(t, list.Items, 1)
		require.Equal(t, "Acme", (*list.Items[0].Fields)["company"])
		o.do(http.MethodPatch, base+"gated/access-requests/"+ar.Id.String(), map[string]any{"status": "maybe"}, http.StatusUnprocessableEntity, nil)
		o.do(http.MethodPatch, base+"gated/access-requests/"+ar.Id.String(), map[string]any{"status": "accepted"}, http.StatusOK, &ar)
		require.Equal(t, gen.AccessRequestStatusAccepted, ar.Status)
		require.Equal(t, "owner", *ar.DecidedBy)
		bob.do(http.MethodPatch, base+"gated/access-requests/"+ar.Id.String(), map[string]any{"status": "rejected"}, http.StatusForbidden, nil)

		// Collaborators.
		bob.do(http.MethodGet, base+"secret/collaborators", nil, http.StatusNotFound, nil)
		var cl gen.CollaboratorList
		o.do(http.MethodPut, base+"secret/collaborators", map[string]any{"items": []map[string]any{{"username": "carol", "role": "read"}}}, http.StatusOK, &cl)
		require.Equal(t, "carol", cl.Items[0].Username)
		o.do(http.MethodPut, base+"secret/collaborators", map[string]any{"items": []map[string]any{{"username": "nobody", "role": "read"}}}, http.StatusUnprocessableEntity, nil)
		carol.do(http.MethodGet, base+"secret/collaborators", nil, http.StatusForbidden, nil)
		o.do(http.MethodGet, base+"secret/collaborators", nil, http.StatusOK, &cl)
		require.Len(t, cl.Items, 1)
		rp, err := dbRepos{d: d, orgs: e.orgs}.Get(ctx, model.RepoID{Type: model.RepoTypeModel, Namespace: "owner", Name: "secret"})
		require.NoError(t, err)
		require.NoError(t, e.svc.Authorizer.Can(ctx, mustPrincipal(t, e, "carol"), auth.ActionRead, auth.Target{Repo: api.AuthRepoTarget(rp)}))

		resolver := api.AuthRepoResolver{Repos: dbRepos{d: d, orgs: e.orgs}}
		got, err := resolver.RepoTarget(ctx, rp.ID)
		require.NoError(t, err)
		require.Equal(t, rp.UUID, got.UUID)
		_, err = resolver.RepoTarget(ctx, model.RepoID{Type: model.RepoTypeModel, Namespace: "owner", Name: "nope"})
		require.ErrorIs(t, err, auth.ErrNotFound)
	})
}

func mustPrincipal(t *testing.T, e *env, name string) model.Principal {
	t.Helper()
	u, err := e.svc.Users.GetByUsername(context.Background(), name)
	require.NoError(t, err)
	return u.Principal(model.AuthSession)
}

func TestAdminEndpoints(t *testing.T) {
	t.Parallel()
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		e := newEnv(t, d)
		e.user("root", true)
		e.user("alice", false)
		root, alice := e.login("root"), e.login("alice")

		for _, path := range []string{"/admin/users", "/admin/orgs", "/admin/settings", "/admin/tokens", "/admin/audit"} {
			alice.do(http.MethodGet, "/api/hangar/v1"+path, nil, http.StatusForbidden, nil)
			e.anon().do(http.MethodGet, "/api/hangar/v1"+path, nil, http.StatusUnauthorized, nil)
		}

		// Every Organisation, for the admin Organisations page.
		for _, name := range []string{"acme", "beta"} {
			alice.do(http.MethodPost, "/api/hangar/v1/orgs", map[string]any{"name": name}, http.StatusCreated, nil)
		}
		var orgs gen.AdminOrgList
		root.do(http.MethodGet, "/api/hangar/v1/admin/orgs", nil, http.StatusOK, &orgs)
		require.Len(t, orgs.Items, 2)
		require.Equal(t, "acme", orgs.Items[0].Name)
		require.Equal(t, 1, *orgs.Items[0].MemberCount, "the creator is a Member")
		root.do(http.MethodGet, "/api/hangar/v1/admin/orgs?limit=1", nil, http.StatusOK, &orgs)
		require.Len(t, orgs.Items, 1)
		require.NotNil(t, orgs.NextCursor)
		var last gen.AdminOrgList
		root.do(http.MethodGet, "/api/hangar/v1/admin/orgs?limit=1&cursor="+*orgs.NextCursor, nil, http.StatusOK, &last)
		require.Equal(t, "beta", last.Items[0].Name)
		require.Nil(t, last.NextCursor)
		root.do(http.MethodGet, "/api/hangar/v1/admin/orgs?q=ACM", nil, http.StatusOK, &orgs)
		require.Len(t, orgs.Items, 1)
		root.do(http.MethodGet, "/api/hangar/v1/admin/orgs?cursor=***", nil, http.StatusUnprocessableEntity, nil)

		var u gen.AdminUser
		root.do(http.MethodPost, "/api/hangar/v1/admin/users", map[string]any{"username": "dave", "email": "dave@example.com"}, http.StatusCreated, &u)
		require.False(t, u.IsAdmin)
		root.do(http.MethodPost, "/api/hangar/v1/admin/users", map[string]any{"username": "dave"}, http.StatusConflict, nil)
		var users gen.AdminUserList
		root.do(http.MethodGet, "/api/hangar/v1/admin/users?limit=2", nil, http.StatusOK, &users)
		require.Len(t, users.Items, 2)
		require.NotNil(t, users.NextCursor)
		root.do(http.MethodGet, "/api/hangar/v1/admin/users?q=dave", nil, http.StatusOK, &users)
		require.Len(t, users.Items, 1)
		yes := true
		root.do(http.MethodPatch, "/api/hangar/v1/admin/users/"+u.Id.String(), map[string]any{"is_admin": yes}, http.StatusOK, &u)
		require.True(t, u.IsAdmin)
		rootID := mustPrincipal(t, e, "root").UserID
		root.do(http.MethodPatch, "/api/hangar/v1/admin/users/"+rootID.String(), map[string]any{"disabled": true}, http.StatusOK, nil)
		// A disabled user's session counts as nobody signed in.
		root.do(http.MethodGet, "/api/hangar/v1/me", nil, http.StatusNoContent, nil)
		root.do(http.MethodGet, "/api/hangar/v1/me/tokens", nil, http.StatusUnauthorized, nil)
		dave := mustPrincipal(t, e, "dave")
		_, err := e.svc.Users.Update(context.Background(), rootID, auth.UserUpdate{Disabled: new(bool)}, model.System())
		require.NoError(t, err)
		root2 := e.login("root")
		root2.do(http.MethodPatch, "/api/hangar/v1/admin/users/"+dave.UserID.String(), map[string]any{"is_admin": false}, http.StatusOK, nil)
		root2.do(http.MethodPatch, "/api/hangar/v1/admin/users/"+rootID.String(), map[string]any{"is_admin": false}, http.StatusConflict, nil)

		var link gen.Link
		root2.do(http.MethodPost, "/api/hangar/v1/admin/users/"+dave.UserID.String()+"/reset-link", nil, http.StatusCreated, &link)
		require.True(t, strings.HasPrefix(link.Url, "http://hangar.test/auth/reset/"))
		root2.do(http.MethodPost, "/api/hangar/v1/admin/users/"+uuid.NewString()+"/reset-link", nil, http.StatusNotFound, nil)

		// The reset link works through the /auth handler and signs in.
		var me auth.MeView
		e.anon().do(http.MethodPost, strings.TrimPrefix(link.Url, "http://hangar.test"), map[string]any{"password": "dave's new password"}, http.StatusOK, &me)
		require.Equal(t, "dave", me.Username)

		var invite gen.Link
		root2.do(http.MethodPost, "/api/hangar/v1/admin/invites", map[string]any{"email": "eve@example.com"}, http.StatusCreated, &invite)
		root2.do(http.MethodPost, "/api/hangar/v1/admin/invites", map[string]any{"org": "nope"}, http.StatusUnprocessableEntity, nil)
		newcomer := e.anon()
		newcomer.do(http.MethodPost, strings.TrimPrefix(invite.Url, "http://hangar.test"), map[string]any{"username": "eve", "password": password}, http.StatusCreated, &me)
		require.Equal(t, "eve@example.com", me.Email)
		newcomer.csrf = me.CSRFToken
		newcomer.do(http.MethodGet, "/api/hangar/v1/me", nil, http.StatusOK, nil)
		e.anon().do(http.MethodPost, strings.TrimPrefix(invite.Url, "http://hangar.test"), map[string]any{"username": "eve2", "password": password}, http.StatusNotFound, nil)
		e.anon().do(http.MethodPost, "/auth/register", map[string]any{"username": "walkin", "password": password}, http.StatusForbidden, nil)

		// Settings.
		var st gen.InstanceSettings
		root2.do(http.MethodGet, "/api/hangar/v1/admin/settings", nil, http.StatusOK, &st)
		require.Equal(t, gen.InstanceSettingsRegistrationClosed, *st.Registration)
		require.Len(t, *st.OidcProviders, 1)
		root2.do(http.MethodPatch, "/api/hangar/v1/admin/settings", map[string]any{"registration": "open", "pull_concurrency": 6}, http.StatusOK, &st)
		require.Equal(t, gen.InstanceSettingsRegistrationOpen, *st.Registration)
		require.Equal(t, 6, *st.PullConcurrency)
		root2.do(http.MethodPatch, "/api/hangar/v1/admin/settings", map[string]any{"pull_windows": []string{"bad"}}, http.StatusUnprocessableEntity, nil)
		e.anon().do(http.MethodPost, "/auth/register", map[string]any{"username": "walkin", "password": password}, http.StatusCreated, nil)
		e.anon().do(http.MethodPost, "/auth/register", map[string]any{"username": "walkin", "password": password}, http.StatusConflict, nil)
		e.anon().do(http.MethodPost, "/auth/register", "x", http.StatusBadRequest, nil)

		// Tokens.
		var tok gen.TokenCreated
		alice.do(http.MethodPost, "/api/hangar/v1/me/tokens", map[string]any{"name": "laptop", "scope": "read"}, http.StatusCreated, &tok)
		var tokens gen.TokenList
		root2.do(http.MethodGet, "/api/hangar/v1/admin/tokens", nil, http.StatusOK, &tokens)
		require.Len(t, tokens.Items, 1)
		require.Equal(t, "alice", *tokens.Items[0].Username)
		root2.do(http.MethodDelete, "/api/hangar/v1/admin/tokens/"+tok.Id.String(), nil, http.StatusNoContent, nil)
		root2.do(http.MethodGet, "/api/hangar/v1/admin/tokens", nil, http.StatusOK, &tokens)
		require.Empty(t, tokens.Items)

		// Audit log, as JSON and CSV.
		var events gen.AuditEventList
		root2.do(http.MethodGet, "/api/hangar/v1/admin/audit?action=token.created,token.revoked", nil, http.StatusOK, &events)
		require.Len(t, events.Items, 2)
		require.Equal(t, "token.revoked", events.Items[0].Action)
		require.Equal(t, "root", *events.Items[0].Actor)
		root2.do(http.MethodGet, "/api/hangar/v1/admin/audit?actor=alice&limit=1", nil, http.StatusOK, &events)
		require.Len(t, events.Items, 1)
		require.NotNil(t, events.NextCursor)
		root2.do(http.MethodGet, "/api/hangar/v1/admin/audit?cursor=bogus", nil, http.StatusBadRequest, nil)
		resp := root2.do(http.MethodGet, "/api/hangar/v1/admin/audit?format=csv&"+url.Values{"action": {"login.failed,login.succeeded"}}.Encode(), nil, http.StatusOK, nil)
		require.Equal(t, "text/csv; charset=utf-8", resp.Header.Get("Content-Type"))
		req, _ := http.NewRequest(http.MethodGet, e.srv.URL+"/api/hangar/v1/admin/audit?format=csv", nil)
		resp, err = root2.http.Do(req)
		require.NoError(t, err)
		rows, err := csv.NewReader(resp.Body).ReadAll()
		resp.Body.Close()
		require.NoError(t, err)
		require.Equal(t, []string{"id", "created_at", "action", "actor", "target_type", "target_id", "ip", "user_agent", "data"}, rows[0])
		require.Greater(t, len(rows), 10)
		actions := map[string]bool{}
		for _, r := range rows[1:] {
			actions[r[2]] = true
		}
		for _, a := range []audit.Action{audit.LoginSucceeded, audit.UserCreated, audit.UserAdminChanged, audit.UserDisabled, audit.UserEnabled,
			audit.UserPasswordReset, audit.InviteCreated, audit.SettingsChanged, audit.TokenCreated, audit.TokenRevoked} {
			require.True(t, actions[string(a)], "audit log has %s", a)
		}
	})
}

func TestAuthHandlerErrors(t *testing.T) {
	t.Parallel()
	e := newEnv(t, dbtest.SQLite(t))
	e.user("alice", false)
	anon := e.anon()
	for path, code := range map[string]string{
		"/auth/oidc/nope":                        "oidc_unknown_provider",
		"/auth/oidc/sso/callback?error=denied":   "oidc_denied",
		"/auth/oidc/sso/callback?state=x&code=y": "oidc_state",
		"/auth/oidc/sso/callback":                "oidc_state",
	} {
		resp := anon.do(http.MethodGet, path, nil, http.StatusFound, nil)
		require.Equal(t, "/auth/login?error="+code, resp.Header.Get("Location"), path)
		require.Equal(t, "no-store", resp.Header.Get("Cache-Control"))
	}
	anon.do(http.MethodPost, "/auth/oidc/sso/link", nil, http.StatusUnauthorized, nil)
	alice := e.login("alice")
	var tok gen.TokenCreated
	alice.do(http.MethodPost, "/api/hangar/v1/me/tokens", map[string]any{"name": "t", "scope": "admin"}, http.StatusCreated, &tok)
	(&client{e: e, http: http.DefaultClient, bearer: tok.Secret}).do(http.MethodPost, "/auth/oidc/sso/link", nil, http.StatusForbidden, nil)
	alice.do(http.MethodDelete, "/auth/oidc/sso/link", nil, http.StatusNotFound, nil)
	anon.do(http.MethodPost, "/auth/reset/bogus", map[string]any{"password": password}, http.StatusNotFound, nil)
	anon.do(http.MethodPost, "/auth/invite/bogus", map[string]any{"username": "x1", "password": password}, http.StatusNotFound, nil)
}

type upstreamLookup map[string]uuid.UUID

func (u upstreamLookup) UpstreamID(_ context.Context, name string) (uuid.UUID, error) {
	if id, ok := u[name]; ok {
		return id, nil
	}
	return uuid.Nil, fmt.Errorf("%w: upstream %s", auth.ErrNotFound, name)
}

// TestEveryW5ActionIsAudited drives every identity and access action in
// docs/spec/05-auth.md "Audit events" through the API and checks the audit
// log holds each one. Repo, Upstream and webhook events belong to the
// workstreams that own those actions.
func TestEveryW5ActionIsAudited(t *testing.T) {
	t.Parallel()
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		ctx := context.Background()
		hf := db.NewID()
		_, err := d.NewInsert().Model(&db.Upstream{ID: hf, Name: "huggingface", DisplayName: "Hugging Face", BaseURL: "https://huggingface.co",
			Kind: "hf", Enabled: true, Source: "config"}).Exec(ctx)
		require.NoError(t, err)
		e := newEnvWith(t, d, upstreamLookup{"huggingface": hf})
		e.user("root", true)
		bob := e.user("bob", false)
		root, b := e.login("root"), e.login("bob")
		e.anon().do(http.MethodPost, "/api/hangar/v1/auth/login", map[string]string{"username": "bob", "password": "wrong-password-1"}, http.StatusUnauthorized, nil)

		var tok gen.TokenCreated
		b.do(http.MethodPost, "/api/hangar/v1/me/tokens", map[string]any{"name": "t", "scope": "read"}, http.StatusCreated, &tok)
		b.do(http.MethodDelete, "/api/hangar/v1/me/tokens/"+tok.Id.String(), nil, http.StatusNoContent, nil)
		var u gen.AdminUser
		root.do(http.MethodPost, "/api/hangar/v1/admin/users", map[string]any{"username": "carol"}, http.StatusCreated, &u)
		root.do(http.MethodPatch, "/api/hangar/v1/admin/users/"+u.Id.String(), map[string]any{"disabled": true, "is_admin": true}, http.StatusOK, nil)
		root.do(http.MethodPatch, "/api/hangar/v1/admin/users/"+u.Id.String(), map[string]any{"disabled": false}, http.StatusOK, nil)
		root.do(http.MethodPost, "/api/hangar/v1/admin/users/"+u.Id.String()+"/reset-link", nil, http.StatusCreated, nil)
		root.do(http.MethodPost, "/api/hangar/v1/admin/invites", map[string]any{}, http.StatusCreated, nil)
		b.do(http.MethodPost, "/api/hangar/v1/orgs", map[string]any{"name": "acme"}, http.StatusCreated, nil)
		b.do(http.MethodPut, "/api/hangar/v1/orgs/acme/members/carol", map[string]any{"role": "read"}, http.StatusOK, nil)
		b.do(http.MethodDelete, "/api/hangar/v1/orgs/acme/members/carol", nil, http.StatusNoContent, nil)
		b.do(http.MethodDelete, "/api/hangar/v1/orgs/acme", nil, http.StatusNoContent, nil)
		insertRepo(t, d, bob.NamespaceID, "gated", model.VisibilityPublic, map[string]any{"mode": "manual"})
		b.do(http.MethodPut, "/api/hangar/v1/repos/models/bob/gated/collaborators", map[string]any{"items": []map[string]any{{"username": "carol", "role": "read"}}}, http.StatusOK, nil)
		var ar gen.AccessRequest
		root.do(http.MethodPost, "/api/hangar/v1/repos/models/bob/gated/access-requests", map[string]any{}, http.StatusCreated, &ar)
		b.do(http.MethodPatch, "/api/hangar/v1/repos/models/bob/gated/access-requests/"+ar.Id.String(), map[string]any{"status": "accepted"}, http.StatusOK, nil)
		root.do(http.MethodPatch, "/api/hangar/v1/admin/settings", map[string]any{"anonymous_read": true}, http.StatusOK, nil)
		b.do(http.MethodPost, "/api/hangar/v1/me/ollama-keys", map[string]any{"public_key": "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIK0wmN/Cr3JXqmLW7u+g9pTh+wyqDHpSQEIQczXkVx9q"}, http.StatusCreated, nil)
		b.do(http.MethodPut, "/api/hangar/v1/me/upstream-tokens/huggingface", map[string]any{"token": "hf_secret"}, http.StatusOK, nil)
		var st gen.UpstreamTokenStatus
		b.do(http.MethodGet, "/api/hangar/v1/me/upstream-tokens/huggingface", nil, http.StatusOK, &st)
		require.True(t, st.Saved)
		b.do(http.MethodDelete, "/api/hangar/v1/me/upstream-tokens/huggingface", nil, http.StatusNoContent, nil)
		_, err = d.NewInsert().Model(&db.Identity{ID: db.NewID(), UserID: bob.ID, Provider: "sso", Subject: "b", Claims: map[string]any{}}).Exec(ctx)
		require.NoError(t, err)
		b.do(http.MethodDelete, "/auth/oidc/sso/link", nil, http.StatusNoContent, nil)
		b.do(http.MethodPost, "/api/hangar/v1/auth/logout", nil, http.StatusNoContent, nil)

		got := map[audit.Action]bool{}
		cursor := ""
		for {
			list, next, err := audit.NewDB(d, nil).List(ctx, audit.Filter{Limit: 500, Cursor: cursor})
			require.NoError(t, err)
			for _, ev := range list {
				got[ev.Action] = true
				require.NotContains(t, fmt.Sprint(ev.Data), "hf_secret")
			}
			if next == "" {
				break
			}
			cursor = next
		}
		for _, a := range []audit.Action{
			audit.LoginSucceeded, audit.LoginFailed, audit.Logout, audit.TokenCreated, audit.TokenRevoked, audit.UserCreated,
			audit.UserDisabled, audit.UserEnabled, audit.UserAdminChanged, audit.UserPasswordReset, audit.InviteCreated,
			audit.OrgCreated, audit.OrgDeleted, audit.MembershipChanged, audit.MembershipRemoved, audit.CollaboratorsChanged,
			audit.AccessRequestDecided, audit.SettingsChanged, audit.OllamaKeyChanged, audit.UpstreamTokenChanged, audit.IdentityLinked,
		} {
			require.True(t, got[a], "no %s event", a)
		}
	})
}
