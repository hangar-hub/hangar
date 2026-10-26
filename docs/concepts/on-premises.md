---
title: Hugging Face Hub on-premises
description: "Can you run the Hugging Face Hub on-premises? What Hugging Face offers today, what a self-hosted Hub must do, and how Hangar provides it on your own servers."
---

# Hugging Face Hub on-premises

Hugging Face does not offer the Hugging Face Hub as software you run on your own servers. Its plans page lists no such option, and it stopped offering on-premises deployments of its Private Hub in 2023. Its Team and Enterprise plans add organisation features to huggingface.co, and your files stay there. To keep models and datasets on your own infrastructure and still use the Hugging Face tools, you run a self-hosted hub that serves the same API, such as Hangar.

## What Hugging Face offers

Hugging Face's Team and Enterprise plans add single sign-on, audit logs, Token management and a choice of US or EU storage regions to an organisation on huggingface.co. The plans page does not list a self-hosted or on-premises option.

In August 2022, Hugging Face announced a Private Hub with managed, customer-cloud and on-premises deployments. A June 2023 update to that announcement says the on-premises deployments are no longer offered, and that the Private Hub is now called Enterprise Hub.

## What an on-premises Hub needs to do

The Hugging Face libraries do not need huggingface.co itself. They need a server that answers the Hub API the way huggingface.co does. `huggingface_hub` reads the server's address from `HF_ENDPOINT`, and `transformers`, `datasets`, `sentence-transformers`, `diffusers`, vLLM and the `hf` CLI all download through it. For code to work unchanged, a self-hosted Hub has to:

- Serve the Hub API endpoints the clients call, including file downloads at a Revision, file listings, Repo metadata and, for uploads, the commit API.
- Keep the same Repo IDs and Commit SHAs as huggingface.co for models you copy from there, so code that pins a Revision keeps working.
- Store large files the way clients expect, and accept uploads from `huggingface_hub`, including Xet uploads from `hf_xet`, which recent versions install by default.
- Check Tokens and decide who can read and write each Repo.

## How Hangar provides it

Hangar is a self-hosted model and dataset hub, compatible with the Hugging Face Hub API, git, Ollama and LM Studio. It runs on your servers, from one container with SQLite and a local folder to replicas over Postgres and S3-compatible storage. Clients set `HF_ENDPOINT` and `HF_TOKEN`, and their code does not change.

| On huggingface.co | In Hangar |
|---|---|
| Models and datasets in Repos with git history | [Repos](repos.md), each a real git repository, with the same Commit SHAs for [Mirrors](mirrors.md) of huggingface.co Repos |
| Downloading from huggingface.co | [Pulls](pulls.md) from huggingface.co or another Instance, of whole Repos or selected files such as one GGUF quant, and optional [Pull-through](mirrors.md#pull-through) |
| Private and gated Repos | Private Repos and [gated models](../guides/gated.md), with Organisations, Roles and Token scopes; see [Permissions](permissions.md) |
| Single sign-on | [OIDC](../operate/oidc/index.md) with providers such as Keycloak, Microsoft Entra ID, Okta and Google |
| Audit logs | An audit log of security-relevant actions, exported with OpenTelemetry |
| Storage regions | Your own storage: a local folder or any S3-compatible bucket; see [Storage backends](../operate/storage.md) |
| The Dataset Viewer | A Dataset Viewer with an API compatible with the Hub's; see [Datasets](../guides/datasets.md) |
| `ollama pull hf.co/...` | `ollama pull` from your Instance; see [Ollama](../guides/ollama.md) |

Hangar's CI runs `huggingface_hub`, the `hf` CLI, `transformers`, `sentence-transformers` and `datasets` against it on every pull request. [Hub API compatibility](../reference/hub-api-compatibility.md) lists every Hub endpoint and whether Hangar serves it, and [Tested versions and platforms](../reference/compatibility.md) lists the versions.

### Connected or air-gapped

An Instance with internet access pulls from huggingface.co when someone asks for a model, and serves it from your storage after that. An Instance with no internet access can pull from another Instance that has it, or be filled from a backup or a Hugging Face cache carried across. See [Air-gapped Instances](../guides/air-gapped.md).

## What stays on huggingface.co

Hangar stores and serves files; it does not run models. Spaces, the Inference API, Hugging Face Jobs, Buckets and kernels are out of scope. So are Xet downloads and SSH git: Hangar serves files over plain HTTP, which every client supports. For the endpoints it serves, see [Hub API compatibility](../reference/hub-api-compatibility.md).

## Licence

Hangar is source-available under the [Elastic License 2.0](../project/licence.md). You can use, change and self-host it, including inside your company, for free. You can't offer it to others as a hosted or managed service.

## Other self-hosted options

Hangar is one of several ways to keep Hugging Face models on your own infrastructure. Caching proxies such as JFrog Artifactory and Sonatype Nexus, and other self-hosted hubs such as MatrixHub, are compared in [How Hangar compares](comparison.md).

## Sources

Checked on 23 October 2026:

- Hugging Face: [Team and Enterprise plans](https://huggingface.co/enterprise)
- Hugging Face: [Introducing the Private Hub](https://huggingface.co/blog/introducing-private-hub), published 3 August 2022, with its June 2023 update
- Hugging Face: [Environment variables](https://huggingface.co/docs/huggingface_hub/package_reference/environment_variables) for `HF_ENDPOINT`
