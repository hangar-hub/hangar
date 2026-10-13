# Hangar

A self-hosted home for your arsenal of models.

Hangar keeps your machine learning models and datasets on your own storage, with full version history, and serves them to the tools you already use:

- **Hugging Face clients**: set `HF_ENDPOINT` and `huggingface_hub`, the `hf` CLI, `transformers`, `diffusers`, `sentence-transformers`, `datasets` and vLLM work unchanged.
- **git**: `git clone` and `git push`, with git-lfs.
- **Ollama**: `ollama pull hangar.example.com/org/model:Q4_K_M` for any GGUF repo.
- **LM Studio**: `hangar lmstudio install org/model:Q4_K_M`.
- **Datasets**: `load_dataset`, pandas and Polars over `hf://`, DuckDB over HTTPS, plus a Dataset Viewer in the browser to preview, search, filter and profile rows.

Pull models and datasets from huggingface.co once, including just the quantisation, Config or Split you want and any code they depend on, then load them from fast local storage as often as you like. Upload models that exist nowhere else. Browse everything you have, and everything you could pull, in one place.

Hangar runs as one container on a laptop or homelab, and scales out to stateless replicas over Postgres and S3-compatible storage. It exports traces, metrics and logs with OpenTelemetry, and supports local accounts and OIDC sign-in.

> **Status:** in development. The plan is in [`docs/spec/`](docs/spec/README.md). There is no release yet.

## Licence

Hangar is source-available under the [Elastic License 2.0](LICENSE). You can use, change and self-host it, including inside your company. You can't offer it to others as a hosted or managed service, or remove licence notices.
