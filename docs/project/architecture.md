---
description: "How Hangar is built: one Go binary with an embedded React UI, git as the one source of truth, every protocol on one host, and background Jobs."
---

# Architecture

Hangar is one Go binary with an embedded React UI. The full design is in the [specification](https://github.com/hangar-hub/hangar/tree/main/docs/spec); this is the short version.

## Processes

```text
               ┌──────────────── load balancer ────────────────┐
               │                                               │
        hangar --role=web (×N)                       hangar --role=worker (×M)
               │   │                                           │   │
               │   └────────────── Postgres ───────────────────┘   │
               │                (metadata, refs, Jobs, sessions)   │
               └────────────────── Blob store ─────────────────────┘
                     (LFS objects, git data, upload staging)
   clients ──────► web replicas; LFS downloads redirect to presigned S3 URLs
```

`web` processes are stateless; everything shared lives in the database or the Blob store. Role `all` runs both in one process, with SQLite. See [Scaling](../operate/scaling.md).

## One source of truth: git

Every Repo is a git repository. The Hub HTTP API, git smart HTTP and the web UI are views of the same Commits, and one package (`internal/repo`) is the only code that writes Commits or refs. Ref updates are compare-and-swap in a database transaction, after the pack bytes are durable in the Blob store, so two concurrent pushes to a branch cannot both win, on any replica.

git packs live in the Blob store and are cached on local disk per replica. Large files are LFS objects stored whole, addressed by SHA-256, and deduplicated across the Instance.

## Protocols on one host

| Path | Served by |
|---|---|
| `/api/hangar/v1/...` | Native API (OpenAPI) |
| `/api/...`, `/{ns}/{name}/resolve/...` | Hugging Face Hub API |
| `/{ns}/{name}.git/...` | git smart HTTP and git-lfs |
| `/v2/...` | Ollama registry |
| `/xet-cas/...` | Xet uploads |
| `/datasets-server/...`, or the `datasets-server.<host>` name | Dataset Viewer API |
| Everything else | The web UI, with the same page URLs as huggingface.co |

## Background work

Pulls, indexing, scans, garbage collection, Parquet exports and webhook deliveries run as Jobs from a queue in the database, with leases, deduplication and schedules. Pull bandwidth is limited by a token bucket shared through the database, so the cap holds across workers.

## Packages

| Package | Does |
|---|---|
| `internal/repo`, `internal/gitstore` | Repos, Commits, refs and git storage |
| `internal/hfapi` | The Hub HTTP API |
| `internal/gitserver` | git smart HTTP and git-lfs |
| `internal/upstream` | Upstreams, Discover, Pulls and Dependencies |
| `internal/jobs`, `internal/gc` | The Job queue, workers and garbage collection |
| `internal/blob` | The filesystem and S3 Blob stores |
| `internal/auth`, `internal/org`, `internal/audit` | Users, sessions, Tokens, OIDC, Organisations and the audit log |
| `internal/index`, `internal/scan` | Metadata extraction and security scanning |
| `internal/datasets` | Configs, Splits, Parquet export and the Dataset Viewer |
| `internal/ollama`, `internal/xet` | The Ollama registry and Xet uploads |
| `internal/cli` | The client and admin commands |
| `ui/` | The web UI |
