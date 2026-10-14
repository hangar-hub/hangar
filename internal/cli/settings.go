package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/zalando/go-keyring"
	"go.yaml.in/yaml/v3"
)

// Environment variables the client commands read. They use the HANGAR_CLI_
// prefix, which the server's configuration loader ignores.
const (
	EnvURL        = "HANGAR_CLI_URL"
	EnvToken      = "HANGAR_CLI_TOKEN" // #nosec G101 -- a variable name, not a credential.
	EnvConfigFile = "HANGAR_CLI_CONFIG"
	EnvNoKeychain = "HANGAR_CLI_NO_KEYCHAIN"
)

// keychainService names Hangar's entries in the OS keychain.
const keychainService = "hangar"

// Settings is the client configuration in ~/.config/hangar/config.yaml.
type Settings struct {
	// URL is the Instance's public URL.
	URL string `yaml:"url"`
	// Username is the signed-in user, for display.
	Username string `yaml:"username,omitempty"`
	// Token is kept here only when the OS keychain is not available.
	Token string `yaml:"token,omitempty"`
	// TokenInKeychain says the Token is in the OS keychain under the URL.
	TokenInKeychain bool `yaml:"token_in_keychain,omitempty"`
}

// configDir returns the directory for the client's files: $XDG_CONFIG_HOME/hangar
// or ~/.config/hangar.
func configDir() (string, error) {
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "hangar"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "hangar"), nil
}

// settingsPath returns where the client configuration lives.
func settingsPath() (string, error) {
	if p := os.Getenv(EnvConfigFile); p != "" {
		return p, nil
	}
	d, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "config.yaml"), nil
}

// loadSettings reads the client configuration. A missing file gives empty settings.
func loadSettings() (*Settings, string, error) {
	path, err := settingsPath()
	if err != nil {
		return nil, "", err
	}
	s := &Settings{}
	b, err := os.ReadFile(path) // #nosec G304 -- the user's own configuration file.
	if errors.Is(err, os.ErrNotExist) {
		return s, path, nil
	}
	if err != nil {
		return nil, path, err
	}
	if err := yaml.Unmarshal(b, s); err != nil {
		return nil, path, fmt.Errorf("read %s: %w", path, err)
	}
	return s, path, nil
}

// save writes the configuration with mode 0600.
func (s *Settings) save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := yaml.Marshal(s)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// keychainEnabled reports whether the OS keychain should be used.
func keychainEnabled() bool {
	v := strings.ToLower(os.Getenv(EnvNoKeychain))
	return v == "" || v == "0" || v == "false"
}

// storeToken saves the Token in the keychain if possible, otherwise in the
// settings file. It reports where it went.
func (s *Settings) storeToken(token string) (inKeychain bool) {
	if keychainEnabled() {
		if err := keyring.Set(keychainService, s.URL, token); err == nil {
			s.Token = ""
			s.TokenInKeychain = true
			return true
		}
	}
	s.Token = token
	s.TokenInKeychain = false
	return false
}

// token returns the saved Token, from the keychain or the file.
func (s *Settings) token() (string, error) {
	if s.TokenInKeychain {
		t, err := keyring.Get(keychainService, s.URL)
		if err != nil {
			return "", fmt.Errorf("read the Token from the OS keychain: %w", err)
		}
		return t, nil
	}
	return s.Token, nil
}

// forgetToken removes the Token from the keychain and the file.
func (s *Settings) forgetToken() {
	if s.TokenInKeychain {
		_ = keyring.Delete(keychainService, s.URL)
	}
	s.Token = ""
	s.TokenInKeychain = false
}
