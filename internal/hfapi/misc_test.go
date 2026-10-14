package hfapi_test

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/config"
	"github.com/jamesagarside/hangar/internal/datasets"
	"github.com/jamesagarside/hangar/internal/datasets/datasetstest"
	"github.com/jamesagarside/hangar/internal/hfapi"
	"github.com/jamesagarside/hangar/internal/model"
)

func TestWhoami(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	r := e.Request(t, http.MethodGet, "/api/whoami-v2", "alice", "")
	body := decode[map[string]any](t, r)
	require.Equal(t, "alice", body["name"])
	require.Equal(t, "user", body["type"])
	orgs := body["orgs"].([]any)
	require.Len(t, orgs, 1)
	require.Equal(t, "acme", orgs[0].(map[string]any)["name"])
	require.Equal(t, "admin", orgs[0].(map[string]any)["roleInOrg"])
	tok := body["auth"].(map[string]any)["accessToken"].(map[string]any)
	require.Equal(t, "alice-laptop", tok["displayName"])
	require.Equal(t, "admin", tok["role"])

	body = decode[map[string]any](t, e.Request(t, http.MethodGet, "/api/whoami-v2", "carol", ""))
	require.Equal(t, []any{}, body["orgs"])
	require.Equal(t, "read", body["auth"].(map[string]any)["accessToken"].(map[string]any)["role"])

	// A session (no Token) reports the session auth type.
	session := e.Users["bob"].Principal
	session.Method, session.Token = model.AuthSession, nil
	e.Authn.Add("cookie-secret", session)
	r = e.Request(t, http.MethodGet, "/api/whoami-v2", "", "", "Cookie", "hangar_session=cookie-secret")
	require.Equal(t, "session", decode[map[string]any](t, r)["auth"].(map[string]any)["type"])

	require.Equal(t, http.StatusUnauthorized, e.Request(t, http.MethodGet, "/api/whoami-v2", "", "").StatusCode)
	require.Equal(t, http.StatusMethodNotAllowed, e.Request(t, http.MethodPost, "/api/whoami-v2", "alice", "").StatusCode)
}

func TestDeviceLogin(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	form := "application/x-www-form-urlencoded"
	r := e.Request(t, http.MethodPost, "/oauth/device", "", "client_id=hf-cli", "Content-Type", form)
	dc := decode[map[string]any](t, r)
	for _, k := range []string{"device_code", "user_code", "verification_uri", "verification_uri_complete", "interval", "expires_in"} {
		require.Contains(t, dc, k)
	}
	poll := func() map[string]any {
		body := url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:device_code"}, "device_code": {dc["device_code"].(string)}, "client_id": {"hf-cli"}}
		return decode[map[string]any](t, e.Request(t, http.MethodPost, "/oauth/token", "", body.Encode(), "Content-Type", form))
	}
	require.Equal(t, "authorization_pending", poll()["error"])
	require.NoError(t, e.Devices.Approve(context.Background(), dc["user_code"].(string), e.Users["alice"].Principal))
	got := poll()
	secret := got["access_token"].(string)
	require.Equal(t, "bearer", got["token_type"])
	require.NotContains(t, got, "refresh_token")
	require.Equal(t, "expired_token", poll()["error"])
	who := decode[map[string]any](t, e.Request(t, http.MethodGet, "/api/whoami-v2", "", "", "Authorization", "Bearer "+secret))
	require.Equal(t, "alice", who["name"])

	r2 := decode[map[string]any](t, e.Request(t, http.MethodPost, "/oauth/device", "", "client_id=x", "Content-Type", form))
	require.NoError(t, e.Devices.Deny(context.Background(), r2["user_code"].(string), e.Users["alice"].Principal))
	body := url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:device_code"}, "device_code": {r2["device_code"].(string)}}
	require.Equal(t, "access_denied", decode[map[string]any](t, e.Request(t, http.MethodPost, "/oauth/token", "", body.Encode(), "Content-Type", form))["error"])

	require.Equal(t, "unsupported_grant_type", decode[map[string]any](t, e.Request(t, http.MethodPost, "/oauth/token", "", "grant_type=refresh_token&refresh_token=x", "Content-Type", form))["error"])
	require.Equal(t, http.StatusBadRequest, e.Request(t, http.MethodPost, "/oauth/device", "", "", "Content-Type", form).StatusCode)
	require.Equal(t, http.StatusBadRequest, e.Request(t, http.MethodPost, "/oauth/device", "", "{}", "Content-Type", "application/json").StatusCode)
	require.Equal(t, http.StatusBadRequest, e.Request(t, http.MethodPost, "/oauth/token", "", "{}", "Content-Type", "application/json").StatusCode)
	require.Equal(t, http.StatusNotFound, e.Request(t, http.MethodPost, "/oauth/other", "", "").StatusCode)
}

func TestDiscussions(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	seed(t, e, "alice", modelID("alice", "talk"), model.VisibilityPublic, map[string]string{"a.txt": "a"})
	base := "/api/models/alice/talk/discussions"
	r := e.Request(t, http.MethodPost, base, "admin", `{"title":" Question ","description":"Why?","pullRequest":false}`)
	d := decode[map[string]any](t, r)
	require.EqualValues(t, 1, d["num"])
	require.Equal(t, "Question", d["title"])
	require.Equal(t, false, d["isPullRequest"])
	require.Equal(t, map[string]any{"name": "alice/talk", "type": "model"}, d["repo"])
	require.Equal(t, http.StatusOK, e.Request(t, http.MethodPost, base, "bob", `{"title":"PR","pullRequest":true}`).StatusCode)

	list := decode[map[string]any](t, e.Request(t, http.MethodGet, base+"?type=all&status=all", "", ""))
	require.EqualValues(t, 2, list["count"])
	require.EqualValues(t, 0, list["start"])
	list = decode[map[string]any](t, e.Request(t, http.MethodGet, base+"?type=pull_request&status=open&p=0", "", ""))
	require.Len(t, list["discussions"], 1)
	require.Equal(t, http.StatusBadRequest, e.Request(t, http.MethodGet, base+"?type=nope", "", "").StatusCode)
	require.Equal(t, http.StatusBadRequest, e.Request(t, http.MethodGet, base+"?status=nope", "", "").StatusCode)

	r = e.Request(t, http.MethodPost, base+"/1/comment", "bob", `{"comment":"Me too"}`)
	msg := decode[map[string]any](t, r)["newMessage"].(map[string]any)
	require.Equal(t, "comment", msg["type"])
	require.Equal(t, "Me too", msg["data"].(map[string]any)["latest"].(map[string]any)["raw"])
	require.Equal(t, http.StatusUnauthorized, e.Request(t, http.MethodPost, base+"/1/comment", "", `{"comment":"x"}`).StatusCode)

	// Only the author or a writer can change status or title.
	require.Equal(t, http.StatusForbidden, e.Request(t, http.MethodPost, base+"/1/comment", "carol", `{"comment":"x"}`).StatusCode, "a read Token cannot comment")
	require.Equal(t, http.StatusForbidden, e.Request(t, http.MethodPost, base+"/1/status", "bob", `{"status":"closed"}`).StatusCode)
	r = e.Request(t, http.MethodPost, base+"/1/status", "admin", `{"status":"closed","comment":"done"}`)
	require.Equal(t, "closed", decode[map[string]any](t, r)["newStatus"].(map[string]any)["data"].(map[string]any)["status"])
	require.Equal(t, http.StatusBadRequest, e.Request(t, http.MethodPost, base+"/1/status", "admin", `{"status":"merged"}`).StatusCode)
	r = e.Request(t, http.MethodPost, base+"/1/title", "alice", `{"title":"Better question"}`)
	title := decode[map[string]any](t, r)["newTitle"].(map[string]any)["data"].(map[string]any)
	require.Equal(t, map[string]any{"from": "Question", "to": "Better question"}, title)
	require.Equal(t, http.StatusBadRequest, e.Request(t, http.MethodPost, base+"/1/title", "alice", `{"title":" "}`).StatusCode)

	details := decode[map[string]any](t, e.Request(t, http.MethodGet, base+"/1?diff=1", "", ""))
	types := []string{}
	for _, ev := range details["events"].([]any) {
		m := ev.(map[string]any)
		types = append(types, m["type"].(string))
		require.Regexp(t, hubDate, m["createdAt"])
	}
	require.Equal(t, []string{"comment", "comment", "status-change", "title-change"}, types)
	require.NotContains(t, details, "changes")
	pr := decode[map[string]any](t, e.Request(t, http.MethodGet, base+"/2", "", ""))
	require.Equal(t, "refs/heads/main", pr["changes"].(map[string]any)["base"])
	require.Equal(t, []any{}, pr["filesWithConflicts"])

	require.Equal(t, http.StatusForbidden, e.Request(t, http.MethodPost, base+"/2/merge", "bob", "").StatusCode)
	require.Equal(t, http.StatusOK, e.Request(t, http.MethodPost, base+"/2/merge", "alice", `{"comment":"thanks"}`).StatusCode)
	require.Equal(t, http.StatusConflict, e.Request(t, http.MethodPost, base+"/2/merge", "alice", "").StatusCode)
	require.Equal(t, http.StatusBadRequest, e.Request(t, http.MethodPost, base+"/1/merge", "alice", "").StatusCode)
	require.Equal(t, http.StatusNotFound, e.Request(t, http.MethodGet, base+"/9", "", "").StatusCode)
	require.Equal(t, http.StatusNotFound, e.Request(t, http.MethodGet, base+"/x", "", "").StatusCode)
	require.Equal(t, http.StatusNotFound, e.Request(t, http.MethodPost, base+"/1/comment/abc/edit", "alice", "{}").StatusCode)
	require.Equal(t, http.StatusBadRequest, e.Request(t, http.MethodPost, base, "bob", `{"title":""}`).StatusCode)
}

func TestLikes(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	seed(t, e, "alice", modelID("alice", "liked"), model.VisibilityPublic, nil)
	require.Equal(t, http.StatusOK, e.Request(t, http.MethodPost, "/api/models/alice/liked/like", "bob", "").StatusCode)
	require.Equal(t, http.StatusOK, e.Request(t, http.MethodPost, "/api/models/alice/liked/like", "admin", "").StatusCode)
	require.Equal(t, http.StatusForbidden, e.Request(t, http.MethodPost, "/api/models/alice/liked/like", "carol", "").StatusCode)
	require.EqualValues(t, 2, decode[map[string]any](t, e.Request(t, http.MethodGet, "/api/models/alice/liked", "", ""))["likes"])
	require.Equal(t, http.StatusOK, e.Request(t, http.MethodDelete, "/api/models/alice/liked/like", "bob", "").StatusCode)
	require.EqualValues(t, 1, decode[map[string]any](t, e.Request(t, http.MethodGet, "/api/models/alice/liked", "", ""))["likes"])
	require.Equal(t, http.StatusUnauthorized, e.Request(t, http.MethodPost, "/api/models/alice/liked/like", "", "").StatusCode)
}

func TestCollections(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	r := e.Request(t, http.MethodPost, "/api/collections", "alice", `{"title":"Small models","namespace":"alice","private":false,"description":"Tiny"}`)
	c := decode[map[string]any](t, r)
	slug := c["slug"].(string)
	require.True(t, strings.HasPrefix(slug, "alice/"))
	require.Regexp(t, hubDate, c["lastUpdated"])
	for _, k := range []string{"title", "owner", "items", "position", "private", "theme", "upvotes"} {
		require.Contains(t, c, k)
	}
	r = e.Request(t, http.MethodPost, "/api/collections", "alice", `{"title":"Small models","namespace":"alice"}`)
	require.Equal(t, http.StatusConflict, r.StatusCode)
	require.Equal(t, slug, decode[map[string]any](t, r)["slug"])
	require.Equal(t, http.StatusUnauthorized, e.Request(t, http.MethodPost, "/api/collections", "", `{"title":"x"}`).StatusCode)
	require.Equal(t, http.StatusOK, e.Request(t, http.MethodPost, "/api/collections", "alice", `{"title":"Secret","private":true}`).StatusCode)

	r = e.Request(t, http.MethodPost, "/api/collections/"+slug+"/items", "alice", `{"item":{"id":"alice/tiny","type":"model"},"note":"Fast"}`)
	c = decode[map[string]any](t, r)
	item := c["items"].([]any)[0].(map[string]any)
	require.Equal(t, "alice/tiny", item["id"])
	require.Equal(t, "Fast", item["note"].(map[string]any)["text"])
	itemID := item["_id"].(string)
	require.Equal(t, http.StatusConflict, e.Request(t, http.MethodPost, "/api/collections/"+slug+"/items", "alice", `{"item":{"id":"alice/tiny","type":"model"}}`).StatusCode)
	require.Equal(t, http.StatusBadRequest, e.Request(t, http.MethodPost, "/api/collections/"+slug+"/items", "alice", `{"item":{"id":"x/y","type":"space"}}`).StatusCode)
	require.Equal(t, http.StatusOK, e.Request(t, http.MethodPost, "/api/collections/"+slug+"/items", "alice", `{"item":{"id":"alice/other-list","type":"collection"}}`).StatusCode)

	require.Equal(t, http.StatusOK, e.Request(t, http.MethodPatch, "/api/collections/"+slug+"/items/"+itemID, "alice", `{"note":"Faster","position":1}`).StatusCode)
	require.Equal(t, http.StatusNotFound, e.Request(t, http.MethodPatch, "/api/collections/"+slug+"/items/not-a-uuid", "alice", `{}`).StatusCode)
	require.Equal(t, http.StatusForbidden, e.Request(t, http.MethodPatch, "/api/collections/"+slug, "bob", `{"title":"Mine"}`).StatusCode)
	r = e.Request(t, http.MethodPatch, "/api/collections/"+slug, "alice", `{"title":"Small","private":true,"theme":"green","position":2}`)
	data := decode[map[string]any](t, r)["data"].(map[string]any)
	require.Equal(t, "Small", data["title"])
	require.Equal(t, true, data["private"])
	require.Equal(t, http.StatusNotFound, e.Request(t, http.MethodGet, "/api/collections/"+slug, "bob", "").StatusCode)
	got := decode[map[string]any](t, e.Request(t, http.MethodGet, "/api/collections/"+slug, "alice", ""))
	require.Len(t, got["items"], 2)

	list := decode[[]map[string]any](t, e.Request(t, http.MethodGet, "/api/collections?owner=alice&limit=10", "alice", ""))
	require.Len(t, list, 2)
	require.NotContains(t, list[0], "position", "huggingface_hub sets position from the list order")
	require.Equal(t, http.StatusOK, e.Request(t, http.MethodDelete, "/api/collections/"+slug+"/items/"+itemID, "alice", "").StatusCode)
	require.Equal(t, http.StatusNotFound, e.Request(t, http.MethodDelete, "/api/collections/"+slug+"/items/x", "alice", "").StatusCode)
	require.Equal(t, http.StatusOK, e.Request(t, http.MethodDelete, "/api/collections/"+slug, "alice", "").StatusCode)
	require.Equal(t, http.StatusNotFound, e.Request(t, http.MethodDelete, "/api/collections/"+slug, "alice", "").StatusCode)
	require.Equal(t, http.StatusNotFound, e.Request(t, http.MethodGet, "/api/collections/a/b/c", "alice", "").StatusCode)
}

func TestXetWriteToken(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	rp, _ := seed(t, e, "alice", modelID("alice", "x"), model.VisibilityPublic, nil)
	r := e.Request(t, http.MethodGet, "/api/models/alice/x/xet-write-token/main", "alice", "")
	require.Equal(t, http.StatusOK, r.StatusCode)
	tok := decode[map[string]any](t, r)
	require.Equal(t, e.URL+"/xet-cas", tok["casUrl"])
	require.Equal(t, tok["casUrl"], r.Header.Get("X-Xet-Cas-Url"))
	require.Equal(t, tok["accessToken"], r.Header.Get("X-Xet-Access-Token"))
	exp, err := strconv.ParseInt(r.Header.Get("X-Xet-Token-Expiration"), 10, 64)
	require.NoError(t, err)
	require.EqualValues(t, tok["exp"], exp)
	require.InDelta(t, time.Now().Add(time.Hour).Unix(), exp, 60)
	claims, err := e.Xet.VerifyToken(context.Background(), tok["accessToken"].(string))
	require.NoError(t, err)
	require.Equal(t, rp.UUID, claims.RepoUUID)
	require.Equal(t, "main", claims.Revision)

	// `None` is the default branch; create_pr needs only the right to open a pull request.
	r = e.Request(t, http.MethodGet, "/api/models/alice/x/xet-write-token/None?create_pr=1", "bob", "")
	require.Equal(t, http.StatusOK, r.StatusCode)
	claims, err = e.Xet.VerifyToken(context.Background(), decode[map[string]any](t, r)["accessToken"].(string))
	require.NoError(t, err)
	require.True(t, claims.CreatePR)
	require.Equal(t, "main", claims.Revision)
	require.Equal(t, http.StatusForbidden, e.Request(t, http.MethodGet, "/api/models/alice/x/xet-write-token/main", "bob", "").StatusCode)
	require.Equal(t, http.StatusUnauthorized, e.Request(t, http.MethodGet, "/api/models/alice/x/xet-write-token/main", "", "").StatusCode)
	// Downloads never use Xet.
	require.Equal(t, http.StatusNotFound, e.Request(t, http.MethodGet, "/api/models/alice/x/xet-read-token/main", "alice", "").StatusCode)
	e.Xet.Reject = true
	require.Equal(t, http.StatusBadRequest, e.Request(t, http.MethodGet, "/api/models/alice/x/xet-write-token/main", "alice", "").StatusCode)
}

func TestParquetAndCroissant(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	id := datasetID("alice", "rows")
	seed(t, e, "alice", id, model.VisibilityPublic, nil)
	e.Viewer.SetTable(id, "default", "train", &datasetstest.Table{Ready: true})
	e.Viewer.SetParquetFiles(id,
		datasets.ParquetFile{Dataset: "alice/rows", Config: "default", Split: "train", Filename: "0001.parquet", URL: e.URL + "/datasets/alice/rows/resolve/refs%2Fconvert%2Fparquet/default/train/0001.parquet"},
		datasets.ParquetFile{Dataset: "alice/rows", Config: "default", Split: "train", Filename: "0000.parquet", URL: e.URL + "/datasets/alice/rows/resolve/refs%2Fconvert%2Fparquet/default/train/0000.parquet"},
		datasets.ParquetFile{Dataset: "alice/rows", Config: "default", Split: "test", Filename: "0000.parquet", URL: e.URL + "/t"},
	)
	base := "/api/datasets/alice/rows/parquet"
	all := decode[map[string]map[string][]string](t, e.Request(t, http.MethodGet, base, "", ""))
	require.Equal(t, []string{e.URL + base + "/default/train/0.parquet", e.URL + base + "/default/train/1.parquet"}, all["default"]["train"])
	cfg := decode[map[string][]string](t, e.Request(t, http.MethodGet, base+"/default", "", ""))
	require.Len(t, cfg["test"], 1)
	require.Len(t, decode[[]string](t, e.Request(t, http.MethodGet, base+"/default/train", "", "")), 2)
	r := e.Request(t, http.MethodGet, base+"/default/train/0.parquet", "", "")
	require.Equal(t, http.StatusFound, r.StatusCode)
	require.True(t, strings.HasSuffix(r.Header.Get("Location"), "/0000.parquet"))
	for _, p := range []string{"/nope", "/default/nope", "/default/train/9.parquet", "/default/train/x.json"} {
		require.Equal(t, http.StatusNotFound, e.Request(t, http.MethodGet, base+p, "", "").StatusCode, p)
	}
	require.Equal(t, http.StatusNotFound, e.Request(t, http.MethodGet, "/api/models/alice/rows/parquet", "", "").StatusCode)

	doc := decode[map[string]any](t, e.Request(t, http.MethodGet, "/api/datasets/alice/rows/croissant", "", ""))
	require.Equal(t, "sc:Dataset", doc["@type"])
	seed(t, e, "alice", datasetID("alice", "empty"), model.VisibilityPublic, nil)
	require.Equal(t, http.StatusNotFound, e.Request(t, http.MethodGet, "/api/datasets/alice/empty/croissant", "", "").StatusCode)
	require.Equal(t, http.StatusNotFound, e.Request(t, http.MethodGet, "/api/datasets/alice/empty/parquet", "", "").StatusCode)
}

func TestRouting(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	seed(t, e, "alice", modelID("alice", "r"), model.VisibilityPublic, nil)
	for _, p := range []string{"/api", "/api/unknown", "/api/models/alice/r/unknown", "/api/models/alice", "/alice/r/blob/main/x", "/api/repos/create/x"} {
		r := e.Request(t, http.MethodGet, p, "", "")
		require.Equal(t, http.StatusNotFound, r.StatusCode, p)
	}
	r := e.Request(t, http.MethodDelete, "/api/models/alice/r", "alice", "")
	require.Equal(t, http.StatusMethodNotAllowed, r.StatusCode)
	require.Equal(t, "GET", r.Header.Get("Allow"))
	require.Equal(t, http.StatusOK, e.Request(t, http.MethodHead, "/api/models/alice/r", "", "").StatusCode)
	require.NotEmpty(t, e.Request(t, http.MethodGet, "/api/models/alice/r", "", "").Header.Get("X-Request-Id"))
	require.Equal(t, http.StatusNotFound, e.Request(t, http.MethodGet, "/api/models/alice/r/revision/a/b", "", "").StatusCode)
}

func TestNewAndSettings(t *testing.T) {
	t.Parallel()
	_, err := hfapi.New(hfapi.Deps{})
	require.ErrorContains(t, err, "Repos")
	require.ErrorContains(t, err, "Settings.PublicURL")

	cfg := config.Default()
	cfg.Server.PublicURL = "https://hangar.example/"
	cfg.Storage.Backend = config.BackendS3
	cfg.HFAPI.Xet = config.XetReject
	cfg.HFAPI.DefaultNamespace = "library"
	s := hfapi.FromConfig(&cfg)
	require.Equal(t, "https://hangar.example", s.PublicURL)
	require.True(t, s.RedirectDownloads)
	require.True(t, s.XetReject)
	require.Equal(t, "library", s.DefaultNamespace)
	require.EqualValues(t, 10<<20, s.RegularUploadMax)
	require.Equal(t, time.Hour, s.PresignTTL)
	cfg.Storage.S3.DownloadMode = config.DownloadProxy
	require.False(t, hfapi.FromConfig(&cfg).RedirectDownloads)
}
