// Package hfapitest runs the Hub API over HTTP against fakes, for the
// hfapi tests and the compat/hf suite. It wires every dependency in
// hfapi.Deps with the fake from its `<pkg>test` package, adds a set of
// users with Tokens, and stands in for the git-lfs batch endpoint (W3's
// gitserver), with basic and multipart transfers, so clients can upload LFS
// files with HF_HUB_DISABLE_XET=1.
//
// The Repo service is the real-git repotest.Fake unless Options names
// another repo.Service, such as the real internal/repo implementation.
package hfapitest

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jamesagarside/hangar/internal/auth"
	"github.com/jamesagarside/hangar/internal/auth/authtest"
	"github.com/jamesagarside/hangar/internal/collections/collectionstest"
	"github.com/jamesagarside/hangar/internal/community/communitytest"
	"github.com/jamesagarside/hangar/internal/datasets/datasetstest"
	"github.com/jamesagarside/hangar/internal/hfapi"
	"github.com/jamesagarside/hangar/internal/httpx"
	"github.com/jamesagarside/hangar/internal/index/indextest"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/org"
	"github.com/jamesagarside/hangar/internal/org/orgtest"
	"github.com/jamesagarside/hangar/internal/repo"
	"github.com/jamesagarside/hangar/internal/repo/repotest"
	"github.com/jamesagarside/hangar/internal/scan/scantest"
	"github.com/jamesagarside/hangar/internal/xet/xettest"
)

// Org is the Organisation every Env has: alice is its admin and bob a
// read Member.
const Org = "acme"

// User is a test user with a Token: alice and admin have admin scope (Repo
// settings need it), bob write and carol read.
type User struct {
	Principal model.Principal
	// Token authenticates as the user with the Token's scope.
	Token string
}

// Options configure an Env.
type Options struct {
	// Repos and LFS replace the repotest fakes; set both or neither.
	Repos repo.Service
	LFS   repo.LFSStore
	// Addr is the listen address; empty means a random local port.
	Addr string
	// Settings adjusts the Hub API settings; PublicURL is set first.
	Settings func(*hfapi.Settings)
	// Deps adjusts the dependencies before the handler is built.
	Deps func(*hfapi.Deps)
	// Orgs replaces the orgtest fake. AddUser and AddOrg must then create
	// users and Organisations in it (and in the Repo backend).
	Orgs org.Service
	// AddUser creates a user and returns their Principal; nil uses the
	// orgtest fake.
	AddUser func(t testing.TB, name string) model.Principal
	// AddOrg creates an Organisation with members and returns its
	// Namespace ID; nil uses the orgtest fake.
	AddOrg func(t testing.TB, name string, members map[uuid.UUID]model.Role) uuid.UUID
	// LFSMultipartThreshold plays the part of the Instance setting
	// `lfs.multipart_threshold` for the git-lfs stand-in: objects larger
	// than this get Hugging Face's multipart transfer when the client
	// offers it. Zero means every upload is a single PUT.
	LFSMultipartThreshold int64
	// LFSPartSize is the multipart part size; zero means
	// LFSMultipartThreshold.
	LFSPartSize int64
}

// Env is a running Hub API with fakes.
type Env struct {
	URL    string
	Server *httptest.Server
	// Handler is the Hub API handler.
	Handler *hfapi.Handler

	Repos       repo.Service
	LFS         repo.LFSStore
	RepoFake    *repotest.Fake
	Authn       *authtest.Authenticator
	Authz       *authtest.Authz
	Tokens      *authtest.Tokens
	Devices     *authtest.DeviceFlow
	Orgs        org.Service
	Community   *communitytest.Fake
	Collections *collectionstest.Fake
	Index       *indextest.Fake
	Scan        *scantest.Fake
	Xet         *xettest.Fake
	Viewer      *datasetstest.Viewer

	// Users holds alice, bob, carol (a read-scope Token) and admin (an
	// Instance admin).
	Users map[string]User
}

// New starts an Env. It stops when the test ends.
func New(t testing.TB, o Options) *Env {
	t.Helper()
	e := &Env{
		Authn: authtest.NewAuthenticator(), Authz: authtest.NewAuthz(),
		Community: communitytest.NewFake(), Collections: collectionstest.NewFake(), Index: indextest.NewFake(),
		Viewer: datasetstest.NewViewer(), Users: map[string]User{},
	}
	e.Tokens = authtest.NewTokens(e.Authn)
	// Public Repos are readable without a Token, as on huggingface.co.
	e.Authz.AnonymousRead = true
	if o.Repos != nil {
		e.Repos, e.LFS = o.Repos, o.LFS
	} else {
		e.RepoFake = repotest.NewFake(t)
		e.Repos, e.LFS = e.RepoFake, e.RepoFake.LFS
	}
	e.Scan = scantest.NewFake(e.Repos)

	addr := o.Addr
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	e.URL = "http://" + ln.Addr().String()
	e.Devices = authtest.NewDeviceFlow(e.Tokens, e.URL)
	e.Xet = xettest.NewFake(e.URL+"/xet-cas", time.Hour)

	addUser, addOrg := o.AddUser, o.AddOrg
	if o.Orgs != nil {
		e.Orgs = o.Orgs
	} else {
		fake := orgtest.NewFake()
		e.Orgs = fake
		addUser = func(t testing.TB, name string) model.Principal {
			p, err := fake.AddUser(name)
			if err != nil {
				t.Fatalf("add user: %v", err)
			}
			return p
		}
		addOrg = func(t testing.TB, name string, members map[uuid.UUID]model.Role) uuid.UUID {
			ctx := context.Background()
			ns, err := fake.CreateOrg(ctx, name, name, model.System())
			if err != nil {
				t.Fatalf("create org: %v", err)
			}
			for id, role := range members {
				if err := fake.SetMember(ctx, name, id, role, org.SourceManual, model.System()); err != nil {
					t.Fatalf("set member: %v", err)
				}
			}
			return ns.ID
		}
	}
	for _, name := range []string{"alice", "bob", "carol", "admin"} {
		p := addUser(t, name)
		p.Email = name + "@example.com"
		if name == "admin" {
			p.IsAdmin = true
		}
		e.Tokens.Principals[p.UserID] = p
		scope := model.ScopeWrite
		switch name {
		case "carol":
			scope = model.ScopeRead
		case "alice", "admin":
			scope = model.ScopeAdmin
		}
		tok := &model.TokenInfo{Name: name + "-laptop", Scope: scope}
		tp := p
		tp.Method, tp.Token = model.AuthToken, tok
		secret := "hf_test_" + name + "_token"
		e.Authn.Add(secret, tp)
		e.Users[name] = User{Principal: tp, Token: secret}
	}
	alice, bob := e.Users["alice"].Principal.UserID, e.Users["bob"].Principal.UserID
	acme := addOrg(t, Org, map[uuid.UUID]model.Role{alice: model.RoleAdmin, bob: model.RoleRead})
	e.Authz.SetRole(alice, acme, model.RoleAdmin)
	e.Authz.SetRole(bob, acme, model.RoleRead)

	settings := hfapi.Settings{PublicURL: e.URL}
	if o.Settings != nil {
		o.Settings(&settings)
	}
	deps := hfapi.Deps{
		Settings: settings, Repos: e.Repos, LFS: e.LFS, Authz: &namespaceAuthz{inner: e.Authz, orgs: e.Orgs},
		Orgs: e.Orgs, Community: e.Community, Collections: e.Collections, Index: e.Index, Scan: e.Scan,
		Xet: e.Xet, Viewer: e.Viewer, Devices: e.Devices,
		Log: slog.New(slog.DiscardHandler),
	}
	if o.Deps != nil {
		o.Deps(&deps)
	}
	if e.RepoFake != nil {
		// Commits may reference only LFS objects the caller uploaded or
		// can read, as with repo.Store.
		e.RepoFake.CheckLFSReferences = true
		e.RepoFake.CanReadFiles = repotest.CanReadFilesWith(deps.Authz)
	}
	h, err := hfapi.New(deps)
	if err != nil {
		t.Fatalf("hfapi.New: %v", err)
	}
	e.Handler = h
	partSize := o.LFSPartSize
	if partSize <= 0 {
		partSize = o.LFSMultipartThreshold
	}
	lfs := &lfsStandIn{lfs: e.LFS, repos: e.Repos, base: e.URL, multipartThreshold: o.LFSMultipartThreshold, partSize: partSize}
	protocols := httpx.Protocols{Hub: h, LFS: lfs, Health: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteJSON(w, r, http.StatusOK, map[string]string{"status": "ok"})
	})}
	handler := httpx.Dispatch(protocols, e.URL)
	handler = lfs.uploads(handler)
	handler = httpx.Authenticate(e.Authn, nil)(handler)
	handler = classify(handler, e.URL)
	handler = httpx.RequestIDs(handler)
	e.Server = &httptest.Server{Listener: ln, Config: &http.Server{Handler: handler, ReadHeaderTimeout: 30 * time.Second}}
	e.Server.Start()
	t.Cleanup(e.Server.Close)
	return e
}

// classify records the protocol before authentication, so a bad credential
// gets the Hub's 401 (docs/build/requests/w4.md asks for the same in
// cmd/hangar).
func classify(next http.Handler, publicURL string) http.Handler {
	viewerHost := "datasets-server." + strings.TrimPrefix(strings.TrimPrefix(publicURL, "http://"), "https://")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpx.SetProtocol(r.Context(), httpx.Classify(r, viewerHost).Protocol())
		next.ServeHTTP(w, r)
	})
}

// namespaceAuthz fills in RepoTarget.NamespaceID from the Namespace name
// before asking the fake Authz, because repotest.Fake records the
// creator's personal Namespace on every Repo, including Organisation Repos.
type namespaceAuthz struct {
	inner auth.Authz
	orgs  org.Service
}

func (a *namespaceAuthz) Can(ctx context.Context, p model.Principal, action auth.Action, t auth.Target) error {
	if t.Repo != nil {
		if ns, err := a.orgs.Get(ctx, t.Repo.ID.Namespace); err == nil {
			rt := *t.Repo
			rt.NamespaceID = ns.ID
			t.Repo = &rt
		}
	}
	return a.inner.Can(ctx, p, action, t)
}

// Request makes a request as user ("" for anonymous).
func (e *Env) Request(t testing.TB, method, path, user string, body string, header ...string) *http.Response {
	t.Helper()
	var r *http.Request
	var err error
	if body == "" {
		r, err = http.NewRequest(method, e.URL+path, nil)
	} else {
		r, err = http.NewRequest(method, e.URL+path, strings.NewReader(body))
	}
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if user != "" {
		r.Header.Set("Authorization", "Bearer "+e.Users[user].Token)
	}
	for i := 0; i+1 < len(header); i += 2 {
		r.Header.Set(header[i], header[i+1])
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(r)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}
