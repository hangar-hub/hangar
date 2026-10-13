package api_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/api"
	"github.com/jamesagarside/hangar/internal/api/gen"
	"github.com/jamesagarside/hangar/internal/auth/authtest"
	"github.com/jamesagarside/hangar/internal/db"
	"github.com/jamesagarside/hangar/internal/db/dbtest"
	"github.com/jamesagarside/hangar/internal/httpx"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/upstream/upstreamtest"
)

// upstreamsServer is the native API with W6's handlers and 501 for the rest.
type upstreamsServer struct {
	*api.Upstreams
	upstreamsUnimplemented
}

type upstreamsUnimplemented struct{ gen.Unimplemented }

type upstreamsEnv struct {
	h     *upstreamstest
	srv   *httptest.Server
	authn *authtest.Authenticator
}

type upstreamstest = upstreamtest.Harness

func newUpstreamsEnv(t *testing.T, d *db.DB) *upstreamsEnv {
	t.Helper()
	h := upstreamtest.NewHarness(t, d, upstreamtest.HarnessOptions{})
	authn := authtest.NewAuthenticator()
	handler := gen.HandlerWithOptions(upstreamsServer{Upstreams: &api.Upstreams{Service: h.Service, Client: h.Client, Repos: h.Repos, Authz: h.Authz,
		Queue: h.Queue}}, gen.ChiServerOptions{BaseURL: "/api/hangar/v1"})
	chain := httpx.RequestIDs(httpx.Authenticate(authn, nil)(handler))
	srv := httptest.NewServer(chain)
	t.Cleanup(srv.Close)
	return &upstreamsEnv{h: h, srv: srv, authn: authn}
}

func (e *upstreamsEnv) do(t *testing.T, token, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		require.NoError(t, err)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, e.srv.URL+"/api/hangar/v1"+path, rd)
	require.NoError(t, err)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var m map[string]any
	if len(raw) > 0 {
		require.NoError(t, json.Unmarshal(raw, &m), string(raw))
	}
	return resp.StatusCode, m
}

func TestUpstreamsAPI(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		e := newUpstreamsEnv(t, d)
		h := e.h
		alice := h.User("alice")
		e.authn.Add("alice", alice)
		admin := h.User("root")
		admin.IsAdmin = true
		e.authn.Add("root", admin)
		ro := h.User("reader")
		ro.Token = &model.TokenInfo{Scope: model.ScopeRead}
		e.authn.Add("ro", ro)

		gguf := model.RepoID{Type: model.RepoTypeModel, Namespace: "acme", Name: "api-GGUF"}
		h.Seed(gguf, map[string][]byte{"README.md": []byte("# card"), "config.json": []byte(`{"auto_map":{"AutoModel":"acme/code--m.M"}}`)},
			map[string][]byte{"m-Q4_K_M.gguf": []byte("q4"), "m-Q8_0.gguf": []byte("q8")})
		h.Seed(model.RepoID{Type: model.RepoTypeModel, Namespace: "acme", Name: "code"}, map[string][]byte{"m.py": []byte("class M: pass")}, nil)

		// Discover needs a signed-in caller.
		code, _ := e.do(t, "", http.MethodGet, "/upstreams", nil)
		require.Equal(t, http.StatusUnauthorized, code)
		code, body := e.do(t, "alice", http.MethodGet, "/upstreams", nil)
		require.Equal(t, http.StatusOK, code)
		require.Len(t, body["items"], 1)
		require.NotContains(t, body["items"].([]any)[0], "has_instance_token")

		code, body = e.do(t, "alice", http.MethodGet, "/discover/models?search=acme&sort=downloads&limit=5", nil)
		require.Equal(t, http.StatusOK, code, body)
		items := body["items"].([]any)
		require.Len(t, items, 2)
		for _, it := range items {
			require.Equal(t, "none", it.(map[string]any)["arsenal"])
		}
		code, _ = e.do(t, "alice", http.MethodGet, "/discover/models?sort=stars", nil)
		require.Equal(t, http.StatusBadRequest, code)
		code, _ = e.do(t, "alice", http.MethodGet, "/discover/models?limit=500", nil)
		require.Equal(t, http.StatusBadRequest, code)
		code, _ = e.do(t, "alice", http.MethodGet, "/discover/models?upstream=nope", nil)
		require.Equal(t, http.StatusNotFound, code)

		code, body = e.do(t, "alice", http.MethodGet, "/discover/models/acme/api-GGUF", nil)
		require.Equal(t, http.StatusOK, code, body)
		require.Len(t, body["quants"], 2)
		require.Len(t, body["dependencies"], 1)
		require.Equal(t, "# card", body["card"])
		code, _ = e.do(t, "alice", http.MethodGet, "/discover/models/acme/missing", nil)
		require.Equal(t, http.StatusNotFound, code)

		// Start a Pull of one quant, and follow it.
		code, _ = e.do(t, "ro", http.MethodPost, "/pulls", map[string]any{"upstream": "huggingface", "repo": "acme/api-GGUF", "type": "model"})
		require.Equal(t, http.StatusForbidden, code, "a read token cannot pull")
		code, _ = e.do(t, "alice", http.MethodPost, "/pulls", map[string]any{"upstream": "huggingface", "repo": "acme/api-GGUF", "type": "model", "bogus": 1})
		require.Equal(t, http.StatusBadRequest, code)
		code, body = e.do(t, "alice", http.MethodPost, "/pulls", map[string]any{"upstream": "huggingface", "repo": "acme/api-GGUF", "type": "model",
			"selection": map[string]any{"quants": []string{"Q4_K_M"}}, "track": true})
		require.Equal(t, http.StatusAccepted, code, body)
		require.Equal(t, "pull", body["kind"])
		pullID := body["id"].(string)
		h.Run(10)
		code, body = e.do(t, "alice", http.MethodGet, "/pulls/"+pullID, nil)
		require.Equal(t, http.StatusOK, code)
		require.Equal(t, "succeeded", body["status"], body)
		require.EqualValues(t, 1, body["progress"].(map[string]any)["files_done"])
		require.Equal(t, "done", body["result"].(map[string]any)["phase"])
		bob := h.User("bob")
		e.authn.Add("bob", bob)
		code, _ = e.do(t, "bob", http.MethodGet, "/pulls/"+pullID, nil)
		require.Equal(t, http.StatusNotFound, code, "other users do not see someone's Pull")
		code, _ = e.do(t, "root", http.MethodGet, "/pulls/"+pullID, nil)
		require.Equal(t, http.StatusOK, code)

		// Mirror settings and state.
		code, body = e.do(t, "alice", http.MethodGet, "/repos/models/acme/api-GGUF/mirror", nil)
		require.Equal(t, http.StatusOK, code, body)
		require.Equal(t, "huggingface", body["upstream"])
		require.Equal(t, "acme/api-GGUF", body["upstream_repo"])
		require.Equal(t, []any{"Q4_K_M"}, body["selection"].(map[string]any)["quants"])
		require.Equal(t, true, body["track"])
		require.Equal(t, false, body["update_available"])
		require.Equal(t, "ok", body["status"])

		code, body = e.do(t, "alice", http.MethodPatch, "/repos/models/acme/api-GGUF/mirror", map[string]any{"schedule": "15 3 * * *"})
		require.Equal(t, http.StatusOK, code, body)
		require.Equal(t, "15 3 * * *", body["schedule"])
		require.Equal(t, "15 3 * * *", h.Queue.Schedules()[0].Cron)
		code, _ = e.do(t, "alice", http.MethodPatch, "/repos/models/acme/api-GGUF/mirror", map[string]any{"schedule": "often"})
		require.Equal(t, http.StatusBadRequest, code)

		code, body = e.do(t, "alice", http.MethodPost, "/repos/models/acme/api-GGUF/mirror/pull", map[string]any{"quants": []string{"Q8_0"}})
		require.Equal(t, http.StatusAccepted, code, body)
		code, body = e.do(t, "alice", http.MethodPost, "/repos/models/acme/api-GGUF/mirror/sync", nil)
		require.Equal(t, http.StatusAccepted, code, body)
		h.Run(10)

		code, body = e.do(t, "alice", http.MethodGet, "/repos/models/acme/api-GGUF/dependencies", nil)
		require.Equal(t, http.StatusOK, code, body)
		require.Len(t, body["needs"], 1)
		need := body["needs"].([]any)[0].(map[string]any)
		require.Equal(t, "acme/code", need["repo"])
		require.Equal(t, "code", need["kind"])
		require.Equal(t, "full", need["arsenal"], "the code Dependency was pulled with the model")
		code, _ = e.do(t, "", http.MethodGet, "/repos/models/acme/missing/dependencies", nil)
		require.Equal(t, http.StatusUnauthorized, code)

		code, _ = e.do(t, "alice", http.MethodPost, "/repos/models/acme/api-GGUF/mirror/detach", nil)
		require.Equal(t, http.StatusForbidden, code, "detaching needs admin")
		code, body = e.do(t, "root", http.MethodPost, "/repos/models/acme/api-GGUF/mirror/detach", nil)
		require.Equal(t, http.StatusOK, code, body)
		require.Equal(t, false, body["mirror"])
		code, _ = e.do(t, "alice", http.MethodPost, "/repos/models/acme/api-GGUF/mirror/sync", nil)
		require.Equal(t, http.StatusNotFound, code)

		// Admin Upstreams.
		code, _ = e.do(t, "alice", http.MethodGet, "/admin/upstreams", nil)
		require.Equal(t, http.StatusForbidden, code)
		code, body = e.do(t, "root", http.MethodPost, "/admin/upstreams", map[string]any{"name": "corp", "base_url": h.Server.URL,
			"instance_token": "hgr_secret", "namespace_prefix": "corp-"})
		require.Equal(t, http.StatusCreated, code, body)
		require.Equal(t, true, body["has_instance_token"])
		require.NotContains(t, strings.ToLower(mustJSON(t, body)), "hgr_secret", "the token is never returned")
		id := body["id"].(string)
		code, body = e.do(t, "root", http.MethodPost, "/admin/upstreams/"+id+"/test", nil)
		require.Equal(t, http.StatusOK, code)
		require.Equal(t, true, body["ok"], body)
		code, _ = e.do(t, "root", http.MethodPost, "/admin/upstreams", map[string]any{"name": "corp", "base_url": h.Server.URL})
		require.Equal(t, http.StatusConflict, code)
		code, _ = e.do(t, "root", http.MethodPost, "/admin/upstreams", map[string]any{"name": "x", "base_url": "ftp://nope"})
		require.Equal(t, http.StatusBadRequest, code)
		code, body = e.do(t, "root", http.MethodPatch, "/admin/upstreams/"+id, map[string]any{"enabled": false, "instance_token": ""})
		require.Equal(t, http.StatusOK, code, body)
		require.Equal(t, false, body["enabled"])
		require.Equal(t, false, body["has_instance_token"])
		code, body = e.do(t, "root", http.MethodGet, "/admin/upstreams", nil)
		require.Equal(t, http.StatusOK, code)
		require.Len(t, body["items"], 2)
		code, body = e.do(t, "root", http.MethodGet, "/admin/upstreams/"+id, nil)
		require.Equal(t, http.StatusOK, code)
		require.Equal(t, "corp", body["name"])
		hf := ""
		for _, it := range e.mustList(t) {
			if it["name"] == "huggingface" {
				hf = it["id"].(string)
			}
		}
		code, _ = e.do(t, "root", http.MethodDelete, "/admin/upstreams/"+hf, nil)
		require.Equal(t, http.StatusConflict, code, "configured Upstreams are removed in the configuration file")
		code, _ = e.do(t, "root", http.MethodDelete, "/admin/upstreams/"+id, nil)
		require.Equal(t, http.StatusNoContent, code)
		code, _ = e.do(t, "root", http.MethodGet, "/admin/upstreams/"+id, nil)
		require.Equal(t, http.StatusNotFound, code)
	})
}

func (e *upstreamsEnv) mustList(t *testing.T) []map[string]any {
	_, body := e.do(t, "root", http.MethodGet, "/admin/upstreams", nil)
	var out []map[string]any
	for _, it := range body["items"].([]any) {
		out = append(out, it.(map[string]any))
	}
	return out
}

func mustJSON(t *testing.T, v any) string {
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return string(b)
}
