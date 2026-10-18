package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/auth"
	"github.com/hangar-hub/hangar/internal/collections"
	"github.com/hangar-hub/hangar/internal/model"
	"github.com/hangar-hub/hangar/internal/org"
	"github.com/hangar-hub/hangar/internal/repo"
)

// TestNamespacesNeedSignInWhileAnonymousReadIsOff checks James's decision
// of 2026-10-18 on the full server, with the real Authorizer: while
// security.anonymous_read is off, user and Organisation profiles, member
// lists, a user's likes, Namespace search results and public Collections
// are for signed-in callers. Anonymous callers get 401, the answer they
// get for a public Repo on the same Instance, whether or not the
// Namespace exists. With anonymous_read on, nothing changes from the Hub's
// behaviour: all of it is public.
func TestNamespacesNeedSignInWhileAnonymousReadIsOff(t *testing.T) {
	in := newTestInstance(t, nil)
	ctx := context.Background()
	h, err := newHandler(ctx, in, newHealth(in.db, in.blobs), false)
	require.NoError(t, err)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	alice, bob := testUser(t, in, "alice"), testUser(t, in, "bob")
	_, err = in.orgs.CreateOrg(ctx, "lab", "Lab", alice)
	require.NoError(t, err)
	require.NoError(t, in.orgs.SetMember(ctx, "lab", bob.UserID, model.RoleRead, org.SourceManual, alice))
	root, err := in.auth.Users.Create(ctx, auth.NewUser{Username: "root", IsAdmin: true, Password: "a long enough password 1"}, model.System())
	require.NoError(t, err)
	token := func(p model.Principal, scope model.Scope) string {
		_, secret, err := in.auth.TokenStore.Create(ctx, auth.NewToken{UserID: p.UserID, Name: string(scope), Scope: scope})
		require.NoError(t, err)
		return secret
	}
	bobRead, rootAdmin := token(bob, model.ScopeRead), token(root.Principal(model.AuthSession), model.ScopeAdmin)
	_, err = in.repos.Create(ctx, repo.CreateParams{ID: model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "public"},
		Actor: alice, Visibility: model.VisibilityPublic, InitialReadme: true})
	require.NoError(t, err)
	picks, err := in.collections.Create(ctx, alice, collections.CreateParams{Namespace: "alice", Title: "Picks", Visibility: model.VisibilityPublic})
	require.NoError(t, err)

	do := func(tok, method, path, body string) (int, string) {
		t.Helper()
		req, err := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		require.NoError(t, err)
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		raw, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		return resp.StatusCode, string(raw)
	}
	expect := func(status int, tok, path string) string {
		t.Helper()
		got, raw := do(tok, http.MethodGet, path, "")
		require.Equal(t, status, got, "GET %s: %s", path, raw)
		return raw
	}
	setAnonymousRead := func(on bool) {
		t.Helper()
		body := `{"anonymous_read":false}`
		if on {
			body = `{"anonymous_read":true}`
		}
		code, raw := do(rootAdmin, http.MethodPatch, "/api/hangar/v1/admin/settings", body)
		require.Equal(t, http.StatusOK, code, raw)
	}
	searchNamespaces := func(tok string) []string {
		t.Helper()
		var res struct {
			Namespaces []struct {
				Name string `json:"name"`
			} `json:"namespaces"`
		}
		require.NoError(t, json.Unmarshal([]byte(expect(http.StatusOK, tok, "/api/hangar/v1/search?q=a&types=namespaces")), &res))
		names := []string{}
		for _, n := range res.Namespaces {
			names = append(names, n.Name)
		}
		return names
	}
	collectionsOf := func(tok, owner string) int {
		t.Helper()
		var res struct {
			Items []json.RawMessage `json:"items"`
		}
		require.NoError(t, json.Unmarshal([]byte(expect(http.StatusOK, tok, "/api/hangar/v1/collections?owner="+owner)), &res))
		return len(res.Items)
	}

	// Each profile endpoint, with a path that exists and one that does not.
	existing := []string{
		"/api/hangar/v1/namespaces/alice",
		"/api/hangar/v1/namespaces/lab",
		"/api/hangar/v1/orgs/lab/members",
		"/api/hangar/v1/orgs/lab/members/bob",
		"/api/users/alice/likes",
	}
	missing := []string{
		"/api/hangar/v1/namespaces/nobody",
		"/api/hangar/v1/orgs/nobody/members",
		"/api/hangar/v1/orgs/lab/members/nobody",
		"/api/users/nobody/likes",
	}

	// anonymous_read is off by default. A public Repo is a closed resource
	// to anonymous callers, and answers 401 on both APIs.
	expect(http.StatusUnauthorized, "", "/api/hangar/v1/repos/models/alice/public")
	expect(http.StatusUnauthorized, "", "/api/models/alice/public")
	for _, p := range append(append([]string{}, existing...), missing...) {
		raw := expect(http.StatusUnauthorized, "", p)
		// The native API's problem says to sign in; the Hub API sends its
		// usual 401 message, as for a Repo.
		require.True(t, strings.Contains(raw, "Sign in to see users and Organisations") || strings.Contains(raw, "You must be authenticated"), "%s: %s", p, raw)
		require.NotContains(t, raw, "nobody", "the answer does not depend on whether %s exists", p)
	}
	require.Empty(t, searchNamespaces(""), "anonymous search finds no users or Organisations")
	require.Zero(t, collectionsOf("", "alice"), "public Collections are closed too")
	expect(http.StatusNotFound, "", "/api/hangar/v1/collections/alice/"+picks.Slug)
	expect(http.StatusNotFound, "", "/api/collections/alice/"+picks.Slug)

	// Signed-in callers, even with a read Token and no Role, see them.
	for _, p := range existing {
		expect(http.StatusOK, bobRead, p)
	}
	for _, p := range missing {
		expect(http.StatusNotFound, bobRead, p)
	}
	require.ElementsMatch(t, []string{"alice", "lab"}, searchNamespaces(bobRead))
	require.Equal(t, 1, collectionsOf(bobRead, "alice"))
	expect(http.StatusOK, bobRead, "/api/hangar/v1/collections/alice/"+picks.Slug)

	// With anonymous_read on, nothing changes from the Hub: all public.
	setAnonymousRead(true)
	expect(http.StatusOK, "", "/api/models/alice/public")
	for _, p := range existing {
		expect(http.StatusOK, "", p)
	}
	for _, p := range missing {
		expect(http.StatusNotFound, "", p)
	}
	require.ElementsMatch(t, []string{"alice", "lab"}, searchNamespaces(""))
	require.Equal(t, 1, collectionsOf("", "alice"))
	expect(http.StatusOK, "", "/api/hangar/v1/collections/alice/"+picks.Slug)
	expect(http.StatusOK, "", "/api/collections/alice/"+picks.Slug)

	// Turning it off again closes them at once, with no restart.
	setAnonymousRead(false)
	expect(http.StatusUnauthorized, "", "/api/hangar/v1/namespaces/alice")
	expect(http.StatusUnauthorized, "", "/api/users/alice/likes")
}
