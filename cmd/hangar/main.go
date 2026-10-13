// Command hangar is the single Hangar binary: `hangar server` and
// `hangar worker` run an Instance, and the other commands manage it.
package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"
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
	exitError = 1
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
		var ue usageError
		if errors.As(err, &ue) {
			fmt.Fprintln(os.Stderr, "Error:", err)
			return exitUsage
		}
		fmt.Fprintln(os.Stderr, "Error:", err)
		return exitError
	}
	return exitOK
}

func newRootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:           "hangar",
		Short:         "Hangar: a self-hosted home for your arsenal of models",
		Long:          "Hangar is a self-hosted model and dataset hub, compatible with the Hugging Face Hub API, git, Ollama and LM Studio.",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return usageError{err} })
	root.AddCommand(
		newServerCommand("server", "Run the Instance: web UI, APIs and Jobs, as set by server.role", ""),
		newServerCommand("worker", "Run a worker that only runs Jobs, with health and metrics endpoints", "worker"),
		newConfigCommand(),
		newHealthcheckCommand(),
		newAdminCommand(),
		newVersionCommand(),
	)
	return root
}

func newVersionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := fmt.Fprintf(cmd.OutOrStdout(), "hangar %s (commit %s, built %s)\n", version, commit, date)
			return err
		},
	}
}
