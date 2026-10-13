# Compatibility suites

Real clients against a real Hangar Instance (docs/spec/13-testing.md). Run them with `make test-compat`.

| Directory | Suite | Owner |
|---|---|---|
| `harness/` | Go launcher: starts `hangar server` on a random port with a temporary SQLite database and Blob store. | Phase 0 |
| `hangar_instance.py`, `conftest.py` | The same launcher for pytest, and the shared `hangar` and `hf_endpoint` fixtures. | Phase 0 |
| `hf/` | Hub clients: `huggingface_hub`, `transformers`, `datasets` and others. | W4 |
| `git/` | `git` and `git-lfs`. | W3 |
| `ollama/` | `ollama pull` and `ollama run`. | W7 |
| `lmstudio/` | `hangar lmstudio` install layout. | W9 |
| `upstream/` | Pulls from huggingface.co and from a second Hangar. | W6 |
| `xet/` | `hf_xet` uploads. | W11 |
| `viewer/` | Dataset Viewer API against the Hub's. | W10 |

## Running

- Go suites use the `compat` build tag: `go test -tags compat ./compat/...`.
- Python suites run with uv: `uv run --project compat pytest compat`.
- Set `HANGAR_BINARY` to test a prebuilt binary; otherwise the launchers build `./cmd/hangar` once per run.

## Fixtures

`fixtures.json` lists the upstream fixtures and `fixtures.py` downloads them into `compat/.fixtures` (ignored by git, cached in CI). Tests that need huggingface.co itself run nightly and on pull requests that change `internal/upstream`; the rest upload fixtures to the test Instance, so they run offline.

Chosen fixtures:

- GGUF: `ggml-org/tiny-llamas`, limited to `stories15M-q4_0.gguf` (19 MB), `stories15M-q8_0.gguf` (27 MB) and `stories260K.gguf` (1.2 MB), so every file is under 50 MB.
- Dataset: `lhoestq/demo1` (5 files, 6 KB).
- MLX: `mlx-community/SmolLM-135M-Instruct-4bit` (79 MB in total), nightly only.
- Remote code: `hf-internal-testing/test_dynamic_model`. The Jina-style cross-Repo `auto_map` pair is built by the W6 tests as local fixtures.
- Pickle scanner fixtures are generated in the tests, never downloaded.
