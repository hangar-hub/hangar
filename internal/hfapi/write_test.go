package hfapi_test

import (
	"context"
	"encoding/base64"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/hfapi"
	"github.com/jamesagarside/hangar/internal/hfapi/hfapitest"
	"github.com/jamesagarside/hangar/internal/model"
)

func TestCreateRepo(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	r := e.Request(t, http.MethodPost, "/api/repos/create", "alice", `{"name":"m1","organization":null,"visibility":"private"}`) //nolint:misspell // Hub API field name.
	require.Equal(t, http.StatusOK, r.StatusCode)
	require.Equal(t, e.URL+"/alice/m1", decode[map[string]any](t, r)["url"])
	rp, err := e.Repos.Get(context.Background(), modelID("alice", "m1"))
	require.NoError(t, err)
	require.Equal(t, model.VisibilityPrivate, rp.Visibility)
	_, err = e.Repos.Resolve(context.Background(), rp.ID, "main")
	require.NoError(t, err, "main resolves straight away")

	// Legacy `private`, datasets, and Organisations.
	r = e.Request(t, http.MethodPost, "/api/repos/create", "alice", `{"name":"d1","organization":"acme","private":false,"type":"dataset"}`) //nolint:misspell // Hub API field name.
	require.Equal(t, http.StatusOK, r.StatusCode)
	require.Equal(t, e.URL+"/datasets/acme/d1", decode[map[string]any](t, r)["url"])
	// A 409 still carries the URL for exist_ok.
	r = e.Request(t, http.MethodPost, "/api/repos/create", "alice", `{"name":"m1"}`)
	require.Equal(t, http.StatusConflict, r.StatusCode)
	body := decode[map[string]any](t, r)
	require.Equal(t, e.URL+"/alice/m1", body["url"])
	require.NotContains(t, body["error"], "conflicting operation")

	require.Equal(t, http.StatusUnauthorized, e.Request(t, http.MethodPost, "/api/repos/create", "", `{"name":"x"}`).StatusCode)
	require.Equal(t, http.StatusForbidden, e.Request(t, http.MethodPost, "/api/repos/create", "bob", `{"name":"x","organization":"acme"}`).StatusCode) //nolint:misspell // Hub API field name.
	require.Equal(t, http.StatusForbidden, e.Request(t, http.MethodPost, "/api/repos/create", "carol", `{"name":"x"}`).StatusCode)
	require.Equal(t, http.StatusNotFound, e.Request(t, http.MethodPost, "/api/repos/create", "alice", `{"name":"x","organization":"nobody"}`).StatusCode) //nolint:misspell // Hub API field name.
	require.Equal(t, http.StatusBadRequest, e.Request(t, http.MethodPost, "/api/repos/create", "alice", `{"name":"x","type":"space","sdk":"gradio"}`).StatusCode)
	require.Equal(t, http.StatusBadRequest, e.Request(t, http.MethodPost, "/api/repos/create", "alice", `{"name":"x","type":"thing"}`).StatusCode)
	require.Equal(t, http.StatusBadRequest, e.Request(t, http.MethodPost, "/api/repos/create", "alice", `{"name":"bad--name"}`).StatusCode)
	require.Equal(t, http.StatusBadRequest, e.Request(t, http.MethodPost, "/api/repos/create", "alice", `{"name":"x","visibility":"secret"}`).StatusCode)
	require.Equal(t, http.StatusBadRequest, e.Request(t, http.MethodPost, "/api/repos/create", "alice", `nope`).StatusCode)

}

func TestDeleteMoveAndSettings(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	seed(t, e, "alice", modelID("alice", "a"), model.VisibilityPublic, map[string]string{"config.json": configJSON})
	seed(t, e, "alice", modelID("alice", "b"), model.VisibilityPublic, nil)

	r := e.Request(t, http.MethodPut, "/api/models/alice/a/settings", "alice", `{"visibility":"private","gated":"auto"}`)
	require.Equal(t, http.StatusOK, r.StatusCode)
	info := decode[map[string]any](t, e.Request(t, http.MethodGet, "/api/models/alice/a", "alice", ""))
	require.Equal(t, true, info["private"])
	require.Equal(t, "auto", info["gated"])
	require.Equal(t, http.StatusOK, e.Request(t, http.MethodPut, "/api/models/alice/a/settings", "alice", `{"private":false,"gated":false}`).StatusCode)
	info = decode[map[string]any](t, e.Request(t, http.MethodGet, "/api/models/alice/a", "", ""))
	require.Equal(t, false, info["gated"])
	for _, bad := range []string{`{"gated":true}`, `{"gated":"sometimes"}`, `{"gated":3}`, `{"gated":[}`, `{"visibility":"x"}`} {
		require.Equal(t, http.StatusBadRequest, e.Request(t, http.MethodPut, "/api/models/alice/a/settings", "alice", bad).StatusCode, bad)
	}
	require.Equal(t, http.StatusForbidden, e.Request(t, http.MethodPut, "/api/models/alice/a/settings", "bob", `{"private":true}`).StatusCode)

	// Move to another name and Namespace; the old ID redirects.
	r = e.Request(t, http.MethodPost, "/api/repos/move", "alice", `{"fromRepo":"alice/a","toRepo":"acme/renamed","type":"model"}`)
	require.Equal(t, http.StatusOK, r.StatusCode, string(readBody(t, r)))
	r = e.Request(t, http.MethodGet, "/api/models/alice/a", "", "")
	require.Equal(t, http.StatusTemporaryRedirect, r.StatusCode)
	require.Equal(t, "/api/models/acme/renamed", r.Header.Get("Location"))
	require.Equal(t, http.StatusForbidden, e.Request(t, http.MethodPost, "/api/repos/move", "alice", `{"fromRepo":"alice/b","toRepo":"bob/b"}`).StatusCode)
	require.Equal(t, http.StatusNotFound, e.Request(t, http.MethodPost, "/api/repos/move", "alice", `{"fromRepo":"alice/b","toRepo":"ghost/b"}`).StatusCode)
	require.Equal(t, http.StatusBadRequest, e.Request(t, http.MethodPost, "/api/repos/move", "alice", `{"fromRepo":"b","toRepo":"alice/c"}`).StatusCode)
	require.Equal(t, http.StatusBadRequest, e.Request(t, http.MethodPost, "/api/repos/move", "alice", `{"fromRepo":"alice/b","toRepo":"c"}`).StatusCode)
	require.Equal(t, http.StatusBadRequest, e.Request(t, http.MethodPost, "/api/repos/move", "alice", `{"fromRepo":"alice/b","toRepo":"alice/c","type":"space"}`).StatusCode)

	// Delete.
	requireHubError(t, e.Request(t, http.MethodDelete, "/api/repos/delete", "bob", `{"name":"b","organization":"alice"}`), http.StatusForbidden, "") //nolint:misspell // Hub API field name.
	require.Equal(t, http.StatusOK, e.Request(t, http.MethodDelete, "/api/repos/delete", "alice", `{"name":"b","organization":null}`).StatusCode)    //nolint:misspell // Hub API field name.
	requireHubError(t, e.Request(t, http.MethodDelete, "/api/repos/delete", "alice", `{"name":"b"}`), http.StatusNotFound, "RepoNotFound")
	requireHubError(t, e.Request(t, http.MethodDelete, "/api/repos/delete", "alice", `{"name":"b--"}`), http.StatusNotFound, "RepoNotFound")
	require.Equal(t, http.StatusBadRequest, e.Request(t, http.MethodDelete, "/api/repos/delete", "alice", `{"name":"b","type":"kernel"}`).StatusCode)
	require.Equal(t, http.StatusNotFound, e.Request(t, http.MethodPost, "/api/repos/other", "alice", `{}`).StatusCode)
}

func TestValidateYAML(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	post := func(content string) *http.Response {
		return e.Request(t, http.MethodPost, "/api/validate-yaml", "", mustJSON(t, map[string]string{"content": content, "repoType": "model"}))
	}
	body := decode[map[string]any](t, post("---\nlicense: mit\ntags: [a]\n---\n# Card\n"))
	require.Equal(t, []any{}, body["errors"])
	require.Equal(t, []any{}, body["warnings"])
	body = decode[map[string]any](t, post("# No metadata\n"))
	require.Len(t, body["warnings"], 1)
	body = decode[map[string]any](t, post("---\n---\n"))
	require.Len(t, body["warnings"], 1)
	r := post("---\nlicense: [mit\n---\n")
	require.Equal(t, http.StatusBadRequest, r.StatusCode)
	require.Equal(t, "application/json; charset=utf-8", r.Header.Get("Content-Type"))
	require.Len(t, decode[map[string]any](t, r)["errors"], 1)

	front, ok := hfapi.FrontMatter("\uFEFF---\r\na: 1\r\n---")
	require.True(t, ok)
	require.Equal(t, "a: 1", front)
	_, ok = hfapi.FrontMatter("---\na: 1\n")
	require.False(t, ok)
	data, err := hfapi.ParseCardData("")
	require.NoError(t, err)
	require.Empty(t, data)
}

func TestPreupload(t *testing.T) {
	t.Parallel()
	e := newEnv(t, func(o *hfapitest.Options) {
		o.Settings = func(s *hfapi.Settings) { s.RegularUploadMax = 1000 }
	})
	small := "small"
	_, sha := seed(t, e, "alice", modelID("alice", "up"), model.VisibilityPublic, map[string]string{
		"config.json": configJSON, "model.safetensors": small, ".gitignore": "*.log\nbuild/\n",
	})
	req := mustJSON(t, map[string]any{"files": []map[string]any{
		{"path": "config.json", "sample": base64.StdEncoding.EncodeToString([]byte(configJSON)), "size": len(configJSON)},
		{"path": "model.safetensors", "sample": "", "size": len(small)},
		{"path": "big.txt", "sample": "", "size": 5000},
		{"path": "debug.log", "sample": "", "size": 1},
		{"path": "build/out.txt", "sample": "", "size": 1},
		{"path": "weights/x.bin", "sample": "", "size": 10},
	}})
	r := e.Request(t, http.MethodPost, "/api/models/alice/up/preupload/main", "alice", req)
	body := decode[map[string]any](t, r)
	require.Equal(t, string(sha), body["commitOid"])
	files := map[string]map[string]any{}
	for _, f := range body["files"].([]any) {
		m := f.(map[string]any)
		files[m["path"].(string)] = m
	}
	require.Equal(t, "regular", files["config.json"]["uploadMode"])
	require.Equal(t, model.GitBlobID([]byte(configJSON)), files["config.json"]["oid"])
	require.Equal(t, "lfs", files["model.safetensors"]["uploadMode"])
	require.Equal(t, sha256hex(small), files["model.safetensors"]["oid"])
	require.Equal(t, "lfs", files["big.txt"]["uploadMode"])
	require.Nil(t, files["big.txt"]["oid"])
	require.Equal(t, "lfs", files["weights/x.bin"]["uploadMode"])
	require.Equal(t, true, files["debug.log"]["shouldIgnore"])
	require.Equal(t, true, files["build/out.txt"]["shouldIgnore"])
	require.Equal(t, false, files["config.json"]["shouldIgnore"])

	// The .gitignore being uploaded replaces the Repo's.
	req = mustJSON(t, map[string]any{"files": []map[string]any{{"path": "debug.log", "size": 1}}, "gitIgnore": "*.tmp\n"})
	body = decode[map[string]any](t, e.Request(t, http.MethodPost, "/api/models/alice/up/preupload/main", "alice", req))
	require.Equal(t, false, body["files"].([]any)[0].(map[string]any)["shouldIgnore"])

	require.Equal(t, http.StatusForbidden, e.Request(t, http.MethodPost, "/api/models/alice/up/preupload/main", "bob", `{"files":[]}`).StatusCode)
	require.Equal(t, http.StatusOK, e.Request(t, http.MethodPost, "/api/models/alice/up/preupload/main?create_pr=1", "bob", `{"files":[]}`).StatusCode)
	require.Equal(t, http.StatusBadRequest, e.Request(t, http.MethodPost, "/api/models/alice/up/preupload/main", "alice", `{"files":[{"path":"/abs","size":1}]}`).StatusCode)
	requireHubError(t, e.Request(t, http.MethodPost, "/api/models/alice/up/preupload/nope", "alice", `{"files":[]}`), http.StatusNotFound, "RevisionNotFound")
	tooMany := mustJSON(t, map[string]any{"files": make([]map[string]any, 1001)})
	require.Equal(t, http.StatusBadRequest, e.Request(t, http.MethodPost, "/api/models/alice/up/preupload/main", "alice", tooMany).StatusCode)
}

func TestPreuploadRejectMode(t *testing.T) {
	t.Parallel()
	e := newEnv(t, func(o *hfapitest.Options) {
		o.Settings = func(s *hfapi.Settings) { s.XetReject = true; s.RegularUploadMax = 100 }
	})
	seed(t, e, "alice", modelID("alice", "rj"), model.VisibilityPublic, nil)
	req := `{"files":[{"path":"model.safetensors","size":100},{"path":"big.safetensors","size":101}]}`
	body := decode[map[string]any](t, e.Request(t, http.MethodPost, "/api/models/alice/rj/preupload/main", "alice", req))
	files := body["files"].([]any)
	require.Equal(t, "regular", files[0].(map[string]any)["uploadMode"])
	require.Equal(t, "lfs", files[1].(map[string]any)["uploadMode"])

	r := e.Request(t, http.MethodGet, "/api/models/alice/rj/xet-write-token/main", "alice", "")
	require.Equal(t, http.StatusBadRequest, r.StatusCode)
	require.Equal(t, "Xet uploads are turned off on this Hangar. Set HF_HUB_DISABLE_XET=1 to upload.", r.Header.Get("X-Error-Message"))
}

func ndjson(t *testing.T, lines ...map[string]any) string {
	t.Helper()
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(mustJSON(t, l))
		b.WriteByte('\n')
	}
	return b.String()
}

func header(summary string, extra ...string) map[string]any {
	v := map[string]any{"summary": summary, "description": ""}
	if len(extra) == 1 {
		v["parentCommit"] = extra[0]
	}
	return map[string]any{"key": "header", "value": v}
}

func fileLine(p, content string) map[string]any {
	return map[string]any{"key": "file", "value": map[string]any{"path": p, "content": base64.StdEncoding.EncodeToString([]byte(content)), "encoding": "base64"}}
}

func TestCommit(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	_, base := seed(t, e, "alice", modelID("alice", "c"), model.VisibilityPublic, map[string]string{"old.bin": "old-weights", "logs/a.txt": "a", "logs/b.txt": "b"})
	weights := "new weights"
	_, err := e.LFS.Ingest(context.Background(), sha256hex(weights), int64(len(weights)), strings.NewReader(weights))
	require.NoError(t, err)
	require.NoError(t, e.LFS.RecordUploader(context.Background(), sha256hex(weights), e.Users["alice"].Principal.UserID))

	body := ndjson(t,
		header("Upload model", string(base)),
		fileLine("config.json", configJSON),
		map[string]any{"key": "file", "value": map[string]any{"path": "notes.txt", "content": "plain"}},
		map[string]any{"key": "lfsFile", "value": map[string]any{"path": "model.safetensors", "algo": "sha256", "oid": sha256hex(weights), "size": len(weights)}},
		map[string]any{"key": "lfsFile", "value": map[string]any{"path": "copy.safetensors", "algo": "sha256", "oid": sha256hex(weights)}},
		map[string]any{"key": "deletedFile", "value": map[string]any{"path": "old.bin"}},
		map[string]any{"key": "deletedFolder", "value": map[string]any{"path": "logs/"}},
	)
	r := e.Request(t, http.MethodPost, "/api/models/alice/c/commit/main", "alice", body, "Content-Type", "application/x-ndjson")
	require.Equal(t, http.StatusOK, r.StatusCode, string(readBody(t, r)))
	res := decode[map[string]any](t, r)
	sha := res["commitOid"].(string)
	require.Equal(t, e.URL+"/alice/c/commit/"+sha, res["commitUrl"])
	require.NotContains(t, res, "pullRequestUrl")

	r = e.Request(t, http.MethodGet, "/alice/c/resolve/main/copy.safetensors", "", "")
	require.Equal(t, weights, string(readBody(t, r)))
	require.Equal(t, "plain", string(readBody(t, e.Request(t, http.MethodGet, "/alice/c/resolve/main/notes.txt", "", ""))))
	requireHubError(t, e.Request(t, http.MethodGet, "/alice/c/resolve/main/old.bin", "", ""), http.StatusNotFound, "EntryNotFound")
	requireHubError(t, e.Request(t, http.MethodGet, "/alice/c/resolve/main/logs/a.txt", "", ""), http.StatusNotFound, "EntryNotFound")

	commit := func(user, path, body string) *http.Response {
		return e.Request(t, http.MethodPost, path, user, body, "Content-Type", "application/x-ndjson")
	}
	// Stale parent, missing files and objects, bad bodies.
	require.Equal(t, http.StatusPreconditionFailed, commit("alice", "/api/models/alice/c/commit/main", ndjson(t, header("x", string(base)), fileLine("y", "y"))).StatusCode)
	require.Equal(t, http.StatusPreconditionFailed, commit("alice", "/api/models/alice/c/commit/main", ndjson(t, header("x", "zzzzz"), fileLine("y", "y"))).StatusCode)
	r = commit("alice", "/api/models/alice/c/commit/main", ndjson(t, header("x"), map[string]any{"key": "deletedFile", "value": map[string]any{"path": "ghost.txt"}}))
	requireHubError(t, r, http.StatusNotFound, "EntryNotFound")
	require.Contains(t, r.Header.Get("X-Error-Message"), hfapi.MsgFileMissing)
	r = commit("alice", "/api/models/alice/c/commit/main", ndjson(t, header("x"), map[string]any{"key": "lfsFile", "value": map[string]any{"path": "m.bin", "algo": "sha256", "oid": strings.Repeat("a", 64), "size": 3}}))
	require.Equal(t, http.StatusBadRequest, r.StatusCode)
	for name, bad := range map[string]string{
		"no header":   ndjson(t, fileLine("a", "a")),
		"two headers": ndjson(t, header("x"), header("y")),
		"empty":       "\n\n",
		"not json":    "{nope\n",
		"empty sum":   ndjson(t, header(" "), fileLine("a", "a")),
		"bad base64":  ndjson(t, header("x"), map[string]any{"key": "file", "value": map[string]any{"path": "a", "content": "!!", "encoding": "base64"}}),
		"bad enc":     ndjson(t, header("x"), map[string]any{"key": "file", "value": map[string]any{"path": "a", "content": "a", "encoding": "rot13"}}),
		"bad algo":    ndjson(t, header("x"), map[string]any{"key": "lfsFile", "value": map[string]any{"path": "a", "algo": "md5", "oid": "x"}}),
		"bad oid":     ndjson(t, header("x"), map[string]any{"key": "lfsFile", "value": map[string]any{"path": "a", "algo": "sha256", "oid": "x"}}),
		"unknown key": ndjson(t, header("x"), map[string]any{"key": "copy", "value": map[string]any{}}),
		"bad path":    ndjson(t, header("x"), fileLine("../etc/passwd", "a")),
		"git path":    ndjson(t, header("x"), fileLine(".git/config", "a")),
		"no ops":      ndjson(t, header("x")),
		"bad value":   `{"key":"header","value":[]}` + "\n",
		"bad file":    ndjson(t, header("x")) + `{"key":"file","value":[]}` + "\n",
		"bad lfs":     ndjson(t, header("x")) + `{"key":"lfsFile","value":[]}` + "\n",
		"bad delete":  ndjson(t, header("x")) + `{"key":"deletedFile","value":[]}` + "\n",
	} {
		require.Equal(t, http.StatusBadRequest, commit("alice", "/api/models/alice/c/commit/main", bad).StatusCode, name)
	}
	requireHubError(t, commit("alice", "/api/models/alice/c/commit/nope", ndjson(t, header("x"), fileLine("a", "a"))), http.StatusNotFound, "RevisionNotFound")
	require.Equal(t, http.StatusForbidden, commit("bob", "/api/models/alice/c/commit/main", ndjson(t, header("x"), fileLine("a", "a"))).StatusCode)
	require.Equal(t, http.StatusUnauthorized, commit("", "/api/models/alice/c/commit/main", ndjson(t, header("x"), fileLine("a", "a"))).StatusCode)
}

func TestCommitLineLimit(t *testing.T) {
	t.Parallel()
	e := newEnv(t, func(o *hfapitest.Options) {
		o.Settings = func(s *hfapi.Settings) { s.CommitLineMax = 4096 }
	})
	seed(t, e, "alice", modelID("alice", "lim"), model.VisibilityPublic, nil)
	body := ndjson(t, header("x"), fileLine("big.txt", strings.Repeat("z", 8192)))
	r := e.Request(t, http.MethodPost, "/api/models/alice/lim/commit/main", "alice", body)
	require.Equal(t, http.StatusRequestEntityTooLarge, r.StatusCode)
}

func TestCreatePR(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	_, base := seed(t, e, "alice", modelID("alice", "pr"), model.VisibilityPublic, map[string]string{"a.txt": "a"})
	// create_commit(create_pr=True): one call opens the pull request and commits to refs/pr/N.
	r := e.Request(t, http.MethodPost, "/api/models/alice/pr/commit/main?create_pr=1", "bob", ndjson(t, header("Fix typo", string(base)), fileLine("a.txt", "b")))
	require.Equal(t, http.StatusOK, r.StatusCode, string(readBody(t, r)))
	res := decode[map[string]any](t, r)
	require.Equal(t, e.URL+"/alice/pr/discussions/1", res["pullRequestUrl"])
	require.Equal(t, "b", string(readBody(t, e.Request(t, http.MethodGet, "/alice/pr/resolve/refs%2Fpr%2F1/a.txt", "", ""))))
	require.Equal(t, "a", string(readBody(t, e.Request(t, http.MethodGet, "/alice/pr/resolve/main/a.txt", "", ""))))
	require.Equal(t, http.StatusPreconditionFailed,
		e.Request(t, http.MethodPost, "/api/models/alice/pr/commit/main?create_pr=1", "bob", ndjson(t, header("x", strings.Repeat("0", 40)), fileLine("a.txt", "c"))).StatusCode)

	// The Xet pipeline opens the pull request first, then commits to its ref.
	r = e.Request(t, http.MethodPost, "/api/models/alice/pr/discussions", "bob", `{"title":"Upload","description":"d","pullRequest":true}`)
	require.Equal(t, http.StatusOK, r.StatusCode)
	num := decode[map[string]any](t, r)["num"]
	require.EqualValues(t, 2, num)
	r = e.Request(t, http.MethodPost, "/api/models/alice/pr/commit/refs%2Fpr%2F2", "bob", ndjson(t, header("Upload (part 1)"), fileLine("c.txt", "c")))
	require.Equal(t, http.StatusOK, r.StatusCode, string(readBody(t, r)))
	r = e.Request(t, http.MethodPost, "/api/models/alice/pr/commit/refs%2Fpr%2F2", "bob", ndjson(t, header("Upload (part 2)"), fileLine("d.txt", "d")))
	require.Equal(t, http.StatusOK, r.StatusCode)
	// Others may not push to bob's pull request; alice, a writer, may.
	require.Equal(t, http.StatusForbidden, e.Request(t, http.MethodPost, "/api/models/alice/pr/commit/refs%2Fpr%2F2", "carol", ndjson(t, header("x"), fileLine("e.txt", "e"))).StatusCode)
	require.Equal(t, http.StatusOK, e.Request(t, http.MethodPost, "/api/models/alice/pr/commit/refs%2Fpr%2F2", "alice", ndjson(t, header("x"), fileLine("e.txt", "e"))).StatusCode)
	requireHubError(t, e.Request(t, http.MethodPost, "/api/models/alice/pr/commit/refs%2Fpr%2F9", "alice", ndjson(t, header("x"), fileLine("e.txt", "e"))), http.StatusNotFound, "RevisionNotFound")
}
