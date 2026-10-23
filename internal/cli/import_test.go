package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/cli/clitest"
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

func TestImportOnly(t *testing.T) {
	e := newEnv(t)
	e.login()
	id, files := ggufRepo(e)
	cache := filepath.Join(e.home, "hf-cache")
	cacheSnapshot(t, cache, id, strings.Repeat("ab", 20), map[string][]byte{"Tiny-Q4_K_M.gguf": files["Tiny-Q4_K_M.gguf"]})
	for _, n := range []string{"acme/one", "acme/two"} {
		cacheSnapshot(t, cache, mustID(t, model.RepoTypeModel, n), strings.Repeat("cd", 20), map[string][]byte{"config.json": []byte(n)})
	}

	imported := func(r result) []string {
		t.Helper()
		var res []importResult
		require.NoError(t, json.Unmarshal([]byte(r.stdout), &res))
		var repos []string
		for _, x := range res {
			repos = append(repos, x.Repo)
		}
		return repos
	}
	r := e.ok("import", "--from-hf-cache", cache, "--dry-run", "--json", "--only", "ACME/T*", "--only", "bartowski/tiny-gguf")
	require.Equal(t, []string{"acme/two", "bartowski/Tiny-GGUF"}, imported(r), "patterns ignore case and * stops at /")

	r = e.run("import", "--from-hf-cache", cache, "--only", "acme/one", "--only", "acme/nope", "--offline")
	require.Equal(t, ExitNotFound, r.code)
	require.Contains(t, r.err.Error(), `--only "acme/nope"`)
	require.Zero(t, e.inst.RequestCount("POST /api/hangar/v1/repos"), "a pattern that matches nothing stops the import before it starts")

	r = e.ok("import", "--from-hf-cache", cache, "--only", "acme/one", "--offline", "--json")
	require.Equal(t, []string{"acme/one"}, imported(r))
	require.Equal(t, []byte("acme/one"), e.inst.Files(mustID(t, model.RepoTypeModel, "acme/one"), "main")["config.json"])
	require.Equal(t, 1, e.inst.RequestCount("POST /api/hangar/v1/repos"), "only the selected Repo is created")

	lm := filepath.Join(e.home, "lm")
	writeTree(t, lm, map[string][]byte{
		"someone/custom model/a.gguf": blobOf("a", 100),
		"someone/other/b.gguf":        blobOf("b", 100),
	})
	r = e.ok("import", "--from-lmstudio", lm, "--dry-run", "--json", "--only", "someone/custom model")
	require.Equal(t, []string{"someone/custom-model"}, imported(r))
	r = e.ok("import", "--from-lmstudio", lm, "--dry-run", "--json", "--only", "someone/custom-model")
	require.Equal(t, []string{"someone/custom-model"}, imported(r), "the Repo ID the folder becomes matches too")

	models := filepath.Join(e.home, "ollama")
	gguf := blobOf("ollama-gguf", 400)
	require.NoError(t, os.MkdirAll(filepath.Join(models, "blobs"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(models, "blobs", "sha256-"+sha(gguf)), gguf, 0o644))
	manifest := []byte(fmt.Sprintf(`{"layers":[{"mediaType":"application/vnd.ollama.image.model","digest":"sha256:%s","size":%d}]}`, sha(gguf), len(gguf)))
	writeTree(t, filepath.Join(models, "manifests"), map[string][]byte{
		"registry.ollama.ai/library/tiny/latest": manifest,
		"registry.ollama.ai/library/tiny/1b":     manifest,
		"registry.ollama.ai/someone/thing/q4":    manifest,
	})
	ollama := func(only ...string) []string {
		t.Helper()
		argv := []string{"import", "--from-ollama", "--ollama-models", models, "--namespace", "acme", "--dry-run", "--json"}
		for _, o := range only {
			argv = append(argv, "--only", o)
		}
		return imported(e.ok(argv...))
	}
	require.Equal(t, []string{"acme/tiny"}, ollama("tiny"), "a model without a tag means :latest, as in Ollama")
	require.ElementsMatch(t, []string{"acme/tiny", "acme/tiny-1b"}, ollama("tiny:*"))
	require.Equal(t, []string{"acme/someone-thing-q4"}, ollama("someone/thing:q4"))

	require.Equal(t, ExitUsage, e.run("import", "--from-dir", lm, "--repo", "acme/x", "--only", "a/b").code)
	require.Equal(t, ExitUsage, e.run("import", "--from-hf-cache", cache, "--only", "[").code)
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

// TestImportMatchesEverySnapshotByContent is a regression test for issue
// #29: the import kept one snapshot per Repo, took the alphabetically
// first without refs/main although the comment said the newest, and
// matched its files by path against that one Commit, so a file whose
// content the Mirror had under another Commit was reported as not
// matched. Every snapshot is now matched by content against every Commit
// the import checks, the newest snapshot is chosen, and each file that
// matches nothing is named with a reason.
func TestImportMatchesEverySnapshotByContent(t *testing.T) {
	e := newEnv(t)
	e.login()
	id := mustID(t, model.RepoTypeModel, "acme/omni-small")
	current, previous := blobOf("current", 100<<10), blobOf("previous", 100<<10)
	main := []clitest.File{{Path: "model.safetensors", Content: current}, {Path: "config.json", Content: []byte(`{"v":2}`)}}
	e.inst.AddUpstream(id, main...)
	newer, older := strings.Repeat("f", 40), strings.Repeat("1", 40)
	e.inst.AddUpstreamRevision(id, newer, main...)
	e.inst.AddUpstreamRevision(id, older, clitest.File{Path: "model.safetensors", Content: previous}, clitest.File{Path: "config.json", Content: []byte(`{"v":1}`)})
	cache := filepath.Join(e.home, "hf-cache")
	cacheSnapshot(t, cache, id, newer, map[string][]byte{"model.safetensors": current, "config.json": []byte(`{"v":2}`)})
	// The older snapshot links model.safetensors to the same blob as the
	// newer one, and holds a file the Mirror has nowhere.
	cacheSnapshot(t, cache, id, older, map[string][]byte{"model.safetensors": current, "notes.safetensors": blobOf("local notes", 3000)})
	repoDir := filepath.Join(cache, repoFolderName(id))
	require.NoError(t, os.Remove(filepath.Join(repoDir, "refs", "main")))
	past := time.Now().Add(-time.Hour)
	require.NoError(t, os.Chtimes(filepath.Join(repoDir, "snapshots", older), past, past))

	r := e.ok("import", "--from-hf-cache", "--json")
	var res []importResult
	require.NoError(t, json.Unmarshal([]byte(r.stdout), &res))
	require.Len(t, res, 1)
	got := res[0]
	require.Empty(t, got.Error)
	require.Equal(t, newer, got.Commit, "without refs/main, the newest snapshot is chosen")
	require.Equal(t, repoDir, got.Source)
	require.Equal(t, []string{"model.safetensors"}, got.Uploaded)
	require.True(t, e.inst.Present(id, newer, "model.safetensors"))
	require.Equal(t, []string{"notes.safetensors"}, got.Skipped, "model.safetensors of the older snapshot matches by content")
	require.Len(t, got.NotMatched, 1)
	require.Equal(t, "notes.safetensors", got.NotMatched[0].Path)
	require.Equal(t, older, got.NotMatched[0].Snapshot)
	require.Contains(t, got.NotMatched[0].Reason, "no file in the Mirror at Commits "+shortSHA(newer))
	require.True(t, strings.HasSuffix(got.NotMatched[0].Reason, " has this content"), got.NotMatched[0].Reason)

	// Without --json, the results name each file that matched nothing.
	r = e.ok("import", "--from-hf-cache")
	require.Contains(t, r.stdout, "1 not matched")
	require.Contains(t, r.stdout, "Not matched:\n  acme/omni-small: notes.safetensors (snapshot "+shortSHA(older)+"): no file in the Mirror at Commits ")
}

// TestImportMatchesFilesByContentNotPath is the LM Studio side of issue
// #29: a file whose content the Mirror references under another name was
// left out, and one whose path matched but content did not was counted as
// not matched without saying why.
func TestImportMatchesFilesByContentNotPath(t *testing.T) {
	e := newEnv(t)
	e.login()
	id, files := ggufRepo(e)
	lm := filepath.Join(e.home, ".lmstudio", "models")
	writeTree(t, lm, map[string][]byte{
		"bartowski/Tiny-GGUF/renamed-Q8_0.gguf": files["Tiny-Q8_0.gguf"],
		"bartowski/Tiny-GGUF/Tiny-Q4_K_M.gguf":  blobOf("not the upstream bytes", 1000),
		"bartowski/Tiny-GGUF/README.md":         files["README.md"],
	})
	r := e.ok("import", "--from-lmstudio", "--json")
	var res []importResult
	require.NoError(t, json.Unmarshal([]byte(r.stdout), &res))
	require.Len(t, res, 1)
	got := res[0]
	require.Empty(t, got.Error)
	require.Equal(t, []string{"renamed-Q8_0.gguf"}, got.Uploaded, "matched by content, under another name")
	require.True(t, e.inst.Present(id, "main", "Tiny-Q8_0.gguf"))
	require.Equal(t, []string{"Tiny-Q4_K_M.gguf"}, got.Skipped, "README.md matches the Mirror's by content")
	require.Equal(t, []notMatchedFile{{Path: "Tiny-Q4_K_M.gguf",
		Reason: "its content differs from Tiny-Q4_K_M.gguf at Commit " + shortSHA(got.Commit) + " and from every other file checked"}}, got.NotMatched)
}

// TestImportFillsAMirrorThatLacksTheCachedCommit is a regression test for
// issue #28 (Systran/faster-whisper-base): when the cached snapshot was
// newer than the Mirror's last sync, the tree at its Commit answered 404,
// the import fell back to a local Repo and the Commit failed with "repo is
// a mirror and is read-only". The import now pulls the Commit's history
// into the Mirror and fills its files.
func TestImportFillsAMirrorThatLacksTheCachedCommit(t *testing.T) {
	e := newEnv(t)
	e.login()
	id := mustID(t, model.RepoTypeModel, "acme/whisper")
	v1 := blobOf("v1", 100<<10)
	e.inst.AddUpstream(id, clitest.File{Path: "model.bin", Content: v1}, clitest.File{Path: "config.json", Content: []byte("{}")})
	cache := filepath.Join(e.home, "hf-cache")
	cacheSnapshot(t, cache, id, strings.Repeat("ab", 20), map[string][]byte{"model.bin": v1})
	e.ok("import", "--from-hf-cache")
	require.True(t, e.inst.Repo(id).Mirror)

	// The upstream Repo moves on, and the newer Commit is cached later.
	newer := strings.Repeat("cd", 20)
	v2 := blobOf("v2", 100<<10)
	e.inst.AddUpstreamRevision(id, newer, clitest.File{Path: "model.bin", Content: v2}, clitest.File{Path: "config.json", Content: []byte("{}")})
	cacheSnapshot(t, cache, id, newer, map[string][]byte{"model.bin": v2})

	r := e.ok("import", "--from-hf-cache", "--json")
	var res []importResult
	require.NoError(t, json.Unmarshal([]byte(r.stdout), &res))
	require.Len(t, res, 1)
	require.Equal(t, "mirror", res[0].Mode, "an existing Mirror never becomes a local Repo")
	require.Empty(t, res[0].Error)
	require.Equal(t, newer, res[0].Commit)
	require.Equal(t, []string{"model.bin"}, res[0].Uploaded)
	require.True(t, e.inst.Present(id, newer, "model.bin"))
	require.True(t, e.inst.Repo(id).Mirror)
}

// TestImportPullsTheMatchedCommit is a regression test for issue #28:
// after uploading the files of a snapshot older than upstream main, the
// import pulled them into the Mirror at its tracked branch, so files that
// had changed since were fetched again from the Upstream, and a file that
// had been removed since failed the Pull. The Pull now uses the Commit the
// files were matched against.
func TestImportPullsTheMatchedCommit(t *testing.T) {
	e := newEnv(t)
	e.login()
	id := mustID(t, model.RepoTypeModel, "acme/moved-on")
	old := strings.Repeat("ef", 20)
	v1, v2, extra := blobOf("old weights", 100<<10), blobOf("new weights", 100<<10), blobOf("extra", 100<<10)
	e.inst.AddUpstream(id, clitest.File{Path: "model.safetensors", Content: v2})
	e.inst.AddUpstreamRevision(id, old, clitest.File{Path: "model.safetensors", Content: v1}, clitest.File{Path: "extra.safetensors", Content: extra})
	cacheSnapshot(t, filepath.Join(e.home, "hf-cache"), id, old, map[string][]byte{"model.safetensors": v1, "extra.safetensors": extra})

	r := e.ok("import", "--from-hf-cache", "--json")
	var res []importResult
	require.NoError(t, json.Unmarshal([]byte(r.stdout), &res))
	require.Len(t, res, 1)
	require.Empty(t, res[0].Error, "a file removed upstream since does not fail the Pull")
	require.Equal(t, old, res[0].Commit)
	require.Equal(t, []string{"extra.safetensors", "model.safetensors"}, res[0].Uploaded)
	require.True(t, e.inst.Present(id, old, "model.safetensors"))
	require.True(t, e.inst.Present(id, old, "extra.safetensors"))
	require.False(t, e.inst.HasObject(sha(v2)), "the newer upstream file is not fetched")
}
