# Databases

Hangar keeps metadata, refs, Jobs and sessions in SQLite or Postgres. The schema is the same on both.

## SQLite

```yaml
database:
  driver: sqlite
  dsn: /var/lib/hangar/hangar.db
```

SQLite is for one process in role `all`: Hangar holds a lock file next to the database and refuses to start a second process against it. Keep the database on a local disk, not a network share.

## Postgres

```yaml
database:
  driver: postgres
  dsn: postgres://hangar:password@postgres:5432/hangar?sslmode=require
  max_open_conns: 20
```

Postgres supports any number of `web` and `worker` processes. Hangar is tested with Postgres 18. Put the DSN in a secret (`HANGAR_DATABASE_DSN`), not in a configuration file.

`database.driver` defaults to `sqlite`, so set it to `postgres` (`HANGAR_DATABASE_DRIVER=postgres`) as well as the DSN. With the `sqlite` driver, Hangar refuses a DSN that is a URL other than `file:` and names the setting to change.

## Migrations

Processes in role `all` apply migrations when they start. With separate `web` and `worker` processes, run migrations once before starting them:

```sh
hangar admin migrate
```

The Helm chart and the scaled Compose example do this for you. `web` and `worker` processes report not ready on `/readyz` until the schema is at the version they expect.

## Moving from SQLite to Postgres

Back up with SQLite configured, then restore with Postgres configured; see [Backup and restore](backup.md).
