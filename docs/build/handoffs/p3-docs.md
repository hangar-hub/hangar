# Phase 3 handoff: docs walk-through

Branch `ws/p3-docs`, from `382cc88`. The step-by-step log is [docs-walkthrough.md](../docs-walkthrough.md).

## What I reviewed

- Every page under `docs/get-started` (binary, docker, compose, helm, first-login, hf-endpoint, first-pull) and `docs/guides` (all 15), followed as written against the local image, a GoReleaser snapshot, the Compose examples, the Helm chart on a throwaway kind cluster, and real clients: `huggingface_hub`, `hf`, `transformers`, `sentence-transformers`, `diffusers`, `datasets`, pandas, Polars, DuckDB, git and git-lfs, Ollama 0.31 and the `hangar` CLI. Upstream Pulls came from huggingface.co.
- `README.md`, the CLI help (`hangar --help` and the commands the guides use), and the landing page's example commands.
- House style across `docs/` with `make lint-style` and a read-through of the user-facing pages; `make docs` (mkdocs `--strict`).

## Findings

Severity is for a self-hosting user following the docs: **high** blocks a documented path, **medium** gives a wrong result or a misleading message on one, **low** is wording.

| # | Severity | Finding | Files | Fix | Test |
|---|---|---|---|---|---|
| 1 | High | `hangar push --branch <new>` uploaded every file, then failed with "revision not found", so the uploading guide's command never worked. The CLI test fake accepted Commits to missing branches, which hid it. | `internal/cli/push.go`, `api.go`, `repoid.go`, `clitest/` | `320939d`: create the branch from the default branch through the Hub API first, as `hf upload --revision` does; the fake now refuses Commits to missing branches. | `TestPushToANewBranch` |
| 2 | High | The first-pull guide's Repo cannot be found in Discover: huggingface.co search leaves `hf-internal-testing` out, by name and by ID. | `ui/src/features/discover/DiscoverPage.tsx`, `docs/get-started/first-pull.md` | `4f57f3f`, `5554cb5`: a search that is a full Repo ID offers **Open**; the guide searches by ID. | `repoIdQuery` tests in `ui/src/features/docs-agreement.test.ts` |
| 3 | High | The Helm guide read the generated password from `hangar-hangar-generated`; the chart names it `hangar-generated`. | `docs/get-started/helm.md`, `deploy/helm/hangar/values.yaml` | `1a0622e` | `TestChartGuideReadsTheGeneratedSecret` (offline, `make lint-deploy`); the kind suite passed on the same chart. |
| 4 | High | First sign-in said to change the password under **Settings**, which has no password change. | `docs/get-started/first-login.md` | `5554cb5`: use a reset link from **Admin**, **Users** (verified). | Docs only. |
| 5 | Medium | `hangar info` of a Repo that exists nowhere, and Discover, said "The Upstream rejected the credentials" (exit 4) to users with no Upstream token: huggingface.co answers an anonymous request for a missing Repo with a bare 401 and no `X-Error-Code`. | `internal/upstream/client.go` | `33e5dc7`: with no credentials sent, a 401 is not found, as `huggingface_hub` reads it. | `TestHTTPClientAnonymous401IsNotFound` |
| 6 | Medium | A dataset Mirror whose Parquet export was not pulled stayed failed after `hangar pull --parquet-only`: the convert Job counted failed Splits as done. | `internal/datasets/pipeline/convert.go` | `bb95881` | `TestMirrorExportPulledLater` |
| 7 | Medium | Dataset Mirrors reported a source size of 0 for every Split (`hangar info`, Viewer `/info` `download_size` and `/size`). | `internal/datasets/pipeline/convert.go` | `a690d1d` | `TestMirrorAdoptsUpstreamExport` (extended) |
| 8 | Medium | Saving gating in a Repo's **Settings** replaced the whole gating object, clearing request form fields set through the API and, with the message box empty, the message. The gated guide also implied the UI edits form fields. | `internal/api/repos.go`, `docs/guides/gated.md` | `8715a05`, `5554cb5` | `TestRepoLifecycleEndpoints` (extended) |
| 9 | Medium | The Ollama guide put key registration under "Private Repos", but with anonymous reads off (the default) Ollama needs the key for every Repo; `hangar ollama install` said "Public Repos still work" when the key was declined. | `docs/guides/ollama.md`, `internal/cli/ollama.go` | `5554cb5`, `23fd187` | `TestOllamaKeyRegisterAndInstall` (extended) |
| 10 | Medium | The Python guide's diffusers example (`tiny-stable-diffusion-pipe`) cannot load with diffusers 0.41, on huggingface.co either. | `docs/guides/python.md` | `5554cb5`: `tiny-stable-diffusion-torch`, loaded through Hangar. | Docs only. |
| 11 | Medium | The datasets guide's Polars example calls huggingface.co: Polars has its own `hf://` client that ignores `HF_ENDPOINT`. | `docs/guides/datasets.md` | `5554cb5`: read through `HfFileSystem` (verified). | Docs only. |
| 12 | Medium | The landing page's example commands named a Repo that does not exist on huggingface.co. | `ui/src/features/HomePage.tsx` | `4f57f3f` | `docs-agreement.test.ts` |
| 13 | Low | Token scope help in the UI predated the 2026-10-17 decision (write Tokens administer Repos). | `ui/src/features/settings/TokensPage.tsx`, `docs/spec/05-auth.md` | `4f57f3f`, `5554cb5` | `docs-agreement.test.ts` |
| 14 | Low | Docs that disagreed with the product: a not-pulled file is a 409, not `EntryNotFoundError`; `hf` keeps one active Token; no **Pull now** button; the **Create Token** label; a Mirror needs the Upstream's export pulled for the Viewer; the hf-endpoint example needs the Repo in the Arsenal; `HANGAR_CLI_CONFIG` and `HANGAR_CLI_NO_KEYCHAIN` were undocumented; the `ping` webhook event was missing; the HPA needs the metrics server. | `docs/` | `5554cb5`, `13dd831`, `1a0622e` | Docs only. |
| 15 | Low | `hangar --help` listed server commands under cobra's "Additional Commands", against the house style; every subcommand's help still used cobra's "Available Commands" and "Global Flags" after the first fix. | `cmd/hangar/main.go` | `73b7664`; round 3: a sentence-case usage template on the root, which every subcommand inherits | `TestCommands` (extended), `TestHelpHeadingsInSentenceCase` (walks every command) |
| 16 | Low | "just" for "only" in the README, `docs/index.md` and the datasets guide. | | `6bfb7e9` | `make lint-style` |

Spec and decision records: `docs/build/decisions.md` ("Decisions from the phase 3 docs walk-through"), `docs/spec/05-auth.md` (Token scopes), `docs/spec/09-web-ui.md` (Discover opens a Repo by ID).

## Residual risks

- **Ollama hides why a quant is missing.** Spec 08 answers a quant that is not in the Arsenal with 404 `MANIFEST_UNKNOWN` and a message naming the Pull to run, but Ollama 0.31 turns any manifest 404 into `file does not exist` and drops the message. Options for James: keep 404 and rely on the docs (done); answer 409 or 400 so Ollama prints the message (a spec 08 change, and Ollama then shows the raw JSON); or have `hangar ollama install` check the quant first and print `hangar pull` itself (CLI only). Not changed, because it needs a spec decision.
- **No self-service password change.** Users change a local password only through an admin's reset link (spec 05 and 09 list none under **Settings**). The docs now say so. Adding one is a product decision for James.
- **Gating form fields have no UI.** They are settable through the API only, and the Repo object does not return them, so the UI cannot show them; the message box also starts empty, so clearing a message from the UI is not possible. Within the spec ("gating" in 09), but thin.
- **Splits already stuck as not pulled.** Fix 6 makes the next convert Job adopt the export, but a Mirror whose Splits failed before the fix and whose export is already in the Arsenal gets no new Job until a new Pull or Commit: `hangar pull` of files already present starts nothing. The Viewer re-queues only pending Splits. This affects Instances that ran a build before `bb95881`, such as the bellatrix preview.
- **Source sizes on existing Mirrors.** Fix 7 applies to conversions after it; Mirrors converted before keep 0 until their next convert.
- **Partial-mirror HEAD answers 200.** A HEAD of a file that is not pulled answers 200 with its size, and the GET answers 409, so `has_file` and size checks treat the file as present. That is by design (planning and progress need sizes) but clients that HEAD then GET see a late failure.
- **Not covered.** vLLM (no GPU here), Homebrew (tap unpublished), the Windows and macOS archives, Argo CD itself (the kind suite stands in for its render-and-apply), HTTPS for Ollama and DuckDB, and the bellatrix-scale guides at 2 GiB and above (smaller Repos stood in).
- **Traefik in `scale.yaml`** answered one 504 while the web replicas started. It cleared once they were healthy; the Compose example has no Traefik health-based routing, so a request in that window can fail.

## Round 2 fixes

Branch `fix/p3-2-docs`, from `132da07`, for two audit findings in `docs/build/decisions.md`.

- **Broken link.** An open question linked `../spec/03-hub-api.md`; the file is `03-hf-api.md`. Nothing caught it because MkDocs builds neither `docs/build` nor `docs/spec`. `scripts/check-doc-links.py` now checks every relative Markdown link under `docs/` and in the root Markdown files, and `make lint-style` runs it. `TestDocLinkCheck` in `deploy/scripts_test.go` covers it, and the check fails on `decisions.md` as it was at `132da07`.
- **Heading case.** The section headings now say "Phase 3", as [15](../../spec/15-build-plan.md) does. Body text that names agents ("the phase 3 scan agent") is unchanged.

## Round 4 fixes

Branch `fix/p3-4-docs`, from `f16466b`, for two audit findings.

- **Threat model test citations.** Boundary 7 cited `TestHTTPClientRedactsPresignedURLs` at `internal/cli/redact_test.go`; it is in `internal/upstream/client_test.go`. `scripts/check-test-citations.py` now checks that every test or fuzz name the docs cite is defined in the test file cited after it, or in some test file when none is cited, and `make lint-style` runs it. It found one more: boundary 2 cited `TestPermissionMatrix` at `cmd/hangar/hub_admin_test.go`; it is in `internal/auth/authz_test.go`. Both now name their own files. `TestTestCitationCheck` in `deploy/scripts_test.go` covers the check.
- **Percent signs.** `docs/benchmarks.md` wrote "0.11 % against 0.1 %", and `CONTRIBUTING.md` "75 %" and "85 %". The Google style writes no space before the percent sign. `scripts/check-spelling.sh` now flags a number followed by a spaced percent sign in prose, outside code spans. `TestPercentSpacingCheck` covers it.
