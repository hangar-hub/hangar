# Request resolutions

How the integration lead resolved every item in `requests/*.md` in phase 2, on branch `integration/v1`. Each line gives the request, the decision and where the change is. "Applied" means the change is on the branch; "accepted" means the decision or edit the workstream made stands; "deferred" means it is not done in phase 2, with the reason.

## W1: Platform

| Item | Resolution |
|---|---|
| 1. `HANGAR_TEST_MINIO` in `make test` | Applied (merged; W9 made the same edit). |
| 2. `docs/research/s3-presigned-checksum.md` | Accepted. |
| 3. `internal/gc` reads and writes other packages' tables | Accepted. 02's rule that packages never touch each other's tables is relaxed for `internal/gc`, under the contract in the request and the `internal/gc` package doc. |
| 4. Composing the native API handlers | Applied in `cmd/hangar/native.go`: one struct embeds every workstream's handler type with no `gen.Unimplemented` fallback, so a missing operation fails to compile. |
| 5. `HANGAR_TEST_MINIO` in CI | Applied (merged). |
| 6. `jobs.repo_id` | Accepted in the initial migration (no released database exists). Every Repo-scoped enqueue now sets `NewJob.Repo`: `index`, `scan`, `git.compact`, `repo.purge`, `zip.export` (W2), `pull` when the Repo exists, `pull.file`, the index and scan Jobs a Pull queues (W6), `ReindexStale` and `RescanStale` (W7), `dataset.convert` and `dataset.stats` (W10), and `hangar admin reindex`. A Pull that creates its Mirror and scheduled `mirror.sync` runs stay Instance-wide, visible to their creator and unrestricted Instance admins. |

## W2: Repo core

| Item | Resolution |
|---|---|
| 1. Server-side Commits with the git binary | Accepted. 04-git.md's decision table and "Server-side Commits" step now say so; `docs/dependencies.md` already did (W9). |
| 2. Additions to `repo.Service`, `LFSStore` and `ListOptions` | Accepted. |
| 3. Read-only joins on `namespaces` and `collaborators` | Accepted. |
| 4. `SearchNamespaces` | Applied: `org.DBService.SearchNamespaces` (case-insensitive substring of name or display name, name order, never the system user), tested on both drivers. |
| 5. Zip export route | Applied: `api.ExportHandler` is mounted at `api.ExportPath` in front of authentication (signed URLs); no OpenAPI operation. |
| 6. No body limit on upload parts | Applied in `cmd/hangar/router.go`. |
| 7. Hooks | Applied: W7 decodes `CommitJob`; `repoQuota` adapts W1's `gc.Accounting` (a refusal is a `*repo.PolicyError`, so every protocol answers with its own refusal and W1's message); `api.RepoDeps.MirrorStatus` fills `mirror_status` from W6. `gitstore.Sweep` is left to `git.compact`; W1's `gc` collects the same packs. |
| 8. Test helper clash | Applied: W1's `env` and `newEnv` in `internal/api` are now `platformEnv` and `newPlatformEnv`. |
| 9. `repotest.Fake` changes | Accepted. |

## W3: git server

| Item | Resolution |
|---|---|
| 1. Mount the git server | Applied. |
| 2. Classify before authentication | Applied: `auth.WriteAuthError` sets the protocol before it writes the 401, so git gets its Basic challenge (checked in `cmd/hangar` tests). |
| 3. go-billy direct | Applied by `go mod tidy`; the dependencies line exists. |
| 4. Quota and Pull-through | Applied: `repoQuota` and W6's `PullThrough`. |
| 5. git 2.39 in CI | Applied (W9's `git-239` job). |
| 6. go-git `HasEncodedObject` workaround | Accepted; remove it when go-git is upgraded past the fix. `git push --atomic` is now documented in `docs/guides/git.md`. |

## W4: Hub API

| Item | Resolution |
|---|---|
| 1. Mount the Hub API | Applied with every real dependency. |
| 2. Classify before authentication | Applied (as W3 item 2). |
| 3. Python dependencies | Accepted; `uv lock --check` passes on the merged `compat/pyproject.toml`. |
| 4. `go.yaml.in/yaml/v3` | Applied by `go mod tidy`; the dependencies lines are merged into one. |
| 5. Repo events from the Repo service | Accepted: `repo.Store` gets `Audit` and `Webhooks`. |
| 6. Likers, liked-by, comment edit and hide | Applied in phase 2 round 1: `GET .../likers`, `GET /api/users/{user}/likes` and `POST .../comment/{id}/edit\|hide` over W7's methods (`internal/hfapi/likes.go`), tested in `internal/hfapi`, `cmd/hangar` and `compat/hf`. |
| 7. Fake workarounds | Accepted; W2 item 9 removed the differences. |
| 8. Client matrix CI | Applied (W9). Phase 2 round 1 added the `hub-binary` job: `HANGAR_HF_INSTANCE=binary` runs the suite against the wired binary on SQLite or Postgres, seeded through the native API, and its Xet cells add `HANGAR_HF_XET_UPLOADS=1`. |
| 9. `hangar env` | Applied (W9). |
| 10. Matrix data file | Applied (W9's `make generate`). |
| 11. Undo a failed `create_pr` | Applied: `closeFailedPR` now calls `community.Service.Discard`, and closes the pull request with a comment only if it cannot be discarded. |

## W5: Identity and access

| Item | Resolution |
|---|---|
| 1. Extra `internal/api` files | Accepted. |
| 2. Composing handlers | Applied (W1 item 4). |
| 3. Real Authenticator | Applied, wrapped in `ollama.ExemptTokenEndpoint`, with `auth.WriteAuthError` as `onError`. |
| 4. Bootstrap on start | Applied in every role that serves HTTP, after the schema is current. |
| 5. Upstream lookup | Applied: `*upstream.Registry` is `auth.Options.Upstreams`. |
| 6. Cleanup Job | Applied: new kind `auth.cleanup`, `auth.Service.RegisterJobs` and an hourly schedule (`auth.CleanupSchedule`). |
| 7. Key rotation | Applied (W9's `hangar admin rotate-key`; purposes match W5, W6 and W7). |
| 8. Browser sign-in endpoints | Decided: they stay in the `/auth/` protocol handler, not the OpenAPI file. |
| 9. Common password licence | Applied (W9's `NOTICE`). |
| 10. Keycloak image | Nothing to do. |

## W6: Upstreams and Pulls

| Item | Resolution |
|---|---|
| 1. Wiring | Applied, including `PullThrough().Handler()` at `upstream.PullThroughPath` outside authentication and the body limit, and the W4 adapter (`hubPullThrough`, which also turns a quota refusal into a 403 with W1's message). |
| 2. Settings adapter | Applied (`pullSettings`). |
| 3. Bandwidth budget from the Instance setting | Applied. |
| 4. Dependencies after every index | Applied through `indexdb.Options.AfterIndex`. |
| 5. `GET .../dependencies` in `discover.go` | Accepted. |
| 6. History-only Pull | Applied: the `Selection` description in `api/openapi.yaml` says so. |
| 7. Who may start a Pull | Confirmed as decided. |
| 8. Mirror visibility | Confirmed as decided. |
| 9. `index.GroupQuants` | Accepted. |
| 10. `go.yaml.in/yaml/v3` | Applied by `go mod tidy`. |
| 11. `upJobResource` | Accepted as is; switching `GetPull` to `api.JobResource` is a refactor for later. |
| 12. CI | Accepted (W9's jobs). `compat/upstream/real_test.go` now builds with the `compat` tag alone. |
| 13. Quotas on Pulls | Applied (`pullQuota`). |

## W7: Metadata, Ollama and community

| Item | Resolution |
|---|---|
| 1. Files outside the listed paths | Accepted. |
| 2. `indexdb` and `communitydb` subpackages | Accepted. The Repo service is bound to them after both exist (`boundIndexer`, `boundCommunity` in `cmd/hangar/instance.go`). |
| 3. Ollama token endpoint | Applied. |
| 4. Token URL from the request host | Accepted. |
| 5. Read-only joins | Accepted. |
| 6. `community.Service` additions | Accepted; W4 now uses `Discard`. |
| 7. `FileType *int` | Accepted. |
| 8. Own GGUF header reader | Accepted; 02's stack table and `docs/dependencies.md` updated. |
| 9. Direct imports | Applied by `go mod tidy`. |
| 10. Vendored Ollama templates | Applied; `NOTICE` now names `internal/ollama/third_party/ollama/`. |
| 11. Settings, Pull-through and dataset hooks | Applied: `webhooks.allow_private_targets` from the Instance settings; `upstream.PullThrough.PullFiles` (new, tested) serves the Ollama registry; `Layout` and `AfterIndex` wired; W6 already queues index and scan after a Pull. |
| 12. Key rotation | Applied (W9). |

## W8: Web UI

| Item | Resolution |
|---|---|
| 1. `ui.external_images` for every user | Applied: `ui_external_images` on `AuthProviders` (`GET /auth/providers`), read by `useExternalImages`. |
| 2. Pickle scan findings per file | Applied in phase 2 round 1: `GET .../files/{rev}/{path}/scan`; the file page lists the imports. |
| 3. List every Organisation | Applied in phase 2 round 1 as `GET /admin/orgs` (Instance admins); the admin page lists them. |
| 4. Header JSON shape | Applied: GGUF metadata values in `GET .../header` are plain values, with long arrays as `{"type": "array", "length": N}`. |
| 5. Repo IDs in Job payloads | Applied: the Jobs API adds the Repo's current ID as `payload.repo` to every Repo-scoped Job whose payload does not name it (`mirror.sync`, `dataset.convert`, `index`, `scan` and the rest). |
| 6. `/filter` without `where` | Already works: an empty `where` matches every row. |
| 7. Pull request compare | Applied in phase 2 round 1: `repo.Service.CompareFile` and `GET .../discussions/{num}/diff`, from the merge base to the head of the ref; the Files changed tab uses it. |
| 8. Self-service profile and password | Deferred: a product decision for James. |
| 9. Resume an upload from the plan | Applied in phase 2 round 1: the plan passes `UploadParams.Resume`, which continues the caller's unexpired upload of the same object, Repo and part size. |

## W9: CLI, packaging and docs

| Item | Resolution |
|---|---|
| 1. `HANGAR_CLI_*` | Applied (merged). |
| 2. Client commands in the binary | Applied (merged). |
| 3. Blob store opener | Applied: `backends.Open` in `cli.Options.Admin.OpenBlobStore`. `hangar admin doctor` keeps its own checks. |
| 4. Protocol label | Applied (merged). |
| 5. Import server behaviour | Applied: `paths: []` plans history only (W6). Uploads on a Mirror store the objects; the CLI now pulls the uploaded paths into the Mirror afterwards, which marks them present without a transfer and within the quota, as W6's importer does. |
| 6. Secret purposes | Confirmed: W6 and W7 seal with `upstream_instance_token` and `webhook_secret`. |
| 7. `create-user`, `reset-password`, `gc`, `reindex` | Applied in `cmd/hangar/admin_ops.go`, tested. |
| 8. Device-code login | Confirmed. |
| 9. Files W9 reads | README screenshots applied from `docs/assets/screenshots/`. |
| 10. `Makefile` and `go.mod` | Applied. |
| 11. Release tasks | For James. |
| 12. `--json` on server commands | Accepted; the new admin commands take it too. |

## W10: Datasets

| Item | Resolution |
|---|---|
| 1. Interface changes | Accepted. |
| 2. Wiring | Applied; the Viewer and pipeline exist only with `viewer.enabled`. |
| 3. Triggers | Applied: `dataset.convert` after the index of a dataset's default-branch head; user Commits to `refs/convert/*` are refused (`repo.Store`, so the Hub API and native API too; pushes were already refused). |
| 4. DATA-9 and DATA-11 summaries | Applied: the index gets each Config and its Split count, formats, modalities, tasks, languages and pretty name from the files and card, and, once the Parquet export exists, the rows and the size category they give (`indexdb.Options.DatasetRows`, re-indexed by the pipeline's `AfterConvert`). `repo.List` filters on them. |
| 5. CLI | Applied (W9). |
| 6. UI | Accepted. |
| 7. DuckDB note | Applied (W9's `docs/guides/datasets.md`). |
| 8. Dependencies | Applied by `go mod tidy`. |
| 9. Fuzz minimisation cap | Applied (merged with W9's file-descriptor fix). |
| 10. `viewer.max_request_bytes` | Applied in phase 2 round 1, default 300 MB. |

## W11: Xet

| Item | Resolution |
|---|---|
| 1. Wiring | Applied. |
| 2. No Hub authentication on CAS requests | Applied: requests classified as Xet skip the Authenticator; the CAS handler verifies its tokens. |
| 3. Own CAS hostname | Applied: requests for `xet.cas_url`'s host go to the CAS handler. |
| 4. Streaming | Applied: no body limit on Xet routes and no write timeout on the server. |
| 5. Research note corrections | Applied to `docs/research/xet-upload-research.md`. |
| 6. Reject-mode error in the docs | Applied (W9's `docs/guides/uploading.md`). |
| 7. Xet CI | Applied: `xet` job in `compat.yml` (hf_xet 1.6.0 and 1.7.0, fakes and real backend, reference xorb). |
| 8. Commit-time ownership of LFS objects | Deferred to the phase 3 security review, with W3's equivalent known gap. |
| 9. `xet.max_file_size` | Accepted. |
