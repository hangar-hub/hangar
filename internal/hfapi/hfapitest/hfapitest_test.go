package hfapitest_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/hfapi"
	"github.com/jamesagarside/hangar/internal/hfapi/hfapitest"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/repo"
)

func TestEnvServesTheHubAPI(t *testing.T) {
	t.Parallel()
	e := hfapitest.New(t, hfapitest.Options{Settings: func(s *hfapi.Settings) { s.DefaultNamespace = hfapitest.Org }})
	require.Len(t, e.Users, 4)
	r := e.Request(t, http.MethodGet, "/api/whoami-v2", "alice", "")
	require.Equal(t, http.StatusOK, r.StatusCode)
	r = e.Request(t, http.MethodGet, "/api/whoami-v2", "", "", "Authorization", "Bearer nope")
	require.Equal(t, http.StatusUnauthorized, r.StatusCode)
	require.Equal(t, "Invalid credentials in Authorization header", r.Header.Get("X-Error-Message"))
	require.Equal(t, http.StatusOK, e.Request(t, http.MethodGet, "/healthz", "", "").StatusCode)
	require.Equal(t, http.StatusTemporaryRedirect, e.Request(t, http.MethodGet, "/api/models/gpt2", "", "").StatusCode)

	// alice administers acme: Repos she creates there are hers to write.
	_, err := e.Repos.Create(context.Background(), repo.CreateParams{ID: model.RepoID{Type: model.RepoTypeModel, Namespace: hfapitest.Org, Name: "m"}, Actor: e.Users["alice"].Principal})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, e.Request(t, http.MethodPost, "/api/models/acme/m/branch/dev", "alice", "").StatusCode)
	require.Equal(t, http.StatusForbidden, e.Request(t, http.MethodPost, "/api/models/acme/m/branch/dev2", "bob", "").StatusCode)
}

func TestLFSStandIn(t *testing.T) {
	t.Parallel()
	e := hfapitest.New(t, hfapitest.Options{})
	_, err := e.Repos.Create(context.Background(), repo.CreateParams{ID: model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "m"}, Actor: e.Users["alice"].Principal})
	require.NoError(t, err)
	content := "weights"
	sum := sha256.Sum256([]byte(content))
	oid := hex.EncodeToString(sum[:])
	batch := `{"operation":"upload","transfers":["basic"],"objects":[{"oid":"` + oid + `","size":7},{"oid":"bad","size":1}]}`
	r := e.Request(t, http.MethodPost, "/alice/m.git/info/lfs/objects/batch", "alice", batch)
	require.Equal(t, http.StatusOK, r.StatusCode)
	var resp struct {
		Objects []struct {
			OID     string `json:"oid"`
			Actions map[string]struct {
				Href string `json:"href"`
			} `json:"actions"`
			Error *struct{ Code int } `json:"error"`
		} `json:"objects"`
	}
	require.NoError(t, json.NewDecoder(r.Body).Decode(&resp))
	require.Equal(t, 422, resp.Objects[1].Error.Code)
	href := resp.Objects[0].Actions["upload"].Href
	require.True(t, strings.HasPrefix(href, e.URL+"/lfs-standin/"))

	put := func(body string) int {
		req, err := http.NewRequest(http.MethodPut, href, strings.NewReader(body))
		require.NoError(t, err)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	require.Equal(t, http.StatusOK, put(content))
	require.Equal(t, http.StatusNotFound, put(content), "upload URLs are single use")
	_, err = e.LFS.Stat(context.Background(), oid)
	require.NoError(t, err)

	// Known objects need no upload; downloads of unknown ones fail; bad bodies and anonymous callers are refused.
	r = e.Request(t, http.MethodPost, "/alice/m.git/info/lfs/objects/batch", "alice", `{"operation":"upload","objects":[{"oid":"`+oid+`","size":7}]}`)
	require.NotContains(t, decodeMap(t, r)["objects"].([]any)[0], "actions")
	other := strings.Repeat("b", 64)
	r = e.Request(t, http.MethodPost, "/alice/m.git/info/lfs/objects/batch", "alice", `{"operation":"download","objects":[{"oid":"`+other+`","size":7}]}`)
	require.Contains(t, decodeMap(t, r)["objects"].([]any)[0], "error")
	require.Equal(t, http.StatusBadRequest, e.Request(t, http.MethodPost, "/alice/m.git/info/lfs/objects/batch", "alice", `{`).StatusCode)
	require.Equal(t, http.StatusUnauthorized, e.Request(t, http.MethodPost, "/alice/m.git/info/lfs/objects/batch", "", batch).StatusCode)
	require.Equal(t, http.StatusNotFound, e.Request(t, http.MethodGet, "/alice/m.git/info/lfs/locks", "alice", "").StatusCode)

	// A mismatched upload is rejected.
	r = e.Request(t, http.MethodPost, "/alice/m.git/info/lfs/objects/batch", "alice", `{"operation":"upload","objects":[{"oid":"`+other+`","size":3}]}`)
	href = decodeMap(t, r)["objects"].([]any)[0].(map[string]any)["actions"].(map[string]any)["upload"].(map[string]any)["href"].(string)
	require.Equal(t, http.StatusBadRequest, put("abc"))
}

func decodeMap(t *testing.T, r *http.Response) map[string]any {
	t.Helper()
	var m map[string]any
	require.NoError(t, json.NewDecoder(r.Body).Decode(&m))
	return m
}
