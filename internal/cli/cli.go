// Package cli holds the hangar client commands (login, pull, push, import,
// lmstudio, ollama, jobs and the rest), the admin commands that work on an
// Instance's database and Blob store directly (backup, restore, doctor,
// rotate-key), and the generators for the CLI and metrics references
// (docs/spec/08-local-tools.md, 12-packaging-and-release.md).
//
// Client commands talk to an Instance through the Hub API and the native
// API, so they work against any Hangar the user can reach. Every command
// prints a table by default and JSON with --json, and exits with 0 on
// success, 1 on an error, 2 on a usage error, 3 when something is not
// found and 4 on an authentication error.
package cli

import (
	"net/http"
	"os"

	"github.com/spf13/cobra"
)

// Options configure the commands Register adds.
type Options struct {
	// Version is the hangar version, sent in the User-Agent.
	Version string
	// Admin gives the admin commands access to the Blob store.
	Admin AdminDeps
	// HTTPClient is used for every request; nil uses a client with
	// connection timeouts but no overall timeout.
	HTTPClient *http.Client
}

// Register adds the client commands, the admin commands and the hidden
// reference generator to root. If root already has an `admin` command, the
// admin commands are added to it.
func Register(root *cobra.Command, opts Options) {
	if opts.HTTPClient == nil {
		opts.HTTPClient = newHTTPClient()
	}
	if opts.Version == "" {
		opts.Version = "dev"
	}
	base := &app{opts: opts, http: opts.HTTPClient, stdout: os.Stdout, stderr: os.Stderr}
	for _, c := range []*cobra.Command{
		base.loginCommand(), base.logoutCommand(), base.whoamiCommand(), base.envCommand(),
		base.searchCommand(), base.infoCommand(), base.pullCommand(), base.pushCommand(),
		base.importCommand(), base.lmstudioCommand(), base.ollamaCommand(), base.jobsCommand(),
	} {
		c.GroupID = groupClient
		root.AddCommand(c)
	}
	if !root.ContainsGroup(groupClient) {
		root.AddGroup(&cobra.Group{ID: groupClient, Title: "Client commands:"})
	}
	admin := findCommand(root, "admin")
	if admin == nil {
		admin = &cobra.Command{Use: "admin", Short: "Administer an Instance directly through its database and Blob store"}
		root.AddCommand(admin)
	}
	admin.AddCommand(base.backupCommand(), base.restoreCommand(), base.doctorCommand(), base.rotateKeyCommand())
	root.AddCommand(referenceCommand(root))
}

const groupClient = "client"

func findCommand(root *cobra.Command, name string) *cobra.Command {
	for _, c := range root.Commands() {
		if c.Name() == name {
			return c
		}
	}
	return nil
}

// with returns a copy of the app that writes to the command's streams, so
// tests can capture output with SetOut and SetErr.
func (a *app) with(cmd *cobra.Command) *app {
	c := *a
	c.stdout = cmd.OutOrStdout()
	c.stderr = cmd.ErrOrStderr()
	return &c
}

// clientFlags adds the flags every client command takes.
func (a *app) clientFlags(cmd *cobra.Command) {
	f := cmd.PersistentFlags()
	f.StringVar(&a.url, "url", "", "Instance URL (default: from hangar login, or "+EnvURL+")")
	f.StringVar(&a.token, "token", "", "Token to use instead of the saved login (or "+EnvToken+")")
	f.BoolVar(&a.json, "json", false, "print JSON instead of text")
}

// args returns a cobra argument check whose errors exit with code 2.
func args(check cobra.PositionalArgs) cobra.PositionalArgs {
	return func(cmd *cobra.Command, a []string) error {
		if err := check(cmd, a); err != nil {
			return &ExitError{Code: ExitUsage, Err: err}
		}
		return nil
	}
}
