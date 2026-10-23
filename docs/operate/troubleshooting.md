---
description: "Fix common Hangar problems: check the configuration end to end with hangar admin doctor, and find what you need in the logs."
---

# Troubleshooting

## Run the doctor

`hangar admin doctor` reads the configuration like the server and checks it end to end:

```sh
hangar admin doctor --config /etc/hangar/hangar.yaml
docker exec hangar hangar admin doctor     # Postgres setups; with SQLite, stop the server first
```

| Check | What it tests |
|---|---|
| configuration | Every key is valid. |
| database | Hangar can connect. |
| schema | Migrations are at the version this binary expects. |
| clock (database) | This host's clock is within 30 seconds of Postgres's. |
| blob store | A write, read and delete work, and the clock agrees with the store. |
| presigned downloads | On S3 in `redirect` mode, a presigned URL works from this host. |
| git | git 2.39 or later is installed. |
| large reads | With `--large-read`, an object of 1100 MiB (or the size you give, such as `--large-read=3GiB`) is read back whole in one slow request. |

It exits with 1 if any check fails, so you can use it in scripts.

`--large-read` finds a proxy in front of the Blob store that cuts long reads, such as nginx with its default `proxy_max_temp_file_size`. The doctor writes the object, reads it back at 16 MiB/s, more slowly than the store sends, and deletes it. At the default size this takes over a minute. Run it after putting a proxy in front of MinIO, or when large uploads take many retries to complete. See [Large files through nginx](proxies.md#large-files-through-nginx).

## Common problems

**The server will not start and lists configuration errors.** Hangar checks the whole configuration and lists every problem at once. `hangar config validate` does the same without starting.

**"unknown environment variable HANGAR_PORT", or another `HANGAR_` name.** Hangar reads every environment variable that starts with `HANGAR_` as configuration, and refuses to start if one is not a configuration key, so a misspelt key cannot go unnoticed. Only `HANGAR_CONFIG` and names that start with `HANGAR_CLI_` or `HANGAR_TEST_` are left alone. The check applies to `hangar server`, `hangar worker`, `hangar healthcheck`, `hangar config` and the `hangar admin` commands; client commands such as `hangar pull` are not affected.

```text
Error: invalid configuration (2 problems):
  - unknown environment variable HANGAR_PORT
  - unknown environment variable HANGAR_ADMIN_PASSWORD
```

The usual cause is a name meant for another tool. The Docker Compose examples read `HANGAR_IMAGE`, `HANGAR_PORT`, `HANGAR_PUBLIC_URL`, `HANGAR_ADMIN_PASSWORD`, `HANGAR_INSTANCE_KEY` and `HANGAR_OIDC_CLIENT_SECRET` from `.env` and pass the values to Hangar under its own names, and the load tests read `HANGAR_URL` and `HANGAR_TOKEN`. If such a name reaches Hangar, for example because you loaded `.env` into your shell with `set -a; . ./.env` or passed it to `docker run -e`, unset it there, or use the configuration key's own variable from the [configuration reference](../reference/configuration.md), such as `HANGAR_SERVER_PUBLIC_URL` or `HANGAR_SECURITY_BOOTSTRAP_ADMIN_PASSWORD`.

**"database is in use by another Hangar process".** SQLite allows one process. Stop the other one, or move to Postgres to run several.

**Sign-in says "Too many failed sign-in attempts".** Someone, perhaps not the user, has sent wrong passwords for the account or from the address. The username's lock does not apply from addresses the user has signed in from with a password in the last 90 days, so they can usually sign in from their usual network. Otherwise wait for the lock to end (at most 15 minutes), or on the server host clear it with `hangar admin unlock-sign-in <username>` (or `--ip <address>`, or `--all`). With SQLite, stop the server first. If every user is throttled at once, check `server.trusted_proxies`: see [TLS and reverse proxies](proxies.md).

**Every admin is locked out.** On the server host, set a new password with `hangar admin reset-password <username>`, which prints a generated password once (or reads one with `--password-stdin`), signs the user out everywhere and clears sign-in throttling. `hangar admin create-user <username> --admin` creates another Instance admin. With SQLite, stop the server first.

**Index or scan results look out of date.** `hangar admin reindex` queues index and scan Jobs for every branch head, or for the Repos you name; the workers run them.

**`/readyz` answers 503.** The body lists the failed checks: usually the database, a pending migration (run `hangar admin migrate`) or the Blob store. A `startup` failure means the process is still building the Instance; it clears when the process logs `hangar started`. Each failed check is also logged at `warn` as `readiness check failed`, with the check's name and error, so a 503 in the access log can be traced to its cause.

**Downloads fail with a 403 from the store, or a TLS error.** In S3 `redirect` mode, clients download straight from the store, so they must reach `storage.s3.endpoint` and trust its certificate, and the clocks must agree. Use `download_mode: proxy` if clients cannot reach the store.

**Large-file uploads fail with a host lookup or connection error for the store's endpoint.** In S3 `direct` upload mode, git-lfs, `huggingface_hub` and `hangar push` upload LFS objects straight to the store, so clients must reach `storage.s3.endpoint`. Use `upload_mode: proxy` if they cannot.

**`huggingface_hub` says a Repo does not exist.** Private Repos answer as missing without a Token that can read them. Check `HF_TOKEN` and `hangar whoami`. For a Mirror, check the Pull finished under **Jobs**.

**`load_dataset` hangs before it starts.** It is waiting for `datasets-server.<host>`; see [Datasets](../guides/datasets.md#the-datasets-server-name).

**Ollama says the server responded with an error over HTTP.** Ollama uses HTTPS unless you pass `--insecure`; see [Ollama](../guides/ollama.md).

**Pulls never start.** Check the pull windows (`pull.windows`) and whether a worker is running: processes in role `web` do not run Jobs.

**git push is rejected.** Mirrors are read-only, default branches refuse force-pushes, and non-LFS files over `git.max_blob_size` are refused. The rejection message says which.

## Logs

Set `telemetry.log_level: debug` for more detail, and `telemetry.log_format: text` to read logs in a terminal. Every log line inside a request has `trace_id` and `span_id`, which match the trace in your tracing backend, and `request_id`. A server error names its request ID, in the message and the `X-Request-Id` response header, and the `hangar` CLI prints it: search the logs for that `request_id` to find the cause.

## Still stuck

Open a GitHub issue with the output of `hangar version` and `hangar admin doctor`, and the log lines around the error, with secrets removed. [Get help](../project/support.md) says which form to use and what to include.
