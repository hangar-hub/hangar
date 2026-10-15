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

	"github.com/jamesagarside/hangar/internal/auth"
	"github.com/jamesagarside/hangar/internal/community"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/repo"
)

// TestHubLikersAndCommentChanges checks the Hub API's liker listings and
// comment edit and hide over the real community store: usernames and
// display names, visibility of private Repos, and who may change a comment.
func TestHubLikersAndCommentChanges(t *testing.T) {
	in := newTestInstance(t, nil)
	ctx := context.Background()
	h, err := newHandler(ctx, in, newHealth(in.db, in.blobs), false)
	require.NoError(t, err)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	token := func(name, display string) (model.Principal, string) {
		u, err := in.auth.Users.Create(ctx, auth.NewUser{Username: name, DisplayName: display, Password: "a long enough password 1"}, model.System())
		require.NoError(t, err)
		_, secret, err := in.auth.TokenStore.Create(ctx, auth.NewToken{UserID: u.ID, Name: "t", Scope: model.ScopeWrite})
		require.NoError(t, err)
		return u.Principal(model.AuthToken), secret
	}
	alice, aliceTok := token("alice", "Alice Liddell")
	bob, bobTok := token("bob", "")
	_, carolTok := token("carol", "")
	for _, name := range []string{"public", "secret"} {
		vis := model.VisibilityPublic
		if name == "secret" {
			vis = model.VisibilityPrivate
		}
		_, err := in.repos.Create(ctx, repo.CreateParams{ID: model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: name}, Actor: alice,
			InitialReadme: true, Visibility: vis})
		require.NoError(t, err)
	}

	call := func(method, path, tok string, body string) (int, string) {
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
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	for _, name := range []string{"public", "secret"} {
		code, body := call(http.MethodPost, "/api/models/alice/"+name+"/like", aliceTok, "")
		require.Equal(t, http.StatusOK, code, body)
	}
	code, body := call(http.MethodPost, "/api/models/alice/public/like", bobTok, "")
	require.Equal(t, http.StatusOK, code, body)

	code, body = call(http.MethodGet, "/api/models/alice/public/likers", bobTok, "")
	require.Equal(t, http.StatusOK, code, body)
	require.JSONEq(t, `[{"user":"alice","fullname":"Alice Liddell","avatarUrl":""},{"user":"bob","fullname":"bob","avatarUrl":""}]`, body)
	code, _ = call(http.MethodGet, "/api/models/alice/secret/likers", bobTok, "")
	require.Equal(t, http.StatusNotFound, code, "a private Repo's likers are as hidden as the Repo")

	code, body = call(http.MethodGet, "/api/users/alice/likes", aliceTok, "")
	require.Equal(t, http.StatusOK, code, body)
	require.Contains(t, body, `"name":"alice/secret"`)
	code, body = call(http.MethodGet, "/api/users/alice/likes", bobTok, "")
	require.Equal(t, http.StatusOK, code, body)
	require.JSONEq(t, `[{"repo":{"name":"alice/public","type":"model"}}]`, body, "Repos the caller cannot read are left out")
	code, _ = call(http.MethodGet, "/api/users/nobody/likes", bobTok, "")
	require.Equal(t, http.StatusNotFound, code)

	// Comments: the author edits; another user cannot; a writer hides.
	d, err := in.community.Create(ctx, community.CreateParams{RepoUUID: mustRepo(t, in, "alice/public").UUID, Repo: mustRepo(t, in, "alice/public").ID, Title: "Typo", Description: "A typo.", Author: bob})
	require.NoError(t, err)
	ev, err := in.community.Comment(ctx, mustRepo(t, in, "alice/public").UUID, d.Num, "Line 3.", bob)
	require.NoError(t, err)
	base := "/api/models/alice/public/discussions/" + jsonNum(d.Num) + "/comment/" + ev.ID.String()
	code, body = call(http.MethodPost, base+"/edit", bobTok, `{"content":"Line 4."}`)
	require.Equal(t, http.StatusOK, code, body)
	var edited struct {
		UpdatedComment struct {
			Data struct {
				Edited bool `json:"edited"`
				Latest struct {
					Raw string `json:"raw"`
				} `json:"latest"`
			} `json:"data"`
		} `json:"updatedComment"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &edited))
	require.True(t, edited.UpdatedComment.Data.Edited)
	require.Equal(t, "Line 4.", edited.UpdatedComment.Data.Latest.Raw)
	code, _ = call(http.MethodPost, base+"/edit", carolTok, `{"content":"Spam"}`)
	require.Equal(t, http.StatusForbidden, code, "only the author or a Repo admin edits a comment")
	code, _ = call(http.MethodPost, base+"/hide", carolTok, "")
	require.Equal(t, http.StatusForbidden, code)
	code, body = call(http.MethodPost, base+"/hide", aliceTok, "")
	require.Equal(t, http.StatusOK, code, body)
	require.Contains(t, body, `"hidden":true`)
	code, _ = call(http.MethodPost, "/api/models/alice/public/discussions/"+jsonNum(d.Num)+"/comment/not-a-uuid/hide", aliceTok, "")
	require.Equal(t, http.StatusNotFound, code)
}

func mustRepo(t *testing.T, in *instance, id string) *repo.Repo {
	t.Helper()
	rid, err := model.ParseRepoID(model.RepoTypeModel, id)
	require.NoError(t, err)
	r, err := in.repos.Get(context.Background(), rid)
	require.NoError(t, err)
	return r
}

func jsonNum(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}
