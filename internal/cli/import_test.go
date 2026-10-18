package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/model"
)

// cacheSnapshot writes files into a Hugging Face cache the way
// huggingface_hub does and returns the Commit SHA used.
func cacheSnapshot(t *testing.T, root string, id model.RepoID, commit string, files map[string][]byte) {
	t.Helper()
	repoDir := filepath.Join(root, repoFolderName(id))
	for p, c := range files {
		etag := model.GitBlobID(c)
		if strings.HasSuffix(p, ".gguf") || strings.HasSuffix(p, ".safetensors") {
			etag = sha(c)
		}
		blob := filepath.Join(repoDir, "blobs", etag)
		require.NoError(t, os.MkdirAll(filepath.Dir(blob), 0o755))
		require.NoError(t, os.WriteFile(blob, c, 0o644))
		link := filepath.Join(repoDir, "snapshots", commit, filepath.FromSlash(p))
		require.NoError(t, os.MkdirAll(filepath.Dir(link), 0o755))
		rel, _ := filepath.Rel(filepath.Dir(link), blob)
		require.NoError(t, os.Symlink(rel, link))
	}
	require.NoError(t, os.MkdirAll(filepath.Join(repoDir, "refs"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(repoDir, "refs", "main"), []byte(commit), 0o644))
}

func TestImportFromHFCache(t *testing.T) {
	e := newEnv(t)
	e.login()
	id, files := ggufRepo(e)
	cache := filepath.Join(e.home, "hf-cache")
	commit := strings.Repeat("ab", 20)
	cacheSnapshot(t, cache, id, commit, map[string][]byte{
		"Tiny-Q4_K_M.gguf": files["Tiny-Q4_K_M.gguf"],
		"README.md":        files["README.md"],
	})
	private := mustID(t, model.RepoTypeModel, "acme/private-ft")
	cacheSnapshot(t, cache, private, strings.Repeat("cd", 20), map[string][]byte{
		"model.safetensors": blobOf("ft", 3000),
		"config.json":       []byte("{}"),
	})
	// Not a Repo folder, so ignored.
	require.NoError(t, os.MkdirAll(filepath.Join(cache, ".locks"), 0o755))

	r := e.ok("import", "--from-hf-cache", "--dry-run")
	require.Contains(t, r.stdout, "bartowski/Tiny-GGUF")
	require.Contains(t, r.stdout, "acme/private-ft")
	require.Zero(t, e.inst.RequestCount("POST /api/hangar/v1/pulls"))

	r = e.ok("import", "--from-hf-cache", "--json")
	var res []importResult
	require.NoError(t, json.Unmarshal([]byte(r.stdout), &res))
	require.Len(t, res, 2)
	byRepo := map[string]importResult{}
	for _, x := range res {
		byRepo[x.Repo] = x
	}
	mirror := byRepo["bartowski/Tiny-GGUF"]
	require.Equal(t, "mirror", mirror.Mode, mirror.Error)
	require.Equal(t, []string{"Tiny-Q4_K_M.gguf"}, mirror.Uploaded, "only the LFS object is uploaded; small files come with the history")
	require.True(t, e.inst.Present(id, commit, "Tiny-Q4_K_M.gguf"))
	require.False(t, e.inst.Present(id, commit, "Tiny-Q8_0.gguf"))
	require.Zero(t, e.inst.RequestCount("GET /{ns}/{name}/resolve/{rev}/*"), "nothing is downloaded")
	require.Equal(t, 1, e.inst.RequestCount("POST /api/hangar/v1/repos/{type}/{ns}/{name}/mirror/pull"),
		"the uploaded files are pulled into the Mirror, which marks them present")

	local := byRepo["acme/private-ft"]
	require.Equal(t, "local", local.Mode, local.Error)
	require.Equal(t, blobOf("ft", 3000), e.inst.Files(private, "main")["model.safetensors"])
}

func TestImportFromHFCacheOffline(t *testing.T) {
	e := newEnv(t)
	e.login()
	id, files := ggufRepo(e)
	cache := filepath.Join(e.home, "other-cache")
	cacheSnapshot(t, cache, id, strings.Repeat("ef", 20), map[string][]byte{"Tiny-Q8_0.gguf": files["Tiny-Q8_0.gguf"]})
	r := e.ok("import", "--from-hf-cache", cache, "--offline", "--namespace", "acme")
	require.Contains(t, r.stdout, "acme/Tiny-GGUF")
	require.Contains(t, r.stdout, "local")
	require.Zero(t, e.inst.RequestCount("POST /api/hangar/v1/pulls"))
	require.Equal(t, files["Tiny-Q8_0.gguf"], e.inst.Files(mustID(t, model.RepoTypeModel, "acme/Tiny-GGUF"), "main")["Tiny-Q8_0.gguf"])
}

func TestImportFromLMStudio(t *testing.T) {
	e := newEnv(t)
	e.login()
	_, files := ggufRepo(e)
	lm := filepath.Join(e.home, ".lmstudio", "models")
	writeTree(t, lm, map[string][]byte{
		"bartowski/Tiny-GGUF/Tiny-Q8_0.gguf":    files["Tiny-Q8_0.gguf"],
		"bartowski/Tiny-GGUF/Tiny-Q4_K_M.gguf":  blobOf("not the upstream bytes", 1000),
		"someone/custom model/custom-Q4_0.gguf": blobOf("custom", 1500),
		".hidden/skip/x.gguf":                   []byte("x"),
	})
	r := e.ok("import", "--from-lmstudio", "--json")
	var res []importResult
	require.NoError(t, json.Unmarshal([]byte(r.stdout), &res))
	require.Len(t, res, 2)
	for _, x := range res {
		require.Empty(t, x.Error)
		switch x.Repo {
		case "bartowski/Tiny-GGUF":
			require.Equal(t, "mirror", x.Mode)
			require.Equal(t, []string{"Tiny-Q8_0.gguf"}, x.Uploaded)
			require.Equal(t, []string{"Tiny-Q4_K_M.gguf"}, x.Skipped, "a file whose SHA-256 differs is not imported")
		case "someone/custom-model":
			require.Equal(t, "local", x.Mode)
		default:
			t.Fatalf("unexpected %s", x.Repo)
		}
	}
	require.Equal(t, blobOf("custom", 1500), e.inst.Files(mustID(t, model.RepoTypeModel, "someone/custom-model"), "main")["custom-Q4_0.gguf"])
}

func TestImportFromOllama(t *testing.T) {
	e := newEnv(t)
	e.login()
	models := filepath.Join(e.home, ".ollama", "models")
	gguf := blobOf("ollama-gguf", 4000)
	tmpl := []byte("{{ .Prompt }}")
	params := []byte(`{"stop":["<|end|>"]}`)
	layers := ""
	for _, l := range []struct {
		mt string
		b  []byte
	}{{"application/vnd.ollama.image.model", gguf}, {"application/vnd.ollama.image.template", tmpl}, {"application/vnd.ollama.image.params", params}, {"application/vnd.ollama.image.unknown", []byte("u")}} {
		require.NoError(t, os.MkdirAll(filepath.Join(models, "blobs"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(models, "blobs", "sha256-"+sha(l.b)), l.b, 0o644))
		if layers != "" {
			layers += ","
		}
		layers += fmt.Sprintf(`{"mediaType":%q,"digest":"sha256:%s","size":%d}`, l.mt, sha(l.b), len(l.b))
	}
	manifest := `{"schemaVersion":2,"layers":[` + layers + `]}`
	writeTree(t, filepath.Join(models, "manifests"), map[string][]byte{
		"registry.ollama.ai/library/tiny/latest": []byte(manifest),
		"registry.ollama.ai/library/tiny/1b":     []byte(manifest),
		"registry.ollama.ai/someone/thing/q4":    []byte(`{"layers":[]}`),
	})

	r := e.run("import", "--from-ollama", "--json")
	require.Equal(t, ExitFailure, r.code, "one manifest has no model layer")
	var res []importResult
	require.NoError(t, json.Unmarshal([]byte(r.stdout), &res))
	require.Len(t, res, 3)
	got := e.inst.Files(mustID(t, model.RepoTypeModel, "alice/tiny"), "main")
	require.Equal(t, gguf, got["tiny.gguf"])
	require.Equal(t, tmpl, got["template"])
	require.Equal(t, params, got["params"])
	got = e.inst.Files(mustID(t, model.RepoTypeModel, "alice/tiny-1b"), "main")
	require.Equal(t, gguf, got["tiny-1b.gguf"])
	for _, x := range res {
		if x.Repo == "alice/someone-thing-q4" {
			require.Contains(t, x.Error, "no GGUF model layer")
		}
	}

	r = e.run("import", "--from-ollama", "--dry-run", "--namespace", "acme")
	require.Equal(t, ExitFailure, r.code)
	require.Contains(t, r.stdout, "acme/tiny ")
	require.Contains(t, r.stdout, "failed: the manifest has no GGUF model layer")
	t.Setenv("OLLAMA_MODELS", filepath.Join(e.home, "nowhere"))
	require.Equal(t, ExitNotFound, e.run("import", "--from-ollama").code)
}

func TestImportFromDirAndUsage(t *testing.T) {
	e := newEnv(t)
	e.login()
	dir := filepath.Join(e.home, "weights")
	writeTree(t, dir, map[string][]byte{"w.safetensors": blobOf("w", 2000)})
	r := e.ok("import", "--from-dir", dir, "--repo", "acme/weights")
	require.Contains(t, r.stdout, "acme/weights")
	require.Equal(t, blobOf("w", 2000), e.inst.Files(mustID(t, model.RepoTypeModel, "acme/weights"), "main")["w.safetensors"])
	r = e.ok("import", "--from-dir", dir, "--repo", "acme/weights2", "--dry-run")
	require.Contains(t, r.stdout, "1 files")

	require.Equal(t, ExitUsage, e.run("import").code)
	require.Equal(t, ExitUsage, e.run("import", "--from-dir", dir).code)
	require.Equal(t, ExitUsage, e.run("import", "--from-dir", dir, "--from-ollama").code)
	require.Equal(t, ExitFailure, e.run("import", "--from-lmstudio", filepath.Join(e.home, "none")).code)
	empty := filepath.Join(e.home, "empty-cache")
	require.NoError(t, os.MkdirAll(empty, 0o755))
	r = e.ok("import", "--from-hf-cache", empty, "--dry-run")
	require.Contains(t, r.stdout, "Nothing to import")
	require.Equal(t, ExitFailure, e.run("import", "--from-hf-cache", filepath.Join(e.home, "missing")).code)
}

func TestSanitiseName(t *testing.T) {
	require.Equal(t, "custom-model", sanitiseName("custom model"))
	require.Equal(t, "a-b", sanitiseName("..a/b.."))
	require.Len(t, sanitiseName(strings.Repeat("x", 200)), 96)
}
