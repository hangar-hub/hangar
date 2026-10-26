package authtest_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/auth"
	"github.com/hangar-hub/hangar/internal/auth/authtest"
	"github.com/hangar-hub/hangar/internal/model"
)

var (
	alice = model.Principal{Kind: model.PrincipalUser, UserID: uuid.New(), Username: "alice"}
	bob   = model.Principal{Kind: model.PrincipalUser, UserID: uuid.New(), Username: "bob"}
	root  = model.Principal{Kind: model.PrincipalUser, UserID: uuid.New(), Username: "root", IsAdmin: true}
	anon  = model.Anonymous()
)

func TestAuthenticator(t *testing.T) {
	a := authtest.NewAuthenticator()
	a.Add("hgr_alice", alice)
	a.Add("cookie-1", bob)

	req := func(set func(*http.Request)) (model.Principal, error) {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		set(r)
		return a.Authenticate(r)
	}
	p, err := req(func(r *http.Request) { r.Header.Set("Authorization", "Bearer hgr_alice") })
	require.NoError(t, err)
	require.Equal(t, "alice", p.Username)
	p, err = req(func(r *http.Request) { r.SetBasicAuth("anything", "hgr_alice") })
	require.NoError(t, err, "git sends the token as a Basic password")
	require.Equal(t, "alice", p.Username)
	p, err = req(func(r *http.Request) { r.AddCookie(&http.Cookie{Name: authtest.SessionCookie, Value: "cookie-1"}) })
	require.NoError(t, err)
	require.Equal(t, "bob", p.Username)
	p, err = req(func(*http.Request) {})
	require.NoError(t, err)
	require.True(t, p.IsAnonymous())
	_, err = req(func(r *http.Request) { r.Header.Set("Authorization", "Bearer nope") })
	require.ErrorIs(t, err, auth.ErrInvalidCredentials)
	_, err = req(func(r *http.Request) { r.Header.Set("Authorization", "Digest x") })
	require.ErrorIs(t, err, auth.ErrInvalidCredentials)
	a.Remove("hgr_alice")
	_, err = req(func(r *http.Request) { r.Header.Set("Authorization", "Bearer hgr_alice") })
	require.ErrorIs(t, err, auth.ErrInvalidCredentials)
}

func repoTarget(ns string, nsID uuid.UUID, vis model.Visibility) auth.Target {
	return auth.Target{Repo: &auth.RepoTarget{ID: model.RepoID{Type: model.RepoTypeModel, Namespace: ns, Name: "m"}, UUID: uuid.New(), NamespaceID: nsID, Visibility: vis}}
}

func TestAuthzPermissionTable(t *testing.T) {
	ctx := context.Background()
	z := authtest.NewAuthz()
	orgID := uuid.New()
	z.SetRole(bob.UserID, orgID, model.RoleRead)

	pub := repoTarget("ml", orgID, model.VisibilityPublic)
	internal := repoTarget("ml", orgID, model.VisibilityInternal)
	priv := repoTarget("ml", orgID, model.VisibilityPrivate)

	// Reads.
	require.ErrorIs(t, z.Can(ctx, anon, auth.ActionRead, pub), auth.ErrNotFound, "anonymous_read is off")
	z.AnonymousRead = true
	require.NoError(t, z.Can(ctx, anon, auth.ActionRead, pub))
	require.ErrorIs(t, z.Can(ctx, anon, auth.ActionRead, internal), auth.ErrNotFound)
	require.NoError(t, z.Can(ctx, alice, auth.ActionRead, internal))
	require.ErrorIs(t, z.Can(ctx, alice, auth.ActionRead, priv), auth.ErrNotFound, "private Repos are 404 to non-members")
	require.NoError(t, z.Can(ctx, bob, auth.ActionRead, priv))
	require.NoError(t, z.Can(ctx, root, auth.ActionRead, priv))

	// Writes and admin.
	require.ErrorIs(t, z.Can(ctx, bob, auth.ActionWrite, priv), auth.ErrForbidden, "read members cannot push")
	z.SetRole(bob.UserID, orgID, model.RoleWrite)
	require.NoError(t, z.Can(ctx, bob, auth.ActionWrite, priv))
	require.NoError(t, z.Can(ctx, bob, auth.ActionPullMirror, priv))
	require.ErrorIs(t, z.Can(ctx, bob, auth.ActionAdmin, priv), auth.ErrForbidden)
	require.ErrorIs(t, z.Can(ctx, anon, auth.ActionDiscuss, pub), auth.ErrForbidden)
	require.NoError(t, z.Can(ctx, alice, auth.ActionDiscuss, pub))

	// Personal Namespaces: the user is admin.
	own := repoTarget("alice", uuid.New(), model.VisibilityPrivate)
	require.NoError(t, z.Can(ctx, alice, auth.ActionAdmin, own))
	require.NoError(t, z.Can(ctx, alice, auth.ActionCreateRepo, auth.Target{Namespace: &auth.NamespaceTarget{Name: "alice"}}))
	require.ErrorIs(t, z.Can(ctx, alice, auth.ActionCreateRepo, auth.Target{Namespace: &auth.NamespaceTarget{ID: orgID, Name: "ml"}}), auth.ErrForbidden)
	require.NoError(t, z.Can(ctx, bob, auth.ActionCreateRepo, auth.Target{Namespace: &auth.NamespaceTarget{ID: orgID, Name: "ml"}}))
	require.ErrorIs(t, z.Can(ctx, bob, auth.ActionManageNamespace, auth.Target{Namespace: &auth.NamespaceTarget{ID: orgID, Name: "ml"}}), auth.ErrForbidden)

	// Collaborators.
	z.SetCollaborator(alice.UserID, priv.Repo.UUID, model.RoleWrite)
	require.NoError(t, z.Can(ctx, alice, auth.ActionWrite, priv))
	require.ErrorIs(t, z.Can(ctx, alice, auth.ActionPullMirror, priv), auth.ErrForbidden, "pulling into a Mirror needs write on the Namespace")

	// Gating.
	gated := repoTarget("ml", orgID, model.VisibilityPublic)
	gated.Repo.Gated = true
	carol := model.Principal{Kind: model.PrincipalUser, UserID: uuid.New(), Username: "carol"}
	require.NoError(t, z.Can(ctx, carol, auth.ActionRead, gated), "metadata stays readable")
	require.ErrorIs(t, z.Can(ctx, carol, auth.ActionReadFiles, gated), auth.ErrGated)
	z.Approve(carol.UserID, gated.Repo.UUID)
	require.NoError(t, z.Can(ctx, carol, auth.ActionReadFiles, gated))
	require.NoError(t, z.Can(ctx, bob, auth.ActionReadFiles, gated), "writers skip the gate")
	require.NoError(t, z.Can(ctx, alice, auth.ActionReadFiles, pub))

	// Instance admin.
	require.NoError(t, z.Can(ctx, root, auth.ActionInstanceAdmin, auth.Target{}))
	require.ErrorIs(t, z.Can(ctx, alice, auth.ActionInstanceAdmin, auth.Target{}), auth.ErrForbidden)
	require.ErrorIs(t, z.Can(ctx, alice, auth.ActionWrite, auth.Target{}), auth.ErrForbidden)

	// Token scopes and restrictions narrow, never widen.
	readOnly := alice
	readOnly.Token = &model.TokenInfo{Scope: model.ScopeRead}
	require.ErrorIs(t, z.Can(ctx, readOnly, auth.ActionWrite, own), auth.ErrForbidden)
	require.NoError(t, z.Can(ctx, readOnly, auth.ActionRead, own))
	limited := alice
	limited.Token = &model.TokenInfo{Scope: model.ScopeAdmin, Restrictions: model.TokenRestrictions{Namespaces: []string{"other"}}}
	require.ErrorIs(t, z.Can(ctx, limited, auth.ActionRead, own), auth.ErrNotFound)
	privReadOnly := readOnly
	require.ErrorIs(t, z.Can(ctx, privReadOnly, auth.ActionWrite, repoTarget("zzz", uuid.New(), model.VisibilityPrivate)), auth.ErrNotFound)
}

func TestTokensAndDeviceFlow(t *testing.T) {
	ctx := context.Background()
	authn := authtest.NewAuthenticator()
	tokens := authtest.NewTokens(authn)
	tokens.Principals[alice.UserID] = alice

	tok, secret, err := tokens.Create(ctx, auth.NewToken{UserID: alice.UserID, Name: "laptop", Scope: model.ScopeWrite})
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(secret, "hgr_"))
	require.Len(t, secret, 44)
	require.Equal(t, secret[:8], tok.Prefix)
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer "+secret)
	p, err := authn.Authenticate(r)
	require.NoError(t, err)
	require.Equal(t, "laptop", p.Token.Name)
	require.Equal(t, model.ScopeWrite, p.Token.Scope)

	_, _, err = tokens.Create(ctx, auth.NewToken{UserID: alice.UserID})
	require.Error(t, err)
	list, err := tokens.List(ctx, alice.UserID)
	require.NoError(t, err)
	require.Len(t, list, 1)
	all, err := tokens.List(ctx, uuid.Nil)
	require.NoError(t, err)
	require.Len(t, all, 1)
	require.ErrorIs(t, tokens.Revoke(ctx, tok.ID, bob), auth.ErrNotFound, "other users cannot revoke it")
	require.NoError(t, tokens.Revoke(ctx, tok.ID, alice))
	_, err = authn.Authenticate(r)
	require.ErrorIs(t, err, auth.ErrInvalidCredentials)

	now := time.Now()
	flow := authtest.NewDeviceFlow(tokens, "https://hangar.example.com/")
	flow.SetClock(func() time.Time { return now })
	code, err := flow.Start(ctx, "hf", "192.0.2.1")
	require.NoError(t, err)
	require.Equal(t, "https://hangar.example.com/settings/device", code.VerificationURI)
	_, err = flow.Poll(ctx, code.DeviceCode)
	require.ErrorIs(t, err, auth.ErrAuthorizationPending)
	require.ErrorIs(t, flow.Approve(ctx, code.UserCode, model.ScopeWrite, anon), auth.ErrForbidden)
	require.NoError(t, flow.Approve(ctx, code.UserCode, model.ScopeWrite, alice))
	got, err := flow.Poll(ctx, code.DeviceCode)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(got, "hgr_"))
	_, err = flow.Poll(ctx, code.DeviceCode)
	require.ErrorIs(t, err, auth.ErrExpiredToken, "a device code is single use")

	denied, err := flow.Start(ctx, "", "")
	require.NoError(t, err)
	require.NoError(t, flow.Deny(ctx, denied.UserCode, alice))
	_, err = flow.Poll(ctx, denied.DeviceCode)
	require.ErrorIs(t, err, auth.ErrAccessDenied)
	require.ErrorIs(t, flow.Deny(ctx, "nope", alice), auth.ErrNotFound)

	expired, err := flow.Start(ctx, "", "")
	require.NoError(t, err)
	now = now.Add(time.Hour)
	_, err = flow.Poll(ctx, expired.DeviceCode)
	require.ErrorIs(t, err, auth.ErrExpiredToken)
	require.ErrorIs(t, flow.Approve(ctx, expired.UserCode, model.ScopeRead, alice), auth.ErrNotFound)
	_, err = flow.Poll(ctx, "unknown")
	require.ErrorIs(t, err, auth.ErrExpiredToken)
}

func TestOllamaAuth(t *testing.T) {
	ctx := context.Background()
	authn := authtest.NewAuthenticator()
	o := authtest.NewOllamaAuth(authn)
	o.AddKey("pubkey:sig", alice)
	repo := model.RepoID{Type: model.RepoTypeModel, Namespace: "bartowski", Name: "gguf"}
	tok, exp, err := o.IssueRegistryToken(ctx, auth.OllamaTokenRequest{Method: "GET", Authorization: "pubkey:sig", Repo: repo})
	require.NoError(t, err)
	require.WithinDuration(t, time.Now().Add(5*time.Minute), exp, time.Minute)
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer "+tok)
	p, err := authn.Authenticate(r)
	require.NoError(t, err)
	require.Equal(t, model.AuthOllamaKey, p.Method)
	require.True(t, p.AllowsRepo(repo))
	require.False(t, p.AllowsRepo(model.RepoID{Type: model.RepoTypeModel, Namespace: "x", Name: "y"}))
	_, _, err = o.IssueRegistryToken(ctx, auth.OllamaTokenRequest{Authorization: "wrong"})
	require.ErrorIs(t, err, auth.ErrInvalidCredentials)
}
