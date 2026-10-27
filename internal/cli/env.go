package cli

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

func (a *app) envCommand() *cobra.Command {
	var shell string
	cmd := &cobra.Command{
		Use:   "env",
		Short: "Print environment variables that point Python tools at the Instance",
		Long: `Prints HF_ENDPOINT and HF_TOKEN for the current Instance, so huggingface_hub,
the hf CLI, transformers, datasets and other Hugging Face tools use Hangar.

It also prints DISABLE_SAFETENSORS_CONVERSION=1. Without it, transformers can
send the Repo ID of a model that has only pickle weights, and any Token passed
to it, to a conversion service on huggingface.co.`,
		Example: `  eval "$(hangar env)"
  hangar env --shell fish | source
  hangar env --shell powershell | Invoke-Expression`,
		Args: args(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			a := a.with(cmd)
			c, err := a.client()
			if err != nil {
				return err
			}
			vars := [][2]string{{"HF_ENDPOINT", c.base.String()}}
			if c.token != "" {
				vars = append(vars, [2]string{"HF_TOKEN", c.token})
			} else {
				a.logf("Not signed in, so HF_TOKEN is not set. Run hangar login to add it.\n")
			}
			vars = append(vars, [2]string{"DISABLE_SAFETENSORS_CONVERSION", "1"})
			if a.json {
				m := map[string]string{}
				for _, v := range vars {
					m[v[0]] = v[1]
				}
				return a.printJSON(m)
			}
			if shell == "" {
				shell = defaultShell()
			}
			for _, v := range vars {
				line, err := exportLine(shell, v[0], v[1])
				if err != nil {
					return err
				}
				a.printf("%s\n", line)
			}
			return nil
		},
	}
	a.clientFlags(cmd)
	cmd.Flags().StringVar(&shell, "shell", "", "shell syntax: bash, zsh, fish or powershell (default: from $SHELL)")
	_ = cmd.RegisterFlagCompletionFunc("shell", values("bash", "zsh", "fish", "powershell"))
	return cmd
}

// defaultShell guesses the shell from $SHELL, falling back to bash, or to
// PowerShell on Windows.
func defaultShell() string {
	if s := os.Getenv("SHELL"); s != "" {
		return filepath.Base(s)
	}
	if os.Getenv("PSModulePath") != "" {
		return "powershell"
	}
	return "bash"
}

// exportLine formats one variable for a shell.
func exportLine(shell, name, value string) (string, error) {
	switch shell {
	case "bash", "zsh", "sh", "dash", "ksh":
		return "export " + name + "=" + posixQuote(value), nil
	case "fish":
		return "set -gx " + name + " " + fishQuote(value) + ";", nil
	case "powershell", "pwsh":
		return "$Env:" + name + " = '" + strings.ReplaceAll(value, "'", "''") + "'", nil
	}
	return "", usagef("--shell must be bash, zsh, fish or powershell, not %q", shell)
}

// fishQuote single-quotes a value for fish, which escapes \ and ' inside quotes.
func fishQuote(s string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s) + "'"
}

// posixQuote single-quotes a value for POSIX shells.
func posixQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
