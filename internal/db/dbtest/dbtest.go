// Package dbtest gives tests a fresh, migrated database: SQLite in a
// temporary directory, or a new database on a Postgres server.
//
// Postgres runs when HANGAR_TEST_POSTGRES_DSN points at a server (CI uses a
// service container, and `make test` starts one container for every
// package) or HANGAR_TEST_POSTGRES=1 (dbtest starts a testcontainers
// Postgres, which needs Docker). When Postgres is requested but cannot
// start, the test fails rather than skipping.
//
// Migrations run once per schema, not once per test. Each test process
// migrates one SQLite file and copies it for every test. On Postgres, the
// first process to need a schema migrates a template database named after
// a hash of the migration files, and every test database is created from
// it with CREATE DATABASE ... TEMPLATE. Editing a migration changes the
// hash, so a server shared between runs never serves an old schema.
package dbtest

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/hangar-hub/hangar/internal/db"
)

// Environment variables that turn on Postgres tests.
const (
	EnvPostgresDSN = "HANGAR_TEST_POSTGRES_DSN"
	EnvPostgres    = "HANGAR_TEST_POSTGRES"
	// PostgresImage is the image testcontainers starts.
	PostgresImage = "postgres:18-alpine"
)

// PostgresEnabled reports whether Postgres tests should run.
func PostgresEnabled() bool {
	return os.Getenv(EnvPostgresDSN) != "" || os.Getenv(EnvPostgres) == "1"
}

// RequirePostgres skips t, saying how to run it, unless Postgres tests
// are enabled. make test and make test-compat enable them, so nothing is
// skipped there; a plain go test without Docker reports the skip instead of
// a pass.
func RequirePostgres(t testing.TB) {
	t.Helper()
	if !PostgresEnabled() {
		t.Skipf("needs Postgres: set %s=1 (needs Docker) or %s; make test and make test-compat set it", EnvPostgres, EnvPostgresDSN)
	}
}

// SQLite returns a migrated SQLite database in a temporary directory,
// closed when the test ends.
func SQLite(t testing.TB) *db.DB {
	t.Helper()
	tmpl, err := sqliteTemplate()
	if err != nil {
		t.Fatalf("migrate SQLite template: %v", err)
	}
	path := filepath.Join(t.TempDir(), "hangar.db")
	if err := os.WriteFile(path, tmpl, 0o600); err != nil {
		t.Fatalf("copy SQLite template: %v", err)
	}
	return open(t, db.SQLite, path)
}

// SQLiteDSN returns the path of a fresh SQLite database file, without
// opening it, for tests that run a Hangar binary.
func SQLiteDSN(t testing.TB) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "hangar.db")
}

// Postgres returns a migrated database on the test Postgres server, dropped
// when the test ends. It fails the test if Postgres is not enabled or does
// not start.
func Postgres(t testing.TB) *db.DB {
	t.Helper()
	admin := adminDSN(t)
	tmpl, err := postgresTemplate(admin)
	if err != nil {
		t.Fatalf("migrate Postgres template: %v", err)
	}
	return open(t, db.Postgres, createDatabase(t, admin, " TEMPLATE "+tmpl))
}

// PostgresDSN creates an empty database on the test Postgres server and
// returns its URL. The database is dropped when the test ends.
func PostgresDSN(t testing.TB) string {
	t.Helper()
	return createDatabase(t, adminDSN(t), "")
}

func adminDSN(t testing.TB) string {
	t.Helper()
	if !PostgresEnabled() {
		t.Fatalf("Postgres tests are not enabled: set %s=1 (needs Docker) or %s", EnvPostgres, EnvPostgresDSN)
	}
	admin, err := serverDSN()
	if err != nil {
		t.Fatalf("start test Postgres: %v", err)
	}
	return admin
}

// createDatabase creates a database with a new name, dropped when the test
// ends, and returns its URL. with is appended to CREATE DATABASE.
func createDatabase(t testing.TB, admin, with string) string {
	t.Helper()
	name := "hangar_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	adminDB := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(admin)))
	defer adminDB.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := adminDB.ExecContext(ctx, "CREATE DATABASE "+name+with); err != nil {
		t.Fatalf("create test database: %v", err)
	}
	t.Cleanup(func() {
		c := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(admin)))
		defer c.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = c.ExecContext(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	})
	dsn, err := databaseDSN(admin, name)
	if err != nil {
		t.Fatalf("parse Postgres DSN: %v", err)
	}
	return dsn
}

// databaseDSN returns admin with its database replaced by name.
func databaseDSN(admin, name string) (string, error) {
	u, err := url.Parse(admin)
	if err != nil {
		return "", err
	}
	u.Path = "/" + name
	return u.String(), nil
}

// Driver names a database to run a test against.
type Driver struct {
	Name string
	Open func(t testing.TB) *db.DB
}

// Drivers returns SQLite and, when enabled, Postgres.
func Drivers() []Driver {
	out := []Driver{{Name: db.SQLite, Open: SQLite}}
	if PostgresEnabled() {
		out = append(out, Driver{Name: db.Postgres, Open: Postgres})
	}
	return out
}

// ForEachDriver runs fn as a subtest against a fresh database for each
// driver in Drivers. Without Postgres tests enabled, the postgres subtest
// is skipped with the reason.
func ForEachDriver(t *testing.T, fn func(t *testing.T, d *db.DB)) {
	t.Helper()
	for _, drv := range Drivers() {
		t.Run(drv.Name, func(t *testing.T) {
			fn(t, drv.Open(t))
		})
	}
	if !PostgresEnabled() {
		// Report the Postgres subtest as skipped rather than leave it out.
		t.Run(db.Postgres, func(t *testing.T) { RequirePostgres(t) })
	}
}

// open opens a database copied from a migrated template.
func open(t testing.TB, driver, dsn string) *db.DB {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	d, err := db.Open(ctx, db.Options{Driver: driver, DSN: dsn, MaxOpenConns: 8})
	if err != nil {
		t.Fatalf("open %s: %v", driver, err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

// migrateAt opens dsn, applies every migration and closes it again.
func migrateAt(ctx context.Context, driver, dsn string) error {
	d, err := db.Open(ctx, db.Options{Driver: driver, DSN: dsn, MaxOpenConns: 1})
	if err != nil {
		return err
	}
	if err := d.Migrate(ctx); err != nil {
		_ = d.Close()
		return err
	}
	return d.Close()
}

var (
	sqliteOnce  sync.Once
	sqliteBytes []byte
	sqliteErr   error
)

// sqliteTemplate returns the bytes of a migrated SQLite database, built
// once per process in a directory it then removes.
func sqliteTemplate() ([]byte, error) {
	sqliteOnce.Do(func() {
		dir, err := os.MkdirTemp("", "hangar-dbtest-")
		if err != nil {
			sqliteErr = err
			return
		}
		defer os.RemoveAll(dir)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		path := filepath.Join(dir, "hangar.db")
		// Closing the only connection checkpoints the WAL into the file and
		// removes it, so the file alone holds the schema.
		if err := migrateAt(ctx, db.SQLite, path); err != nil {
			sqliteErr = err
			return
		}
		if _, err := os.Stat(path + "-wal"); err == nil {
			sqliteErr = fmt.Errorf("the WAL of %s was not checkpointed on close", path)
			return
		}
		sqliteBytes, sqliteErr = os.ReadFile(path)
	})
	return sqliteBytes, sqliteErr
}

// templateName returns the name of the Postgres template database for the
// current migration files: "hangar_template_" and the first 16 hex digits
// of a SHA-256 over their names and contents.
func templateName() (string, error) {
	files, err := db.MigrationFiles(db.Postgres)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	err = fs.WalkDir(files, ".", func(path string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return err
		}
		b, err := fs.ReadFile(files, path)
		if err != nil {
			return err
		}
		fmt.Fprintf(h, "%s\x00%d\x00", path, len(b))
		h.Write(b)
		return nil
	})
	if err != nil {
		return "", err
	}
	return "hangar_template_" + hex.EncodeToString(h.Sum(nil))[:16], nil
}

var (
	templateMu    sync.Mutex
	templateReady = map[string]string{}
)

// postgresTemplate returns the name of a migrated template database on the
// server at admin, creating it if no process has yet. An advisory lock
// keeps two processes from building it at once, and the build happens
// under another name that is renamed only once migrated, so a process
// killed mid-build leaves nothing another process would use.
func postgresTemplate(admin string) (string, error) {
	templateMu.Lock()
	defer templateMu.Unlock()
	if name, ok := templateReady[admin]; ok {
		return name, nil
	}
	name, err := templateName()
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := ensureTemplate(ctx, admin, name); err != nil {
		return "", err
	}
	templateReady[admin] = name
	return name, nil
}

// ensureTemplate creates the migrated database name on the server at admin
// unless it exists.
func ensureTemplate(ctx context.Context, admin, name string) error {
	adminDB := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(admin)))
	defer adminDB.Close()
	conn, err := adminDB.Conn(ctx)
	if err != nil {
		return fmt.Errorf("connect to test Postgres: %w", err)
	}
	defer conn.Close()
	sum := sha256.Sum256([]byte(name))
	key := int64(binary.BigEndian.Uint64(sum[:8]))
	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock($1)", key); err != nil {
		return fmt.Errorf("lock template: %w", err)
	}
	defer func() { _, _ = conn.ExecContext(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", key) }()
	var exists bool
	if err := conn.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)", name).Scan(&exists); err != nil {
		return fmt.Errorf("look up template: %w", err)
	}
	if !exists {
		build := name + "_build"
		if _, err := conn.ExecContext(ctx, "DROP DATABASE IF EXISTS "+build+" WITH (FORCE)"); err != nil {
			return fmt.Errorf("drop unfinished template: %w", err)
		}
		if _, err := conn.ExecContext(ctx, "CREATE DATABASE "+build); err != nil {
			return fmt.Errorf("create template: %w", err)
		}
		dsn, err := databaseDSN(admin, build)
		if err != nil {
			return err
		}
		if err := migrateAt(ctx, db.Postgres, dsn); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, "ALTER DATABASE "+build+" RENAME TO "+name); err != nil {
			return fmt.Errorf("finish template: %w", err)
		}
	}
	return nil
}

var (
	serverOnce sync.Once
	serverURL  string
	serverErr  error
)

// serverDSN returns the admin URL of the shared test Postgres server,
// starting a container on first use. Testcontainers' reaper removes the
// container when the test process exits.
func serverDSN() (string, error) {
	serverOnce.Do(func() {
		if dsn := os.Getenv(EnvPostgresDSN); dsn != "" {
			serverURL = dsn
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		c, err := tcpostgres.Run(ctx, PostgresImage,
			tcpostgres.WithDatabase("postgres"),
			tcpostgres.WithUsername("hangar"),
			tcpostgres.WithPassword("hangar"),
			testcontainers.WithWaitStrategy(
				wait.ForLog("database system is ready to accept connections").
					WithOccurrence(2).WithStartupTimeout(2*time.Minute)),
		)
		if err != nil {
			serverErr = fmt.Errorf("run %s: %w", PostgresImage, err)
			return
		}
		serverURL, serverErr = c.ConnectionString(ctx, "sslmode=disable")
	})
	return serverURL, serverErr
}
