# Docs walk-through, phase 3

The phase 3 docs agent followed every page under `docs/get-started` and `docs/guides` on 2026-10-17, in order, from `382cc88`. Each row is a step as the page gives it, what happened, and the fix with its commit on `ws/p3-docs`.

## Set-up and stand-ins

Nothing is published yet ([decisions](decisions.md), "Release on hold"), so local equivalents stood in. The docs keep the published names.

| Published thing | Stand-in |
|---|---|
| `ghcr.io/jamesagarside/hangar:latest` | `make image IMAGE=hangar:p3docs`, rebuilt after each product fix |
| Release archives and `checksums.txt` | `goreleaser release --snapshot --clean --skip=sign,sbom,publish`; the `linux_arm64` archive in a clean `debian:trixie` container |
| `go install ...@latest` | `go install ./cmd/hangar` from `git archive HEAD`, the files a module zip holds |
| Homebrew tap | The cask GoReleaser renders into `dist/homebrew`; not installed |
| `git clone https://github.com/jamesagarside/hangar.git` | `git clone` of the worktree |
| `oci://ghcr.io/jamesagarside/charts/hangar` | `deploy/helm/hangar` with `image.repository=hangar`, `image.tag=p3docs`, on the kind cluster `hangar-p3docs` (deleted afterwards) |
| AWS S3 and IRSA in the Helm scale-out example | Postgres and MinIO in the cluster, from `deploy/kind_test.go` |
| `hangar.example.com` | `localhost:8080`, plain HTTP |
| A browser | Playwright for the UI steps; the device page was also approved over the API from inside the binary container |

Clients ran with their state kept out of the host user's: `HOME` in a scratch folder, `HANGAR_CLI_NO_KEYCHAIN=1`, an Ollama server on its own port and models folder, `--lmstudio-dir` resolution under the scratch `HOME`, and `git` with system configuration off. Python clients came from `compat/` (`huggingface_hub` 2.2.0, `transformers` 5.19.0, `datasets` 5.1.0, `sentence-transformers` 5.7.0), with `diffusers` 0.41.0, pandas, Polars 2.0.0 and DuckDB added per run. Pulls from huggingface.co used small Repos where a guide names a large one: `bartowski/SmolLM2-135M-Instruct-GGUF:Q4_K_M` (100 MiB) for `bartowski/Llama-3.2-3B-Instruct-GGUF:Q4_K_M` (2 GiB), because the home connection is about 40 Mbit/s.

Docker Hub did not resolve from Docker Desktop during the run; `debian:trixie` came through the Harbor pull-through cache and was tagged locally.

## Get started

| Page | Step | Result | Fix |
|---|---|---|---|
| binary | Release archive: `sha256sum --ignore-missing -c checksums.txt`, `tar xzf ... hangar` | Works. A clean Debian image has no `curl` or `sudo`; the page is for a normal machine, so left as it is. | |
| binary | `go install` | Builds without the UI, as the page says. `go.mod` has no `replace`, so `@latest` will work once published. | |
| binary | `hangar server` with the four `--set` flags | Starts, creates `$HOME/hangar`, prints the generated password and Instance key once. | |
| binary | `hangar login --url ...` | Device code printed, approved, Token saved in `config.yaml` (no keychain in the container). | |
| binary | `hangar pull hf-internal-testing/tiny-random-bert` | Pulled and downloaded 10 files into the cache. | |
| docker | `docker run` quickstart, `docker logs hangar` | Output matches the page. Healthcheck goes healthy. | |
| docker | Read-only root filesystem variant | Works. | |
| docker | Configuration file at `/etc/hangar/hangar.yaml` | Works with `server.public_url` in the file. | |
| compose | Clone, create `.env` with `umask 077` and `set -C` | Mode 0600; a second run refuses to overwrite; `.env` is ignored by git. | |
| compose | `quickstart.yaml` | Ready; sign-in with `HANGAR_ADMIN_PASSWORD` works. | |
| compose | `postgres-minio.yaml` | Ready; MinIO console on 127.0.0.1:9001; a Pull and download through proxy mode work. | |
| compose | `scale.yaml` | Two web and two worker replicas, migrate ran, Traefik dashboard answers, the collector prints spans. A Pull ran on the workers. One `/readyz` answered 504 from Traefik while the replicas started, then 200. | |
| compose | `oidc-keycloak.yaml` | Sign-in with Keycloak works for `bob` (write Member of `ml`) and `alice` (Instance admin). | |
| compose | `HANGAR_PORT=9090` | Public URL follows the port. | |
| helm | One Pod with SQLite, then read the password | The install works, but the documented `kubectl get secret ... hangar-hangar-generated` found nothing: the chart names it `hangar-generated`, as the install notes say. | Guide and `values.yaml` comment fixed; `TestChartGuideReadsTheGeneratedSecret` checks the guide against the chart. `1a0622e` |
| helm | `web.replicas=2` with persistence on | Refused with the documented message. | |
| helm | Scaled out with Postgres and S3 | Hook migration, two web replicas through the HPA minimum, PodDisruptionBudget, `helm test` passes. The HPA reports no CPU metrics without the metrics server. | Values table says the HPA needs the metrics server. `1a0622e` |
| helm | Run with Argo CD | Covered by `TestKindGitOpsInstall` on the same cluster: render with no cluster access, apply with Postgres and MinIO, init-container migrations. Passed. | |
| helm | `generateSecrets: false` with persistence | Hangar generates the secrets on its volume and prints the password once. | |
| first-login | Sign in, then "Change the password under **Settings**" | There is no password change under **Settings**: **Profile** says to ask an admin for a reset link. The spec has no self-service change either. | The page now says to create a reset link for `hangar-admin` under **Admin**, **Users**, which works. `5554cb5` |
| first-login | **Create token** | The button is **Create Token**. | `5554cb5` |
| first-login | Token scopes | The page matches the 2026-10-17 decision, but the UI's scope help said `admin` was needed for settings and webhooks. | UI copy fixed with a test. `4f57f3f` |
| first-login | `hangar login` device sign-in | Works from the host CLI with the browser approving. | |
| first-login | **Admin**, **Users** invite links | Present. | |
| first-pull | Discover, search `tiny-random-bert`, open the result | The result is not there: huggingface.co search leaves `hf-internal-testing` out, so neither the name nor the full ID finds it. | Discover offers **Open** for a search that is a full Repo ID; the page says to search by ID. `4f57f3f`, `5554cb5` |
| first-pull | **Pull into arsenal**, follow under **Jobs** | Works. | |
| first-pull | `hangar pull ...:Q4_K_M` | Partial mirror with one quant; `hangar info` lists the rest as not in the arsenal. | |
| first-pull | `hangar info nope/nope` (exit codes in the CLI guide) | Said "The Upstream rejected the credentials" and exited 4, for a user with no Upstream token: huggingface.co answers a missing Repo with a bare 401. | Anonymous 401 is not found. `33e5dc7` |
| hf-endpoint | `eval "$(hangar env)"`, then `AutoModel.from_pretrained` | Works once the Repo is pulled. Run before the Pull, as the task order put it, it fails as not found. | The page says the Repo must be in the Arsenal and links to first-pull. `5554cb5` |
| hf-endpoint | `os.environ` and `endpoint=` | Work. | |

## Guides

| Page | Step | Result | Fix |
|---|---|---|---|
| air-gapped | Outer Instance as an Upstream with a read Token, `hangar pull ... --upstream outer` | The inner Instance mirrors the Repo with the same Commit SHA. | |
| collections | `create_collection`, `add_collection_item` | Work. | |
| datasets | `load_dataset`, streaming | Work in about 4 seconds on localhost. | |
| datasets | pandas `hf://` | Works. | |
| datasets | Polars `pl.scan_parquet("hf://...")` | Fails: Polars has its own `hf://` client that always calls huggingface.co. | The page reads through `HfFileSystem`. `5554cb5` |
| datasets | DuckDB with an HTTP secret | Works. | |
| datasets | `hangar info datasets/lhoestq/demo1` | Source size `0 B` for every Split of a Mirror. | Mirrors that adopt the Upstream's export now record source sizes, also used by the Viewer's `/info` and `/size`. `a690d1d` |
| datasets | Mirror whose Parquet export is not pulled | Splits fail with "Pull it to preview"; after `hangar pull --parquet-only` they stayed failed, because the convert Job skipped Commits with no pending Splits. | Convert runs again for not-pulled Splits; the page says how to pull the export. `bb95881`, `13dd831` |
| datasets | `--config --split --parquet-only --to` | Works. | |
| gated | Upstream token under **Settings**, **Upstream tokens** | Present. | |
| gated | Gating modes and the message | Present in the Repo's **Settings**. Request form fields are not in the UI, only in the native API, and saving gating in the UI cleared them. | The page shows the API call; a gating update keeps keys it does not send. `5554cb5`, `8715a05` |
| git | Clone, shallow, `GIT_LFS_SKIP_SMUDGE`, `lfs pull --include`, push with LFS | Work. Non-LFS file over 10 MiB is refused with the documented message. `--atomic` fails with the documented message. A wrong Token gets "Authentication failed". | |
| hangar-cli | login, whoami, logout, search, `--upstream`, info, pull, jobs, env, `--json`, exit codes | Work, after the not-found fix above. The page did not name `HANGAR_CLI_CONFIG` or `HANGAR_CLI_NO_KEYCHAIN`. `hangar --help` used cobra's "Additional Commands" heading. | Page names both variables; help groups in sentence case. `5554cb5`, `73b7664` |
| hf-cli | `hf auth login --token`, whoami, download, `repos create`, upload | Work. | |
| hf-cli | `hf auth login` in the browser | Opens the device page; approving creates a write Token. | |
| hf-cli | "stores the Token per endpoint name" | `hf` keeps one active Token whatever the endpoint. | `5554cb5` |
| import | `--from-hf-cache` (mirror and `--offline --namespace`), `--from-lmstudio`, `--from-ollama`, `--dry-run` | Work. | |
| lm-studio | `hangar lmstudio install`, `list`, `remove --prune` | Work; the models folder resolves to `~/.lmstudio/models` when nothing else exists. | |
| ollama | `ollama pull --insecure host/repo:Q4_K_M` | 401: the key is needed for every Repo while `security.anonymous_read` is off, which is the default. The page put key registration under "Private Repos". | Page registers the key first. `5554cb5` |
| ollama | `hangar ollama install` without the key | Prompt said "Public Repos still work", which is wrong by default. | CLI copy fixed with a test. `23fd187` |
| ollama | Run, case-insensitive tag | Work. | |
| ollama | A quant not in the arsenal | Ollama prints `file does not exist`; it drops the message Hangar sends with the 404, which names the Pull to run. | Page says so; the status is the spec's (see open question in the handoff). `5554cb5` |
| pull-windows | Configuration keys | Valid. | |
| pull-windows | **Pull now** | No such button; `hangar pull --now` and the **Let writers pull now** admin setting exist. | `5554cb5` |
| python | `huggingface_hub`, `transformers`, `sentence-transformers`, `HF_HUB_OFFLINE=1` | Work. | |
| python | `diffusers` with `tiny-stable-diffusion-pipe` | Fails on huggingface.co too: the pipeline names Flax classes that diffusers 0.41 removed. | Example uses `tiny-stable-diffusion-torch`, checked through Hangar. `5554cb5` |
| python | Not-pulled file of a Partial mirror | HTTP 409 with the `hangar pull` command, not `EntryNotFoundError`. | Page describes the 409. `5554cb5` |
| uploading | `huggingface_hub` create, `upload_folder`, `upload_file(create_pr=True)` | Work, through `hf_xet`. | |
| uploading | `hangar push --create --private` | Works. | |
| uploading | `hangar push ./data datasets/acme/reviews --branch march` | Uploaded every file, then failed: "revision not found: march". | `hangar push` creates the branch first. `320939d` |
| vllm | `vllm serve`, `docker run --gpus all` | Not run: no GPU or vLLM on this Mac. The `HF_ENDPOINT` path it relies on is the one the Python guide covers. | |
| webhooks | Create, test event, `repo.updated` on push, signature check with the page's function | Work. The test event is `ping`, which the reference did not list. | Reference lists `ping`. `5554cb5` |

## Elsewhere

| Where | Finding | Fix |
|---|---|---|
| Landing page | Example commands named `TinyLlama/TinyLlama-1.1B-Chat-GGUF`, which does not exist on huggingface.co. | Names Repos the docs use, with a test. `4f57f3f` |
| `docs/` house style | `make lint-style` passed. A read-through found "just" used for "only" in three places. | `6bfb7e9` |
| `make docs` | `mkdocs build --strict` passes before and after. | |
