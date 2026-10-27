package db_test

import (
	"context"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun/migrate"

	"github.com/hangar-hub/hangar/internal/db"
	"github.com/hangar-hub/hangar/internal/db/dbtest"
)

// forEachEmptyDB runs fn against an empty, unmigrated database of each
// driver.
func forEachEmptyDB(t *testing.T, fn func(t *testing.T, d *db.DB)) {
	t.Helper()
	t.Run(db.SQLite, func(t *testing.T) {
		d, err := db.Open(context.Background(), db.Options{Driver: db.SQLite, DSN: filepath.Join(t.TempDir(), "x.db")})
		require.NoError(t, err)
		t.Cleanup(func() { _ = d.Close() })
		fn(t, d)
	})
	t.Run(db.Postgres, func(t *testing.T) {
		dbtest.RequirePostgres(t)
		d, err := db.Open(context.Background(), db.Options{Driver: db.Postgres, DSN: dbtest.PostgresDSN(t)})
		require.NoError(t, err)
		t.Cleanup(func() { _ = d.Close() })
		fn(t, d)
	})
}

// migrationFiles returns the driver's migration files as a map, with the
// file names changed by rename.
func migrationFiles(t *testing.T, driver string, rename func(string) string) fstest.MapFS {
	t.Helper()
	files, err := db.MigrationFiles(driver)
	require.NoError(t, err)
	out := fstest.MapFS{}
	require.NoError(t, fs.WalkDir(files, ".", func(path string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return err
		}
		b, err := fs.ReadFile(files, path)
		if err != nil {
			return err
		}
		out[rename(path)] = &fstest.MapFile{Data: b}
		return nil
	}))
	return out
}

type appliedMigration struct {
	Name    string `bun:"name"`
	GroupID int64  `bun:"group_id"`
}

func applied(t *testing.T, d *db.DB) map[string]int64 {
	t.Helper()
	var rows []appliedMigration
	require.NoError(t, d.NewSelect().Table("bun_migrations").Column("name", "group_id").Scan(context.Background(), &rows))
	out := map[string]int64{}
	for _, r := range rows {
		_, dup := out[r.Name]
		require.False(t, dup, "migration %s is recorded twice", r.Name)
		out[r.Name] = r.GroupID
	}
	return out
}

// TestMigrationFilesAreTransactional checks every migration is named
// NNNN_name.tx.up.sql, so bun runs it in a transaction and a migrator
// killed part of the way through leaves nothing to repair by hand.
func TestMigrationFilesAreTransactional(t *testing.T) {
	for _, driver := range []string{db.SQLite, db.Postgres} {
		files, err := db.MigrationFiles(driver)
		require.NoError(t, err)
		n := 0
		require.NoError(t, fs.WalkDir(files, ".", func(path string, e fs.DirEntry, err error) error {
			if err != nil || e.IsDir() {
				return err
			}
			n++
			require.True(t, strings.HasSuffix(path, ".tx.up.sql"), "%s migration %s must end in .tx.up.sql", driver, path)
			return nil
		}))
		require.NotZero(t, n)
	}
}

// TestMigrateKeepsMigrationsAppliedByEarlierVersions migrates a database
// the way Hangar 1.2.3 did, with migration files not named .tx, and checks
// that the current files are recognised as the same migrations: nothing is
// applied again and the recorded rows are kept.
func TestMigrateKeepsMigrationsAppliedByEarlierVersions(t *testing.T) {
	forEachEmptyDB(t, func(t *testing.T, d *db.DB) {
		ctx := context.Background()
		released := map[string]bool{"0001": true, "0002": true, "0003": true}
		old := migrationFiles(t, d.Driver(), func(p string) string { return strings.Replace(p, ".tx.up.sql", ".up.sql", 1) })
		for p := range old {
			if !released[p[:4]] {
				delete(old, p)
			}
		}
		m := migrate.NewMigrations()
		require.NoError(t, m.Discover(old))
		mg := migrate.NewMigrator(d.DB, m, migrate.WithMarkAppliedOnSuccess(true))
		require.NoError(t, mg.Init(ctx))
		_, err := mg.Migrate(ctx)
		require.NoError(t, err)
		before := applied(t, d)
		require.Equal(t, map[string]int64{"0001": 1, "0002": 1, "0003": 1}, before)

		require.NoError(t, d.Migrate(ctx), "the released migrations must not run again")
		after := applied(t, d)
		for name, group := range before {
			require.Equal(t, group, after[name], "migration %s keeps its record", name)
		}
		st, err := d.Schema(ctx)
		require.NoError(t, err)
		require.True(t, st.UpToDate(), "pending: %v", st.Pending)
		require.Empty(t, st.Unknown)
	})
}

// TestMigrationIsAllOrNothing runs a migration that fails after its first
// statement. Its first statement is rolled back and it is not recorded, so
// the fixed migration then applies from the start.
func TestMigrationIsAllOrNothing(t *testing.T) {
	forEachEmptyDB(t, func(t *testing.T, d *db.DB) {
		ctx := context.Background()
		files := migrationFiles(t, d.Driver(), func(p string) string { return p })
		files["9990_half_done.tx.up.sql"] = &fstest.MapFile{Data: []byte(
			"CREATE TABLE half_done (id integer PRIMARY KEY);\n--bun:split\nINSERT INTO no_such_table VALUES (1);\n")}
		require.Error(t, d.MigrateFS(ctx, files))
		var n int
		require.Error(t, d.NewSelect().Table("half_done").ColumnExpr("count(*)").Scan(ctx, &n), "the failed migration's table was rolled back")
		require.NotContains(t, applied(t, d), "9990")

		files["9990_half_done.tx.up.sql"] = &fstest.MapFile{Data: []byte(
			"CREATE TABLE half_done (id integer PRIMARY KEY);\n--bun:split\nINSERT INTO half_done VALUES (1);\n")}
		require.NoError(t, d.MigrateFS(ctx, files))
		require.NoError(t, d.NewSelect().Table("half_done").ColumnExpr("count(*)").Scan(ctx, &n))
		require.Equal(t, 1, n)
		require.Contains(t, applied(t, d), "9990")
		require.NoError(t, d.MigrateFS(ctx, files), "an applied migration is not run again")
	})
}

// TestMigrateRefusesANewerSchema checks that a binary refuses a database
// that a newer version of Hangar migrated, instead of running against a
// schema it does not know.
func TestMigrateRefusesANewerSchema(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		ctx := context.Background()
		_, err := d.ExecContext(ctx, "INSERT INTO bun_migrations (name, group_id) VALUES ('9999', 99)")
		require.NoError(t, err)

		err = d.Migrate(ctx)
		require.ErrorIs(t, err, db.ErrSchemaNewer)
		require.Contains(t, err.Error(), "9999")

		st, err := d.Schema(ctx)
		require.NoError(t, err)
		require.Equal(t, []string{"9999"}, st.Unknown)
		require.Equal(t, "9999", st.Current)
		require.ErrorIs(t, st.CheckNotNewer(), db.ErrSchemaNewer)
	})
}

// TestRepoPresentBytesMigration checks that 0011 moves what size_bytes
// counted, git data plus the LFS objects held, to present_bytes, and adds
// the LFS objects a Partial mirror does not hold to size_bytes.
func TestRepoPresentBytesMigration(t *testing.T) {
	forEachEmptyDB(t, func(t *testing.T, d *db.DB) {
		ctx := context.Background()
		files := migrationFiles(t, d.Driver(), func(p string) string { return p })
		before := fstest.MapFS{}
		for p, f := range files {
			if p < "0011" {
				before[p] = f
			}
		}
		require.NoError(t, d.MigrateFS(ctx, before))

		ns, mirror, local := uuid.NewString(), uuid.NewString(), uuid.NewString()
		_, err := d.ExecContext(ctx, "INSERT INTO namespaces (id, name, kind) VALUES (?, 'acme', 'org')", ns)
		require.NoError(t, err)
		type lfsObject struct {
			size    int64
			present bool
		}
		// The Partial mirror holds 100 bytes of git data and one of its two
		// LFS objects; the local Repo holds everything.
		for _, r := range []struct {
			id, name  string
			size, lfs int64
			objects   []lfsObject
		}{
			{mirror, "mirror", 1100, 1000, []lfsObject{{1000, true}, {5000, false}}},
			{local, "local", 300, 200, []lfsObject{{200, true}}},
		} {
			_, err := d.ExecContext(ctx, "INSERT INTO repos (id, namespace_id, name, type, visibility, size_bytes, lfs_bytes) VALUES (?, ?, ?, 'model', 'public', ?, ?)",
				r.id, ns, r.name, r.size, r.lfs)
			require.NoError(t, err)
			for i, o := range r.objects {
				_, err := d.ExecContext(ctx, "INSERT INTO repo_lfs (repo_id, oid, size, present) VALUES (?, ?, ?, ?)", r.id, r.name+strconv.Itoa(i), o.size, o.present)
				require.NoError(t, err)
			}
		}

		require.NoError(t, d.MigrateFS(ctx, files))
		for _, want := range []struct {
			id                     string
			size, present, lfsHeld int64
		}{{mirror, 6100, 1100, 1000}, {local, 300, 300, 200}} {
			var size, present, lfs int64
			require.NoError(t, d.NewSelect().Table("repos").Column("size_bytes", "present_bytes", "lfs_bytes").
				Where("id = ?", want.id).Scan(ctx, &size, &present, &lfs))
			require.Equal(t, want.size, size, "size_bytes is the total")
			require.Equal(t, want.present, present, "present_bytes is what size_bytes counted")
			require.Equal(t, want.lfsHeld, lfs, "lfs_bytes does not change")
		}
	})
}
