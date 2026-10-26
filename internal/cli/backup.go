package cli

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"github.com/uptrace/bun"

	"github.com/hangar-hub/hangar/internal/blob"
	"github.com/hangar-hub/hangar/internal/config"
	"github.com/hangar-hub/hangar/internal/db"
	"github.com/hangar-hub/hangar/internal/secrets"
)

// backupFormat is the version of the backup layout.
const backupFormat = 1

// backupManifest is manifest.json at the top of a backup folder.
type backupManifest struct {
	Format        int              `json:"format"`
	HangarVersion string           `json:"hangar_version"`
	CreatedAt     time.Time        `json:"created_at"`
	Driver        string           `json:"database_driver"`
	Schema        string           `json:"schema_version"`
	Tables        map[string]int64 `json:"tables"`
	BlobBackend   string           `json:"blob_backend,omitempty"`
	Blobs         int64            `json:"blobs"`
	BlobBytes     int64            `json:"blob_bytes"`
	SkippedBlobs  bool             `json:"blobs_skipped"`
}

// skippedBlobPrefixes are Blob store keys a backup leaves out: staging
// areas that are swept after 24 hours anyway (02-architecture.md).
var skippedBlobPrefixes = []string{"tmp/", "uploads/", "exports/"}

func skipBlobKey(key string) bool {
	for _, p := range skippedBlobPrefixes {
		if strings.HasPrefix(key, p) {
			return true
		}
	}
	return false
}

func (a *app) backupCommand() *cobra.Command {
	var f adminFlags
	var out string
	var skipBlobs bool
	cmd := &cobra.Command{
		Use:   "backup",
		Short: "Back up the database and the Blob store to a folder",
		Long: `Writes a backup folder: manifest.json, every database table as JSON Lines in
db/, and every Blob store object in blobs/ under its key. Staging objects
(tmp/, uploads/ and exports/) are left out.

The database is read in one transaction, so it is consistent. Objects are
copied after it; LFS objects and git packs never change once written, and
garbage collection keeps unreferenced objects for gc.grace_period, so every
object the database references is in the backup.

With SQLite, stop Hangar first: only one process may open the database. With
Postgres, the backup can run while Hangar is running. With --skip-blobs, back
up the Blob store with the storage's own tools instead (for example bucket
replication or a filesystem snapshot).`,
		Example: `  hangar admin backup --output /backups/hangar-$(date +%F)
  hangar admin backup --output /backups/db-only --skip-blobs`,
		Args: args(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			a := a.with(cmd)
			if out == "" {
				return usagef("--output is required")
			}
			cfg, err := f.load()
			if err != nil {
				return err
			}
			m, err := a.backup(cmd.Context(), cfg, out, skipBlobs)
			if err != nil {
				return err
			}
			if a.json {
				return a.printJSON(m)
			}
			var rows int64
			for _, n := range m.Tables {
				rows += n
			}
			a.printf("Backed up %d rows in %d tables", rows, len(m.Tables))
			if !m.SkippedBlobs {
				a.printf(" and %d objects (%s)", m.Blobs, formatBytes(m.BlobBytes))
			}
			a.printf(" to %s.\n", out)
			return nil
		},
	}
	f.register(cmd, a)
	cmd.Flags().StringVarP(&out, "output", "o", "", "folder to write the backup to; must not exist or be empty")
	cmd.Flags().BoolVar(&skipBlobs, "skip-blobs", false, "back up the database only")
	return cmd
}

func (a *app) backup(ctx context.Context, cfg *config.Config, out string, skipBlobs bool) (*backupManifest, error) {
	if entries, err := os.ReadDir(out); err == nil && len(entries) > 0 {
		return nil, usagef("%s is not empty", out)
	}
	var store blob.Store
	if !skipBlobs {
		s, err := a.openStore(ctx, cfg)
		if err != nil {
			return nil, err
		}
		store = s
	}
	d, err := openDB(ctx, cfg)
	if err != nil {
		return nil, err
	}
	defer d.Close()
	st, err := d.Schema(ctx)
	if err != nil {
		return nil, err
	}
	if !st.UpToDate() {
		return nil, fmt.Errorf("the database schema is at %q and this binary expects %q; run hangar admin migrate, or use the matching Hangar version", st.Current, st.Expected)
	}
	if err := os.MkdirAll(filepath.Join(out, "db"), 0o700); err != nil {
		return nil, err
	}
	m := &backupManifest{Format: backupFormat, HangarVersion: a.opts.Version, CreatedAt: time.Now().UTC(),
		Driver: d.Driver(), Schema: st.Current, Tables: map[string]int64{}, SkippedBlobs: skipBlobs}
	if err := dumpTables(ctx, d, filepath.Join(out, "db"), m.Tables); err != nil {
		return nil, err
	}
	if store != nil {
		m.BlobBackend = cfg.Storage.Backend
		prog := newProgress(a.stderr, "Copying objects", 0, a.json)
		if m.Blobs, m.BlobBytes, err = copyBlobsOut(ctx, store, filepath.Join(out, "blobs"), prog); err != nil {
			return nil, err
		}
		prog.finish()
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(out, "manifest.json"), b, 0o600); err != nil {
		return nil, err
	}
	return m, nil
}

// tableName returns a model's table name.
func tableName(d *db.DB, model any) string {
	return d.Table(reflect.TypeOf(model)).Name
}

// dumpTables writes every table but the derived ones to <dir>/<table>.jsonl
// in one read transaction.
func dumpTables(ctx context.Context, d *db.DB, dir string, counts map[string]int64) error {
	opts := &sql.TxOptions{ReadOnly: true}
	if !d.IsSQLite() {
		opts.Isolation = sql.LevelRepeatableRead
	}
	return d.RunInTx(ctx, opts, func(ctx context.Context, tx bun.Tx) error {
		for _, model := range db.BackupModels() {
			typ := reflect.TypeOf(model).Elem()
			name := tableName(d, model)
			n, err := dumpTable(ctx, d, tx, typ, filepath.Join(dir, name+".jsonl"))
			if err != nil {
				return fmt.Errorf("back up %s: %w", name, err)
			}
			counts[name] = n
		}
		return nil
	})
}

func dumpTable(ctx context.Context, d *db.DB, tx bun.Tx, typ reflect.Type, path string) (int64, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) // #nosec G304 -- inside the backup folder.
	if err != nil {
		return 0, err
	}
	defer f.Close()
	w := bufio.NewWriterSize(f, 1<<20)
	enc := json.NewEncoder(w)
	rows, err := tx.NewSelect().Model(reflect.New(typ).Interface()).Rows(ctx)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var n int64
	for rows.Next() {
		row := reflect.New(typ).Interface()
		if err := d.ScanRow(ctx, rows, row); err != nil {
			return n, err
		}
		if err := enc.Encode(row); err != nil {
			return n, err
		}
		n++
	}
	if err := rows.Err(); err != nil {
		return n, err
	}
	if err := w.Flush(); err != nil {
		return n, err
	}
	return n, f.Sync()
}

// copyBlobsOut copies every object except staging ones into dir/<key>.
func copyBlobsOut(ctx context.Context, store blob.Store, dir string, prog *progress) (count, bytes int64, err error) {
	err = store.List(ctx, "", func(info blob.Info) error {
		if skipBlobKey(info.Key) {
			return nil
		}
		if err := blob.ValidateKey(info.Key); err != nil {
			return fmt.Errorf("refusing object key %q: %w", info.Key, err)
		}
		dst := filepath.Join(dir, filepath.FromSlash(info.Key))
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return err
		}
		r, _, err := store.Get(ctx, info.Key, nil)
		if err != nil {
			return fmt.Errorf("read %s: %w", info.Key, err)
		}
		defer r.Close()
		f, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600) // #nosec G304 -- a validated key inside the backup folder.
		if err != nil {
			return err
		}
		n, err := io.Copy(f, progressReader{r: r, p: prog})
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return fmt.Errorf("copy %s: %w", info.Key, err)
		}
		if info.Size >= 0 && n != info.Size {
			return fmt.Errorf("copy %s: received %d bytes, expected %d", info.Key, n, info.Size)
		}
		count++
		bytes += n
		return nil
	})
	return count, bytes, err
}

func (a *app) restoreCommand() *cobra.Command {
	var f adminFlags
	var skipBlobs, force bool
	cmd := &cobra.Command{
		Use:   "restore <backup-folder>",
		Short: "Restore a backup made by hangar admin backup",
		Long: `Restores a backup folder into the configured database and Blob store. The
database is migrated first, and must then be at the same schema version as
the backup: restore with the Hangar version that made the backup, then
upgrade. The database must be empty unless --force is given, which deletes
every row first.

Rows are written in one transaction, so a failed restore leaves the database
as it was. Objects already in the Blob store with the right size are not
copied again. A backup from SQLite restores into Postgres and the other way
round, and a filesystem backup restores into S3 and the other way round.

Stop every Hangar process first.`,
		Example: `  hangar admin restore /backups/hangar-2026-10-13
  hangar admin restore /backups/hangar-2026-10-13 --set database.driver=postgres --set database.dsn=postgres://...`,
		Args: args(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, argv []string) error {
			a := a.with(cmd)
			cfg, err := f.load()
			if err != nil {
				return err
			}
			m, err := a.restore(cmd.Context(), cfg, argv[0], skipBlobs, force)
			if err != nil {
				return err
			}
			if a.json {
				return a.printJSON(m)
			}
			var rows int64
			for _, n := range m.Tables {
				rows += n
			}
			a.printf("Restored %d rows", rows)
			if !skipBlobs && !m.SkippedBlobs {
				a.printf(" and %d objects", m.Blobs)
			}
			a.printf(" from %s.\n", argv[0])
			return nil
		},
	}
	f.register(cmd, a)
	cmd.Flags().BoolVar(&skipBlobs, "skip-blobs", false, "restore the database only")
	cmd.Flags().BoolVar(&force, "force", false, "delete every row in the database before restoring")
	return cmd
}

func (a *app) restore(ctx context.Context, cfg *config.Config, dir string, skipBlobs, force bool) (*backupManifest, error) {
	b, err := os.ReadFile(filepath.Join(dir, "manifest.json")) // #nosec G304 -- the backup the admin named.
	if err != nil {
		return nil, fmt.Errorf("%s is not a Hangar backup: %w", dir, err)
	}
	var m backupManifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("read manifest.json: %w", err)
	}
	if m.Format != backupFormat {
		return nil, fmt.Errorf("backup format %d is not supported; this version of Hangar reads format %d", m.Format, backupFormat)
	}
	var store blob.Store
	if !skipBlobs && !m.SkippedBlobs {
		if store, err = a.openStore(ctx, cfg); err != nil {
			return nil, err
		}
	}
	d, err := openDB(ctx, cfg)
	if err != nil {
		return nil, err
	}
	defer d.Close()
	if err := d.Migrate(ctx); err != nil {
		return nil, err
	}
	st, err := d.Schema(ctx)
	if err != nil {
		return nil, err
	}
	if st.Current != m.Schema {
		return nil, fmt.Errorf("the backup is at schema %q but this database is at %q; restore with Hangar %s, then upgrade", m.Schema, st.Current, m.HangarVersion)
	}
	prog := newProgress(a.stderr, "Restoring rows", 0, a.json)
	if err := loadTables(ctx, d, filepath.Join(dir, "db"), force, prog); err != nil {
		return nil, err
	}
	prog.finish()
	if store != nil {
		bp := newProgress(a.stderr, "Copying objects", m.BlobBytes, a.json)
		if err := copyBlobsIn(ctx, store, filepath.Join(dir, "blobs"), bp); err != nil {
			return nil, err
		}
		bp.finish()
	}
	return &m, nil
}

// loadTables inserts every table's rows in db.AllModels order, so foreign
// keys point at rows already inserted, in one transaction.
func loadTables(ctx context.Context, d *db.DB, dir string, force bool, prog *progress) error {
	models := db.AllModels()
	return d.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		for i := len(models) - 1; i >= 0; i-- {
			n, err := tx.NewSelect().Model(models[i]).Count(ctx)
			if err != nil {
				return err
			}
			if n == 0 {
				continue
			}
			if !force {
				return fmt.Errorf("the database is not empty (%s has %d rows); restore into an empty database or pass --force", tableName(d, models[i]), n)
			}
			if _, err := tx.NewDelete().Model(models[i]).Where("1 = 1").Exec(ctx); err != nil {
				return fmt.Errorf("empty %s: %w", tableName(d, models[i]), err)
			}
		}
		for _, model := range models {
			name := tableName(d, model)
			if err := loadTable(ctx, tx, reflect.TypeOf(model).Elem(), filepath.Join(dir, name+".jsonl"), prog); err != nil {
				return fmt.Errorf("restore %s: %w", name, err)
			}
		}
		return nil
	})
}

const restoreBatch = 200

func loadTable(ctx context.Context, tx bun.Tx, typ reflect.Type, path string, prog *progress) error {
	f, err := os.Open(path) // #nosec G304 -- inside the backup folder.
	if errors.Is(err, os.ErrNotExist) {
		return nil // a table added after the backup's schema has no rows.
	}
	if err != nil {
		return err
	}
	defer f.Close()
	dec := json.NewDecoder(bufio.NewReaderSize(f, 1<<20))
	batch := reflect.MakeSlice(reflect.SliceOf(reflect.PointerTo(typ)), 0, restoreBatch)
	flush := func() error {
		if batch.Len() == 0 {
			return nil
		}
		s := reflect.New(batch.Type())
		s.Elem().Set(batch)
		if _, err := tx.NewInsert().Model(s.Interface()).Exec(ctx); err != nil {
			return err
		}
		batch = batch.Slice(0, 0)
		return nil
	}
	for {
		row := reflect.New(typ)
		if err := dec.Decode(row.Interface()); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return err
		}
		batch = reflect.Append(batch, row)
		prog.add(1)
		if batch.Len() == restoreBatch {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	return flush()
}

// copyBlobsIn puts every file under dir into the store at its relative key,
// skipping objects already stored with the same size.
func copyBlobsIn(ctx context.Context, store blob.Store, dir string, prog *progress) error {
	root, err := os.OpenRoot(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer root.Close()
	return fs.WalkDir(root.FS(), ".", func(rel string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return err
		}
		key := rel
		if err := blob.ValidateKey(key); err != nil {
			return fmt.Errorf("refusing object key %q: %w", key, err)
		}
		st, err := e.Info()
		if err != nil {
			return err
		}
		if info, err := store.Stat(ctx, key); err == nil && info.Size == st.Size() {
			prog.add(st.Size())
			return nil
		}
		f, err := root.Open(rel)
		if err != nil {
			return err
		}
		defer f.Close()
		if err := store.Put(ctx, key, progressReader{r: f, p: prog}, st.Size()); err != nil {
			return fmt.Errorf("write %s: %w", key, err)
		}
		return nil
	})
}

// reencrypt re-seals every secret column with the new key in one
// transaction and records an audit event. It returns the count per table.
func reencrypt(ctx context.Context, d *db.DB, oldKey, newKey []byte) (map[string]int, error) {
	from, err := secrets.New(oldKey)
	if err != nil {
		return nil, err
	}
	to, err := secrets.New(newKey)
	if err != nil {
		return nil, err
	}
	counts := map[string]int{}
	err = d.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		for _, t := range rotateTargets {
			var rows []struct {
				ID    uuid.UUID `bun:"id"`
				Value string    `bun:"value"`
			}
			err := tx.NewSelect().Table(t.Table).
				ColumnExpr("id, ? AS value", bun.Ident(t.Column)).
				Where("? IS NOT NULL AND ? <> ''", bun.Ident(t.Column), bun.Ident(t.Column)).
				Scan(ctx, &rows)
			if err != nil {
				return fmt.Errorf("read %s.%s: %w", t.Table, t.Column, err)
			}
			for _, r := range rows {
				plain, err := from.Open(r.Value, t.Purpose)
				if err != nil {
					return fmt.Errorf("%s %s: %w (is security.instance_key the key these secrets were made with?)", t.Table, r.ID, err)
				}
				ct, err := to.Seal(plain, t.Purpose)
				if err != nil {
					return err
				}
				if _, err := tx.NewUpdate().Table(t.Table).Set("? = ?", bun.Ident(t.Column), ct).
					Set("updated_at = ?", db.Now()).Where("id = ?", r.ID).Exec(ctx); err != nil {
					return fmt.Errorf("update %s %s: %w", t.Table, r.ID, err)
				}
			}
			counts[t.Table] = len(rows)
		}
		data, _ := json.Marshal(map[string]any{"reencrypted": counts})
		ev := &db.AuditEvent{ID: db.NewID(), Action: "instance_key.rotated", TargetType: "instance", TargetID: "instance",
			IP: "", UserAgent: "hangar admin rotate-key", Data: data}
		ev.CreatedAt, ev.UpdatedAt = db.Now(), db.Now()
		_, err := tx.NewInsert().Model(ev).Exec(ctx)
		return err
	})
	return counts, err
}
