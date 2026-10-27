package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/cli/clitest"
	"github.com/hangar-hub/hangar/internal/model"
)

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// ggufRepo adds an upstream GGUF Repo with two quants and a projector.
func ggufRepo(e *env) (model.RepoID, map[string][]byte) {
	id := mustID(e.t, model.RepoTypeModel, "bartowski/Tiny-GGUF")
	files := map[string][]byte{
		"README.md":              []byte("# Tiny\n"),
		"config.json":            []byte(`{"architectures":["Llama"]}`),
		"Tiny-Q4_K_M.gguf":       blobOf("q4km", 200<<10),
		"Tiny-Q8_0.gguf":         blobOf("q8", 300<<10),
		"mmproj-Tiny-f16.gguf":   blobOf("proj", 50<<10),
		"nested/notes/extra.txt": []byte("extra"),
	}
	var fs []clitest.File
	for p, c := range files {
		fs = append(fs, clitest.File{Path: p, Content: c})
	}
	e.inst.AddUpstream(id, fs...)
	return id, files
}

func TestPullFromUpstreamIntoTheHFCache(t *testing.T) {
	e := newEnv(t)
	e.login()
	id, files := ggufRepo(e)

	r := e.ok("pull", "bartowski/Tiny-GGUF:q4_k_m")
	require.Contains(t, r.stderr, "not in the Arsenal")
	require.Contains(t, r.stderr, "Pull: succeeded")
	require.Contains(t, r.stdout, "Downloaded 5 files")
	require.Equal(t, 1, e.inst.RequestCount("POST /api/hangar/v1/pulls"))

	commit := e.inst.Head(id, "main")
	cache := filepath.Join(e.home, "hf-cache", "models--bartowski--Tiny-GGUF")
	ref, err := os.ReadFile(filepath.Join(cache, "refs", "main"))
	require.NoError(t, err)
	require.Equal(t, commit, string(ref))
	for _, p := range []string{"Tiny-Q4_K_M.gguf", "mmproj-Tiny-f16.gguf", "README.md", "config.json", "nested/notes/extra.txt"} {
		link := filepath.Join(cache, "snapshots", commit, filepath.FromSlash(p))
		got, err := os.ReadFile(link)
		require.NoError(t, err, p)
		require.Equal(t, files[p], got, p)
		target, err := os.Readlink(link)
		require.NoError(t, err)
		require.False(t, filepath.IsAbs(target), "snapshot links are relative, as huggingface_hub makes them")
	}
	_, err = os.Stat(filepath.Join(cache, "snapshots", commit, "Tiny-Q8_0.gguf"))
	require.True(t, os.IsNotExist(err), "other quants are not downloaded")
	// LFS blobs are named by SHA-256, regular files by git blob ID.
	_, err = os.Stat(filepath.Join(cache, "blobs", sha(files["Tiny-Q4_K_M.gguf"])))
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(cache, "blobs", model.GitBlobID(files["README.md"])))
	require.NoError(t, err)
	require.False(t, e.inst.Present(id, "main", "Tiny-Q8_0.gguf"), "the Pull selected only the quant")

	// A second pull of the same files downloads nothing.
	before := e.inst.RequestCount("GET /{ns}/{name}/resolve/{rev}/*")
	r = e.ok("pull", "bartowski/Tiny-GGUF:Q4_K_M", "--json")
	require.Equal(t, before, e.inst.RequestCount("GET /{ns}/{name}/resolve/{rev}/*"))
	var res pullResult
	require.NoError(t, json.Unmarshal([]byte(r.stdout), &res))
	require.Len(t, res.Files, 5)
	for _, f := range res.Files {
		require.True(t, f.Cached, f.Path)
	}

	// Another quant is missing from the Partial mirror, so it is pulled into it.
	r = e.ok("pull", "bartowski/Tiny-GGUF:Q8_0")
	require.Contains(t, r.stderr, "not in the Arsenal. Pulling them.")
	require.Equal(t, 1, e.inst.RequestCount("POST /api/hangar/v1/repos/{type}/{ns}/{name}/mirror/pull"))
	require.True(t, e.inst.Present(id, "main", "Tiny-Q8_0.gguf"))
}

// A GGUF file under hfapi.regular_upload_max is a regular git blob, because
// the default .gitattributes has no *.gguf rule, so a quant cannot be chosen
// by LFS status alone.
func TestPullQuantOfRegularBlobGGUFs(t *testing.T) {
	e := newEnv(t)
	e.login()
	files := map[string][]byte{
		"README.md":            []byte("# Tiny\n"),
		"tiny-Q4_K_M.gguf":     blobOf("q4", 30<<10),
		"tiny-Q8_0.gguf":       blobOf("q8", 40<<10),
		"mmproj-tiny-f16.gguf": blobOf("pj", 10<<10),
		"model.safetensors":    blobOf("st", 20<<10),
	}
	src := filepath.Join(e.home, "src")
	writeTree(t, src, files)
	e.ok("push", src, "acme/tiny", "--create")
	id := mustID(t, model.RepoTypeModel, "acme/tiny")
	for _, p := range []string{"tiny-Q4_K_M.gguf", "tiny-Q8_0.gguf", "mmproj-tiny-f16.gguf"} {
		require.False(t, e.inst.IsLFS(id, "main", p), "%s is a regular blob", p)
	}
	require.True(t, e.inst.IsLFS(id, "main", "model.safetensors"))

	want := []string{".gitattributes", "README.md", "mmproj-tiny-f16.gguf", "tiny-Q4_K_M.gguf"}
	r := e.ok("pull", "acme/tiny:Q4_K_M", "--json")
	var res pullResult
	require.NoError(t, json.Unmarshal([]byte(r.stdout), &res))
	var got []string
	for _, f := range res.Files {
		got = append(got, f.Path)
	}
	require.ElementsMatch(t, want, got, "only the chosen quant, its projector and the small files")

	lm := filepath.Join(e.home, "lmstudio")
	r = e.ok("lmstudio", "install", "acme/tiny:Q8_0", "--lmstudio-dir", lm)
	require.Contains(t, r.stdout, "(4 files)")
	dir := filepath.Join(lm, "acme", "tiny")
	for _, p := range []string{"tiny-Q8_0.gguf", "mmproj-tiny-f16.gguf", "README.md"} {
		b, err := os.ReadFile(filepath.Join(dir, p))
		require.NoError(t, err, p)
		require.Equal(t, files[p], b, p)
	}
	for _, p := range []string{"tiny-Q4_K_M.gguf", "model.safetensors"} {
		_, err := os.Stat(filepath.Join(dir, p))
		require.True(t, os.IsNotExist(err), "%s is not installed", p)
	}
}

func TestPullToAFolderAndResume(t *testing.T) {
	e := newEnv(t)
	e.login()
	id := mustID(t, model.RepoTypeModel, "acme/local")
	big := blobOf("weights", 300<<10)
	e.inst.AddRepo(id, clitest.File{Path: "model.safetensors", Content: big}, clitest.File{Path: "config.json", Content: []byte("{}")})
	dir := filepath.Join(e.home, "out")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	// A partial download from an earlier run.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "model.safetensors.incomplete"), big[:100<<10], 0o644))

	r := e.ok("pull", "acme/local", "--to", dir, "--include", "*.safetensors")
	require.Contains(t, r.stdout, "Downloaded 1 files")
	got, err := os.ReadFile(filepath.Join(dir, "model.safetensors"))
	require.NoError(t, err)
	require.Equal(t, big, got)
	_, err = os.Stat(filepath.Join(dir, "config.json"))
	require.True(t, os.IsNotExist(err))
	_, err = os.Stat(filepath.Join(dir, "model.safetensors.incomplete"))
	require.True(t, os.IsNotExist(err))
	require.Zero(t, e.inst.RequestCount("POST /api/hangar/v1/pulls"), "a local Repo needs no Pull")

	// Already there: verified by hash and skipped.
	r = e.ok("pull", "acme/local", "--to", dir, "--json")
	var res pullResult
	require.NoError(t, json.Unmarshal([]byte(r.stdout), &res))
	for _, f := range res.Files {
		if f.Path == "model.safetensors" {
			require.True(t, f.Cached)
		}
	}
}

func TestPullServerOnlyAndErrors(t *testing.T) {
	e := newEnv(t)
	e.login()
	id, _ := ggufRepo(e)
	r := e.ok("pull", id.String(), "--server-only")
	require.Contains(t, r.stdout, "is in the Arsenal")
	_, err := os.Stat(filepath.Join(e.home, "hf-cache", "models--bartowski--Tiny-GGUF"))
	require.True(t, os.IsNotExist(err))

	r = e.run("pull", id.String()+":Q2_K")
	require.Equal(t, ExitNotFound, r.code)
	require.Contains(t, r.err.Error(), "Q4_K_M")

	r = e.run("pull", "nobody/nothing")
	require.Equal(t, ExitNotFound, r.code)

	r = e.run("pull", "acme/x", "--config", "default")
	require.Equal(t, ExitUsage, r.code)
	r = e.run("pull", "datasets/acme/x:Q4_K_M")
	require.Equal(t, ExitUsage, r.code)
	r = e.run("pull", "not-a-repo-id")
	require.Equal(t, ExitUsage, r.code)

	other := mustID(t, model.RepoTypeModel, "acme/failing")
	e.inst.AddUpstream(other, clitest.File{Path: "a.bin", Content: []byte("x")})
	e.inst.FailPulls = "upstream answered 500"
	r = e.run("pull", other.String())
	require.Equal(t, ExitFailure, r.code)
	require.Contains(t, r.err.Error(), "upstream answered 500")
}

func TestPullDatasetParquetExport(t *testing.T) {
	e := newEnv(t)
	e.login()
	id := mustID(t, model.RepoTypeDataset, "lhoestq/demo1")
	e.inst.AddUpstream(id, clitest.File{Path: "data/train.csv", Content: []byte("a,b\n1,2\n")}, clitest.File{Path: "README.md", Content: []byte("# demo")})
	e.inst.AddUpstreamRevision(id, parquetRevision,
		clitest.File{Path: "default/train/0000.parquet", Content: blobOf("train", 2000)},
		clitest.File{Path: "default/test/0000.parquet", Content: blobOf("test", 2000)})
	dir := filepath.Join(e.home, "data")
	r := e.ok("pull", "datasets/lhoestq/demo1", "--config", "default", "--split", "train", "--parquet-only", "--to", dir)
	require.Contains(t, r.stdout, "Downloaded 1 files")
	got, err := os.ReadFile(filepath.Join(dir, "default", "train", "0000.parquet"))
	require.NoError(t, err)
	require.Equal(t, blobOf("train", 2000), got)
	require.False(t, e.inst.Present(id, parquetRevision, "default/test/0000.parquet"))

	// The dataset now exists; info shows it with its Configs.
	r = e.ok("info", "datasets/lhoestq/demo1")
	require.Contains(t, r.stdout, "datasets/lhoestq/demo1")
	require.Contains(t, r.stdout, "Mirror of lhoestq/demo1 on huggingface")
}

func TestInfoAndSearch(t *testing.T) {
	e := newEnv(t)
	e.login()
	id, files := ggufRepo(e)

	// Not in the Arsenal yet: info shows the Upstream's view.
	r := e.ok("info", id.String())
	require.Contains(t, r.stdout, "on huggingface")
	require.Contains(t, r.stdout, "Q4_K_M")
	require.Contains(t, r.stdout, "hangar pull bartowski/Tiny-GGUF")
	r = e.ok("info", id.String(), "--json")
	require.Contains(t, r.stdout, `"upstream": "huggingface"`)
	r = e.run("info", id.String(), "--local")
	require.Equal(t, ExitNotFound, r.code)
	r = e.run("info", "nobody/nothing")
	require.Equal(t, ExitNotFound, r.code)

	r = e.ok("search", "tiny", "--upstream")
	require.Contains(t, r.stdout, "bartowski/Tiny-GGUF")
	require.Contains(t, r.stdout, "42")

	e.ok("pull", id.String()+":Q4_K_M", "--server-only")
	// Size counts each stored file once, LFS objects included.
	var stored int64
	for p, c := range files {
		if e.inst.Present(id, "main", p) {
			stored += int64(len(c))
		}
	}
	require.Greater(t, stored, int64(200<<10))
	r = e.ok("info", id.String())
	require.Regexp(t, `Size:\s+`+regexp.QuoteMeta(formatBytes(stored))+`\n`, r.stdout)
	require.Contains(t, r.stdout, "Partly in arsenal")
	require.Contains(t, r.stdout, "Not in arsenal")
	require.Contains(t, r.stdout, "Q8_0")
	r = e.ok("info", id.String(), "--json")
	var info repoInfo
	require.NoError(t, json.Unmarshal([]byte(r.stdout), &info))
	require.Equal(t, "partial", string(info.Arsenal))
	require.Len(t, info.Quants, 2)

	r = e.ok("search", "--upstream=huggingface", "tiny", "--json")
	require.Contains(t, r.stdout, `"arsenal": "partial"`)
	r = e.ok("search", "tiny")
	require.Contains(t, r.stdout, "bartowski/Tiny-GGUF")
	require.Contains(t, r.stdout, "mirror")
	r = e.ok("search", "--filter", "quant=Q4_K_M", "--filter", "format=gguf", "--filter", "params_min=1", "--filter", "mirror=true", "--json")
	require.Contains(t, r.stdout, "Tiny-GGUF")
	r = e.ok("search", "nothing-matches")
	require.Contains(t, r.stdout, "No Repos match")
	for _, bad := range [][]string{
		{"search", "--filter", "nonsense"},
		{"search", "--filter", "colour=red"},
		{"search", "--filter", "params_min=lots"},
		{"search", "--filter", "mirror=perhaps"},
		{"search", "--type", "space"},
		{"search", "--limit", "0"},
		{"search", "--upstream", "--limit", "900"},
	} {
		require.Equal(t, ExitUsage, e.run(bad...).code, bad)
	}
}

func TestGlobMatcher(t *testing.T) {
	m, err := newGlobMatcher([]string{"*.gguf", "docs/", "a?c", "[!x]y"})
	require.NoError(t, err)
	for p, want := range map[string]bool{
		"x.gguf": true, "dir/x.gguf": true, "docs/a/b.md": true, "abc": true, "zy": true,
		"xy": false, "x.bin": false, "docsx": false,
	} {
		require.Equal(t, want, m.match(p), p)
	}
	require.True(t, (globMatcher{}).empty())
}

func TestHFCacheFolderNames(t *testing.T) {
	id := mustID(t, model.RepoTypeDataset, "ab/cd")
	require.Equal(t, "datasets--ab--cd", repoFolderName(id))
	got, ok := parseRepoFolderName("datasets--ab--cd")
	require.True(t, ok)
	require.Equal(t, id, got)
	for _, bad := range []string{"spaces--ab--cd", "models--ab", "models--ab--cd--ef", "models--ab--c d"} {
		_, ok := parseRepoFolderName(bad)
		require.False(t, ok, bad)
	}
	t.Setenv("HF_HUB_CACHE", "")
	t.Setenv("HF_HOME", "/hf")
	require.Equal(t, filepath.Join("/hf", "hub"), defaultHFCacheDir())
	t.Setenv("HF_HOME", "")
	t.Setenv("XDG_CACHE_HOME", "/xdg")
	require.Equal(t, filepath.Join("/xdg", "huggingface", "hub"), defaultHFCacheDir())
}
