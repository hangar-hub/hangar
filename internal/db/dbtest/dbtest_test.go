package dbtest_test

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/db"
	"github.com/jamesagarside/hangar/internal/db/dbtest"
)

func TestHelpersGiveMigratedDatabases(t *testing.T) {
	ctx := context.Background()
	d := dbtest.SQLite(t)
	st, err := d.Schema(ctx)
	require.NoError(t, err)
	require.True(t, st.UpToDate())
	require.True(t, d.IsSQLite())
	require.NotEmpty(t, dbtest.SQLiteDSN(t))

	names := []string{}
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		names = append(names, d.Driver())
		_, err := d.NewInsert().Model(&db.Setting{Key: "k", Value: []byte(`true`)}).Exec(ctx)
		require.NoError(t, err)
	})
	require.Contains(t, names, db.SQLite)
	if dbtest.PostgresEnabled() {
		require.Contains(t, names, db.Postgres)
		pg := dbtest.Postgres(t)
		require.Equal(t, db.Postgres, pg.Driver())
	}
}

func TestPostgresEnabledReadsTheEnvironment(t *testing.T) {
	t.Setenv(dbtest.EnvPostgres, "")
	t.Setenv(dbtest.EnvPostgresDSN, "")
	require.False(t, dbtest.PostgresEnabled())
	t.Setenv(dbtest.EnvPostgres, "1")
	require.True(t, dbtest.PostgresEnabled())
	require.Len(t, dbtest.Drivers(), 2)
	_ = os.Unsetenv(dbtest.EnvPostgres)
}

// TestDatabasesFromTheTemplateAreSeparate checks that every database
// copied from a template is migrated and that writes to one do not reach
// another, on each driver.
func TestDatabasesFromTheTemplateAreSeparate(t *testing.T) {
	ctx := context.Background()
	for _, drv := range dbtest.Drivers() {
		t.Run(drv.Name, func(t *testing.T) {
			a, b := drv.Open(t), drv.Open(t)
			for _, d := range []*db.DB{a, b} {
				st, err := d.Schema(ctx)
				require.NoError(t, err)
				require.True(t, st.UpToDate(), "pending migrations: %v", st.Pending)
			}
			_, err := a.NewInsert().Model(&db.Setting{Key: "only-in-a", Value: []byte(`1`)}).Exec(ctx)
			require.NoError(t, err)
			n, err := b.NewSelect().Model((*db.Setting)(nil)).Where("key = ?", "only-in-a").Count(ctx)
			require.NoError(t, err)
			require.Zero(t, n)
			c := drv.Open(t)
			n, err = c.NewSelect().Model((*db.Setting)(nil)).Where("key = ?", "only-in-a").Count(ctx)
			require.NoError(t, err)
			require.Zero(t, n, "a write reached the template")
		})
	}
}

// TestPostgresDSNIsEmpty checks that PostgresDSN gives an unmigrated
// database, for tests that run a binary which migrates it.
func TestPostgresDSNIsEmpty(t *testing.T) {
	dbtest.RequirePostgres(t)
	ctx := context.Background()
	d, err := db.Open(ctx, db.Options{Driver: db.Postgres, DSN: dbtest.PostgresDSN(t)})
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	st, err := d.Schema(ctx)
	require.NoError(t, err)
	require.Empty(t, st.Current)
	require.NotEmpty(t, st.Pending)
}
