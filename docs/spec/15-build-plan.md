# 15. Build plan

This plan builds Hangar v1.0 in one push with parallel agents. It is written for the agents doing the work and for the person running them.

## Shape

| Phase | Agents | Runs | Output |
|---|---|---|---|
| 0. Foundation | 1 | alone | A repo that builds, with every interface, the full schema, the OpenAPI file, fakes for every interface, CI and the compat harness. |
| 1. Workstreams | 11 | in parallel, as two batches (5, then 6) | Each workstream's packages, implemented and tested against fakes. |
| 2. Integration | 1 lead, plus fix agents as needed | after all of phase 1 | All branches merged, real wiring, every suite green. |
| 3. Hardening | 3 | in parallel | Security review fixes, a docs walk-through from a clean machine, load test results. |
| 4. Release | James | last | Public repo, tag `v1.0.0`. |

The workflow size guideline is under 10 agents per workflow. Run each phase as its own workflow, and phase 1 as two batches (W1 to W5, then W6 to W11). Workstreams build against fakes, so the batches do not wait on each other's code. Or raise the limit in `/config` and run all 11 at once.

## Rules for every agent

1. Read `CLAUDE.md`, `CONTEXT.md`, [01](01-product.md), [02](02-architecture.md) and the spec files your workstream names before writing code.
2. Work in your own git worktree on branch `ws/<id>-<slug>` (for example `ws/w4-hub-api`), created from the phase 0 commit.
3. Only edit files your workstream owns. If you need a change elsewhere (an interface, the OpenAPI file, a migration), write it in `docs/build/requests/<ws-id>.md` with the reason, make the smallest compatible change in your branch, and the integration lead reconciles it. Never change another workstream's package.
4. `go.mod`: phase 0 adds every dependency in [02](02-architecture.md). You may `go get` a new one (and add a line to `docs/dependencies.md`), but never run `go mod tidy` in phase 1, because it removes modules other workstreams need.
5. No stubs, `TODO`s or "not implemented" errors in shipped code. Fakes live only in `*test` packages.
6. Tests ship with code ([13](13-testing.md)). Before you hand off: `make lint test` green, plus any compat suite your workstream owns, run against fakes or your own wiring where the real pieces are not merged yet.
7. Conventional Commits (`feat(hfapi): ...`). No attribution lines in commits or pull requests.
8. House style for all prose, comments meant for users, UI copy and docs: British English, Google developer documentation style, no em dashes.
9. Finish with `docs/build/handoffs/<ws-id>.md`: what is done (by feature ID), what is tested, known gaps, and anything the integration lead must wire.

## Phase 0: Foundation

One agent. Everything later depends on it, so it must be complete and compile.

- Repo scaffold as in [02](02-architecture.md) "Repository layout"; `Makefile`; `.golangci.yml`; Biome config; `.editorconfig`.
- `go.mod` with every dependency listed in [02](02-architecture.md), pinned.
- `internal/config`: full config struct with defaults, env and flag loading, validation that reports every error, `hangar config reference`.
- `internal/db`: bun setup for both drivers, transaction helper, **every table in [02](02-architecture.md)** as migrations for both dialects, model structs, `dbtest` helpers that give a fresh SQLite or Postgres database.
- `internal/telemetry`: providers, resource, shutdown, helpers to create the instruments in [11](11-observability.md).
- `internal/httpx`: request ID, recovery, access log, auth extraction hook (calls an `auth.Authenticator` interface), Hub-style and problem-detail error writers, trusted proxies.
- Interfaces with doc comments, plus a fake for each in `<pkg>test`: `blob.Store`, `repo.Service`, `jobs.Queue` and handler registry, `auth.Authenticator` and `auth.Authz`, `upstream.Client`, `index.Indexer`, `scan.Scanner`, `webhook.Emitter`, `audit.Recorder`.
- `xet.Ingestor` interface with a fake (token issue, xorb put, shard accept), so W4 can mount the token endpoints.
- `datasets.Viewer` and `datasets.Layout` interfaces with fakes, so W6 and W8 can build against them.
- Domain types shared across packages in `internal/model` (RepoID, RepoType, CommitSHA, Revision, Entry, LFS pointer, Principal, Visibility, Role, Scope).
- `api/openapi.yaml` complete per [10](10-native-api.md), with `oapi-codegen` generating server interfaces into `internal/api/gen` and `openapi-typescript` generating `ui/src/api/schema.d.ts`; drift check target.
- Router skeleton in `cmd/hangar` that mounts each protocol's handler interface at the routes in [02](02-architecture.md), using fakes, so the binary starts.
- `ui/` scaffold: Vite, routes for every page in [09](09-web-ui.md) as empty shells with layout, theme, generated API client, Biome, Vitest, Playwright config.
- `compat/` harness: a launcher that starts a Hangar binary on a random port with a temp SQLite database and Blob store, pytest and Go test entry points, fixture download script with caching.
- CI workflows from [12](12-packaging-and-release.md) running whatever exists so far.
- Copy [03](03-hf-api.md) and [04](04-git.md) references into place; check every interface method the HF and git contracts need exists.

Done when: `make build lint test` passes, `hangar server` starts and serves `/healthz`, every interface has a fake with its own tests.

## Phase 1: Workstreams

| ID | Workstream | Owns | Spec | Features |
|---|---|---|---|---|
| W1 | Platform: Blob store, Jobs, GC | `internal/blob`, `internal/jobs`, `internal/gc`, `internal/api/admin_storage.go` | 02, 11 | OPS-1, OPS-4 (GC, usage, quotas enforcement helper), PULL-7 (rate limiter primitive), UP-5, DOWN-2 (presign) |
| W2 | Repo core: git storage and Repo service | `internal/gitstore`, `internal/repo`, `internal/api/repos.go`, `internal/api/uploads.go` | 02, 04 | REPO-1 to REPO-5, REPO-9, UP-6, Commit and ref CAS, tree and Commit caches, zip export Job |
| W3 | Git server and LFS | `internal/gitserver`, `compat/git` | 04 | UP-2, DOWN-3 |
| W4 | Hub API | `internal/hfapi`, `compat/hf` | 03 | UP-1, DOWN-1, REPO-6 and REPO-7 Hub endpoints, BROWSE-5 Hub collections endpoints |
| W5 | Identity and access | `internal/auth`, `internal/org`, `internal/audit`, `internal/api/{me,auth,orgs,admin_users,admin_tokens,admin_audit}.go` | 05 | AUTH-1 to AUTH-7, EVT-2, REPO-8 access requests |
| W6 | Upstreams and Pulls | `internal/upstream`, `internal/api/{discover,pulls,mirrors,admin_upstreams}.go`, `compat/upstream` | 06, 14 (DATA-7, DATA-8) | PULL-1 to PULL-12, BROWSE-2 backend, dataset Discover and Config, Split and Parquet-only Pulls |
| W7 | Metadata, Ollama and community | `internal/index`, `internal/scan`, `internal/ollama`, `internal/collections`, `internal/community`, `internal/webhook`, `internal/api/{collections,discussions,webhooks}.go`, `compat/ollama` | 07, 08 (Ollama), 10 | META-1 to META-3, DOWN-4, REPO-6 and REPO-7 core, BROWSE-5, EVT-1 |
| W8 | Web UI | `ui/`, `internal/web` | 09, 10, 14 (Viewer tab, card editor) | BROWSE-1 to BROWSE-6, UP-3, DOWN-5 UI, OPS-9 UI, every page |
| W10 | Datasets | `internal/datasets`, `internal/api/datasets.go`, `compat/viewer` | 14 | DATA-1 to DATA-13 (backend; the Viewer tab and card editor UI are W8's, against W10's API) |
| W11 | Xet upload ingestion | `internal/xet` (including the `xet.gc` Job for xorb retention), `internal/api/xet.go` (token endpoints are mounted by W4's router through an interface), `compat/xet` | 03 (Xet), research `xet-upload-research.md` | UP-7 |
| W9 | CLI, packaging and docs | `internal/cli`, `deploy/`, `.goreleaser.yaml`, `Dockerfile`, `.github/`, `docs/` (except `docs/spec`, `docs/build`), `README.md`, repo files, `compat/lmstudio`, `loadtest/`, `scripts/` | 08 (CLI, LM Studio), 11 (dashboards), 12, 13 | UP-4, OPS-3 packaging, OPS-5 dashboards, OPS-6 docs, OPS-7, OPS-8, every docs page |

### Critical path and order of attention

W2 is the core every other server workstream builds on. W2 publishes its `repo.Service` behaviour early: as soon as Commit, Tree, Entry, OpenFile, Resolve and UpdateRef work on SQLite and the filesystem, W2 pushes its branch so W3, W4 and W6 can test against the real implementation as well as the fake. The same applies to W1's filesystem `blob.Store` and W5's `Authenticator`.

### Per-workstream definitions of done

- **W1:** conformance suite green on filesystem and MinIO, including multipart and presign; Job queue claim, lease expiry, dedupe and scheduling tested on SQLite and Postgres; distributed rate limiter test with 2 workers; GC never deletes an object referenced by a Commit or an in-progress upload.
- **W2:** Commits created through `repo.Service` are byte-identical git objects to what `git` would produce for the same tree (checked with `git fsck` and `git cat-file` on an exported copy); ref CAS under concurrency; rename redirects; soft delete and purge; Partial mirror presence flags.
- **W3:** `compat/git` green against W2's implementation; LFS batch with basic and multipart transfers; locks API behaviour per [04](04-git.md).
- **W4:** `compat/hf` green against W2's implementation for everything that does not need Pulls; every MUST in [03](03-hf-api.md) implemented; compatibility matrix data file generated by the suite.
- **W5:** local login, invites, reset, OIDC against Keycloak in `testcontainers`, token scopes and restrictions, permission matrix table test covering every cell in [05](05-auth.md), Ollama signature verification with keys generated in the test, audit events for every listed action.
- **W6:** `compat/upstream` green; Pull resume after kill; Dependency resolution on fixtures including the Jina form; Discover arsenal-state merging; pull windows and bandwidth cap; imports from an HF cache and LM Studio folder.
- **W7:** parser fuzz targets; index output for every test fixture checked against golden files; pickle scanner catches the malicious fixtures and passes the benign ones; `compat/ollama` green; webhook signing and retries.
- **W8:** every page in [09](09-web-ui.md) and the dataset Viewer tab and card editor in [14](14-datasets.md), built against the generated client, with a mock server from the OpenAPI file for development; Playwright flows for sign-in, Discover and Pull, upload, browse files and GGUF viewer, token creation, admin settings; axe clean; screenshots at three widths committed to `docs/assets/screenshots/`.
- **W10:** Config and Split resolution matches the `datasets` library on its own test cases; Parquet export for every listed source format; `compat/viewer` matches the Hub within the stated tolerances; `where` and `orderby` parser fuzzed; `/rows` and `/filter` load targets met; Croissant output validates with `mlcroissant`.
- **W11:** xorb, shard and hash code passes the `xet-team/xet-spec-reference-files` vectors; xorb and shard parsers fuzzed; `compat/xet` green: `upload_file` and `upload_folder` with `hf_xet` 1.6.0 and the latest 1.x installed, the same file to a second Repo (deduplicated through the client cache), resume after killing the client, token expiry during a long upload, `/v2/shards` heartbeats behind a proxy with a 30-second idle timeout, `reject` mode, and every reassembled object's SHA-256 matching the source.
- **W9:** CLI commands with tests against a test Instance; image builds for both architectures; Compose examples start; Helm chart lints and installs on kind; docs site builds with no broken links; generated references (config, CLI, metrics) wired into the docs build.

## Phase 2: Integration

One lead agent.

1. Merge branches in this order, resolving conflicts and `docs/build/requests/*`: W1, W2, W5, W3, W4, W11, W7, W10, W6, W9, W8. Run `go mod tidy` once after all merges.
2. Replace fakes with real implementations in `cmd/hangar` wiring; register every Job handler.
3. Run everything: `make lint test`, all compat suites, Playwright, smoke tests on every Compose example and Helm on kind.
4. For each failing area, start a fix agent scoped to the owning workstream's packages, with the failing output. Repeat until green.
5. Write `docs/build/integration-report.md`: feature ID checklist with test evidence, open issues.

## Phase 3: Hardening

Three agents in parallel, each fixing what it finds (within the owning packages) and reporting.

- **Security:** write `docs/security/threat-model.md`; review authn and authz on every route (a test that enumerates the router and checks each route against the permission matrix), path traversal in tree, zip and import paths, SSRF in Upstreams and webhooks, presigned URL scope and lifetime, secrets in logs and spans, CSRF, CSP, git protocol abuse (huge packs, deep trees), pickle scanner bypasses, dependency CVEs.
- **Docs walk-through:** on a clean container, follow every Get started and Guides page exactly as written; fix the docs or the product wherever they disagree; check house style.
- **Performance:** run `loadtest/` on the reference setup, fix regressions against the targets, commit `docs/benchmarks.md`.

## Phase 4: Release

On hold until James says go ([decisions](../build/decisions.md), "Release on hold"): before then, nothing is pushed, tagged or published, and work stops after the release dry run.

James creates `github.com/hangar-hub/hangar` (public), pushes `main`, enables Pages, private vulnerability reporting and the CLA app, creates the Homebrew tap repo, and merges the release-please PR for `v1.0.0`.
