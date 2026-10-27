# 02. Architecture

## Stack

| Concern | Choice | Notes |
|---|---|---|
| Language | Go, current stable release | Pin the exact version in `go.mod` (`toolchain`) and the Dockerfile. |
| HTTP | `net/http` with `github.com/go-chi/chi/v5` | chi handles the Hub's mixed path shapes; see "Routing". |
| Database access | `github.com/uptrace/bun` with `pgdriver` and `sqliteshim` over `modernc.org/sqlite` | Pure Go, so `CGO_ENABLED=0` static builds for every platform. |
| Migrations | `bun/migrate`, SQL files per dialect | `internal/db/migrations/{postgres,sqlite}/NNNN_name.tx.up.sql`: each migration runs, and is recorded, in one transaction. |
| Object storage | `github.com/minio/minio-go/v7` | Works with every S3-compatible store and supports presigning and multipart. |
| Git | `git` binary (2.39 or later) for fetch; `github.com/go-git/go-git/v6` (pinned pre-release) for push, server-side Commits and Mirror fetches | See [04](04-git.md). |
| OIDC | `github.com/coreos/go-oidc/v3` and `golang.org/x/oauth2` | |
| Passwords | `golang.org/x/crypto/argon2` (argon2id) | |
| OpenTelemetry | `go.opentelemetry.io/otel` SDK, `contrib/exporters/autoexport`, `otelhttp`, `otelslog` bridge | See [11](11-observability.md). |
| Logging | `log/slog` | JSON to stdout and OTLP. |
| CLI | `github.com/spf13/cobra` | One binary: `hangar server`, `hangar worker`, client commands. |
| Config | `github.com/knadh/koanf/v2` | YAML file, then `HANGAR_*` env vars, then flags. |
| GGUF parsing | Hangar's own header reader in `internal/index/gguf` over 1 MiB ranged reads, with `github.com/gpustack/gguf-parser-go` (MIT) for ggml type sizes and file type names | `gguf-parser-go` reads only local paths or URLs, not an `io.ReaderAt` (`docs/build/requests/w7.md` item 8); see [07](07-indexing-and-scanning.md). |
| Native API contract | OpenAPI 3.1 in `api/openapi.yaml`, types from `github.com/oapi-codegen/oapi-codegen/v2` | The UI client is generated from the same file. |
| Web UI | React, TypeScript, Vite, TanStack Router and Query, Tailwind CSS, shadcn/ui | Built assets embedded with `go:embed`. See [09](09-web-ui.md). |

Use the latest stable release of every dependency at implementation time and pin it exactly. Do not add a dependency without a line in `docs/dependencies.md` saying why.

## Processes

One binary, `hangar`, runs in one of three roles:

| Role | Serves | Runs Jobs | Scale |
|---|---|---|---|
| `all` (default) | Web UI, native API, Hub API, git, Ollama registry | Yes | The only valid role with SQLite. |
| `web` | Everything HTTP | No | Any number of replicas behind a load balancer. |
| `worker` | Health and metrics only | Yes | Any number of replicas. |

`web` processes are stateless. Everything shared lives in the database or the Blob store. Any request can hit any replica, including each request of a git push or an LFS multipart upload.

With SQLite, Hangar refuses to start a second process against the same database file: it holds an exclusive lock file next to the database and exits with a clear error.

```
               ┌──────────────── load balancer ────────────────┐
               │                                               │
        hangar --role=web (×N)                       hangar --role=worker (×M)
               │   │                                           │   │
               │   └────────────── Postgres ───────────────────┘   │
               │                (metadata, refs, jobs, sessions)   │
               └────────────────── Blob store ─────────────────────┘
                     (LFS objects, git data, upload staging)
   clients ──────► web replicas; LFS downloads redirect to presigned S3 URLs
```

## Repository layout

```
cmd/hangar/                 main package: cobra root, wires roles
internal/config/            config struct, loading, validation, reference generator
internal/db/                bun setup, dialect helpers, migrations, transactions
internal/blob/              Blob store interface + fs/ and s3/ backends + conformance tests
internal/gitstore/          git object storage, packs, refs (compare-and-swap)
internal/repo/              Repo service: create, commit, read trees, revisions, rename, delete
internal/auth/              users, sessions, passwords, OIDC, tokens, permission checks
internal/org/               Organisations, Members, Roles, collaborators
internal/hfapi/             Hub HTTP API handlers (read and write)
internal/gitserver/         git smart HTTP and the git-lfs API
internal/ollama/            Ollama registry endpoints
internal/upstream/          Upstream client, Discover search, Pull planner, Dependency resolver
internal/jobs/              Job queue, worker loop, leases, scheduling, rate limiting
internal/index/             metadata extraction (GGUF, safetensors, config.json, cards)
internal/scan/              pickle scanner and integrity checks
internal/webhook/           webhook registration and delivery
internal/audit/             audit events
internal/collections/       Collections
internal/community/         discussions and pull requests
internal/api/               native API handlers (implements generated OpenAPI interfaces)
internal/datasets/          Config and Split layout, Parquet export, statistics, query parser, Dataset Viewer API, Croissant
internal/web/               UI asset serving, SPA routing, CSP headers
internal/telemetry/         OTel setup, common attributes, metric instruments
internal/httpx/             middleware: request IDs, auth, errors, recovery, CORS, limits
internal/cli/               client commands (login, pull, push, lmstudio, ollama, import)
api/openapi.yaml            native API contract
ui/                         React app (pnpm), builds into internal/web/dist
compat/                     client compatibility suites (Python, git, Ollama)
deploy/compose/             Docker Compose examples
deploy/helm/hangar/         Helm chart
docs/                       user documentation (MkDocs Material) and this spec
```

Packages talk to each other through interfaces defined in their own package, never through each other's tables. `internal/repo` is the only package that writes Commits or refs; the HTTP API, git server, upstream Pulls and the UI all go through it.

## Core interfaces

These signatures are the contract between workstreams. Phase 0 of the build plan commits them as Go code; later phases implement them.

```go
// internal/blob
type Store interface {
    Stat(ctx context.Context, key string) (Info, error)                     // ErrNotFound
    Get(ctx context.Context, key string, rng *Range) (io.ReadCloser, Info, error)
    Put(ctx context.Context, key string, r io.Reader, size int64) error     // streams; size -1 if unknown
    Delete(ctx context.Context, key string) error
    List(ctx context.Context, prefix string, fn func(Info) error) error
    // Multipart: S3 native; fs emulates with part files under uploads/.
    CreateMultipart(ctx context.Context, key string) (uploadID string, err error)
    PresignPart(ctx context.Context, key, uploadID string, part int, ttl time.Duration) (url string, err error) // ErrNotSupported on fs
    PutPart(ctx context.Context, key, uploadID string, part int, r io.Reader, size int64) (etag string, err error)
    CompleteMultipart(ctx context.Context, key, uploadID string, parts []Part) error
    AbortMultipart(ctx context.Context, key, uploadID string) error
    PresignGet(ctx context.Context, key string, ttl time.Duration, filename string) (url string, err error) // ErrNotSupported on fs
    Copy(ctx context.Context, srcKey, dstKey string) error                  // server-side where possible
}

// internal/repo
type Service interface {
    Create(ctx context.Context, p CreateParams) (*Repo, error)
    Get(ctx context.Context, id RepoID) (*Repo, error)                       // follows renames
    Resolve(ctx context.Context, id RepoID, revision string) (CommitSHA, error)
    Tree(ctx context.Context, id RepoID, commit CommitSHA, path string, opts TreeOpts) ([]Entry, error)
    Entry(ctx context.Context, id RepoID, commit CommitSHA, path string) (Entry, error)
    OpenFile(ctx context.Context, id RepoID, commit CommitSHA, path string, rng *blob.Range) (io.ReadCloser, Entry, error)
    Commit(ctx context.Context, p CommitParams) (*CommitResult, error)       // ops: add, add LFS (by oid), delete, delete folder, copy
    Log(ctx context.Context, id RepoID, revision string, opts LogOpts) ([]CommitInfo, error)
    Refs(ctx context.Context, id RepoID) (Refs, error)
    UpdateRefs(ctx context.Context, id RepoID, updates []RefUpdate) error // atomic compare-and-swap of every ref; ErrRefConflict ([04](04-git.md))
    // plus Rename, Transfer, Delete, Restore, Settings
}

// internal/jobs
type Queue interface {
    Enqueue(ctx context.Context, j NewJob) (JobID, error)   // dedupe by (kind, key) while pending or running
    Cancel(ctx context.Context, id JobID) error
    Get(ctx context.Context, id JobID) (*Job, error)
}
type Handler func(ctx context.Context, j *Job, progress Progress) error
```

## Data model

All tables have `created_at` and `updated_at` (UTC). IDs are UUIDv7 unless stated. The same schema runs on SQLite and Postgres; JSON columns are `jsonb` on Postgres and `text` on SQLite.

| Table | Purpose | Key columns |
|---|---|---|
| `namespaces` | Users and Organisations share one name space | `name` (unique, case-insensitive), `kind` (`user`, `org`), `display_name`, `avatar_key`, `reserved_mirror` (bool, set when created by a Pull) |
| `users` | People | `namespace_id`, `email`, `password_hash` (null for OIDC-only), `is_admin`, `disabled_at` |
| `identities` | OIDC links | `user_id`, `provider`, `subject` (unique per provider), `email`, `claims` (json) |
| `sessions` | UI sessions | `id_hash`, `user_id`, `csrf_secret`, `expires_at`, `user_agent`, `ip` |
| `tokens` | API and git tokens | `user_id`, `name`, `hash` (SHA-256 of secret), `prefix` (first 8 chars, for display), `scopes`, `restrictions` (json), `expires_at`, `last_used_at` |
| `ollama_keys` | Ollama public keys | `user_id`, `public_key`, `fingerprint` |
| `upstream_tokens` | Users' tokens for Upstreams | `user_id`, `upstream_id`, `ciphertext` (AES-256-GCM with the Instance key) |
| `memberships` | Organisation members | `org_namespace_id`, `user_id`, `role` |
| `repos` | Repos | `namespace_id`, `name`, `type`, `visibility`, `default_branch`, `head_ref`, `description`, `settings` (json), `gating` (json), `deleted_at`, `size_bytes` (git data plus every LFS object referenced, held or not), `present_bytes` (git data plus the LFS objects held), `lfs_bytes` (LFS objects held) |
| `repo_redirects` | Renames and transfers | `old_namespace`, `old_name`, `type`, `repo_id` |
| `collaborators` | Per-repo access | `repo_id`, `user_id`, `role` |
| `refs` | Branches, tags, PR refs | `repo_id`, `name` (full, for example `refs/heads/main`), `target` (SHA-1); compare-and-swap on `target` ([04](04-git.md)) |
| `packs` | git packfiles per Repo | `repo_id`, `pack_sha1`, `size`, `object_count`, `state` (`pending`, `live`, `retired`, `orphaned`, `deleting`), `retired_at` |
| `commits` | Commit metadata cache for fast listing | `repo_id`, `sha`, `parents`, `author`, `committer`, `title`, `message`, `authored_at` |
| `tree_entries` | Flattened file list per indexed Commit, for fast Hub API listing | `repo_id`, `commit`, `path`, `type`, `size`, `git_oid`, `lfs_oid`, `lfs_size`, `last_commit` |
| `lfs_objects` | One row per stored LFS object | `oid` (SHA-256, primary key), `size`, `storage_key`, `verified_at`, `last_referenced_at` |
| `lfs_uploaders` | Users who uploaded an LFS object's bytes, so their Commits may reference it ([04](04-git.md) "Who may reference an LFS object") | `oid`, `user_id` |
| `repo_lfs` | Which Repos reference an LFS object (access control and GC) | `repo_id`, `oid`, `present` (bool, false for not-yet-pulled files in a Partial mirror) |
| `uploads` | In-progress multipart uploads | `oid`, `size`, `upload_id`, `storage_key`, `user_id`, `repo_id`, `parts` (json), `expires_at` |
| `upstreams` | Configured Upstreams | `name`, `base_url`, `kind` (`hf`), `enabled`, `instance_token_ciphertext` |
| `mirrors` | Mirror settings and state | `repo_id`, `upstream_id`, `upstream_repo`, `include` (globs), `exclude` (globs), `track` (bool), `track_ref`, `schedule`, `last_synced_commit`, `status`, `last_error` |
| `dependencies` | Repo to Repo links | `repo_id`, `commit`, `target_repo`, `target_revision`, `kind` (`code`, `base_model`, `adapter_base`, `card_base_model`, `dataset`), `required`, `source_path`, `source_key` |
| `metadata` | Index results | `repo_id`, `commit`, `summary` (json: formats, architecture, parameters, quantisations, context_length, licence, pipeline_tag, library_name, gguf, safetensors, card_data), `indexed_at`, `indexer_version` |
| `scan_results` | Scan results per file | `repo_id`, `commit`, `path`, `status` (`safe`, `suspicious`, `unsafe`, `error`), `findings` (json), `scanner_version` |
| `jobs` | Job queue | `kind`, `dedupe_key`, `payload` (json), `status`, `priority`, `attempts`, `max_attempts`, `run_after`, `lease_owner`, `lease_expires_at`, `progress` (json), `error`, `created_by` |
| `schedules` | Recurring jobs | `kind`, `key`, `cron`, `next_run_at` |
| `collections` | Collections | `namespace_id`, `slug`, `title`, `description`, `visibility`, `theme` |
| `collection_items` | Items | `collection_id`, `item_type`, `item_id`, `position`, `note` |
| `discussions` | Discussions and pull requests | `repo_id`, `num`, `kind`, `title`, `status`, `author_id`, `pr_ref` |
| `discussion_events` | Comments, status changes, commits | `discussion_id`, `type`, `author_id`, `body`, `data` (json) |
| `likes` | Likes | `user_id`, `repo_id` |
| `download_counts` | Daily download counters | `repo_id`, `day`, `count` |
| `access_requests` | Gated Repo requests | `repo_id`, `user_id`, `status`, `fields` (json) |
| `webhooks` | Webhooks | `scope_namespace_id`, `scope_repo_id`, `url`, `secret_ciphertext`, `events`, `enabled` |
| `webhook_deliveries` | Delivery log | `webhook_id`, `event`, `payload`, `status_code`, `attempt`, `next_attempt_at`, `duration_ms` |
| `audit_events` | Audit log | `actor_user_id`, `actor_token_id`, `action`, `target_type`, `target_id`, `ip`, `data` (json) |
| `quotas` | Storage limits | `namespace_id`, `bytes_limit` |
| `settings` | Instance settings editable in the admin UI | `key`, `value` (json) |
| `xet_xorbs` | Xet xorbs kept for client-side deduplication | `hash` (primary key), `size`, `storage_key`, `last_referenced_at` |
| `xet_files` | Xet file hash to LFS object | `xet_hash` (primary key), `oid` (SHA-256), `size`, `created_at` |
| `dataset_splits`, `dataset_stats` | Dataset Configs, Splits and statistics | See [14](14-datasets.md) |

Download counting follows the Hub's rule: count a download when a client fetches a Repo's `config.json` (models) or its first data file (datasets), not on every file, so one `from_pretrained` counts once.

## Blob store layout

The same keys on both backends, under an optional prefix:

```
lfs/sha256/<oid[0:2]>/<oid[2:4]>/<oid>       LFS objects, stored whole
git/<repo-uuid>/...                          git data; layout set in 04-git.md
uploads/<upload-uuid>/part-<n>               multipart staging (fs backend only)
tmp/<uuid>                                   streaming staging, swept after 24 hours
xet/xorbs/<hash>                             Xet xorbs, kept for xet.xorb_retention
avatars/<namespace-uuid>                     avatars
exports/<job-uuid>.zip                       generated zips, swept after 24 hours
```

The filesystem backend writes to a temporary file and renames it into place, so readers never see partial objects. It must work on NFS and SMB mounts (no reliance on `flock` across hosts; with the filesystem backend and more than one process, the database coordinates everything).

## Writes and consistency

- A Commit is visible when its ref update commits. Ref updates are compare-and-swap on `refs.target` in a database transaction, and pack bytes are durable in the Blob store before that transaction ([04](04-git.md)). Two concurrent pushes or HTTP commits to the same branch: one wins and the other gets the Hub's conflict error or git's non-fast-forward rejection.
- LFS objects are written and verified (SHA-256 and size) before any Commit that references them is accepted. A Commit that references a missing LFS object is rejected, except in Partial mirrors, where `repo_lfs.present = false` marks the gap.
- Garbage collection (a scheduled Job) deletes LFS objects with no `repo_lfs` rows whose `last_referenced_at` is older than the grace period (default 7 days), and unreachable git data per [04](04-git.md).

## Jobs

The `jobs` table is the queue on both databases.

- **Claim:** Postgres uses `SELECT ... FOR UPDATE SKIP LOCKED`. SQLite has one process, so the worker claims inside a transaction.
- **Leases:** a claimed Job has a lease (default 60 seconds), renewed while it runs. A Job whose lease expires goes back to the queue, and its handler must be idempotent.
- **Deduplication:** `(kind, dedupe_key)` is unique while pending or running. Pulling the same files twice joins the existing Job.
- **Kinds:** `pull`, `pull.file`, `mirror.sync`, `index`, `scan`, `gc`, `git.compact`, `lfs.verify`, `dataset.convert`, `dataset.stats`, `xet.gc`, `webhook.deliver`, `zip.export`, `repo.purge`, `upload.expire`.
- **Scheduling:** `schedules` rows create Jobs when `next_run_at` passes; one worker wins each run through the same claim.
- **Progress:** handlers report progress (bytes done, total, rate), which the UI polls through the native API.
- **Rate limiting:** Pull byte rates are limited per Instance with a token bucket whose budget is shared through the database (each worker leases a slice of the budget every few seconds), so the cap holds across replicas.

## Routing

Hangar serves several protocols on one host. Path shapes follow the Hub exactly so swapping a Hub host for a Hangar host works.

| Prefix or shape | Handler |
|---|---|
| `/api/hangar/v1/...` | Native API (OpenAPI) |
| `/api/...` | Hub HTTP API ([03](03-hf-api.md)) |
| `/{ns}/{name}/resolve/...`, `/datasets/{ns}/{name}/resolve/...` | Hub file downloads |
| `/{ns}/{name}.git/...`, `/{ns}/{name}/info/refs`, `/{ns}/{name}/git-upload-pack`, `/{ns}/{name}/git-receive-pack`, `/{ns}/{name}.git/info/lfs/...` (and `/datasets/` forms) | git and git-lfs ([04](04-git.md)) |
| `/v2/...` | Ollama registry ([08](08-local-tools.md)) |
| `/xet-cas/v1/...`, `/xet-cas/v2/...`, `/v1/telemetry` | Xet CAS upload endpoints ([03](03-hf-api.md)) |
| `/datasets-server/...`, and any path when `Host` is `datasets-server.<host>` | Dataset Viewer API ([14](14-datasets.md)) |
| `/healthz`, `/readyz` | Health |
| `/auth/...` | Login, logout, OIDC callbacks |
| `/assets/...` | UI assets |
| everything else | UI routes, mirroring Hub pages: `/{ns}/{name}`, `/{ns}/{name}/tree/{rev}/{path}`, `/{ns}/{name}/blob/{rev}/{path}`, `/{ns}/{name}/commit/{sha}`, `/datasets/...`, `/{ns}` |

Reserved Namespace names (refused at creation): `api`, `assets`, `auth`, `datasets`, `datasets-server`, `models`, `spaces`, `v2`, `healthz`, `readyz`, `settings`, `xet-cas`, `v1`, `admin`, `discover`, `arsenal`, `collections`, `new`, `login`, `logout`, `docs`, plus any name starting with `-` or `.`.

## Configuration

`hangar server --config hangar.yaml`. Every key is also an environment variable: `storage.s3.bucket` is `HANGAR_STORAGE_S3_BUCKET`. `hangar config reference` prints every key with its type, default and description, and the docs page is generated from it.

```yaml
server:
  public_url: https://hangar.example.com   # required; used in redirects, LFS hrefs, git URLs
  listen: :8080
  role: all                                 # all | web | worker
  trusted_proxies: [10.0.0.0/8]             # for X-Forwarded-For / -Proto
  max_request_body: 64MiB                   # for non-streaming endpoints

database:
  driver: sqlite                            # sqlite | postgres
  dsn: /var/lib/hangar/hangar.db            # or postgres://...
  max_open_conns: 20

storage:
  backend: fs                               # fs | s3
  fs:
    path: /var/lib/hangar/blobs
  s3:
    endpoint: s3.amazonaws.com
    region: eu-west-2
    bucket: hangar
    prefix: ""
    access_key_id: ""                       # or AWS default credential chain / IRSA
    secret_access_key: ""
    use_path_style: false
    download_mode: redirect                 # redirect | proxy
    presign_ttl: 1h
    retries: 5                              # resend a failed part or object upload; 0 = never
    stall_timeout: 2m                       # cancel and retry a request that makes no progress
    startup_wait: 5m                        # keep trying to reach the bucket at start; 0 = exit at once

security:
  instance_key: ""                          # 32-byte base64; encrypts stored secrets; required
  anonymous_read: false
  registration: closed                      # open | invite | closed
  local_login: true
  session_ttl: 720h
  bootstrap_admin:                          # created on first start if no admin exists
    username: hangar-admin
    password: ""                            # or password_file
    email: ""

oidc:
  providers:
    - id: unifi
      name: UniFi
      issuer: https://sso.example.com
      client_id: hangar
      client_secret: ""
      scopes: [openid, profile, email, groups]
      claims: { username: preferred_username, email: email, name: name, groups: groups }
      admin_groups: [hangar-admins]
      org_mappings:
        - { group: ml-team, org: ml, role: write }
      auto_create_users: true

upstreams:
  huggingface:
    base_url: https://huggingface.co
    enabled: true
    token: ""                               # optional Instance token for public pulls
pull:
  bandwidth_limit: 0                        # bytes/s across the Instance; 0 = unlimited
  concurrency: 4                            # files at once across the Instance
  windows: []                               # e.g. ["01:00-07:00"] in server.timezone
  stall_timeout: 2m                         # cut and retry an upstream request that answers or sends nothing
  pull_through:
    enabled: false
    allow: ["*"]                            # globs of upstream repo IDs
  dependencies:
    auto_pull_code: true
    auto_pull_base_models: false
    max_depth: 5

git:
  max_blob_size: 10MiB                      # non-LFS file limit on push
  max_pack_size: 2GiB
  allow_force_push: false                   # for default branches; admins always can
  compact_threshold: 20
  pack_grace_period: 1h
  cache_dir: /var/cache/hangar/git
  cache_size: 10GiB
  timeout: 1h

lfs:
  multipart_threshold: 64MiB
  upload_url_ttl: 24h
  verify_wait: 10m

hfapi:
  regular_upload_max: 10MiB                 # preupload answers "regular" below this unless .gitattributes says LFS
  xet: ingest                               # ingest | reject; see 03-hf-api.md

xet:
  cas_url: ""                               # default <public_url>/xet-cas
  token_ttl: 1h
  xorb_retention: 672h                      # 28 days, longer than hf_xet's 21-day shard cache

jobs:
  workers: 4                                # concurrent Jobs per worker process
  lease: 60s

gc:
  schedule: "0 3 * * *"
  grace_period: 168h

telemetry:                                  # standard OTEL_* env vars also apply and win
  service_name: hangar
  traces: otlp                              # otlp | console | none
  metrics: otlp                             # otlp | prometheus | console | none
  logs: otlp
```

Startup validates the whole config and exits with every error at once, not just the first.

## Errors and limits

- Hub API errors use the Hub's status codes, `X-Error-Code` and `X-Error-Message` headers and JSON body ([03](03-hf-api.md)).
- Native API errors use RFC 9457 problem details.
- Every streaming endpoint has no body size limit apart from quotas; every buffered endpoint has `server.max_request_body`.
- Timeouts: read header 30 seconds; streaming endpoints have no total timeout but an idle timeout of 5 minutes. Request bodies of buffered endpoints also have a total timeout: 30 seconds for sign-in, registration and the other routes under `/auth/` and `/api/hangar/v1/auth/`, and 2 minutes for the rest of the native API.

## Scale targets

These are the v1.0 targets. The load tests in [13](13-testing.md) check them.

| Measure | Target |
|---|---|
| Repos per Instance (Postgres) | 100,000 |
| Files per Repo | 100,000 |
| `HEAD /resolve` p99 at 200 requests/s per replica | under 50 ms, excluding upstream Pulls |
| Server memory during a 50 GiB upload or download | flat, under 256 MiB per replica |
| Download throughput in `proxy` mode | limited by network and storage, not CPU |
| Web replica start to ready | under 5 seconds |
