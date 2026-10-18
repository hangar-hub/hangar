package main

import (
	"errors"

	"github.com/spf13/cobra"

	"github.com/hangar-hub/hangar/internal/config"
)

// referenceField is one key in the --json output of hangar config reference.
type referenceField struct {
	Key         string `json:"key"`
	Env         string `json:"env"`
	Type        string `json:"type"`
	Default     string `json:"default"`
	Description string `json:"description"`
	Secret      bool   `json:"secret"`
}

// validation is the --json output of hangar config validate.
type validation struct {
	Valid    bool     `json:"valid"`
	Problems []string `json:"problems"`
}

func newConfigCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "config", Short: "Work with the configuration"}
	var refJSON bool
	reference := &cobra.Command{
		Use:   "reference",
		Short: "Print every configuration key with its type, default and description",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !refJSON {
				return config.WriteReference(cmd.OutOrStdout())
			}
			fields := []referenceField{}
			for _, f := range config.Fields() {
				def := f.Default
				// As in the Markdown reference, secret defaults are not shown.
				if f.Secret && def != `""` {
					def = "(secret)"
				}
				fields = append(fields, referenceField{Key: f.Key, Env: f.Env, Type: f.Type, Default: def, Description: f.Description, Secret: f.Secret})
			}
			return writeJSON(cmd.OutOrStdout(), fields)
		},
	}
	addJSONFlag(reference, &refJSON)
	cmd.AddCommand(reference)

	var flags configFlags
	var valJSON bool
	validate := &cobra.Command{
		Use:   "validate",
		Short: "Check the configuration and list every problem",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := flags.load("")
			if err == nil {
				err = cfg.Validate()
			}
			if valJSON {
				var problems config.Errors
				if err != nil && !errors.As(err, &problems) {
					return err
				}
				if jerr := writeJSON(cmd.OutOrStdout(), validation{Valid: err == nil, Problems: append([]string{}, problems...)}); jerr != nil {
					return jerr
				}
				return err
			}
			if err != nil {
				return err
			}
			cmd.Println("The configuration is valid.")
			return nil
		},
	}
	flags.register(validate)
	addJSONFlag(validate, &valJSON)
	cmd.AddCommand(validate)
	return cmd
}
