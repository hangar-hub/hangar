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
