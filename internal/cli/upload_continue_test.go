package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/model"
)

// TestPushCarriesOnAfterAFileFails is the regression test for the second
// half of bellatrix finding 8: a file that still failed after its retries
// stopped the push at once, so the files after it were never sent and the
// next run had to upload them too. The push now sends every other file,
// names the one that failed and makes no Commit; the next run sends only
// the part that failed and commits everything.
func TestPushCarriesOnAfterAFileFails(t *testing.T) {
	e := newEnv(t)
	e.login()
	waits := recordWaits(t)
	dir := filepath.Join(e.home, "model")
	files := map[string][]byte{
		"a.safetensors": blobOf("first", 256<<10),
		"b.safetensors": blobOf("second", 256<<10),
		"c.safetensors": blobOf("third", 256<<10),
	}
	writeTree(t, dir, files)
	id := mustID(t, model.RepoTypeModel, "acme/carry-on")
	e.inst.AddRepo(id)
	head := e.inst.Head(id, "main")
	// Part 3 of the second file fails every time it is sent, after its
	// other parts are stored.
	e.inst.FailPartLast(sha(files["b.safetensors"]), 3, transferAttempts)

	r := e.run("push", dir, "acme/carry-on")
	require.Equal(t, ExitFailure, r.code)
	require.Contains(t, r.err.Error(), "b.safetensors: part 3")
	require.Contains(t, r.err.Error(), "run the command again to resume")
	require.NotContains(t, r.err.Error(), "a.safetensors")
	require.NotContains(t, r.err.Error(), "c.safetensors")
	require.Len(t, *waits, transferAttempts-1, "the failed part was retried before the push moved on")
	require.True(t, e.inst.HasObject(sha(files["a.safetensors"])))
	require.True(t, e.inst.HasObject(sha(files["c.safetensors"])), "the file after the failed one was uploaded")
	require.False(t, e.inst.HasObject(sha(files["b.safetensors"])))
	require.Equal(t, head, e.inst.Head(id, "main"), "no Commit is made without every file")
	require.Equal(t, 4+3+4, e.inst.PartPuts())

	puts := e.inst.PartPuts()
	e.ok("push", dir, "acme/carry-on")
	require.Equal(t, puts+1, e.inst.PartPuts(), "only the failed part is sent again")
	got := e.inst.Files(id, "main")
	for p, c := range files {
		require.Equal(t, c, got[p], p)
	}
}

// TestPushNamesEveryFileThatFailed checks that when several files fail,
// the error names each one and still exits with the code of their errors.
func TestPushNamesEveryFileThatFailed(t *testing.T) {
	e := newEnv(t)
	e.login()
	recordWaits(t)
	dir := filepath.Join(e.home, "model")
	files := map[string][]byte{
		"a.safetensors": blobOf("one", 128<<10),
		"b.safetensors": blobOf("two", 128<<10),
		"c.safetensors": blobOf("three", 128<<10),
	}
	writeTree(t, dir, files)
	id := mustID(t, model.RepoTypeModel, "acme/several")
	e.inst.AddRepo(id)
	head := e.inst.Head(id, "main")
	e.inst.FailPartLast(sha(files["a.safetensors"]), 1, transferAttempts)
	e.inst.FailPartLast(sha(files["c.safetensors"]), 2, transferAttempts)

	r := e.run("push", dir, "acme/several")
	require.Equal(t, ExitFailure, r.code)
	msg := r.err.Error()
	require.True(t, strings.HasPrefix(msg, "2 files failed to upload: a.safetensors: part 1"), msg)
	require.Contains(t, msg, "; c.safetensors: part 2")
	require.True(t, e.inst.HasObject(sha(files["b.safetensors"])))
	require.Equal(t, head, e.inst.Head(id, "main"))
}

// TestImportCarriesOnAfterAFileFails covers bellatrix finding 8 for
// hangar import: a file of a Mirror that fails to upload no longer stops
// the other files of the same Repo. They are uploaded and added to the
// Mirror, the result names the failed file, the import goes on to the
// next Repo, and running it again sends only the failed file.
func TestImportCarriesOnAfterAFileFails(t *testing.T) {
	e := newEnv(t)
	e.login()
	recordWaits(t)
	// The fake Upstream Repo has no .gitattributes, so make its GGUF files
	// big enough to go up as LFS objects, as they do on a real Instance.
	e.inst.InlineMax = 1 << 10
	id, files := ggufRepo(e)
	cache := filepath.Join(e.home, "hf-cache")
	commit := strings.Repeat("ab", 20)
	cacheSnapshot(t, cache, id, commit, map[string][]byte{
		"Tiny-Q4_K_M.gguf":     files["Tiny-Q4_K_M.gguf"],
		"Tiny-Q8_0.gguf":       files["Tiny-Q8_0.gguf"],
		"mmproj-Tiny-f16.gguf": files["mmproj-Tiny-f16.gguf"],
	})
	local := mustID(t, model.RepoTypeModel, "acme/private-ft")
	cacheSnapshot(t, cache, local, strings.Repeat("cd", 20), map[string][]byte{"model.safetensors": blobOf("ft", 3000)})
	// Tiny-Q4_K_M.gguf sorts first, so the files after it show whether the
	// import carried on.
	e.inst.FailPartLast(sha(files["Tiny-Q4_K_M.gguf"]), 2, transferAttempts)

	r := e.run("import", "--from-hf-cache", "--json")
	require.Equal(t, ExitFailure, r.code, r.stdout+r.stderr)
	require.Contains(t, r.err.Error(), "1 of 2 imports failed")
	var res []importResult
	require.NoError(t, json.Unmarshal([]byte(r.stdout), &res))
	byRepo := map[string]importResult{}
	for _, x := range res {
		byRepo[x.Repo] = x
	}
	mirror := byRepo["bartowski/Tiny-GGUF"]
	require.Equal(t, "mirror", mirror.Mode, "a failed file's error never turns a Mirror into a local Repo")
	require.Equal(t, []string{"Tiny-Q8_0.gguf", "mmproj-Tiny-f16.gguf"}, mirror.Uploaded)
	require.Contains(t, mirror.Error, "Tiny-Q4_K_M.gguf: part 2")
	require.NotEmpty(t, mirror.Commit)
	require.True(t, e.inst.Present(id, commit, "Tiny-Q8_0.gguf"), "the files that went up are added to the Mirror")
	require.True(t, e.inst.Present(id, commit, "mmproj-Tiny-f16.gguf"))
	require.False(t, e.inst.Present(id, commit, "Tiny-Q4_K_M.gguf"))
	require.Empty(t, byRepo["acme/private-ft"].Error, "the next Repo is imported")

	// Importing acme/private-ft again would find it is a local Repo now,
	// so take it out of the cache before the second run.
	require.NoError(t, os.RemoveAll(filepath.Join(cache, repoFolderName(local))))
	puts := e.inst.PartPuts()
	r = e.ok("import", "--from-hf-cache", "--json")
	res = nil
	require.NoError(t, json.Unmarshal([]byte(r.stdout), &res))
	for _, x := range res {
		if x.Repo == "bartowski/Tiny-GGUF" {
			require.Empty(t, x.Error)
			require.Equal(t, []string{"Tiny-Q4_K_M.gguf"}, x.Uploaded, "only the failed file is sent again")
		}
	}
	require.Equal(t, puts+1, e.inst.PartPuts(), "only its failed part")
	require.True(t, e.inst.Present(id, commit, "Tiny-Q4_K_M.gguf"))
}
