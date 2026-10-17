package main

import (
	"context"
	"fmt"
	"io"
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
	var waitForDatabase time.Duration
	migrate := &cobra.Command{
		Use:   "migrate",
		Short: "Apply database migrations",
		Long: "Applies every pending migration. The Helm chart runs it in a pre-install and pre-upgrade Job, or in an init container of each web and worker Pod. " +
			"Migrators that start together wait for one another. With --wait-for-database, it also waits for a Postgres server that does not answer yet, such as one whose Pod is still starting.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := flags.load("")
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Minute)
			defer cancel()
			st, err := migrateDatabase(ctx, cfg, waitForDatabase, cmd.ErrOrStderr())
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
	migrate.Flags().DurationVar(&waitForDatabase, "wait-for-database", 0,
		"how long to keep trying to connect while a Postgres database does not answer (0 fails at once)")
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

func migrateDatabase(ctx context.Context, cfg *config.Config, waitForDatabase time.Duration, status io.Writer) (db.SchemaStatus, error) {
	d, err := openDatabaseWaiting(ctx, cfg, waitForDatabase, status)
	if err != nil {
		return db.SchemaStatus{}, err
	}
	defer d.Close()
	if err := d.Migrate(ctx); err != nil {
		return db.SchemaStatus{}, err
	}
	return d.Schema(ctx)
}

// Bounds of the retry interval in openDatabaseWaiting.
const (
	databaseRetryFirst = time.Second
	databaseRetryMax   = 5 * time.Second
)

// openDatabaseWaiting opens the database, and while a Postgres server does
// not answer, keeps trying for up to wait, writing one line to status per
// attempt. A migrate init container uses it so that it waits for a database
// Pod that is still starting instead of exiting into the kubelet's growing
// restart back-off. SQLite is never retried: its errors do not go away.
func openDatabaseWaiting(ctx context.Context, cfg *config.Config, wait time.Duration, status io.Writer) (*db.DB, error) {
	deadline := time.Now().Add(wait)
	retry := databaseRetryFirst
	for {
		d, err := openDatabase(ctx, cfg, false)
		if err == nil || wait <= 0 || cfg.Database.Driver != db.Postgres || ctx.Err() != nil {
			return d, err
		}
		left := time.Until(deadline)
		if left <= 0 {
			return nil, fmt.Errorf("the database did not answer within %s: %w", wait, err)
		}
		_, _ = fmt.Fprintf(status, "Waiting for the database: %v\n", err)
		t := time.NewTimer(min(retry, left))
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, fmt.Errorf("wait for the database: %w (last error: %w)", context.Cause(ctx), err)
		case <-t.C:
		}
		retry = min(retry*2, databaseRetryMax)
	}
}
