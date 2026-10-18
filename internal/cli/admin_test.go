package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"

	"github.com/jamesagarside/hangar/internal/blob"
	"github.com/jamesagarside/hangar/internal/blob/blobtest"
	"github.com/jamesagarside/hangar/internal/config"
	"github.com/jamesagarside/hangar/internal/db"
	"github.com/jamesagarside/hangar/internal/db/dbtest"
	"github.com/jamesagarside/hangar/internal/secrets"
)

var (
	fixtureID  = uuid.MustParse("01920000-0000-7000-8000-000000000001")
	fixtureTS  = time.Date(2026, 10, 13, 12, 34, 56, 123456000, time.UTC)
	fixtureDay = time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
)

// checkValues satisfies the CHECK constraints in the schema and foreign
// keys to text columns (the same values as internal/db's round-trip test).
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

// fillRow sets every column of a model to a non-zero value, so a backup
// and restore round trip checks every column.
func fillRow(t *testing.T, table string, v reflect.Value) {
	t.Helper()
	typ := v.Type()
	for i := range typ.NumField() {
		f, sf := v.Field(i), typ.Field(i)
		if sf.Name == "BaseModel" {
			continue
		}
		if sf.Anonymous {
			fillRow(t, table, f)
			continue
		}
		fillValue(t, table, strings.Split(sf.Tag.Get("bun"), ",")[0], f)
	}
}

func fillValue(t *testing.T, table, col string, f reflect.Value) {
	t.Helper()
	if f.Kind() == reflect.Pointer {
		f.Set(reflect.New(f.Type().Elem()))
		fillValue(t, table, col, f.Elem())
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

// seed inserts one row into every table and returns them as JSON by table.
func seed(t *testing.T, d *db.DB) map[string]string {
	t.Helper()
	ctx := context.Background()
	out := map[string]string{}
	for _, m := range db.AllModels() {
		row := reflect.New(reflect.TypeOf(m).Elem())
		name := tableName(d, m)
		fillRow(t, name, row.Elem())
		_, err := d.NewInsert().Model(row.Interface()).Exec(ctx)
		require.NoError(t, err, name)
		out[name] = rowsJSON(t, d, m)
	}
	return out
}

// rowsJSON selects a table and returns it as JSON with times in UTC.
func rowsJSON(t *testing.T, d *db.DB, m any) string {
	t.Helper()
	slice := reflect.New(reflect.SliceOf(reflect.TypeOf(m).Elem()))
	require.NoError(t, d.NewSelect().Model(slice.Interface()).Scan(context.Background()))
	b, err := json.Marshal(utcTimes(slice.Elem()).Interface())
	require.NoError(t, err)
	return string(b)
}

func utcTimes(v reflect.Value) reflect.Value {
	var walk func(v reflect.Value)
	walk = func(v reflect.Value) {
		switch v.Kind() {
		case reflect.Pointer:
			if !v.IsNil() {
				walk(v.Elem())
			}
		case reflect.Slice:
			for i := range v.Len() {
				walk(v.Index(i))
			}
		case reflect.Struct:
			if tm, ok := v.Interface().(time.Time); ok {
				if v.CanSet() {
					v.Set(reflect.ValueOf(tm.UTC()))
				}
				return
			}
			for i := range v.NumField() {
				if v.Type().Field(i).IsExported() {
					walk(v.Field(i))
				}
			}
		}
	}
	walk(v)
	return v
}

// adminConfig writes nothing; it returns --set flags for a database.
func dbFlags(t *testing.T, driver, dsn, dataDir string) []string {
	t.Helper()
	return []string{
		"--set", "database.driver=" + driver, "--set", "database.dsn=" + dsn,
		"--set", "server.public_url=http://localhost:8080", "--set", "server.data_dir=" + dataDir,
		"--set", "storage.fs.path=" + filepath.Join(dataDir, "blobs"), "--set", "git.cache_dir=" + filepath.Join(dataDir, "git"),
	}
}

func driverDSNs(t *testing.T) map[string][2]string {
	t.Helper()
	out := map[string][2]string{
		"sqlite": {filepath.Join(t.TempDir(), "a.db"), filepath.Join(t.TempDir(), "b.db")},
	}
	if dbtest.PostgresEnabled() {
		out["postgres"] = [2]string{dbtest.PostgresDSN(t), dbtest.PostgresDSN(t)}
		out["sqlite-to-postgres"] = [2]string{filepath.Join(t.TempDir(), "c.db"), dbtest.PostgresDSN(t)}
	}
	return out
}

func driverOf(dsn string) string {
	if strings.HasPrefix(dsn, "postgres") {
		return db.Postgres
	}
	return db.SQLite
}

func openForTest(t *testing.T, dsn string) *db.DB {
	t.Helper()
	d, err := db.Open(context.Background(), db.Options{Driver: driverOf(dsn), DSN: dsn})
	require.NoError(t, err)
	require.NoError(t, d.Migrate(context.Background()))
	return d
}

func TestBackupAndRestoreRoundTrip(t *testing.T) {
	for name, dsns := range driverDSNs(t) {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			src, dst := blobtest.NewFake(), blobtest.NewFake()
			ctx := context.Background()
			objects := map[string][]byte{
				"lfs/sha256/ab/cd/abcd": blobOf("lfs", 5000),
				"git/repo/pack/p.pack":  []byte("pack"),
				"tmp/staging":           []byte("skip"),
				"uploads/u/part-1":      []byte("skip"),
				"exports/x.zip":         []byte("skip"),
			}
			for k, v := range objects {
				require.NoError(t, src.Put(ctx, k, bytes.NewReader(v), int64(len(v))))
			}

			d := openForTest(t, dsns[0])
			want := seed(t, d)
			require.NoError(t, d.Close())

			out := filepath.Join(e.home, "backup")
			e.deps = AdminDeps{OpenBlobStore: func(context.Context, *config.Config) (blob.Store, error) { return src, nil }}
			args := append([]string{"admin", "backup", "--output", out, "--json"}, dbFlags(t, driverOf(dsns[0]), dsns[0], e.home)...)
			r := e.ok(args...)
			var m backupManifest
			require.NoError(t, json.Unmarshal([]byte(r.stdout), &m))
			require.Equal(t, int64(2), m.Blobs, "staging objects are left out")
			require.Len(t, m.Tables, len(db.AllModels()))
			for table, n := range m.Tables {
				require.Equal(t, int64(1), n, table)
			}
			require.Equal(t, ExitUsage, e.run(append([]string{"admin", "backup", "--output", out}, dbFlags(t, driverOf(dsns[0]), dsns[0], e.home)...)...).code, "the folder must be empty")

			e.deps = AdminDeps{OpenBlobStore: func(context.Context, *config.Config) (blob.Store, error) { return dst, nil }}
			restore := append([]string{"admin", "restore", out}, dbFlags(t, driverOf(dsns[1]), dsns[1], e.home)...)
			r = e.ok(restore...)
			require.Contains(t, r.stdout, "Restored")
			for _, k := range []string{"lfs/sha256/ab/cd/abcd", "git/repo/pack/p.pack"} {
				got, ok := dst.Bytes(k)
				require.True(t, ok, k)
				require.Equal(t, objects[k], got)
			}
			_, ok := dst.Bytes("tmp/staging")
			require.False(t, ok)

			d2 := openForTest(t, dsns[1])
			for _, model := range db.AllModels() {
				require.JSONEq(t, want[tableName(d2, model)], rowsJSON(t, d2, model), tableName(d2, model))
			}
			require.NoError(t, d2.Close())

			// A second restore needs --force, and then gives the same rows.
			r = e.run(restore...)
			require.Equal(t, ExitFailure, r.code)
			require.Contains(t, r.err.Error(), "not empty")
			e.ok(append(restore, "--force", "--skip-blobs")...)
			d3 := openForTest(t, dsns[1])
			defer d3.Close()
			for _, model := range db.AllModels() {
				require.JSONEq(t, want[tableName(d3, model)], rowsJSON(t, d3, model))
			}
		})
	}
}

func TestBackupErrors(t *testing.T) {
	e := newEnv(t)
	dsn := filepath.Join(e.home, "x.db")
	flags := dbFlags(t, "sqlite", dsn, e.home)
	r := e.run(append([]string{"admin", "backup", "--output", filepath.Join(e.home, "b")}, flags...)...)
	require.ErrorIs(t, r.err, errNoBlobStore)
	require.Equal(t, ExitUsage, e.run(append([]string{"admin", "backup"}, flags...)...).code)
	r = e.run(append([]string{"admin", "backup", "--skip-blobs", "--output", filepath.Join(e.home, "b")}, flags...)...)
	require.Contains(t, r.err.Error(), "schema", "an unmigrated database is refused")
	r = e.run(append([]string{"admin", "restore", filepath.Join(e.home, "nothing")}, flags...)...)
	require.Contains(t, r.err.Error(), "not a Hangar backup")
	require.Equal(t, ExitUsage, e.run("admin", "backup", "--set", "nonsense.key=1", "--output", "x").code)

	// A backup made at another schema version is refused.
	bad := filepath.Join(e.home, "bad")
	require.NoError(t, os.MkdirAll(bad, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(bad, "manifest.json"), []byte(`{"format":1,"schema_version":"0001_old","blobs_skipped":true}`), 0o600))
	r = e.run(append([]string{"admin", "restore", bad}, flags...)...)
	require.Contains(t, r.err.Error(), "0001_old")
	require.NoError(t, os.WriteFile(filepath.Join(bad, "manifest.json"), []byte(`{"format":9}`), 0o600))
	r = e.run(append([]string{"admin", "restore", bad}, flags...)...)
	require.Contains(t, r.err.Error(), "format 9")
}

func sealedKey(t *testing.T, seed byte) (string, *secrets.Sealer) {
	t.Helper()
	raw := bytes.Repeat([]byte{seed}, 32)
	s, err := secrets.New(raw)
	require.NoError(t, err)
	return base64.StdEncoding.EncodeToString(raw), s
}

func TestRotateKeyReencryptsSecrets(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	dsn := filepath.Join(e.home, "hangar.db")
	d := openForTest(t, dsn)
	seed(t, d)
	oldKey, oldSealer := sealedKey(t, 1)
	plain := map[string]string{"upstream_tokens": "hf_user_token", "upstreams": "hf_instance_token", "webhooks": "whsec"}
	for _, tg := range rotateTargets {
		ct, err := oldSealer.Seal(plain[tg.Table], tg.Purpose)
		require.NoError(t, err)
		_, err = d.NewUpdate().Table(tg.Table).Set("? = ?", bun.Ident(tg.Column), ct).Where("1 = 1").Exec(ctx)
		require.NoError(t, err)
	}
	require.NoError(t, d.Close())

	// The key comes from secrets.yaml, which is rewritten.
	require.NoError(t, os.WriteFile(filepath.Join(e.home, config.SecretsFileName), []byte("instance_key: "+oldKey+"\nbootstrap_admin_password: pw\n"), 0o600))
	newKey, newSealer := sealedKey(t, 2)
	r := e.ok(append([]string{"admin", "rotate-key", "--new-key", newKey}, dbFlags(t, "sqlite", dsn, e.home)...)...)
	require.Contains(t, r.stdout, "Re-encrypted 3 secrets")
	b, _ := os.ReadFile(filepath.Join(e.home, config.SecretsFileName))
	require.Contains(t, string(b), newKey)
	require.Contains(t, string(b), "bootstrap_admin_password: pw")

	d = openForTest(t, dsn)
	for _, tg := range rotateTargets {
		var ct string
		require.NoError(t, d.NewSelect().Table(tg.Table).ColumnExpr("?", bun.Ident(tg.Column)).Limit(1).Scan(ctx, &ct))
		got, err := newSealer.Open(ct, tg.Purpose)
		require.NoError(t, err, tg.Table)
		require.Equal(t, plain[tg.Table], got)
	}
	n, err := d.NewSelect().Table("audit_events").Where("action = ?", "instance_key.rotated").Count(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 1, n)
	require.NoError(t, d.Close())

	// With the key in the configuration, the new one is printed. The old
	// key no longer opens the secrets, so a second rotation from it fails.
	args := append([]string{"admin", "rotate-key", "--set", "security.instance_key=" + oldKey, "--json"}, dbFlags(t, "sqlite", dsn, e.home)...)
	r = e.run(args...)
	require.Error(t, r.err)
	require.Contains(t, r.err.Error(), "instance_key")
	args = append([]string{"admin", "rotate-key", "--set", "security.instance_key=" + newKey, "--json"}, dbFlags(t, "sqlite", dsn, e.home)...)
	r = e.ok(args...)
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(r.stdout), &out))
	require.NotEmpty(t, out["instance_key"])
	require.NotEqual(t, newKey, out["instance_key"])

	require.Equal(t, ExitUsage, e.run(append([]string{"admin", "rotate-key", "--set", "security.instance_key=" + newKey, "--new-key", "short"}, dbFlags(t, "sqlite", dsn, e.home)...)...).code)
	empty := t.TempDir()
	r = e.run(append([]string{"admin", "rotate-key"}, dbFlags(t, "sqlite", dsn, empty)...)...)
	require.Contains(t, r.err.Error(), "cannot be read")
}

func TestDoctor(t *testing.T) {
	e := newEnv(t)
	dsn := filepath.Join(e.home, "hangar.db")
	d := openForTest(t, dsn)
	require.NoError(t, d.Close())
	store := blobtest.NewFake()
	e.deps = AdminDeps{OpenBlobStore: func(context.Context, *config.Config) (blob.Store, error) { return store, nil }}
	flags := append(dbFlags(t, "sqlite", dsn, e.home), "--set", "security.instance_key="+base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{3}, 32)),
		"--set", "security.bootstrap_admin.password=a-long-enough-password")
	r := e.run(append([]string{"admin", "doctor"}, flags...)...)
	require.NoError(t, r.err, r.stdout)
	for _, c := range []string{"configuration", "database", "schema", "blob store", "git"} {
		require.Contains(t, r.stdout, c)
	}
	require.Empty(t, store.Keys(), "the round trip cleans up")

	r = e.run(append([]string{"admin", "doctor", "--json"}, dbFlags(t, "sqlite", filepath.Join(e.home, "new.db"), e.home)...)...)
	require.Error(t, r.err)
	var results []checkResult
	require.NoError(t, json.Unmarshal([]byte(r.stdout), &results))
	require.False(t, results[0].OK, "the configuration has no Instance key or password, so it does not validate")

	// Secrets the server generated on first start, in server.data_dir, are
	// the ones doctor checks.
	dataDir := t.TempDir()
	generated := config.Default()
	generated.Server.DataDir = dataDir
	_, err := config.EnsureSecrets(&generated)
	require.NoError(t, err)
	r = e.run(append([]string{"admin", "doctor"}, dbFlags(t, "sqlite", dsn, dataDir)...)...)
	require.NoError(t, r.err, r.stdout)

	e.deps = AdminDeps{}
	r = e.run(append([]string{"admin", "doctor"}, flags...)...)
	require.Error(t, r.err)
	require.Contains(t, r.stdout, "OpenBlobStore")

	_, err = skewMessage(time.Now().Add(time.Hour), time.Now())
	require.Error(t, err)
	msg, err := skewMessage(time.Now(), time.Now())
	require.NoError(t, err)
	require.Contains(t, msg, "clock within")
}

func TestReferences(t *testing.T) {
	e := newEnv(t)
	r := e.ok("reference", "cli")
	for _, s := range []string{"# CLI reference", "## hangar pull", "## hangar lmstudio install", "## hangar admin backup", "`--json`", "| 4 |"} {
		require.Contains(t, r.stdout, s)
	}
	require.NotContains(t, r.stdout, "## hangar reference", "hidden commands stay out")
	require.NotContains(t, r.stdout, "\u2014", "house style: no em dashes")

	r = e.ok("reference", "metrics")
	require.Contains(t, r.stdout, "`hangar.download.bytes` | `hangar_download_bytes_total` | counter | `By`")
	require.Contains(t, r.stdout, "`hangar.jobs.duration` | `hangar_jobs_duration_seconds` | histogram")
	require.Contains(t, r.stdout, "`hangar.repos` | `hangar_repos` | gauge")
}

func TestMetricsReferenceCoversEveryInstrument(t *testing.T) {
	ins, err := hangarInstruments()
	require.NoError(t, err)
	seen := map[string]bool{}
	for _, i := range ins {
		seen[i.Name] = true
		_, ok := metricAttributes[i.Name]
		require.True(t, ok, "add %s to metricAttributes", i.Name)
		require.NotEmpty(t, i.Description, i.Name)
	}
	for name := range metricAttributes {
		require.True(t, seen[name], "%s is in metricAttributes but internal/telemetry has no such instrument", name)
	}
}
