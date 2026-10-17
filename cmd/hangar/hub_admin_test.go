package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/auth"
	"github.com/jamesagarside/hangar/internal/config"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/org"
	"github.com/jamesagarside/hangar/internal/repo"
)

// TestWriteTokenAdministersReposThroughHubAPIOnly checks James's decision
// of 2026-10-17 ("Hub actions only") on the full server, with the real
// Authorizer: a write Token may delete a Repo, change its visibility or
// gating, and rename it within its Namespace, only through the Hub API
// endpoints huggingface_hub calls, and only when its user is the Repo's
// admin. Transfers to another Namespace, webhooks, collaborators,
// access-request decisions and every native API admin action still need
// the admin scope.
func TestWriteTokenAdministersReposThroughHubAPIOnly(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.NotFoundHandler())
	pub := "http://" + srv.Listener.Addr().String()
	in := newTestInstance(t, func(c *config.Config) { c.Server.PublicURL = pub })
	ctx := context.Background()
	h, err := newHandler(ctx, in, newHealth(in.db, in.blobs), false)
	require.NoError(t, err)
	srv.Config.Handler = h
	srv.Start()
	t.Cleanup(srv.Close)

	alice, bob := testUser(t, in, "alice"), testUser(t, in, "bob")
	_, err = in.orgs.CreateOrg(ctx, "lab", "Lab", alice)
	require.NoError(t, err)
	require.NoError(t, in.orgs.SetMember(ctx, "lab", bob.UserID, model.RoleWrite, org.SourceManual, alice))
	token := func(p model.Principal, scope model.Scope) string {
		_, secret, err := in.auth.TokenStore.Create(ctx, auth.NewToken{UserID: p.UserID, Name: string(scope), Scope: scope})
		require.NoError(t, err)
		return secret
	}
	write, admin, bobWrite := token(alice, model.ScopeWrite), token(alice, model.ScopeAdmin), token(bob, model.ScopeWrite)
	for _, id := range []string{"alice/settings", "alice/rename", "alice/transfer", "alice/delete", "alice/native", "alice/old", "lab/shared"} {
		rid, err := model.ParseRepoID(model.RepoTypeModel, id)
		require.NoError(t, err)
		_, err = in.repos.Create(ctx, repo.CreateParams{ID: rid, Actor: alice, Visibility: model.VisibilityPublic})
		require.NoError(t, err)
	}
	do := func(tok, method, path, body string) (int, string) {
		t.Helper()
		req, err := http.NewRequest(method, pub+path, strings.NewReader(body))
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+tok)
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
	expect := func(status int, tok, method, path, body string) {
		t.Helper()
		got, raw := do(tok, method, path, body)
		require.Equal(t, status, got, "%s %s %s: %s", method, path, body, raw)
	}

	// Allowed with a write Token: the Hub API's update_repo_settings,
	// same-Namespace move_repo and delete_repo.
	expect(http.StatusOK, write, http.MethodPut, "/api/models/alice/settings/settings", `{"private":true}`)
	expect(http.StatusOK, write, http.MethodPut, "/api/models/alice/settings/settings", `{"gated":"manual"}`)
	expect(http.StatusOK, write, http.MethodPost, "/api/repos/move", `{"fromRepo":"alice/rename","toRepo":"alice/renamed","type":"model"}`)
	expect(http.StatusOK, write, http.MethodDelete, "/api/repos/delete", `{"name":"delete","type":"model"}`)
	rp, err := in.repos.Get(ctx, model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "settings"})
	require.NoError(t, err)
	require.Equal(t, model.VisibilityPrivate, rp.Visibility)
	require.Equal(t, "manual", rp.Gating.Mode)

	// Refused with a write Token: a transfer to another Namespace, even
	// one the user administers.
	expect(http.StatusForbidden, write, http.MethodPost, "/api/repos/move", `{"fromRepo":"alice/transfer","toRepo":"lab/transfer","type":"model"}`)
	expect(http.StatusOK, admin, http.MethodPost, "/api/repos/move", `{"fromRepo":"alice/transfer","toRepo":"lab/transfer","type":"model"}`)
	// An old ID that redirects to another Namespace does not turn a
	// transfer into a rename.
	expect(http.StatusOK, admin, http.MethodPost, "/api/repos/move", `{"fromRepo":"alice/old","toRepo":"lab/old","type":"model"}`)
	expect(http.StatusForbidden, write, http.MethodPost, "/api/repos/move", `{"fromRepo":"alice/old","toRepo":"alice/old2","type":"model"}`)

	// Refused with a write Token whose user is not the Repo's admin.
	expect(http.StatusForbidden, bobWrite, http.MethodDelete, "/api/repos/delete", `{"name":"shared","organization":"lab","type":"model"}`) //nolint:misspell // Hub API field name.
	expect(http.StatusForbidden, bobWrite, http.MethodPut, "/api/models/lab/shared/settings", `{"private":true}`)
	expect(http.StatusForbidden, bobWrite, http.MethodPost, "/api/repos/move", `{"fromRepo":"lab/shared","toRepo":"lab/shared2","type":"model"}`)

	// Refused with a write Token: every native API admin action.
	native := "/api/hangar/v1/repos/models/alice/native"
	expect(http.StatusForbidden, write, http.MethodDelete, native, "")
	expect(http.StatusForbidden, write, http.MethodPatch, native, `{"visibility":"private"}`)
	expect(http.StatusForbidden, write, http.MethodPost, native+"/rename", `{"name":"native2"}`)
	expect(http.StatusForbidden, write, http.MethodPost, native+"/transfer", `{"namespace":"lab"}`)
	expect(http.StatusForbidden, write, http.MethodPut, native+"/collaborators", `{"items":[{"username":"bob","role":"write"}]}`)
	expect(http.StatusForbidden, write, http.MethodGet, native+"/access-requests", "")
	expect(http.StatusForbidden, write, http.MethodPost, "/api/hangar/v1/webhooks",
		`{"repo":"model/alice/native","url":"https://hooks.example.com/x","events":["repo.created"],"secret":"a webhook secret"}`)
	// The admin scope still does all of them.
	expect(http.StatusOK, admin, http.MethodPatch, native, `{"visibility":"private"}`)
	expect(http.StatusOK, admin, http.MethodPut, native+"/collaborators", `{"items":[{"username":"bob","role":"write"}]}`)
	expect(http.StatusNoContent, admin, http.MethodDelete, native, "")
}
