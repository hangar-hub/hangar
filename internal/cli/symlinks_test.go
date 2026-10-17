package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/model"
)

// TestPushSkipsSymlinksOutOfTheFolder checks that hangar push and hangar
// import --from-dir never upload a file a symbolic link points to outside
// the folder. Before, a model folder holding config.json ->
// ../../.ssh/id_ed25519 uploaded the key.
func TestPushSkipsSymlinksOutOfTheFolder(t *testing.T) {
	e := newEnv(t)
	e.login()
	secret := filepath.Join(e.home, "secret-key")
	require.NoError(t, os.WriteFile(secret, []byte("PRIVATE KEY"), 0o600))
	dir := filepath.Join(e.home, "model")
	writeTree(t, dir, map[string][]byte{"README.md": []byte("# Model\n"), "weights/real.bin": []byte("weights")})
	rel, err := filepath.Rel(dir, secret)
	require.NoError(t, err)
	require.NoError(t, os.Symlink(rel, filepath.Join(dir, "config.json")))
	require.NoError(t, os.Symlink(secret, filepath.Join(dir, "weights", "abs.bin")))
	require.NoError(t, os.Symlink("real.bin", filepath.Join(dir, "weights", "alias.bin")))

	r := e.ok("push", dir, "acme/linked", "--create")
	require.Contains(t, r.stderr, "Skipped config.json: it is a symbolic link to a file outside "+dir)
	require.Contains(t, r.stderr, "Skipped weights/abs.bin")
	got := e.inst.Files(mustID(t, model.RepoTypeModel, "acme/linked"), "main")
	require.NotContains(t, got, "config.json")
	require.NotContains(t, got, "weights/abs.bin")
	require.Equal(t, []byte("weights"), got["weights/alias.bin"], "a link inside the folder is followed")
	for p, c := range got {
		require.NotContains(t, string(c), "PRIVATE KEY", p)
	}

	r = e.ok("import", "--from-dir", dir, "--repo", "acme/imported")
	require.Contains(t, r.stderr, "Skipped config.json")
	require.NotContains(t, e.inst.Files(mustID(t, model.RepoTypeModel, "acme/imported"), "main"), "config.json")
}

// TestSymlinksIntoACacheRepoAreFollowed checks that a Hugging Face cache
// snapshot, whose files link into the Repo's blobs folder, still pushes
// and imports, while a link out of the Repo's cache folder does not.
func TestSymlinksIntoACacheRepoAreFollowed(t *testing.T) {
	e := newEnv(t)
	e.login()
	cache := filepath.Join(e.home, "hf-cache")
	id := mustID(t, model.RepoTypeModel, "acme/cached")
	commit := strings.Repeat("ef", 20)
	cacheSnapshot(t, cache, id, commit, map[string][]byte{"config.json": []byte("{}"), "model.safetensors": blobOf("w", 2000)})
	secret := filepath.Join(e.home, "secret-token")
	require.NoError(t, os.WriteFile(secret, []byte("hf_secret"), 0o600))
	snap := filepath.Join(cache, repoFolderName(id), "snapshots", commit)
	require.NoError(t, os.Symlink(secret, filepath.Join(snap, "token.txt")))

	r := e.ok("push", snap, "acme/pushed", "--create")
	require.Contains(t, r.stderr, "Skipped token.txt")
	got := e.inst.Files(mustID(t, model.RepoTypeModel, "acme/pushed"), "main")
	require.Equal(t, []byte("{}"), got["config.json"])
	require.Equal(t, blobOf("w", 2000), got["model.safetensors"])
	require.NotContains(t, got, "token.txt")

	r = e.ok("import", "--from-hf-cache", cache, "--offline")
	require.Contains(t, r.stderr, "Skipped token.txt")
	got = e.inst.Files(id, "main")
	require.Equal(t, blobOf("w", 2000), got["model.safetensors"])
	require.NotContains(t, got, "token.txt")
}

// TestSymlinksToCacheBlobsAreFollowed checks that a folder hangar lmstudio
// filled with links into the Hugging Face cache still imports.
func TestSymlinksToCacheBlobsAreFollowed(t *testing.T) {
	e := newEnv(t)
	e.login()
	cache := filepath.Join(e.home, "hf-cache")
	id := mustID(t, model.RepoTypeModel, "acme/linked-gguf")
	commit := strings.Repeat("12", 20)
	weights := blobOf("q4", 4000)
	cacheSnapshot(t, cache, id, commit, map[string][]byte{"m-Q4_K_M.gguf": weights})
	lm := filepath.Join(e.home, "lmstudio", "acme", "linked-gguf")
	require.NoError(t, os.MkdirAll(lm, 0o755))
	require.NoError(t, os.Symlink(filepath.Join(cache, repoFolderName(id), "blobs", sha(weights)), filepath.Join(lm, "m-Q4_K_M.gguf")))
	r := e.ok("push", lm, "acme/from-lm", "--create")
	require.NotContains(t, r.stderr, "Skipped")
	require.Equal(t, weights, e.inst.Files(mustID(t, model.RepoTypeModel, "acme/from-lm"), "main")["m-Q4_K_M.gguf"])
}

func TestWithin(t *testing.T) {
	for _, c := range []struct {
		path, dir string
		want      bool
	}{
		{"/a/b", "/a/b", true}, {"/a/b/c", "/a/b", true}, {"/a/bc", "/a/b", false}, {"/a", "/a/b", false},
		{"/a/b/..c", "/a/b", true}, {"/x/y", "/a/b", false},
	} {
		require.Equal(t, c.want, within(c.path, c.dir), "%s in %s", c.path, c.dir)
	}
}
