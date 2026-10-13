package main

import (
	"github.com/spf13/cobra"

	"github.com/jamesagarside/hangar/internal/config"
)

func newConfigCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "config", Short: "Work with the configuration"}
	cmd.AddCommand(&cobra.Command{
		Use:   "reference",
		Short: "Print every configuration key with its type, default and description",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return config.WriteReference(cmd.OutOrStdout())
		},
	})
	var flags configFlags
	validate := &cobra.Command{
		Use:   "validate",
		Short: "Check the configuration and list every problem",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := flags.load("")
			if err != nil {
				return err
			}
			if err := cfg.Validate(); err != nil {
				return err
			}
			cmd.Println("The configuration is valid.")
			return nil
		},
	}
	flags.register(validate)
	cmd.AddCommand(validate)
	return cmd
}
