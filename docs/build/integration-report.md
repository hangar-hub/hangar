# Integration report

Phase 2 step 5 of [`15-build-plan.md`](../spec/15-build-plan.md): every feature ID in [`01-product.md`](../spec/01-product.md) and [`14-datasets.md`](../spec/14-datasets.md) with the tests that show it works, and the open issues. It describes branch `integration/v1` at `6fe4b25` with the lead's second round 5 branch merged on top, which also brings in `fix/r4-lead` and `fix/r4-w7`. The rows below were first checked at `5975bec`; [Merges after 5975bec](#merges-after-5975bec) lists what changed since and how it was checked. Each owner's handoff describes its own fixes in more detail.

## How it was checked

The wired binary is `cmd/hangar` built from this branch. Every row below names tests that run in `make lint test` (unit and integration tests on SQLite and Postgres, MinIO for S3) or in `make test-compat`, the Playwright suites, or the manual checks listed here.

| Check | Command | Result |
|---|---|---|
| Wiring, readiness and adapters | `go test -race -tags integration ./cmd/hangar/` | ok; statement coverage 83.9% (floor 75%) |
| Readiness during start-up | `HANGAR_BINARY=... go test -count=10 -tags compat -run TestStartServesHealthAndStops ./compat/harness/` | 10 of 10 passed |
| S3 proxy modes against MinIO | `HANGAR_TEST_MINIO=1 go test -run 'TestS3ProxyModes\|TestOpenS3' ./cmd/hangar/ ./internal/blob/...` | ok |
| Unsafe-download gate on every route | `go test -run 'TestUnsafeDownloadGateCoversEveryRoute\|TestServingSurfaceIsClassified\|TestFileByteReadersAreClassified' ./cmd/hangar/` and the gate tests in `internal/scan`, `internal/hfapi`, `internal/gitserver`, `internal/ollama`, `internal/api` | ok |
| Ollama client against the binary | `HANGAR_BINARY=... go test -tags compat ./compat/ollama/` with Ollama 0.31.1 | 7 of 7 subtests passed |
| Hub clients against the binary | `HANGAR_HF_INSTANCE=binary uv run --project compat pytest compat/hf`, also with `HANGAR_HF_XET_UPLOADS=1` | 41 passed (the library tests on Python 3.11, see the README note on SciPy and macOS) |
| Hub clients against the fakes, with the matrix written | `HANGAR_HF_WRITE_MATRIX=1 uv run --project compat pytest compat/hf` | 41 passed |
| UI against the mock | `pnpm exec playwright test e2e/flows.spec.ts e2e/pages.spec.ts` | 297 passed |
| UI against a real Instance | `make e2e-real` (seeded by `ui/e2e-real/seed.ts`) | 16 passed, desktop and phone |
| Compose `postgres-minio` and `scale` with S3 proxy uploads | Image built from this branch; `hangar push`, `git lfs push` and `huggingface_hub` 0.36.1 `upload_file` of 12 MiB files, downloads compared byte for byte, a zip export | All round trips identical; the zip URL is Hangar's |
| Docs Home demo | `scripts/demo.sh` against the binary | `docs/assets/demo.gif` |
| Compose and Helm smoke, rerun at `fix/r2-lead` | Image built from that branch; `scripts/compose-smoke.sh` on `quickstart`, `postgres-minio`, `scale` and `oidc-keycloak`; on kind, the chart with `ci/default-values.yaml`, `helm test` and `scripts/smoke.sh` through a port-forward, each including the `pull` step | All passed |
| Full lint and test run after the merges since `5975bec` | `make lint test` on the lead's second round 5 branch | Passed in 16 minutes at a load average of 45 to 58; `internal/api` 459 s, `internal/upstream` 389 s |
| Compat harness cleanup | `go test -tags compat ./compat/harness ./compat/lmstudio ./compat/viewer ./compat/ollama` and `pytest compat/test_harness.py`, counting `hangar-compat-bin-*` in `$TMPDIR` before and after | No directory left behind |

## Feature checklist

| ID | Status | Evidence |
|---|---|---|
| REPO-1 | Done | `internal/repo` lifecycle tests on both drivers; `compat/hf` `test_repos.py`; `ui/e2e` flows |
| REPO-2 | Done | `internal/repo` list visibility tests; `internal/auth` permission tests; `compat/hf` `test_errors.py` |
| REPO-3 | Done | `internal/repo` `commit_test.go` (log, diff, `FileDiff`, `CompareFile`); `internal/api` `TestUploadCommitAndBrowseEndpoints`; mock flows |
| REPO-4 | Done | `internal/repo/lifecycle_test.go`; `compat/hf` `move_repo` and redirect tests; `compat/git` |
| REPO-5 | Done | `internal/repo/lifecycle_test.go` (restore window, purge) |
| REPO-6 | Done | `internal/community/communitydb`; `internal/hfapi` discussion tests; `internal/api` `TestDiscussionEndpoints` and `TestPullRequestFileDiff`; `compat/hf` `test_discussions_and_pull_requests`, `test_edit_and_hide_comments`; real-Instance flow "opens a pull request from an upload and shows its diff" |
| REPO-7 | Done | `internal/community/communitydb`; `internal/hfapi` `TestLikersAndUserLikes`; `cmd/hangar` `TestHubLikersAndCommentChanges`; `compat/hf` `test_likers_and_liked_repos`, `test_unlike` |
| REPO-8 | Done | `internal/auth/access_requests`; `compat/hf` gated tests; mock flows |
| REPO-9 | Done | `internal/repo` settings tests; `compat/hf` `update_repo_settings` |
| UP-1 | Done | `compat/hf` `test_upload.py` (fakes and the binary); `internal/hfapi` write tests |
| UP-2 | Done | `compat/git` (latest git and 2.39); `internal/gitserver` |
| UP-3 | Done | mock flows (parts, resume after reload against the resuming plan); real-Instance upload flow |
| UP-4 | Done | `internal/cli` push tests; `hangar push` of 12 MiB on the `postgres-minio` and `scale` examples |
| UP-5 | Done | `internal/blob` `TestStreamsLargeObjects` on both backends; nightly 6 GiB Hub cell |
| UP-6 | Done | `internal/repo/lfs_test.go`; `compat/hf` `test_upload_over_multipart_threshold` (second upload sends nothing) |
| UP-7 | Done | `compat/xet` (hf_xet 1.6.0 and 1.7.0, fakes and real); `compat/hf` `hub-binary` job, Xet cells |
| DOWN-1 | Done | `internal/hfapi` resolve tests; `compat/hf` `test_download.py` |
| DOWN-2 | Done | `internal/blob/backends` `TestOpenS3`; `internal/hfapi` `TestResolveRedirectMode`; `cmd/hangar` `TestS3ProxyModesKeepTransfersOnHangar` for proxy mode |
| DOWN-3 | Done | `compat/git`; `internal/gitserver` |
| DOWN-4 | Done | `compat/ollama` against the binary; `internal/ollama` |
| DOWN-5 | Done | `internal/api` zip export test; `internal/repo` `runZipExport` |
| PULL-1 | Done | `internal/upstream` registry tests; `compat/upstream` |
| PULL-2 | Done | `internal/upstream` plan and pack tests; `compat/upstream` |
| PULL-3 | Done | `internal/upstream`; `internal/gitserver` `TestLFSPartialMirror` |
| PULL-4 | Done | `internal/upstream` matcher tests; mock Discover flow |
| PULL-5 | Done | `internal/upstream` Pull Job tests; `internal/jobs` |
| PULL-6 | Done | `internal/upstream` transfer tests |
| PULL-7 | Done | `internal/jobs/limiter_test.go`; `internal/upstream` window tests |
| PULL-8 | Done | `internal/upstream` credential tests |
| PULL-9 | Done | `internal/upstream` tracking tests |
| PULL-10 | Done | `internal/upstream` Pull-through tests; `compat/upstream`; `cmd/hangar` `TestHubPullThroughAdapter` |
| PULL-11 | Done | `internal/upstream` dependency tests; `cmd/hangar` `TestDatasetWiring` (`afterIndex`) |
| PULL-12 | Done | `internal/cli` import tests; `internal/upstream` importer |
| BROWSE-1 | Done | `internal/repo` list and facet tests; mock flows (virtualised Arsenal) |
| BROWSE-2 | Done | `internal/upstream` discover tests; mock Discover flows |
| BROWSE-3 | Done | mock page tests at three widths; real-Instance browsing flow |
| BROWSE-4 | Done | mock viewer flows; real-Instance safetensors flow; `internal/api` `TestFileScan` |
| BROWSE-5 | Done | `internal/collections`; `compat/hf` `test_collections`; mock flows |
| BROWSE-6 | Done | mock keyboard search flow |
| AUTH-1 | Done | `internal/auth`; `internal/api` `TestLoginThrottleMessage`; real-Instance sign-in flow |
| AUTH-2 | Done | `internal/auth` OIDC tests; the `oidc-keycloak` Compose job in `compat.yml` |
| AUTH-3 | Done | `internal/auth/login` tests |
| AUTH-4 | Done | `internal/auth` Token tests; real-Instance Token flow |
| AUTH-5 | Done | `internal/org` on both drivers (`TestListOrgs` too); `internal/api` `TestAdminEndpoints` |
| AUTH-6 | Done | `internal/auth/bootstrap`; `cmd/hangar` admin command tests |
| AUTH-7 | Done | `internal/auth` Ollama key tests; `compat/ollama` private Repo with a registered key against the binary |
| META-1 | Done | `internal/index` extract and fuzz tests |
| META-2 | Done | `internal/scan`; `internal/scan/gate_test.go`; route checks in `cmd/hangar/unsafe_test.go`, enumeration of protocols, signed-link routes and file byte readers in `cmd/hangar/unsafe_surface_test.go` |
| META-3 | Done | `internal/scan` integrity tests |
| DATA-1 | Done | as REPO-1 to REPO-9 for dataset Repos |
| DATA-2 | Done | `internal/datasets/layout` (70 layouts against `datasets` 5.1.0) |
| DATA-3 | Done | `internal/datasets/pipeline` |
| DATA-4 | Done | `internal/datasets/viewer`; `compat/viewer` |
| DATA-5 | Done | `internal/datasets/stats` (32 oracle cases against the Hub's statistics code) |
| DATA-6 | Done | mock Viewer flow; real-Instance Viewer flow |
| DATA-7 | Done | `internal/upstream` dataset Discover tests |
| DATA-8 | Done | `internal/upstream` dataset selection tests |
| DATA-9 | Done | `internal/index` dataset summary; `internal/index/indexdb` `TestDatasetSplitSizesFollowTheParquetExport` and `TestModelCardDatasetsAreIndexed`; `internal/datasets/pipeline` `TestAfterConvertRunsOncePerExport`; `cmd/hangar` `TestDatasetWiring` |
| DATA-10 | Done | `internal/datasets/croissant` |
| DATA-11 | Done | `internal/repo` list filters on the dataset summary; mock Arsenal facets |
| DATA-12 | Done | `internal/cli` dataset tests |
| DATA-13 | Done | `internal/datasets/card`; mock card editor flow |
| EVT-1 | Done | `internal/webhook` |
| EVT-2 | Done | `internal/audit`; `internal/api` audit tests |
| OPS-1 | Done | `internal/blob` conformance on both backends; S3 proxy modes |
| OPS-2 | Done | every `dbtest.ForEachDriver` test runs on SQLite and Postgres |
| OPS-3 | Done | Compose `scale` (migrate, two web and two worker processes) started and used in this round; the Helm job in `compat.yml` |
| OPS-4 | Done | `internal/gc`; `cmd/hangar` `TestQuotaAdapters`, `TestAdminGC` |
| OPS-5 | Done | `internal/telemetry`; `cmd/hangar` readiness tests |
| OPS-6 | Done | `internal/config`; `make check-generate` |
| OPS-7 | Done | image built from this branch in this round; the Compose and Helm jobs in `compat.yml` |
| OPS-8 | Done | `internal/cli` backup and restore tests |
| OPS-9 | Done | mock admin flows; real-Instance admin flows |

## What round 1 of the lead fixed

1. `cmd/hangar` coverage was 73.4%. Tests now cover the wiring adapters and admin error paths: 83.9%.
2. `/readyz` answered 200 before the Instance was built. A `startup` check keeps it at 503 until `hangar started` is logged.
3. Large-file uploads failed on the S3 examples, whose MinIO endpoint clients cannot resolve. `storage.s3.upload_mode: proxy` sends uploads through Hangar, `download_mode: proxy` now covers zip exports too, and both examples set them.
4. `scan.block_unsafe_downloads` applied to Hub API `GET /resolve` only and failed open while a scan was pending. `scan.Gate` now covers the Hub API (including presigned links on `HEAD`), git-lfs batches and signed links, Ollama blobs and zip exports, fails closed for pickle, joblib and NumPy files without a result, and the UI asks before such a download.
5. The Home page demo is recorded.
6. `compat/ollama` runs against the binary; Playwright has a real-Instance suite in CI; the Hub client suite has the `hub-binary` job (SQLite and Postgres, with and without Xet uploads, and the floor client).
7. Requests done this round: W4 6 and 8, W8 2, 3, 5, 7 and 9, W10 4 and 10 (see [`RESOLUTIONS.md`](requests/RESOLUTIONS.md)). Found while doing them: the sign-in throttling message was a raw Go error, and the mock's wrong-password message differed from the server's.

## What rounds 2 and 3 fixed

Round 2 merged `fix/r2-lead`, `fix/r2-w2`, `fix/r2-w4`, `fix/r2-w6`, `fix/r2-w7` and `fix/r2-w8`; round 3 merged `fix/r3-w1`, `fix/r3-w7`, `fix/r3-w8`, `fix/r3-w9` and `fix/r3-w11`.

1. **Unsafe downloads (lead, W7, W2).** The signed Pull-through route and the signed zip export download are gated, and `TestServingSurfaceIsClassified` enumerates every protocol and signed-link route instead of a hand-kept list. `scan.Gate.CheckObjects` fails closed for an object that no scanned file covers, judging every file that holds it in every ref tip and Flagged Commit. W2's request 10 carries scan results from the nearest scanned ancestor to unchanged files while a new Commit's scan is pending, so the file page and downloads stop flickering to unscanned after each Commit (see [`RESOLUTIONS.md`](requests/RESOLUTIONS.md)). After both merges, `414027d` updated `TestCurrentWithoutScannedAncestor` for the combined verdict. Round 3 added tests that a failed scan Job stores nothing and leaves every file that can run code refused, on each driver and end to end.
2. **Anonymous listing (lead).** `repo.Options.AnonymousRead` is asked on each anonymous List, so `security.anonymous_read` set through `PATCH /admin/settings` applies without a restart and Discover against a Hangar Upstream finds its Repos. Round 3 updated the Xet real backend (W11) to the new signature.
3. **Job attempt fencing (W1).** Progress, result and finish writes match the attempt number a run was claimed with as well as the lease owner, so a run that `RecoverExpired` requeued gets `ErrLeaseLost` at its next write and cannot finish the Job under the new run.
4. **Flaky tests (W6, lead).** `TestUpstreamsAPI/sqlite` lost `update_available` under `-race` load because a hidden 10-second limit and a fixed 5-second budget raced the test Upstream's git calls; the budget is now `Upstreams.UpdateCheckTimeout` and a missed check is logged (W6 handoff, item 8). `TestPullThroughRepoAndFile/sqlite` waited 5 seconds for planning instead of the Service's 30 (item 9). `TestFirstReadyAnswerServesEveryRoute` now closes its poll client's idle connections before shutdown.
5. **Scan and index Jobs on deleted Repos (W7).** They end quietly when the Repo is deleted mid-run instead of failing at `WARN`.
6. **Compat and CI (lead, W9).** The viewer's `instance-postgres` backend now sets `database.driver=postgres`, so it runs on Postgres; `compat/.python-version` pins Python 3.11 for `compat/.venv`; the git 2.39 job installs uv, and `scripts/check-workflow-uv.sh` fails a job that needs uv without installing it.
7. **UI (W8).** Markdown headings nest under the page's `h1`, the search palette's listbox stays valid while results load, `e2e-real/` runs the build plan's flows and 51 pages against real Instances with a second Instance as the Upstream, and its sign-in flows wait out sign-in locks so reruns within the hour pass.
8. **House style (W9).** The Xet upload research uses British spellings.

## What round 5 of the lead fixed

1. **Leaked compat binaries.** The Go harness and the pytest launcher built `./cmd/hangar` into a new `hangar-compat-bin-*` directory on every run and never removed it, about 65 MB each. `harness.Main`, called from `TestMain` in every Go compat package that builds the binary, removes it when the tests end, and `Binary` refuses to build outside `Main`; `hangar_instance.py` removes it with an `atexit` hook. The builder tests in `compat/harness` and `test_built_binary_is_removed_when_the_process_exits` in `compat/test_harness.py` cover both.
2. **Records.** W2's request 10 has its row in `RESOLUTIONS.md`, and this report covers rounds 2 and 3.

## Merges after 5975bec

These branches were merged into `integration/v1` after `5975bec`, in this order. Fix branch names carry the round they were opened in, so some round 3 branches were merged after round 4 and 5 ones.

| Branch | Owner | What it changed |
|---|---|---|
| `fix/r5-lead` | Lead | The compat harnesses remove the `hangar` binary they build; the records in this report and `RESOLUTIONS.md` (see [What round 5 of the lead fixed](#what-round-5-of-the-lead-fixed)) |
| `fix/r5-w6`, with `fix/r4-w6` | W6 | The lost-lease Pull compat test follows attempt fencing; Pull-through waits for the history of a Mirror that planning has just created, instead of answering 404 `Invalid rev id: main` in that window |
| `fix/r5-w10` | W10 | The viewer fixture downloads retry a stalled read and verify each file, so `TestCroissantValidates` no longer fails on a slow huggingface.co |
| `fix/r3-lead` | Lead | `internal/api` tests run in parallel, every test package shares one Postgres container, and each test database is a copy of a migrated template; doctor checks that start git or dial get a minute in tests |
| `fix/r3-w4`, `fix/r3-w11` | W4, W11 | The real compat backends pass `AnonymousRead` as a setting function, and `make lint-go` vets their build tags |
| `fix/r3-w9` | W9 | `make lint-style` checks docs for American spellings; the git 2.39 compat job installs uv and the compat lock |
| `fix/r3-w8` | W8 | `e2e-real` sign-in flows wait out sign-in locks |
| `fix/r3-w6` | W6 | The Pull-through Mirror history fix, as in `fix/r5-w6` |
| `fix/r3-w7` | W7 | Tests that the gate stays closed after a scan Job fails |
| `fix/r3-w1` | W1 | Job progress, result and finish writes are fenced on the attempt a run claimed |
| `fix/r4-w8` | W8 | `e2e-real` waits for slow git-backed reads, and the editor flows create their Repos in hooks |
| `fix/r4-w2` | W2 | Completing an upload resumes a broken read of the assembled object with ranged reads, and keeps the upload and its parts when it fails, so a retried completion can succeed |
| `fix/r4-lead` | Lead | The same four commits as `fix/r3-lead`, under the round 4 name; merging it changes no file |
| `fix/r4-w7` | W7 | `compat/ollama` starts `ollama serve` in its own process group and stops it with its runners, so no `llama-server` is left running |

### What the lead's second round 5 pass fixed

1. **Request IDs for server errors.** A 500 from the native API, the Ollama registry, dataset configs and Repo metadata said "The request ID identifies it in the server log" but named no ID, the log lines carried no `request_id`, and the CLI dropped the ID. Now:
   - `httpx.WithInfo` stores the request ID where the log handlers read it, and the stdout handler and the OpenTelemetry log bridge add `request_id` to every record logged with the request's context, including the access log line and the `native api request failed` line.
   - Every 500 message comes from `httpx.InternalErrorMessage`: "Internal server error. The request ID is `<id>`."
   - The CLI's `APIError` keeps the ID from `X-Request-Id` or the problem's `request_id`, and prints it for 5xx answers whose message does not already name it.

   Tests: `internal/api` `TestInternalErrorNamesTheRequestIDTheLogCarries`, `internal/httpx` `TestLogLinesCarryTheRequestID` and `TestRecoverWritesErrorInProtocolFormat`, `internal/telemetry` `TestHandlersAddTheRequestID`, `internal/cli` `TestAPIErrorNamesTheRequestIDOfServerErrors`. The operator docs ([Observability](../operate/observability.md), [Troubleshooting](../operate/troubleshooting.md)) list the field.
2. **Branch completeness.** `fix/r4-lead` and `fix/r4-w7` are merged, and this report covers every merge after `5975bec`.

## Open issues

- **W8 8, self-service profile and password changes.** A product decision for James: there is no `PATCH /me` or `POST /me/password`; admins send reset links.
- **W11 8 and W3, ownership of LFS and Xet objects at Commit time.** Deferred to the phase 3 security review, as the build plan says: a user who knows an object's SHA-256 can reference it from their own Repo.
- **git-upload-pack and unsafe files.** `scan.Gate` does not gate `git clone` without LFS, because git sends whole Commits. The Hub's default `.gitattributes` keeps every pickle format in LFS, and [Security](../operate/security.md) says so.
- **Scan errors are not blocked.** A file the scanner read but could not parse (status `error`) downloads with blocking on, because many `.bin` files are not pickles. The phase 3 review should confirm the trade-off.
- **Package timeouts under load.** Resolved by `fix/r3-lead` (the same commits as `fix/r4-lead`). A full `make lint test` on the lead's second round 5 branch, at a load average of 45 to 58 on 12 CPUs with other projects running, passed in 16 minutes: `internal/api` in 459 s and `internal/upstream` in 389 s, both inside the 10-minute package timeout.
- **Local-only checks.** There is no remote yet, so the `hub-binary` Hub cells and the real-Instance Playwright step have run on the development machine only, not in CI.
- **`/admin/system` does not report `upload_mode`.** It reports `download_mode`; adding `upload_mode` changes the OpenAPI file and is left to the owners.
