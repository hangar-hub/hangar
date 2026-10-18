package upstreamtest_test

import (
	"context"
	"errors"
	"io"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/model"
	"github.com/hangar-hub/hangar/internal/repo"
	"github.com/hangar-hub/hangar/internal/repo/repotest"
	"github.com/hangar-hub/hangar/internal/upstream"
	"github.com/hangar-hub/hangar/internal/upstream/upstreamtest"
)

var (
	hf   = upstream.Upstream{Name: "huggingface", BaseURL: "https://huggingface.co", Enabled: true}
	jina = model.RepoID{Type: model.RepoTypeModel, Namespace: "jinaai", Name: "jina-embeddings-v3"}
	anon = upstream.Credentials{}
)

func seed(t *testing.T) (*upstreamtest.Fake, model.CommitSHA, string) {
	t.Helper()
	ctx := context.Background()
	f := upstreamtest.NewFake(t)
	_, err := f.Repos.Create(ctx, repo.CreateParams{ID: jina})
	require.NoError(t, err)
	weights := []byte(strings.Repeat("w", 100))
	obj, err := f.Repos.LFS.Put(ctx, weights)
	require.NoError(t, err)
	res, err := f.Repos.Commit(ctx, repo.CommitParams{Repo: jina, Summary: "weights", Operations: []repo.Operation{
		{Kind: repo.OpAdd, Path: "config.json", Content: strings.NewReader(`{"auto_map":{"AutoModel":"jinaai/xlm-roberta-flash-implementation--modeling_lora.XLMRobertaLoRA"}}`)},
		{Kind: repo.OpAddLFS, Path: "model.safetensors", OID: obj.OID, Size: obj.Size},
	}})
	require.NoError(t, err)
	return f, res.SHA, obj.OID
}

func TestFakeInfoTreeAndDownload(t *testing.T) {
	ctx := context.Background()
	f, sha, oid := seed(t)

	info, err := f.RepoInfo(ctx, hf, anon, jina, "main")
	require.NoError(t, err)
	require.Equal(t, sha, info.SHA)
	require.Len(t, info.Siblings, 3)
	require.Contains(t, string(info.Raw), string(sha))

	entries, err := f.Tree(ctx, hf, anon, jina, "", "", true)
	require.NoError(t, err)
	require.Len(t, entries, 3)

	rc, meta, err := f.Download(ctx, hf, anon, jina, sha, "model.safetensors", nil)
	require.NoError(t, err)
	b, _ := io.ReadAll(rc)
	require.Len(t, b, 100)
	require.Equal(t, oid, meta.ETag)
	require.True(t, meta.LFS)

	_, err = f.RepoInfo(ctx, hf, anon, jina, "nope")
	require.ErrorIs(t, err, upstream.ErrNotFound)
	_, err = f.RepoInfo(ctx, hf, anon, model.RepoID{Type: model.RepoTypeModel, Namespace: "no", Name: "pe"}, "")
	require.ErrorIs(t, err, upstream.ErrNotFound)
	_, _, err = f.Download(ctx, hf, anon, jina, sha, "missing.bin", nil)
	require.ErrorIs(t, err, upstream.ErrNotFound)

	page, err := f.Search(ctx, hf, anon, upstream.SearchQuery{Search: "jina", Limit: 10})
	require.NoError(t, err)
	require.Len(t, page.Repos, 1)
	require.Equal(t, sha, page.Repos[0].SHA)
	require.Equal(t, 1, f.Calls("Search"))
}

func TestFakePrivateGatedAndFailures(t *testing.T) {
	ctx := context.Background()
	f, _, _ := seed(t)
	priv := model.VisibilityPrivate
	_, err := f.Repos.UpdateSettings(ctx, jina, repo.SettingsUpdate{Visibility: &priv}, model.System())
	require.NoError(t, err)
	f.RequireToken = "hf_secret"
	_, err = f.RepoInfo(ctx, hf, anon, jina, "")
	require.ErrorIs(t, err, upstream.ErrNotFound)
	_, err = f.RepoInfo(ctx, hf, upstream.Credentials{Token: "hf_secret"}, jina, "")
	require.NoError(t, err)
	page, err := f.Search(ctx, hf, upstream.Credentials{Token: "hf_secret"}, upstream.SearchQuery{Author: "jinaai"})
	require.NoError(t, err)
	require.Len(t, page.Repos, 1)

	f.Gated = []string{jina.String()}
	_, err = f.Tree(ctx, hf, upstream.Credentials{Token: "hf_secret"}, jina, "", "", false)
	require.ErrorIs(t, err, upstream.ErrGated)

	boom := &upstream.RateLimitError{}
	f.FailNext("Download", boom)
	_, _, err = f.Download(ctx, hf, anon, jina, "", "x", nil)
	require.ErrorAs(t, err, &boom)
}

func TestFakeFetchPackKeepsCommitSHAs(t *testing.T) {
	ctx := context.Background()
	f, sha, _ := seed(t)
	res, err := f.FetchPack(ctx, hf, anon, jina, nil)
	require.NoError(t, err)
	require.Equal(t, sha, res.Refs["refs/heads/main"])
	require.NotNil(t, res.Pack)

	// Store the pack in a Mirror: the Commit is identical.
	local := repotest.NewFake(t)
	_, err = local.Create(ctx, repo.CreateParams{ID: jina, Mirror: true})
	require.NoError(t, err)
	var updates []repo.RefUpdate
	for name, target := range res.Refs {
		updates = append(updates, repo.RefUpdate{Name: name, New: target})
	}
	require.NoError(t, local.ApplyPack(ctx, repo.ApplyPackParams{Repo: jina, Source: repo.SourceMirror, Pack: res.Pack, Updates: updates}))
	got, err := local.Resolve(ctx, jina, "main")
	require.NoError(t, err)
	require.Equal(t, sha, got)
	e, err := local.Entry(ctx, jina, got, "model.safetensors")
	require.NoError(t, err)
	require.False(t, e.Present, "the LFS object is not in the Mirror until a Pull transfers it")
	refs, err := local.LFSRefs(ctx, jina, []string{e.LFS.OID})
	require.NoError(t, err)
	require.False(t, refs[e.LFS.OID].Present, "pointers in fetched packs are referenced but not present")
	require.Equal(t, int64(100), refs[e.LFS.OID].Size)

	// With what we have, there is nothing new.
	again, err := f.FetchPack(ctx, hf, anon, jina, []model.CommitSHA{sha})
	require.NoError(t, err)
	require.Nil(t, again.Pack)
}

func TestFakeViewer(t *testing.T) {
	ctx := context.Background()
	f := upstreamtest.NewFake(t)
	q := url.Values{"dataset": {"ns/d"}}
	f.SetViewerResponse("/splits", q, `{"splits":[]}`)
	body, err := f.Viewer(ctx, hf, anon, "/splits", q)
	require.NoError(t, err)
	require.JSONEq(t, `{"splits":[]}`, string(body))
	_, err = f.Viewer(ctx, hf, anon, "/rows", q)
	require.ErrorIs(t, err, upstream.ErrNotFound)
	f.FailNext("Viewer", errors.New("down"))
	_, err = f.Viewer(ctx, hf, anon, "/splits", q)
	require.Error(t, err)
}
