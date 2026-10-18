package hfapitest_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/hfapi"
	"github.com/hangar-hub/hangar/internal/hfapi/hfapitest"
	"github.com/hangar-hub/hangar/internal/model"
	"github.com/hangar-hub/hangar/internal/repo"
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

func TestLFSStandInMultipart(t *testing.T) {
	t.Parallel()
	e := hfapitest.New(t, hfapitest.Options{LFSMultipartThreshold: 10, LFSPartSize: 4})
	ctx := context.Background()
	content := []byte("0123456789ABC") // 13 bytes: parts of 4, 4, 4 and 1
	sum := sha256.Sum256(content)
	oid := hex.EncodeToString(sum[:])

	// start asks for upload actions and returns the upload action.
	start := func(oid string, size int, transfers string) map[string]any {
		t.Helper()
		r := e.Request(t, http.MethodPost, "/alice/m.git/info/lfs/objects/batch", "alice",
			`{"operation":"upload","transfers":`+transfers+`,"objects":[{"oid":"`+oid+`","size":`+strconv.Itoa(size)+`}]}`)
		require.Equal(t, http.StatusOK, r.StatusCode)
		obj := decodeMap(t, r)["objects"].([]any)[0].(map[string]any)
		return obj["actions"].(map[string]any)["upload"].(map[string]any)
	}
	send := func(method, url string, body []byte) *http.Response {
		t.Helper()
		req, err := http.NewRequest(method, url, bytes.NewReader(body))
		require.NoError(t, err)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		return resp
	}
	// partURLs returns the numbered part URLs in order, as the client sorts them.
	partURLs := func(upload map[string]any) []string {
		header := upload["header"].(map[string]any)
		var keys []string
		for k := range header {
			if k != "chunk_size" {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		urls := make([]string, len(keys))
		for i, k := range keys {
			require.Equal(t, fmt.Sprintf("%05d", i+1), k)
			urls[i] = header[k].(string)
		}
		return urls
	}
	type part struct {
		PartNumber int    `json:"partNumber"`
		ETag       string `json:"etag"`
	}
	upload := func(upload map[string]any, data []byte) []part {
		t.Helper()
		var parts []part
		for i, url := range partURLs(upload) {
			end := min((i+1)*4, len(data))
			resp := send(http.MethodPut, url, data[i*4:end])
			require.Equal(t, http.StatusOK, resp.StatusCode)
			require.NotEmpty(t, resp.Header.Get("ETag"), "every part answer carries an ETag")
			parts = append(parts, part{PartNumber: i + 1, ETag: resp.Header.Get("ETag")})
		}
		return parts
	}
	completion := func(oid string, parts []part) []byte {
		b, err := json.Marshal(map[string]any{"oid": oid, "parts": parts})
		require.NoError(t, err)
		return b
	}

	// Without the multipart transfer, or at the threshold, the upload is a single PUT.
	require.NotContains(t, start(oid, len(content), `["basic"]`), "header")
	small := sha256.Sum256(content[:10])
	require.NotContains(t, start(hex.EncodeToString(small[:]), 10, `["basic","multipart"]`), "header")

	up := start(oid, len(content), `["basic","multipart"]`)
	require.Equal(t, "4", up["header"].(map[string]any)["chunk_size"])
	require.Len(t, partURLs(up), 4, "ceil(size / chunk_size) parts")
	href := up["href"].(string)
	require.True(t, strings.HasSuffix(href, "/complete"))
	parts := upload(up, content)

	// A part of the wrong size, an unknown part number, and a single PUT to a
	// multipart upload are refused; a part may be sent again.
	urls := partURLs(up)
	require.Equal(t, http.StatusBadRequest, send(http.MethodPut, urls[0], []byte("012")).StatusCode)
	require.Equal(t, http.StatusNotFound, send(http.MethodPut, strings.TrimSuffix(urls[0], "/part/1")+"/part/5", []byte("x")).StatusCode)
	require.Equal(t, http.StatusNotFound, send(http.MethodPut, strings.TrimSuffix(href, "/complete"), content).StatusCode)
	resp := send(http.MethodPut, urls[3], content[12:])
	require.Equal(t, parts[3].ETag, resp.Header.Get("ETag"))
	_, err := e.LFS.Stat(ctx, oid)
	require.Error(t, err, "nothing is stored before the upload completes")

	resp = send(http.MethodPost, href, completion(oid, parts))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	obj, err := e.LFS.Stat(ctx, oid)
	require.NoError(t, err)
	require.Equal(t, int64(len(content)), obj.Size)
	require.Equal(t, http.StatusNotFound, send(http.MethodPost, href, completion(oid, parts)).StatusCode, "completion URLs are single use")
	require.NotContains(t, decodeMap(t, e.Request(t, http.MethodPost, "/alice/m.git/info/lfs/objects/batch", "alice",
		`{"operation":"upload","transfers":["multipart"],"objects":[{"oid":"`+oid+`","size":13}]}`))["objects"].([]any)[0], "actions")

	// Completions that do not match the parts are refused and store nothing.
	other := []byte("abcdefghijklm")
	otherSum := sha256.Sum256(other)
	otherOID := hex.EncodeToString(otherSum[:])
	bad := func(oid string, mutate func([]part) []part) int {
		t.Helper()
		up := start(otherOID, len(other), `["multipart"]`)
		parts := upload(up, other)
		return send(http.MethodPost, up["href"].(string), completion(oid, mutate(parts))).StatusCode
	}
	same := func(p []part) []part { return p }
	require.Equal(t, http.StatusBadRequest, bad(oid, same), "a different oid")
	require.Equal(t, http.StatusBadRequest, bad(otherOID, func(p []part) []part { return p[:3] }), "a missing part")
	require.Equal(t, http.StatusBadRequest, bad(otherOID, func(p []part) []part { p[0], p[1] = p[1], p[0]; return p }), "parts out of order")
	require.Equal(t, http.StatusBadRequest, bad(otherOID, func(p []part) []part { p[2].ETag = `"x"`; return p }), "a wrong ETag")
	up = start(otherOID, len(other), `["multipart"]`)
	require.Equal(t, http.StatusBadRequest, send(http.MethodPost, up["href"].(string), []byte("{")).StatusCode)
	_, err = e.LFS.Stat(ctx, otherOID)
	require.Error(t, err)

	// Parts whose bytes do not hash to the oid are refused at completion.
	wrong := strings.Repeat("c", 64)
	up = start(wrong, len(other), `["multipart"]`)
	require.Equal(t, http.StatusBadRequest, send(http.MethodPost, up["href"].(string), completion(wrong, upload(up, other))).StatusCode)
	_, err = e.LFS.Stat(ctx, wrong)
	require.Error(t, err)
}

func decodeMap(t *testing.T, r *http.Response) map[string]any {
	t.Helper()
	var m map[string]any
	require.NoError(t, json.NewDecoder(r.Body).Decode(&m))
	return m
}
