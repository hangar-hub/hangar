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

It exits with 1 if any check fails, so you can use it in scripts.

## Common problems

**The server will not start and lists configuration errors.** Hangar checks the whole configuration and lists every problem at once. `hangar config validate` does the same without starting.

**"database is in use by another Hangar process".** SQLite allows one process. Stop the other one, or move to Postgres to run several.

**Every admin is locked out.** On the server host, set a new password with `hangar admin reset-password <username>`, which prints a generated password once (or reads one with `--password-stdin`), signs the user out everywhere and clears sign-in throttling. `hangar admin create-user <username> --admin` creates another Instance admin. With SQLite, stop the server first.

**Index or scan results look out of date.** `hangar admin reindex` queues index and scan Jobs for every branch head, or for the Repos you name; the workers run them.

**`/readyz` answers 503.** The body lists the failed checks: usually the database, a pending migration (run `hangar admin migrate`) or the Blob store.

**Downloads fail with a 403 from the store, or a TLS error.** In S3 `redirect` mode, clients download straight from the store, so they must reach `storage.s3.endpoint` and trust its certificate, and the clocks must agree. Use `download_mode: proxy` if clients cannot reach the store.

**`huggingface_hub` says a Repo does not exist.** Private Repos answer as missing without a Token that can read them. Check `HF_TOKEN` and `hangar whoami`. For a Mirror, check the Pull finished under **Jobs**.

**`load_dataset` hangs before it starts.** It is waiting for `datasets-server.<host>`; see [Datasets](../guides/datasets.md#the-datasets-server-name).

**Ollama says the server responded with an error over HTTP.** Ollama uses HTTPS unless you pass `--insecure`; see [Ollama](../guides/ollama.md).

**Pulls never start.** Check the pull windows (`pull.windows`) and whether a worker is running: processes in role `web` do not run Jobs.

**git push is rejected.** Mirrors are read-only, default branches refuse force-pushes, and non-LFS files over `git.max_blob_size` are refused. The rejection message says which.

## Logs

Set `telemetry.log_level: debug` for more detail, and `telemetry.log_format: text` to read logs in a terminal. Every log line inside a request has `trace_id` and `span_id`, which match the trace in your tracing backend.
