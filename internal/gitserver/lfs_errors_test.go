package gitserver_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/gitserver/gitservertest"
	"github.com/hangar-hub/hangar/internal/model"
	"github.com/hangar-hub/hangar/internal/repo"
)

type quota struct{ limit int64 }

func (q quota) CheckQuota(_ context.Context, _ uuid.UUID, add int64) error {
	if add > q.limit {
		return errors.New("the alice Namespace is over its storage quota")
	}
	return nil
}

func TestLFSQuota(t *testing.T) {
	t.Parallel()
	f := newFixture(t, func(o *gitservertest.Options, _ *fixture) { o.Quota = quota{limit: 1000} })
	resp, b := f.batch(t, publicID, tokAlice, batchReq("upload", nil, obj(strings.Repeat("a", 64), 2000)))
	require.Equal(t, http.StatusInsufficientStorage, resp.StatusCode)
	require.Contains(t, b.Message, "over its storage quota")
	resp, _ = f.batch(t, publicID, tokAlice, batchReq("upload", nil, obj(strings.Repeat("a", 64), 10)))
	require.Equal(t, http.StatusOK, resp.StatusCode)

	// Pushes count their pack against the quota too.
	dir := f.clone(t, publicID, tokAlice)
	content, _ := randomContent(t, 4000)
	writeFile(t, dir, "x.bin", content)
	f.git.Must(t, dir, "add", "-A")
	f.git.Must(t, dir, "commit", "-q", "-m", "x")
	out, err := f.git.Run(context.Background(), dir, "push", "origin", "main")
	require.Error(t, err)
	require.Contains(t, out, "over its storage quota")
}

func TestLFSRequestErrors(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	post := func(path, token, ct, body string) *http.Response {
		return f.do(t, http.MethodPost, path, token, strings.NewReader(body), http.Header{"Content-Type": {ct}})
	}
	const batchPath = "/alice/tiny.git/info/lfs/objects/batch"
	const lfsCT = "application/vnd.git-lfs+json"
	require.Equal(t, http.StatusUnsupportedMediaType, post(batchPath, "", "text/plain", "{}").StatusCode)
	require.Equal(t, http.StatusUnprocessableEntity, post(batchPath, "", lfsCT, "{").StatusCode)
	require.Equal(t, http.StatusUnprocessableEntity, post(batchPath, "", lfsCT, `{"operation":"delete","objects":[]}`).StatusCode)
	require.Equal(t, http.StatusConflict, post(batchPath, "", lfsCT, `{"operation":"download","objects":[],"hash_algo":"sha512"}`).StatusCode)
	many, _ := json.Marshal(map[string]any{"operation": "download", "objects": make([]map[string]any, 1001)})
	require.Equal(t, http.StatusRequestEntityTooLarge, post(batchPath, "", lfsCT, string(many)).StatusCode)
	require.Equal(t, http.StatusRequestEntityTooLarge, post(batchPath, "", lfsCT, `{"operation":"download","x":"`+strings.Repeat("a", 5<<20)+`"}`).StatusCode)
	resp := f.get(t, batchPath, "")
	require.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)
	require.Equal(t, "POST", resp.Header.Get("Allow"))

	// Invalid objects are reported per object; charset parameters are fine.
	resp = post(batchPath, "", lfsCT+"; charset=utf-8", `{"operation":"download","objects":[{"oid":"XYZ","size":1},{"oid":"`+strings.Repeat("a", 64)+`","size":-1}]}`)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var b lfsBatch
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&b))
	require.Equal(t, http.StatusUnprocessableEntity, b.Objects[0].Error.Code)
	require.Equal(t, http.StatusUnprocessableEntity, b.Objects[1].Error.Code)
	_, ub := f.batch(t, publicID, tokAlice, batchReq("upload", nil, obj("nothex", 1)))
	require.Equal(t, http.StatusUnprocessableEntity, ub.Objects[0].Error.Code)

	// Access control.
	up := batchReq("upload", nil, obj(strings.Repeat("a", 64), 1))
	resp, b2 := f.batch(t, publicID, "", up)
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	require.Equal(t, `Basic realm="Hangar"`, resp.Header.Get("LFS-Authenticate"))
	require.NotEmpty(t, b2.Message)
	resp, _ = f.batch(t, publicID, tokAliceRead, up)
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	resp, _ = f.batch(t, publicID, tokBob, up)
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	resp, _ = f.batch(t, privateID, tokBob, batchReq("download", nil))
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
	resp, _ = f.batch(t, privateID, "", batchReq("download", nil))
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	resp, b2 = f.batch(t, mirrorID, tokAlice, up)
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	require.Contains(t, b2.Message, "Mirror")

	// Gated Repos need an approved access request to download.
	gated := true
	_, err := f.repos.UpdateSettings(context.Background(), publicID, repo.SettingsUpdate{Gating: &repo.Gating{Mode: "manual"}}, alice)
	require.NoError(t, err)
	require.True(t, gated)
	resp, b2 = f.batch(t, publicID, tokBob, batchReq("download", nil))
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	require.Contains(t, b2.Message, "restricted")
	resp, _ = f.batch(t, publicID, "", batchReq("download", nil))
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	resp = f.get(t, "/alice/tiny.git/info/refs?service=git-upload-pack", tokBob)
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
}

func TestLFSLocksAndRoutes(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	for _, p := range []string{"/alice/tiny.git/info/lfs/locks", "/alice/tiny.git/info/lfs/locks/verify", "/alice/tiny/info/lfs/locks/1/unlock", "/datasets/alice/data.git/info/lfs/locks"} {
		resp := f.do(t, http.MethodPost, p, tokAlice, strings.NewReader(`{}`), http.Header{"Content-Type": {"application/vnd.git-lfs+json"}})
		require.Equal(t, http.StatusNotFound, resp.StatusCode, p)
		var body map[string]string
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
		require.Equal(t, "Locking is not supported", body["message"])
	}
	oid := strings.Repeat("a", 64)
	for _, tc := range []struct {
		method, path string
		status       int
	}{
		{http.MethodGet, "/alice/tiny.git/info/lfs/other", http.StatusNotFound},
		{http.MethodGet, "/alice/tiny.git/info/x", http.StatusNotFound},
		{http.MethodGet, "/alice/tiny.git/info/lfs/objects/nothex", http.StatusNotFound},
		{http.MethodPut, "/alice/tiny.git/info/lfs/objects/nothex", http.StatusNotFound},
		{http.MethodDelete, "/alice/tiny.git/info/lfs/objects/" + oid, http.StatusMethodNotAllowed},
		{http.MethodGet, "/alice/tiny.git/info/lfs/objects/" + oid, http.StatusForbidden},
		{http.MethodPut, "/alice/tiny.git/info/lfs/objects/" + oid, http.StatusForbidden},
		{http.MethodPut, "/alice/tiny.git/info/lfs/objects/" + oid + "?size=1", http.StatusForbidden},
		{http.MethodGet, "/alice/tiny.git/info/lfs/verify", http.StatusMethodNotAllowed},
		{http.MethodPost, "/alice/tiny.git/info/lfs/verify", http.StatusUnprocessableEntity},
		{http.MethodGet, "/alice/tiny.git/info/lfs/uploads/x", http.StatusMethodNotAllowed},
		{http.MethodPost, "/alice/tiny.git/info/lfs/uploads/x", http.StatusNotFound},
		{http.MethodPost, "/alice/tiny.git/info/lfs/uploads/" + uuid.NewString(), http.StatusForbidden},
		{http.MethodGet, "/alice/tiny.git/info/lfs/uploads/x/1", http.StatusMethodNotAllowed},
		{http.MethodPut, "/alice/tiny.git/info/lfs/uploads/x/1", http.StatusNotFound},
		{http.MethodPut, "/alice/tiny.git/info/lfs/uploads/" + uuid.NewString() + "/0", http.StatusNotFound},
		{http.MethodPut, "/alice/tiny.git/info/lfs/uploads/" + uuid.NewString() + "/1", http.StatusForbidden},
	} {
		resp := f.do(t, tc.method, tc.path, "", strings.NewReader("x"), nil)
		require.Equal(t, tc.status, resp.StatusCode, "%s %s", tc.method, tc.path)
	}
}

func TestLFSSignedURLsForMissingRepoAndUploads(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	content, oid := randomContent(t, (1<<20)+300000)
	_, b := f.batch(t, publicID, tokAlice, batchReq("upload", []string{"multipart"}, obj(oid, len(content))))
	up := b.Objects[0].Actions["upload"]

	// A part without Content-Length is refused.
	req, _ := http.NewRequest(http.MethodPut, up.Header["00001"], &chunked{data: content[:262144]})
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusLengthRequired, resp.StatusCode)

	// Part numbers outside the upload are refused.
	bad := strings.Replace(up.Header["00001"], "/1?", "/9?", 1)
	require.Equal(t, http.StatusForbidden, call(t, http.MethodPut, &lfsAction{Href: bad}, []byte("x")).StatusCode, "the signature covers the path")

	// An empty completion is refused.
	require.Equal(t, http.StatusUnprocessableEntity, call(t, http.MethodPost, &lfsAction{Href: up.Href}, []byte(`{"oid":"`+oid+`","parts":[]}`)).StatusCode)

	// After a rename the signed URLs still reach the Repo; after deletion they do not.
	_, err = f.repos.Rename(context.Background(), publicID, "renamed", alice)
	require.NoError(t, err)
	resp = call(t, http.MethodPut, &lfsAction{Href: up.Header["00001"]}, content[:262144])
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.NoError(t, f.repos.Delete(context.Background(), model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "renamed"}, alice))
	require.Equal(t, http.StatusNotFound, call(t, http.MethodPut, &lfsAction{Href: up.Header["00002"]}, content[262144:2*262144]).StatusCode)

	// The git and LFS endpoints of a renamed Repo follow the rename.
	g := newFixture(t)
	_, err = g.repos.Rename(context.Background(), publicID, "moved", alice)
	require.NoError(t, err)
	g.clone(t, publicID, "")
	resp, _ = g.batch(t, publicID, "", batchReq("download", nil))
	require.Equal(t, http.StatusOK, resp.StatusCode)
}

// chunked is a reader with no known length, so the request is chunked.
type chunked struct{ data []byte }

func (c *chunked) Read(p []byte) (int, error) {
	if len(c.data) == 0 {
		return 0, io.EOF
	}
	n := copy(p, c.data)
	c.data = c.data[n:]
	return n, nil
}
