---
title: Serve vLLM models from a private model hub
description: "Serve models with vLLM from weights stored in Hangar: point its huggingface_hub downloads at your Instance with HF_ENDPOINT, in Docker or on Kubernetes."
---

# Use vLLM

vLLM downloads models with `huggingface_hub`, so `HF_ENDPOINT` points it at Hangar. Every vLLM server then loads weights from your own network instead of huggingface.co, and a model pinned to a Commit SHA loads the same files it would there.

Hangar's CI tests the `huggingface_hub` downloads that vLLM uses, but does not run vLLM itself; see [Tested versions and platforms](../reference/compatibility.md).

## Before you start

1. Pull the model into the Arsenal. `--server-only` fills the Arsenal without downloading the files to your machine:

    ```sh
    hangar pull Qwen/Qwen3-0.6B --server-only
    ```

    vLLM reads the config, tokenizer and generation config files as well as the weights, so pull the whole Repo, not only the weights. If you pull only some files, the Repo becomes a [Partial mirror](../concepts/mirrors.md#partial-mirrors) and vLLM fails on the first file that is not present. An admin can instead turn on [Pull-through](../concepts/mirrors.md#pull-through), so a request for a missing Repo or file pulls it.

2. Create a `read` Token for the servers under **Settings**, **Tokens**. A Token can be limited to the Repos it serves.

## Serve a model

```sh
export HF_ENDPOINT=https://hangar.example.com
export HF_TOKEN=hgr_...
vllm serve Qwen/Qwen3-0.6B
```

Mirrors keep the upstream Commit SHAs, so a Revision pinned for huggingface.co works unchanged:

```sh
vllm serve Qwen/Qwen3-0.6B --revision <commit SHA>
```

## In a container

```sh
docker run --gpus all -p 8000:8000 \
  -e HF_ENDPOINT=https://hangar.example.com -e HF_TOKEN=hgr_... \
  -v ~/.cache/huggingface:/root/.cache/huggingface \
  vllm/vllm-openai:latest --model Qwen/Qwen3-0.6B
```

Mounting the cache means a restart loads from disk without asking Hangar again.

## On Kubernetes

Keep the Token in a Secret and the Hugging Face cache on a volume, so a restarted Pod does not download the model again. [Load models in Kubernetes without downloading them on every Pod start](kubernetes.md) has a complete Deployment for vLLM, with an init container that downloads the model from Hangar before the server starts.

## Start with no network

For a fully offline start, download the files once into the local cache, then set `HF_HUB_OFFLINE=1`:

```sh
hangar pull Qwen/Qwen3-0.6B
HF_HUB_OFFLINE=1 vllm serve Qwen/Qwen3-0.6B
```

`hangar pull` writes into the same Hugging Face cache vLLM reads.

## Errors you may see

| Error | Cause | Fix |
|---|---|---|
| `RepositoryNotFoundError`, "Repository Not Found for url" | `HF_TOKEN` is missing or cannot read the Repo, or the Repo is not in the Arsenal. Private Repos answer as if they did not exist. | Check the Token with `HF_TOKEN=hgr_... hf auth whoami`, and pull the Repo first. |
| "Invalid credentials in Authorization header" | The Token is wrong, expired or revoked, or it is a huggingface.co token (`hf_...`) rather than a Hangar one (`hgr_...`). | Create a Token in Hangar. |
| `HfHubHTTPError` with status 409, naming a `hangar pull` command | The Repo is a Partial mirror and the file was not pulled. | Run the command it names, or pull the whole Repo. |
| `LocalEntryNotFoundError` with `HF_HUB_OFFLINE=1` | The files are not in the local cache. | Run `hangar pull` for the model, or start once without `HF_HUB_OFFLINE`. |

For other problems, see [Troubleshooting](../operate/troubleshooting.md).
