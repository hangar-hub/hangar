# Release dry run for v1.0.0

The release dry run that Phase 4 of [`15-build-plan.md`](../spec/15-build-plan.md) asks for before James decides to release, run on 2026-10-18 from `main` at `9815b88` on branch `release/dry-run`. The release is on hold ([decisions](decisions.md), "Release on hold"), so nothing left the machine: no repository, push, tag, image, chart, Pages site or release-please run. Signing used a throwaway key with no transparency log.

**Note, 2026-10-18: the GitHub owner is now `hangar-hub`.** After this run, James moved the project's GitHub home to the `hangar-hub` organisation ([decisions](decisions.md), "GitHub organisation `hangar-hub`"). This record keeps the `jamesagarside` addresses the run saw, such as `github.com/jamesagarside/hangar` in the SBOMs and `ghcr.io/jamesagarside/hangar` in the checklist; the code, docs and workflows now use `github.com/hangar-hub/hangar`, `ghcr.io/hangar-hub/hangar` and `oci://ghcr.io/hangar-hub/charts/hangar`. [What James needs to do](#what-james-needs-to-do) is updated for the organisation.

## Result

Every packaging step that can run locally passed, after five fixes on this branch. The AWS S3 run was cut short by a slow home connection; see [AWS S3](#aws-s3). The v1.0.0 checklist in [`12-packaging-and-release.md`](../spec/12-packaging-and-release.md) has four items that pass and three that need James, because they need the public repository, CI runs on GitHub or a decision only he can make. No item fails.

| # | Checklist item | Status | Evidence |
|---|---|---|---|
| 1 | Every feature in 01 implemented, with its tests | PASS | All 73 feature IDs in 01 and 14 have a row in the [integration report](integration-report.md#feature-checklist) with a passing test. On this branch `make lint-go` passed and `make test-go` passed in every package but `deploy`, where `TestKindScriptFailsWhenTheImageCannotBeLoaded` exceeded its 30 second budget under load; with that fixed (`035ed17`) the `deploy` package passes. The "20 GB import" item under [Open issues](integration-report.md#open-issues) is stale: the report's summary records that the import passed on the preview at `382cc88` on 2026-10-17. |
| 2 | `ci.yml`, `compat.yml` and `fuzz.yml` green on `main` for amd64 and arm64 | NEEDS JAMES | There is no remote, so no workflow has run on GitHub. Their local equivalents passed at `a998d78` on arm64 ([final check](integration-report.md#phase-3-decisions-and-final-check-2026-10-18)); nothing ran on amd64 apart from the image below. `actionlint` 1.7.12 reports nothing and all 26 action refs exist (`scripts/check-action-refs.sh`). |
| 3 | Quickstart, every Compose example and the Helm chart work from a clean machine, following only the docs | NEEDS JAMES | The docs pull `ghcr.io/jamesagarside/hangar:latest` and `oci://ghcr.io/jamesagarside/charts/hangar`, which do not exist until the release. Local stand-ins pass: the [docs walk-through](docs-walkthrough.md), the four Compose examples and kind at `a998d78`, and in this run the image quickstart on both platforms and the binary quickstart (below). Recheck from a clean machine after the release. |
| 4 | Load test targets met on the reference setup, results in `docs/benchmarks.md` | NEEDS JAMES | [Benchmarks](../benchmarks.md) passes 9 targets and does not measure "100,000 Repos per Instance". The runs were on a shared laptop, not the reference setup, with 4 × 1 GiB uploads instead of 50 GiB. James decides whether this is enough for v1.0.0 or wants the missing target measured. |
| 5 | Security review done, threat model, findings fixed or recorded | PASS | [Threat model](../security/threat-model.md) written; Phase 3 fixed 61 findings and the threat model records every residual risk; James decided the open questions on 2026-10-18. In this run `govulncheck` v1.9.0 found no vulnerability that the code calls, and Trivy found no critical CVE in the image. Trivy found 5 high CVEs in `github.com/getkin/kin-openapi` v0.144.0 that the code never reaches (only the unused generated `GetSpec` loads a spec); the module is updated to v0.151.0 on this branch, see [Fixes](#fixes). |
| 6 | Docs complete, no TODO or placeholder pages, screenshots current | PASS, after a fix | `make docs` (`mkdocs build --strict`) builds 64 pages with no warning. No published page contains a TODO or placeholder text, and none links to the excluded `spec/`, `research/` or `build/` pages; the threat model and contributing pages link to the spec on GitHub on purpose (`9ca750d`). The screenshots were not current: the landing page changed after they were taken (`4f57f3f`). `pnpm run screenshots` regenerated all 30 (`aad25d7`). |
| 7 | James creates the public repository, enables Pages, private vulnerability reporting and the CLA app, then pushes the tag | NEEDS JAMES | Release steps; see [What James needs to do](#what-james-needs-to-do). |

## Fixes

| Commit | Change | Checked by |
|---|---|---|
| `d905096` | `.dockerignore` leaves out `dist/`, about 400 MB after a GoReleaser snapshot, which `docker build` sent as context | `make image-multiarch` and `make release-rehearsal` ran with it |
| `e32d4d6` | `release.yml` sets `org.opencontainers.image.licenses=Elastic-2.0` and the title in `docker/metadata-action`, which otherwise takes the licence GitHub detects. GitHub does not detect ELv2, so the label would read `NOASSERTION` and replace the Dockerfile's. The `docs` job now runs `make docs` before `mike deploy`, which does not build with `--strict` | `actionlint`, `scripts/check-workflow-uv.sh`; metadata-action's documented label override |
| `c4668be` | `github.com/getkin/kin-openapi` v0.144.0 to v0.151.0, clearing the 5 high CVEs Trivy reported | `make check-generate` (no change), `make lint-go`, `make test-go`; the rebuilt image has no critical or high finding in Trivy |
| `035ed17` | The kind script tests allow the script 2 minutes, not 30 seconds. With fakes it takes about a second, but 34 seconds during a full `make test` on a loaded host. The check guards against a 5 to 6 minute rollout wait, which 2 minutes still catches | `go test -race -count=3 -run '^TestKindScript' ./deploy/`, then the `deploy` package through `with-test-postgres.sh` and `with-test-minio.sh` |
| `aad25d7` | Regenerated screenshots | `pnpm run screenshots`: 30 passed |

## Binaries (GoReleaser)

`goreleaser check` passes (GoReleaser 2.18.3). `goreleaser release --snapshot --clean --skip=sign` builds every artefact. Signing is skipped here because the `signs` block signs keyless, which would write to the public transparency log; `make release-rehearsal` runs it with a throwaway key.

With no remote, GoReleaser cannot read the git state and falls back to version `0.0.0-SNAPSHOT-none`, commit `none` and build date `0001-01-01`. That only happens without a remote. To see what a release builds, the run gave git a remote URL through the environment (`GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=remote.origin.url GIT_CONFIG_VALUE_0=https://github.com/jamesagarside/hangar.git`, which changes no file and contacts nothing) and set `GORELEASER_CURRENT_TAG=v1.0.0`, as `release.yml` does. The binary then reports:

```text
hangar 1.0.0-SNAPSHOT-9815b88 (commit 9815b88, built 2026-10-18T11:36:58Z)
```

A release prints `hangar 1.0.0` with the same commit and date.

| Artefact | Size |
|---|---|
| `hangar_<version>_linux_amd64.tar.gz` | 18.3 MB |
| `hangar_<version>_linux_arm64.tar.gz` | 16.6 MB |
| `hangar_<version>_darwin_amd64.tar.gz` | 18.4 MB |
| `hangar_<version>_darwin_arm64.tar.gz` | 17.2 MB |
| `hangar_<version>_windows_amd64.zip` | 18.4 MB |
| `hangar_<version>_windows_arm64.zip` | 16.6 MB |
| `<archive>.spdx.json`, one per archive | SPDX 2.3, 129 packages (132 for Windows), `github.com/jamesagarside/hangar` among them |
| `checksums.txt` | 12 lines: the archives and their SBOMs |
| `checksums.txt.sigstore.json` | Written by `make release-rehearsal` only |
| `homebrew/Casks/hangar.rb` | Cask for macOS and Linux on both architectures, with download URLs under `releases/download/v1.0.0/` |

Each archive holds `hangar` (or `hangar.exe`), `LICENSE`, `NOTICE`, `README.md` and `CHANGELOG.md`. The binaries are static: ELF for Linux, Mach-O for macOS and PE32+ for Windows, each for the right architecture (`file`).

The macOS arm64 binary, from its archive, ran the [binary quickstart](../get-started/binary.md) against a temporary directory: it printed the generated admin password and Instance key once, wrote `secrets.yaml` with mode 0600, and answered `/healthz` with `{"status":"ok"}`, `/readyz` with `{"status":"ready"}` and `/` with the UI. `hangar healthcheck` exited 0.

## Container image

`make image-multiarch` (`VERSION=1.0.0`) passed on a throwaway `docker-container` builder in 13 minutes 33 seconds, then the same build with `--load` put both platforms in the local image store.

| | linux/amd64 | linux/arm64 |
|---|---|---|
| Compressed size | 47.2 MB | 43.7 MB |
| User | `65532:65532` | `65532:65532` |
| Entrypoint, command | `hangar`, `server` | `hangar`, `server` |
| Volume, port | `/var/lib/hangar`, 8080 | `/var/lib/hangar`, 8080 |
| Health check | `hangar healthcheck` | `hangar healthcheck` |
| `hangar version` | `hangar 1.0.0 (commit 9815b88, built 2026-10-18T11:43:58Z)` | the same |
| `git --version` | 2.56.0 | 2.56.0 |
| Shell | none (`sh`: executable file not found) | none |

Labels on both: `org.opencontainers.image.title`, `description`, `source`, `documentation`, `licenses=Elastic-2.0`, `version=1.0.0` and `revision=9815b88`.

Both platforms ran the [Docker quickstart](../get-started/docker.md) with the documented `--read-only --tmpfs /tmp --tmpfs /var/cache/hangar:uid=65532,gid=65532`: generated secrets printed once, `/healthz` and `/readyz` answered, the UI loaded and Docker reported the container `healthy`. amd64 ran under emulation on the arm64 host. `scripts/image-config-check.sh` passed. Trivy 0.73.0 with the release gate's settings (`--severity CRITICAL --ignore-unfixed`) exits 0 on both platforms.

The rootfs stage logs `ERROR: ldconfig-commit.sh: script exited with error 127` from `apk add --root`: the trigger needs a shell, which the root filesystem has none of. The build succeeds, git runs, and every earlier image has done the same.

## Signing and SBOMs (`make release-rehearsal`)

Passed with cosign 3.1.3, syft 1.54.1 and yq, with the same remote URL and tag as above:

```text
Verified OK
ok: checksums.txt verifies against its Sigstore bundle
ok: a changed checksums.txt does not verify
hangar_1.0.0-SNAPSHOT-9815b88_<os>_<arch>.<ext>: OK                (12 files)
ok: SPDX SBOM of hangar_1.0.0-SNAPSHOT-9815b88_<os>_<arch>.<ext> lists 129 packages (132 for Windows), github.com/jamesagarside/hangar among them
ok: the image signature verifies
ok: the image does not verify against another key
ok: SPDX SBOM of the image lists 158 packages, github.com/jamesagarside/hangar among them
ok: the SPDX SBOM attestation verifies
```

The rehearsal verifies with the throwaway key. The commands in [Security hardening](../operate/security.md#verify-the-image) verify keyless signatures instead, with `--certificate-identity-regexp '^https://github.com/jamesagarside/hangar/.github/workflows/release.yml@refs/(heads/main|tags/v.+)$'`. That matches both ways `release.yml` runs: called from `release-please.yml` on `main`, where the certificate names the called workflow at `refs/heads/main`, and from a pushed tag. Keyless signing can only be proven in the first release.

## Helm chart

`helm package deploy/helm/hangar --version 1.0.0 --app-version 1.0.0`, as `release.yml` runs it, gives `hangar-1.0.0.tgz` (10.2 KB) with `version: 1.0.0` and `appVersion: 1.0.0`. `Chart.yaml` in the repository keeps `0.0.0` on purpose; the workflow sets both from the tag. `helm lint` on the package passes with the CI values and both examples. `helm template` of the package renders `ghcr.io/jamesagarside/hangar:1.0.0` in every case, and kubeconform 0.7.0 (strict, Kubernetes 1.33) finds every resource valid:

| Values | Resources |
|---|---|
| `ci/default-values.yaml` | 6 |
| `examples/gitops-values.yaml` | 6 |
| `examples/scale-values.yaml` | 16, the migration Job among them |
| `examples/scale-values.yaml` with `migrations.mode=initContainer` | 14 |

## Documentation site

`make docs` passed. Spot checks of the built site: Home, Get started and its Docker, Compose, Helm and binary pages, the threat model and the benchmarks page all render with their headings. The site has no `spec/`, `research/` or `build/` directory, and the only links into those trees are the GitHub links above. The words "open source" appear only on the licence page, which says ELv2 is not an OSI-approved open source licence.

## Release automation

Read, not run.

- **release-please produces 1.0.0.** The manifest is `{".": "0.0.0"}` and the config sets `initial-version: 1.0.0`. In release-please 17.11.2 (`build/src/manifest.js`), a manifest version of `0.0.0` is not used as a previous release, so with no earlier release the version is `initialReleaseVersion()`, which returns `initial-version` (`build/src/strategies/base.js`). The tag is `v1.0.0`, since `include-component-in-tag` is off and `include-v-in-tag` defaults to on. Every key in the config is in the release-please schema.
- **The workflow runs what this dry run ran.** `binaries`: GoReleaser `release --clean` with cosign 3.0.6 and syft. `image`: buildx for both platforms with push, provenance and buildx SBOM, then `cosign sign` keyless, an SPDX SBOM from syft, `cosign attest --type spdxjson` and `actions/attest-build-provenance`, matching the rehearsal's sign, SBOM and attest steps. `scan`: Trivy as above. `chart`: `helm package` with the tag's version, `helm push` to `oci://ghcr.io/jamesagarside/charts` and `cosign sign` of the chart digest. `docs`: `mike deploy` of `1.0` with the alias `latest`.
- **Action refs.** `actionlint` passes, and all 26 refs exist. They are pinned to release tags, mostly major tags such as `actions/checkout@v7` and `anchore/sbom-action@v0`, not to commit SHAs. Tags can be moved, and the jobs that use them hold `id-token: write` and `packages: write`; pinning to SHAs, which Dependabot keeps current, is a choice for James.

## AWS S3

James approved the run with his `elastic-sa` profile. Each run used a new private bucket `hangar-s3-conformance-<timestamp>` in `eu-west-2` with the public access block on, and a trap that deleted every object, aborted incomplete uploads and deleted the bucket whether the tests passed or not. All three buckets were deleted, and `aws s3api list-buckets` lists no `hangar-s3-conformance` bucket afterwards. The command is the one in the "AWS S3" section of [`s3-presigned-checksum.md`](../research/s3-presigned-checksum.md), at `9815b88`, which has the LFS reference and upload changes made since the last run on 2026-10-15.

The result is incomplete: the home connection to AWS was far slower than on 2026-10-15. `netstat` showed the test's connection to S3 draining its send queue at about 3 to 5 KB/s, with bursts of a few hundred KB/s.

| Run | Tests | Result |
|---|---|---|
| 1, whole package, 90 minute timeout | The fault-injection tests, and 26 of the 27 conformance subtests that ran, among them `Multipart` (822 s), `PutVerifiedMultipart` (3,448 s), `MultipartPartRetryWithNewContent`, `ConcurrentPuts` (recovered from `503 SlowDown`), `PresignGetHTTP`, `PresignPutHTTP` and `PresignedDownloadsAreAttachments` | Passed |
| 1 | `PresignPartHTTP` | Failed: the test's own HTTP client has a 1 minute timeout, and the 5 MiB part did not finish uploading in it (`context deadline exceeded (Client.Timeout exceeded while awaiting headers)`) |
| 1 | `TestPutUnknownSizeStreamsInParts` | Still running at the 90 minute timeout |
| 2, the tests run 1 did not reach | `TestPutUnknownSizeStreamsInParts` | Failed after 6 attempts at one part, each cancelled by the 2 minute stall timeout while the send queue drained at a few KB/s. Stopped during `TestPutKnownSizeOverSinglePutLimit`, which was stalling the same way |
| 3, the tests that move little data | `TestCheck`, `TestStoredContentTypeIsPinned`, `TestRangedGetReportsWholeSize`, `TestPresignPutLimit`, `TestNewValidatesOptions`, `TestCopyRefusesASourceThatChanges`, `TestCopyMultiGigabyteObjects` | Passed |

Not run on AWS: `TestStreamsLargeObjects` and `TestCopyLargeObjectsInParts`, which upload 48 MiB and 10 MiB. `TestPartRetriesOnMinIO` skips on AWS by design.

Nothing here points at Hangar: every failure was an upload that could not finish over the connection, and the same tests pass on MinIO in `make test`. On a link this slow, the store's stall timeout cancels a part that is still moving, because the kernel's send queue (about 550 KB here) takes more than 2 minutes to drain once the body has been read; `storage.s3.stall_timeout` can be raised for such links. Rerun the whole package from a faster connection before the release.

## What James needs to do

Before the release:

1. Create the public repository `github.com/hangar-hub/hangar` in the `hangar-hub` organisation and push `main`.
2. Enable GitHub Pages from the `gh-pages` branch (the first `docs.yml` or release run creates it), private vulnerability reporting and the CLA Assistant app.
3. Give the release GitHub App access to the repository. Updated 2026-10-18: the workflows use the organisation's app `hangar-hub-release` instead of tokens, and James has already set it up, with the organisation Actions variable `RELEASE_APP_ID`, the secret `RELEASE_APP_PRIVATE_KEY` and the tap `hangar-hub/homebrew-tap`. At release, add the `hangar` repository to the app's installation and to the selected repositories of the variable and the secret. Without them release-please cannot open its pull request and GoReleaser cannot publish the cask.
4. Decide whether item 4 stands as it is, or whether 100,000 Repos per Instance must be measured first.
5. ~~Decide whether to pin the actions in `release.yml` to commit SHAs.~~ Decided 2026-10-18: every workflow pins every action to a commit SHA ([decisions](decisions.md), "GitHub organisation `hangar-hub`").
6. Rerun the AWS S3 suite from a faster connection, or approve an agent doing so (see [AWS S3](#aws-s3)).
7. Check LM Studio by hand against a real server; no suite covers it ([integration report](integration-report.md#gaps-and-decisions), "Not covered in Phase 3").
8. Give the go-ahead, then merge the release-please pull request for `v1.0.0`.

After the release:

1. Watch the first CI, compat and fuzz runs on GitHub, on both architectures (item 2).
2. Make the GHCR packages `hangar` and `charts/hangar` public if they are not, and check that an anonymous `docker pull ghcr.io/hangar-hub/hangar:1.0.0` and `helm pull oci://ghcr.io/hangar-hub/charts/hangar --version 1.0.0` work.
3. Run the keyless verification commands in [Security hardening](../operate/security.md#verify-the-image) against the published image and `checksums.txt`.
4. Follow the quickstart, a Compose example and the Helm page from a clean machine (item 3).
