# Hangar v1.0 specification

| File | Covers |
|---|---|
| [01-product.md](01-product.md) | What Hangar is, personas, principles, tone, the full v1.0 feature list with IDs, non-goals |
| [02-architecture.md](02-architecture.md) | Stack, processes and scaling, repo layout, core interfaces, data model, Blob store layout, consistency, Jobs, routing, configuration, scale targets |
| [03-hf-api.md](03-hf-api.md) | The Hugging Face Hub HTTP API contract Hangar implements |
| [04-git.md](04-git.md) | git storage, git smart HTTP and the git-lfs API |
| [05-auth.md](05-auth.md) | Users, Organisations, permissions, local login, OIDC, sessions, Tokens, Ollama keys, audit |
| [06-upstreams-and-pulls.md](06-upstreams-and-pulls.md) | Upstreams, Mirrors, the Pull lifecycle, network controls, tracking, Pull-through, Discover, the GGUF quant picker, Dependencies, bulk import |
| [07-indexing-and-scanning.md](07-indexing-and-scanning.md) | Metadata extraction and security scanning |
| [08-local-tools.md](08-local-tools.md) | The Ollama registry endpoint, LM Studio installs, the local cache, the `hangar` CLI |
| [09-web-ui.md](09-web-ui.md) | Web UI stack, pages and behaviour |
| [10-native-api.md](10-native-api.md) | The native API and webhook events |
| [11-observability.md](11-observability.md) | OpenTelemetry traces, metrics, logs, health, dashboards |
| [12-packaging-and-release.md](12-packaging-and-release.md) | Image, Compose, Helm, binaries, docs site, repo files, licence, CI, release checklist |
| [13-testing.md](13-testing.md) | Test layers, fixtures, compatibility suites, load tests, coverage |
| [14-datasets.md](14-datasets.md) | Datasets: Configs and Splits, Parquet export, Dataset Viewer API, statistics, Viewer UI, dataset Discover and Pulls, Croissant |
| [15-build-plan.md](15-build-plan.md) | How agents build it: phases, workstreams, ownership, definitions of done |

Supporting research is in [`../research/`](../research/):

- [`hf-hub-api-contract.md`](../research/hf-hub-api-contract.md): the Hub HTTP API as `huggingface_hub` 2.2.0 and downstream libraries use it
- [`git-server-research.md`](../research/git-server-research.md): git smart HTTP and git-lfs on stateless replicas, tested against real clients
- [`xet-upload-research.md`](../research/xet-upload-research.md): the Xet upload protocol, formats and reassembly

## Decisions

Made by James on 2026-10-13:

- Name: Hangar. Tone: "your arsenal of models".
- Go server and CLI, React UI.
- SQLite and Postgres; filesystem and S3 storage.
- Full `git clone` and `git push` in v1.0.
- Local login and OIDC; multiple users; versioning.
- OpenTelemetry for all telemetry; container-first.
- Elastic License 2.0, published at `github.com/hangar-hub/hangar`.
- As many features as possible in v1.0, so the project needs little maintenance after release.
- First-class datasets: Viewer, Parquet export, partial Pulls by Config and Split.
- Accept Xet uploads (reassembled into whole files) so `huggingface_hub` uploads work with no client setup; downloads stay plain HTTP.
