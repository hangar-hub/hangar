package cli

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"go.yaml.in/yaml/v3"

	"github.com/jamesagarside/hangar/internal/blob"
	"github.com/jamesagarside/hangar/internal/config"
	"github.com/jamesagarside/hangar/internal/db"
)

// AdminDeps gives the admin commands what internal/cli cannot build itself.
type AdminDeps struct {
	// OpenBlobStore opens the Blob store the configuration names. The
	// commands that copy or check Blob store data need it; without it they
	// stop with an error that names the missing wiring.
	OpenBlobStore func(ctx context.Context, cfg *config.Config) (blob.Store, error)
}

// errNoBlobStore is returned when a build has no Blob store opener.
var errNoBlobStore = errors.New("this hangar build cannot open the Blob store (cli.AdminDeps.OpenBlobStore is not set)")

// adminFlags are the configuration flags of the admin commands, the same as
// `hangar server` takes.
type adminFlags struct {
	file string
	set  []string
}

func (f *adminFlags) register(cmd *cobra.Command, a *app) {
	fl := cmd.Flags()
	fl.BoolVar(&a.json, "json", false, "print JSON instead of text")
	fl.StringVarP(&f.file, "config", "c", "", "YAML configuration file (or set HANGAR_CONFIG)")
	fl.StringArrayVar(&f.set, "set", nil, "set a configuration key, such as --set database.dsn=... (repeatable)")
}

func (f *adminFlags) load() (*config.Config, error) {
	cfg, err := config.Load(config.LoadOptions{File: f.file, Set: f.set})
	if err != nil {
		return nil, &ExitError{Code: ExitUsage, Err: err}
	}
	return cfg, nil
}

// openDB opens the configured database.
func openDB(ctx context.Context, cfg *config.Config) (*db.DB, error) {
	d, err := db.Open(ctx, db.Options{Driver: cfg.Database.Driver, DSN: cfg.Database.DSN, MaxOpenConns: cfg.Database.MaxOpenConns})
	if errors.Is(err, db.ErrLocked) {
		return nil, fmt.Errorf("%w: stop Hangar before running this command on a SQLite database", err)
	}
	return d, err
}

// openStore opens the Blob store through the deps.
func (a *app) openStore(ctx context.Context, cfg *config.Config) (blob.Store, error) {
	if a.opts.Admin.OpenBlobStore == nil {
		return nil, errNoBlobStore
	}
	return a.opts.Admin.OpenBlobStore(ctx, cfg)
}

// Secret columns that rotate-key re-encrypts, with the purpose each one is
// sealed with (internal/secrets binds the purpose into the ciphertext).
var rotateTargets = []struct {
	Table, Column, Purpose string
}{
	{"upstream_tokens", "ciphertext", "upstream_token"},
	{"upstreams", "instance_token_ciphertext", "upstream_instance_token"},
	{"webhooks", "secret_ciphertext", "webhook_secret"},
}

func (a *app) rotateKeyCommand() *cobra.Command {
	var f adminFlags
	var newKey string
	cmd := &cobra.Command{
		Use:   "rotate-key",
		Short: "Replace the Instance key and re-encrypt every stored secret",
		Long: `Generates a new security.instance_key (or uses --new-key) and, in one
database transaction, re-encrypts every secret sealed with the old key:
users' upstream tokens, Upstream Instance tokens and webhook secrets.

Stop every Hangar process first: a process still running with the old key
cannot read the re-encrypted secrets. If the key came from secrets.yaml in
server.data_dir, the file is updated. Otherwise the new key is printed once;
put it in your configuration, environment or Kubernetes Secret before you
start Hangar again.`,
		Args: args(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			a := a.with(cmd)
			cfg, err := f.load()
			if err != nil {
				return err
			}
			return a.rotateKey(cmd.Context(), cfg, newKey)
		},
	}
	f.register(cmd, a)
	cmd.Flags().StringVar(&newKey, "new-key", "", "the new key, 32 bytes base64-encoded (default: generate one)")
	return cmd
}

func (a *app) rotateKey(ctx context.Context, cfg *config.Config, newKey string) error {
	fromFile := false
	secretsPath := filepath.Join(cfg.Server.DataDir, config.SecretsFileName)
	stored := map[string]any{}
	if cfg.Security.InstanceKey == "" {
		b, err := os.ReadFile(secretsPath) // #nosec G304 -- server.data_dir from the server configuration.
		if err != nil {
			return fmt.Errorf("security.instance_key is not set and %s cannot be read: %w", secretsPath, err)
		}
		if err := yaml.Unmarshal(b, &stored); err != nil {
			return fmt.Errorf("read %s: %w", secretsPath, err)
		}
		k, _ := stored["instance_key"].(string)
		if k == "" {
			return fmt.Errorf("%s has no instance_key", secretsPath)
		}
		cfg.Security.InstanceKey = k
		fromFile = true
	}
	oldRaw, err := cfg.InstanceKeyBytes()
	if err != nil {
		return fmt.Errorf("security.instance_key %w", err)
	}
	if newKey == "" {
		k := make([]byte, config.InstanceKeyBytes)
		if _, err := rand.Read(k); err != nil {
			return err
		}
		newKey = base64.StdEncoding.EncodeToString(k)
	}
	newCfg := *cfg
	newCfg.Security.InstanceKey = newKey
	newRaw, err := newCfg.InstanceKeyBytes()
	if err != nil {
		return usagef("--new-key %v", err)
	}
	d, err := openDB(ctx, cfg)
	if err != nil {
		return err
	}
	defer d.Close()
	counts, err := reencrypt(ctx, d, oldRaw, newRaw)
	if err != nil {
		return err
	}
	total := 0
	for _, n := range counts {
		total += n
	}
	if fromFile {
		stored["instance_key"] = newKey
		out, err := yaml.Marshal(stored)
		if err == nil {
			err = writeFile0600(secretsPath, out)
		}
		if err != nil {
			a.printf("The secrets are re-encrypted, but %s could not be updated (%v).\nSet security.instance_key to this new key before you start Hangar:\n%s\n", secretsPath, err, newKey)
			return err
		}
	}
	if a.json {
		out := map[string]any{"reencrypted": counts, "secrets_file_updated": fromFile}
		if !fromFile {
			out["instance_key"] = newKey
		}
		return a.printJSON(out)
	}
	a.printf("Re-encrypted %d secrets with the new Instance key.\n", total)
	if fromFile {
		a.printf("Updated %s.\n", secretsPath)
	} else {
		a.printf("Set security.instance_key (HANGAR_SECURITY_INSTANCE_KEY) to this new key before you start Hangar. It is not shown again:\n%s\n", newKey)
	}
	return nil
}

func writeFile0600(path string, b []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
