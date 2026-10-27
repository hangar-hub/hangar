// Command hangar is the single Hangar binary: `hangar server` and
// `hangar worker` run an Instance, and the other commands manage it.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime/debug"
	"strings"

	"github.com/spf13/cobra"

	"github.com/hangar-hub/hangar/internal/blob"
	"github.com/hangar-hub/hangar/internal/blob/backends"
	"github.com/hangar-hub/hangar/internal/cli"
	"github.com/hangar-hub/hangar/internal/config"
)

// Set at build time with -ldflags "-X main.version=... -X main.commit=... -X main.date=...".
// A build without them, such as `go install ...@v1.2.3`, takes what it can
// from the build information Go embeds instead (see fromBuildInfo).
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

// fromBuildInfo fills in the version, commit and date that -ldflags left at
// their defaults, from the build information Go embeds in every binary.
// `go install github.com/hangar-hub/hangar/cmd/hangar@v1.2.3` records the
// module version, v1.2.3. A build in a git checkout records the commit and
// its time (vcs.revision, vcs.time) and, from Go 1.24, a version derived
// from the nearest tag. Values set with -ldflags always win, and "(devel)",
// which test binaries and builds outside a module report, is ignored.
func fromBuildInfo(v, c, d string, info *debug.BuildInfo) (string, string, string) {
	if info == nil {
		return v, c, d
	}
	if mv := info.Main.Version; v == "dev" && mv != "" && mv != "(devel)" {
		// Release builds print the version without the v, as GoReleaser's
		// {{ .Version }} does.
		v = strings.TrimPrefix(mv, "v")
	}
	for _, s := range info.Settings {
		switch {
		case s.Key == "vcs.revision" && c == "none" && s.Value != "":
			// Release builds print the short commit, as git rev-parse --short does.
			c = s.Value[:min(len(s.Value), 7)]
		case s.Key == "vcs.time" && d == "unknown" && s.Value != "":
			d = s.Value
		}
	}
	return v, c, d
}

// Exit codes from docs/spec/08-local-tools.md. Client commands (internal/cli)
// add 3 for not found and 4 for auth errors.
const (
	exitOK    = 0
	exitUsage = 2
)

type usageError struct{ error }

func main() {
	info, _ := debug.ReadBuildInfo()
	version, commit, date = fromBuildInfo(version, commit, date, info)
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
	// Subcommands inherit the root's usage template, so this one template
	// keeps every help page's headings in sentence case (house style).
	root.SetUsageTemplate(usageTemplate)
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
	cli.SetDefaultCompletions(root)
	return root
}

// usageTemplate is cobra's default usage template (v1.10.2) with its
// headings in sentence case: "Available commands", "Additional commands" and
// "Global flags" instead of cobra's title case.
const usageTemplate = `Usage:{{if .Runnable}}
  {{.UseLine}}{{end}}{{if .HasAvailableSubCommands}}
  {{.CommandPath}} [command]{{end}}{{if gt (len .Aliases) 0}}

Aliases:
  {{.NameAndAliases}}{{end}}{{if .HasExample}}

Examples:
{{.Example}}{{end}}{{if .HasAvailableSubCommands}}{{$cmds := .Commands}}{{if eq (len .Groups) 0}}

Available commands:{{range $cmds}}{{if (or .IsAvailableCommand (eq .Name "help"))}}
  {{rpad .Name .NamePadding }} {{.Short}}{{end}}{{end}}{{else}}{{range $group := .Groups}}

{{.Title}}{{range $cmds}}{{if (and (eq .GroupID $group.ID) (or .IsAvailableCommand (eq .Name "help")))}}
  {{rpad .Name .NamePadding }} {{.Short}}{{end}}{{end}}{{end}}{{if not .AllChildCommandsHaveGroup}}

Additional commands:{{range $cmds}}{{if (and (eq .GroupID "") (or .IsAvailableCommand (eq .Name "help")))}}
  {{rpad .Name .NamePadding }} {{.Short}}{{end}}{{end}}{{end}}{{end}}{{end}}{{if .HasAvailableLocalFlags}}

Flags:
{{.LocalFlags.FlagUsages | trimTrailingWhitespaces}}{{end}}{{if .HasAvailableInheritedFlags}}

Global flags:
{{.InheritedFlags.FlagUsages | trimTrailingWhitespaces}}{{end}}{{if .HasHelpSubCommands}}

Additional help topics:{{range .Commands}}{{if .IsAdditionalHelpTopicCommand}}
  {{rpad .CommandPath .CommandPathPadding}} {{.Short}}{{end}}{{end}}{{end}}{{if .HasAvailableSubCommands}}

Use "{{.CommandPath}} [command] --help" for more information about a command.{{end}}
`

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
