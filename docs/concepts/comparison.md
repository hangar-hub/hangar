---
title: Self-hosted Hugging Face Hub alternatives compared
description: "How Hangar compares with MatrixHub, KohakuHub, CSGHub, olah, JFrog Artifactory, Sonatype Nexus and MLflow for keeping Hugging Face models private."
---

# How Hangar compares

Teams that want models and datasets on their own infrastructure usually look at one of three kinds of tool: a caching proxy in front of huggingface.co, a full self-hosted hub, or an MLOps model registry. Hangar is a self-hosted hub: it serves the Hugging Face Hub API, keeps Mirrors with their git history, and also accepts your own uploads.

This page compares Hangar with tools people evaluate alongside it. Each fact about another tool comes from that tool's own documentation or source code, linked in [Sources](#sources): MatrixHub, KohakuHub and ModnFace were checked on 21 October 2026, and the others on 20 October 2026. Tools change, so check their current documentation before you decide.

## At a glance

"Not documented" means the tool's own documentation does not say.

| | Hangar | MatrixHub | KohakuHub | CSGHub | olah | JFrog Artifactory | Sonatype Nexus | MLflow Model Registry |
|---|---|---|---|---|---|---|---|---|
| Self-hosted | Yes | Yes | Yes | Yes | Yes | Yes, or SaaS | Yes, or cloud | Yes |
| Licence | Elastic License 2.0 | Apache-2.0 | AGPL-3.0; its Dataset Viewer needs a paid licence for commercial use after a trial | Apache-2.0 | MIT | Commercial | Community Edition or Pro | Apache-2.0 |
| Downloads with `HF_ENDPOINT` | Yes | Yes | Yes | Yes | Yes | Yes | Not documented | No |
| Uploads with `huggingface_hub` | Yes | Yes, except to proxy projects | Yes | Yes | No | Yes, to local repositories | No, proxy only | No |
| Datasets | Yes | No, models only (on its roadmap) | Yes | Yes | Yes | Yes | Yes | No |
| How it fetches from huggingface.co | Pulls Mirrors with git history and Commit SHAs, whole Repos or selected files, plus optional Pull-through | Caches a model when a client first asks for it through a proxy project; admins can also run manual or scheduled sync policies | Does not copy: for a Repo it does not hold, it shows the upstream one and redirects downloads to it | Admins run mirror tasks that sync whole Repos | Caches file blocks as clients download them | Caches files as clients download them | Caches files as clients download them | Does not fetch |
| `git clone` with git-lfs | Yes | Yes, over HTTP or SSH | Clone only; push is in development | Yes | No | Not documented | Not documented | No |
| `ollama pull` | Yes | Not documented | Not documented | Not documented | No | Not documented | Not documented | No |
| Xet uploads from `hf_xet` | Yes | Not documented | Not documented | Not documented | Partial | Yes, with `huggingface_hub` up to 1.19.0 | Not yet | No |
| Web UI for browsing | Yes, with a Dataset Viewer | Yes | Yes, with a Dataset Viewer | Yes | Basic | Yes | Yes | Yes |
| Air-gapped use | Yes, fed from another Instance | Listed as a use case, with sync between instances | Not documented | Not documented | Yes, serves only what is cached | Not documented | Not documented | Not documented |

## Caching proxies: olah, Artifactory and Nexus

A caching proxy sits in front of huggingface.co and keeps a copy of what clients download. It is the smallest change for teams that only need faster or controlled downloads.

- **olah** is a lightweight mirror server. It caches files block by block as clients ask for them, and an offline mode serves only what is already cached. It has no upload routes.
- **JFrog Artifactory** has remote Hugging Face repositories that cache huggingface.co, and local repositories you can upload to with `upload_folder`. It suits organisations that already run Artifactory for other package types.
- **Sonatype Nexus Repository** supports Hugging Face as a proxy repository for models and datasets. It has no hosted Hugging Face repositories, so you cannot upload to it.

Choose Hangar over a proxy when you want Mirrors that keep the upstream git history and Commit SHAs, so a model pinned to a Commit still resolves after upstream changes, or when you also need uploads, git, Ollama or LM Studio.

## Self-hosted hubs: MatrixHub, KohakuHub and CSGHub

These serve the Hub API from your own infrastructure and accept uploads, as Hangar does.

- **MatrixHub** is a self-hosted model hub aimed at serving models to inference engines such as vLLM, SGLang and Dynamo inside a company network. Repositories live in projects. A proxy project caches a model from huggingface.co, or another registry, the first time a client asks for it, and admins can also pull or push models between registries, including other MatrixHub instances, by hand or on a schedule. It handles models only; dataset support is on its roadmap. It runs with Docker Compose on SQLite, or with Helm on MySQL, and stores files on a local volume or NFS; S3-compatible storage is on its roadmap. On 21 October 2026, its latest release was 0.2.0.
- **KohakuHub** is a self-hosted Hugging Face Hub alternative for models and datasets, built on LakeFS for versioning, S3-compatible storage such as MinIO, and Postgres or SQLite. When a Repo is not stored locally, it can show the Repo from huggingface.co or another KohakuHub instance and send downloads there, rather than keeping a copy. `git clone` works; `git push` is in development. Its README describes it as an alpha. The core is AGPL-3.0, and its Dataset Viewer is under the Kohaku Software License 1.0: free for non-commercial use, with commercial use after a trial needing a paid licence.
- **CSGHub** from OpenCSG is a self-hosted hub with a web UI, git access through Gitea, and dataset previews. Admins run mirror tasks that sync whole Repos from upstream hubs.

Hangar differs in scope: it serves GGUF models straight to Ollama and LM Studio, pulls only the files you choose, such as one quantisation, as a [Partial mirror](mirrors.md#partial-mirrors), keeps the upstream git history and Commit SHAs of every Mirror, and runs as a single binary or container with SQLite, or with Postgres and S3-compatible storage.

**ModnFace** describes itself, on its GitHub organisation's profile, as a self-hostable alternative to the Hugging Face Hub built on Hugging Face's own open-source components, with Xet storage, Spaces and air-gapped deployment. On 21 October 2026 the organisation published forks of other projects, such as `huggingface_hub`, `xet-core` and `transformers`, but no server code, installation guide or licence for ModnFace itself. Its features cannot be checked, so it is not in the table.

## Model registries: MLflow

**MLflow Model Registry** tracks model versions, aliases and lineage for models you train and log with MLflow. It does not serve the Hugging Face Hub API, so `huggingface_hub`, `transformers` and `HF_ENDPOINT` do not work with it.

The two can work together: MLflow for experiment tracking and model lineage, and Hangar to store the weights and serve them to Hugging Face clients, git, Ollama and LM Studio.

## Hugging Face Team and Enterprise

Hugging Face's paid plans add organisation features such as single sign-on, audit logs and a choice of US or EU storage regions. The plans page does not offer a self-hosted or on-premises option, so the files stay on huggingface.co.

Choose Hangar when the files must stay on your own servers or network, including [air-gapped networks](../guides/air-gapped.md).

## What Hangar does not do

Hangar stores and serves files; it does not run models. Spaces, the Inference API, Hugging Face Jobs, Buckets and kernels, SSH git and Xet downloads are out of scope. For the Hub API endpoints Hangar serves, see [Hub API compatibility](../reference/hub-api-compatibility.md).

## Sources

Checked on 21 October 2026:

- MatrixHub: [README](https://github.com/matrixhub-ai/matrixhub/blob/main/README.md), [overview](https://github.com/matrixhub-ai/matrixhub/blob/main/website/docs/overview/index.md), [installation](https://github.com/matrixhub-ai/matrixhub/blob/main/website/docs/installation/index.md), [proxy projects](https://github.com/matrixhub-ai/matrixhub/blob/main/website/docs/guides/mirror-from-huggingface.md), [uploads and downloads](https://github.com/matrixhub-ai/matrixhub/blob/main/website/docs/operations/model-repo/upload-download.md), [remote synchronisation](https://github.com/matrixhub-ai/matrixhub/blob/main/website/docs/operations/platform-settings/remote-sync.md), [roadmap](https://github.com/matrixhub-ai/matrixhub/blob/main/ROADMAP.md), [releases](https://github.com/matrixhub-ai/matrixhub/releases)
- KohakuHub: [README](https://github.com/KohakuBlueleaf/KohakuHub/blob/main/README.md), [external source fallback](https://github.com/KohakuBlueleaf/KohakuHub/blob/main/docs/features/core/fallback.md), [git support](https://github.com/KohakuBlueleaf/KohakuHub/blob/main/docs/Git.md), [licensing](https://github.com/KohakuBlueleaf/KohakuHub/blob/main/docs/non-commercial/licensing.md)
- ModnFace: [organisation profile](https://github.com/ModnFace), [repositories](https://github.com/orgs/ModnFace/repositories)

Checked on 20 October 2026:

- olah: [README](https://github.com/vtuber-plan/olah/blob/main/README.md), [API routes](https://github.com/vtuber-plan/olah/blob/main/src/olah/server_api_routes.py), [file routes](https://github.com/vtuber-plan/olah/blob/main/src/olah/server_file_routes.py), [configuration](https://github.com/vtuber-plan/olah/blob/main/assets/full_configs.toml)
- JFrog Artifactory: [Hugging Face repositories](https://docs.jfrog.com/artifactory/docs/hugging-face-repositories), [JFrog CLI with Hugging Face](https://docs.jfrog.com/artifactory/docs/use-hugging-face-with-jfrog-cli)
- Sonatype Nexus Repository: [Hugging Face repositories](https://help.sonatype.com/en/hugging-face-repositories.html), [formats](https://help.sonatype.com/en/formats.html)
- CSGHub: [csghub](https://github.com/OpenCSGs/csghub), [csghub-server](https://github.com/OpenCSGs/csghub-server), [server API routes](https://github.com/OpenCSGs/csghub-server/blob/main/api/router/api.go)
- MLflow: [Model Registry](https://mlflow.org/docs/latest/ml/model-registry/), [repository](https://github.com/mlflow/mlflow)
- Hugging Face: [Team and Enterprise plans](https://huggingface.co/enterprise)
