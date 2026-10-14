package main

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/jamesagarside/hangar/internal/config"
	"github.com/jamesagarside/hangar/internal/db"
)

func newAdminCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "admin",
		Short: "Administer an Instance directly through its database and Blob store",
	}
	var flags configFlags
	var asJSON bool
	migrate := &cobra.Command{
		Use:   "migrate",
		Short: "Apply database migrations",
		Long:  "Applies every pending migration. The Helm chart runs it in a pre-install and pre-upgrade Job.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := flags.load("")
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Minute)
			defer cancel()
			st, err := migrateDatabase(ctx, cfg)
			if err != nil {
				return err
			}
			if asJSON {
				return writeJSON(cmd.OutOrStdout(), schemaVersion{Current: st.Current, Expected: st.Expected, Pending: append([]string{}, st.Pending...)})
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "The database schema is at version %s.\n", st.Current)
			return err
		},
	}
	flags.register(migrate)
	addJSONFlag(migrate, &asJSON)
	cmd.AddCommand(migrate, newCreateUserCommand(), newResetPasswordCommand(), newGCCommand(), newReindexCommand())
	return cmd
}

// schemaVersion is the --json output of hangar admin migrate.
type schemaVersion struct {
	Current  string   `json:"current"`
	Expected string   `json:"expected"`
	Pending  []string `json:"pending"`
}

func openDatabase(ctx context.Context, cfg *config.Config, tracing bool) (*db.DB, error) {
	return db.Open(ctx, db.Options{Driver: cfg.Database.Driver, DSN: cfg.Database.DSN, MaxOpenConns: cfg.Database.MaxOpenConns, Tracing: tracing})
}

func migrateDatabase(ctx context.Context, cfg *config.Config) (db.SchemaStatus, error) {
	d, err := openDatabase(ctx, cfg, false)
	if err != nil {
		return db.SchemaStatus{}, err
	}
	defer d.Close()
	if err := d.Migrate(ctx); err != nil {
		return db.SchemaStatus{}, err
	}
	return d.Schema(ctx)
}
