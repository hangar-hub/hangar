# CLAUDE.md

Hangar is a self-hosted model and dataset hub for your arsenal of models: a Hugging Face Hub-compatible server with git, Ollama, LM Studio and Dataset Viewer support, in Go with a React UI. It is being built from the spec in `docs/spec/`.

## Start here

- The spec index is `docs/spec/README.md`. The build plan is `docs/spec/15-build-plan.md`; if you are a build agent, follow its rules for your workstream.
- Shared vocabulary is in `CONTEXT.md`. Use its terms in code, docs and UI copy, and avoid the words it lists under "Avoid".
- Research notes are in `docs/research/`. Every claim there cites a primary source.

## Commands

- `make build` builds the UI and the `hangar` binary.
- `make lint test` runs linters and unit and integration tests.
- `make test-compat` runs the client compatibility suites in `compat/`.
- `make dev` runs the server with live UI reload.

## Rules

- Compatibility with the Hugging Face Hub API is the product. Check behaviour against `docs/spec/03-hf-api.md` and the `huggingface_hub` source, not memory.
- `internal/repo` is the only package that writes Commits or refs.
- No stubs, placeholder data or `TODO`s in shipped code. Fakes live in `*test` packages.
- Never log or record on spans: tokens, passwords, cookies, presigned URLs, `Authorization` headers.
- Conventional Commits. No attribution lines in commits or pull requests.

## Writing style

Use the house style: the Google developer documentation style guide, in British English, with no em dashes. This applies to docs, UI copy, error messages, CLI help and comments meant for users.

## Licence

Elastic License 2.0. Describe Hangar as "source-available", not "open source".
