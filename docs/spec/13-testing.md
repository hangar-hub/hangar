# 13. Testing

Tests are how a one-shot build proves it works. Every workstream in [15](15-build-plan.md) ships its tests with its code; a workstream is not done until its tests and the relevant compat suites pass.

## Layers

| Layer | Where | Tools |
|---|---|---|
| Unit | next to code (`*_test.go`), `ui/src/**/*.test.ts(x)` | Go `testing`, `testify/require`; Vitest, Testing Library |
| Conformance | `internal/blob/blobtest`, `internal/db/dbtest` | One suite run against every backend: filesystem and MinIO; SQLite and Postgres. Containers via `testcontainers-go`. |
| Integration | `internal/<pkg>/*_integration_test.go`, build tag `integration` | Real database and Blob store, real HTTP server on a random port. |
| Fuzz | parsers | `go test -fuzz` for GGUF, safetensors, pickle, LFS pointers, NDJSON commit payloads, git pkt-line, Ollama signature parsing, Markdown front matter. |
| Compatibility | `compat/` | Real clients against a real Instance (below). |
| End to end (UI) | `ui/e2e/` | Playwright, against Compose quickstart; axe accessibility checks on every page; phone, tablet and desktop widths. |
| Load | `loadtest/` | k6 scripts. |

## Test data

Use tiny real models so tests are fast and need no GPU:

- `hf-internal-testing/tiny-random-bert`, `hf-internal-testing/tiny-random-gpt2`, `hf-internal-testing/tiny-random-LlamaForCausalLM` (transformers loads)
- a tiny remote-code repo (for example `hf-internal-testing/test_dynamic_model`) and a fixture pair that copies Jina's cross-repo `auto_map` form, for Dependency resolution
- a small public GGUF repo with several quants (choose one under 50 MB per file; record the choice in `compat/README.md`)
- a tiny dataset repo for `datasets.load_dataset`
- a tiny MLX repo
- generated fixtures for pickle scanning (benign and malicious pickles built in the test, never downloaded)

Compat tests that need huggingface.co run nightly and on PRs that touch `internal/upstream`; everything else uses fixtures uploaded to the test Instance, so most tests run offline. Cache upstream fixtures in CI.

## Compatibility suites (`compat/`)

### Hub clients (`compat/hf/`, pytest)

Matrix: the latest `huggingface_hub` release, the oldest release still in use by the latest `transformers` (document the floor), with and without `hf_xet` installed, Python 3.10 and the latest Python.

Covers:

- `hf auth login --token` and `hf auth whoami`
- `create_repo` (public, private, dataset, `exist_ok`), `delete_repo`, `move_repo`, `update_repo_settings`
- `upload_file`, `upload_folder`, `create_commit` with adds, deletes, copies; `upload_large_folder`; files over the multipart threshold (use a 6 GiB sparse file in nightly only, 200 MB on PRs with a lowered threshold via Instance setting)
- `create_pr=True`, discussions and comments, merge
- `create_branch`, `create_tag`, `delete_branch`, `list_repo_refs`, `list_repo_commits`
- `model_info`, `dataset_info`, `list_models` with search and filters, `list_repo_files`, `list_repo_tree` (recursive, paginated), `get_paths_info`, `file_exists`, `revision_exists`
- `hf_hub_download` by branch, tag, commit SHA, `refs/pr/1`; resume after an interrupted download; `snapshot_download` with `allow_patterns`; offline reuse of the cache (`HF_HUB_OFFLINE=1`)
- `transformers` `AutoModel.from_pretrained` and `AutoTokenizer.from_pretrained` for the tiny models; `trust_remote_code=True` for the remote-code fixture after a Pull resolves its Dependency
- `sentence-transformers` load of a tiny model; `diffusers` load of a tiny pipeline (nightly)
- `datasets.load_dataset` for the tiny dataset, through `HfFileSystem`
- `HfFileSystem` `ls`, `glob`, `open`, `info`
- error mapping: `RepositoryNotFoundError`, `RevisionNotFoundError`, `EntryNotFoundError`, `GatedRepoError`, 401 vs 404 for private Repos
- collections API
- `hf download` and `hf upload` CLI commands

### git (`compat/git/`, Go tests driving the `git` and `git-lfs` CLIs)

Clone (full, `--depth 1`, `--filter=blob:none` if supported), fetch, pull, push new branch, push LFS files over the multipart threshold, delete branch, tags, force-push rules, push rejected on a Mirror, concurrent pushes from two clients to the same branch through two replicas (one must be rejected cleanly), clone of a Repo created by the HTTP API and vice versa (same Commit SHAs), `GIT_LFS_SKIP_SMUDGE=1` clone then `git lfs pull --include`, auth with token as password, 404 (not 403) for private Repos without access. Matrix: git 2.39 (Debian 12) and the latest git; latest git-lfs.

### Ollama (`compat/ollama/`)

Run the pinned `ollama/ollama` image, `ollama pull` a public tiny GGUF from the test Instance by quant and by `latest`, `ollama show` (template, parameters, licence), `ollama run` a one-token prompt, a private Repo with a registered key, and a refused cross-host redirect never happening (blobs served directly).

### LM Studio layout (`compat/lmstudio/`)

LM Studio itself is not automated. Test that `hangar lmstudio install` produces the documented layout, links resolve, and `list` and `remove` work. A manual check with LM Studio is a release checklist item.

### Upstream pulls (`compat/upstream/`, nightly)

Pull a tiny repo fully from huggingface.co and check Commit SHAs equal the Hub's `sha`; Partial mirror of one quant; tracking picks up a new Commit (use a fixture Repo on a second Hangar Instance as the Upstream, so the test controls it); Pull-through on `hf_hub_download`; bandwidth cap holds within 10 % over 60 seconds across 2 workers; resume after killing the worker mid-file.

### Xet uploads (`compat/xet/`)

See W11's definition of done in [15](15-build-plan.md). The Hub client matrix above runs every upload test with `hf_xet` installed (Xet path) and with `HF_HUB_DISABLE_XET=1` (LFS path).

### Datasets (`compat/viewer/`)

See [14](14-datasets.md) "Tests".

## Smoke test

`scripts/smoke.sh <url>`: health, login, create Repo, upload, download, `git clone`, Pull a tiny upstream Repo (skippable offline). Run by CI after starting each Compose example and the Helm chart on kind.

## Load tests (`loadtest/`)

k6 scenarios: `resolve` HEAD and GET mix, `model_info`, tree listing of a 100,000-file Repo, concurrent 1 GiB uploads (one Repo per client), concurrent downloads of a large file, `git clone` of a 10,000-Commit Repo, and Dataset Viewer `/rows` on a 10 million row Split. Reference setup: 2 `web` replicas (2 vCPU, 1 GiB each), 1 `worker`, Postgres, MinIO, on one host. Record results against the targets in [02](02-architecture.md).

## Coverage

Go statement coverage at least 75 % per package, excluding generated code; `internal/auth`, `internal/repo`, `internal/gitserver` and `internal/hfapi` at least 85 %. CI reports coverage per package and fails below the floor.
