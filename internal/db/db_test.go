package db_test

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"

	"github.com/hangar-hub/hangar/internal/db"
	"github.com/hangar-hub/hangar/internal/db/dbtest"
)

var (
	fixtureID  = uuid.MustParse("01920000-0000-7000-8000-000000000001")
	fixtureTS  = time.Date(2026, 10, 13, 12, 34, 56, 123456000, time.UTC)
	fixtureDay = time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
)

// checkValues satisfies the CHECK constraints in the schema, and foreign
// keys to text columns.
var checkValues = map[string]map[string]string{
	"lfs_uploaders":      {"oid": "lfs_objects.oid"},
	"namespaces":         {"kind": "org"},
	"tokens":             {"scope": "write"},
	"memberships":        {"role": "admin", "source": "oidc"},
	"oauth_device_codes": {"status": "approved"},
	"repos":              {"type": "dataset", "visibility": "internal"},
	"collaborators":      {"role": "write"},
	"packs":              {"state": "live"},
	"tree_entries":       {"type": "file"},
	"upstreams":          {"kind": "hf", "source": "config"},
	"dependencies":       {"kind": "code"},
	"scan_results":       {"status": "suspicious"},
	"jobs":               {"status": "running"},
	"collections":        {"visibility": "private"},
	"discussions":        {"kind": "pull_request", "status": "open"},
	"access_requests":    {"status": "pending"},
	"dataset_splits":     {"status": "ready"},
	"invites":            {"role": "write"},
}

// fill sets every field of the model to a non-zero value, so a round trip
// checks every column. All UUIDs are fixtureID, so foreign keys point at
// the rows inserted earlier in db.AllModels order.
func fill(t *testing.T, table string, v reflect.Value) {
	t.Helper()
	typ := v.Type()
	for i := range typ.NumField() {
		f := v.Field(i)
		sf := typ.Field(i)
		if sf.Name == "BaseModel" {
			continue
		}
		if sf.Anonymous {
			fill(t, table, f)
			continue
		}
		col := strings.Split(sf.Tag.Get("bun"), ",")[0]
		setValue(t, table, col, f)
	}
}

func setValue(t *testing.T, table, col string, f reflect.Value) {
	t.Helper()
	if f.Kind() == reflect.Pointer {
		f.Set(reflect.New(f.Type().Elem()))
		setValue(t, table, col, f.Elem())
		return
	}
	if s, ok := checkValues[table][col]; ok {
		f.SetString(s)
		return
	}
	switch f.Interface().(type) {
	case uuid.UUID:
		f.Set(reflect.ValueOf(fixtureID))
		return
	case time.Time:
		if col == "day" {
			f.Set(reflect.ValueOf(fixtureDay))
		} else {
			f.Set(reflect.ValueOf(fixtureTS))
		}
		return
	case json.RawMessage:
		f.Set(reflect.ValueOf(json.RawMessage(`{"k":[1,"two"]}`)))
		return
	case []byte:
		f.SetBytes([]byte{0, 1, 2, 255})
		return
	case db.GitSignature:
		f.Set(reflect.ValueOf(db.GitSignature{Name: "James", Email: "james@example.com", When: fixtureTS}))
		return
	case db.TokenRestrictionsColumn:
		f.Set(reflect.ValueOf(db.TokenRestrictionsColumn{Namespaces: []string{"ml"}}))
		return
	case []db.UploadPart:
		f.Set(reflect.ValueOf([]db.UploadPart{{Number: 1, ETag: `"abc"`, Size: 5 << 20}}))
		return
	case map[string]any:
		f.Set(reflect.ValueOf(map[string]any{"k": "v"}))
		return
	case map[string]string:
		f.Set(reflect.ValueOf(map[string]string{"traceparent": "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"}))
		return
	case []string:
		f.Set(reflect.ValueOf([]string{"a", "b"}))
		return
	}
	switch f.Kind() {
	case reflect.String:
		f.SetString(table + "." + col)
	case reflect.Bool:
		f.SetBool(true)
	case reflect.Int, reflect.Int64:
		f.SetInt(42)
	default:
		t.Fatalf("%s.%s: no fixture for %s", table, col, f.Type())
	}
}

func tableName(d *db.DB, m any) string {
	return d.Table(reflect.TypeOf(m)).Name
}

func TestEveryTableRoundTrips(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		ctx := context.Background()
		for _, m := range db.AllModels() {
			typ := reflect.TypeOf(m).Elem()
			table := tableName(d, m)
			row := reflect.New(typ)
			fill(t, table, row.Elem())
			_, err := d.NewInsert().Model(row.Interface()).Exec(ctx)
			require.NoError(t, err, "insert into %s", table)

			back := reflect.New(typ)
			// Copy the primary key into back, then select by it.
			for _, pk := range d.Table(typ).PKs {
				back.Elem().FieldByIndex(pk.Index).Set(row.Elem().FieldByIndex(pk.Index))
			}
			require.NoError(t, d.NewSelect().Model(back.Interface()).WherePK().Scan(ctx), "select from %s", table)
			want, err := json.Marshal(normalise(row.Interface()))
			require.NoError(t, err)
			got, err := json.Marshal(normalise(back.Interface()))
			require.NoError(t, err)
			require.JSONEq(t, string(want), string(got), "round trip of %s", table)
		}
	})
}

// normalise converts every time to UTC, as Postgres returns local times.
func normalise(v any) any {
	rv := reflect.ValueOf(v).Elem()
	walkTimes(rv)
	return v
}

func walkTimes(v reflect.Value) {
	switch v.Kind() {
	case reflect.Pointer:
		if !v.IsNil() {
			walkTimes(v.Elem())
		}
	case reflect.Struct:
		if t, ok := v.Interface().(time.Time); ok {
			if v.CanSet() {
				v.Set(reflect.ValueOf(t.UTC()))
			}
			return
		}
		for i := range v.NumField() {
			if v.Type().Field(i).IsExported() {
				walkTimes(v.Field(i))
			}
		}
	}
}

func TestSchemaHasAModelForEveryTable(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		tables := listTables(t, d)
		var models []string
		for _, m := range db.AllModels() {
			models = append(models, tableName(d, m))
		}
		sort.Strings(models)
		require.Equal(t, tables, models)
	})
}

func listTables(t *testing.T, d *db.DB) []string {
	t.Helper()
	var tables []string
	var q string
	if d.IsSQLite() {
		q = "SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' AND name NOT LIKE 'bun_%' ORDER BY name"
	} else {
		q = "SELECT table_name FROM information_schema.tables WHERE table_schema = 'public' AND table_name NOT LIKE 'bun_%' ORDER BY table_name"
	}
	require.NoError(t, d.NewRaw(q).Scan(context.Background(), &tables))
	return tables
}

func listColumns(t *testing.T, d *db.DB, table string) []string {
	t.Helper()
	var cols []string
	var q *bun.RawQuery
	if d.IsSQLite() {
		q = d.NewRaw("SELECT name FROM pragma_table_info(?) ORDER BY name", table)
	} else {
		q = d.NewRaw("SELECT column_name FROM information_schema.columns WHERE table_schema = 'public' AND table_name = ? ORDER BY column_name", table)
	}
	require.NoError(t, q.Scan(context.Background(), &cols))
	return cols
}

func TestDialectsHaveTheSameColumns(t *testing.T) {
	dbtest.RequirePostgres(t)
	lite, pg := dbtest.SQLite(t), dbtest.Postgres(t)
	tables := listTables(t, lite)
	require.Equal(t, tables, listTables(t, pg))
	for _, table := range tables {
		require.Equal(t, listColumns(t, lite, table), listColumns(t, pg, table), table)
	}
}

func TestEveryModelColumnExists(t *testing.T) {
	d := dbtest.SQLite(t)
	for _, m := range db.AllModels() {
		tbl := d.Table(reflect.TypeOf(m))
		cols := listColumns(t, d, tbl.Name)
		var fields []string
		for _, f := range tbl.Fields {
			fields = append(fields, f.Name)
		}
		sort.Strings(fields)
		require.Equal(t, cols, fields, "model and table %s must have the same columns", tbl.Name)
	}
}

func TestMigrateIsIdempotentAndSchemaStatus(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		ctx := context.Background()
		require.NoError(t, d.Migrate(ctx))
		st, err := d.Schema(ctx)
		require.NoError(t, err)
		require.True(t, st.UpToDate())
		require.Equal(t, "0005", st.Expected)
		require.Equal(t, st.Expected, st.Current)
	})
}

func TestSchemaStatusBeforeMigrate(t *testing.T) {
	ctx := context.Background()
	d, err := db.Open(ctx, db.Options{Driver: db.SQLite, DSN: filepath.Join(t.TempDir(), "x.db")})
	require.NoError(t, err)
	defer d.Close()
	st, err := d.Schema(ctx)
	require.NoError(t, err)
	require.False(t, st.UpToDate())
	require.Equal(t, []string{"0001", "0002", "0003", "0004", "0005"}, st.Pending)
}

func TestInTxRollsBackAndClassifiesErrors(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		ctx := context.Background()
		ns := &db.Namespace{ID: db.NewID(), Name: "Bellatrix", Kind: "org"}
		_, err := d.NewInsert().Model(ns).Exec(ctx)
		require.NoError(t, err)
		require.False(t, ns.CreatedAt.IsZero(), "timestamps hook sets created_at")
		require.Equal(t, ns.CreatedAt, ns.UpdatedAt)

		// Names are unique case-insensitively.
		_, err = d.NewInsert().Model(&db.Namespace{ID: db.NewID(), Name: "bellatrix", Kind: "user"}).Exec(ctx)
		require.True(t, db.IsUniqueViolation(err), "got %v", err)

		boom := errors.New("boom")
		err = d.InTx(ctx, func(ctx context.Context, tx bun.Tx) error {
			_, err := tx.NewInsert().Model(&db.Namespace{ID: db.NewID(), Name: "rolled-back", Kind: "org"}).Exec(ctx)
			require.NoError(t, err)
			return boom
		})
		require.ErrorIs(t, err, boom)
		n, err := d.NewSelect().Model((*db.Namespace)(nil)).Where("name = ?", "rolled-back").Count(ctx)
		require.NoError(t, err)
		require.Zero(t, n)

		_, err = d.NewInsert().Model(&db.User{ID: db.NewID(), NamespaceID: db.NewID()}).Exec(ctx)
		require.True(t, db.IsForeignKeyViolation(err), "got %v", err)

		err = d.NewSelect().Model(&db.Namespace{}).Where("name = ?", "nobody").Scan(ctx)
		require.True(t, db.IsNotFound(err))

		time.Sleep(2 * time.Millisecond)
		before := ns.UpdatedAt
		ns.DisplayName = "Bellatrix"
		_, err = d.NewUpdate().Model(ns).WherePK().Exec(ctx)
		require.NoError(t, err)
		require.True(t, ns.UpdatedAt.After(before), "timestamps hook sets updated_at")
	})
}

func TestJobsDedupeIndexOnlyWhileActive(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		ctx := context.Background()
		key := "pull:ns/name"
		job := func(status string) *db.Job {
			return &db.Job{ID: db.NewID(), Kind: "pull", DedupeKey: &key, Status: status, RunAfter: db.Now(), MaxAttempts: 3,
				Payload: json.RawMessage(`{}`), Progress: json.RawMessage(`{}`), Result: json.RawMessage(`{}`), TraceContext: map[string]string{}}
		}
		_, err := d.NewInsert().Model(job("succeeded")).Exec(ctx)
		require.NoError(t, err)
		_, err = d.NewInsert().Model(job("pending")).Exec(ctx)
		require.NoError(t, err)
		_, err = d.NewInsert().Model(job("running")).Exec(ctx)
		require.True(t, db.IsUniqueViolation(err), "a second active job with the same key is refused, got %v", err)
	})
}

func TestTimesCompareInOrder(t *testing.T) {
	// SQLite stores times as text; check that comparisons in SQL follow time
	// order, including fractional seconds, which the Job queue relies on.
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		ctx := context.Background()
		base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
		for i, off := range []time.Duration{0, 450 * time.Millisecond, 500 * time.Millisecond, time.Second, 1500 * time.Microsecond} {
			_, err := d.NewInsert().Model(&db.OllamaNonce{Nonce: string(rune('a' + i)), ExpiresAt: base.Add(off)}).Exec(ctx)
			require.NoError(t, err)
		}
		var nonces []string
		require.NoError(t, d.NewSelect().Model((*db.OllamaNonce)(nil)).Column("nonce").
			Where("expires_at > ?", base.Add(time.Millisecond)).OrderExpr("expires_at ASC").Scan(ctx, &nonces))
		require.Equal(t, []string{"e", "b", "c", "d"}, nonces)
	})
}

func TestSQLiteRefusesASecondProcess(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "hangar.db")
	first, err := db.Open(ctx, db.Options{Driver: db.SQLite, DSN: path})
	require.NoError(t, err)
	_, err = db.Open(ctx, db.Options{Driver: db.SQLite, DSN: path})
	require.ErrorIs(t, err, db.ErrLocked)
	require.NoError(t, first.Close())
	again, err := db.Open(ctx, db.Options{Driver: db.SQLite, DSN: path})
	require.NoError(t, err, "the lock is released on close")
	require.NoError(t, again.Close())
}

func TestOpenRejectsUnknownDriver(t *testing.T) {
	_, err := db.Open(context.Background(), db.Options{Driver: "mysql"})
	require.Error(t, err)
}

// TestSQLiteRefusesURLDSNs: a postgres:// DSN with the default sqlite
// driver used to become the relative path "postgres:/user:pass@host...",
// and Open created a database and a lock file under the working directory.
func TestSQLiteRefusesURLDSNs(t *testing.T) {
	t.Chdir(t.TempDir())
	ctx := context.Background()
	for _, dsn := range []string{
		"postgres://hangar:s3cret@db.example.com:5432/hangar",
		"postgresql://hangar:s3cret@db.example.com/hangar?sslmode=disable",
		"POSTGRES://hangar:s3cret@db.example.com/hangar",
		"mysql://hangar:s3cret@db.example.com/hangar",
	} {
		_, err := db.Open(ctx, db.Options{Driver: db.SQLite, DSN: dsn})
		require.ErrorContains(t, err, "database.driver", dsn)
		require.ErrorContains(t, err, "postgres", dsn)
		require.NotContains(t, err.Error(), "s3cret", "the error must not echo the password")
	}
	entries, err := os.ReadDir(".")
	require.NoError(t, err)
	require.Empty(t, entries, "Open must create nothing for a refused DSN")

	// File paths, file: URLs and Windows-style drive paths still open.
	for _, dsn := range []string{"hangar.db", "file:other.db?cache=shared", "file:" + filepath.Join(t.TempDir(), "x.db"), ":memory:"} {
		d, err := db.Open(ctx, db.Options{Driver: db.SQLite, DSN: dsn})
		require.NoError(t, err, dsn)
		require.NoError(t, d.Close())
	}
}

func TestNewIDIsVersion7(t *testing.T) {
	id := db.NewID()
	require.Equal(t, uuid.Version(7), id.Version())
	require.Equal(t, time.UTC, db.Now().Location())
}

// TestConcurrentPostgresMigratesWaitForOneAnother runs several migrators at
// once against an empty Postgres database, as the migrate init containers
// of Pods that start together do. Each must wait for the others and
// succeed: before Migrate took an advisory lock, concurrent runs failed on
// bun's CREATE TABLE IF NOT EXISTS (SQLSTATE 23505 on
// pg_type_typname_nsp_index) or on its migration lock row, and every loser
// exited 1 into CrashLoopBackOff.
func TestConcurrentPostgresMigratesWaitForOneAnother(t *testing.T) {
	dbtest.RequirePostgres(t)
	dsn := dbtest.PostgresDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	const migrators = 8
	// Open every handle first, so the Migrate calls start as close together
	// as the start barrier allows. A pool of one connection, as hangar admin
	// migrate's callers may use, must not deadlock against the lock.
	dbs := make([]*db.DB, migrators)
	for i := range dbs {
		d, err := db.Open(ctx, db.Options{Driver: db.Postgres, DSN: dsn, MaxOpenConns: 1})
		require.NoError(t, err)
		t.Cleanup(func() { _ = d.Close() })
		dbs[i] = d
	}
	start := make(chan struct{})
	errs := make(chan error, migrators)
	for _, d := range dbs {
		go func() {
			<-start
			errs <- d.Migrate(ctx)
		}()
	}
	close(start)
	for range migrators {
		require.NoError(t, <-errs)
	}
	st, err := dbs[0].Schema(ctx)
	require.NoError(t, err)
	require.True(t, st.UpToDate(), "pending after concurrent migrates: %v", st.Pending)
}

// TestMigrateIgnoresALockLeftByAKilledMigrator checks that a migration lock
// row left behind by a migrator killed between taking and releasing bun's
// table lock (OOM, eviction, node loss) does not block later migrations.
// The lock Migrate takes is released with the process or session that held
// it, so nothing is left to clean up by hand.
func TestMigrateIgnoresALockLeftByAKilledMigrator(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		ctx := context.Background()
		_, err := d.ExecContext(ctx, "INSERT INTO bun_migration_locks (table_name) VALUES ('bun_migrations')")
		require.NoError(t, err)
		require.NoError(t, d.Migrate(ctx))
	})
}

// TestPostgresMigrateWaitsForTheLockHolder holds Migrate's advisory lock
// from another session, as a migrator in another Pod would. Migrate waits
// until its context ends, and once the holding session ends, as when a
// migrator is killed, the next Migrate runs without anything to clean up.
func TestPostgresMigrateWaitsForTheLockHolder(t *testing.T) {
	dbtest.RequirePostgres(t)
	dsn := dbtest.PostgresDSN(t)
	ctx := context.Background()
	holder, err := db.Open(ctx, db.Options{Driver: db.Postgres, DSN: dsn, MaxOpenConns: 1})
	require.NoError(t, err)
	defer holder.Close()
	conn, err := holder.Conn(ctx)
	require.NoError(t, err)
	// The key Migrate locks: SHA-256("hangar.migrate"), first eight bytes.
	sum := sha256.Sum256([]byte("hangar.migrate"))
	_, err = conn.ExecContext(ctx, "SELECT pg_advisory_lock(?)", int64(binary.BigEndian.Uint64(sum[:8])))
	require.NoError(t, err)

	d, err := db.Open(ctx, db.Options{Driver: db.Postgres, DSN: dsn, MaxOpenConns: 1})
	require.NoError(t, err)
	defer d.Close()
	short, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	err = d.Migrate(short)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	st, err := d.Schema(ctx)
	require.NoError(t, err)
	require.False(t, st.UpToDate(), "Migrate ran while another session held the lock")

	// End the holding session without unlocking: closing the pool closes
	// the connection once it is back in it.
	require.NoError(t, conn.Close())
	require.NoError(t, holder.Close())
	done, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	require.NoError(t, d.Migrate(done))
	st, err = d.Schema(ctx)
	require.NoError(t, err)
	require.True(t, st.UpToDate())
}
