package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
	"github.com/zalando/go-keyring"

	"github.com/hangar-hub/hangar/internal/cli/clitest"
)

func TestLoginWithTokenWhoamiAndLogout(t *testing.T) {
	e := newEnv(t)
	r := e.ok("login", "--url", e.inst.URL+"/", "--token", clitest.Token)
	require.Contains(t, r.stdout, "Signed in to "+e.inst.URL+" as alice")

	cfgPath := filepath.Join(e.home, ".config", "hangar", "config.yaml")
	st, err := os.Stat(cfgPath)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), st.Mode().Perm())
	b, _ := os.ReadFile(cfgPath)
	require.Contains(t, string(b), "url: "+e.inst.URL)
	require.Contains(t, string(b), clitest.Token, "without a keychain the Token is in the file")

	r = e.ok("whoami")
	require.Contains(t, r.stdout, "alice on "+e.inst.URL)
	require.Contains(t, r.stdout, "acme")
	r = e.ok("whoami", "--json")
	var me map[string]any
	require.NoError(t, json.Unmarshal([]byte(r.stdout), &me))
	require.Equal(t, "alice", me["username"])

	r = e.ok("logout")
	require.Contains(t, r.stdout, "Signed out")
	b, _ = os.ReadFile(cfgPath)
	require.NotContains(t, string(b), clitest.Token)
	r = e.run("whoami")
	require.Equal(t, ExitAuth, r.code, r.stderr)
	r = e.ok("logout")
	require.Contains(t, r.stdout, "Signed out")
}

func TestLoginRejectsABadTokenWithExitCode4(t *testing.T) {
	e := newEnv(t)
	r := e.run("login", "--url", e.inst.URL, "--token", "hgr_wrong")
	require.Equal(t, ExitAuth, r.code)
	r = e.run("login", "--url", "ftp://nope", "--token", "x")
	require.Equal(t, ExitUsage, r.code)
	r = e.run("whoami")
	require.Equal(t, ExitUsage, r.code, "no Instance URL is a usage error")
}

func TestLoginReadsTheTokenFromStdin(t *testing.T) {
	e := newEnv(t)
	r := e.runIn(clitest.Token+"\n", "login", "--url", e.inst.URL, "--token", "-", "--json")
	require.NoError(t, r.err, r.stderr)
	require.Contains(t, r.stdout, `"username": "alice"`)
	r = e.runIn("\n", "login", "--url", e.inst.URL, "--token", "-")
	require.Equal(t, ExitUsage, r.code)
}

func TestLoginUsesTheKeychain(t *testing.T) {
	keyring.MockInit()
	e := newEnv(t)
	t.Setenv(EnvNoKeychain, "")
	r := e.ok("login", "--url", e.inst.URL, "--token", clitest.Token)
	require.Contains(t, r.stdout, "OS keychain")
	b, _ := os.ReadFile(filepath.Join(e.home, ".config", "hangar", "config.yaml"))
	require.NotContains(t, string(b), clitest.Token)
	require.Contains(t, string(b), "token_in_keychain: true")
	got, err := keyring.Get(keychainService, e.inst.URL)
	require.NoError(t, err)
	require.Equal(t, clitest.Token, got)
	e.ok("whoami")
	e.ok("logout")
	_, err = keyring.Get(keychainService, e.inst.URL)
	require.Error(t, err)
}

func TestDeviceLogin(t *testing.T) {
	e := newEnv(t)
	var opened string
	openBrowser = func(u string) error { opened = u; return nil }
	defer func() { openBrowser = func(string) error { return nil } }()
	go func() {
		for range 200 {
			if e.inst.ApproveDevice("ABCD-0001") {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
	r := e.ok("login", "--url", e.inst.URL)
	require.Contains(t, r.stderr, "ABCD-0001")
	// The page asks for the code to be typed, so the CLI opens it without
	// the code even when the Instance sends a link that has it.
	require.True(t, strings.HasSuffix(opened, "/settings/device"), "opened %s", opened)
	require.Contains(t, r.stderr, "type the code ABCD-0001")
	require.Contains(t, r.stdout, "as alice")
	e.ok("whoami")
}

func TestDeviceLoginDenied(t *testing.T) {
	e := newEnv(t)
	go func() {
		for range 200 {
			if e.inst.DenyDevice("ABCD-0001") {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
	r := e.run("login", "--url", e.inst.URL, "--no-browser")
	require.Equal(t, ExitAuth, r.code)
	require.Contains(t, r.err.Error(), "denied")
}

func TestEnvPrintsExportsForEveryShell(t *testing.T) {
	e := newEnv(t)
	e.login()
	r := e.ok("env")
	require.Contains(t, r.stdout, "export HF_ENDPOINT='"+e.inst.URL+"'")
	require.Contains(t, r.stdout, "export HF_TOKEN='"+clitest.Token+"'")
	require.Contains(t, r.stdout, "export DISABLE_SAFETENSORS_CONVERSION='1'")
	r = e.ok("env", "--shell", "fish")
	require.Contains(t, r.stdout, "set -gx HF_ENDPOINT '"+e.inst.URL+"';")
	r = e.ok("env", "--shell", "powershell")
	require.Contains(t, r.stdout, "$Env:HF_TOKEN = '"+clitest.Token+"'")
	r = e.ok("env", "--json")
	var m map[string]string
	require.NoError(t, json.Unmarshal([]byte(r.stdout), &m))
	require.Equal(t, "1", m["DISABLE_SAFETENSORS_CONVERSION"])
	r = e.run("env", "--shell", "tcsh")
	require.Equal(t, ExitUsage, r.code)

	e.ok("logout")
	r = e.ok("env", "--url", e.inst.URL)
	require.NotContains(t, r.stdout, "HF_TOKEN")
	require.Contains(t, r.stderr, "Not signed in")
}

func TestEnvUsesHangarCLIVariables(t *testing.T) {
	e := newEnv(t)
	t.Setenv(EnvURL, e.inst.URL)
	t.Setenv(EnvToken, clitest.Token)
	r := e.ok("whoami")
	require.True(t, strings.HasPrefix(r.stdout, "alice"))
}

func TestQuoting(t *testing.T) {
	require.Equal(t, `'it'\''s'`, posixQuote("it's"))
	require.Equal(t, `'it\'s \\ x'`, fishQuote(`it's \ x`))
	line, err := exportLine("pwsh", "A", "it's")
	require.NoError(t, err)
	require.Equal(t, `$Env:A = 'it''s'`, line)
	t.Setenv("SHELL", "")
	t.Setenv("PSModulePath", "")
	require.Equal(t, "bash", defaultShell())
	t.Setenv("PSModulePath", "x")
	require.Equal(t, "powershell", defaultShell())
}

func TestLogoutJSON(t *testing.T) {
	e := newEnv(t)
	var res logoutResult
	r := e.ok("logout", "--json")
	require.NoError(t, json.Unmarshal([]byte(r.stdout), &res))
	require.Equal(t, logoutResult{}, res, "not signed in")

	e.login()
	r = e.ok("logout", "--json")
	require.NoError(t, json.Unmarshal([]byte(r.stdout), &res))
	require.Equal(t, logoutResult{URL: e.inst.URL, SignedOut: true}, res)
	require.Equal(t, ExitAuth, e.run("whoami").code)
}

// TestEveryCommandTakesJSON checks 08-local-tools.md: --json on every
// command.
func TestEveryCommandTakesJSON(t *testing.T) {
	root := &cobra.Command{Use: "hangar"}
	Register(root, Options{Version: "test"})
	var walk func(c *cobra.Command)
	var checked int
	var missing []string
	walk = func(c *cobra.Command) {
		for _, sub := range c.Commands() {
			if sub.Hidden || sub.Name() == "help" || sub.Name() == "completion" {
				continue
			}
			if sub.Runnable() {
				checked++
				if sub.Flag("json") == nil {
					missing = append(missing, sub.CommandPath())
				}
			}
			walk(sub)
		}
	}
	walk(root)
	require.Empty(t, missing, "commands without --json")
	require.Greater(t, checked, 20)
}
