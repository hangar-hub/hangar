// Command hangar is the single Hangar binary: `hangar server` and
// `hangar worker` run an Instance, and the other commands manage it.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/jamesagarside/hangar/internal/blob"
	"github.com/jamesagarside/hangar/internal/blob/backends"
	"github.com/jamesagarside/hangar/internal/cli"
	"github.com/jamesagarside/hangar/internal/config"
)

// Set at build time with -ldflags "-X main.version=... -X main.commit=... -X main.date=...".
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

// Exit codes from docs/spec/08-local-tools.md. Client commands (internal/cli)
// add 3 for not found and 4 for auth errors.
const (
	exitOK    = 0
	exitUsage = 2
)

type usageError struct{ error }

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	root := newRootCommand()
	root.SetArgs(args)
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		var ue usageError
		if errors.As(err, &ue) {
			return exitUsage
		}
		return cli.ExitCode(err)
	}
	return exitOK
}

// Help groups for the commands this package adds.
const (
	groupServer = "server"
	groupOther  = "other"
)

func newRootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:           "hangar",
		Short:         "Hangar: a self-hosted home for your arsenal of models",
		Long:          "Hangar is a self-hosted model and dataset hub, compatible with the Hugging Face Hub API, git, Ollama and LM Studio.",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return usageError{err} })
	// Every command has a help group, so none falls under cobra's
	// "Additional Commands" heading. The groups are added after the client
	// commands' group (internal/cli), which help lists first.
	for _, c := range []*cobra.Command{
		newServerCommand("server", "Run the Instance: web UI, APIs and Jobs, as set by server.role", ""),
		newServerCommand("worker", "Run a worker that only runs Jobs, with health and metrics endpoints", "worker"),
		newConfigCommand(),
		newHealthcheckCommand(),
		newAdminCommand(),
	} {
		c.GroupID = groupServer
		root.AddCommand(c)
	}
	versionCmd := newVersionCommand()
	versionCmd.GroupID = groupOther
	root.AddCommand(versionCmd)
	root.SetHelpCommandGroupID(groupOther)
	root.SetCompletionCommandGroupID(groupOther)
	// Client commands, the backup, restore, doctor and rotate-key admin
	// commands, and the hidden reference generators (internal/cli, W9).
	cli.Register(root, cli.Options{Version: version, Admin: cli.AdminDeps{
		OpenBlobStore: func(ctx context.Context, cfg *config.Config) (blob.Store, error) {
			return backends.Open(ctx, cfg.Storage)
		},
	}})
	root.AddGroup(&cobra.Group{ID: groupServer, Title: "Server commands:"}, &cobra.Group{ID: groupOther, Title: "Other commands:"})
	return root
}

// versionInfo is the --json output of hangar version.
type versionInfo struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Date    string `json:"date"`
}

func newVersionCommand() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print the version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if asJSON {
				return writeJSON(cmd.OutOrStdout(), versionInfo{Version: version, Commit: commit, Date: date})
			}
			_, err := fmt.Fprintf(cmd.OutOrStdout(), "hangar %s (commit %s, built %s)\n", version, commit, date)
			return err
		},
	}
	addJSONFlag(cmd, &asJSON)
	return cmd
}
