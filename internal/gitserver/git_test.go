package gitserver_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/repo"
)

func TestCloneProtocolsAndShallow(t *testing.T) {
	f := newFixture(t)
	want := f.resolve(t, publicID, "main")
	for _, proto := range []string{"0", "1", "2"} {
		t.Run("v"+proto, func(t *testing.T) {
			dir := f.clone(t, publicID, "", "-c", "protocol.version="+proto)
			require.Equal(t, want, f.git.Must(t, dir, "rev-parse", "HEAD"))
			require.FileExists(t, dir+"/README.md")
			shallow := f.clone(t, publicID, "", "-c", "protocol.version="+proto, "--depth", "1")
			require.Equal(t, "true", f.git.Must(t, shallow, "rev-parse", "--is-shallow-repository"))
		})
	}
	// Without .git, and for datasets.
	dir := t.TempDir() + "/plain"
	f.git.Must(t, "", "clone", "-q", f.inst.RemoteURL(publicID, "", false), dir)
	ds := f.clone(t, datasetID, "")
	require.Equal(t, f.resolve(t, datasetID, "main"), f.git.Must(t, ds, "rev-parse", "HEAD"))
}

func TestPartialCloneFilter(t *testing.T) {
	f := newFixture(t)
	dir := f.clone(t, publicID, "", "--filter=blob:none")
	require.Equal(t, "true", f.git.Must(t, dir, "config", "remote.origin.promisor"))
	require.Contains(t, f.git.Must(t, dir, "cat-file", "-p", "HEAD:README.md"), "# tiny")
}

func TestPushFetchAndPull(t *testing.T) {
	f := newFixture(t)
	a := f.clone(t, publicID, tokAlice)
	b := f.clone(t, publicID, tokAlice)
	head := f.commit(t, a, "config.json", `{"a":1}`, "add config")
	f.git.Must(t, a, "push", "-q", "origin", "main")
	require.Equal(t, head, f.resolve(t, publicID, "main"))

	f.git.Must(t, b, "pull", "-q", "--ff-only")
	require.Equal(t, head, f.git.Must(t, b, "rev-parse", "HEAD"))

	// The Repo service sees the file pushed with git.
	rc, _, err := f.repos.OpenFile(context.Background(), publicID, model.CommitSHA(head), "config.json", nil)
	require.NoError(t, err)
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	require.Equal(t, `{"a":1}`, string(got))

	// A Commit made through the Repo service is what git fetches.
	res, err := f.repos.Commit(context.Background(), repo.CommitParams{Repo: publicID, Revision: "main", Summary: "api commit",
		Operations: []repo.Operation{{Kind: repo.OpAdd, Path: "api.txt", Content: strings.NewReader("from the API\n")}}, Actor: alice})
	require.NoError(t, err)
	f.git.Must(t, a, "fetch", "-q", "origin")
	require.Equal(t, string(res.SHA), f.git.Must(t, a, "rev-parse", "origin/main"))
}

func TestPushBranchesAndTags(t *testing.T) {
	f := newFixture(t)
	dir := f.clone(t, publicID, tokAlice)
	f.git.Must(t, dir, "checkout", "-q", "-b", "feature")
	sha := f.commit(t, dir, "f.txt", "feature\n", "feature")
	f.git.Must(t, dir, "push", "-q", "origin", "feature")
	require.Equal(t, sha, f.resolve(t, publicID, "feature"))

	// A branch at an existing Commit sends an empty pack.
	f.git.Must(t, dir, "push", "-q", "origin", "feature:refs/heads/copy")
	require.Equal(t, sha, f.resolve(t, publicID, "copy"))

	f.git.Must(t, dir, "tag", "v1")
	f.git.Must(t, dir, "tag", "-a", "v2", "-m", "release")
	f.git.Must(t, dir, "push", "-q", "origin", "v1", "v2")
	require.Equal(t, sha, f.resolve(t, publicID, "v1"))
	require.Equal(t, sha, f.resolve(t, publicID, "v2"))

	f.git.Must(t, dir, "push", "-q", "origin", "--delete", "copy", "v2")
	_, err := f.repos.Resolve(context.Background(), publicID, "copy")
	require.ErrorIs(t, err, repo.ErrRevisionNotFound)
	_, err = f.repos.Resolve(context.Background(), publicID, "v2")
	require.ErrorIs(t, err, repo.ErrRevisionNotFound)

	// The ls-remote advertisement shows the new refs.
	out := f.git.Must(t, "", "ls-remote", f.remote(publicID, ""))
	require.Contains(t, out, "refs/heads/feature")
	require.Contains(t, out, "refs/tags/v1")
}

func TestPushPolicy(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	dir := f.clone(t, publicID, tokCarol)
	main := f.resolve(t, publicID, "main")

	out, err := f.git.Run(ctx, dir, "push", "origin", "--delete", "main")
	require.Error(t, err)
	require.Contains(t, out, "default branch and cannot be deleted")

	out, err = f.git.Run(ctx, dir, "push", "origin", "HEAD:refs/pr/1")
	require.Error(t, err)
	require.Contains(t, out, "pull request refs")

	out, err = f.git.Run(ctx, dir, "push", "origin", "HEAD:refs/custom/x")
	require.Error(t, err)
	require.Contains(t, out, "only branches")

	// A write collaborator cannot force-push the default branch.
	f.git.Must(t, dir, "commit", "-q", "--amend", "-m", "rewritten")
	out, err = f.git.Run(ctx, dir, "push", "--force", "origin", "main")
	require.Error(t, err)
	require.Contains(t, out, "force-pushing the default branch is not allowed")
	require.Equal(t, main, f.resolve(t, publicID, "main"))

	// But can force-push other branches.
	f.git.Must(t, dir, "push", "-q", "origin", "HEAD:refs/heads/topic")
	f.git.Must(t, dir, "commit", "-q", "--amend", "-m", "again")
	f.git.Must(t, dir, "push", "-q", "--force", "origin", "HEAD:refs/heads/topic")

	// The Namespace admin can force-push the default branch.
	adminDir := f.clone(t, publicID, tokAlice)
	f.git.Must(t, adminDir, "commit", "-q", "--amend", "-m", "admin rewrite")
	f.git.Must(t, adminDir, "push", "-q", "--force", "origin", "main")
	require.Equal(t, f.git.Must(t, adminDir, "rev-parse", "HEAD"), f.resolve(t, publicID, "main"))
}

func TestForcePushAllowedByConfig(t *testing.T) {
	f := newFixture(t, withConfig(func(c *gitserverConfig) { c.AllowForcePush = true }))
	dir := f.clone(t, publicID, tokCarol)
	f.git.Must(t, dir, "commit", "-q", "--amend", "-m", "rewritten")
	f.git.Must(t, dir, "push", "-q", "--force", "origin", "main")
	require.Equal(t, f.git.Must(t, dir, "rev-parse", "HEAD"), f.resolve(t, publicID, "main"))
}

func TestPushToMirrorRefused(t *testing.T) {
	f := newFixture(t)
	dir := f.clone(t, mirrorID, tokAlice)
	f.commit(t, dir, "x.txt", "x", "x")
	out, err := f.git.Run(context.Background(), dir, "push", "origin", "main")
	require.Error(t, err)
	require.Contains(t, out, "is a Mirror and is read-only")
}

func TestPushRefusesLargeNonLFSBlob(t *testing.T) {
	f := newFixture(t)
	dir := f.clone(t, publicID, tokAlice)
	big := make([]byte, 2<<20)
	_, _ = rand.Read(big)
	writeFile(t, dir, "weights/model.dat", big)
	f.git.Must(t, dir, "add", "-A")
	f.git.Must(t, dir, "commit", "-q", "-m", "big")
	out, err := f.git.Run(context.Background(), dir, "push", "origin", "main")
	require.Error(t, err)
	require.Contains(t, out, "weights/model.dat is 2 MiB, over the 1 MiB limit")
	require.Contains(t, out, "git lfs track")
}

func TestPushRefusesPackOverLimit(t *testing.T) {
	f := newFixture(t, withConfig(func(c *gitserverConfig) { c.MaxPackSize = 64 << 10; c.MaxBlobSize = 0 }))
	dir := f.clone(t, publicID, tokAlice)
	big := make([]byte, 256<<10)
	_, _ = rand.Read(big)
	writeFile(t, dir, "blob.bin", big)
	f.git.Must(t, dir, "add", "-A")
	f.git.Must(t, dir, "commit", "-q", "-m", "big")
	before := f.resolve(t, publicID, "main")
	// git sends small packs with a Content-Length, refused before reading.
	out, err := f.git.Run(context.Background(), dir, "push", "origin", "main")
	require.Error(t, err)
	require.Contains(t, out, "413")
	// Larger packs are chunked; the limit applies while reading.
	out, err = f.git.Run(context.Background(), dir, "-c", "http.postBuffer=4096", "push", "origin", "main")
	require.Error(t, err)
	require.Contains(t, out, "git.max_pack_size")
	require.Equal(t, before, f.resolve(t, publicID, "main"))
}

func TestPushAuth(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	dir := f.clone(t, publicID, "")
	f.commit(t, dir, "x.txt", "x", "x")

	// Anonymous pushes get a Basic challenge; with prompts off git fails.
	out, err := f.git.Run(ctx, dir, "push", f.remote(publicID, ""), "main")
	require.Error(t, err)
	require.Contains(t, strings.ToLower(out), "could not read username")

	// A read-only token is refused with 403.
	out, err = f.git.Run(ctx, dir, "push", f.remote(publicID, tokAliceRead), "main")
	require.Error(t, err)
	require.Contains(t, out, "403")

	// A user without access to a public Repo gets 403, and 404 for a private one.
	out, err = f.git.Run(ctx, dir, "push", f.remote(publicID, tokBob), "main")
	require.Error(t, err)
	require.Contains(t, out, "403")
	_, err = f.git.Run(ctx, "", "ls-remote", f.remote(privateID, tokBob))
	require.Error(t, err)
	resp := f.get(t, "/alice/secret.git/info/refs?service=git-upload-pack", tokBob)
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
	resp = f.get(t, "/alice/secret.git/info/refs?service=git-upload-pack", "")
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	require.Equal(t, `Basic realm="Hangar"`, resp.Header.Get("WWW-Authenticate"))
	resp = f.get(t, "/alice/nothing.git/info/refs?service=git-upload-pack", "")
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode, "missing Repos look like private ones")
	resp = f.get(t, "/alice/nothing.git/info/refs?service=git-upload-pack", tokBob)
	require.Equal(t, http.StatusNotFound, resp.StatusCode)

	// A bad token gets a challenge too.
	resp = f.get(t, "/alice/tiny.git/info/refs?service=git-upload-pack", "wrong")
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	require.Equal(t, `Basic realm="Hangar"`, resp.Header.Get("WWW-Authenticate"))

	// The owner can read the private Repo.
	f.clone(t, privateID, tokAlice)
}

func (f *fixture) get(t *testing.T, path, token string) *http.Response {
	t.Helper()
	return f.do(t, http.MethodGet, path, token, nil, nil)
}

func (f *fixture) do(t *testing.T, method, path, token string, body io.Reader, header http.Header) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, f.inst.URL+path, body)
	require.NoError(t, err)
	for k, v := range header {
		req.Header[k] = v
	}
	if token != "" {
		req.SetBasicAuth("user", token)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func readAll(t *testing.T, r io.Reader) string {
	t.Helper()
	b, err := io.ReadAll(r)
	require.NoError(t, err)
	return string(b)
}

func TestSmartHTTPEdgeCases(t *testing.T) {
	f := newFixture(t)
	// Dumb HTTP is not served.
	resp := f.get(t, "/alice/tiny.git/info/refs", "")
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	resp = f.get(t, "/alice/tiny.git/HEAD", "")
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
	resp = f.do(t, http.MethodPost, "/alice/tiny.git/info/refs?service=git-upload-pack", "", nil, nil)
	require.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)
	resp = f.get(t, "/alice/tiny.git/git-upload-pack", "")
	require.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)
	resp = f.do(t, http.MethodPost, "/alice/tiny.git/git-upload-pack", "", strings.NewReader("0000"), http.Header{"Content-Type": {"text/plain"}})
	require.Equal(t, http.StatusUnsupportedMediaType, resp.StatusCode)
	resp = f.do(t, http.MethodPost, "/alice/tiny.git/git-receive-pack", tokAlice, strings.NewReader("0000"), http.Header{"Content-Type": {"text/plain"}})
	require.Equal(t, http.StatusUnsupportedMediaType, resp.StatusCode)
	resp = f.do(t, http.MethodPost, "/alice/tiny.git/git-upload-pack", "", strings.NewReader("0000"),
		http.Header{"Content-Type": {"application/x-git-upload-pack-request"}, "Content-Encoding": {"br"}})
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	resp = f.get(t, "/alice/bad%2Fname.git/info/refs?service=git-upload-pack", "")
	require.Equal(t, http.StatusNotFound, resp.StatusCode)

	// The v0 advertisement starts with the smart reply; v2 does not.
	resp = f.get(t, "/alice/tiny.git/info/refs?service=git-upload-pack", "")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "application/x-git-upload-pack-advertisement", resp.Header.Get("Content-Type"))
	require.True(t, strings.HasPrefix(readAll(t, resp.Body), "001e# service=git-upload-pack\n0000"))
	resp = f.do(t, http.MethodGet, "/alice/tiny.git/info/refs?service=git-upload-pack", "", nil, http.Header{"Git-Protocol": {"version=2"}})
	require.True(t, strings.HasPrefix(readAll(t, resp.Body), "000eversion 2\n"))
	resp = f.do(t, http.MethodHead, "/alice/tiny.git/info/refs?service=git-upload-pack", "", nil, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	// The receive-pack advertisement hides refs Hangar owns.
	_, err := f.repos.Commit(context.Background(), repo.CommitParams{Repo: publicID, Revision: "refs/pr/1", CreateFrom: model.CommitSHA(f.resolve(t, publicID, "main")),
		Summary: "pr", Operations: []repo.Operation{{Kind: repo.OpAdd, Path: "pr.txt", Content: strings.NewReader("pr")}}, Actor: alice})
	require.NoError(t, err)
	resp = f.get(t, "/alice/tiny.git/info/refs?service=git-receive-pack", tokAlice)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	adv := readAll(t, resp.Body)
	require.Contains(t, adv, "refs/heads/main")
	require.NotContains(t, adv, "refs/pr/1")
	require.Contains(t, f.git.Must(t, "", "ls-remote", f.remote(publicID, "")), "refs/pr/1", "upload-pack shows pull request refs, as the Hub does")

	// gzip request bodies work: an ls-refs request over v2.
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write([]byte("0014command=ls-refs\n00010009peel\n0000"))
	_ = zw.Close()
	resp = f.do(t, http.MethodPost, "/alice/tiny.git/git-upload-pack", "", &gz, http.Header{
		"Content-Type": {"application/x-git-upload-pack-request"}, "Content-Encoding": {"gzip"}, "Git-Protocol": {"version=2"}})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, readAll(t, resp.Body), "refs/heads/main")

	// A malformed push request is answered, not hung.
	resp = f.do(t, http.MethodPost, "/alice/tiny.git/git-receive-pack", tokAlice, strings.NewReader("zzzz"),
		http.Header{"Content-Type": {"application/x-git-receive-pack-request"}})
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	// A declared body over the limit is refused before reading.
	resp = f.do(t, http.MethodPost, "/alice/tiny.git/git-receive-pack", tokAlice, bytes.NewReader(make([]byte, 65<<20)),
		http.Header{"Content-Type": {"application/x-git-receive-pack-request"}})
	require.Equal(t, http.StatusRequestEntityTooLarge, resp.StatusCode)
}
