package cli

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/cli/clitest"
	"github.com/hangar-hub/hangar/internal/model"
)

// TestPushUploadsSharedContentOnce is a regression test for issue #28: two
// files with the same content in one upload plan share one upload on the
// Instance, and the first completion removes it, so the second file's
// status request answered 404 and the push failed. The object now goes up
// once and both files are committed.
func TestPushUploadsSharedContentOnce(t *testing.T) {
	e := newEnv(t)
	e.login()
	dir := filepath.Join(e.home, "model")
	weights := blobOf("shared", 256<<10) // four parts
	writeTree(t, dir, map[string][]byte{
		"a/model.safetensors": weights,
		"b/model.safetensors": weights,
	})
	id := mustID(t, model.RepoTypeModel, "acme/shared")
	e.inst.AddRepo(id)

	e.ok("push", dir, "acme/shared")
	require.Equal(t, 4, e.inst.PartPuts(), "the shared object goes up once")
	got := e.inst.Files(id, "main")
	require.Equal(t, weights, got["a/model.safetensors"])
	require.Equal(t, weights, got["b/model.safetensors"])
}

// TestUploadSucceedsWhenAnotherUploadStoresTheObject is a regression test
// for issue #28: when another upload of the same object completed first,
// the Instance answered 404 for this file's upload and the CLI failed
// instead of noticing the object was stored.
func TestUploadSucceedsWhenAnotherUploadStoresTheObject(t *testing.T) {
	e := newEnv(t)
	e.login()
	dir := filepath.Join(e.home, "model")
	weights := blobOf("landed", 256<<10)
	writeTree(t, dir, map[string][]byte{"model.safetensors": weights})
	id := mustID(t, model.RepoTypeModel, "acme/landed")
	e.inst.AddRepo(id)
	e.inst.LandDuringUpload(weights)

	e.ok("push", dir, "acme/landed")
	require.Zero(t, e.inst.PartPuts(), "the object was stored by the other upload")
	require.Equal(t, weights, e.inst.Files(id, "main")["model.safetensors"])
}

// TestImportFromHFCacheWithSharedContent is the import side of issue #28:
// a cached snapshot that holds the same weights under two paths failed
// with "The upload does not exist or has expired".
func TestImportFromHFCacheWithSharedContent(t *testing.T) {
	e := newEnv(t)
	e.login()
	id := mustID(t, model.RepoTypeModel, "acme/two-copies")
	weights := blobOf("copies", 200<<10)
	e.inst.AddUpstream(id,
		clitest.File{Path: "one/model.safetensors", Content: weights},
		clitest.File{Path: "two/model.safetensors", Content: weights},
		clitest.File{Path: "config.json", Content: []byte("{}")},
	)
	commit := strings.Repeat("ab", 20)
	cacheSnapshot(t, filepath.Join(e.home, "hf-cache"), id, commit, map[string][]byte{
		"one/model.safetensors": weights,
		"two/model.safetensors": weights,
	})

	r := e.ok("import", "--from-hf-cache", "--json")
	var res []importResult
	require.NoError(t, json.Unmarshal([]byte(r.stdout), &res))
	require.Len(t, res, 1)
	require.Empty(t, res[0].Error)
	require.Equal(t, "mirror", res[0].Mode)
	require.Equal(t, []string{"one/model.safetensors", "two/model.safetensors"}, res[0].Uploaded)
	require.Equal(t, 4, e.inst.PartPuts(), "the shared object goes up once")
	require.True(t, e.inst.Present(id, commit, "one/model.safetensors"))
	require.True(t, e.inst.Present(id, commit, "two/model.safetensors"))
}
