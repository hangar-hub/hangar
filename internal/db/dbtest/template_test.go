package dbtest

import (
	"context"
	"database/sql"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/hangar-hub/hangar/internal/db"
)

func TestTemplateNameFollowsTheMigrations(t *testing.T) {
	a, err := templateName()
	require.NoError(t, err)
	b, err := templateName()
	require.NoError(t, err)
	require.Equal(t, a, b)
	require.Regexp(t, `^hangar_template_[0-9a-f]{16}$`, a)
}

// TestEnsureTemplateReplacesAnUnfinishedBuild checks that a template build
// a killed process left behind is dropped and rebuilt, and that processes
// asking for the same template at once get one migrated database.
func TestEnsureTemplateReplacesAnUnfinishedBuild(t *testing.T) {
	RequirePostgres(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	admin := adminDSN(t)
	name := "hangar_template_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	adminDB := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(admin)))
	t.Cleanup(func() {
		_, _ = adminDB.ExecContext(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
		_, _ = adminDB.ExecContext(context.Background(), "DROP DATABASE IF EXISTS "+name+"_build WITH (FORCE)")
		_ = adminDB.Close()
	})

	// A build that stopped half way: the database exists with no schema.
	_, err := adminDB.ExecContext(ctx, "CREATE DATABASE "+name+"_build")
	require.NoError(t, err)

	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Go(func() { errs[i] = ensureTemplate(ctx, admin, name) })
	}
	wg.Wait()
	for _, err := range errs {
		require.NoError(t, err)
	}

	var builds int
	require.NoError(t, adminDB.QueryRowContext(ctx, "SELECT count(*) FROM pg_database WHERE datname = $1", name+"_build").Scan(&builds))
	require.Zero(t, builds)

	dsn, err := databaseDSN(admin, name)
	require.NoError(t, err)
	d, err := db.Open(ctx, db.Options{Driver: db.Postgres, DSN: dsn, MaxOpenConns: 1})
	require.NoError(t, err)
	st, err := d.Schema(ctx)
	require.NoError(t, err)
	require.True(t, st.UpToDate(), "pending migrations: %v", st.Pending)
	require.NoError(t, d.Close())
}
