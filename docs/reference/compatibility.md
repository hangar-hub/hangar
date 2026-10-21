---
title: Tested clients, versions and platforms
description: "The client, library, database and platform versions Hangar's CI tests against on every pull request and nightly, and what is documented but untested."
---

# Tested versions and platforms

Hangar's CI runs the client compatibility suites in [`compat/`](https://github.com/hangar-hub/hangar/tree/main/compat) on every pull request and every night, against a real `hangar` binary as well as the Go test server ([`compat.yml`](https://github.com/hangar-hub/hangar/blob/main/.github/workflows/compat.yml)). This page lists the versions they use, as of Hangar 1.2.3. Each release's docs show the versions that release was tested with.

For each Hugging Face Hub endpoint and whether Hangar serves it, see [Hub API compatibility](hub-api-compatibility.md).

## Hugging Face clients

| Client | Tested | Runs |
|---|---|---|
| `huggingface_hub` and the `hf` CLI | 2.2.0, the latest, and 1.31.0, the oldest | Every pull request and nightly |
| Python | 3.10 (with `huggingface_hub` 1.31.0), 3.11 (the default) and 3.14 | Every pull request and nightly |
| `hf_xet` | 1.7.0 with the whole suite; 1.6.0 and 1.7.0 in the Xet upload suite; and the whole suite again without `hf_xet` installed | Every pull request and nightly |
| `transformers` | 5.19.0 | Every pull request and nightly |
| `sentence-transformers` | 5.7.0 | Every pull request and nightly |
| `datasets` | 5.1.0 | Every pull request and nightly |
| `torch` | 2.14.1 | Every pull request and nightly |
| `diffusers` | 0.41.0, with `huggingface_hub` 1.33.0 | Nightly only |

1.31.0 is the oldest `huggingface_hub` that `transformers` 5.19.0 and `datasets` 5.1.0 accept. Older versions may work, but nothing tests them.

The suite runs against both databases, SQLite and Postgres, and with Xet uploads both on and off. Uploads of 200 MB files run on every pull request, and of a 6 GiB file nightly.

## git, Ollama and LM Studio

| Client | Tested | Notes |
|---|---|---|
| git and git-lfs | git 2.39 and git-lfs from Debian 12, and the versions on GitHub's Ubuntu 24.04 runners | git 2.39 is also the oldest version the server needs. |
| Ollama | 0.31.1, the `ollama/ollama` image | Hangar's Ollama registry follows this release's client behaviour. |
| LM Studio | The folder layout that `hangar lmstudio install` writes | LM Studio is a desktop app, so loading a model in it is a manual check that [`compat/lmstudio`](https://github.com/hangar-hub/hangar/blob/main/compat/lmstudio/README.md) describes. |

## Databases, storage and deployment

| Component | Tested | Notes |
|---|---|---|
| SQLite | Built into Hangar | One process in role `all`. |
| Postgres | 18 | The Go tests and the Hub client suite run on Postgres as well as SQLite. |
| S3-compatible storage | MinIO in CI | AWS S3 (`eu-west-2`) passed the suite on 15 October 2026. Other providers on the [storage page](../operate/storage.md#s3-compatible-stores) are configured as described there, but are not in CI. |
| Docker Compose | The four examples in `deploy/compose` | Each starts and passes the smoke test. |
| Kubernetes | The Helm chart needs Kubernetes 1.29 or later | CI installs the chart on a kind cluster with Helm 4.1.3, runs `helm test`, a GitOps-style install with Postgres and MinIO, and the smoke test. |
| Kibana | 9.4 or later for the dashboard | See [Observability](../operate/observability.md). |

## Platforms

| Platform | Built | Tested in CI |
|---|---|---|
| Linux, amd64 and arm64 | Binaries and the container image | Go tests, a build and a server smoke test on both architectures. The container image runs in the Compose and Helm jobs on amd64. |
| macOS, amd64 and arm64 | Binaries and the Homebrew cask | No |
| Windows, amd64 and arm64 | Binaries | No |

The macOS and Windows binaries are built from the same source as the Linux ones, but CI does not run them.

## Documented, but not tested in CI

These guides describe tools that work through parts Hangar does test, such as `huggingface_hub` downloads and `HfFileSystem`, but CI does not run the tools themselves:

- [vLLM](../guides/vllm.md).
- pandas and Polars over `hf://`, and DuckDB over HTTPS ([Datasets](../guides/datasets.md)).

## Report a gap

If a client that works with huggingface.co fails with Hangar, open a [compatibility gap](https://github.com/hangar-hub/hangar/issues/new?template=compatibility.yml) issue with the client's version, the call that fails and its error, and whether the same call works against huggingface.co.
