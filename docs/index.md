---
title: Docs for the self-hosted model registry
description: "Hangar is a self-hosted model and dataset hub, compatible with the Hugging Face Hub API, git, Ollama and LM Studio. Install it and pull your first model."
---

# Hangar

**A self-hosted home for your arsenal of models.**

[Get started](get-started/index.md){ .md-button .md-button--primary } [Try the demo](https://demo.hangar.page/){ .md-button }

Hangar is a self-hosted model and dataset hub, compatible with the Hugging Face Hub API, git, Ollama and LM Studio. It keeps your machine learning models and datasets on your own storage, with full version history, and serves them to the tools you already use:

- **Hugging Face clients.** Set `HF_ENDPOINT`, and `huggingface_hub`, the `hf` CLI, `transformers`, `diffusers`, `sentence-transformers`, `datasets` and vLLM work unchanged.
- **git.** `git clone` and `git push`, with git-lfs.
- **Ollama.** `ollama pull hangar.example.com/org/model:Q4_K_M` for any GGUF Repo.
- **LM Studio.** `hangar lmstudio install org/model:Q4_K_M`.
- **Datasets.** `load_dataset`, pandas and Polars over `hf://`, DuckDB over HTTPS, and a Dataset Viewer in the browser.

Pull models and datasets from huggingface.co once, including only the quantisation, Config or Split you want and any code they depend on, then load them from fast local storage as often as you like. Upload models that exist nowhere else. Browse everything you have, and everything you could pull, in one place.

![Hangar in 30 seconds in a terminal: check the sign-in, pull a model from huggingface.co, download it with the hf CLI and show its details](assets/demo.gif){ width="979" height="650" }

To try the web UI without installing anything, open the [demo](https://demo.hangar.page/). It runs on sample data, and nothing you do there is saved.

## Quickstart

```sh
docker run -d --name hangar -p 8080:8080 -v hangar:/var/lib/hangar \
  -e HANGAR_SERVER_PUBLIC_URL=http://localhost:8080 \
  ghcr.io/hangar-hub/hangar:latest
docker logs hangar   # prints the generated admin password and Instance key once
```

Open <http://localhost:8080>, sign in as `hangar-admin`, and pull `hf-internal-testing/tiny-random-bert` from **Discover**. Then point your Python tools at Hangar:

```sh
export HF_ENDPOINT=http://localhost:8080
export HF_TOKEN=hgr_...   # create one under Settings, Tokens
python -c "from transformers import AutoModel; AutoModel.from_pretrained('hf-internal-testing/tiny-random-bert')"
```

Next: [Get started](get-started/index.md) covers Docker, Compose, Helm and the binary, your first sign-in and your first Pull.

## What it runs on

Hangar runs as one container on a laptop or homelab, with SQLite and a local folder. It scales out to stateless replicas over Postgres and S3-compatible storage, with separate `web` and `worker` processes. It exports traces, metrics and logs with OpenTelemetry, and supports local accounts and OIDC sign-in. See [Scaling](operate/scaling.md).

## What it is not

Hangar stores and serves files; it does not run models. Spaces, the Inference API, Hugging Face Jobs, Buckets and kernels, SSH git and Xet downloads are out of scope. It accepts Xet uploads, so `huggingface_hub` with `hf_xet` installed works with no setup, but it stores whole files and serves them over plain HTTP.

## Licence

Hangar is source-available under the [Elastic License 2.0](project/licence.md). You can use, change and self-host it, including inside your company. You can't offer it to others as a hosted or managed service, or remove licence notices.
