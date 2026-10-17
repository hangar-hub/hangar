# Integration report

Phase 2 step 5 of [`15-build-plan.md`](../spec/15-build-plan.md): every feature ID in [`01-product.md`](../spec/01-product.md) and [`14-datasets.md`](../spec/14-datasets.md) with the test that proves it, the Hub API contract MUSTs, the suite results, the request resolutions and the open issues.

It describes branch `integration/v1` at `195e3a2` (`merge: fix/r7-w9`). Every suite in [Suite results](#suite-results) ran in round 8 on detached worktrees at that commit. Phase 2 is **not finished**: the suites are green, but a user's 20 GB import failed on the preview Instance, which runs an older build (see [Open issues](#open-issues)). `main` is unchanged.

## Summary

- Every feature ID has at least one named test, and every test named here exists in the tree (checked in the round 8 audit).
- Every MUST in section 13 of the Hub API contract has a handler and a test. None is unimplemented.
- Round 8 results: unit, lint, coverage and fuzz green; Hub client suites green on SQLite and Postgres, with and without Xet uploads, on the latest and the floor clients; tool suites (Ollama, LM Studio, Viewer, Upstreams) green; real-Instance Playwright 195 of 195, three times; Compose and Helm smoke green.
- 108 request items have a resolution; 3 are deferred, 2 of them to James.
- AWS S3 passed in full on 2026-10-15 after the fixes for the 2026-10-14 failures.
- Red: the preview Instance's import failure (high), which needs a redeploy and a rerun by James. This report was stale before this update; it now covers every merge.

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

Every row passed in round 8 unless its result says otherwise. "unit" is `make test`; the other names are the suites above. "Mock" rows rest on the `ui/e2e` mock suite, which last ran during the `5975bec` checks.

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
| UP-3 | Real-Instance browser upload flow; mock flows for parts and resume after reload | Passed: real Instance. Mock: passed at `5975bec` |
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
| BROWSE-1 | `internal/repo` list and facet tests; mock Arsenal flows | Passed: unit. Mock: passed at `5975bec` |
| BROWSE-2 | `internal/upstream` discover tests; real-Instance Discover flow | Passed: unit, real Instance |
| BROWSE-3 | Real-Instance browsing flow and 51 page checks at three widths | Passed: real Instance |
| BROWSE-4 | Real-Instance safetensors header and GGUF viewer flows; `internal/api` `TestFileScan` | Passed: real Instance, unit |
| BROWSE-5 | `internal/collections`; `compat/hf` `test_collections`; real-Instance Collection keyboard reorder flow | Passed: unit, Hub clients, real Instance |
| BROWSE-6 | Mock keyboard search flow | Mock: passed at `5975bec`. Not covered by a round 8 suite |
| AUTH-1 | `internal/auth`; `internal/api` `TestLoginThrottleMessage`; real-Instance sign-in flow | Passed: unit, real Instance |
| AUTH-2 | `internal/auth` OIDC tests; Compose `oidc-keycloak` smoke | Passed: unit, Compose |
| AUTH-3 | `internal/auth/login` tests | Passed: unit |
| AUTH-4 | `internal/auth` Token tests; real-Instance Token flow | Passed: unit, real Instance |
| AUTH-5 | `internal/org` on both drivers; `internal/api` `TestAdminEndpoints`; real-Instance Organisations flow | Passed: unit, real Instance |
| AUTH-6 | `internal/auth/bootstrap`; `cmd/hangar` admin command tests; Compose `quickstart` with a generated admin password | Passed: unit, Compose |
| AUTH-7 | `internal/auth` Ollama key tests; `compat/ollama` `private_Repo_with_a_registered_key` | Passed: unit, Ollama |
| META-1 | `internal/index` extract and fuzz tests | Passed: unit, fuzz |
| META-2 | `internal/scan`; `cmd/hangar` `TestUnsafeDownloadGateCoversEveryRoute`, `TestServingSurfaceIsClassified`, `TestFileByteReadersAreClassified`; real-Instance unsafe pickle flow | Passed: unit, fuzz, real Instance |
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
| W8 Web UI | 9 | 1: request 8, self-service profile and password (a product decision for James) |
| W9 CLI, packaging and docs | 12 | 1: request 11, release tasks (James) |
| W10 Datasets | 10 | 0 |
| W11 Xet | 9 | 1: request 8, Commit-time ownership of LFS and Xet objects (phase 3 security review) |

The round 8 audit spot-checked the wiring items in `cmd/hangar` (`GCSchedule`, `ReindexStale`, `RescanStale`, `RecordQueueGauge`, `ConnectSrc`, `CleanupSchedule`, `NewLimiter`, `SyncConfig`, `AfterIndex`, `EnqueueConvert`, `OpenBlobStore`, `PullThroughPath`, `ExportPath`, `ExemptTokenEndpoint`, `gc.Register`, `InstanceLimits`, `ParseWindows`) and found all of them.

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

- **A 20 GB import fails on the preview Instance (high, needs James).** `hangar import` of `Qwen/Qwen2.5-Coder-32B-Instruct-GGUF` failed with HTTP 500 on `qwen2.5-coder-32b-instruct-q4_k_m.gguf`. The preview's log shows upload completion failing while reading the assembled object back from Aquila's MinIO: `repo: upload lfs part: upload part 65: unexpected EOF` at 14:55:49Z on 2026-10-16, then part 66 at 15:23:27Z, and part 66 once for `lmstudio-community/Qwen3.8-27B-MLX-4bit` (a later attempt succeeded). The CLI's message names no request ID, so it was built before `1c40413`, and the preview runs image `preview-5975bec`. Both sides are fixed on this branch: the server resumes a broken read with ranged reads, keeps the upload and its parts and answers 503 (`fix/r4-w2`), and the CLI sends the completion again after that 503 (`071ee7d`). `TestCompleteUploadResumesABrokenRead`, `TestCompleteUploadKeepsPartsWhenTheReadFails`, `TestResumingReader` and `TestImportResendsCompletionAfter503WithRetryAfter` pass with `-race -count=3`. No round 8 suite moves a multi-GB file, so the fix is not proven end to end. To close it, James rebuilds the preview from `integration/v1`, installs the CLI from the same commit and reruns the import. Why the MinIO read drops at about the same offset each time is not known; the proxy in front of Aquila's MinIO is worth checking if the rerun still logs retries.

### Gaps and decisions

- **W8 8, self-service profile and password changes.** A product decision for James: there is no `PATCH /me` or `POST /me/password`; admins send reset links.
- **W11 8 and W3, ownership of LFS and Xet objects at Commit time.** Deferred to the phase 3 security review: a user who knows an object's SHA-256 can reference it from their own Repo.
- **git-upload-pack and unsafe files.** `scan.Gate` does not gate `git clone` without LFS, because git sends whole Commits. The Hub's default `.gitattributes` keeps every pickle format in LFS, and [Security](../operate/security.md) says so.
- **Scan errors are not blocked.** A file the scanner read but could not parse (status `error`) downloads with blocking on, because many `.bin` files are not pickles. The phase 3 review should confirm the trade-off.
- **`/admin/system` does not report `upload_mode`.** It reports `download_mode`; adding `upload_mode` changes the OpenAPI file and is left to the owners.
- **Release on hold.** James decided on 2026-10-16 that nothing is published until he says go ([decisions](decisions.md), "Release on hold"). Work stops after the release dry run.
- **Release not exercised.** Multi-arch image publishing, cosign signing and the SBOM run only in `release.yml`; `make release-rehearsal` was not part of round 8, and the first real run is James's release (W9 11).
- **Local-only checks.** There is no remote, so every result here comes from the development machine, not CI.
- **Not covered in round 8.** BROWSE-6 and the mock flows for UP-3 and BROWSE-1 last ran in the `ui/e2e` mock suite during the `5975bec` checks. The `diffusers` cell and the 6 GiB file are nightly.
- **`govulncheck` binary.** The installed `govulncheck` is built with an older Go and cannot load this module; CI's `go run ...@v1.9.0` form works. This is a local tool problem.
