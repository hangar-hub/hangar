//go:build compat

// Package lmstudio checks that `hangar lmstudio install` lays models out the
// way LM Studio reads them (docs/spec/08-local-tools.md, 13-testing.md). It
// drives the real hangar binary. LM Studio itself is not automated; a manual
// check with LM Studio is a release checklist item (README.md).
//
// The Instance is the one named by HANGAR_LMSTUDIO_URL and
// HANGAR_LMSTUDIO_TOKEN (a write Token) when set, otherwise the fake
// Instance in internal/cli/clitest.
package lmstudio

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/compat/harness"
	"github.com/hangar-hub/hangar/internal/cli/clitest"
)

type env struct {
	t      *testing.T
	bin    string
	home   string
	lmDir  string
	cache  string
	url    string
	token  string
	prefix string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{t: t, bin: harness.Binary(t), home: t.TempDir()}
	e.lmDir = filepath.Join(e.home, "lmstudio", "models")
	e.cache = filepath.Join(e.home, "hf-cache")
	e.url, e.token = os.Getenv("HANGAR_LMSTUDIO_URL"), os.Getenv("HANGAR_LMSTUDIO_TOKEN")
	e.prefix = os.Getenv("HANGAR_LMSTUDIO_NAMESPACE")
	if e.url == "" {
		inst := clitest.New(t)
		e.url, e.token = inst.URL, clitest.Token
	}
	if e.prefix == "" {
		e.prefix = "acme"
	}
	return e
}

// hangar runs the binary with an isolated home, cache and configuration.
func (e *env) hangar(args ...string) string {
	e.t.Helper()
	cmd := exec.Command(e.bin, args...)
	cmd.Env = append(os.Environ(),
		"HOME="+e.home, "XDG_CONFIG_HOME="+filepath.Join(e.home, ".config"),
		"HF_HUB_CACHE="+e.cache, "HANGAR_CLI_URL="+e.url, "HANGAR_CLI_TOKEN="+e.token, "HANGAR_CLI_NO_KEYCHAIN=1")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	require.NoError(e.t, err, "hangar %s\nstdout: %s\nstderr: %s", strings.Join(args, " "), out.String(), errb.String())
	return out.String()
}

// content returns repeatable bytes that look like a GGUF file.
func content(seed string, n int) []byte {
	b := append([]byte("GGUF"), bytes.Repeat([]byte(seed), n/len(seed)+1)...)
	return b[:n]
}

func writeRepo(t *testing.T, dir string, files map[string][]byte) {
	t.Helper()
	for p, c := range files {
		require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(dir, p)), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, p), c, 0o644))
	}
}

func TestInstallLayoutLinksListAndRemove(t *testing.T) {
	e := newEnv(t)
	repo := fmt.Sprintf("%s/lmstudio-compat-%d", e.prefix, time.Now().UnixNano())
	ns, name, _ := strings.Cut(repo, "/")
	files := map[string][]byte{
		"tiny-Q4_K_M.gguf":     content("q4", 300<<10),
		"tiny-Q8_0.gguf":       content("q8", 400<<10),
		"mmproj-tiny-f16.gguf": content("pj", 100<<10),
		"README.md":            []byte("# tiny\n"),
	}
	src := filepath.Join(e.home, "src")
	writeRepo(t, src, files)
	e.hangar("push", src, repo, "--create")

	// Symlinks (the default on macOS and Linux) into the cache's blobs.
	e.hangar("lmstudio", "install", repo+":Q4_K_M", "--lmstudio-dir", e.lmDir)
	dir := filepath.Join(e.lmDir, ns, name)
	realCache, err := filepath.EvalSymlinks(e.cache)
	require.NoError(t, err)
	for _, p := range []string{"tiny-Q4_K_M.gguf", "mmproj-tiny-f16.gguf", "README.md"} {
		full := filepath.Join(dir, p)
		target, err := os.Readlink(full)
		require.NoError(t, err, "%s is a symlink", p)
		require.True(t, strings.HasPrefix(target, filepath.Join(realCache, "models--"+ns+"--"+name, "blobs")), target)
		got, err := os.ReadFile(full)
		require.NoError(t, err, "the link resolves")
		require.Equal(t, files[p], got)
	}
	_, err = os.Stat(filepath.Join(dir, "tiny-Q8_0.gguf"))
	require.True(t, os.IsNotExist(err), "only the chosen quant is installed")

	// The cache is the standard Hugging Face layout, shared with Python tools.
	ref, err := os.ReadFile(filepath.Join(e.cache, "models--"+ns+"--"+name, "refs", "main"))
	require.NoError(t, err)
	snap := filepath.Join(e.cache, "models--"+ns+"--"+name, "snapshots", string(ref), "tiny-Q4_K_M.gguf")
	got, err := os.ReadFile(snap)
	require.NoError(t, err)
	require.Equal(t, files["tiny-Q4_K_M.gguf"], got)

	var list []map[string]any
	require.NoError(t, json.Unmarshal([]byte(e.hangar("lmstudio", "list", "--json")), &list))
	require.Len(t, list, 1)
	require.Equal(t, repo, list[0]["repo"])
	require.Equal(t, false, list[0]["update_available"])

	// A new Commit shows as a newer Revision.
	writeRepo(t, src, map[string][]byte{"README.md": []byte("# tiny, updated\n")})
	e.hangar("push", src, repo)
	require.NoError(t, json.Unmarshal([]byte(e.hangar("lmstudio", "list", "--json")), &list))
	require.Equal(t, true, list[0]["update_available"])

	// Hard links and copies give real files.
	for _, mode := range []string{"hardlink", "copy"} {
		other := filepath.Join(e.home, "lm-"+mode)
		e.hangar("lmstudio", "install", repo+":Q8_0", "--lmstudio-dir", other, "--link-mode", mode)
		st, err := os.Lstat(filepath.Join(other, ns, name, "tiny-Q8_0.gguf"))
		require.NoError(t, err)
		require.True(t, st.Mode().IsRegular(), mode)
	}

	out := e.hangar("lmstudio", "remove", repo, "--prune")
	require.Contains(t, out, "Removed")
	_, err = os.Stat(dir)
	require.True(t, os.IsNotExist(err))
	require.Contains(t, e.hangar("lmstudio", "list"), "Nothing is installed")
}
