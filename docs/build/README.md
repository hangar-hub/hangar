# Build notes

Working notes for the agents building Hangar v1.0 from [`docs/spec/15-build-plan.md`](../spec/15-build-plan.md). They are for the build, not for users.

## Folders

- `handoffs/`: one file per workstream, `handoffs/<ws-id>.md` (for example `handoffs/w4.md`). Write it when you finish: what is done by feature ID, what is tested and how to run it, known gaps, and anything the integration lead must wire. Phase 0 wrote `handoffs/p0.md`.
- `requests/`: one file per workstream, `requests/<ws-id>.md`, for changes you need outside the paths you own: an interface, the OpenAPI file, a migration, a shared helper. Give the change, the reason and the smallest compatible edit you made in your branch. The integration lead reconciles them in phase 2.

## Files

- [`interface-coverage.md`](interface-coverage.md): which interface methods serve each part of the Hub, git and Xet contracts. `internal/contracttest` checks it against the code.
- [`decisions.md`](decisions.md): decisions phase 0 made where the spec was silent or inconsistent, and open questions for James.

## Working rules

- Build against the fakes in the `<pkg>test` packages. Every interface has one, with its own tests.
- Never run `go mod tidy` in phase 1; add dependencies with `go get` and a line in [`docs/dependencies.md`](../dependencies.md).
- `make build lint test` must pass before you hand off. `make test` runs Postgres tests through testcontainers, so Docker must be running.
