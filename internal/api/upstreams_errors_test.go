package api_test

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/api"
	"github.com/hangar-hub/hangar/internal/db"
	"github.com/hangar-hub/hangar/internal/db/dbtest"
	"github.com/hangar-hub/hangar/internal/model"
	"github.com/hangar-hub/hangar/internal/repo"
	"github.com/hangar-hub/hangar/internal/upstream"
)

func TestUpstreamsAPIErrors(t *testing.T) {
	t.Parallel()
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		e := newUpstreamsEnv(t, d)
		h := e.h
		alice := h.User("alice")
		e.authn.Add("alice", alice)
		ds := model.RepoID{Type: model.RepoTypeDataset, Namespace: "acme", Name: "data"}
		h.Seed(ds, map[string][]byte{"a.csv": []byte("x\n1\n")}, nil)
		gated := model.RepoID{Type: model.RepoTypeModel, Namespace: "acme", Name: "gated"}
		h.Seed(gated, map[string][]byte{"a.txt": []byte("a")}, nil)
		h.Remote.Gated = []string{gated.String()}

		code, body := e.do(t, "alice", http.MethodGet, "/discover/datasets?search=acme&filter=modality:tabular&author=acme&cursor=0", nil)
		require.Equal(t, http.StatusOK, code, body)
		require.Empty(t, body["items"], "filters go to the Upstream, and no dataset has that tag")
		code, body = e.do(t, "alice", http.MethodGet, "/discover/datasets?search=acme", nil)
		require.Equal(t, http.StatusOK, code)
		require.Len(t, body["items"], 1)
		code, _ = e.do(t, "alice", http.MethodGet, "/discover/datasets/acme/data?rev=main&upstream=huggingface", nil)
		require.Equal(t, http.StatusOK, code)
		code, body = e.do(t, "alice", http.MethodGet, "/discover/models/acme/gated", nil)
		require.Equal(t, http.StatusForbidden, code)
		require.Contains(t, body["detail"], "Upstream tokens")
		code, _ = e.do(t, "alice", http.MethodGet, "/discover/models/a/b", nil)
		require.Equal(t, http.StatusBadRequest, code, "invalid Repo IDs are refused")
		code, _ = e.do(t, "", http.MethodGet, "/discover/models", nil)
		require.Equal(t, http.StatusUnauthorized, code)
		code, _ = e.do(t, "", http.MethodGet, "/discover/models/acme/x", nil)
		require.Equal(t, http.StatusUnauthorized, code)

		h.Server.Fail("/api/models", http.StatusBadGateway, 4, "") // the client tries 4 times
		code, _ = e.do(t, "alice", http.MethodGet, "/discover/models?search=zzz", nil)
		require.Equal(t, http.StatusBadGateway, code)
		h.Server.Fail("/api/models/acme/rl", http.StatusTooManyRequests, 1, "3600")
		code, _ = e.do(t, "alice", http.MethodGet, "/discover/models/acme/rl", nil)
		require.Equal(t, http.StatusServiceUnavailable, code)

		code, _ = e.do(t, "alice", http.MethodPost, "/pulls", map[string]any{"upstream": "huggingface", "repo": "acme/data", "type": "space"})
		require.Equal(t, http.StatusBadRequest, code)
		code, _ = e.do(t, "alice", http.MethodPost, "/pulls", map[string]any{"upstream": "huggingface", "repo": "nope", "type": "model"})
		require.Equal(t, http.StatusBadRequest, code)
		code, _ = e.do(t, "alice", http.MethodPost, "/pulls", map[string]any{"upstream": "huggingface", "repo": "acme/data", "type": "dataset", "now": true,
			"selection": map[string]any{"paths": []string{"../x"}}})
		require.Equal(t, http.StatusBadRequest, code)
		code, _ = e.do(t, "", http.MethodPost, "/pulls", map[string]any{})
		require.Equal(t, http.StatusUnauthorized, code)
		code, body = e.do(t, "alice", http.MethodPost, "/pulls", map[string]any{"upstream": "huggingface", "repo": "acme/data", "type": "dataset",
			"revision": "main", "selection": map[string]any{"paths": []string{}}})
		require.Equal(t, http.StatusAccepted, code, body)
		require.Equal(t, true, body["payload"].(map[string]any)["selection"].(map[string]any)["none"], "an empty paths list is a history-only Pull")
		code, _ = e.do(t, "alice", http.MethodGet, "/pulls/"+uuid.NewString(), nil)
		require.Equal(t, http.StatusNotFound, code)
		code, _ = e.do(t, "", http.MethodGet, "/pulls/"+uuid.NewString(), nil)
		require.Equal(t, http.StatusUnauthorized, code)
		h.Run(10)

		// Mirror endpoints on a local Repo, a missing Repo and bad bodies.
		mine := model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "mine"}
		_, err := h.Repos.Create(h.Ctx, repo.CreateParams{ID: mine, Actor: alice})
		require.NoError(t, err)
		for _, c := range []struct{ method, path string }{{http.MethodGet, "/mirror"}, {http.MethodPost, "/mirror/sync"}, {http.MethodPost, "/mirror/detach"}} {
			code, _ = e.do(t, "alice", c.method, "/repos/models/alice/mine"+c.path, nil)
			require.Equal(t, http.StatusNotFound, code, c.path)
		}
		code, _ = e.do(t, "alice", http.MethodPatch, "/repos/models/alice/mine/mirror", map[string]any{"track": true})
		require.Equal(t, http.StatusNotFound, code)
		code, _ = e.do(t, "alice", http.MethodPost, "/repos/models/alice/mine/mirror/pull", map[string]any{})
		require.Equal(t, http.StatusNotFound, code)
		code, _ = e.do(t, "alice", http.MethodPatch, "/repos/datasets/acme/data/mirror", map[string]any{"bad": 1})
		require.Equal(t, http.StatusBadRequest, code)
		code, _ = e.do(t, "alice", http.MethodPost, "/repos/datasets/acme/data/mirror/pull", map[string]any{"bad": 1})
		require.Equal(t, http.StatusBadRequest, code)
		code, body = e.do(t, "alice", http.MethodPatch, "/repos/datasets/acme/data/mirror", map[string]any{"selection": map[string]any{"include": []string{"*.csv"}}, "track_ref": "main"})
		require.Equal(t, http.StatusOK, code, body)
		require.Equal(t, "main", body["track_ref"])
		code, _ = e.do(t, "alice", http.MethodPatch, "/repos/datasets/acme/data/mirror", map[string]any{"track_ref": "bad..ref"})
		require.Equal(t, http.StatusBadRequest, code)
		code, _ = e.do(t, "alice", http.MethodGet, "/repos/models/alice/nothing/mirror", nil)
		require.Equal(t, http.StatusNotFound, code)
		code, _ = e.do(t, "", http.MethodGet, "/repos/models/alice/nothing/mirror", nil)
		require.Equal(t, http.StatusUnauthorized, code)
		code, body = e.do(t, "alice", http.MethodGet, "/repos/models/alice/mine/dependencies", nil)
		require.Equal(t, http.StatusOK, code)
		require.Empty(t, body["needs"])

		// Admin errors.
		code, _ = e.do(t, "", http.MethodGet, "/admin/upstreams", nil)
		require.Equal(t, http.StatusUnauthorized, code)
		root := h.User("root")
		root.IsAdmin = true
		e.authn.Add("root", root)
		code, _ = e.do(t, "root", http.MethodGet, "/admin/upstreams/"+uuid.NewString(), nil)
		require.Equal(t, http.StatusNotFound, code)
		code, _ = e.do(t, "root", http.MethodPatch, "/admin/upstreams/"+uuid.NewString(), map[string]any{"enabled": true})
		require.Equal(t, http.StatusNotFound, code)
		code, _ = e.do(t, "root", http.MethodPatch, "/admin/upstreams/"+uuid.NewString(), map[string]any{"bad": true})
		require.Equal(t, http.StatusBadRequest, code)
		code, _ = e.do(t, "root", http.MethodPost, "/admin/upstreams", map[string]any{"bad": true})
		require.Equal(t, http.StatusBadRequest, code)
		code, _ = e.do(t, "root", http.MethodDelete, "/admin/upstreams/"+uuid.NewString(), nil)
		require.Equal(t, http.StatusNotFound, code)
		code, _ = e.do(t, "root", http.MethodPost, "/admin/upstreams/"+uuid.NewString()+"/test", nil)
		require.Equal(t, http.StatusNotFound, code)
		code, body = e.do(t, "root", http.MethodPost, "/admin/upstreams", map[string]any{"name": "down", "base_url": "http://127.0.0.1:1", "enabled": false,
			"display_name": "Down", "viewer_url": "http://127.0.0.1:1"})
		require.Equal(t, http.StatusCreated, code, body)
		code, body = e.do(t, "root", http.MethodPost, "/admin/upstreams/"+body["id"].(string)+"/test", nil)
		require.Equal(t, http.StatusOK, code)
		require.Equal(t, false, body["ok"])
		require.Contains(t, body["message"], "did not answer")
		up, err := h.Upstreams.ByName(h.Ctx, "down")
		require.NoError(t, err)
		code, _ = e.do(t, "alice", http.MethodPost, "/pulls", map[string]any{"upstream": up.Name, "repo": "acme/data", "type": "dataset"})
		require.Equal(t, http.StatusConflict, code, "a disabled Upstream")
		h.SetSettings(upstream.Settings{AllowOverrideWindows: false})
		code, _ = e.do(t, "alice", http.MethodPost, "/pulls", map[string]any{"upstream": "huggingface", "repo": "acme/data", "type": "dataset", "now": true})
		require.Equal(t, http.StatusForbidden, code)
	})
}

func TestUpstreamMirrorSummary(t *testing.T) {
	t.Parallel()
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		e := newUpstreamsEnv(t, d)
		h := e.h
		alice := h.User("alice")
		id := model.RepoID{Type: model.RepoTypeModel, Namespace: "acme", Name: "sum"}
		h.Seed(id, map[string][]byte{"a.txt": []byte("a")}, nil)
		_, _, err := h.Service.StartPull(h.Ctx, upstream.PullRequest{Repo: id, Actor: alice})
		require.NoError(t, err)
		h.Run(10)
		r, err := h.Repos.Get(h.Ctx, id)
		require.NoError(t, err)
		s, err := api.UpstreamMirrorSummary(h.Ctx, h.Service, r)
		require.NoError(t, err)
		require.Equal(t, "huggingface", s.Upstream)
		require.Equal(t, "acme/sum", s.UpstreamRepo)
		require.Equal(t, "ok", s.Status)
		require.NotNil(t, s.LastSyncedAt)
		mine := model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "plain"}
		pr, err := h.Repos.Create(h.Ctx, repo.CreateParams{ID: mine, Actor: alice})
		require.NoError(t, err)
		s, err = api.UpstreamMirrorSummary(h.Ctx, h.Service, pr)
		require.NoError(t, err)
		require.Nil(t, s)
	})
}
