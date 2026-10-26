package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"

	"github.com/hangar-hub/hangar/internal/api/gen"
	"github.com/hangar-hub/hangar/internal/model"
	"github.com/hangar-hub/hangar/internal/sshsig"
)

// runOllama runs the ollama CLI. Tests replace it.
var runOllama = func(ctx context.Context, argv []string, stdout, stderr io.Writer) error {
	bin, err := exec.LookPath("ollama")
	if err != nil {
		return fmt.Errorf("ollama is not installed or not on PATH; get it from https://ollama.com/download")
	}
	cmd := exec.CommandContext(ctx, bin, argv...)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	return cmd.Run()
}

// ollamaDir is Ollama's folder, ~/.ollama, which holds its key pair.
func ollamaDir() string {
	return filepath.Join(userHome(), ".ollama")
}

// ollamaPublicKey reads ~/.ollama/id_ed25519.pub and returns it with its
// SHA-256 fingerprint, the form the Instance stores.
func ollamaPublicKey() (key, fingerprint string, err error) {
	p := filepath.Join(ollamaDir(), "id_ed25519.pub")
	b, err := os.ReadFile(p) // #nosec G304 -- Ollama's fixed key path.
	if errors.Is(err, os.ErrNotExist) {
		return "", "", notFoundf("no Ollama key at %s; run ollama once (for example ollama list) to create it", p)
	}
	if err != nil {
		return "", "", err
	}
	pub, _, _, _, err := ssh.ParseAuthorizedKey(b)
	if err != nil {
		return "", "", fmt.Errorf("read %s: %w", p, err)
	}
	return strings.TrimSpace(string(b)), ssh.FingerprintSHA256(pub), nil
}

// ollamaSigner reads Ollama's private key, ~/.ollama/id_ed25519, which
// signs the challenge that proves to the Instance that this user holds the
// key. Ollama writes it without a passphrase.
func ollamaSigner(fingerprint string) (ssh.Signer, error) {
	p := filepath.Join(ollamaDir(), "id_ed25519")
	b, err := os.ReadFile(p) // #nosec G304 -- Ollama's fixed key path.
	if errors.Is(err, os.ErrNotExist) {
		return nil, notFoundf("no Ollama private key at %s; run ollama once (for example ollama list) to create it", p)
	}
	if err != nil {
		return nil, err
	}
	signer, err := ssh.ParsePrivateKey(b)
	var missing *ssh.PassphraseMissingError
	if errors.As(err, &missing) {
		return nil, usagef("%s has a passphrase; register the key in the web UI under Settings, Ollama keys, which shows the ssh-keygen command that signs with it", p)
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", p, err)
	}
	if got := ssh.FingerprintSHA256(signer.PublicKey()); got != fingerprint {
		return nil, usagef("%s is for the key %s, but id_ed25519.pub is %s; fix the pair before registering it", p, got, fingerprint)
	}
	return signer, nil
}

func (a *app) ollamaCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ollama",
		Short: "Install Hangar GGUF models into Ollama",
		Args:  args(cobra.NoArgs),
	}
	a.clientFlags(cmd)
	var yes, skipKey, pull bool
	install := &cobra.Command{
		Use:   "install <repo>[:<quant>]",
		Short: "Pull a GGUF model into Ollama from the Instance",
		Long: `Runs ollama pull <host>/<namespace>/<name>:<quant> against the Instance, adding
--insecure for an http:// Instance.

Before that, checks that the quant is in the Arsenal. Without a quant, Ollama
asks for "latest", which is Q4_K_M or the closest quant. Ollama shows only
"file does not exist" for a quant the Instance has not pulled, so when it is
missing this command says so, prints the hangar pull command that pulls it
into the Instance, and offers to run it (--pull runs it without asking).

Then checks that your Ollama key (~/.ollama/id_ed25519.pub) is registered with
the Instance, and offers to register it. Ollama needs the key for every Repo
unless the Repo is public and the Instance allows anonymous reads
(security.anonymous_read, off by default).`,
		Example: `  hangar ollama install bartowski/Llama-3.2-3B-Instruct-GGUF:Q4_K_M
  hangar ollama install bartowski/Llama-3.2-3B-Instruct-GGUF:Q8_0 --pull
  hangar ollama install acme/private-model --yes`,
		Args: args(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, argv []string) error {
			a := a.with(cmd)
			ref, err := parseRepoRef(argv[0], "model", true)
			if err != nil {
				return err
			}
			if strings.HasPrefix(argv[0], "datasets/") {
				return usagef("Ollama installs models, not datasets")
			}
			c, err := a.client()
			if err != nil {
				return err
			}
			if c.base.Path != "" && c.base.Path != "/" {
				return usagef("Ollama needs the Instance at the root of its host, not under %s", c.base.Path)
			}
			// One reader for every question, so an answer typed or piped
			// ahead is not lost between them.
			in := bufio.NewReader(cmd.InOrStdin())
			if err := a.ensureOllamaQuant(cmd.Context(), c, ref, in, pull); err != nil {
				return err
			}
			if c.token != "" && !skipKey {
				if err := a.ensureOllamaKey(cmd.Context(), c, in, yes); err != nil {
					return err
				}
			}
			name := c.base.Host + "/" + ref.ID.String()
			if ref.Quant != "" {
				name += ":" + ref.Quant
			}
			argvOllama := []string{"pull"}
			if c.base.Scheme == "http" {
				argvOllama = append(argvOllama, "--insecure")
			}
			argvOllama = append(argvOllama, name)
			a.logf("Running ollama %s\n", strings.Join(argvOllama, " "))
			if err := runOllama(cmd.Context(), argvOllama, a.stderr, a.stderr); err != nil {
				return fmt.Errorf("ollama pull: %w", err)
			}
			if a.json {
				return a.printJSON(map[string]string{"model": name})
			}
			a.printf("Installed %s. Run it with: ollama run %s\n", name, name)
			return nil
		},
	}
	install.Flags().BoolVarP(&yes, "yes", "y", false, "register the Ollama key without asking")
	install.Flags().BoolVar(&pull, "pull", false, "pull the quant into the Instance first, without asking, if it is not in the Arsenal")
	install.Flags().BoolVar(&skipKey, "skip-key-check", false, "do not check the Ollama key, for public Repos on an Instance that allows anonymous reads")

	key := &cobra.Command{Use: "key", Short: "Manage your Ollama key on the Instance", Args: args(cobra.NoArgs)}
	register := &cobra.Command{
		Use:   "register",
		Short: "Register ~/.ollama/id_ed25519.pub with the Instance",
		Long: `Registers your Ollama key (~/.ollama/id_ed25519.pub) with the Instance, unless
it is already registered. To prove the key is yours, the CLI signs a challenge
from the Instance with the private key, ~/.ollama/id_ed25519, which never
leaves this machine.`,
		Args: args(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			a := a.with(cmd)
			c, err := a.client()
			if err != nil {
				return err
			}
			k, already, err := a.registerOllamaKey(cmd.Context(), c)
			if err != nil {
				return err
			}
			if a.json {
				return a.printJSON(map[string]any{"fingerprint": k, "already_registered": already})
			}
			if already {
				a.printf("Your Ollama key %s is already registered.\n", k)
			} else {
				a.printf("Registered your Ollama key %s.\n", k)
			}
			return nil
		},
	}
	key.AddCommand(register)
	cmd.AddCommand(install, key)
	return cmd
}

// registerOllamaKey registers the local key unless it is already there.
func (a *app) registerOllamaKey(ctx context.Context, c *client) (fingerprint string, already bool, err error) {
	pub, fp, err := ollamaPublicKey()
	if err != nil {
		return "", false, err
	}
	keys, err := c.ollamaKeys(ctx)
	if err != nil {
		return "", false, err
	}
	for _, k := range keys {
		if k.Fingerprint == fp {
			return fp, true, nil
		}
	}
	signer, err := ollamaSigner(fp)
	if err != nil {
		return "", false, err
	}
	ch, err := c.ollamaKeyChallenge(ctx)
	if err != nil {
		return "", false, err
	}
	sig, err := sshsig.Sign(signer, ch.Namespace, []byte(ch.Challenge))
	if err != nil {
		return "", false, err
	}
	host, _ := os.Hostname()
	name := "Ollama on " + orDash(host)
	if _, err := c.addOllamaKey(ctx, gen.OllamaKeyCreate{Name: &name, PublicKey: pub, Challenge: ch.Challenge, Signature: sig}); err != nil {
		return "", false, err
	}
	return fp, false, nil
}

// ensureOllamaKey checks the key is registered and offers to register it.
func (a *app) ensureOllamaKey(ctx context.Context, c *client, in *bufio.Reader, yes bool) error {
	_, fp, err := ollamaPublicKey()
	if err != nil {
		return err
	}
	keys, err := c.ollamaKeys(ctx)
	if err != nil {
		return err
	}
	for _, k := range keys {
		if k.Fingerprint == fp {
			return nil
		}
	}
	if !yes {
		a.logf("Your Ollama key %s is not registered with %s. Ollama needs it unless the Repo is public and the Instance allows anonymous reads.\nRegister it now? [y/N] ", fp, c.base.Host)
		if !answeredYes(in) {
			a.logf("Not registering the key. Only public Repos on an Instance that allows anonymous reads work.\n")
			return nil
		}
	}
	if _, _, err := a.registerOllamaKey(ctx, c); err != nil {
		return err
	}
	a.logf("Registered your Ollama key %s.\n", fp)
	return nil
}

// answeredYes reads one line and reports whether it is y or yes.
func answeredYes(in *bufio.Reader) bool {
	line, _ := in.ReadString('\n')
	ans := strings.ToLower(strings.TrimSpace(line))
	return ans == "y" || ans == "yes"
}

// ollamaPullCommand is the hangar pull command that puts a quant of a Repo
// into the Arsenal without downloading it here.
func ollamaPullCommand(id model.RepoID, quant string) string {
	ref := id.String()
	if quant != "" {
		ref += ":" + quant
	}
	return "hangar pull " + ref + " --server-only"
}

// ollamaQuantGroup picks the quant group the Ollama registry serves for a
// tag, as internal/ollama does: no tag or "latest" picks Q4_K_M or the
// closest quant, otherwise the tag matches a group name or a GGUF filename,
// ignoring case (08-local-tools.md "Names and tags").
func ollamaQuantGroup(id model.RepoID, groups []gen.QuantGroup, tag string) (gen.QuantGroup, error) {
	if len(groups) == 0 {
		return gen.QuantGroup{}, notFoundf("%s has no GGUF files, so Ollama cannot use it", display(id))
	}
	if tag == "" || strings.EqualFold(tag, "latest") {
		want := preferredQuant(groups)
		for _, g := range groups {
			if g.Name == want {
				return g, nil
			}
		}
	}
	for _, g := range groups {
		if strings.EqualFold(g.Name, tag) {
			return g, nil
		}
	}
	for _, g := range groups {
		for _, f := range g.Files {
			base := path.Base(f)
			if strings.EqualFold(base, tag) || strings.EqualFold(strings.TrimSuffix(base, path.Ext(base)), tag) {
				return g, nil
			}
		}
	}
	names := make([]string, 0, len(groups))
	for _, g := range groups {
		names = append(names, g.Name)
	}
	return gen.QuantGroup{}, notFoundf("%s has no quant %q; it has %s", display(id), tag, strings.Join(names, ", "))
}

// ensureOllamaQuant checks that the quant Ollama will ask for is in the
// Arsenal. Ollama 0.31 shows only "file does not exist" for a quant the
// Instance has not pulled (decided by James on 2026-10-18 to keep the
// registry's 404 and check here instead), so when it is missing this says
// so, prints the hangar pull command and offers to run it. With pull set,
// it runs it without asking.
func (a *app) ensureOllamaQuant(ctx context.Context, c *client, ref repoRef, in *bufio.Reader, pull bool) error {
	quant, missing, err := a.missingOllamaQuant(ctx, c, ref)
	if err != nil || !missing {
		return err
	}
	name := display(ref.ID)
	if quant != "" {
		name += ":" + quant
	}
	command := ollamaPullCommand(ref.ID, quant)
	a.logf("%s is not in the Arsenal, so Ollama would fail with \"file does not exist\". Pull it into the Instance first:\n  %s\n", name, command)
	if !pull {
		a.logf("Pull it now? [y/N] ")
		if !answeredYes(in) {
			return notFoundf("%s is not in the Arsenal; pull it with: %s", name, command)
		}
	}
	if _, err := a.pull(ctx, repoRef{ID: ref.ID, Quant: quant}, pullOptions{ServerOnly: true, Upstream: "huggingface"}); err != nil {
		return err
	}
	a.logf("%s is in the Arsenal.\n", name)
	return nil
}

// missingOllamaQuant reports the quant Ollama will ask for and whether any
// of its files, or of its projectors, are not in the Arsenal yet. quant is
// empty when the Repo is in neither the Arsenal nor the Upstream's quant
// list, so the whole Repo is the Pull to run.
func (a *app) missingOllamaQuant(ctx context.Context, c *client, ref repoRef) (quant string, missing bool, err error) {
	q, err := c.quants(ctx, ref.ID, "")
	switch {
	case err == nil:
	case IsNotFound(err):
		// Not in the Arsenal, or not one the caller can read: name the
		// quant the Upstream would give.
		quant = ref.Quant
		if d, derr := c.discoverDetail(ctx, ref.ID, "huggingface", ""); derr == nil {
			if g, gerr := ollamaQuantGroup(ref.ID, deref(d.Quants), ref.Quant); gerr == nil {
				quant = g.Name
			} else if len(deref(d.Quants)) > 0 {
				return "", false, gerr
			}
		}
		return quant, true, nil
	case ExitCode(err) == ExitAuth && c.token == "":
		// Ollama signs its own requests, so it may read what this CLI,
		// signed out, cannot.
		a.logf("Could not check that %s is in the Arsenal without signing in (hangar login). Running ollama pull anyway.\n", display(ref.ID))
		return "", false, nil
	default:
		return "", false, err
	}
	g, err := ollamaQuantGroup(ref.ID, q.Items, ref.Quant)
	if err != nil {
		return "", false, err
	}
	if !g.Present {
		return g.Name, true, nil
	}
	if projectors := deref(g.Projectors); len(projectors) > 0 {
		_, entries, err := c.tree(ctx, ref.ID, q.Commit)
		if err != nil {
			return "", false, err
		}
		present := map[string]bool{}
		for _, e := range entries {
			present[e.Path] = e.Present
		}
		for _, p := range projectors {
			if !present[p] {
				return g.Name, true, nil
			}
		}
	}
	return g.Name, false, nil
}
