# Hangar

A self-hosted model and dataset hub, compatible with the Hugging Face Hub API, git, Ollama and LM Studio: one place to keep your arsenal of machine learning models and datasets.

## Language

### The instance

**Instance**:
One running deployment of Hangar, made of one or more server and worker processes sharing a database and a blob store.
_Avoid_: Server (when the whole deployment is meant), cluster, site

**Arsenal**:
Everything stored in an Instance that a user can see. Used in the UI and docs, never in API names. The UI lists it in two sections, **Models** and **Datasets**.
_Avoid_: Library, catalogue (when local content is meant)

**Namespace**:
The owner part of a repo ID: either a User or an Organisation. Names are unique across both.
_Avoid_: Owner, account

**User**:
A person who signs in, locally or through OIDC. Every User has a personal Namespace.

**Organisation**:
A Namespace that Users join as Members with a Role.
_Avoid_: Team, group, org (in prose; `org` is fine in code)

**Role**:
A Member's permission level in an Organisation: `read`, `write` or `admin`.

### Content

**Repo**:
A versioned set of files with a type (`model` or `dataset`) and an ID of the form `namespace/name`. Every Repo is a git repository.
_Avoid_: Model (when the container is meant), project

**Revision**:
Anything that names a Commit: a branch, a tag or a commit SHA.

**Commit**:
A real git commit in a Repo. Commit SHAs are git SHA-1 values, as on the Hugging Face Hub.

**LFS object**:
A large file's content, stored once per Instance and addressed by its SHA-256. Git trees hold a pointer file instead.
_Avoid_: Blob (in prose; "blob" means a git blob)

**Blob store**:
Where LFS objects and git data live: a local filesystem or an S3-compatible bucket.
_Avoid_: Bucket, disk (when the abstraction is meant)

**Model card**:
A Repo's `README.md`, with optional YAML front matter (`cardData`).

### Datasets

**Config**:
A named subset of a dataset Repo, from the card's `configs:` YAML or inferred from file layout.
_Avoid_: Subset, configuration (in Hangar's UI and docs)

**Split**:
A named part of a Config, such as `train` or `test`.

**Parquet export**:
Parquet shards of every Config and Split on the `refs/convert/parquet` branch.
_Avoid_: Conversion, cache

**Dataset Viewer**:
The service that previews, searches, filters and summarises dataset rows, with an API compatible with the Hub's.
_Avoid_: datasets-server (except when naming the hostname)

### Getting models and datasets in

**Upstream**:
Another Hugging Face-compatible hub that Hangar can fetch from, such as huggingface.co or another Hangar Instance.
_Avoid_: Remote, source, registry

**Mirror**:
A Repo that tracks a Repo on an Upstream, keeping the upstream ID and Commit SHAs. A Mirror can hold every file or only selected files.
_Avoid_: Cache, copy, fork

**Partial mirror**:
A Mirror that holds the full git history but only some LFS objects, for example one GGUF quantisation. Files that are not held are "not present".

**Pull**:
Fetching a Repo, or some of its files, from an Upstream into the Arsenal. Pulls run as Jobs.
_Avoid_: Download (when fetching from an Upstream is meant), sync

**Pull-through**:
An optional mode where a client's request for a Repo that is not in the Arsenal starts a Pull automatically.
_Avoid_: Proxy cache

**Dependency**:
A link from one Repo to another that it needs or builds on. A **code dependency** (from `auto_map`) is required and pulled automatically. A **base model** dependency (from adapters or model cards) is suggested, not pulled.

**Discover**:
The UI view that searches Upstreams and marks what is already in the Arsenal.

### Work and events

**Job**:
A unit of background work run by a worker: Pull, index, scan, garbage collection, webhook delivery.

**Index**:
Metadata extracted from a Commit's files: formats, architecture, parameter count, quantisation, context length, licence.

**Scan**:
A security check on a Commit's files, such as finding dangerous imports in pickle files.

**Collection**:
A named, ordered list of Repos that a Namespace curates.
_Avoid_: Loadout, list

**Webhook**:
An HTTP callback that a Namespace or Repo admin registers for events.

**Token**:
A secret that a client presents as a bearer token or git password. Tokens have scopes and an optional expiry.
_Avoid_: API key, PAT
