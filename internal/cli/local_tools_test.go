package cli

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"

	"github.com/hangar-hub/hangar/internal/cli/clitest"
	"github.com/hangar-hub/hangar/internal/model"
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

// writeOllamaKey writes an ed25519 key pair where Ollama keeps it, as
// Ollama does, and returns its fingerprint.
func writeOllamaKey(t *testing.T, home string) string {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	sp, err := ssh.NewPublicKey(pub)
	require.NoError(t, err)
	block, err := ssh.MarshalPrivateKey(priv, "")
	require.NoError(t, err)
	dir := filepath.Join(home, ".ollama")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "id_ed25519"), pem.EncodeToMemory(block), 0o600))
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
	// The quants are in the Arsenal, so install goes straight to the key.
	_, files := ggufRepo(e)
	var local []clitest.File
	for p, c := range files {
		local = append(local, clitest.File{Path: p, Content: c})
	}
	e.inst.AddRepo(mustID(t, model.RepoTypeModel, "bartowski/Tiny-GGUF"), local...)

	fp := writeOllamaKey(t, e.home)
	// Install asks first; "n" leaves the key unregistered but still pulls.
	r = e.runIn("n\n", "ollama", "install", "bartowski/Tiny-GGUF:Q4_K_M")
	require.NoError(t, r.err, r.stderr)
	require.Contains(t, r.stderr, "Register it now?")
	// Anonymous reads are off by default, so without the key only public
	// Repos on an Instance that allows them work (docs/guides/ollama.md).
	require.Contains(t, r.stderr, "Only public Repos on an Instance that allows anonymous reads will work.")
	require.NotContains(t, r.stderr, "Public Repos still work")
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

// TestOllamaKeyRegisterProvesPossession checks that hangar ollama key
// register signs the Instance's challenge with ~/.ollama/id_ed25519 (the
// fake Instance verifies the signature), and refuses a private key that
// does not match id_ed25519.pub instead of sending a signature that fails.
func TestOllamaKeyRegisterProvesPossession(t *testing.T) {
	e := newEnv(t)
	e.login()
	writeOllamaKey(t, e.home)
	other := t.TempDir()
	writeOllamaKey(t, other)
	dir := filepath.Join(e.home, ".ollama")
	b, err := os.ReadFile(filepath.Join(other, ".ollama", "id_ed25519"))
	require.NoError(t, err)
	good, err := os.ReadFile(filepath.Join(dir, "id_ed25519"))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "id_ed25519"), b, 0o600))
	r := e.run("ollama", "key", "register")
	require.Equal(t, ExitUsage, r.code, r.stderr)
	require.ErrorContains(t, r.err, "fix the pair")
	require.Empty(t, e.inst.OllamaKeys())

	require.NoError(t, os.WriteFile(filepath.Join(dir, "id_ed25519"), good, 0o600))
	e.ok("ollama", "key", "register")
	require.Len(t, e.inst.OllamaKeys(), 1)
}

func TestOllamaInstallWithYesRegistersTheKey(t *testing.T) {
	e := newEnv(t)
	e.login()
	writeOllamaKey(t, e.home)
	orig := runOllama
	defer func() { runOllama = orig }()
	runOllama = func(context.Context, []string, io.Writer, io.Writer) error { return nil }
	e.inst.AddRepo(mustID(t, model.RepoTypeModel, "acme/model"), clitest.File{Path: "Model-Q4_0.gguf", Content: blobOf("m", 1000)})
	r := e.ok("ollama", "install", "acme/model:Q4_0", "--yes", "--json")
	require.Contains(t, r.stdout, `"model"`)
	require.Len(t, e.inst.OllamaKeys(), 1)
}

// TestOllamaInstallChecksTheQuantIsInTheArsenal checks James's decision of
// 2026-10-18: the Ollama registry keeps its 404 MANIFEST_UNKNOWN for a quant
// that is not pulled, which Ollama 0.31 shows only as "file does not
// exist", so hangar ollama install checks the quant first. When it is
// missing, the command says so, prints the hangar pull command, and offers
// to run it; --pull runs it without asking. Ollama never runs for a quant
// that is not in the Arsenal.
func TestOllamaInstallChecksTheQuantIsInTheArsenal(t *testing.T) {
	e := newEnv(t)
	e.login()
	var ran [][]string
	orig := runOllama
	defer func() { runOllama = orig }()
	runOllama = func(_ context.Context, argv []string, _, _ io.Writer) error {
		ran = append(ran, argv)
		return nil
	}
	host := strings.TrimPrefix(e.inst.URL, "http://")
	id, _ := ggufRepo(e)
	install := func(stdin string, args ...string) result {
		t.Helper()
		return e.runIn(stdin, append([]string{"ollama", "install", "--skip-key-check"}, args...)...)
	}
	const fails = `so Ollama would fail with "file does not exist"`

	// Not in the Arsenal at all: "n" (or no answer) leaves it, exits 3 and
	// names the command, with the quant the Upstream would give for latest.
	r := install("n\n", "bartowski/Tiny-GGUF:Q4_K_M")
	require.Equal(t, ExitNotFound, r.code, r.stderr)
	require.Contains(t, r.stderr, "bartowski/Tiny-GGUF:Q4_K_M is not in the Arsenal, "+fails)
	require.Contains(t, r.stderr, "  hangar pull bartowski/Tiny-GGUF:Q4_K_M --server-only\n")
	require.Contains(t, r.stderr, "Pull it now? [y/N]")
	require.Contains(t, r.err.Error(), "pull it with: hangar pull bartowski/Tiny-GGUF:Q4_K_M --server-only")
	r = install("", "bartowski/Tiny-GGUF")
	require.Equal(t, ExitNotFound, r.code, r.stderr)
	require.Contains(t, r.stderr, "hangar pull bartowski/Tiny-GGUF:Q4_K_M --server-only", "latest is Q4_K_M")
	require.Empty(t, ran, "Ollama does not run for a quant that is not in the Arsenal")
	require.Nil(t, e.inst.Repo(id), "nothing was pulled without a yes")

	// "y" pulls that quant only, then runs Ollama.
	r = install("y\n", "bartowski/Tiny-GGUF:Q4_K_M")
	require.NoError(t, r.err, r.stderr)
	require.Contains(t, r.stderr, "bartowski/Tiny-GGUF:Q4_K_M is in the Arsenal.")
	require.True(t, e.inst.Present(id, "main", "Tiny-Q4_K_M.gguf"))
	require.True(t, e.inst.Present(id, "main", "mmproj-Tiny-f16.gguf"), "the projector comes with the quant")
	require.False(t, e.inst.Present(id, "main", "Tiny-Q8_0.gguf"))
	require.Equal(t, []string{"pull", "--insecure", host + "/bartowski/Tiny-GGUF:Q4_K_M"}, ran[0])

	// A quant the Partial mirror does not hold, matched without regard to
	// case as the registry matches it.
	r = install("", "bartowski/Tiny-GGUF:q8_0")
	require.Equal(t, ExitNotFound, r.code, r.stderr)
	require.Contains(t, r.stderr, "bartowski/Tiny-GGUF:Q8_0 is not in the Arsenal, "+fails)
	require.Contains(t, r.stderr, "hangar pull bartowski/Tiny-GGUF:Q8_0 --server-only")
	require.Len(t, ran, 1)
	r = install("", "bartowski/Tiny-GGUF:q8_0", "--pull")
	require.NoError(t, r.err, r.stderr)
	require.NotContains(t, r.stderr, "Pull it now?", "--pull does not ask")
	require.True(t, e.inst.Present(id, "main", "Tiny-Q8_0.gguf"))
	require.Equal(t, host+"/bartowski/Tiny-GGUF:q8_0", ran[1][2])

	// Present quants run Ollama at once, without a question, by group name
	// or by file name.
	for _, tag := range []string{"", ":Q4_K_M", ":Tiny-Q8_0.gguf", ":latest"} {
		r = install("", "bartowski/Tiny-GGUF"+tag)
		require.NoError(t, r.err, r.stderr)
		require.NotContains(t, r.stderr, "not in the Arsenal", tag)
	}
	require.Len(t, ran, 6)

	// A quant the Repo does not have is not something a Pull can fix.
	r = install("y\n", "bartowski/Tiny-GGUF:Q2_K")
	require.Equal(t, ExitNotFound, r.code)
	require.Contains(t, r.err.Error(), `has no quant "Q2_K"; it has Q4_K_M, Q8_0`)
	require.NotContains(t, r.stderr, "hangar pull")

	// The quant's files are present but its projector is not.
	vision := mustID(t, model.RepoTypeModel, "acme/Vision-GGUF")
	e.inst.AddUpstream(vision, clitest.File{Path: "Vision-Q4_K_M.gguf", Content: blobOf("v", 3000)},
		clitest.File{Path: "mmproj-Vision-f16.gguf", Content: blobOf("vp", 2000)})
	e.ok("pull", "acme/Vision-GGUF", "--include", "Vision-Q4_K_M.gguf", "--server-only")
	require.False(t, e.inst.Present(vision, "main", "mmproj-Vision-f16.gguf"))
	r = install("", "acme/Vision-GGUF")
	require.Equal(t, ExitNotFound, r.code, r.stderr)
	require.Contains(t, r.stderr, "hangar pull acme/Vision-GGUF:Q4_K_M --server-only")
	r = install("yes\n", "acme/Vision-GGUF")
	require.NoError(t, r.err, r.stderr)
	require.True(t, e.inst.Present(vision, "main", "mmproj-Vision-f16.gguf"))
	require.Equal(t, host+"/acme/Vision-GGUF", ran[len(ran)-1][2])

	// A Repo neither the Arsenal nor the Upstream has: the Pull to run is
	// the whole Repo.
	r = install("", "acme/nowhere")
	require.Equal(t, ExitNotFound, r.code)
	require.Contains(t, r.stderr, "hangar pull acme/nowhere --server-only")

	// A JSON caller gets the same refusal and nothing on stdout.
	r = install("", "bartowski/Tiny-GGUF:Q2_K", "--json")
	require.Equal(t, ExitNotFound, r.code)
	require.Empty(t, r.stdout)
}
