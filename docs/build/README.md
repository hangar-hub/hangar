# Build notes

Working notes for the agents building Hangar v1.0 from [`docs/spec/15-build-plan.md`](../spec/15-build-plan.md). They are for the build, not for users.

## Folders

- `handoffs/`: one file per workstream, `handoffs/<ws-id>.md` (for example `handoffs/w4.md`). Write it when you finish: what is done by feature ID, what is tested and how to run it, known gaps, and anything the integration lead must wire. Phase 0 wrote `handoffs/p0.md`.
- `requests/`: one file per workstream, `requests/<ws-id>.md`, for changes you need outside the paths you own: an interface, the OpenAPI file, a migration, a shared helper. Give the change, the reason and the smallest compatible edit you made in your branch. The integration lead reconciles them in phase 2.

## Files

- [`interface-coverage.md`](interface-coverage.md): which interface methods serve each part of the Hub, git and Xet contracts. `internal/contracttest` checks it against the code.
- [`decisions.md`](decisions.md): decisions phase 0 made where the spec was silent or inconsistent, and open questions for James.
- [`release-dry-run.md`](release-dry-run.md): the v1.0.0 release dry run: artefacts, the checklist in spec 12 and what James needs to do.

## Working rules

- Build against the fakes in the `<pkg>test` packages. Every interface has one, with its own tests.
- Never run `go mod tidy` in phase 1; add dependencies with `go get` and a line in [`docs/dependencies.md`](../dependencies.md).
- `make build lint test` must pass before you hand off. Docker must be running: `make test`, `make cover` and `make test-compat` run the Go tests against one Postgres container and one MinIO container for the whole run (`scripts/with-test-postgres.sh` and `scripts/with-test-minio.sh`), and `make test` runs the UI tests even when the Go tests fail.
- When you run `go test` over many packages yourself, run it through both scripts too. Without them, each package starts its own containers with testcontainers, and its reaper can remove a package's MinIO while that package's tests still run (`Reaper handshake failed: read ack: EOF` in the output, then `connection refused`).
