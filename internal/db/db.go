// Package db opens Hangar's database (SQLite or Postgres) with bun, runs the
// per-dialect SQL migrations, and provides a transaction helper, error
// classification and the model structs for every table.
//
// Packages own their tables: only the package that owns a table reads or
// writes it, and other packages go through that package's interface
// (docs/spec/02-architecture.md "Repository layout").
package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"embed"
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/dialect/sqlitedialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/uptrace/bun/driver/sqliteshim"
	"github.com/uptrace/bun/extra/bunotel"
	"github.com/uptrace/bun/migrate"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// Drivers.
const (
	SQLite   = "sqlite"
	Postgres = "postgres"
)

//go:embed migrations/postgres/*.sql migrations/sqlite/*.sql
var migrationFiles embed.FS

// DB is an open database.
type DB struct {
	*bun.DB
	driver string
	lock   *fileLock
	// pgConnector opens Postgres connections outside the pool, for the
	// migration lock.
	pgConnector driver.Connector
}

// Options configures Open.
type Options struct {
	// Driver is SQLite or Postgres.
	Driver string
	// DSN is a file path (SQLite) or a postgres:// URL.
	DSN string
	// MaxOpenConns limits open connections. Zero means 20.
	MaxOpenConns int
	// Tracing adds the bunotel query hook, which records statements without
	// parameter values.
	Tracing bool
}

// ErrLocked is returned when another process holds the SQLite database.
var ErrLocked = errors.New("database is in use by another Hangar process")

// Open connects to the database. For SQLite it creates the parent
// directory, takes an exclusive lock file next to the database so a second
// process cannot use it, and turns on WAL, foreign keys and a busy timeout.
func Open(ctx context.Context, opts Options) (*DB, error) {
	maxConns := opts.MaxOpenConns
	if maxConns <= 0 {
		maxConns = 20
	}
	var d *DB
	switch opts.Driver {
	case SQLite:
		path := sqlitePath(opts.DSN)
		var lock *fileLock
		if path != "" {
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				return nil, fmt.Errorf("create database directory: %w", err)
			}
			l, err := acquireLock(path + ".lock")
			if err != nil {
				return nil, err
			}
			lock = l
		}
		sqldb, err := sql.Open(sqliteshim.ShimName, sqliteDSN(opts.DSN))
		if err != nil {
			lock.release()
			return nil, fmt.Errorf("open sqlite: %w", err)
		}
		sqldb.SetMaxOpenConns(maxConns)
		sqldb.SetConnMaxLifetime(0)
		d = &DB{DB: bun.NewDB(sqldb, sqlitedialect.New()), driver: SQLite, lock: lock}
	case Postgres:
		connector := pgdriver.NewConnector(pgdriver.WithDSN(opts.DSN))
		sqldb := sql.OpenDB(connector)
		sqldb.SetMaxOpenConns(maxConns)
		sqldb.SetMaxIdleConns(maxConns)
		sqldb.SetConnMaxIdleTime(5 * time.Minute)
		d = &DB{DB: bun.NewDB(sqldb, pgdialect.New()), driver: Postgres, pgConnector: connector}
	default:
		return nil, fmt.Errorf("unknown database driver %q", opts.Driver)
	}
	if opts.Tracing {
		d.DB = d.WithQueryHook(bunotel.NewQueryHook(bunotel.WithDBName("hangar")))
	}
	if err := d.PingContext(ctx); err != nil {
		_ = d.Close()
		return nil, fmt.Errorf("connect to %s: %w", opts.Driver, err)
	}
	return d, nil
}

// sqlitePath returns the file path of a SQLite DSN, or "" for in-memory databases.
func sqlitePath(dsn string) string {
	p := strings.TrimPrefix(dsn, "file:")
	if i := strings.IndexByte(p, '?'); i >= 0 {
		p = p[:i]
	}
	if p == "" || p == ":memory:" || strings.HasPrefix(p, ":memory:") {
		return ""
	}
	return p
}

func sqliteDSN(dsn string) string {
	base := dsn
	if !strings.HasPrefix(base, "file:") {
		base = "file:" + base
	}
	sep := "?"
	if strings.Contains(base, "?") {
		sep = "&"
	}
	return base + sep + strings.Join([]string{
		"_pragma=busy_timeout(10000)",
		"_pragma=journal_mode(WAL)",
		"_pragma=foreign_keys(1)",
		"_pragma=synchronous(NORMAL)",
		"_txlock=immediate",
	}, "&")
}

// Driver returns SQLite or Postgres.
func (d *DB) Driver() string { return d.driver }

// IsSQLite reports whether the database is SQLite.
func (d *DB) IsSQLite() bool { return d.driver == SQLite }

// Close closes the connection pool and releases the SQLite lock.
func (d *DB) Close() error {
	err := d.DB.Close()
	d.lock.release()
	return err
}

// InTx runs fn in a transaction, committing if fn returns nil. On SQLite the
// transaction takes the write lock when it starts (BEGIN IMMEDIATE), so
// concurrent writers queue instead of failing.
func (d *DB) InTx(ctx context.Context, fn func(ctx context.Context, tx bun.Tx) error) error {
	return d.RunInTx(ctx, nil, fn)
}

// NewID returns a new UUIDv7, the ID format of every table.
func NewID() uuid.UUID { return uuid.Must(uuid.NewV7()) }

// Now returns the current time in UTC with microsecond precision, which both
// databases store exactly.
func Now() time.Time { return time.Now().UTC().Truncate(time.Microsecond) }

func migrationsFS(driver string) (fs.FS, error) {
	return fs.Sub(migrationFiles, "migrations/"+driver)
}

// MigrationFiles returns the SQL migration files Migrate applies for driver.
// Test helpers hash them to tell whether a migrated template database is
// still current.
func MigrationFiles(driver string) (fs.FS, error) {
	if driver != SQLite && driver != Postgres {
		return nil, fmt.Errorf("unknown database driver %q", driver)
	}
	return migrationsFS(driver)
}

func (d *DB) migrator() (*migrate.Migrator, *migrate.Migrations, error) {
	sub, err := migrationsFS(d.driver)
	if err != nil {
		return nil, nil, err
	}
	m := migrate.NewMigrations()
	if err := m.Discover(sub); err != nil {
		return nil, nil, fmt.Errorf("discover migrations: %w", err)
	}
	return migrate.NewMigrator(d.DB, m, migrate.WithMarkAppliedOnSuccess(true)), m, nil
}

// Migrate applies every pending migration. Migrators that start together,
// such as the migrate init containers of several Pods, wait for one another
// instead of failing: see lockMigrations.
func (d *DB) Migrate(ctx context.Context) error {
	mg, _, err := d.migrator()
	if err != nil {
		return err
	}
	unlock, err := d.lockMigrations(ctx)
	if err != nil {
		return fmt.Errorf("lock migrations: %w", err)
	}
	defer unlock()
	// bun's Init and its bun_migration_locks lock are not safe to run
	// concurrently: two CREATE TABLE IF NOT EXISTS on Postgres race and fail
	// with SQLSTATE 23505, and its lock is a row that fails at once instead of
	// waiting and that a migrator killed before Unlock leaves behind for good.
	// lockMigrations replaces it, so Migrate never takes it.
	if err := mg.Init(ctx); err != nil {
		return fmt.Errorf("init migrations: %w", err)
	}
	if _, err := mg.Migrate(ctx); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	return nil
}

// migrationLockKey is the Postgres advisory lock key Migrate holds: the
// first eight bytes of SHA-256("hangar.migrate") as a signed integer.
var migrationLockKey = func() int64 {
	sum := sha256.Sum256([]byte("hangar.migrate"))
	return int64(binary.BigEndian.Uint64(sum[:8]))
}()

// Lock polling bounds for lockMigrations.
const (
	migrationLockFirstWait = 50 * time.Millisecond
	migrationLockMaxWait   = 2 * time.Second
)

// lockMigrations makes this the only migrator of the database until unlock
// is called, waiting for others until ctx ends.
//
// On Postgres it holds a session-level advisory lock on a dedicated
// connection. Postgres releases it when the session ends, so a migrator
// killed mid-migration (OOM, eviction, node loss) leaves nothing behind. It
// polls pg_try_advisory_lock with a capped exponential backoff rather than
// blocking in pg_advisory_lock, so ctx cancellation stops the wait.
//
// SQLite needs nothing more: Open holds an exclusive lock file, so no other
// process can migrate the same database.
func (d *DB) lockMigrations(ctx context.Context) (unlock func(), err error) {
	if d.driver != Postgres {
		return func() {}, nil
	}
	// The lock's connection comes from its own pool of one, so holding it
	// never takes a connection Init and Migrate need, even when the
	// caller's pool has only one.
	lockDB := sql.OpenDB(d.pgConnector)
	lockDB.SetMaxOpenConns(1)
	fail := func(err error) (func(), error) {
		_ = lockDB.Close()
		return nil, err
	}
	conn, err := lockDB.Conn(ctx)
	if err != nil {
		return fail(err)
	}
	wait := migrationLockFirstWait
	for {
		var locked bool
		if err := conn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock($1)", migrationLockKey).Scan(&locked); err != nil {
			_ = conn.Close()
			return fail(err)
		}
		if locked {
			break
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			_ = conn.Close()
			return fail(fmt.Errorf("wait for another migrator: %w", context.Cause(ctx)))
		case <-t.C:
		}
		wait = min(wait*2, migrationLockMaxWait)
	}
	return func() {
		// Closing the session releases the lock too; unlock first so a
		// connection the pool keeps never holds it.
		_, _ = conn.ExecContext(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", migrationLockKey)
		_ = conn.Close()
		_ = lockDB.Close()
	}, nil
}

// SchemaStatus describes how far the database schema is from the binary's.
type SchemaStatus struct {
	// Expected is the name of the newest migration this binary knows.
	Expected string
	// Current is the name of the newest applied migration, or "".
	Current string
	// Pending lists migrations this binary knows that are not applied.
	Pending []string
}

// UpToDate reports whether every known migration is applied.
func (s SchemaStatus) UpToDate() bool { return len(s.Pending) == 0 }

// Schema reports the schema status. /readyz uses it so pods wait for the
// migration Job.
func (d *DB) Schema(ctx context.Context) (SchemaStatus, error) {
	mg, known, err := d.migrator()
	if err != nil {
		return SchemaStatus{}, err
	}
	ms, err := mg.MigrationsWithStatus(ctx)
	if err != nil {
		// The migrations table does not exist yet.
		all := known.Sorted()
		st := SchemaStatus{}
		for _, m := range all {
			st.Pending = append(st.Pending, m.Name)
			st.Expected = m.Name
		}
		return st, nil
	}
	st := SchemaStatus{}
	names := make([]string, 0, len(ms))
	for _, m := range ms {
		names = append(names, m.Name)
		if m.IsApplied() {
			if m.Name > st.Current {
				st.Current = m.Name
			}
		} else {
			st.Pending = append(st.Pending, m.Name)
		}
	}
	sort.Strings(names)
	if len(names) > 0 {
		st.Expected = names[len(names)-1]
	}
	return st, nil
}

// IsUniqueViolation reports whether err is a unique or primary key
// constraint violation on either database.
func IsUniqueViolation(err error) bool {
	var pgErr pgdriver.Error
	if errors.As(err, &pgErr) {
		return pgErr.Field('C') == "23505"
	}
	var sqlErr *sqlite.Error
	if errors.As(err, &sqlErr) {
		code := sqlErr.Code()
		return code == sqlite3.SQLITE_CONSTRAINT_UNIQUE || code == sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY
	}
	return false
}

// IsForeignKeyViolation reports whether err is a foreign key violation.
func IsForeignKeyViolation(err error) bool {
	var pgErr pgdriver.Error
	if errors.As(err, &pgErr) {
		return pgErr.Field('C') == "23503"
	}
	var sqlErr *sqlite.Error
	if errors.As(err, &sqlErr) {
		return sqlErr.Code() == sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY
	}
	return false
}

// IsNotFound reports whether err is sql.ErrNoRows.
func IsNotFound(err error) bool { return errors.Is(err, sql.ErrNoRows) }
