# Contributing to Hangar

Thanks for helping. This page covers setting up, testing and sending changes.

## Before you start

- For anything bigger than a small fix, open an issue first, so we can agree on the approach.
- Hangar's behaviour is specified in [`docs/spec/`](https://github.com/jamesagarside/hangar/tree/main/docs/spec). Compatibility with the Hugging Face Hub is the product: check behaviour against [`docs/spec/03-hf-api.md`](https://github.com/jamesagarside/hangar/blob/main/docs/spec/03-hf-api.md) and the `huggingface_hub` source, not memory.
- Use the vocabulary in [`CONTEXT.md`](https://github.com/jamesagarside/hangar/blob/main/CONTEXT.md) in code, docs and UI copy.

## Set up

You need Go (the version in `go.mod`), Node.js 24 with corepack, Docker (for Postgres and MinIO in tests), git 2.39 or later with git-lfs, and [uv](https://docs.astral.sh/uv/) for the Python compatibility suites. The repository has a dev container (`.devcontainer/`) with all of them.

```sh
git clone https://github.com/jamesagarside/hangar.git
cd hangar
make build        # the UI and bin/hangar
make dev          # the server on :8080 and the UI with live reload on :5173
```

## make targets

| Target | Does |
|---|---|
| `make build` | Build the UI and the `hangar` binary into `bin/`. |
| `make dev` | Run the server with live UI reload. |
| `make lint` | Go, UI and house style linters. |
| `make test` | Unit and integration tests, on SQLite and Postgres and against MinIO (needs Docker). |
| `make test-compat` | The client compatibility suites in `compat/`. |
| `make generate` | Regenerate the API code, the configuration, CLI and metrics references and the dashboards. CI fails if they are out of date. |
| `make lint-deploy` | Lint the Helm chart and check the Compose files. |
| `make image` | Build the container image. |
| `make docs` | Build the documentation site. |
| `make help` | Everything else. |

## Tests

Tests ship with code. Every change needs tests that fail without it, and `make lint test` must pass. Database tests run on SQLite and Postgres with `dbtest.ForEachDriver`; the Blob store conformance suite runs on the filesystem and MinIO. Coverage must stay at 75 % per package, 85 % for `internal/auth`, `internal/repo`, `internal/gitserver` and `internal/hfapi`.

Never skip, disable or weaken a test to get a change through. No stubs, placeholder data or `TODO`s in shipped code; fakes live in `*test` packages.

## Style

- Go: `gofmt` and `goimports`, checked by golangci-lint.
- UI: Biome.
- Prose, UI text, error messages and CLI help: the Google developer documentation style guide, in British English, with no em dashes. `make lint-style` catches the dashes.
- Never log or record on spans: tokens, passwords, cookies, presigned URLs or `Authorization` headers.

## Commits and pull requests

- Use [Conventional Commits](https://www.conventionalcommits.org), such as `feat(hfapi): support list_repo_likers` or `fix(cli): resume downloads after a 416`. release-please builds the changelog from them.
- Keep pull requests focused, and fill in the template.
- Do not add attribution lines to commits or pull requests.

## Contributor licence agreement

Before your first pull request is merged, the CLA Assistant GitHub app asks you to sign the contributor licence agreement, once, in a comment. It lets the project keep distributing your contribution under the Elastic License 2.0.

## Licence

Hangar is source-available under the Elastic License 2.0. By contributing you agree your contribution is distributed under it.
