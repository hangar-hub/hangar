# Phase 3 handoff: open gaps

The phase 3 "gaps" workstream of [`15-build-plan.md`](../../spec/15-build-plan.md), on branch `ws/p3-gaps` from `382cc88`. It closes the items left open in the [integration report](../integration-report.md) ("Still open" in the preview feedback section, and "Gaps and decisions").

## What was reviewed

- `GET /admin/system`: the OpenAPI schema, `internal/api/admin_system.go`, its wiring in `cmd/hangar/native.go` and the admin **System** page.
- Server start-up in `cmd/hangar/server.go` and `router.go`: the order of database, Blob store, listener and readiness, against the chart's startup, liveness and readiness probes (`deploy/helm/hangar/templates/_helpers.tpl`).
- `scripts/kind-chart-test.sh`: how it loads images and what happens when the Hangar image is missing.
- Log levels in `internal/datasets/pipeline/convert.go` against the Viewer's.
- Every Go test and test helper for early returns that report PASS when a backend or tool is missing (a scan of `if` blocks that log and return, or return on an `Enabled()` check or a `HANGAR_TEST_` variable), `dbtest.ForEachDriver`, the Blob store conformance suite and the Ollama compat targets.
- `make release-rehearsal`: the GoReleaser snapshot with SBOMs and signing, and the image signing, SBOM and attestation steps of `release.yml`, with a throwaway key.

## Findings

| # | Severity | Finding | Fix | Regression tests |
|---|---|---|---|---|
| 1 | Low | `/admin/system` reported `download_mode` but not `upload_mode`, so an admin could not see whether clients upload to the bucket directly | `a23e48e`: `blob_store.upload_mode` in the OpenAPI schema (with descriptions for every `blob_store` field), `SystemInfo.UploadMode`, `uploadMode(cfg)` (S3 only), and the System page shows "s3, redirect downloads, direct uploads" | `TestAdminSystemReportsS3TransferModes`, `TestAdminSystem` (filesystem reports neither mode), `TestUploadModeIsReportedOnlyForS3`, Vitest "names the Blob store's download and upload modes" |
| 2 | Medium | A web or worker process exited at once when its S3 Blob store did not answer at start (`reach bucket "hangar": ... connection refused`), and the kubelet's restart back-off then outlasted the rollout. The process also did not listen until the store answered, so `/healthz` failed the startup probe | `22eded4`: the server listens first, answers `/healthz`, keeps `/readyz` at 503 with `startup: the server is still starting: waiting for the Blob store to answer`, and retries the S3 store (1 s doubling to 10 s) for up to the new `storage.s3.startup_wait` (default `5m`, `0` fails at once), logging `waiting for the Blob store` with the endpoint, bucket, attempt and error at WARN. The filesystem backend is not retried. Decision recorded in [decisions](../decisions.md); documented in [02](../../spec/02-architecture.md), [Storage](../../operate/storage.md) and the configuration reference | `TestServeAnswersHealthWhileWaitingForTheBlobStore` (fails on the old code: `serve` exits before listening), `TestOpenBlobStoreWaitsForTheStore`, `TestOpenBlobStoreGivesUpWaiting`, `TestOpenBlobStoreFailsAtOnce`, `TestOpenBlobStoreStopsWaitingOnShutdown`, `TestValidateReportsEveryError` (range check) |
| 3 | Low | `scripts/kind-chart-test.sh` skipped a Hangar image that was not local, so each `TestKind` test waited 5 to 6 minutes and failed with `ErrImageNeverPull` | `cebfa8f`: the script fails before creating a cluster, naming the image and `make image IMAGE=...`, and fails when the Hangar image cannot be loaded into the cluster | `TestKindScriptFailsAtOnceWithoutTheImage`, `TestKindScriptFailsWhenTheImageCannotBeLoaded` (both fail on the old script), `TestKindScriptLoadsTheImageAndRunsTheTests`; they run the script with stand-in `docker`, `kind`, `kubectl` and `go` |
| 4 | Low | The conversion logged "the dataset has no Configs" at WARN for every dataset with no Viewer files, a normal state since "Datasets hold any files" | `f210b05`: DEBUG, as the Viewer logs it | `TestDatasetOfOtherFilesLogsAtDebug`, `TestFailures` (now asserts the level); both fail with WARN |
| 5 | Medium (test integrity) | 23 places in Go tests returned early and so reported PASS when Postgres or S3 was not configured, for example `TestHubBasicUploadToMinIOWithoutXet` and the S3 conformance suite; `ForEachDriver` and the Ollama compat targets left the Postgres case out without a trace; the conformance suite's `ListUploads` case passed silently for every store without the capability | `81978ac`: `dbtest.RequirePostgres` and `blobtest.RequireS3` call `t.Skipf` with how to enable the test; `ForEachDriver` and `TestOllamaClient` report a skipped `postgres` subtest; the fault-injection case skips with its reason; `Capabilities.ListUploads` makes the conformance case assert the declared capability either way (the filesystem store lists, S3 and the fake must say they cannot). `scripts/check-silent-skips.sh` runs in `make lint` and fails on the pattern | `TestSilentSkipCheck` (the check finds all 23 places in the tree at `382cc88`, and none now); `TestConformance/ListUploads` |
| 6 | None | `make release-rehearsal` had never run | Ran it with cosign 3.1.3 and syft 1.54.1 (release binaries, checksums verified) and yq 4.52.5. It passed with no changes: GoReleaser built 6 archives, the `checksums.txt` bundle verifies and a changed `checksums.txt` does not, every archive is in `checksums.txt` with an SPDX SBOM listing the hangar module, the image signature verifies and does not verify against another key, and the image's SPDX SBOM attestation verifies | The run itself (below) |

`9546e56` is a lint fix to finding 4's test, and `cb5c244` gives finding 2's serve-level test room on a loaded host.

## Verification

All on `ws/p3-gaps` on 2026-10-17, Go 1.27.2 and Node 24.19.0, on a shared machine whose load average was between 30 and 130 while other agents ran load tests and suites.

| Suite | Command | Result |
|---|---|---|
| Lint | `make lint` (golangci-lint, `go vet` for the compat tags, Biome, `tsc`, `lint-style` with the new `check-silent-skips.sh`) | 0 issues |
| Unit and integration | `make test GOTESTFLAGS='-race -count=1 -json'` (SQLite, Postgres 18 and MinIO) | Exit 0. 2,195 Go tests and subtests passed, 0 failed, **0 skipped**; 77 packages ok. Vitest 108 of 108. The first run, before `TestServeAnswersHealthWhileWaitingForTheBlobStore` allowed for a slow start, failed that test alone (the server took over 3 s to listen at load average 128); the test now waits up to 2 minutes for the listener |
| Generated files | `make check-generate` | No diff |
| Chart and scripts, static | `make lint-deploy` | Passed |
| Compat, Go | `go test -json -count=1 -tags compat ./compat/...` through `with-test-postgres.sh` and `with-test-minio.sh` (as `make test-compat`) | 205 passed, **0 skipped**, 6 failed under load: `TestOllamaClient/in-process` (`ollama pull` timed out at "pulling manifest" after 99 s, so `show` and `run` found no model) and `TestPullThroughFromHuggingFace` ("the pull has not finished planning yet", against live huggingface.co). Both passed when rerun alone: `TestOllamaClient` 47 s with all three targets, `binary-postgres` included; `TestPullThroughFromHuggingFace` 4 s |
| Compat, pytest | `uv run --project compat pytest compat -rs` | 69 passed, 2 skipped by design (see Residual risks) |
| Release rehearsal | `make release-rehearsal IMAGE=hangar:p3gaps-rehearsal` with cosign 3.1.3, syft 1.54.1, yq 4.52.5, GoReleaser 2.18.3 | Exit 0, no changes needed |
| kind script | `scripts/kind-chart-test.sh hangar:p3gaps-does-not-exist` | Exit 1 in 0.3 s with the new message, no cluster created |

Not run: a full `scripts/kind-chart-test.sh` install. The machine had 4.4 GiB of disk free, shared with other agents' kind clusters and images, which is not enough for a kind node, a fresh image and Postgres and MinIO. A GitOps kind run is the natural end-to-end check of `storage.s3.startup_wait`, so run it once there is room.

## Residual risks

- **Every S3 error is retried at start.** A wrong bucket, region or credentials now fails start-up after `storage.s3.startup_wait` (5 minutes by default) instead of at once. Each attempt logs the error at WARN, so the cause is visible at once; set `startup_wait: 0` to fail fast. This was chosen to match `--wait-for-database`, because a GitOps install may create the bucket or credentials next to Hangar.
- **Only server processes wait.** `hangar admin` commands and `hangar admin doctor` still fail at once on an unreachable store; they are run by hand.
- **Requests other than health checks during the wait** get the start-up handler's answer, as they already did while a process waited for `hangar admin migrate`. Load balancers must use `/readyz`, as the chart and Compose files do.
- **The silent-skip check is a pattern match.** It catches `if` blocks that only log and return, and returns on `Enabled(...)` checks and `HANGAR_TEST_` variables. A test that gates on something else and returns without logging is not caught. Two early returns are kept on purpose: `TestRealWorkerProcess` in `compat/upstream` is a subprocess entry point that does nothing unless its parent test starts it, and `recordMatrix` in `compat/hf` is a helper, not a test.
- **Two pytest modules skip in the locked environment by design.** `compat/hf/test_diffusers.py` and `test_upload_large_folder.py` skip visibly with their reason and run in other CI matrix cells; `make test-compat` uses the lock, so it reports them as skipped.
- **The kind script tests use stand-ins.** They check the script's logic, not kind itself, and the full kind install did not run this round (disk).
- **Two compat tests are sensitive to host load.** `TestOllamaClient/in-process` and `TestPullThroughFromHuggingFace` failed once at load average 30 to 40 and passed alone. They are outside this area; neither change here touches them beyond reporting the Ollama Postgres target as skipped when Postgres is off.
- **The release rehearsal is offline.** It uses a throwaway key with no Fulcio certificate, transparency log or timestamp, so the keyless GitHub OIDC signing in `release.yml` and the multi-arch push still run for the first time in James's release.
