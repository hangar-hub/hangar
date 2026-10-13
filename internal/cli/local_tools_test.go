package cli

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"

	"github.com/jamesagarside/hangar/internal/cli/clitest"
	"github.com/jamesagarside/hangar/internal/model"
)

func TestLMStudioInstallListRemove(t *testing.T) {
	e := newEnv(t)
	e.login()
	id, files := ggufRepo(e)
	lm := filepath.Join(e.home, "lm-models")

	r := e.ok("lmstudio", "install", id.String(), "--lmstudio-dir", lm)
	require.Contains(t, r.stderr, "No quant given; installing Q4_K_M")
	dir := filepath.Join(lm, "bartowski", "Tiny-GGUF")
	for _, p := range []string{"Tiny-Q4_K_M.gguf", "mmproj-Tiny-f16.gguf", "README.md"} {
		full := filepath.Join(dir, p)
		target, err := os.Readlink(full)
		require.NoError(t, err, p)
		require.True(t, filepath.IsAbs(target))
		require.Contains(t, target, filepath.Join("hf-cache", "models--bartowski--Tiny-GGUF", "blobs"))
		got, err := os.ReadFile(full)
		require.NoError(t, err)
		require.Equal(t, files[p], got)
	}
	_, err := os.Stat(filepath.Join(dir, "Tiny-Q8_0.gguf"))
	require.True(t, os.IsNotExist(err))

	r = e.ok("lmstudio", "list")
	require.Contains(t, r.stdout, "bartowski/Tiny-GGUF")
	require.Contains(t, r.stdout, "up to date")

	// A new upstream Commit makes an update available.
	head := e.inst.Head(id, "main")
	other := mustID(t, model.RepoTypeModel, "acme/second")
	e.inst.AddRepo(other, clitest.File{Path: "Second-Q8_0.gguf", Content: blobOf("s", 2000)})
	e.ok("lmstudio", "install", "acme/second:Q8_0", "--lmstudio-dir", lm, "--link-mode", "copy")
	st, err := os.Lstat(filepath.Join(lm, "acme", "second", "Second-Q8_0.gguf"))
	require.NoError(t, err)
	require.True(t, st.Mode().IsRegular(), "copy mode writes a real file")
	dir2 := filepath.Join(e.home, "push2")
	writeTree(t, dir2, map[string][]byte{"Second-Q8_0.gguf": blobOf("t", 2000)})
	e.ok("push", dir2, "acme/second")
	r = e.ok("lmstudio", "list", "--json")
	var list []lmStatus
	require.NoError(t, json.Unmarshal([]byte(r.stdout), &list))
	require.Len(t, list, 2)
	for _, s := range list {
		if s.Repo == "acme/second" {
			require.True(t, s.UpdateAvailable)
		} else {
			require.Equal(t, head, s.Commit)
			require.False(t, s.UpdateAvailable)
		}
	}
	r = e.ok("lmstudio", "list")
	require.Contains(t, r.stdout, "newer Revision")

	r = e.ok("lmstudio", "remove", id.String(), "--prune")
	require.Contains(t, r.stdout, "Freed")
	_, err = os.Stat(dir)
	require.True(t, os.IsNotExist(err), "the model folder is removed when empty")
	blobs, err := os.ReadDir(filepath.Join(e.home, "hf-cache", "models--bartowski--Tiny-GGUF", "blobs"))
	require.NoError(t, err)
	require.Empty(t, blobs, "prune deletes blobs no snapshot uses")
	require.Equal(t, ExitNotFound, e.run("lmstudio", "remove", id.String()).code)

	e.ok("lmstudio", "remove", "acme/second")
	r = e.ok("lmstudio", "list")
	require.Contains(t, r.stdout, "Nothing is installed")
	require.Equal(t, ExitUsage, e.run("lmstudio", "install", id.String(), "--link-mode", "teleport", "--lmstudio-dir", lm).code)
}

func TestLMStudioHardlinkAndFolderDiscovery(t *testing.T) {
	e := newEnv(t)
	e.login()
	id := mustID(t, model.RepoTypeModel, "mlx-community/Tiny-4bit")
	e.inst.AddRepo(id, clitest.File{Path: "model.safetensors", Content: blobOf("mlx", 3000)}, clitest.File{Path: "config.json", Content: []byte("{}")})

	// LM Studio's home comes from the pointer file, and the models folder
	// from downloadsFolder in its settings.json.
	home := filepath.Join(e.home, "lmhome")
	models := filepath.Join(e.home, "elsewhere")
	require.NoError(t, os.MkdirAll(home, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(e.home, ".lmstudio-home-pointer"), []byte(home+"\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(home, "settings.json"), []byte(`{"downloadsFolder":"`+models+`"}`), 0o644))
	require.Equal(t, models, lmStudioModelsDir(""))

	e.ok("lmstudio", "install", id.String(), "--link-mode", "hardlink")
	full := filepath.Join(models, "mlx-community", "Tiny-4bit", "model.safetensors")
	st, err := os.Lstat(full)
	require.NoError(t, err)
	require.True(t, st.Mode().IsRegular())
	got, _ := os.ReadFile(full)
	require.Equal(t, blobOf("mlx", 3000), got)

	require.NoError(t, os.Remove(filepath.Join(e.home, ".lmstudio-home-pointer")))
	require.Equal(t, filepath.Join(e.home, ".lmstudio", "models"), lmStudioModelsDir(""))
	require.NoError(t, os.MkdirAll(filepath.Join(e.home, ".cache", "lm-studio"), 0o755))
	require.Equal(t, filepath.Join(e.home, ".cache", "lm-studio", "models"), lmStudioModelsDir(""))
	require.Equal(t, "/x", lmStudioModelsDir("/x"))
}

// writeOllamaKey writes an ed25519 public key where Ollama keeps it and
// returns its fingerprint.
func writeOllamaKey(t *testing.T, home string) string {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	sp, err := ssh.NewPublicKey(pub)
	require.NoError(t, err)
	dir := filepath.Join(home, ".ollama")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "id_ed25519.pub"), ssh.MarshalAuthorizedKey(sp), 0o644))
	return ssh.FingerprintSHA256(sp)
}

func TestOllamaKeyRegisterAndInstall(t *testing.T) {
	e := newEnv(t)
	e.login()
	var ran [][]string
	orig := runOllama
	defer func() { runOllama = orig }()
	runOllama = func(_ context.Context, argv []string, _, _ io.Writer) error {
		ran = append(ran, argv)
		return nil
	}

	r := e.run("ollama", "key", "register")
	require.Equal(t, ExitNotFound, r.code, "no key yet")

	fp := writeOllamaKey(t, e.home)
	// Install asks first; "n" leaves the key unregistered but still pulls.
	r = e.runIn("n\n", "ollama", "install", "bartowski/Tiny-GGUF:Q4_K_M")
	require.NoError(t, r.err, r.stderr)
	require.Contains(t, r.stderr, "Register it now?")
	require.Empty(t, e.inst.OllamaKeys())
	host := strings.TrimPrefix(e.inst.URL, "http://")
	require.Equal(t, []string{"pull", "--insecure", host + "/bartowski/Tiny-GGUF:Q4_K_M"}, ran[0])
	require.Contains(t, r.stdout, "ollama run "+host+"/bartowski/Tiny-GGUF:Q4_K_M")

	r = e.runIn("y\n", "ollama", "install", "bartowski/Tiny-GGUF")
	require.NoError(t, r.err, r.stderr)
	require.Len(t, e.inst.OllamaKeys(), 1)
	require.Equal(t, fp, e.inst.OllamaKeys()[0].Fingerprint)
	require.Equal(t, host+"/bartowski/Tiny-GGUF", ran[1][2])

	r = e.ok("ollama", "key", "register")
	require.Contains(t, r.stdout, "already registered")
	r = e.ok("ollama", "key", "register", "--json")
	require.Contains(t, r.stdout, `"already_registered": true`)

	require.Equal(t, ExitUsage, e.run("ollama", "install", "datasets/acme/data").code)
	require.Equal(t, ExitUsage, e.run("ollama", "install", "acme/x", "--url", e.inst.URL+"/sub").code)
}

func TestOllamaInstallWithYesRegistersTheKey(t *testing.T) {
	e := newEnv(t)
	e.login()
	writeOllamaKey(t, e.home)
	orig := runOllama
	defer func() { runOllama = orig }()
	runOllama = func(context.Context, []string, io.Writer, io.Writer) error { return nil }
	r := e.ok("ollama", "install", "acme/model:Q4_0", "--yes", "--json")
	require.Contains(t, r.stdout, `"model"`)
	require.Len(t, e.inst.OllamaKeys(), 1)
}
