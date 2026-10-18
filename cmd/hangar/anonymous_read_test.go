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
	"github.com/hangar-hub/hangar/internal/config"
	"github.com/hangar-hub/hangar/internal/model"
	"github.com/hangar-hub/hangar/internal/repo"
)

// TestAnonymousReadSettingAppliesToRepoListing checks the Instance setting
// security.anonymous_read, which an admin changes with PATCH
// /admin/settings, lets anonymous clients list and search public Repos at
// once and after a restart, as it lets them read one. Discover against a
// Hangar Upstream lists anonymously.
func TestAnonymousReadSettingAppliesToRepoListing(t *testing.T) {
	ctx := context.Background()
	serve := func(in *instance) string {
		t.Helper()
		h, err := newHandler(ctx, in, newHealth(in.db, in.blobs), false)
		require.NoError(t, err)
		srv := httptest.NewServer(h)
		t.Cleanup(srv.Close)
		return srv.URL
	}
	do := func(method, url, token, body string) (int, string) {
		t.Helper()
		req, err := http.NewRequest(method, url, strings.NewReader(body))
		require.NoError(t, err)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
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
	listIDs := func(url string) []string {
		t.Helper()
		code, body := do(http.MethodGet, url, "", "")
		require.Equal(t, http.StatusOK, code, body)
		var out []struct {
			ID string `json:"id"`
		}
		require.NoError(t, json.Unmarshal([]byte(body), &out), body)
		ids := []string{}
		for _, m := range out {
			ids = append(ids, m.ID)
		}
		return ids
	}

	var cfg *config.Config
	in := newTestInstance(t, func(c *config.Config) { cfg = c })
	require.False(t, cfg.Security.AnonymousRead, "the configuration leaves anonymous read off")
	u, err := in.auth.Users.Create(ctx, auth.NewUser{Username: "root", Password: "a long enough password 1", IsAdmin: true}, model.System())
	require.NoError(t, err)
	p := u.Principal(model.AuthSession)
	_, token, err := in.auth.TokenStore.Create(ctx, auth.NewToken{UserID: p.UserID, Name: "t", Scope: model.ScopeAdmin})
	require.NoError(t, err)
	id := model.RepoID{Type: model.RepoTypeModel, Namespace: "root", Name: "tiny-llama"}
	_, err = in.repos.Create(ctx, repo.CreateParams{ID: id, Actor: p, InitialReadme: true, Visibility: model.VisibilityPublic})
	require.NoError(t, err)
	_, err = in.repos.Create(ctx, repo.CreateParams{ID: model.RepoID{Type: model.RepoTypeModel, Namespace: "root", Name: "secret"},
		Actor: p, InitialReadme: true, Visibility: model.VisibilityPrivate})
	require.NoError(t, err)
	base := serve(in)

	require.Empty(t, listIDs(base+"/api/models"), "anonymous read is off")
	code, body := do(http.MethodPatch, base+"/api/hangar/v1/admin/settings", token, `{"anonymous_read":true}`)
	require.Equal(t, http.StatusOK, code, body)

	code, body = do(http.MethodGet, base+"/api/models/root/tiny-llama", "", "")
	require.Equal(t, http.StatusOK, code, body)
	require.Equal(t, []string{"root/tiny-llama"}, listIDs(base+"/api/models"))
	require.Equal(t, []string{"root/tiny-llama"}, listIDs(base+"/api/models?search=tiny"))

	code, body = do(http.MethodPatch, base+"/api/hangar/v1/admin/settings", token, `{"anonymous_read":false}`)
	require.Equal(t, http.StatusOK, code, body)
	require.Empty(t, listIDs(base+"/api/models"))
	code, body = do(http.MethodPatch, base+"/api/hangar/v1/admin/settings", token, `{"anonymous_read":true}`)
	require.Equal(t, http.StatusOK, code, body)

	// After a restart on the same data, the stored setting still applies,
	// not the configuration file's value. SQLite allows one process, so
	// the first Instance stops first.
	in.close(ctx)
	require.NoError(t, in.db.Close())
	again := newTestInstance(t, func(c *config.Config) { *c = *cfg })
	require.Equal(t, []string{"root/tiny-llama"}, listIDs(serve(again)+"/api/models?search=tiny"))
}
