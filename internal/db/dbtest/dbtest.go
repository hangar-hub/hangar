// Package dbtest gives tests a fresh, migrated database: SQLite in a
// temporary directory, or a new database on a Postgres server.
//
// Postgres runs when HANGAR_TEST_POSTGRES_DSN points at a server (CI uses a
// service container) or HANGAR_TEST_POSTGRES=1 (dbtest starts a
// testcontainers Postgres, which needs Docker). `make test` sets
// HANGAR_TEST_POSTGRES=1. When Postgres is requested but cannot start, the
// test fails rather than skipping.
package dbtest

import (
	"context"
	"database/sql"
	"fmt"
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

	"github.com/jamesagarside/hangar/internal/db"
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

// SQLite returns a migrated SQLite database in a temporary directory,
// closed when the test ends.
func SQLite(t testing.TB) *db.DB {
	t.Helper()
	return open(t, db.SQLite, filepath.Join(t.TempDir(), "hangar.db"))
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
	return open(t, db.Postgres, PostgresDSN(t))
}

// PostgresDSN creates an empty database on the test Postgres server and
// returns its URL. The database is dropped when the test ends.
func PostgresDSN(t testing.TB) string {
	t.Helper()
	if !PostgresEnabled() {
		t.Fatalf("Postgres tests are not enabled: set %s=1 (needs Docker) or %s", EnvPostgres, EnvPostgresDSN)
	}
	admin, err := serverDSN()
	if err != nil {
		t.Fatalf("start test Postgres: %v", err)
	}
	name := "hangar_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	adminDB := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(admin)))
	defer adminDB.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := adminDB.ExecContext(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create test database: %v", err)
	}
	t.Cleanup(func() {
		c := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(admin)))
		defer c.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = c.ExecContext(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	})
	u, err := url.Parse(admin)
	if err != nil {
		t.Fatalf("parse Postgres DSN: %v", err)
	}
	u.Path = "/" + name
	return u.String()
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
// driver in Drivers.
func ForEachDriver(t *testing.T, fn func(t *testing.T, d *db.DB)) {
	t.Helper()
	if !PostgresEnabled() {
		t.Logf("Postgres subtests not registered: set %s=1 or %s to run them", EnvPostgres, EnvPostgresDSN)
	}
	for _, drv := range Drivers() {
		t.Run(drv.Name, func(t *testing.T) {
			fn(t, drv.Open(t))
		})
	}
}

func open(t testing.TB, driver, dsn string) *db.DB {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	d, err := db.Open(ctx, db.Options{Driver: driver, DSN: dsn, MaxOpenConns: 8})
	if err != nil {
		t.Fatalf("open %s: %v", driver, err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("migrate %s: %v", driver, err)
	}
	return d
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
