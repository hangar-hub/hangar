package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"

	"github.com/jamesagarside/hangar/internal/api/gen"
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

func (a *app) ollamaCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ollama",
		Short: "Install Hangar GGUF models into Ollama",
		Args:  args(cobra.NoArgs),
	}
	a.clientFlags(cmd)
	var yes, skipKey bool
	install := &cobra.Command{
		Use:   "install <repo>[:<quant>]",
		Short: "Pull a GGUF model into Ollama from the Instance",
		Long: `Runs ollama pull <host>/<namespace>/<name>:<quant> against the Instance, adding
--insecure for an http:// Instance. Before that, checks that your Ollama key
(~/.ollama/id_ed25519.pub) is registered with the Instance, which Ollama needs
for Repos that are not public, and offers to register it.`,
		Example: `  hangar ollama install bartowski/Llama-3.2-3B-Instruct-GGUF:Q4_K_M
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
			if c.token != "" && !skipKey {
				if err := a.ensureOllamaKey(cmd.Context(), c, cmd.InOrStdin(), yes); err != nil {
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
	install.Flags().BoolVar(&skipKey, "skip-key-check", false, "do not check the Ollama key, for public Repos")

	key := &cobra.Command{Use: "key", Short: "Manage your Ollama key on the Instance", Args: args(cobra.NoArgs)}
	register := &cobra.Command{
		Use:   "register",
		Short: "Register ~/.ollama/id_ed25519.pub with the Instance",
		Args:  args(cobra.NoArgs),
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
	host, _ := os.Hostname()
	name := "Ollama on " + orDash(host)
	if _, err := c.addOllamaKey(ctx, gen.OllamaKeyCreate{Name: &name, PublicKey: pub}); err != nil {
		return "", false, err
	}
	return fp, false, nil
}

// ensureOllamaKey checks the key is registered and offers to register it.
func (a *app) ensureOllamaKey(ctx context.Context, c *client, stdin io.Reader, yes bool) error {
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
		a.logf("Your Ollama key %s is not registered with %s. Ollama needs it for Repos that are not public.\nRegister it now? [y/N] ", fp, c.base.Host)
		line, _ := bufio.NewReader(stdin).ReadString('\n')
		if ans := strings.ToLower(strings.TrimSpace(line)); ans != "y" && ans != "yes" {
			a.logf("Not registering the key. Public Repos still work.\n")
			return nil
		}
	}
	if _, _, err := a.registerOllamaKey(ctx, c); err != nil {
		return err
	}
	a.logf("Registered your Ollama key %s.\n", fp)
	return nil
}
