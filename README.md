# Hangar

**A self-hosted home for your arsenal of models.**

![The Models list in Hangar's web UI, with filters for format, task, quantisation and size](docs/assets/screenshots/models-desktop.webp)

Hangar keeps your machine learning models and datasets on your own storage, with full version history, and serves them to the tools you already use:

- **Hugging Face clients**: set `HF_ENDPOINT` and `huggingface_hub`, the `hf` CLI, `transformers`, `diffusers`, `sentence-transformers`, `datasets` and vLLM work unchanged.
- **git**: `git clone` and `git push`, with git-lfs.
- **Ollama**: `ollama pull hangar.example.com/org/model:Q4_K_M` for any GGUF repo.
- **LM Studio**: `hangar lmstudio install org/model:Q4_K_M`.
- **Datasets**: `load_dataset`, pandas and Polars over `hf://`, DuckDB over HTTPS, plus a Dataset Viewer in the browser to preview, search, filter and profile rows.

Pull models and datasets from huggingface.co once, including only the quantisation, Config or Split you want and any code they depend on, then load them from fast local storage as often as you like. Upload models that exist nowhere else. Browse everything you have, and everything you could pull, in one place.

Hangar runs as one container on a laptop or homelab, and scales out to stateless replicas over Postgres and S3-compatible storage. It exports traces, metrics and logs with OpenTelemetry, and supports local accounts and OIDC sign-in.

> **Status:** in development. There is no release yet. The plan is in [`docs/spec/`](docs/spec/README.md).

| Model card | GGUF viewer | Dataset Viewer |
|---|---|---|
| ![A model's page with its card, files, metadata and the Use this model button](docs/assets/screenshots/model-card-desktop.webp) | ![The GGUF viewer showing a file's metadata, chat template and tensors](docs/assets/screenshots/gguf-viewer-desktop.webp) | ![The Dataset Viewer showing rows of a dataset Split with column statistics](docs/assets/screenshots/dataset-viewer-desktop.webp) |

To try the web UI without installing anything, open the demo at <https://demo.hangar.page/>. It runs on sample data, and nothing you do there is saved.

## Quickstart

```sh
docker run -d --name hangar -p 8080:8080 -v hangar:/var/lib/hangar \
  -e HANGAR_SERVER_PUBLIC_URL=http://localhost:8080 \
  ghcr.io/jamesagarside/hangar:latest
docker logs hangar   # prints the generated admin password and Instance key once
```

Open <http://localhost:8080>, sign in as `hangar-admin`, pull a model from **Discover**, then:

```sh
export HF_ENDPOINT=http://localhost:8080 HF_TOKEN=hgr_...
python -c "from transformers import AutoModel; AutoModel.from_pretrained('hf-internal-testing/tiny-random-bert')"
```

## Install

| Option | How |
|---|---|
| Container image | `ghcr.io/jamesagarside/hangar` for linux/amd64 and linux/arm64, signed with cosign |
| Docker Compose | Examples for SQLite, Postgres and MinIO, a scaled-out setup and Keycloak in [`deploy/compose`](deploy/compose) |
| Kubernetes | `helm install hangar oci://ghcr.io/jamesagarside/charts/hangar --set publicURL=https://hangar.example.com`, with `publicURL` set to the address clients use; see the [Helm guide](https://hangar.page/latest/get-started/helm/) and [`deploy/helm/hangar`](deploy/helm/hangar) |
| Binary | [Release archives](https://github.com/jamesagarside/hangar/releases) for Linux, macOS and Windows, `brew install --cask jamesagarside/tap/hangar`, or `go install github.com/jamesagarside/hangar/cmd/hangar@latest` |

## Documentation

<https://hangar.page/>: getting started, concepts, guides for every client, operating Hangar, and the API, CLI, configuration and metrics references.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) and the [code of conduct](CODE_OF_CONDUCT.md). Report security problems privately, as [SECURITY.md](SECURITY.md) describes.

## Licence

Hangar is source-available under the [Elastic License 2.0](LICENSE). You can use, change and self-host it, including inside your company. You can't offer it to others as a hosted or managed service, or remove licence notices. Third-party notices are in [NOTICE](NOTICE).
