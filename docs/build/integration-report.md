# Integration report

Phase 2 step 5 of [`15-build-plan.md`](../spec/15-build-plan.md): every feature ID in [`01-product.md`](../spec/01-product.md) and [`14-datasets.md`](../spec/14-datasets.md) with the test that proves it, the Hub API contract MUSTs, the suite results, the request resolutions and the open issues.

It describes branch `integration/v1` at `97d17c1`, ready for release, and [Releases (2026-10-18 and 2026-10-19)](#releases-2026-10-18-and-2026-10-19) records v1.0.0 and v1.1.0 and the CI fixes after them. [Pre-release check (2026-10-18)](#pre-release-check-2026-10-18) has the last run of every suite, all green; earlier sections describe earlier rounds. [Phase 3 decisions and final check (2026-10-18)](#phase-3-decisions-and-final-check-2026-10-18) has the merges since Phase 3 and the final run of every suite, all green, after which `main` was fast-forwarded. [Phase 3 hardening (2026-10-17)](#phase-3-hardening-2026-10-17) describes what Phase 3 found and fixed and its round 4 results at `6610f45`; [Preview feedback round (2026-10-17)](#preview-feedback-round-2026-10-17) and the later sections describe earlier rounds.

## Summary

- Phase 3 reviewed security (authorisation, untrusted input, the scanner and download gate), performance, the docs and the open gaps, and fixed 61 findings, 3 of them critical and 17 high. The [threat model](../security/threat-model.md), [benchmarks](../benchmarks.md) and [docs walk-through](docs-walkthrough.md) are written.
- Final check at `a998d78` (2026-10-18): every suite green, run one after another by the lead: lint, unit on SQLite and Postgres, coverage, generated code, client compatibility (7 Go packages, pytest 71 passed and 2 skipped), UI on the mock server (542 passed) and on two real Instances (369 passed), deploy checks, image, four Compose examples, kind (9 tests) and the docs site. Round 4 of Phase 3 at `6610f45` had left the unit suite unfinished and one coverage gap; both are closed.
- Every feature ID has at least one named test, and every test named here exists in the tree (checked in the round 8 audit). Every MUST in section 13 of the Hub API contract has a handler and a test; the round 4 Hub matrix check lists 85 endpoint and test pairs.
- 108 request items have a resolution; 3 are deferred. W11 8 (who may reference an LFS or Xet object by its hash) was decided by James on 2026-10-18: references are checked at Commit.
- James decided the five Phase 3 questions on 2026-10-18 (see [decisions](decisions.md)). He decided the last product question, self-service profile and password changes (W8 8), the same day; it is closed on branch `feat/self-service-account` (see [W8 8 closed](#w8-8-closed-self-service-profile-and-password)). He gave the go-ahead on 2026-10-18, and v1.0.0 and v1.1.0 are released (see [Releases](#releases-2026-10-18-and-2026-10-19)).
- AWS S3 passed in full on 2026-10-15 after the fixes for the 2026-10-14 failures.
- The preview Instance's large import, red in earlier rounds, passed on `382cc88` on 2026-10-17: a 19.85 GB GGUF imported with its SHA-256 matching huggingface.co (bellatrix PRs #121 and #122). The preview has not yet been updated past `382cc88`.

## Releases (2026-10-18 and 2026-10-19)

James gave the go-ahead on 2026-10-18. Both releases ran through release-please and `release.yml` on GitHub Actions, with no manual publishing step.

| Release | Tag commit | Release run | Checked after |
|---|---|---|---|
| v1.0.0 | `91f5535` | passed, 2026-10-18 | `cosign verify` of the image and the checksums, anonymous GHCR pulls of the image and the chart, Homebrew cask `hangar-hub/tap/hangar` installed and `hangar version` prints 1.0.0 |
| v1.1.0 | `268d6eb` | passed, 2026-10-19 | `cosign verify ghcr.io/hangar-hub/hangar:1.1.0` (identity `release.yml@refs/tags/v*`, issuer GitHub Actions), image for `linux/amd64` and `linux/arm64`, chart 1.1.0 from `oci://ghcr.io/hangar-hub/charts/hangar`, cask 1.1.0 upgraded and `hangar version` prints 1.1.0 at `268d6eb`, docs `versions.json` lists 1.1 as `latest` |

The docs and the demo moved from GitHub Pages to Cloudflare Workers Builds, with branch Previews under `hangar.page` (see [decisions](decisions.md), 2026-10-19). Checked on 2026-10-19 with a throwaway branch: `<name>.docs.hangar.page` served that branch's docs with `X-Robots-Tag: noindex` and the security headers, and `<name>.demo.hangar.page` served the demo with its CSP, `X-Frame-Options: DENY` and `noindex`, and answered `/api/hangar/v1/me`. Neither Worker answers on `workers.dev`. The v1.1.0 deploy of `demo` used the configuration from before Previews, which turned off Previews on `demo.hangar.page`; they were turned back on, and from the next release the `demo` branch's configuration keeps them on.

### CI failures fixed after the release

The first public CI runs showed intermittent failures that did not appear on the development machine. Each was traced to a cause and fixed; no test was skipped, retried by the harness or loosened.

| PR | Failure | Cause and fix |
|---|---|---|
| #11 | `TestPullFromUpstreamIntoTheHFCache` counted 5 requests, not 4, on arm64 | The test fake counted a request after its handler returned. It now counts when the response starts |
| #14 | `TestPartUploadRetriesFaults/NoResponse` on arm64 | A 200 with no `ETag` was taken as a finished part. `PutPart` now retries it, and a `NoETag` fault covers it |
| #14 | `TestCacheEvictsLeastRecentlyUsed` on amd64 | `gitstore.Store.Acquire` chose at random between a finished sync and a cancelled context. It now returns the context's error first |
| #19 | `TestRealBandwidthCapAcrossTwoWorkers` moved 60.9 to 89.5% of the cap | The shared bandwidth limiter left budget unused when a worker finished or joined. An idle worker now gives its slice back, and a worker with less than the whole budget looks for more every quarter of a refresh period. All 48 local runs of a 10-second window passed after the fix; all 27 that split the files across workers failed before it. Some CI failures took about 30 seconds longer than the limiter explains; the test now logs each Job's attempts, so a recurrence shows the cause |
| #21 | The chart's helm test was refused on the Service address | `helm install --wait` returns when the Pod is ready, a moment before the Service routes to it. `hangar healthcheck --retry-for` retries while the connection fails, and the helm test uses it. A `/readyz` answer other than 200 still fails at once |

### Still open after the releases

- **100,000 repositories per Instance** is not measured. The benchmarks stop well short of it.
- **AWS S3** was last run in full on 2026-10-15, before the releases. It is to be rerun from James's home network.
- **The bellatrix preview** still runs the Harbor build `preview-c79363e`, not a released image. Moving it to the GHCR image and chart needs a `pg_dump` first, because migrations 0002 and 0003 are new.
- **Manual checks for James:** LM Studio against a real server, and the quickstart on a clean machine.

## Pre-release check (2026-10-18)

After the final check at `a998d78`, `integration/v1` took the self-service account changes (W8 8, closed), the release dry run's fixes and [report](release-dry-run.md), the Cloudflare docs hosting and mock demo, the move to `github.com/hangar-hub/hangar`, the release GitHub App, the action SHA pins and a 14-day Dependabot cooldown (see [decisions](decisions.md), 2026-10-18). The lead then ran every suite one after another at `97d17c1`:

| Suite | Command | Exit | Result |
|---|---|---|---|
| Lint | `make lint` | 0 | |
| Unit, SQLite and Postgres, S3 on MinIO | `make test` | 0 | 451 s |
| Coverage | `make cover` | 0 | every package at or above its floor |
| Generated code | `make check-generate` | 0 | no differences |
| Client compatibility | `make test-compat` | 0 | 7 Go packages ok; pytest 71 passed, 2 skipped |
| UI, mock server | `CI=1 make e2e` | 0 | 557 passed, 43 skipped by design |
| UI, demo Worker under `wrangler dev` | `CI=1 make test-demo` | 0 | 563 passed, 43 skipped by design |
| UI, two real Instances | `playwright test -c playwright.real.config.ts` | 0 | 378 passed; 0 `WARN` or `ERROR` lines in either server log |
| GoReleaser configuration | `goreleaser check` | 0 | |
| Chart, Compose and workflow checks | `make lint-deploy` | 0 | all 28 action refs pinned to the commit their tag names |
| Image | `make image` | 0 | |
| Compose examples | `scripts/compose-smoke.sh <example> --skip pull` | 0 | quickstart, postgres-minio, scale, oidc-keycloak |
| Helm on kind | `make test-kind` | 0 | |
| Docs site | `make docs` | 0 | `mkdocs build --strict` |
| Signing and SBOM rehearsal | `make release-rehearsal` | 0 | checksum and image signatures verify, a wrong key fails, SPDX attestation verifies (158 packages) |

Still open before or at release: the AWS S3 suite needs a rerun from a fast connection (the dry run's was cut short at 3 to 5 KB/s); the "100,000 Repos per Instance" target was not measured; the preview on bellatrix still runs `382cc88`; and James's release steps in spec 12.

## Phase 3 decisions and final check (2026-10-18)

James answered the five open questions from Phase 3 on 2026-10-18; each decision, its code and its tests are in [decisions](decisions.md) under "Phase 3 decisions by James (2026-10-18)", and the [threat model](../security/threat-model.md) has no open decisions left. Merged into `integration/v1`:

| Merge | Branch | What it does |
|---|---|---|
| `871d1bf` (fast-forward) | `fix/p3-5-lead` | The items left open after Phase 3: the tag case of security input 6c through the full server, the Viewer scan time check, a URL DSN refused with the SQLite driver, upload completion status and a size-scaled timeout in the CLI, axe in both themes on every page, and `TestKindBlobStoreStartsLate` |
| `1f60a82` | `fix/p3-6-misc` | Namespace profiles need sign-in while `anonymous_read` is off; jittered backoff for concurrent Commits to one branch; `hangar ollama install` checks the quant first |
| | `fix/p3-6-readyz` | `/readyz` names the Blob store only when start-up waits for it (`36e4444`): `TestReadyzWhileWaitingForMigrate` failed 6 in 50 runs under load before, 0 in 200 after |
| `a998d78` | `fix/p3-6-scan` | `scan.block_level` (`unsafe` or `suspicious`); LFS and Xet object references checked at Commit (`lfs_uploaders`, migration `0002`) |

The lead then ran every suite one after another at `a998d78` (`9ca750d` only changes one link in the threat model):

| Suite | Command | Exit | Result |
|---|---|---|---|
| Lint | `make lint` | 0 | 198 s |
| Unit, SQLite and Postgres, S3 on MinIO | `make test` | 0 | 581 s |
| Coverage | `make cover` | 0 | every package at or above its floor |
| Generated code | `make check-generate` | 0 | no differences |
| Client compatibility | `make test-compat` | 0 | 7 Go packages ok; pytest 71 passed, 2 skipped. The first run failed only in `compat/viewer`, where two tests could not resolve `huggingface.co` (`gaierror(8, 'nodename nor servname provided, or not known')`); the rerun passed every package |
| UI, mock server | `CI=1 make e2e` | 0 | 542 passed, 43 skipped by design |
| UI, two real Instances | `playwright test -c playwright.real.config.ts` | 0 | 369 passed; 0 `WARN` or `ERROR` lines in either server log |
| Chart, Compose and workflow checks | `make lint-deploy` | 0 | |
| Image | `make image` | 0 | |
| Compose examples | `scripts/compose-smoke.sh <example> --skip pull` | 0 | quickstart, postgres-minio, scale, oidc-keycloak |
| Helm on kind | `make test-kind` | 0 | 9 tests passed, `TestKindBlobStoreStartsLate` included |
| Docs site | `make docs` | 0 | `mkdocs build --strict` |

Not run in this check: the floor-client cells, the diffusers cell, the 6 GiB nightly upload, fuzzing, a k6 rerun after the Commit backoff change (`docs/benchmarks.md` keeps the 2026-10-17 numbers), and the preview on bellatrix, which still runs `382cc88`.

## Phase 3 hardening (2026-10-17)

Phase 3 of [`15-build-plan.md`](../spec/15-build-plan.md) ran from `382cc88` on 2026-10-17 in six areas, each on its own branch with a handoff in `handoffs/`: authorisation and browser security, untrusted input, the scanner and download gate (together the security review), performance, the docs walk-through and the open gaps from this report. All six merged into `integration/v1` (`c136924`, `15da985`, `80b59c0`, `6287fc4`, `40ad448`, `19d259e`). The lead then aligned the UI's Token scope help with the "Hub actions only" decision (`c4fc196`), filed the Phase 3 decisions and open questions in [decisions](decisions.md) (`d9c0eab`), fixed how the bypass test sends its Token (`f06d896`) and added the [threat model](../security/threat-model.md) (`132da07`). Three audit rounds followed, each merged as a fix branch:

| Round | Merge | What it fixed |
|---|---|---|
| 2 | `4c3d8f2` (`fix/p3-2-input`) | Tests for the `gitstoretest` pack builders, so the helper package meets its 75% coverage floor (`6686e9e`) |
| 2 | `d1cfd3d` (`fix/p3-2-docs`) | A broken spec link in [decisions](decisions.md); `scripts/check-doc-links.py` now checks every relative Markdown link and runs in `make lint-style` (`8dbcf62`) |
| 3 | `2ed8ed7` (`fix/p3-3-authz`) | `TestRouteAuthorisationMatrix` failed about once in 41 runs on a loaded host: its probe read the route before the access log recorded it. The probe now waits for its request's log line (`b9cb5d5`); the server code was correct |
| 3 | `f16466b` (`fix/p3-3-docs`) | Every subcommand's help headings in sentence case (`87cbc4d`) |
| 4 | `6610f45` (`fix/p3-4-docs`) | Two threat model test citations named the wrong file; `scripts/check-test-citations.py` now checks every cited test against the file cited for it. Spaces before percent signs are flagged (`8257431`) |

`6610f45` is the branch head and the commit every suite in [Suite results at `6610f45`](#suite-results-at-6610f45) ran on. `main` stays at `382cc88`: the unit suite did not finish and the audit found one coverage gap, so nothing was fast-forwarded, pushed or published.

### What each area found and fixed

Each handoff lists every finding with its commit and regression tests. 61 findings in total, 3 of them critical and 17 high.

**Authorisation and browser security** ([`p3-authz.md`](handoffs/p3-authz.md), 7 findings: 2 high, 1 medium, 3 low, 1 process).

- A write Token could do every Repo admin action through every API since `54e5307`. James decided on 2026-10-17 on "Hub actions only": a new `hub_admin` action lets a write Token delete a Repo, change its visibility and gating, and rename it within its Namespace through the Hub API only (`425a744`). Transfers, webhooks, collaborators, access requests and native admin actions need the admin scope again. Tests: `TestWriteTokenAdministersReposThroughHubAPIOnly`, the `hub_admin` column of `TestPermissionMatrix`.
- Cross-origin requests with ambient credentials (Basic or the session cookie) are refused with 403 through Go's `CrossOriginProtection`, sign-in included, and a Token as a Basic password works only on git and git-lfs routes (`5ad144f`). Tests: `TestCrossOriginBrowserCredentials`, `TestBasicCredentialsOnlyForGit`.
- An expiring Token can no longer create a Token that outlives it (`5199150`); signed URL fields are escaped before signing (`c1d1038`); raw paths no longer reach route templates (`4b8215c`); UI pages send `Cross-Origin-Opener-Policy` and `Permissions-Policy` (`4b609f3`).
- `TestRouteAuthorisationMatrix` finds every route of every router, fails on a route without an authorisation rule, and probes all 222 routes as six callers. `TestSecretsStayOutOfLogsAndTelemetry` checks 16 secrets against debug logs and OTLP output. No secret was found in logs or telemetry.

**Untrusted input** ([`p3-input.md`](handoffs/p3-input.md), 10 findings: 2 high, 6 medium, 2 low).

- Delta bombs and tree fan-out bombs in pushes and Pulls could exhaust a replica's memory from a pack of a few kilobytes. Packs are checked as they stream, before go-git or `git index-pack` rebuilds them, and Commits are limited to 1,000,000 paths of at most 4,096 bytes (`dc3918b`, `aa8a2ce`, `2188682`, `bdd53d3`). Tests: `TestPushRefusesDeltaBomb`, `TestApplyPackRefusesTreeBombs`.
- Path components that git clients check out as `.git` (NTFS and HFS+ spellings), zip export entries outside the Repo's folder, and unbounded ref updates, objects and push options are refused (`dc3918b`).
- Upstream redirects and webhooks can no longer reach reserved addresses, such as the cloud metadata service; the check runs on the dialled address, so DNS rebinding cannot pass it (`65f13b7`). Tests: `TestUpstreamRedirectsCannotReachTheLocalNetwork`, `TestReservedTargetsAreAlwaysRefused`.
- `hangar push` and `hangar import` no longer follow symbolic links out of the uploaded folder (`9cf64d1`, `f7c80a7`); Upstream ref advertisements are bounded (`8bf25b8`); kin-openapi moved past two CVEs (`97a9692`).
- The Phase 2 residuals W6 (cross-Namespace Pull starvation) and W11 (Xet index CPU amplification) were confirmed fixed by reverting each fix and watching its test fail.

**Scanner and download gate** ([`p3-scan.md`](handoffs/p3-scan.md), 15 findings: 3 critical, 8 high, 4 medium).

- Critical: joblib array bytes hid calls from the scanner; a `.bin` beside an `.xml` of the same name was never read (security input 6a); and scan errors never blocked a download (6b). Every file a loader may unpickle is now read by content, joblib files are read as joblib reads them, and the gate refuses partial results of files that can run code (`bdcd21b`, `4e62696`).
- High: compressed and archived pickle names that standard loaders open were never scanned; objects were judged by ref tips only, so an older Commit could hold an unsafe file under the same OID (6c); `git clone` was not gated; and the opcode walker differed from CPython on `.npy` files, protocol 0 strings, Python 2 names, dotted names and deny list entries. `Gate.CheckGit` now gates every fetch (`f65ba44`).
- `scan.Version` is 2, and workers rescan older results at start. `TestUnsafeDownloadGateCatchesScannerBypasses` drives each bypass through the Hub API, LFS batch and Ollama.

**Performance** ([`p3-perf.md`](handoffs/p3-perf.md), 7 findings: 1 high, 2 medium, 4 low).

- Unbounded argon2id checks on unauthenticated sign-in let 40 parallel failed sign-ins OOM-kill a web replica. At most two hashes run at once per process (`89631fa`; `TestPasswordHashingIsBounded`).
- `loadtest/upload.js` had never passed because of a header bug, and repeated runs uploaded nothing (`1044d4c`, `2581fd6`; `loadtest/lib/payload.test.js`).
- A Token-authenticated `HEAD /resolve` went from 14 queries to 8 (`7857795`, `8ae082e`, `39d739b`, `ed2ef92`; `TestHubDownloadQueries`).

**Docs walk-through** ([`p3-docs.md`](handoffs/p3-docs.md), 16 findings: 4 high, 8 medium, 4 low). See [Docs walk-through](#docs-walk-through).

**Open gaps** ([`p3-gaps.md`](handoffs/p3-gaps.md), 6 items: 2 medium, 3 low, 1 with no change needed).

- A server now listens and answers `/healthz` while it waits up to `storage.s3.startup_wait` (default `5m`) for an S3 Blob store that does not answer yet, instead of exiting (`22eded4`; `TestServeAnswersHealthWhileWaitingForTheBlobStore`). Decision recorded in [decisions](decisions.md) and spec 02.
- 23 tests returned early and reported PASS when Postgres or S3 was not configured. They now skip visibly through `dbtest.RequirePostgres` and `blobtest.RequireS3`, and `scripts/check-silent-skips.sh` in `make lint` stops the pattern returning (`81978ac`).
- `/admin/system` reports `upload_mode` (`a23e48e`); `scripts/kind-chart-test.sh` fails at once without the Hangar image (`cebfa8f`); datasets with no Viewer files log at `DEBUG` (`f210b05`).
- `make release-rehearsal` ran for the first time and passed with no changes, with a throwaway signing key.

### Residual risks

The [threat model](../security/threat-model.md) lists each residual risk under its trust boundary, with the code and tests behind each mitigation, and its "Open decisions" section listed three security questions for James, which he decided on 2026-10-18 (see [decisions](decisions.md), "Phase 3 decisions by James"). In short:

- **Decisions for James (decided 2026-10-18).** Whether the download gate also refuses suspicious pickles; who may reference an LFS or Xet object by its hash (global deduplication lets anyone who knows an object's SHA-256 and size commit a pointer to it, and the batch API reveals whether an object exists); and whether Namespace profiles and member lists are public when `anonymous_read` is off. [decisions](decisions.md) also asks about concurrent server-side Commits to one branch (409 after three retries, from the performance review) and Ollama's handling of a quant that is not pulled (from the docs walk-through).
- **Accepted and documented.** Presigned URLs are scoped to the object and cannot be revoked before they expire; no HSTS header or per-IP rate limit (the proxy's job); a sign-in flood still delays sign-ins on a replica; Ollama blob responses send no `nosniff` header; `webhooks.allow_private_targets` is on by default; a Namespace admin's write Token can force-push the default branch; paths that differ only in case or Unicode normalisation can coexist; pickles under names no standard loader uses are not scanned; xz and lzma pickles give partial results and are refused; histories past the gate's walk limits are refused for git fetches.
- **From the gaps and performance reviews.** Every S3 error is retried at start, so a wrong bucket fails after `startup_wait` instead of at once; clone memory is git's (a replica peaked at 496 MiB); and every benchmark comes from one shared laptop.

### Load test results

[Benchmarks](../benchmarks.md) has the results, setup, commands and deviations. Every scale target that a scenario measures passed on the reference Compose setup (two web replicas at 2 CPUs and 1 GiB, Postgres, MinIO): `HEAD /resolve` p99 7 to 28 ms with the host lightly loaded (target 50 ms; 87 to 206 ms when other workloads saturated the host), model info p99 12.5 to 62.7 ms, a 100,000-file listing p95 6.15 to 6.79 s, a 10,000-Commit clone p95 0.60 to 1.13 s, replica memory peaks of 81 MiB in uploads and 115 MiB in downloads, 1.2 GB/s downloads in `proxy` mode, start to ready in 0.85 to 1.61 s, and Dataset Viewer `/rows` p99 16.1 and 27.8 ms on 10 million rows. 100,000 Repos per Instance and a 50 GiB upload were not measured. The sign-in burst that OOM-killed a replica before `89631fa` now peaks at 209 MiB per replica with no restarts.

### Docs walk-through

[Docs walk-through](docs-walkthrough.md) logs every step of every Get started and Guides page, followed as written on 2026-10-17 against local stand-ins for the unpublished image, archives and chart, with real clients. Of its 16 findings, four blocked a documented path: `hangar push --branch` to a new branch never worked (`320939d`, `TestPushToANewBranch`), the first-pull guide's Repo could not be found in Discover (`4f57f3f`, `5554cb5`), the Helm guide read the wrong Secret (`1a0622e`, `TestChartGuideReadsTheGeneratedSecret`), and first sign-in pointed at a password change that does not exist (`5554cb5`). The medium findings fixed an anonymous 401 from huggingface.co read as bad credentials (`33e5dc7`), two dataset Mirror conversion bugs (`bb95881`, `a690d1d`), gating settings cleared by the UI (`8715a05`), and wrong Ollama, diffusers, Polars and landing page examples. vLLM, Homebrew, the Windows and macOS archives, Argo CD itself and guides at 2 GiB and above were not covered.

### Suite results at `6610f45`

Round 4 ran on 2026-10-18 on detached worktrees at `6610f45` (`Merge branch 'fix/p3-4-docs'`), Go 1.27.2, Node 24.19.0, Python 3.11.7, `huggingface_hub` 2.2.0 and `hf_xet` 1.7.0, on a shared machine with other projects' workloads.

| Suite | Command | Result |
|---|---|---|
| Lint | `make lint` | Exit 0: golangci-lint 0 issues, `go vet` for the compat tag sets, Biome 219 files, `tsc -b`, `lint-style` (test timeouts, test targets, silent skips, 85 Hub endpoint and test pairs) |
| Workflows | `actionlint .github/workflows/*` | Exit 0, no findings |
| Vulnerabilities | `go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...` | 0 vulnerabilities reached; 1 in a required module, not called |
| Unit and integration | `make test` (`-race -count=1 -timeout 30m -tags integration`, SQLite, Postgres 18 and MinIO) | **Not finished.** The runner stopped it about 2 minutes in, before any package reported, because the workflow asked for its result early. `make cover` and `make check-generate` did not start. Not a test failure, but the suite is not green |
| Audit, tests | `go test -race -count=1 -tags integration -run <every test the threat model cites> ./...` on Postgres and MinIO | All 102 cited Test and Fuzz names exist at the files cited; 27 of 27 packages ok. `TestOIDCKeycloak` ran against a real Keycloak |
| Audit, flakiness | `TestRouteAuthorisationMatrix`, `TestSecretsStayOutOfLogsAndTelemetry`, `TestUnsafeDownloadGateCoversEveryRoute`, `TestUnsafeDownloadGateCatchesScannerBypasses` and `TestWriteTokenAdministersReposThroughHubAPIOnly` with `-race -count=3` | 3 of 3 each. The route matrix took 40 to 52 s |
| Audit, other | `make test-compat`, `make lint-style`, Vitest, a check of the generated CLI and configuration references | All passed: Vitest 113 of 113; the generated references match the binary. No test was skipped, weakened or deleted since `382cc88`; every changed threshold tightens. **One gap:** see [Still open after Phase 3](#still-open-after-phase-3) |
| Client compatibility | `HANGAR_TEST_POSTGRES=1 HANGAR_TEST_MINIO=1 make test-compat` | Exit 0. Go: `git` 46, `harness` 7, `hf` 2, `lmstudio` 1, `ollama` 29, `upstream` 26 and `viewer` 100 tests and subtests, 0 skipped, 0 failed (run twice). pytest: 69 passed, 2 skipped |
| Hub clients against the binary | `HANGAR_HF_INSTANCE=binary uv run --frozen --project compat pytest compat/hf` | 42 passed, 2 skipped in each of four cells: SQLite and Postgres, each with Xet uploads off and on. The skips are the `diffusers` and `upload_large_folder` tests, which run in other matrix cells |
| Xet | `uv run ... pytest compat/xet` (README command) | 16 passed in each of six runs: the fake backend, `xetreal` on SQLite and `xetreal` on Postgres, each with `hf_xet` 1.7.0 and 1.6.0 |
| Image | `make image IMAGE=hangar-p3:test` | Built |
| Chart and scripts, static | `make lint-deploy` | Exit 0: `helm lint` for 3 values files, kubeconform valid for every render, shellcheck, 26 action refs, 8 jobs with uv |
| Compose | `HANGAR_IMAGE=hangar-p3:test scripts/compose-smoke.sh <example> --skip pull` | 6 of 6 checks on each of `quickstart`, `postgres-minio`, `oidc-keycloak` and `scale`, including upload, download and `git clone` |
| Helm on kind | `make test-kind IMAGE=hangar-p3:test` | 8 PASS, 0 FAIL: the three kind script tests, `TestKindGeneratedSecretsSurviveRenders`, `TestKindNotesReadOnlyRenderedSecretKeys` (2 subtests) and `TestKindGitOpsInstall`; package ok in 36 s |
| UI against the mock | `pnpm exec playwright test` (no retries) at 390, 1024 and 1440 px | 356 passed, 43 skipped by design (30 screenshot tests that need `SCREENSHOTS=1`, desktop-only budget and lint tests, the phone card-height test). Axe in light on every page and in dark on 10 pages at each width |
| UI against a real Instance | `pnpm exec playwright test -c playwright.real.config.ts` against two `make build` binaries (no retries) | 210 passed, 0 skipped. No `WARN` or `ERROR` line in either server's log |

No test failed and none was flaky. Earlier in Phase 3, before the audit rounds, each area ran its own suites: `ws/p3-gaps` ran `make test` with 2,195 Go tests and subtests passed and 0 skipped, and the others are in their handoffs. None of those replaces a full `make test` at `6610f45`.

### Still open after Phase 3

Branch `fix/p3-5-lead`, from `55ee4e2`, closed every item below on 2026-10-18 and fixed two problems found on the bellatrix preview. Results are in [Suite results on `fix/p3-5-lead`](#suite-results-on-fixp3-5-lead).

- **Rerun the unit suite (must). Closed.** `make test`, `make cover` and `make check-generate` all passed on `fix/p3-5-lead`; see the results table.
- **Tag case of security input 6c through the full server. Closed** (`e83e5f2`). `TestUnsafeDownloadGateRefusesObjectsHeldOnlyByATag` in `cmd/hangar/unsafe_bypass_test.go` tags a Commit holding a pickle, with a lightweight tag and with an annotated tag, deletes the pickle from `main`, and checks 403 from the Hub API resolve on the tag, the LFS batch and `/v2/.../blobs/sha256:<oid>`. The server already refused all three; no gate change was needed.
- **Dark-theme axe coverage. Closed** (`e7770dc`). Both Playwright suites now run axe in the dark theme on every signed-in and signed-out page they check in light, and check that the page follows the emulated scheme. No violation was found in the teal dark palette.
- **Viewer scan timing flake. Closed** (`0733bd8`). `internal/datasets/viewer/scan.go` now reads the clock once every 4,096 rows read, never before the first row, and uses the Viewer's injected clock. `TestScanTimeLimitMakesResultsPartial` drives the limit with a clock that moves a minute per read and fails on the old check. `TestScanLimitsMakeResultsPartial` and `TestScanTimeLimitMakesResultsPartial` passed 200 of 200 runs each with `-race -count=200`.
- **No kind run starts the Blob store late. Closed** (`21bcaaf`). `TestKindBlobStoreStartsLate` in `deploy/kind_test.go` installs Hangar and Postgres without MinIO, checks that the web Pod answers `/healthz`, is not ready on `/readyz` and logs that it waits for the Blob store, then starts MinIO and checks that the web and worker Pods become ready with no container restart.
- **Load-sensitive compat tests.** Not rerun on this branch; no change.

Found on the bellatrix preview and fixed:

- **A Postgres DSN with the default SQLite driver (medium).** With `database.driver` left at `sqlite`, a DSN such as `postgres://user:pass@host:5432/hangar` passed validation, and Hangar created a SQLite database and `hangar.lock` at the relative path `postgres:/user:pass@host:5432/hangar`. Validation now refuses a SQLite DSN with any URL scheme other than `file:`, names `database.driver` and suggests `postgres`, and names only the scheme, so the password never reaches the error; `db.Open` refuses it too and creates nothing (`04678aa`). Tests: `TestValidateRefusesURLDSNsForSQLite`, `TestSQLiteRefusesURLDSNs`. Every script and test helper in the tree that sets a Postgres DSN also sets the driver; the only caller that did not was the `instance-postgres` backend of `compat/viewer`, fixed in `16fef27`, which created `compat/viewer/postgres:/...`.
- **The CLI waited silently on long upload completions (medium).** On a 19.85 GB GGUF the CLI's fixed 5-minute response timeout cut off the upload completion while the Instance was still verifying the file; the Instance logged 499 and carried on, the retry got 200 after 63 seconds, and the CLI printed nothing. The completion now waits 5 minutes or 40 seconds per GiB, whichever is longer, shows `Verifying <file> (<size>) on the Instance` with the time taken, prints one line when it asks again after a timeout or a dropped connection, and repeats the status line every minute when stderr is not a terminal (`8a78d5c`). Tests: `TestLongCompletionsSayWhatTheyWaitFor` (push and import against a slow fake Instance), `TestCompletionTimeoutGrowsWithTheFile`, `TestResponseTimeoutIsPerRequest`, `TestWaitStatusRepeatsInALog`. [Uploading](../guides/uploading.md) and the `hangar push` help describe it.

### Suite results on `fix/p3-5-lead`

Run on 2026-10-18 at `21bcaaf` (before this report change), Go 1.27.2 and Node 24.19.0, on a shared machine.

| Suite | Command | Result |
|---|---|---|
| Lint | `make lint` | Exit 0: golangci-lint, `go vet` for the compat tag sets, Biome 219 files, `tsc -b`, `lint-style` |
| Unit and integration | `make test` (SQLite, Postgres 18 and MinIO) | Exit 0: 78 Go packages ok, 0 failed; Vitest 113 of 113; load test helpers 3 of 3 |
| Coverage | `make cover` | Exit 0: every package at or above its floor |
| Generated files | `make check-generate` | Exit 0, no differences |
| UI against the mock | `pnpm exec playwright test` (no retries) | Exit 0: 536 passed, 43 skipped by design. Axe in light and dark on every page at each width |
| UI against a real Instance | `pnpm exec playwright test -c playwright.real.config.ts` against two `make build` binaries | Exit 0: 369 passed. No `WARN` or `ERROR` line in either server's log |
| Helm on kind | `make test-kind IMAGE=hangar-fix-p3-5:test` | Exit 0: 9 PASS, 0 FAIL, including `TestKindBlobStoreStartsLate` and `TestKindGitOpsInstall` |
| Viewer scan | `go test -race -count=200 -tags integration -run '^(TestScanLimitsMakeResultsPartial\|TestScanTimeLimitMakesResultsPartial)$' ./internal/datasets/viewer` | Exit 0: 200 of 200 each |

## Preview feedback round (2026-10-17)

James deployed image `preview-5975bec` on bellatrix (Raspberry Pis, MinIO on a NAS behind Nginx Proxy Manager) on 2026-10-16 and reported 13 findings. This round fixed them on `integration/v1` from `c79363e`. The suites first ran in round 6 at `3a16ea0`. The results in [Suite results](#suite-results-at-9f08db7) come from the verification round, which ran on detached worktrees at `9f08db7` (`merge: fix/pf8-lead`), the branch head when it started. The verification round found a migration race in the chart, and round 9 fixed it and merged as `cd42d2b`, the current head. Round 9 has targeted checks, and [Final check at `cd42d2b`](#final-check-at-cd42d2b) ran every suite again at that commit. `main` was fast-forwarded to this branch after it, and nothing was pushed or published.

`a73486e` reverted `d1141f6`, a script that ran Go packages again when files vanished from the Go build cache mid-run. The files vanished because the orchestrator's disk trimmer deleted them while tests were building; the trimmer now removes only cache entries untouched for 3 hours, so the script was treating a symptom. `make test`, `make test-compat` and `scripts/coverage.sh` call `go test` directly again.

### What changed for each finding

"Already fixed" means a fix from fix rounds 5 to 8, before this round.

| Finding | What changed | Commits | Regression tests |
|---|---|---|---|
| 1. `huggingface_hub` single-PUT LFS uploads of 10 to 64 MiB fail on MinIO without Xet | `PresignPut` signs no headers and writes to a staging key; `Verify` hashes the staged object and copies it into place server-side, so a client still cannot write unverified bytes to a shared key. The fake Blob store now refuses unsigned or extra `x-amz-` headers as MinIO and AWS do | `6707273` | `TestPresignedPutWorksWithoutHeaders`, `TestPresignedPutCannotReplaceAVerifiedObject`, `TestPresignedPutVerifyRejectsWrongContent`, `TestFakeEnforcesSignedHeaderRules`, `compat/hf` `TestHubBasicUploadToMinIOWithoutXet` (20 MiB, Xet off, real MinIO). Recorded in the Hub compatibility matrix (`4299915`) |
| 2. The migration hook deadlocks the first Argo CD sync | `migrations.mode=initContainer` runs migrations in an init container instead of a Helm hook. `docs/get-started/helm.md` has a "Run with Argo CD" section. The verification round found that init containers starting together crashed instead of waiting; round 9 makes them wait on a Postgres advisory lock and for the database, and GitOps renders skip the `helm test` Pod | `29d2073`, `b6a8e5f`, `d91688f`; round 9: `026bd0a`, `9b1ca9b`, `d237fa9`, `0f6e751` | `TestChartMigrationModes`, `TestKindGitOpsInstall`, `TestConcurrentPostgresMigratesWaitForOneAnother`, `TestMigrateIgnoresALockLeftByAKilledMigrator`, `TestPostgresMigrateWaitsForTheLockHolder`, `TestMigrateWaitsForTheDatabase`, `TestHelmTemplateCommandsSkipTests` |
| 3. `generateSecrets` regenerates secrets when `lookup` returns nothing | The chart refuses to generate secrets when it cannot reach the cluster, and `existingSecret` is documented for GitOps | `29d2073`, `b6a8e5f` | `TestChartGenerateSecretsNeedsCluster`, `TestKindGeneratedSecretsSurviveRenders` |
| 4. The secret checksum ignores `existingSecret`; the cache volume equals the pack cache | The checksum annotation follows `existingSecret`, and the cache volume is sized above `git.cache_size` | `29d2073` | `TestChartSecretChecksumFollowsExistingSecret`, `TestChartCacheVolumeOutgrowsGitCache` |
| 5. The storage docs say `us-east-1` for MinIO | `docs/operate/storage.md` says to use MinIO's configured region | `d91688f` | `TestStorageDocsMinIORegion` |
| 6. Log noise: health probes, `create_repo(exist_ok=True)`, the landing page's `/me` 401 | Passing health probes log at `DEBUG`; creating an existing Repo uses `ON CONFLICT DO NOTHING`; `GET /me` answers 204 when nobody is signed in. After the audit, the signed-out page tests fail on any failed request or `Failed to load resource` error | `80fc37e`, `c76ce22` | `TestAccessLogPutsPassingHealthProbesAtDebug`, `TestCreateExistingRepoCausesNoDatabaseError` (both drivers), `TestSessionsTokensAndKeys`, `ui/mock/mock.test.ts`, the signed-out tests in `ui/e2e/pages.spec.ts` and `ui/e2e-real/pages.spec.ts` |
| 7. A write Token cannot delete a Repo through the Hub API | A write Token can administer the Repos its user administers, as on the Hub. Decision recorded in [decisions](decisions.md) | `54e5307` | `TestPermissionMatrix` (member admin with a write Token), `compat/hf` `test_write_token_administers_own_repos` |
| 8. One failed part PUT aborts a whole import | Part uploads and downloads retry up to six times on resets, timeouts and 408, 425, 429 and 5xx answers, with jittered backoff from 1 to 30 seconds or `Retry-After`. An expired part URL (403) plans the file again with new URLs. Downloads resume with `Range`. Progress counts resent bytes once. Other files carry on when one fails | `66a9488`, `1d0e599`, `eecc324` | `TestPushRetriesFailedParts`, `TestPushDoesNotRetryAPartTheInstanceRefuses`, `TestPushRenewsExpiredPartURLs`, `TestPullRetriesABrokenDownload`, `TestTransferBackoffGrowsAndIsCapped`, `internal/cli/upload_continue_test.go` |
| 9. A client-cancelled sign-in is logged as a 500 `ERROR` | Already fixed for authentication and git reads (`6caf784`, `56fd278`). This round extended 499 to every error writer, including the Dataset Viewer and Ollama | `6c93d86`, `9d8d29d` | `TestCancelledLoginIsNotAServerError`, `clientgone_internal_test.go` in `api`, `auth`, `hfapi`, `gitserver`, `ollama` and `datasets/viewer` |
| 10. `hangar import` looks frozen while it waits; Discover takes 40 s | The import prints what it is waiting for per Repo, with elapsed time. A Discover detail fetches the card, dependencies and Viewer answers in parallel instead of one after another | `66a9488`, `8dc2c71`, `d38afa5` | `TestImportSaysWhatItIsWaitingFor`, `TestWaitStatusOnATerminal`, `TestDiscoverDetailFetchesFromTheUpstreamAtOnce` |
| 11 and 12. Product decisions: release on hold, Taxiway teal, separate Models and Datasets, equal-height cards | Models and Datasets are separate top-level sections with their own lists, filters and New buttons; `/arsenal` redirects and keeps its filters. A model has a "Datasets used" picker that writes `datasets:` to its card, and a dataset lists "Used by". Datasets of any file types explain themselves when the Viewer cannot read them. Taxiway teal tokens in both themes. Cards in a row have equal height with stats at the bottom and quant chips capped with "+N". Spec 09, `docs/guides/datasets.md`, the search palette, the README screenshot and spec 15's Phase 4 hold note are updated, and [decisions](decisions.md) records every decision | `6fd838a`, `2c15e82` | `ui/e2e/sections.spec.ts` (4 tests), the Taxiway teal, equal-height card, main navigation and dark-theme axe tests in `ui/e2e/pages.spec.ts` |
| 13. Completion re-streams the whole object through the server, and nginx cuts reads at 1 GiB | (a) Already fixed: the server resumes a broken read with ranged GETs and keeps the upload and its parts (`fix/r4-w2`), and the CLI sends a completion again after 503 (`071ee7d`). (b) Completion hashes the assembled object and copies it into place server-side, in 512 MiB `UploadPartCopy` requests above the copy part size, pinned to the source's ETag. (c) A completion carries on after its caller leaves, and a repeated completion joins the one in progress. (d) `docs/operate/proxies.md` gives the nginx settings for MinIO, and `hangar admin doctor --large-read` finds a proxy that cuts long reads | `6707273`, `7972a2c`, `a3e3502`, `6e470e6`, `87f07c3`, `d91688f`, `dbbe9fb` | `TestCompleteUploadResumesABrokenRead`, `TestResumingReader*`, `TestCompleteUploadKeepsPartsWhenTheReadFails`, `TestCompleteUploadCopiesServerSide`, `TestCopyMultiGigabyteObjects` (2 GiB+, 5 GiB, 6 GiB), `TestCopyRefusesASourceThatChanges`, `TestCompleteUploadCarriesOnWhenTheCallerGoesAway`, `TestCompleteUploadFinishesAfterTheCallerLeft`, `TestPushFinishesACompletionAProxyCutOff`, `internal/repo/lfs_large_internal_test.go`, `TestProxyDocsNginxStreamsLargeFiles`, `internal/cli/doctor_test.go` |

The round also fixed two flaky tests the runs exposed: `TestPushRefusesLargeNonLFSBlob` (`b0ddde7`, a go-git pack index quirk that dropped the file name about once in a few hundred runs) and `TestCompleteUploadCarriesOnWhenTheCallerGoesAway` (`b27da7d`, the test opened its gate before the second completion had joined the first).

### Suite results at `9f08db7`

The verification round, on detached worktrees at `9f08db7`, Go 1.27.2, Node 24.19.0, Python 3.11.7 and `huggingface_hub` 2.2.0. Load average was between 50 and 90, with up to four runners' Go suites and other projects on the machine at once.

| Suite | Command | Result |
|---|---|---|
| Lint | `make lint` | Exit 0: golangci-lint, `go vet` for the compat tags, Biome, `tsc`, `lint-style` (spelling, test timeouts, compat matrix) |
| Vulnerabilities | `go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...` | 0 vulnerabilities reached; 3 in required modules, none called |
| Workflows | `actionlint .github/workflows/*` | Exit 0, no findings |
| Unit and integration | `make test` (`-race -count=1 -timeout 30m -tags integration`, SQLite, Postgres 18 and MinIO) | **Not finished.** The runner stopped it after about 6 minutes, before any package reported, while three other runners' Go suites shared the CPU. `make cover` and `make check-generate` did not start. See [Still open](#still-open) |
| git and Hub clients in Go | `go test -count=1 -timeout 30m -tags compat ./compat/harness ./compat/hf ./compat/git` with `HANGAR_TEST_POSTGRES_DSN` and `HANGAR_TEST_S3_*` set | ok. `TestHubBasicUploadToMinIOWithoutXet` passed in 4.5 s (20 MiB, Xet off, presigned MinIO URL). `compat/git` 814 s: 9 of 9 tests on the fake, real SQLite and real Postgres backends, including `TestLFSMultipartTransfer` and `TestLFSPushCloneAndPull` in both URL modes. `go vet -tags compat,xetserver` and `compat,xetserver,xetreal` on `./compat/xet`: clean |
| Hub clients against the binary | `HANGAR_HF_INSTANCE=binary uv run --frozen --project compat pytest compat/hf` | 42 passed, 2 skipped in each of four cells: SQLite and Postgres, each with Xet uploads off and on (`HANGAR_HF_XET_UPLOADS=1`). The skips are the `diffusers` and `upload_large_folder` tests, which run in other matrix cells. `make test-compat`'s pytest step against the Go test server: 47 passed, 2 skipped |
| Xet | `uv run ... pytest compat/xet` (README command) | 16 passed in each of four runs: `xetreal` on SQLite and on Postgres with `hf_xet` 1.7.0, the fake backend with 1.7.0, and `xetreal` on SQLite with 1.6.0, the oldest accepted |
| Ollama, Upstreams, Dataset Viewer, LM Studio | `HANGAR_TEST_POSTGRES=1 scripts/with-test-postgres.sh go test -count=1 -timeout 45m -tags compat -v ./compat/ollama/ ./compat/upstream/ ./compat/viewer/ ./compat/lmstudio/` | Exit 0. 23 top-level tests, 0 failed, 0 skipped. Ollama 0.31.1: 7 of 7 scenarios on in-process, binary SQLite and binary Postgres. Upstreams: real-pieces and live huggingface.co tests on both drivers. Viewer: 7 of 7 datasets match the Hub on SQLite and Postgres; pytest `compat/viewer` 6 of 6. LM Studio against the fake Instance |
| UI against the mock | `pnpm exec playwright test` at 390, 1024 and 1440 px | Light: 356 passed, 43 skipped by design (30 screenshot tests that need `SCREENSHOTS=1`, desktop-only budget and lint tests, the phone card-height test). Dark: 356 passed, 43 skipped. `--repeat-each 5` on the new section, teal, card, navigation and palette tests: 115 passed, 0 flaky |
| UI against a real Instance | `pnpm exec playwright test -c playwright.real.config.ts` against two `make build` binaries | Light: 210 passed, 0 skipped. Dark: 210 passed. 0 `ERROR` lines in either server's log |
| Compose | `docker compose up -d --wait`, `scripts/smoke.sh`, and a `huggingface_hub` check with `HF_HUB_DISABLE_XET=1` (`create_repo` twice with `exist_ok`, a 20 MiB LFS upload and download with matching SHA-256, `delete_repo` with a write Token) | Passed on `quickstart` (also with a generated admin password), `postgres-minio`, `scale` and `oidc-keycloak` (OIDC sign-in with admin and Organisation mapping). No Hangar `ERROR` or `WARN`, no probe access lines and no Postgres duplicate-key error |
| Deploy, static | `helm lint` (default, `ci/default-values`, scale, GitOps `--strict`); kubeconform 0.7.0 for Kubernetes 1.33; `go test ./deploy`; `go test -tags helm -run ^TestChart ./deploy` | All passed. `TestChart*`: 7 of 7 |
| Helm on kind | `scripts/kind-chart-test.sh`; `helm install --wait` with the chart defaults and with `examples/gitops-values.yaml`, smoke, `helm test` | Chart defaults and GitOps installs passed smoke, LFS and `helm test`. **Red:** `TestKindGitOpsInstall` passed but took 351 s, because migrate init containers that started together crashed and backed off; a GitOps render applied with `kubectl` left the `helm test` Pod in `Error`. Both fixed in round 9 |
| Audit | Every regression test for findings 1 to 13 with `-race -count=3`; `go test -race -tags integration` over 26 changed packages on Postgres 18 and MinIO; Vitest; Playwright on the changed specs; the Hub matrix; house style; a diff review of tests since `c79363e` | 40 finding tests, 3 times each: all passed, none flaky. 26 of 26 packages ok. Vitest 107 of 107. Playwright 356 passed. 85 of 85 Hub endpoint and test pairs listed; `compat/hf` in-process 42 passed, 2 skipped. House style clean. No test skipped, weakened or deleted; every removed assertion was replaced or moved |

Apart from the chart race, no test failed and none was flaky. No suite in this round stopped with `[build failed]` from a vanished cache file. Two slow runs came from host load, not from the code: the first binary SQLite Hub cell took 698 s beside `compat/git` (35 s on a quieter host), and `TestCroissantValidates/instance-postgres` took 534 s against 87 s on SQLite.

These results replace round 6 at `3a16ea0`, where 33 Go packages passed only on a rerun after the cache problem above and the audit was red for two findings that were fixed before the verification round.

### After the verification round

Round 9 merged as `cd42d2b` after the verification round, so no suite at `9f08db7` covers it.

| Commit | What it changed | Checked |
|---|---|---|
| `026bd0a` | Migrate init containers that start together crashed instead of waiting. bun's `Init` raced on Postgres (SQLSTATE 23505 on `pg_type_typname_nsp_index`) and its lock row failed at once, and a migrator killed before `Unlock` left the row behind for good. `Migrate` now holds a Postgres advisory lock, polled on a connection outside the pool, and no longer takes bun's lock | `TestConcurrentPostgresMigratesWaitForOneAnother` (8 migrators on an empty database), `TestMigrateIgnoresALockLeftByAKilledMigrator` (both drivers) and `TestPostgresMigrateWaitsForTheLockHolder`; the first two failed before the fix with the errors seen on kind |
| `9b1ca9b` | A GitOps render applied with `kubectl apply` ran the `helm test` Pod before Hangar was up, and it stayed in `Error`. The GitOps docs and example values say to render with `--skip-tests` (Argo CD already skips Helm test hooks). `TestKindGitOpsInstall` renders with `--skip-tests`, runs three `web` and three `worker` Pods, and fails on a failed Pod or a migration collision in the Postgres log | `TestHelmTemplateCommandsSkipTests` (failed on the old docs); `scripts/kind-chart-test.sh` passed |
| `d237fa9` | `hangar admin migrate --wait-for-database` retries a Postgres that does not answer yet; the chart passes `migrations.waitForDatabase` (default `5m`) in `initContainer` mode, so the init container no longer exits into the kubelet's growing restart back-off | `TestMigrateWaitsForTheDatabase`, `TestMigrateGivesUpWaitingForTheDatabase`, `TestMigrateFailsAtOnceWithoutWaitForDatabase`, `TestChartMigrationModes` |
| `0f6e751` | `scripts/kind-chart-test.sh` loads partly local multi-platform images (Postgres and MinIO under Docker's containerd image store) from a `docker save --platform` archive, so the node no longer pulls them | `scripts/kind-chart-test.sh`: all three `TestKind` tests passed; `TestKindGitOpsInstall` went from 351 s to 22 s |

Earlier fixes in this round that the verification round covers: `b27da7d` (the flaky `TestCompleteUploadCarriesOnWhenTheCallerGoesAway`), `c76ce22` (the signed-out page tests fail on any failed request, so the `/me` 401 cannot come back unnoticed), `a73486e` (the revert above; the lead's `make lint test` at `8daab65`, which has the same code as `9f08db7`, exited 0 with 77 Go packages ok and Vitest 107 of 107) and `8daab65` (the Compose and CI Postgres health checks use TCP, so `pg_isready` no longer passes while the image's init server is still running; `TestPostgresHealthchecksUseTCP`, `TestPgIsReadyTCPCheck`, and `scale` first start 10 of 10).

### Final check at `cd42d2b`

The lead ran every suite again at `cd42d2b`, the round 9 merge, on one detached worktree with no other test runners on the machine, Go 1.27.2 and Node 24.19.0. Commits after it change only this report.

| Suite | Command | Result |
|---|---|---|
| Lint | `make lint` | Exit 0 |
| Unit and integration | `make test` (`-race`, SQLite, Postgres 18 and MinIO) | Exit 0 in 6 min 27 s. Every Go package ok; Vitest 107 of 107 |
| Coverage | `make cover` | Exit 0: every package at or above its floor |
| Generated files | `make check-generate` | Exit 0 |
| Chart, static | `make lint-deploy` | Exit 0 |
| Image | `make image` | Built |
| Compose | `scripts/compose-smoke.sh <example> --skip pull` | Passed on `quickstart`, `postgres-minio`, `scale` and `oidc-keycloak` |
| Helm on kind | `make test-kind` | `TestKindGeneratedSecretsSurviveRenders`, `TestKindNotesReadOnlyRenderedSecretKeys` and `TestKindGitOpsInstall` passed. `TestKindGitOpsInstall` took 44 s, against 351 s before round 9 |
| UI against the mock | `pnpm exec playwright test` | 356 passed, 43 skipped by design |
| UI against a real Instance | `pnpm exec playwright test -c playwright.real.config.ts` against two `make build` binaries | 210 passed |
| Client compatibility | `make test-compat` | `compat/git`, `harness`, `hf`, `lmstudio`, `ollama`, `upstream` and `viewer` ok; pytest 69 passed, 2 skipped. `TestHubBasicUploadToMinIOWithoutXet` passed in 8.5 s against real MinIO |

The first attempt at the unit run failed in `ui-deps` because the shell had Node 20, which lacks `node:sqlite` for pnpm 11; the first image build failed on a DNS lookup of `apk.cgr.dev`. Both were environment problems, and both passed on the rerun shown here.

### Still open

As of `cd42d2b`. Phase 3 closed the first three items and the release rehearsal; [Still open after Phase 3](#still-open-after-phase-3) has what remains.

- **Hangar exits when the Blob store does not answer at start.** Closed in Phase 3 by `storage.s3.startup_wait` (`22eded4`). In one round 9 kind run, MinIO took about 5 minutes to pull; with migrations no longer holding the Pods back, the `hangar` containers started first, exited with `reach bucket "hangar": ... connection refused`, and went into the kubelet's restart back-off past the 6-minute rollout wait. `0f6e751` removed the delay from the test, but a GitOps install whose object store starts late still waits on that back-off. Waiting for the Blob store at start, as `--wait-for-database` does for migrations, is not done.
- **`scripts/kind-chart-test.sh` skips a Hangar image that is not local.** It loads only images it finds, so a missing `HANGAR_KIND_IMAGE` makes each `TestKind` test wait 5 to 6 minutes and fail with `ErrImageNeverPull` instead of failing at once. The audit hit this when another runner deleted the image it was using. Closed in Phase 3 (`cebfa8f`).
- **Datasets without data files log a `WARN`.** `internal/datasets/pipeline/convert.go:211` logs "the dataset has no Configs" for every dataset with no Viewer files (29 lines in one `e2e-real` run). Since datasets may hold any file types (decision 11a), this is a normal state; the Viewer already logs it at `DEBUG`. Low priority, in the spirit of finding 6. Closed in Phase 3: it logs at `DEBUG` (`f210b05`).
- **Rerun the import on the preview.** James approved the lead running it on 2026-10-17. Nothing here has moved a multi-GB file through Nginx Proxy Manager, and no Compose or kind setup exercises presigned single-PUT uploads (only `compat/hf` against MinIO does). The lead rebuilds the preview and the CLI from `integration/v1`, runs `hangar admin doctor --large-read` against Aquila's MinIO, and reruns the `Qwen2.5-Coder-32B-Instruct-GGUF` import. This closes the red item in [Open issues](#open-issues).
- **Local `govulncheck`.** `~/go/bin/govulncheck` was built with Go 1.26 and cannot load this module; rebuild it with Go 1.27. `go run ...@v1.9.0` works.
- **Not run this round.** The floor-client, latest-Python and without-`hf_xet` Hub cells, the `diffusers` and `upload_large_folder` cells, the 6 GiB nightly file, fuzz, LM Studio against a real server, and `make release-rehearsal` (which the Phase 3 gaps review ran and passed).
- **Release on hold.** Lifted on 2026-10-18; see [Releases](#releases-2026-10-18-and-2026-10-19).

## Suite results

All at `195e3a2`, round 8. Load average on the machine was between 45 and 138 during the runs, with other projects running.

| Suite | Command | Result |
|---|---|---|
| Lint | `make lint` | Exit 0. golangci-lint 0 issues; `go vet` for the `xetserver`, `xetreal` and `w2real` tags clean; Biome checked 207 files; `tsc -b` clean; `lint-style` (em dashes, spelling) clean |
| Unit and integration | `make test` (`scripts/with-test-postgres.sh go test -race -count=1 -tags integration ./...` with Postgres 18 and MinIO) | Exit 0. 77 packages ok, 0 failed. Vitest 20 files, 88 of 88 tests |
| Coverage | `make cover` | Exit 0. 74 packages at or above their floors. Strict packages: `auth` 86.2%, `gitserver` 89.9%, `hfapi` 89.0%, `repo` 85.8% (floor 85%). Lowest: `datasets/media` 77.2% (floor 75%) |
| Generated files | `make check-generate` | ok, no diff |
| Vulnerabilities | `go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...` | 0 vulnerabilities reached; 3 in required modules, none called |
| Fuzz | every `Fuzz*` target in `internal/` for 30 s, flags as `scripts/fuzz.sh` | 34 of 34 passed |
| Workflows | `actionlint` over `.github/workflows` | No findings |
| git clients | `HANGAR_TEST_POSTGRES=1 scripts/with-test-postgres.sh go test -count=1 -race -tags compat ./compat/git ./compat/hf` | ok (581 s) on the fake, real SQLite and real Postgres backends |
| Hub clients against the binary | `HANGAR_HF_INSTANCE=binary uv run --project compat pytest compat/hf` | `huggingface_hub` 2.2.0 and `hf_xet` 1.7.0 on Python 3.11: 41 passed, 2 skipped on SQLite and on Postgres, each with Xet uploads off and on (`HANGAR_HF_XET_UPLOADS=1`). Python 3.14.7: 41 passed, 2 skipped. Floor client 1.31.0 on Python 3.10: 43 passed, 1 skipped, Xet off on SQLite and Xet on on Postgres. Without `hf_xet` on Postgres: 41 passed, 2 skipped. 200 MB multipart file (`HANGAR_HF_MULTIPART_FILE_SIZE=200000000`, `test_upload.py`, Xet on): 9 passed |
| Xet | `HANGAR_XET_GO_TAGS=xetreal` pytest `compat/xet`, and `go test -race -tags compat,xetserver,xetreal ./compat/xet` | `hf_xet` 1.6.0 and 1.7.0 on SQLite and Postgres: 16 passed each. Go: ok |
| Ollama | `HANGAR_BINARY=... HANGAR_TEST_POSTGRES=1 go test -count=1 -tags compat ./compat/ollama/` with Ollama 0.31.1 | ok (69 s). 7 of 7 scenarios on in-process, binary SQLite and binary Postgres |
| LM Studio | `go test -count=1 -tags compat ./compat/lmstudio/` | ok |
| Dataset Viewer | `go test -count=1 -tags compat ./compat/viewer/...` and `uv run --project compat pytest compat/viewer` | ok (398 s): Croissant validates, 70 of 70 layouts match `datasets`, 7 of 7 datasets match the Hub, on fakes, SQLite and Postgres. pytest 6 passed |
| Upstreams | `go test -count=1 -tags compat -timeout 30m ./compat/upstream/` | ok (228 s). Real-pieces, huggingface.co and fake scenarios all passed, including the lost-lease, quota and resume-after-kill tests on both drivers |
| UI against a real Instance | `pnpm exec playwright test -c playwright.real.config.ts` against two `make build` binaries (one as the Upstream) | 14 flows and 51 page checks at 1440, 1024 and 390 px: 195 passed on fresh Instances, 195 on a rerun against the same Instances, 195 on recreated Instances. No flaky test in 585 runs. 0 `ERROR` lines in the server logs |
| Deploy, static | `go test -count=1 ./deploy/` | ok |
| Compose | `docker compose up -d --wait` and a smoke script (health, sign-in, Token, `huggingface_hub` 0.36.1 upload and download of a 12 MiB LFS file, SHA-256 compared) | Passed on `quickstart` (also with a generated admin password across a restart), `postgres-minio`, `oidc-keycloak` (OIDC sign-in with admin and Organisation mapping) and `scale` (two web and two worker replicas behind Traefik; no `Authorization`, `x-amz-signature` or `hangar_session` in the collector output) |
| Helm on kind | `helm lint --strict`, `helm install` with `ci/default-values.yaml`, smoke, `helm test`; `examples/scale-values.yaml` with Postgres 18 and MinIO, then `helm upgrade --reuse-values` | All passed. Migration hooks ran on install and upgrade |

Not run in round 8: the `diffusers` Hub cell and the 6 GiB sparse file (both nightly), the mock Playwright suite (`ui/e2e`, last run during the `5975bec` checks with 297 passed), and `make release-rehearsal`.

## Feature checklist

Every row passed in round 8 unless its result says otherwise. "unit" is `make test`; the other names are the suites above. "Mock" rows rest on the `ui/e2e` mock suite, which last passed at `6610f45` (356 passed). Phase 3 added tests for several rows, listed in its handoffs; META-2 names the Phase 3 gate tests.

| ID | Test that proves it | Latest result |
|---|---|---|
| REPO-1 | `internal/repo` lifecycle tests on both drivers; `compat/hf` `test_repos.py` | Passed: unit, Hub clients |
| REPO-2 | `internal/repo` list visibility tests; `internal/auth` permission tests; `compat/hf` `test_errors.py` | Passed: unit, Hub clients |
| REPO-3 | `internal/repo` `commit_test.go` (log, diff, `FileDiff`, `CompareFile`); `internal/api` `TestUploadCommitAndBrowseEndpoints` | Passed: unit |
| REPO-4 | `internal/repo/lifecycle_test.go`; `compat/hf` `move_repo` and redirect tests; `compat/git` | Passed: unit, Hub clients, git clients |
| REPO-5 | `internal/repo/lifecycle_test.go` (restore window, purge) | Passed: unit |
| REPO-6 | `internal/hfapi` discussion tests; `internal/api` `TestDiscussionEndpoints`, `TestPullRequestFileDiff`; `compat/hf` `test_discussions_and_pull_requests`, `test_edit_and_hide_comments`; real-Instance pull request diff flow | Passed: unit, Hub clients, real Instance |
| REPO-7 | `internal/hfapi` `TestLikersAndUserLikes`; `cmd/hangar` `TestHubLikersAndCommentChanges`; `compat/hf` `test_likers_and_liked_repos`, `test_unlike` | Passed: unit, Hub clients |
| REPO-8 | `internal/auth/access_requests`; `compat/hf` `test_gated_repo` | Passed: unit, Hub clients |
| REPO-9 | `internal/repo` settings tests; `compat/hf` `update_repo_settings` | Passed: unit, Hub clients |
| UP-1 | `internal/hfapi` write tests; `compat/hf` `test_upload.py` against the binary, including the 200 MB multipart file | Passed: unit, Hub clients |
| UP-2 | `compat/git` `TestCloneFetchPull`, `TestLFSPushCloneAndPull`, `TestLFSMultipartTransfer`; `internal/gitserver` | Passed: git clients, unit |
| UP-3 | Real-Instance browser upload flow; mock flows for parts and resume after reload | Passed: real Instance. Mock: passed at `6610f45` |
| UP-4 | `internal/cli` push tests, including `TestPushResendsCompletionAfter503WithRetryAfter` | Passed: unit |
| UP-5 | `internal/blob` `TestStreamsLargeObjects` on both backends and on AWS S3 (2026-10-15); `internal/hfapi` `TestResolveRedirectOver50GB`; nightly 6 GiB Hub cell | Passed: unit. Nightly cell not run in round 8 |
| UP-6 | `internal/repo/lfs_test.go`; `compat/hf` `test_upload_over_multipart_threshold` (the second upload sends nothing) | Passed: unit, Hub clients |
| UP-7 | `compat/xet` (`hf_xet` 1.6.0 and 1.7.0, real backend); `compat/hf` with `HANGAR_HF_XET_UPLOADS=1`; `internal/hfapi` `TestXetWriteToken` | Passed: Xet, Hub clients, unit |
| DOWN-1 | `internal/hfapi` resolve tests; `compat/hf` `test_download.py` | Passed: unit, Hub clients |
| DOWN-2 | `internal/blob/backends` `TestOpenS3`; `internal/hfapi` `TestResolveRedirectMode`; `cmd/hangar` `TestS3ProxyModesKeepTransfersOnHangar`; presigned GETs on AWS S3 | Passed: unit; AWS on 2026-10-15 |
| DOWN-3 | `compat/git`; `internal/gitserver` | Passed: git clients, unit |
| DOWN-4 | `compat/ollama` against the binary; `internal/ollama` | Passed: Ollama, unit |
| DOWN-5 | `internal/api` zip export test; `internal/repo` `TestZipExportJob` | Passed: unit |
| PULL-1 | `internal/upstream` registry tests; `compat/upstream` | Passed: unit, Upstreams |
| PULL-2 | `internal/upstream` plan and pack tests; `compat/upstream` `TestPullTinyRepoFromHuggingFace` | Passed: unit, Upstreams |
| PULL-3 | `internal/gitserver` `TestLFSPartialMirror`; `compat/upstream` `TestPartialMirrorOfOneQuantFromHuggingFace` | Passed: unit, Upstreams |
| PULL-4 | `internal/upstream` matcher tests; real-Instance Discover and Pull of one GGUF quant | Passed: unit, real Instance |
| PULL-5 | `internal/upstream` Pull Job tests; `internal/jobs`; `compat/upstream` `TestRealResumeAfterKillingTheWorkerProcess` | Passed: unit, Upstreams |
| PULL-6 | `internal/upstream` transfer tests | Passed: unit |
| PULL-7 | `internal/jobs/limiter_test.go`; `compat/upstream` `TestRealBandwidthCapAcrossTwoWorkers`, `TestRealPullWindowsAndPullNow` | Passed: unit, Upstreams |
| PULL-8 | `internal/upstream` credential tests | Passed: unit |
| PULL-9 | `internal/upstream` tracking tests; `compat/upstream` `TestRealPullAndTrackFromASecondInstance` | Passed: unit, Upstreams |
| PULL-10 | `internal/upstream` Pull-through tests; `cmd/hangar` `TestHubPullThroughAdapter`; `compat/upstream` `TestPullThroughFromHuggingFace`, `TestRealPullThroughWithHFHubDownloadAndGitLFS` | Passed: unit, Upstreams |
| PULL-11 | `internal/upstream` dependency tests; `cmd/hangar` `TestDatasetWiring` | Passed: unit |
| PULL-12 | `internal/cli` import tests, including `TestImportResendsCompletionAfter503WithRetryAfter`; `compat/upstream` `TestRealImportThroughTheNativeAPI` | Passed: unit, Upstreams. A 20 GB import against the preview failed; see [Open issues](#open-issues) |
| BROWSE-1 | `internal/repo` list and facet tests; mock Arsenal flows | Passed: unit. Mock: passed at `6610f45` |
| BROWSE-2 | `internal/upstream` discover tests; real-Instance Discover flow | Passed: unit, real Instance |
| BROWSE-3 | Real-Instance browsing flow and 51 page checks at three widths | Passed: real Instance |
| BROWSE-4 | Real-Instance safetensors header and GGUF viewer flows; `internal/api` `TestFileScan` | Passed: real Instance, unit |
| BROWSE-5 | `internal/collections`; `compat/hf` `test_collections`; real-Instance Collection keyboard reorder flow | Passed: unit, Hub clients, real Instance |
| BROWSE-6 | Mock keyboard search flow | Mock: passed at `6610f45` |
| AUTH-1 | `internal/auth`; `internal/api` `TestLoginThrottleMessage`; real-Instance sign-in flow | Passed: unit, real Instance |
| AUTH-2 | `internal/auth` OIDC tests; Compose `oidc-keycloak` smoke | Passed: unit, Compose |
| AUTH-3 | `internal/auth/login` tests | Passed: unit |
| AUTH-4 | `internal/auth` Token tests; real-Instance Token flow | Passed: unit, real Instance |
| AUTH-5 | `internal/org` on both drivers; `internal/api` `TestAdminEndpoints`; real-Instance Organisations flow | Passed: unit, real Instance |
| AUTH-6 | `internal/auth/bootstrap`; `cmd/hangar` admin command tests; Compose `quickstart` with a generated admin password | Passed: unit, Compose |
| AUTH-7 | `internal/auth` Ollama key tests; `compat/ollama` `private_Repo_with_a_registered_key` | Passed: unit, Ollama |
| META-1 | `internal/index` extract and fuzz tests | Passed: unit, fuzz |
| META-2 | `internal/scan`; `cmd/hangar` `TestUnsafeDownloadGateCoversEveryRoute`, `TestServingSurfaceIsClassified`, `TestFileByteReadersAreClassified`, `TestUnsafeDownloadGateCatchesScannerBypasses`; real-Instance unsafe pickle flow | Passed: unit, fuzz, real Instance. Phase 3 gate tests passed 3 of 3 in the round 4 audit |
| META-3 | `internal/scan` integrity tests | Passed: unit |
| DATA-1 | As REPO-1 to REPO-9 for dataset Repos | Passed: unit, Hub clients |
| DATA-2 | `compat/viewer` `TestLayoutMatchesDatasetsLibrary` (70 layouts against `datasets` 5.1.0) | Passed: Dataset Viewer |
| DATA-3 | `internal/datasets/pipeline` | Passed: unit |
| DATA-4 | `internal/datasets/viewer`; `compat/viewer` `TestViewerMatchesHub` | Passed: unit, Dataset Viewer |
| DATA-5 | `internal/datasets/stats` (32 oracle cases against the Hub's statistics code) | Passed: unit |
| DATA-6 | Real-Instance Dataset Viewer paging and search flow | Passed: real Instance |
| DATA-7 | `internal/upstream` dataset Discover tests | Passed: unit |
| DATA-8 | `internal/upstream` dataset selection tests | Passed: unit |
| DATA-9 | `internal/index/indexdb` `TestDatasetSplitSizesFollowTheParquetExport`, `TestModelCardDatasetsAreIndexed`; `internal/datasets/pipeline` `TestAfterConvertRunsOncePerExport` | Passed: unit |
| DATA-10 | `internal/datasets/croissant`; `compat/viewer` `TestCroissantValidates` | Passed: unit, Dataset Viewer |
| DATA-11 | `internal/repo` list filters on the dataset summary | Passed: unit |
| DATA-12 | `internal/cli` dataset tests | Passed: unit |
| DATA-13 | `internal/datasets/card`; real-Instance dataset card editor flow | Passed: unit, real Instance |
| EVT-1 | `internal/webhook` | Passed: unit |
| EVT-2 | `internal/audit`; `internal/api` audit tests; real-Instance audit page | Passed: unit, real Instance |
| OPS-1 | `internal/blob` conformance on the filesystem, MinIO and AWS S3 (2026-10-15) | Passed: unit; AWS on 2026-10-15 |
| OPS-2 | Every `dbtest.ForEachDriver` test on SQLite and Postgres | Passed: unit |
| OPS-3 | Compose `scale` and Helm `scale-values.yaml` (two web and two worker processes) | Passed: Compose, Helm |
| OPS-4 | `internal/gc`; `cmd/hangar` `TestQuotaAdapters`, `TestAdminGC`; `compat/upstream` `TestRealPullRespectsNamespaceQuota` | Passed: unit, Upstreams |
| OPS-5 | `internal/telemetry`; `internal/httpx` readiness tests; Compose `scale` collector output | Passed: unit, Compose |
| OPS-6 | `internal/config`; `make check-generate` | Passed: unit |
| OPS-7 | Image built from `195e3a2`; Compose and Helm smoke; `actionlint` | Passed: deploy. Signing and multi-arch publishing first run at release (James) |
| OPS-8 | `internal/cli` backup and restore tests | Passed: unit |
| OPS-9 | Real-Instance Instance settings, users, Upstreams, storage and audit flows | Passed: real Instance |

## Hub API contract MUSTs

The 24 MUST items in section 13 of [`hf-hub-api-contract.md`](../research/hf-hub-api-contract.md). Handlers are in `internal/hfapi` unless named otherwise. Every row passed in round 8 (unit and Hub client suites).

| MUST | Handler | Test | Result |
|---|---|---|---|
| Model and dataset routing on the escaped path, `%2F` decoded in revision and path | `path.go`, `routes.go` | `TestRouting`, `TestResolveRevisions`; `test_tree_and_paths_info` | Passed |
| `HEAD` and `GET /resolve` headers: `X-Repo-Commit`, `ETag` and `X-Linked-Etag`, sizes, no `Content-Encoding` | `resolve.go` | `TestResolveRegularFile`, `TestResolveLFSFile`; `test_hf_hub_download_by_revision` | Passed |
| Off-origin presigned redirects with the metadata on the 3xx | `resolve.go` | `TestResolveRedirectMode`, `TestResolveRedirectOver50GB` | Passed |
| `Range` gives 206 with a numeric `Content-Range`; 416 past the end | `resolve.go` | `TestResolveRanges`; `test_ranges_and_safetensors_metadata`, `test_resume_partial_download` | Passed |
| Repo info shape (`id`, `sha`, `siblings`, `lfs`, `Z` dates; no `xetHash`, `transformersInfo`, `inferenceProviderMapping` or `evalResults`) | `info.go` | `TestRepoInfo`, `TestDatasetInfoAndEmptyMirror`; `test_model_info_shape` | Passed |
| Tree: recursive, directories included, absolute `Link rel="next"`, 404 `EntryNotFound` | `tree.go` | `TestTree`, `TestTreePagination` | Passed |
| `paths-info` (form and JSON), existing entries only | `tree.go` | `TestPathsInfo` | Passed |
| Error headers `X-Error-Code` and `X-Error-Message`, JSON body, `X-Repo-Commit` on `EntryNotFound` | `internal/httpx/errors.go`, `hfapi.go` | `TestResolveErrors`, `TestServiceErrorMapping`; `test_not_found_errors`, `test_gated_repo` | Passed |
| Bearer auth, anonymous reads of public Repos, `Invalid credentials` 401 | `internal/httpx` (`MsgInvalidCredentials`) | `TestResolveErrors`; `test_bad_token` | Passed |
| Never send Xet download headers or `xetHash` | `resolve.go`, `info.go` | `TestResolveLFSFile`, `read_test.go` assertions; `test_download_with_hf_xet_installed` | Passed |
| Case-insensitive booleans; `expand` and `expand[]` | `internal/httpx` (`QueryBool`, `QueryList`) | `TestQueryHelpers` | Passed |
| UTC datetimes with milliseconds and `Z` | `internal/model` (`HubTimeLayout`) | `read_test.go` | Passed |
| Immutable Commits | `internal/repo` | `TestCommitsAreByteIdenticalToGit` | Passed |
| Complete recursive `siblings` | `info.go` | `test_model_info_shape`; `test_diffusers_pipeline` (nightly, not run in round 8) | Passed |
| Discussions guard against safetensors auto-conversion | `discussions.go` (superseded by REPO-6) | `TestSafetensorsConversionGuard`; `test_pickle_only_model_never_contacts_conversion_space` | Passed |
| `datasets-server.<host>` answers fast | `internal/httpx` (`Classify`), Dataset Viewer | `TestClassify`; `compat/viewer` | Passed |
| `whoami-v2` | `identity.go` | `TestWhoami`; `test_whoami`, `test_hf_auth_login_and_whoami` | Passed |
| `repos/create` with `url` in the 409 body | `manage.go` | `TestCreateRepo`; `test_create_repo_variants` | Passed |
| `validate-yaml` | `yaml.go` | `TestValidateYAML` | Passed |
| `preupload` with the server-side LFS policy and `oid` | `preupload.go` | `TestPreupload`, `TestPreuploadRejectMode` | Passed |
| LFS batch, basic and multipart, with deduplication and SHA-256 checks | `internal/gitserver/lfs.go` | `TestLFSBasicUploadDownloadAndPush`, `TestLFSMultipartThroughHangar`; `test_upload_file_and_folder`, `test_upload_over_multipart_threshold` | Passed |
| Commit NDJSON, `parentCommit`, `create_pr` | `commit.go`, `prcommit.go` | `TestCommit`, `TestCreatePR`; `test_create_commit_adds_deletes_copies` | Passed |
| `RevisionNotFound` for a missing branch; branch 409 | `refs.go`, `info.go` | `TestBranchesAndTags`; `test_exists_checks` | Passed |
| Xet policy | `xet.go`, `routes.go` (superseded by UP-7: uploads accepted, `xet-read-token` answers 404 as `03-hf-api.md` decides) | `TestXetWriteToken`; `compat/xet` | Passed |

Unimplemented MUSTs: none.

## Request resolutions

Every item in `requests/w1.md` to `requests/w11.md` has a row in [`RESOLUTIONS.md`](requests/RESOLUTIONS.md): 108 items. 3 are deferred; the other 105 are applied on the branch or accepted as the workstream left them.

| Workstream | Items | Deferred |
|---|---|---|
| W1 Platform | 6 | 0 |
| W2 Repo core | 10 | 0 |
| W3 git server | 6 | 0 |
| W4 Hub API | 11 | 0 |
| W5 Identity and access | 10 | 0 |
| W6 Upstreams and Pulls | 13 | 0 |
| W7 Metadata, Ollama and community | 12 | 0 |
| W8 Web UI | 9 | 0 (request 8, self-service profile and password, closed on 2026-10-18) |
| W9 CLI, packaging and docs | 12 | 1: request 11, release tasks (James) |
| W10 Datasets | 10 | 0 |
| W11 Xet | 9 | 1: request 8, Commit-time ownership of LFS and Xet objects (phase 3 security review) |

The round 8 audit spot-checked the wiring items in `cmd/hangar` (`GCSchedule`, `ReindexStale`, `RescanStale`, `RecordQueueGauge`, `ConnectSrc`, `CleanupSchedule`, `NewLimiter`, `SyncConfig`, `AfterIndex`, `EnqueueConvert`, `OpenBlobStore`, `PullThroughPath`, `ExportPath`, `ExemptTokenEndpoint`, `gc.Register`, `InstanceLimits`, `ParseWindows`) and found all of them.

## W8 8 closed: self-service profile and password

James decided on 2026-10-18 to let users change their own profile and password before v1.0. Branch `feat/self-service-account` from `main` at `9815b88` adds `PATCH /me`, `POST /me/password`, a **Settings**, **Profile** page that edits both, and, after two security reviews the same day, email confirmation links: an email a user sets for themselves waits as pending until a single-use link that an admin creates, bound to the address the admin saw, is used, and only verified emails match OIDC sign-ins. The rules are in [decisions](decisions.md) under "Phase 3 decisions by James (2026-10-18), self-service accounts".

| Commit | What it does |
|---|---|
| `38cb14a` | `PATCH /me` and `POST /me/password`, `has_password` on `Me`, audit events, route matrix entries |
| `a0706a9` | The Profile page, mock handlers, mock and real Playwright flows |
| `1cb7d6f` | Spec 05, 09 and 10, threat model, decisions and user and admin docs |
| `f9f6421` | Self-set emails pending in `email_changes` (migration `0003`) until confirmed; only verified emails match OIDC first sign-in |
| `30d1998`, `ef94378` | The UI and docs for pending emails and the confirmation page |
| `b166893` | `TestCommands` expects schema `0003` |
| `c937c8e` | Confirmation links bound to the address the admin saw (409 otherwise), with the bound address in the response |

Tests: `TestChangePassword`, `TestUpdateProfile`, `TestSelfSetEmailsNeedConfirmation`, `TestRegistrationEmailsNeedConfirmation` (`internal/auth`); `TestUpdateMe`, `TestChangeMyPassword`, `TestChangeMyPasswordIsThrottled`, `TestEveryW5ActionIsAudited` (`internal/api`); `TestSelfServiceAccountOnTheFullServer`, `TestRouteAuthorisationMatrix` (`cmd/hangar`); the "account" flows in `ui/e2e/flows.spec.ts` and "changes the display name, email and password on the Profile page" in `ui/e2e-real/flows.spec.ts`, with axe in both themes.

Results on the branch at `c937c8e`, run one after another on the development machine: `make lint` exit 0; `make check-generate` exit 0; `make test` exit 0 (78 Go packages on SQLite and Postgres, UI 114 passed, load test helpers 3 passed); `make cover` exit 0 (`internal/auth` 86.0%, `internal/api` 83.3%, every package at its floor); `CI=1 make e2e` exit 0 (557 passed, 43 skipped); the real-Instance suite against two Instances started as `compat.yml` starts them exit 0 (378 passed).

The CLI has no account or profile command group and signs in with Tokens, which cannot change passwords, so there is no `hangar account password`.

## Shipped binary

`go list -deps ./cmd/hangar` gives 947 packages, and no Hangar `*test` or fake package is among them. None of the 284 shipped Go files contains a `TODO`, `FIXME` or stub. `gen.Unimplemented` is referenced only from `_test` files and `clitest`. The native server embeds all 16 handler types, so an operation without a handler fails to compile.

## AWS S3

James approved a test against AWS S3 with his `elastic-sa` profile in `eu-west-2`. Details are in the "AWS S3" section of [`s3-presigned-checksum.md`](../research/s3-presigned-checksum.md).

- **2026-10-14.** Presigned checksum PUTs, presigned parts and presigned GETs passed. Four tests failed, both causes in Hangar:
  - `TestCheck`: AWS answers `HEAD` in a missing bucket with a bare 404, so `Stat` reported a missing object instead of a missing bucket, and the start-up check could not say what was wrong.
  - `PutVerifiedMultipart`, `MultipartPartRetryWithNewContent` and `TestStreamsLargeObjects`: one dropped connection or a slow response on a part PUT failed the whole upload, because minio-go retries only seekable bodies and waits one minute for response headers.
- **Fixes (`8f8f3dc`).** `Stat` confirms the bucket with `HeadBucket` when an object is missing, and `Check`, which `backends.Open` runs at start-up, explains a missing bucket, a bucket in another region and a 403. Part and single PUTs retry (`storage.s3.retries`, 5 by default) and replay the same bytes, and a request that makes no progress for `storage.s3.stall_timeout` (2 minutes) is cancelled and retried.
- **2026-10-15 rerun.** The whole S3 suite passed on AWS twice, including `TestCheck`, `TestStreamsLargeObjects` and every presign subtest. Concurrent PUTs to one key got `503 SlowDown`, and the retries recovered them.
- **Round 8.** The fault-injection tests `TestPartUploadRetriesFaults`, `TestPutVerifiedMultipartSurvivesFaults`, `TestPartRetriesOnMinIO` and `TestCheckExplainsBucketProblems` passed in `make test`.
- **Not seen on AWS.** The busy-link conditions of 2026-10-14 did not recur on 2026-10-15, so on AWS the retries were seen working only for `SlowDown`. Dropped connections and stalls are covered by the fault-injection tests only.

## Merges since the previous report

The previous version of this report was written in `4462828`. These merges came after it, in this order.

| Merge | Branch | What it changed |
|---|---|---|
| `e8baccb` | `fix/r5-w2` | Git reads killed by a cancelled context report the cancellation instead of a 500 and an `ERROR` line (`56fd278`). Tests for failed reopens of a resuming read, large git blobs and the blob cache bring `internal/repo` back over its 85% floor (`32d9404`) |
| `245949d` | `fix/r5-w5` | A request the client closed during authentication gets 499 instead of an unlogged 500; other authentication store errors are logged at `ERROR` with the request ID (`6caf784`) |
| `ddffcf4` | `fix/r5-w9` | `hangar push` and `hangar import` send an upload completion again after 503 with `Retry-After`, up to 10 times (`071ee7d`) |
| `4f65c07` | `fix/r5-lead` | Request IDs in every 500 message and log line (`1c40413`); the shared test Postgres container and template databases; compat harness binaries removed after each run |
| `01cb31f` | `fix/r6-lead` | A readiness probe that times out no longer caches its own failure, and every failed check is logged with its name (`2ad5361`) |
| `122e4bc` | `fix/r6-w4` | A test for the over-50 GB redirect without `X-Linked-Size` (`7861cdf`) |
| `b70e861` | `fix/r6-w9` | `/readyz` answers within 10 seconds and every probe waits longer than that (`4b440b7`); release checksums are signed into cosign 3 bundles, and `make release-rehearsal` rehearses OPS-7 locally (`0b39255`) |
| `195e3a2` | `fix/r7-w9` | Helm NOTES print the generated password command only when the chart generates the password (`8a4f1a5`) |

Fixes in earlier rounds are described in the owners' handoffs, in `RESOLUTIONS.md` and in this file's history.

## Open issues

### Red

- **A 20 GB import fails on the preview Instance (high, needs James).** The preview's `hangar import` of `Qwen/Qwen2.5-Coder-32B-Instruct-GGUF` failed with HTTP 500 on `qwen2.5-coder-32b-instruct-q4_k_m.gguf` on 2026-10-16, while upload completion read the assembled object back from Aquila's MinIO (`repo: upload lfs part: upload part 65: unexpected EOF`, then part 66). The preview then ran image `preview-5975bec`. The server side is fixed further on this branch: completion resumes a broken read with ranged reads and keeps the upload and its parts (`fix/r4-w2`), hashes the assembled object and copies it into place server-side, and outlives its caller; the CLI resends a completion after 503 (`071ee7d`) and retries failed parts (finding 8 of [Preview feedback round (2026-10-17)](#preview-feedback-round-2026-10-17)). `TestCompleteUploadResumesABrokenRead`, `TestCompleteUploadKeepsPartsWhenTheReadFails`, `TestCompleteUploadCopiesServerSide` and `TestImportResendsCompletionAfter503WithRetryAfter` pass. No suite here moves a multi-GB file through Nginx Proxy Manager, so the fix is not proven end to end. To close it, rebuild the preview and the CLI from `integration/v1`, run `hangar admin doctor --large-read` against Aquila's MinIO, and rerun the import. Why the MinIO read drops at about the same offset each time is not known; the proxy in front of Aquila's MinIO is worth checking if the rerun still logs retries.

### Gaps and decisions

Open:

- **Unit suite at `6610f45`, and the 6c tag test.** See [Still open after Phase 3](#still-open-after-phase-3). Both must be closed before `main` moves.
- **Questions for James.** The open questions in [decisions](decisions.md): whether `scan.block_unsafe_downloads` also refuses suspicious pickles; who may reference an LFS or Xet object by its hash (W11 8, deferred from Phase 2; global deduplication stays and is documented in the [threat model](../security/threat-model.md)); whether Namespace profiles and member lists are public when `anonymous_read` is off; how concurrent server-side Commits to one branch should behave; and how Ollama should learn that a quant is not pulled. The current behaviour stands until he decides.
- **Dark-theme axe coverage.** Whether every page needs a dark-theme axe check; see [Still open after Phase 3](#still-open-after-phase-3).
- **Release on hold.** Lifted by James on 2026-10-18; v1.0.0 and v1.1.0 are released (see [Releases](#releases-2026-10-18-and-2026-10-19)).
- **Release exercised.** Keyless signing through GitHub OIDC and the multi-arch push ran in `release.yml` for v1.0.0 and v1.1.0, and both verify (see [Releases](#releases-2026-10-18-and-2026-10-19)).
- **Local-only checks.** Results before 2026-10-18 come from the development machine. Since then every pull request runs the suites on GitHub Actions on amd64 and arm64; every benchmark still comes from one shared laptop.
- **Not covered in Phase 3.** The floor-client, latest-Python and without-`hf_xet` Hub cells, the `diffusers` and `upload_large_folder` cells, the 6 GiB nightly file, a timed fuzz run (round 4 ran the fuzz seeds only), LM Studio against a real server, and a kind install that starts the Blob store late.
- **`govulncheck` binary.** The installed `govulncheck` is built with an older Go and cannot load this module; the `go run ...@v1.9.0` form works. This is a local tool problem.

Closed in Phase 3:

- **W8 8, self-service profile and password changes.** Closed on 2026-10-18; see [W8 8 closed](#w8-8-closed-self-service-profile-and-password).

- **git-upload-pack and unsafe files.** `Gate.CheckGit` gates every git fetch (`f65ba44`).
- **Scan errors are not blocked.** Data that is not a pickle is now safe, and a partial scan of a file that can run code is refused (`4e62696`).
- **`/admin/system` does not report `upload_mode`.** It does (`a23e48e`).
- **Tests that passed without their backend.** They skip visibly, and `make lint` stops the pattern (`81978ac`).
