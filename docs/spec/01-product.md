# 01. Product

## What Hangar is

Hangar is a self-hosted model hub for your arsenal of models. It stores models and datasets with full version history and serves them through the interfaces people already use:

- the Hugging Face Hub HTTP API, so `huggingface_hub`, the `hf` CLI, `transformers`, `diffusers`, `sentence-transformers`, `datasets` and vLLM work by setting `HF_ENDPOINT`
- git and git-lfs, so `git clone` and `git push` work
- the Ollama registry protocol, so `ollama pull <hangar-host>/<org>/<repo>:<quant>` works for GGUF repos
- the `hangar` CLI, which also installs models into LM Studio and Ollama

It pulls from huggingface.co (or any compatible Upstream), resolves the Dependencies a model needs, and accepts uploads of models that exist nowhere else.

Datasets are first-class too: discover them, pull just the Config and Split you need (or only its Parquet export), preview and query rows in the browser, and load them with `datasets`, pandas, Polars or DuckDB (over HTTPS). See [14](14-datasets.md).

It runs as one container for a laptop or homelab, and as stateless replicas over Postgres and S3 when you need more.

## Who it is for

| Persona | Needs |
|---|---|
| **Tinkerer** (the first user) | Pulls lots of models and datasets to try. Wants them kept on fast local storage, browsable, and quick to load into LM Studio or Ollama without downloading again. |
| **ML team** | Shares private fine-tunes and datasets with versioning, permissions and SSO. Points existing code at Hangar without changing it. |
| **Platform team** | Runs it for many users: S3, Postgres, horizontal scaling, OpenTelemetry, Helm, audit log. Often in an air-gapped network fed from an Upstream. |

## Principles

1. **Compatible by default.** If it works against huggingface.co, it works against Hangar with `HF_ENDPOINT` set. Hangar URLs mirror Hugging Face URLs, so swapping the host in any Hub URL works. Deviations are bugs.
2. **Git is the source of truth.** Repos are real git repos. The HTTP API, git and the UI are three views of the same Commits.
3. **Your data stays readable.** LFS objects are stored whole, addressed by SHA-256, never chunked or transformed. An admin can recover any file from the Blob store with standard tools.
4. **One container to start, replicas to scale.** SQLite and a local directory for the quickstart. Postgres and S3 for scale. Same binary.
5. **Careful with the network.** Pulls are queued, resumable, rate-limited and can be confined to time windows, so a big Pull never saturates a home connection.
6. **Observable.** Traces, metrics and logs go out over OpenTelemetry.

## Language and tone

Docs and UI copy use the house style: Google developer documentation style, British English, no em dashes.

The arsenal theme appears in the README tagline, the landing page, empty states and a few labels. It must never make an instruction harder to follow, and it never appears in API names.

| Place | Copy |
|---|---|
| Tagline | Hangar: a self-hosted home for your arsenal of models. |
| Main library views | **Models** and **Datasets**, two separate lists of what is in the arsenal |
| Upstream search view | **Discover** |
| Pull button | **Pull into arsenal** |
| Empty Models list | Your arsenal has no models yet. Discover a model to pull, or upload your own. |
| Empty Datasets list | Your arsenal has no datasets yet. Discover a dataset to pull, or upload your own files. |
| Repo badge for a Mirror | **Mirrored from huggingface.co** |
| Dataset preview not ready | Preparing preview. Hangar is converting this dataset to Parquet. |
| Partial mirror file | **Not in arsenal** (with a Pull action) |

## Feature list

Every feature below is in scope for v1.0. IDs are referenced from the build plan and tests.

### Repos and versioning (REPO)

- **REPO-1** Model and dataset Repos under user and Organisation Namespaces, with IDs `namespace/name`.
- **REPO-2** Visibility: `public` (anyone, including anonymous users if the Instance allows anonymous reads), `internal` (any signed-in user), `private` (Namespace members and explicit collaborators).
- **REPO-3** Git-native history: branches, tags, Commits, Commit detail with file-level diff (text diff for files under 1 MiB, size and SHA change for others).
- **REPO-4** Create, rename and transfer Repos between Namespaces, with redirects from the old ID (HTTP API, git and UI).
- **REPO-5** Delete Repos: soft delete with a 7-day restore window (configurable), then purge.
- **REPO-6** Pull requests and discussions compatible with the Hub (`refs/pr/N`, `create_pr=True`, comments, merge, close).
- **REPO-7** Likes and download counts (per Repo, per day).
- **REPO-8** Gated Repos: the owner can require users to request access, and admins approve or reject requests.
- **REPO-9** Repo settings: description, default branch, tags, licence, gating, visibility.

### Uploading (UP)

- **UP-1** Upload through the Hub HTTP API (`upload_file`, `upload_folder`, `create_commit`, `upload_large_folder`), including LFS multipart for large files.
- **UP-2** Upload through `git push` with git-lfs.
- **UP-3** Upload in the web UI: drag and drop files or folders, resumable, parallel multipart straight to the Blob store, with a commit message and branch choice.
- **UP-4** Upload with `hangar push <dir> <repo>`.
- **UP-5** Large files: a single file up to 5 TiB (the S3 object limit); no request buffers a whole file in memory or on local disk.
- **UP-7** Xet uploads: `huggingface_hub` with `hf_xet` installed (the default) uploads with no client setup. Hangar accepts the Xet protocol and stores whole files ([03](03-hf-api.md)).
- **UP-6** Deduplication: an LFS object that already exists anywhere in the Instance is never stored twice. A Commit may still reference it only when its author uploaded the bytes or can read a Repo that holds it ([04](04-git.md) "Who may reference an LFS object").

### Downloading (DOWN)

- **DOWN-1** Hub API downloads (`hf_hub_download`, `snapshot_download`, `from_pretrained`), with ranges and resume.
- **DOWN-2** On S3, LFS downloads redirect to presigned URLs, so file bytes do not pass through Hangar. Configurable: `redirect` or `proxy`.
- **DOWN-3** `git clone`, `git fetch` and `git lfs pull`, including shallow clones.
- **DOWN-4** `ollama pull` for GGUF Repos (see [08](08-local-tools.md)).
- **DOWN-5** Browser downloads of single files, and of a whole Revision as a zip, streamed.

### Upstreams and pulling (PULL)

- **PULL-1** Upstreams: huggingface.co is built in. Admins can add others (another Hangar, a corporate mirror) and disable any of them.
- **PULL-2** Pull a whole Repo, or chosen files or patterns, into the Arsenal as a Mirror that keeps the upstream ID and Commit SHAs.
- **PULL-3** Partial mirrors: full git history, chosen LFS objects only. Missing files show as "not in arsenal" and can be pulled later.
- **PULL-4** GGUF quant picker: group a Repo's GGUF files by quantisation (including split files `-00001-of-0000N`) and pull one or more.
- **PULL-5** Jobs show progress (bytes, rate, ETA), can be cancelled and retried, and resume after restarts.
- **PULL-6** Integrity: every pulled LFS object is checked against its SHA-256 before it becomes visible.
- **PULL-7** Network controls: an Instance-wide bandwidth cap, concurrency limits and optional time windows ("only pull between 01:00 and 07:00").
- **PULL-8** Gated and private upstream Repos: users store their own upstream token. Pulls use the requesting user's token.
- **PULL-9** Tracking: a Mirror can follow an upstream branch on a schedule, and pull new Commits (and new versions of its selected files) automatically.
- **PULL-10** Pull-through: optional, off by default. A Hub API or git request for a missing Repo that matches the allowlist starts a Pull and serves files as they arrive.
- **PULL-11** Dependencies: code dependencies from `auto_map` are pulled automatically and recursively. Base models and other suggested dependencies are listed with a one-click Pull.
- **PULL-12** Bulk import: `hangar import --from-hf-cache` and `--from-dir` add models you already have on disk without downloading them again.

### Browsing (BROWSE)

- **BROWSE-1** Models and Datasets views: everything you can see, as two separate lists that never mix the types, each a grid or list with search and filters: for models task, library, format (GGUF, safetensors, MLX, ONNX, PyTorch), quantisation, parameter count, context length, size, licence, Namespace, and Mirror or local; for datasets the facets in [14](14-datasets.md) DATA-11. A model can name the datasets it uses, and a dataset lists the models that use it; these links are optional.
- **BROWSE-2** Discover view: search Upstreams with the same filters, with each result marked **In arsenal**, **Partly in arsenal** or not, as LM Studio does.
- **BROWSE-3** Repo page: rendered Model card, metadata sidebar, Files tab, Commits tab, branches and tags, Community tab, Dependencies panel and a **Use this model** panel with snippets for every client.
- **BROWSE-4** File views: text and code with highlighting, rendered Markdown, JSON, images, and header viewers for GGUF and safetensors files that never download the whole file.
- **BROWSE-5** Collections: curated, ordered lists of Repos, compatible with the Hub collections API.
- **BROWSE-6** Global search with a keyboard shortcut (`/` or Cmd-K) across Repos, Namespaces and Collections.

### Users and access (AUTH)

- **AUTH-1** Local accounts with username and password (argon2id), self-registration off by default, invite links, and admin password reset.
- **AUTH-2** OIDC: one or more providers, with PKCE, claim mapping, group-to-Organisation and Role mapping, and just-in-time accounts.
- **AUTH-3** Local login can be turned off when OIDC is on, except for a break-glass admin.
- **AUTH-4** Tokens: `read`, `write` and `admin` scopes, optionally limited to some Namespaces or Repos, with an optional expiry and last-used time. Sent as a bearer token (HTTP API) or a password (git).
- **AUTH-5** Organisations with Members and Roles; repo collaborators.
- **AUTH-6** Instance admins; first admin bootstrapped from configuration.
- **AUTH-7** Ollama keys: users register their Ollama public key so `ollama pull` of a private Repo works, as on the Hub.

### Indexing and security (META)

- **META-1** Index every Commit on every branch head: file formats, architecture, parameter count, dtype breakdown, quantisation, context length, chat template, tokenizer, licence, task and library. Exposed in the Hub API (`safetensors`, `gguf`, `cardData`, `pipeline_tag`, `library_name`) and the UI.
- **META-2** Pickle scan: flag dangerous imports in `.bin`, `.pt`, `.pth`, `.ckpt` and `.pkl` files, including inside zip-based PyTorch archives. Shown on files and Repos and exposed as `securityStatus`.
- **META-3** Integrity checks: safetensors headers parse, GGUF headers parse, LFS pointer sizes match objects.

### Datasets (DATA)

- **DATA-1** to **DATA-13**: dataset Configs and Splits, Parquet export, a Dataset Viewer API compatible with the Hub's, column statistics, the Viewer tab, Discover and partial Pulls for datasets, dataset metadata, Croissant, facets, CLI support and a card editor. Specified in [14](14-datasets.md).

### Integrations and events (EVT)

- **EVT-1** Webhooks for Repo, Pull and discussion events, signed with HMAC-SHA256, retried with backoff, with a delivery log.
- **EVT-2** Audit log of security-relevant actions, viewable by admins and exported as OpenTelemetry logs.

### Operations (OPS)

- **OPS-1** Blob store backends: local filesystem and S3-compatible (AWS S3, MinIO, Ceph RGW, Cloudflare R2, Backblaze B2, Garage).
- **OPS-2** Databases: SQLite (single replica) and Postgres (any number of replicas).
- **OPS-3** Process roles: `all`, `web` and `worker`, scaled independently.
- **OPS-4** Quotas per Namespace, storage usage reporting, and garbage collection of unreferenced LFS objects and git data.
- **OPS-5** OpenTelemetry traces, metrics and logs; health and readiness endpoints.
- **OPS-6** Configuration through a YAML file, environment variables and flags, with a generated reference.
- **OPS-7** Container images for linux/amd64 and linux/arm64, signed, with an SBOM. Docker Compose examples and a Helm chart.
- **OPS-8** Backup and restore commands and documentation for each backend combination.
- **OPS-9** Admin UI: users, Organisations, Upstreams, storage, quotas, Jobs, webhooks, audit log and Instance settings.

## Non-goals for v1.0

- Spaces (hosted apps) and the Inference API. Hangar stores and serves files; it does not run models.
- Xet downloads and global Xet chunk deduplication. Hangar accepts Xet uploads but stores whole files and serves them over plain HTTP ([03](03-hf-api.md)).
- SSH git transport. Git is over HTTPS only.
- Email delivery. Invites and resets use links that admins copy.
- Billing and per-request rate limiting. Put a reverse proxy in front if you need limits.
- Docker Model Runner and ModelPack OCI artefacts. The Ollama endpoint covers the OCI use case for v1.0.
