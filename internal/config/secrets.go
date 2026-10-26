package config

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/knadh/koanf/parsers/yaml"
)

// SecretsFileName is the file in server.data_dir that holds generated secrets.
const SecretsFileName = "secrets.yaml"

// Generated lists secrets that EnsureSecrets created on this start. The
// caller prints them once; later starts read them from the secrets file.
type Generated struct {
	Path          string
	InstanceKey   string
	AdminPassword string
}

// Any reports whether anything was generated.
func (g *Generated) Any() bool { return g != nil && (g.InstanceKey != "" || g.AdminPassword != "") }

// EnsureSecrets fills security.instance_key and the bootstrap admin password
// when the configuration leaves them empty. It reads them from
// <server.data_dir>/secrets.yaml if that file exists, and otherwise generates
// them and writes the file with mode 0600. In roles web and worker it never
// generates an Instance key, because every process must use the same one. A password_file, if set, is read
// into security.bootstrap_admin.password.
func EnsureSecrets(c *Config) (*Generated, error) {
	if c.Security.BootstrapAdmin.PasswordFile != "" {
		b, err := os.ReadFile(c.Security.BootstrapAdmin.PasswordFile)
		if err != nil {
			return nil, fmt.Errorf("read security.bootstrap_admin.password_file: %w", err)
		}
		c.Security.BootstrapAdmin.Password = strings.TrimRight(string(b), "\r\n")
		if c.Security.BootstrapAdmin.Password == "" {
			return nil, errors.New("security.bootstrap_admin.password_file is empty")
		}
	}
	needKey := c.Security.InstanceKey == ""
	needPassword := c.Security.BootstrapAdmin.Password == ""
	if !needKey && !needPassword {
		return &Generated{}, nil
	}

	path := filepath.Join(c.Server.DataDir, SecretsFileName)
	stored := map[string]any{}
	switch b, err := os.ReadFile(path); { //nolint:gosec // path is server.data_dir from the server config.
	case err == nil:
		stored, err = yaml.Parser().Unmarshal(b)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
	case errors.Is(err, fs.ErrNotExist):
	default:
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	gen := &Generated{Path: path}
	changed := false
	if needKey {
		if v, _ := stored["instance_key"].(string); v != "" {
			c.Security.InstanceKey = v
		} else if c.Server.splitRole() {
			return nil, errors.New(c.Server.missingSplitRoleKey())
		} else {
			key := make([]byte, InstanceKeyBytes)
			if _, err := rand.Read(key); err != nil {
				return nil, err
			}
			c.Security.InstanceKey = base64.StdEncoding.EncodeToString(key)
			stored["instance_key"] = c.Security.InstanceKey
			gen.InstanceKey = c.Security.InstanceKey
			changed = true
		}
	}
	if needPassword {
		if v, _ := stored["bootstrap_admin_password"].(string); v != "" {
			c.Security.BootstrapAdmin.Password = v
		} else {
			pw, err := RandomPassword(24)
			if err != nil {
				return nil, err
			}
			c.Security.BootstrapAdmin.Password = pw
			stored["bootstrap_admin_password"] = pw
			gen.AdminPassword = pw
			changed = true
		}
	}
	if changed {
		out, err := yaml.Parser().Marshal(stored)
		if err != nil {
			return nil, err
		}
		if err := os.MkdirAll(c.Server.DataDir, 0o700); err != nil {
			return nil, fmt.Errorf("create server.data_dir: %w", err)
		}
		if err := writeFileAtomic(path, out, 0o600); err != nil {
			return nil, fmt.Errorf("write %s: %w", path, err)
		}
	}
	return gen, nil
}

// LoadStoredSecrets fills an empty security.instance_key and bootstrap
// admin password from secrets.yaml in server.data_dir, where the first
// start saved them, without generating or writing anything. Commands that
// inspect an Instance, such as hangar admin doctor, use it so the
// configuration they check is the one the server runs with. A missing file
// is not an error.
func LoadStoredSecrets(c *Config) error {
	if c.Security.InstanceKey != "" && c.Security.BootstrapAdmin.Password != "" {
		return nil
	}
	path := filepath.Join(c.Server.DataDir, SecretsFileName)
	b, err := os.ReadFile(path) //nolint:gosec // path is server.data_dir from the server config.
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	stored, err := yaml.Parser().Unmarshal(b)
	if err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	if v, _ := stored["instance_key"].(string); v != "" && c.Security.InstanceKey == "" {
		c.Security.InstanceKey = v
	}
	if v, _ := stored["bootstrap_admin_password"].(string); v != "" && c.Security.BootstrapAdmin.Password == "" {
		c.Security.BootstrapAdmin.Password = v
	}
	return nil
}

const passwordAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz23456789"

// RandomPassword returns a password of n characters from an alphabet
// without look-alike characters.
func RandomPassword(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	out := make([]byte, n)
	for i, v := range b {
		// 256 % 57 bias is negligible for a generated password of this length,
		// but reject the biased tail anyway.
		for int(v) >= 256-256%len(passwordAlphabet) {
			var one [1]byte
			if _, err := rand.Read(one[:]); err != nil {
				return "", err
			}
			v = one[0]
		}
		out[i] = passwordAlphabet[int(v)%len(passwordAlphabet)]
	}
	return string(out), nil
}

func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".secrets-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
