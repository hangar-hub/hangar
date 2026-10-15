package hfapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/community"
	"github.com/jamesagarside/hangar/internal/model"
)

func TestLikersAndUserLikes(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	seed(t, e, "alice", modelID("alice", "pub"), model.VisibilityPublic, nil)
	seed(t, e, "alice", modelID("alice", "priv"), model.VisibilityPrivate, nil)
	for _, name := range []string{"pub", "priv"} {
		r := e.Request(t, http.MethodPost, "/api/models/alice/"+name+"/like", "alice", "")
		require.Equal(t, http.StatusOK, r.StatusCode)
	}

	r := e.Request(t, http.MethodGet, "/api/models/alice/pub/likers", "bob", "")
	require.Equal(t, http.StatusOK, r.StatusCode)
	var likers []map[string]string
	require.NoError(t, json.Unmarshal(readBody(t, r), &likers))
	require.Len(t, likers, 1)
	for _, k := range []string{"user", "fullname"} {
		require.NotEmpty(t, likers[0][k], k)
	}
	require.Contains(t, likers[0], "avatarUrl")
	requireHubError(t, e.Request(t, http.MethodGet, "/api/models/alice/priv/likers", "bob", ""), http.StatusNotFound, "RepoNotFound")

	likes := func(user, as string) []string {
		t.Helper()
		r := e.Request(t, http.MethodGet, "/api/users/"+user+"/likes", as, "")
		require.Equal(t, http.StatusOK, r.StatusCode)
		var out []struct {
			Repo struct{ Name, Type string } `json:"repo"`
		}
		require.NoError(t, json.Unmarshal(readBody(t, r), &out))
		names := []string{}
		for _, l := range out {
			require.Equal(t, "model", l.Repo.Type)
			names = append(names, l.Repo.Name)
		}
		return names
	}
	require.ElementsMatch(t, []string{"alice/pub", "alice/priv"}, likes("alice", "alice"))
	require.Equal(t, []string{"alice/pub"}, likes("alice", "bob"), "Repos the caller cannot read are left out")
	require.Equal(t, http.StatusNotFound, e.Request(t, http.MethodGet, "/api/users/nobody/likes", "bob", "").StatusCode)
	require.Equal(t, http.StatusNotFound, e.Request(t, http.MethodGet, "/api/users/acme/likes", "bob", "").StatusCode, "an Organisation likes nothing")
	require.Equal(t, http.StatusNotFound, e.Request(t, http.MethodGet, "/api/users/alice", "bob", "").StatusCode)
}

func TestEditAndHideComments(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	rp, _ := seed(t, e, "alice", modelID("alice", "talk"), model.VisibilityPublic, nil)
	ctx := context.Background()
	d, err := e.Community.Create(ctx, community.CreateParams{RepoUUID: rp.UUID, Repo: rp.ID, Title: "Typo", Description: "A typo.", Author: e.Users["bob"].Principal})
	require.NoError(t, err)
	ev, err := e.Community.Comment(ctx, rp.UUID, d.Num, "Line 3.", e.Users["bob"].Principal)
	require.NoError(t, err)
	base := "/api/models/alice/talk/discussions/" + strconv.Itoa(d.Num) + "/comment/"

	r := e.Request(t, http.MethodPost, base+ev.ID.String()+"/edit", "bob", `{"content":"Line 4."}`)
	require.Equal(t, http.StatusOK, r.StatusCode)
	var out struct {
		UpdatedComment struct {
			ID   string `json:"id"`
			Data struct {
				Edited   bool `json:"edited"`
				Hidden   bool `json:"hidden"`
				NumEdits int  `json:"numEdits"`
				Latest   struct {
					Raw string `json:"raw"`
				} `json:"latest"`
			} `json:"data"`
		} `json:"updatedComment"`
	}
	require.NoError(t, json.Unmarshal(readBody(t, r), &out))
	require.Equal(t, ev.ID.String(), out.UpdatedComment.ID)
	require.True(t, out.UpdatedComment.Data.Edited)
	require.Equal(t, 1, out.UpdatedComment.Data.NumEdits)
	require.Equal(t, "Line 4.", out.UpdatedComment.Data.Latest.Raw)

	require.Equal(t, http.StatusForbidden, e.Request(t, http.MethodPost, base+ev.ID.String()+"/edit", "carol", `{"content":"Spam"}`).StatusCode)
	require.Equal(t, http.StatusBadRequest, e.Request(t, http.MethodPost, base+ev.ID.String()+"/edit", "bob", `{"content":""}`).StatusCode)
	require.Equal(t, http.StatusNotFound, e.Request(t, http.MethodPost, base+"not-a-uuid/edit", "bob", `{"content":"x"}`).StatusCode)
	require.Equal(t, http.StatusNotFound, e.Request(t, http.MethodPost, base+d.Events[0].ID.String()+"0/hide", "alice", "").StatusCode)
	require.Equal(t, http.StatusNotFound, e.Request(t, http.MethodPost, "/api/models/alice/talk/discussions/99/comment/"+ev.ID.String()+"/hide", "alice", "").StatusCode)
	require.Equal(t, http.StatusNotFound, e.Request(t, http.MethodPost, base+ev.ID.String()+"/delete", "alice", "").StatusCode)

	r = e.Request(t, http.MethodPost, base+ev.ID.String()+"/hide", "alice", "")
	require.Equal(t, http.StatusOK, r.StatusCode, "a Repo admin hides another user's comment")
	require.NoError(t, json.Unmarshal(readBody(t, r), &out))
	require.True(t, out.UpdatedComment.Data.Hidden)
	require.Empty(t, out.UpdatedComment.Data.Latest.Raw)
}
