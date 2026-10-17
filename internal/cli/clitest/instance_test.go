package clitest_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"

	"github.com/jamesagarside/hangar/internal/api/gen"
	"github.com/jamesagarside/hangar/internal/cli/clitest"
	"github.com/jamesagarside/hangar/internal/model"
)

func client(t *testing.T, i *clitest.Instance, token string) *gen.ClientWithResponses {
	t.Helper()
	c, err := gen.NewClientWithResponses(i.URL+"/api/hangar/v1", gen.WithRequestEditorFn(func(_ context.Context, r *http.Request) error {
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		return nil
	}))
	require.NoError(t, err)
	return c
}

func shaHex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func ptr[T any](v T) *T { return &v }

func TestUploadCommitAndDownload(t *testing.T) {
	for _, presign := range []bool{false, true} {
		i := clitest.New(t)
		i.Presign = presign
		i.PartSize = 1 << 10
		ctx := context.Background()
		c := client(t, i, clitest.Token)

		r, err := c.CreateRepoWithResponse(ctx, gen.RepoCreate{Type: gen.RepoTypeSingularModel, Namespace: "acme", Name: "m", Visibility: ptr(gen.Private)})
		require.NoError(t, err)
		require.Equal(t, http.StatusCreated, r.StatusCode())
		r, _ = c.CreateRepoWithResponse(ctx, gen.RepoCreate{Type: gen.RepoTypeSingularModel, Namespace: "acme", Name: "m"})
		require.Equal(t, http.StatusConflict, r.StatusCode())
		r, _ = c.CreateRepoWithResponse(ctx, gen.RepoCreate{Type: gen.RepoTypeSingularModel, Namespace: "a", Name: "m"})
		require.Equal(t, http.StatusUnprocessableEntity, r.StatusCode())

		big := bytes.Repeat([]byte("weights!"), 400) // 3200 bytes, four parts
		small := []byte(`{"a":1}`)
		plan, err := c.PlanUploadsWithResponse(ctx, gen.Models, "acme", "m", gen.UploadPlanRequest{Files: []gen.UploadFile{
			{Path: "model.safetensors", Size: int64(len(big)), Sha256: shaHex(big)},
			{Path: "config.json", Size: int64(len(small)), Sha256: shaHex(small)},
		}})
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, plan.StatusCode(), string(plan.Body))
		up := plan.JSON200.Files[0]
		require.Equal(t, gen.Upload, up.Action)
		require.Equal(t, gen.Inline, plan.JSON200.Files[1].Action)
		require.Len(t, *up.PartUrls, 4)

		i.FailPart(shaHex(big), 2, 1)
		var parts []gen.UploadedPart
		for n, u := range *up.PartUrls {
			chunk := big[n*1024 : min((n+1)*1024, len(big))]
			put := func() *http.Response {
				req, _ := http.NewRequest(http.MethodPut, u, bytes.NewReader(chunk))
				if !presign {
					req.Header.Set("Authorization", "Bearer "+clitest.Token)
				}
				resp, err := http.DefaultClient.Do(req)
				require.NoError(t, err)
				_ = resp.Body.Close()
				return resp
			}
			resp := put()
			if n == 1 {
				require.Equal(t, http.StatusInternalServerError, resp.StatusCode)
				resp = put()
			}
			require.Equal(t, http.StatusOK, resp.StatusCode)
			parts = append(parts, gen.UploadedPart{Number: n + 1, Etag: resp.Header.Get("ETag")})
		}
		require.Equal(t, 4, i.PartPuts())
		st, err := c.GetUploadWithResponse(ctx, gen.Models, "acme", "m", *up.UploadId)
		require.NoError(t, err)
		require.Len(t, st.JSON200.Parts, 4)
		bad, _ := c.CompleteUploadWithResponse(ctx, gen.Models, "acme", "m", *up.UploadId, gen.UploadComplete{Parts: parts[:3]})
		require.Equal(t, http.StatusUnprocessableEntity, bad.StatusCode())
		done, err := c.CompleteUploadWithResponse(ctx, gen.Models, "acme", "m", *up.UploadId, gen.UploadComplete{Parts: parts})
		require.NoError(t, err)
		require.Equal(t, gen.Verified, done.JSON200.State)
		require.True(t, i.HasObject(shaHex(big)))

		cr, err := c.CreateCommitWithResponse(ctx, gen.Models, "acme", "m", gen.CommitRequest{Summary: "add", Operations: []gen.CommitOperation{
			{Op: gen.AddLfs, Path: "model.safetensors", Sha256: ptr(shaHex(big)), Size: ptr(int64(len(big)))},
			{Op: gen.Add, Path: "config.json", ContentBase64: ptr(base64.StdEncoding.EncodeToString(small))},
			{Op: gen.Delete, Path: ".gitattributes"},
		}})
		require.NoError(t, err)
		require.Equal(t, http.StatusCreated, cr.StatusCode(), string(cr.Body))
		again, _ := c.CreateCommitWithResponse(ctx, gen.Models, "acme", "m", gen.CommitRequest{Summary: "same", Operations: []gen.CommitOperation{
			{Op: gen.Add, Path: "config.json", ContentBase64: ptr(base64.StdEncoding.EncodeToString(small))},
		}})
		require.True(t, *again.JSON201.NoOp)
		missing, _ := c.CreateCommitWithResponse(ctx, gen.Models, "acme", "m", gen.CommitRequest{Summary: "x", Operations: []gen.CommitOperation{
			{Op: gen.AddLfs, Path: "x.bin", Sha256: ptr(strings.Repeat("0", 64)), Size: ptr(int64(1))},
		}})
		require.Equal(t, http.StatusBadRequest, missing.StatusCode())

		// Private: anonymous callers get 404, Token holders the Repo.
		anon, _ := client(t, i, "").GetRepoWithResponse(ctx, gen.Models, "acme", "m")
		require.Equal(t, http.StatusNotFound, anon.StatusCode())
		got, _ := c.GetRepoWithResponse(ctx, gen.Models, "acme", "m")
		require.Equal(t, "acme/m", got.JSON200.Id)

		tree, err := c.GetTreeRootWithResponse(ctx, gen.Models, "acme", "m", "main", &gen.GetTreeRootParams{Recursive: ptr(true), Limit: ptr(1)})
		require.NoError(t, err)
		require.Len(t, tree.JSON200.Items, 1)
		require.NotNil(t, tree.JSON200.NextCursor)
		tree, _ = c.GetTreeRootWithResponse(ctx, gen.Models, "acme", "m", "main", &gen.GetTreeRootParams{Recursive: ptr(true), Cursor: tree.JSON200.NextCursor})
		require.Equal(t, "model.safetensors", tree.JSON200.Items[0].Path)
		require.Equal(t, shaHex(big), tree.JSON200.Items[0].Lfs.Oid)
		sub, _ := c.GetTreeWithResponse(ctx, gen.Models, "acme", "m", "main", "nothing", &gen.GetTreeParams{})
		require.Empty(t, sub.JSON200.Items)
		nf, _ := c.GetTreeRootWithResponse(ctx, gen.Models, "acme", "m", "nope", &gen.GetTreeRootParams{})
		require.Equal(t, http.StatusNotFound, nf.StatusCode())

		// Downloads with ranges and the Hub's headers.
		req, _ := http.NewRequest(http.MethodGet, i.URL+"/acme/m/resolve/main/model.safetensors", nil)
		req.Header.Set("Range", "bytes=1000-")
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		require.Equal(t, http.StatusPartialContent, resp.StatusCode)
		require.Equal(t, big[1000:], body)
		require.Equal(t, `"`+shaHex(big)+`"`, resp.Header.Get("X-Linked-Etag"))
		require.Equal(t, i.Head(mustID(t, "acme/m"), "main"), resp.Header.Get("X-Repo-Commit"))
		for _, u := range []string{"/acme/m/resolve/main/none.txt", "/acme/zz/resolve/main/x", "/datasets/acme/m/resolve/main/x"} {
			resp, err := http.Get(i.URL + u)
			require.NoError(t, err)
			_ = resp.Body.Close()
			require.Equal(t, http.StatusNotFound, resp.StatusCode, u)
		}
	}
}

func mustID(t *testing.T, s string) model.RepoID {
	t.Helper()
	id, err := model.ParseRepoID(model.RepoTypeModel, s)
	require.NoError(t, err)
	return id
}

func TestDiscoverPullsAndJobs(t *testing.T) {
	i := clitest.New(t)
	ctx := context.Background()
	c := client(t, i, clitest.Token)
	id := mustID(t, "org/tiny-GGUF")
	i.AddUpstream(id,
		clitest.File{Path: "tiny-Q4_K_M.gguf", Content: []byte("q4")},
		clitest.File{Path: "tiny-Q8_0.gguf", Content: []byte("q8")},
		clitest.File{Path: "mmproj-tiny-f16.gguf", Content: []byte("pj")},
		clitest.File{Path: "README.md", Content: []byte("hi")})

	d, err := c.DiscoverWithResponse(ctx, gen.Models, &gen.DiscoverParams{Search: ptr("TINY")})
	require.NoError(t, err)
	require.Len(t, d.JSON200.Items, 1)
	require.Equal(t, gen.ArsenalStateNone, d.JSON200.Items[0].Arsenal)
	dd, _ := c.GetDiscoverDetailWithResponse(ctx, gen.Models, "org", "tiny-GGUF", &gen.GetDiscoverDetailParams{})
	require.Len(t, *dd.JSON200.Quants, 2)
	nf, _ := c.GetDiscoverDetailWithResponse(ctx, gen.Models, "org", "none", &gen.GetDiscoverDetailParams{})
	require.Equal(t, http.StatusNotFound, nf.StatusCode())

	unauth, _ := client(t, i, "").StartPullWithResponse(ctx, gen.PullCreate{Upstream: "huggingface", Repo: id.String(), Type: gen.RepoTypeSingularModel})
	require.Equal(t, http.StatusUnauthorized, unauth.StatusCode())
	missing, _ := c.StartPullWithResponse(ctx, gen.PullCreate{Upstream: "huggingface", Repo: "org/none", Type: gen.RepoTypeSingularModel})
	require.Equal(t, http.StatusNotFound, missing.StatusCode())
	p, err := c.StartPullWithResponse(ctx, gen.PullCreate{Upstream: "huggingface", Repo: id.String(), Type: gen.RepoTypeSingularModel,
		Selection: &gen.Selection{Quants: &[]string{"q4_k_m"}, Exclude: &[]string{"README*"}}})
	require.NoError(t, err)
	require.Equal(t, http.StatusAccepted, p.StatusCode())
	j1, _ := c.GetJobWithResponse(ctx, p.JSON202.Id)
	require.Equal(t, gen.JobStatusRunning, j1.JSON200.Status)
	j2, _ := c.GetPullWithResponse(ctx, p.JSON202.Id)
	require.Equal(t, gen.JobStatusSucceeded, j2.JSON200.Status)
	require.True(t, i.Present(id, "main", "tiny-Q4_K_M.gguf"))
	require.True(t, i.Present(id, "main", "mmproj-tiny-f16.gguf"))
	require.False(t, i.Present(id, "main", "tiny-Q8_0.gguf"))

	q, _ := c.ListQuantsWithResponse(ctx, gen.Models, "org", "tiny-GGUF", &gen.ListQuantsParams{})
	require.Len(t, q.JSON200.Items, 2)
	list, _ := c.ListReposWithResponse(ctx, &gen.ListReposParams{Q: ptr("tiny"), Quant: ptr("Q8_0"), Type: ptr(gen.Models)})
	require.Len(t, list.JSON200.Items, 1)
	require.True(t, list.JSON200.Items[0].Mirror)
	deps, _ := c.GetDependenciesWithResponse(ctx, gen.Models, "org", "tiny-GGUF")
	require.Empty(t, deps.JSON200.Needs)

	more, _ := c.PullMoreIntoMirrorWithResponse(ctx, gen.Models, "org", "tiny-GGUF", gen.Selection{Paths: &[]string{"tiny-Q8_0.gguf"}})
	i.FailPulls = "boom"
	for range 2 {
		_, _ = c.GetJobWithResponse(ctx, more.JSON202.Id)
	}
	failed, _ := c.GetJobWithResponse(ctx, more.JSON202.Id)
	require.Equal(t, gen.JobStatusFailed, failed.JSON200.Status)
	require.Equal(t, "boom", *failed.JSON200.Error)
	i.FailPulls = ""

	third, _ := c.StartPullWithResponse(ctx, gen.PullCreate{Upstream: "huggingface", Repo: id.String(), Type: gen.RepoTypeSingularModel})
	cancelled, _ := c.CancelJobWithResponse(ctx, third.JSON202.Id)
	require.Equal(t, gen.JobStatusCancelled, cancelled.JSON200.Status)
	jobs, _ := c.ListJobsWithResponse(ctx, &gen.ListJobsParams{Status: ptr(gen.JobStatusCancelled)})
	require.Len(t, jobs.JSON200.Items, 1)
	nojob, _ := c.CancelJobWithResponse(ctx, cancelled.JSON200.Id)
	require.Equal(t, http.StatusOK, nojob.StatusCode())

	local := mustID(t, "acme/local")
	i.AddRepo(local, clitest.File{Path: "a.txt", Content: []byte("a")})
	notMirror, _ := c.PullMoreIntoMirrorWithResponse(ctx, gen.Models, "acme", "local", gen.Selection{})
	require.Equal(t, http.StatusConflict, notMirror.StatusCode())
	require.NotNil(t, i.Repo(local))
	require.Equal(t, []byte("a"), i.Files(local, "main")["a.txt"])
	require.Positive(t, i.RequestCount("POST /api/hangar/v1/pulls"))
}

func TestDatasetsParquetAndConfigs(t *testing.T) {
	i := clitest.New(t)
	ctx := context.Background()
	c := client(t, i, clitest.Token)
	id, _ := model.ParseRepoID(model.RepoTypeDataset, "org/data")
	i.AddUpstream(id, clitest.File{Path: "train.csv", Content: []byte("a")})
	i.AddUpstreamRevision(id, "refs/convert/parquet",
		clitest.File{Path: "default/train/0000.parquet", Content: []byte("p1")},
		clitest.File{Path: "default/test/0000.parquet", Content: []byte("p2")})
	p, _ := c.StartPullWithResponse(ctx, gen.PullCreate{Upstream: "huggingface", Repo: "org/data", Type: gen.RepoTypeSingularDataset,
		Selection: &gen.Selection{ParquetOnly: ptr(true), Configs: &[]string{"default"}, Splits: &[]string{"train"}}})
	for range 2 {
		_, _ = c.GetJobWithResponse(ctx, p.JSON202.Id)
	}
	require.True(t, i.Present(id, "refs/convert/parquet", "default/train/0000.parquet"))
	require.False(t, i.Present(id, "refs/convert/parquet", "default/test/0000.parquet"))
	tree, err := c.GetTreeRootWithResponse(ctx, gen.Datasets, "org", "data", "refs/convert/parquet", &gen.GetTreeRootParams{})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, tree.StatusCode(), string(tree.Body))
	require.Equal(t, "default", tree.JSON200.Items[0].Path, "a non-recursive listing shows folders")
	cfg, _ := c.ListDatasetConfigsWithResponse(ctx, "org", "data", &gen.ListDatasetConfigsParams{})
	require.Empty(t, cfg.JSON200.Configs)
}

func TestMeKeysAndDeviceLogin(t *testing.T) {
	i := clitest.New(t)
	ctx := context.Background()
	c := client(t, i, clitest.Token)
	me, _ := c.GetMeWithResponse(ctx)
	require.Equal(t, "alice", me.JSON200.Username)
	anon, _ := client(t, i, "").GetMeWithResponse(ctx)
	require.Equal(t, http.StatusNoContent, anon.StatusCode())

	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	sp, _ := ssh.NewPublicKey(pub)
	k, _ := c.AddMyOllamaKeyWithResponse(ctx, gen.OllamaKeyCreate{PublicKey: string(ssh.MarshalAuthorizedKey(sp))})
	require.Equal(t, ssh.FingerprintSHA256(sp), k.JSON201.Fingerprint)
	bad, _ := c.AddMyOllamaKeyWithResponse(ctx, gen.OllamaKeyCreate{PublicKey: "nope"})
	require.Equal(t, http.StatusUnprocessableEntity, bad.StatusCode())
	i.AddOllamaKey("SHA256:other")
	keys, _ := c.ListMyOllamaKeysWithResponse(ctx)
	require.Len(t, keys.JSON200.Items, 2)

	post := func(path string, form url.Values) map[string]any {
		resp, err := http.PostForm(i.URL+path, form)
		require.NoError(t, err)
		defer resp.Body.Close()
		var out map[string]any
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
		return out
	}
	require.Equal(t, "invalid_request", post("/oauth/device", url.Values{})["error"])
	dc := post("/oauth/device", url.Values{"client_id": {"x"}})
	code := dc["device_code"].(string)
	require.Equal(t, "authorization_pending", post("/oauth/token", url.Values{"device_code": {code}})["error"])
	require.True(t, i.ApproveDevice(dc["user_code"].(string)))
	tok := post("/oauth/token", url.Values{"device_code": {code}})["access_token"].(string)
	require.Equal(t, "expired_token", post("/oauth/token", url.Values{"device_code": {code}})["error"])
	me, _ = client(t, i, tok).GetMeWithResponse(ctx)
	require.Equal(t, "alice", me.JSON200.Username)

	dc = post("/oauth/device", url.Values{"client_id": {"x"}})
	require.True(t, i.DenyDevice(dc["user_code"].(string)))
	require.Equal(t, "access_denied", post("/oauth/token", url.Values{"device_code": {dc["device_code"].(string)}})["error"])
	require.False(t, i.ApproveDevice("none"))
	require.False(t, i.DenyDevice("none"))
	i.DeviceAutoApprove = true
	dc = post("/oauth/device", url.Values{"client_id": {"x"}})
	require.NotEmpty(t, post("/oauth/token", url.Values{"device_code": {dc["device_code"].(string)}})["access_token"])

	// Unimplemented operations answer 501.
	resp, err := c.ListUpstreamsWithResponse(ctx)
	require.NoError(t, err)
	require.Equal(t, http.StatusNotImplemented, resp.StatusCode())

	presigned, _ := http.NewRequest(http.MethodPut, i.URL+"/presigned/not-a-uuid/1?sig=fake", strings.NewReader("x"))
	r2, err := http.DefaultClient.Do(presigned)
	require.NoError(t, err)
	_ = r2.Body.Close()
	require.Equal(t, http.StatusBadRequest, r2.StatusCode)
	i.Close()
}

func TestPresignedDownloadRedirects(t *testing.T) {
	i := clitest.New(t)
	i.RedirectDownloads = true
	weights := bytes.Repeat([]byte("w"), 4<<10)
	i.AddRepo(model.RepoID{Type: model.RepoTypeModel, Namespace: "acme", Name: "m"},
		clitest.File{Path: "w.safetensors", Content: weights}, clitest.File{Path: "README.md", Content: []byte("# M\n")})
	require.NotEqual(t, i.URL, i.PresignBase, "presigned URLs are on another origin")
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	get := func(c *http.Client, u string, auth bool) *http.Response {
		req, err := http.NewRequest(http.MethodGet, u, nil)
		require.NoError(t, err)
		if auth {
			req.Header.Set("Authorization", "Bearer "+clitest.Token)
		}
		resp, err := c.Do(req)
		require.NoError(t, err)
		t.Cleanup(func() { _ = resp.Body.Close() })
		return resp
	}

	// The LFS file redirects to a presigned URL; the small file does not.
	resp := get(noFollow, i.URL+"/acme/m/resolve/main/w.safetensors", false)
	require.Equal(t, http.StatusFound, resp.StatusCode)
	loc := resp.Header.Get("Location")
	require.True(t, strings.HasPrefix(loc, i.PresignBase+"/presigned-objects/"), loc)
	require.Equal(t, "4096", resp.Header.Get("X-Linked-Size"))
	require.Equal(t, http.StatusOK, get(noFollow, i.URL+"/acme/m/resolve/main/README.md", false).StatusCode)

	resp = get(http.DefaultClient, loc, false)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, weights, body)

	require.Equal(t, http.StatusForbidden, get(http.DefaultClient, loc, true).StatusCode, "a presigned URL takes no Authorization header")
	require.Equal(t, http.StatusForbidden, get(http.DefaultClient, strings.TrimSuffix(loc, "?sig=fake"), false).StatusCode)
	require.Equal(t, http.StatusNotFound, get(http.DefaultClient, i.PresignBase+"/presigned-objects/missing?sig=fake", false).StatusCode)
}

func TestFailPartLastWaitsForTheOtherParts(t *testing.T) {
	i := clitest.New(t)
	i.PartSize = 1 << 10
	ctx := context.Background()
	c := client(t, i, clitest.Token)
	r, err := c.CreateRepoWithResponse(ctx, gen.RepoCreate{Type: gen.RepoTypeSingularModel, Namespace: "acme", Name: "m"})
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, r.StatusCode())

	big := bytes.Repeat([]byte("weights!"), 400) // 3200 bytes, four parts
	plan, err := c.PlanUploadsWithResponse(ctx, gen.Models, "acme", "m", gen.UploadPlanRequest{Files: []gen.UploadFile{
		{Path: "model.safetensors", Size: int64(len(big)), Sha256: shaHex(big)},
	}})
	require.NoError(t, err)
	urls := *plan.JSON200.Files[0].PartUrls
	require.Len(t, urls, 4)
	put := func(n int) int {
		chunk := big[(n-1)*1024 : min(n*1024, len(big))]
		req, _ := http.NewRequest(http.MethodPut, urls[n-1], bytes.NewReader(chunk))
		req.Header.Set("Authorization", "Bearer "+clitest.Token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}

	i.FailPartLast(shaHex(big), 2, 1)
	failed := make(chan int, 1)
	go func() { failed <- put(2) }()
	for _, n := range []int{1, 3} {
		require.Equal(t, http.StatusOK, put(n))
	}
	select {
	case code := <-failed:
		t.Fatalf("part 2 was answered (%d) before part 4 was stored", code)
	case <-time.After(100 * time.Millisecond):
	}
	require.Equal(t, http.StatusOK, put(4))
	select {
	case code := <-failed:
		require.Equal(t, http.StatusInternalServerError, code)
	case <-time.After(clitest.PartFailureWait / 2):
		t.Fatal("part 2 was not answered after the other parts were stored")
	}
	require.Equal(t, 3, i.PartPuts())

	// The failure was used up: part 2 is now stored.
	require.Equal(t, http.StatusOK, put(2))
	require.Equal(t, 4, i.PartPuts())
}

// The fake's upload plan follows the real server's: a file goes inline when
// the Repo's .gitattributes leaves it out of LFS and it is no larger than
// InlineMax (10 MiB by default, hfapi.regular_upload_max), whatever its
// extension. A new Repo starts with the Hub's default .gitattributes, which
// has no *.gguf rule.
func TestUploadPlanFollowsGitAttributes(t *testing.T) {
	i := clitest.New(t)
	ctx := context.Background()
	c := client(t, i, clitest.Token)
	r, err := c.CreateRepoWithResponse(ctx, gen.RepoCreate{Type: gen.RepoTypeSingularModel, Namespace: "acme", Name: "q"})
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, r.StatusCode())
	attrs := i.Files(mustID(t, "acme/q"), "main")[".gitattributes"]
	require.Contains(t, string(attrs), "*.safetensors filter=lfs")
	require.NotContains(t, string(attrs), "*.gguf")

	gguf, st := []byte("GGUF small quant"), []byte("small weights")
	plan := func() []gen.UploadPlanItem {
		t.Helper()
		p, err := c.PlanUploadsWithResponse(ctx, gen.Models, "acme", "q", gen.UploadPlanRequest{Files: []gen.UploadFile{
			{Path: "tiny-Q4_K_M.gguf", Size: int64(len(gguf)), Sha256: shaHex(gguf)},
			{Path: "model.safetensors", Size: int64(len(st)), Sha256: shaHex(st)},
			{Path: "big.gguf", Size: 10<<20 + 1, Sha256: strings.Repeat("a", 64)},
		}})
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, p.StatusCode(), string(p.Body))
		return p.JSON200.Files
	}
	got := plan()
	require.Equal(t, gen.Inline, got[0].Action, "a small GGUF file is a regular blob")
	require.Equal(t, gen.Upload, got[1].Action, ".gitattributes puts safetensors in LFS")
	require.Equal(t, gen.Upload, got[2].Action, "over InlineMax")

	// Committed inline, the GGUF file has no LFS metadata in the tree.
	cr, err := c.CreateCommitWithResponse(ctx, gen.Models, "acme", "q", gen.CommitRequest{Summary: "Add", Operations: []gen.CommitOperation{
		{Op: gen.Add, Path: "tiny-Q4_K_M.gguf", ContentBase64: ptr(base64.StdEncoding.EncodeToString(gguf))},
	}})
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, cr.StatusCode(), string(cr.Body))
	tree, err := c.GetTreeRootWithResponse(ctx, gen.Models, "acme", "q", "main", &gen.GetTreeRootParams{})
	require.NoError(t, err)
	for _, e := range tree.JSON200.Items {
		if e.Path == "tiny-Q4_K_M.gguf" {
			require.Nil(t, e.Lfs)
		}
	}

	// Once the Repo's .gitattributes tracks *.gguf, GGUF files are LFS
	// objects, through the plan and through an inline add. As on the server,
	// a Commit follows its parent's .gitattributes.
	commit := func(summary, p string, content []byte) {
		t.Helper()
		cr, err := c.CreateCommitWithResponse(ctx, gen.Models, "acme", "q", gen.CommitRequest{Summary: summary, Operations: []gen.CommitOperation{
			{Op: gen.Add, Path: p, ContentBase64: ptr(base64.StdEncoding.EncodeToString(content))},
		}})
		require.NoError(t, err)
		require.Equal(t, http.StatusCreated, cr.StatusCode(), string(cr.Body))
	}
	commit("Track GGUF", ".gitattributes", append(attrs, []byte("*.gguf filter=lfs diff=lfs merge=lfs -text\n")...))
	require.Equal(t, gen.Upload, plan()[0].Action)
	commit("Add Q8_0", "other-Q8_0.gguf", gguf)
	require.True(t, i.IsLFS(mustID(t, "acme/q"), "main", "other-Q8_0.gguf"))
	require.False(t, i.IsLFS(mustID(t, "acme/q"), "main", "tiny-Q4_K_M.gguf"))
	require.Equal(t, gguf, i.Files(mustID(t, "acme/q"), "main")["other-Q8_0.gguf"])
}
