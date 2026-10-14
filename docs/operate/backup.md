# Back up and restore

What to back up:

- The **database** (SQLite file or Postgres).
- The **Blob store** (the `storage.fs.path` folder, or the bucket).
- The **Instance key** (`security.instance_key`, or `secrets.yaml` in `server.data_dir`). Without it, saved upstream tokens and webhook secrets cannot be decrypted.

## hangar admin backup

`hangar admin backup` reads the configuration like `hangar server` does and writes a folder with:

- `manifest.json`: the Hangar version, schema version, database driver, row counts and object counts;
- `db/<table>.jsonl`: every table, read in one transaction, so it is consistent;
- `blobs/<key>`: every Blob store object, except staging areas.

```sh
hangar admin backup --config /etc/hangar/hangar.yaml --output /backups/hangar-$(date +%F)
```

Objects are copied after the database. They never change once written, and garbage collection keeps unreferenced objects for `gc.grace_period` (7 days), so every object the database refers to is in the backup.

`hangar admin restore` loads a backup into an empty database, migrated to the same schema version, in one transaction, and copies objects that the Blob store does not already have:

```sh
hangar admin restore /backups/hangar-2026-10-13 --config /etc/hangar/hangar.yaml
```

A backup made on SQLite restores into Postgres and the other way round, and a filesystem backup restores into S3 and the other way round, so restore is also how you move between backends. Restore with the Hangar version that made the backup, then upgrade. `--force` empties a database that is not empty first.

Stop Hangar before you restore. Keep the Instance key with the backup, separately from it.

## Per backend combination

| Setup | Back up with | Notes |
|---|---|---|
| SQLite and filesystem (one container) | `hangar admin backup` with Hangar stopped, or a snapshot of the volume with Hangar stopped | SQLite allows one process, so `admin backup` cannot run beside the server. With Docker: `docker stop hangar && docker run --rm -v hangar:/var/lib/hangar -v /backups:/backups ghcr.io/jamesagarside/hangar:latest admin backup --output /backups/$(date +%F) && docker start hangar`. |
| Postgres and filesystem | `hangar admin backup` while running, or `pg_dump` plus a filesystem snapshot or `rsync` of the blobs folder | Take the database dump before the files. |
| Postgres and S3 | `hangar admin backup --skip-blobs` (or `pg_dump`) plus bucket versioning or replication | Copying a large bucket through Hangar is slow; S3's own replication is better. Take the database backup first. |
| SQLite and S3 | `hangar admin backup --skip-blobs` with Hangar stopped, plus bucket versioning or replication | |

Test your restores: restore into a scratch Instance and run `hangar admin doctor` against it.
