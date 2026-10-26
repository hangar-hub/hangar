package db

import (
	"context"
	"io/fs"
)

// MigrateFS is Migrate with the migration files in fsys, for tests.
func (d *DB) MigrateFS(ctx context.Context, fsys fs.FS) error { return d.migrateFS(ctx, fsys) }

// IsUndefinedTable is isUndefinedTable, for tests.
func IsUndefinedTable(err error) bool { return isUndefinedTable(err) }
