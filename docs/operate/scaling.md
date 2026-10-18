# Scaling

One binary runs in one of three roles (`server.role`, or `hangar server --role`):

| Role | Serves | Runs Jobs | Scale |
|---|---|---|---|
| `all` (default) | Web UI, native API, Hub API, git, Ollama | Yes | The only role for SQLite. |
| `web` | Everything HTTP | No | Any number of replicas behind a load balancer. |
| `worker` | Health and metrics only | Yes | Any number of replicas. |

`web` processes are stateless: everything shared lives in Postgres or the Blob store, so any request can reach any replica, including each request of a git push or a multipart upload. Run them behind a load balancer that does not buffer request or response bodies and has no total request timeout, because uploads and downloads stream.

## What a scaled-out Instance needs

- Postgres and an S3-compatible bucket (or a shared filesystem path).
- The same `security.instance_key` on every process.
- `hangar admin migrate` before new versions start.
- `jobs.workers` Jobs run concurrently per worker process (4 by default). Pull transfers are also capped across the Instance by `pull.concurrency`.

The [scaled Compose example](../get-started/compose.md#scaled-out) and the [Helm chart](../get-started/helm.md#scaled-out-with-postgres-and-s3) show complete setups.

## Targets

On the reference setup (two `web` replicas with 2 vCPU and 1 GiB each, one `worker`, Postgres and MinIO), Hangar aims for:

| Measure | Target |
|---|---|
| Repos per Instance | 100,000 |
| Files per Repo | 100,000 |
| `HEAD /resolve` p99 at 200 requests per second per replica | under 50 ms |
| Memory during a 50 GiB upload or download | flat, under 256 MiB per replica |
| Web replica start to ready | under 5 seconds |

The load tests in [`loadtest/`](https://github.com/hangar-hub/hangar/tree/main/loadtest) measure them.
