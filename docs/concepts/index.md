---
description: "The words Hangar uses in its UI, API and docs, such as Instance, Arsenal, Repo, Revision, Mirror, Upstream, Pull and Job."
---

# Concepts

The words Hangar uses, in its UI, API and these docs.

## The Instance

| Term | Meaning |
|---|---|
| **Instance** | One running deployment of Hangar: one or more server and worker processes sharing a database and a Blob store. |
| **Arsenal** | Everything stored in an Instance that you can see. The UI lists it in two sections, **Models** and **Datasets**. |
| **Namespace** | The owner part of a Repo ID: a User or an Organisation. Names are unique across both. |
| **User** | A person who signs in, locally or through OIDC. Every User has a personal Namespace. |
| **Organisation** | A Namespace that Users join as Members with a Role. |
| **Role** | A Member's permission level in an Organisation: `read`, `write` or `admin`. See [Permissions](permissions.md). |
| **Token** | A secret that a client presents as a bearer token or git password, with a scope and an optional expiry. See [Permissions](permissions.md#tokens). |

## Content

| Term | Meaning |
|---|---|
| **Repo** | A versioned set of files, of type `model` or `dataset`, with an ID `namespace/name`. Every Repo is a git repository. See [Repos and Revisions](repos.md). |
| **Revision** | Anything that names a Commit: a branch, a tag or a Commit SHA. |
| **Commit** | A real git commit in a Repo. Commit SHAs are git SHA-1 values, as on the Hugging Face Hub. |
| **LFS object** | A large file's content, stored once per Instance and addressed by its SHA-256. git trees hold a pointer file instead. |
| **Blob store** | Where LFS objects and git data live: a local folder or an S3-compatible bucket. See [Storage backends](../operate/storage.md). |
| **Model card** | A Repo's `README.md`, with optional YAML front matter. |
| **Collection** | A named, ordered list of Repos that a Namespace curates. See [Collections](../guides/collections.md). |

## Datasets

| Term | Meaning |
|---|---|
| **Config** | A named subset of a dataset Repo, from the card's `configs:` YAML or inferred from its file layout. |
| **Split** | A named part of a Config, such as `train` or `test`. |
| **Parquet export** | Parquet files of every Config and Split, on the `refs/convert/parquet` branch. |
| **Dataset Viewer** | The service that previews, searches, filters and summarises dataset rows, with an API compatible with the Hub's. See [Datasets](../guides/datasets.md). |

## Getting models and datasets in

| Term | Meaning |
|---|---|
| **Upstream** | Another Hugging Face-compatible hub Hangar can fetch from, such as huggingface.co or another Hangar Instance. See [Mirrors and Upstreams](mirrors.md). |
| **Mirror** | A Repo that tracks a Repo on an Upstream, keeping its ID and Commit SHAs. A tracking Mirror's check for new upstream Commits is a Mirror sync. |
| **Partial mirror** | A Mirror with the full git history but only some LFS objects, such as one GGUF quantisation. Files it does not hold are not present. |
| **Pull** | Fetching a Repo, or some of its files, from an Upstream into the Arsenal. See [Pulls and Dependencies](pulls.md). |
| **Pull-through** | An optional mode where a client's request for a Repo that is not in the Arsenal starts a Pull automatically. |
| **Dependency** | A link from one Repo to another it needs (code) or builds on (a base model). |
| **Discover** | The UI view that searches Upstreams and marks what is already in the Arsenal. |

## Work and events

| Term | Meaning |
|---|---|
| **Job** | Background work, such as a Pull, indexing or garbage collection. See [Jobs](jobs.md). |
| index | Metadata Hangar reads from a Commit's files: formats, architecture, parameter count, quantisation, context length and licence. |
| scan | A security check on a Commit's files, such as finding dangerous imports in pickle files. See [Security hardening](../operate/security.md#blocking-unsafe-downloads). |
| webhook | An HTTP callback that a Namespace or Repo admin registers for events. See [Webhooks](../guides/webhooks.md). |
