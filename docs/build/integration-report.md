# Integration report

Phase 2 step 5 of [`15-build-plan.md`](../spec/15-build-plan.md): every feature ID in [`01-product.md`](../spec/01-product.md) and [`14-datasets.md`](../spec/14-datasets.md) with the tests that show it works, and the open issues. It describes branch `integration/v1` with the integration lead's round 1 fixes (`fix/r1-lead`). Fixes from the other owners' round 1 branches are listed in their own handoffs once merged.

## How it was checked

The wired binary is `cmd/hangar` built from this branch. Every row below names tests that run in `make lint test` (unit and integration tests on SQLite and Postgres, MinIO for S3) or in `make test-compat`, the Playwright suites, or the manual checks listed here.

| Check | Command | Result |
|---|---|---|
| Wiring, readiness and adapters | `go test -race -tags integration ./cmd/hangar/` | ok; statement coverage 83.9% (floor 75%) |
| Readiness during start-up | `HANGAR_BINARY=... go test -count=10 -tags compat -run TestStartServesHealthAndStops ./compat/harness/` | 10 of 10 passed |
| S3 proxy modes against MinIO | `HANGAR_TEST_MINIO=1 go test -run 'TestS3ProxyModes\|TestOpenS3' ./cmd/hangar/ ./internal/blob/...` | ok |
| Unsafe-download gate on every route | `go test -run TestUnsafeDownloadGateCoversEveryRoute ./cmd/hangar/` and the gate tests in `internal/scan`, `internal/hfapi`, `internal/gitserver`, `internal/ollama`, `internal/api` | ok |
| Ollama client against the binary | `HANGAR_BINARY=... go test -tags compat ./compat/ollama/` with Ollama 0.31.1 | 7 of 7 subtests passed |
| Hub clients against the binary | `HANGAR_HF_BACKEND=binary uv run --project compat pytest compat/hf`, also with `HANGAR_HF_XET_UPLOADS=1` | 41 passed (the library tests on Python 3.11, see the README note on SciPy and macOS) |
| Hub clients against the fakes, with the matrix written | `HANGAR_HF_WRITE_MATRIX=1 uv run --project compat pytest compat/hf` | 41 passed |
| UI against the mock | `pnpm exec playwright test e2e/flows.spec.ts e2e/pages.spec.ts` | 297 passed |
| UI against a real Instance | `make e2e-real` (seeded by `ui/e2e-real/seed.ts`) | 16 passed, desktop and phone |
| Compose `postgres-minio` and `scale` with S3 proxy uploads | Image built from this branch; `hangar push`, `git lfs push` and `huggingface_hub` 0.36.1 `upload_file` of 12 MiB files, downloads compared byte for byte, a zip export | All round trips identical; the zip URL is Hangar's |
| Docs Home demo | `scripts/demo.sh` against the binary | `docs/assets/demo.gif` |

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
| UP-7 | Done | `compat/xet` (hf_xet 1.6.0 and 1.7.0, fakes and real); `compat/hf` `binary-xet` cell |
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
| META-2 | Done | `internal/scan`; `internal/scan/gate_test.go`; route enumeration in `cmd/hangar/unsafe_test.go` |
| META-3 | Done | `internal/scan` integrity tests |
| DATA-1 | Done | as REPO-1 to REPO-9 for dataset Repos |
| DATA-2 | Done | `internal/datasets/layout` (70 layouts against `datasets` 5.1.0) |
| DATA-3 | Done | `internal/datasets/pipeline` |
| DATA-4 | Done | `internal/datasets/viewer`; `compat/viewer` |
| DATA-5 | Done | `internal/datasets/stats` (32 oracle cases against the Hub's statistics code) |
| DATA-6 | Done | mock Viewer flow; real-Instance Viewer flow |
| DATA-7 | Done | `internal/upstream` dataset Discover tests |
| DATA-8 | Done | `internal/upstream` dataset selection tests |
| DATA-9 | Done | `internal/index` dataset summary; `internal/index/indexdb` `TestDatasetRowsReplaceTheCardSizeCategory`; `internal/datasets/pipeline` `TestAfterConvertRunsOncePerExport`; `cmd/hangar` `TestDatasetWiring` |
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
6. `compat/ollama` runs against the binary; Playwright has a real-Instance suite in CI; the Hub client suite has `binary` and `binary-xet` cells.
7. Requests done this round: W4 6 and 8, W8 2, 3, 5, 7 and 9, W10 4 and 10 (see [`RESOLUTIONS.md`](requests/RESOLUTIONS.md)). Found while doing them: the sign-in throttling message was a raw Go error, and the mock's wrong-password message differed from the server's.

## Open issues

- **W8 8, self-service profile and password changes.** A product decision for James: there is no `PATCH /me` or `POST /me/password`; admins send reset links.
- **W11 8 and W3, ownership of LFS and Xet objects at Commit time.** Deferred to the phase 3 security review, as the build plan says: a user who knows an object's SHA-256 can reference it from their own Repo.
- **git-upload-pack and unsafe files.** `scan.Gate` does not gate `git clone` without LFS, because git sends whole Commits. The Hub's default `.gitattributes` keeps every pickle format in LFS, and [Security](../operate/security.md) says so.
- **Scan errors are not blocked.** A file the scanner read but could not parse (status `error`) downloads with blocking on, because many `.bin` files are not pickles. The phase 3 review should confirm the trade-off.
- **A flaky Upstreams test.** `internal/api` `TestUpstreamsAPI/sqlite` failed once (`update_available` was true) in a full `-race` run on a heavily loaded machine and passed three times in a row afterwards. W6 should find the cause.
- **Not rerun in this round.** Helm on kind and the `quickstart` and `oidc-keycloak` Compose examples were not rerun after these fixes, and the new `binary` and `binary-xet` Hub cells and real-Instance Playwright step ran locally only, as there is no remote yet.
- **`/admin/system` does not report `upload_mode`.** It reports `download_mode`; adding `upload_mode` changes the OpenAPI file and is left to the owners.
