package main

import (
	"encoding/json"
	"io"

	"github.com/spf13/cobra"
)

// addJSONFlag adds the --json flag that every command takes
// (docs/spec/08-local-tools.md).
func addJSONFlag(cmd *cobra.Command, v *bool) {
	cmd.Flags().BoolVar(v, "json", false, "print JSON instead of text")
}

// writeJSON writes v as indented JSON.
func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
