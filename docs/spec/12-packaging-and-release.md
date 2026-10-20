# 12. Packaging, documentation and release

## Container image

- `ghcr.io/hangar-hub/hangar:<version>` and `:latest`, for `linux/amd64` and `linux/arm64`.
- Multi-stage Dockerfile: Node stage builds the UI, Go stage builds a static binary (`CGO_ENABLED=0`, `-trimpath`, version stamped with `-ldflags`), final stage Chainguard `wolfi-base` with `git` installed (the server runs `git upload-pack`; see [04](04-git.md)), no shell tools beyond what `git` needs.
- Runs as UID 65532, read-only root filesystem (with a writable `/var/cache/hangar` and `/tmp`), data in `/var/lib/hangar` (volume), config at `/etc/hangar/hangar.yaml` (optional; env vars are enough).
- Compose and Helm use `/readyz` for health. The image also declares `HEALTHCHECK CMD ["hangar", "healthcheck"]` (a subcommand that calls `/readyz` on localhost).
- Image is signed with cosign keyless (GitHub OIDC), with an SPDX SBOM attestation and SLSA build provenance.

## Quickstart (must work in under 5 minutes on a clean machine)

```sh
docker run -d --name hangar -p 8080:8080 -v hangar:/var/lib/hangar \
  -e HANGAR_SERVER_PUBLIC_URL=http://localhost:8080 \
  ghcr.io/hangar-hub/hangar:latest
docker logs hangar   # prints the generated admin password and instance key once
```

With no `instance_key` and no bootstrap password, the first start generates both, writes them to `/var/lib/hangar/secrets.yaml` (mode 0600), and prints them once. Later starts read that file.

## Compose examples (`deploy/compose/`)

| File | What it runs |
|---|---|
| `quickstart.yaml` | One Hangar container, SQLite, filesystem storage. |
| `postgres-minio.yaml` | Hangar (`all`), Postgres, MinIO with a bucket created on start. |
| `scale.yaml` | 2 `web` replicas behind Traefik, 2 `worker` replicas, Postgres, MinIO, an OpenTelemetry Collector printing to its debug exporter. |
| `oidc-keycloak.yaml` | Adds Keycloak with a realm import, for trying OIDC locally. |

CI starts each one and runs the smoke test ([13](13-testing.md)).

## Helm chart (`deploy/helm/hangar`)

- Published as an OCI chart at `oci://ghcr.io/hangar-hub/charts/hangar`.
- Values: image, `config` (rendered to a ConfigMap), `secrets` (or `existingSecret`), `web.replicas`, `worker.replicas`, autoscaling (HPA on CPU for both), resources, PodDisruptionBudgets, topology spread, `persistence` (for SQLite and filesystem mode; forces `web.replicas: 1` and `worker.replicas: 0` with role `all`, and the chart fails to render otherwise), Ingress and Gateway API `HTTPRoute`, Service, ServiceAccount (with annotations for IRSA or Workload Identity), NetworkPolicy (optional), ServiceMonitor (optional, for the Prometheus exporter), extra env and volumes.
- Migrations run in a pre-install and pre-upgrade Job (`hangar admin migrate`), or, with `migrations.mode: initContainer` for GitOps tools that run Helm hooks before the rest of a sync, in an init container of each `web` and `worker` pod; `web` and `worker` pods wait for the expected schema version in `/readyz`.
- Security context: non-root, read-only root filesystem, no privilege escalation, all capabilities dropped, `RuntimeDefault` seccomp.
- `helm lint`, `kubeconform` and `ct install` against kind in CI.

## Binaries

GoReleaser builds `hangar` for linux, darwin and windows on amd64 and arm64, with checksums, SBOMs and cosign signatures. Install options in the docs: release archives, `go install github.com/hangar-hub/hangar/cmd/hangar@latest`, the container image, and a Homebrew tap (`hangar-hub/homebrew-tap`, updated by GoReleaser; creating the tap repo is a release step that needs James).

## Documentation site

MkDocs Material in `docs/`, served by a Cloudflare Worker with static assets at `https://hangar.page/`, published on every release (and `dev` from `main`). mike publishes each version to its own directory on the `gh-pages` branch (`/dev/`, `/1.0/`, with `/latest/` as an alias). The root is Hangar's home page, `deploy/cloudflare/docs-root/index.html`: one static file with inline styles and scripts, the screenshots from `/latest/assets/`, and a sitemap, `robots.txt`, favicon and sharing image beside it (sources for the images in `deploy/cloudflare/brand/`). It replaces the redirect to `latest` that `mike set-default` would write, so the workflows do not run that command, and `TestDocsHomePage` checks its metadata and links. The Worker `hangar-docs` serves `gh-pages` as it is. Workers Builds, Cloudflare's Git integration for Workers, sees each push to `gh-pages` through the Cloudflare GitHub app and runs `wrangler deploy` at the branch root, with no build command. `wrangler.jsonc` there makes the Worker serve only static assets from the branch root, answer a missing page with the nearest `404.html`, and attach the custom domain `hangar.page`; `.assetsignore` keeps that file, itself and the git checkout off the site. GitHub holds no Cloudflare credentials, and the workflows stay as they were for GitHub Pages. Cloudflare Pages would serve the branch the same way, but Cloudflare now builds new static sites on Workers, and the demo is already a Worker. Every other branch pushed to the repository gets a Preview of its docs: Workers Builds runs `scripts/docs-preview.sh` as the preview command, which builds the branch's docs into `site/` with `mkdocs build --strict` and the production `_headers`, and runs `wrangler preview` with `deploy/cloudflare/docs-preview/wrangler.jsonc`. A Preview is named after its branch by `scripts/preview-name.sh` and served at `https://<name>.docs.hangar.page/`, through the production configuration's preview-only route `docs.hangar.page`, for which Cloudflare makes the wildcard DNS record and certificate. It sends `X-Robots-Tag: noindex`, which the script adds to its `_headers`, and has no versions, version selector or redirects. Neither Worker has a `workers.dev` address. The release PR's branch gets a Preview like any other, so the docs and the demo of a release can be checked before it is merged. Workers Builds builds branches pushed to the repository; whether it also builds pull requests from forks, which would run outside code in Cloudflare's build, is to be checked on the first fork pull request. Cloudflare keeps the 100 most recently deployed Previews on the free plan and deletes older ones. `TestDocsPreviewConfig` checks the preview configuration. Canonical URLs and each version's sitemap use `https://hangar.page/latest/`, whichever version built them: the mike plugin's `canonical_version: latest` in `mkdocs.yml` joins it to `site_url`, so search results keep their address across releases. Nothing else hard-codes a path prefix. Links from outside the site (README, chart, image, Homebrew cask, `SECURITY.md`) point at `https://hangar.page/` or, for a page, `https://hangar.page/latest/<page>/`. The Worker reads `_headers`, `_redirects`, a root `404.html` and the home page's files from the root of the branch, which mike leaves alone, so after each publish `scripts/docs-root-files.sh` copies them there from `deploy/cloudflare/docs-root/`, with `wrangler.jsonc` and `.assetsignore`, and commits only when they changed. `TestDocsWorkerConfig` (`deploy/cloudflare_test.go`) checks the Worker's configuration:

- `_headers` sends the Go server's security headers (`X-Frame-Options`, `X-Content-Type-Options`, `Referrer-Policy`, `Cross-Origin-Opener-Policy`, `Permissions-Policy`) and a Content Security Policy like its one, widened for what Material and the native API page need: inline scripts, Google Fonts, the GitHub API for the repository's stars and latest release, and Redoc from `cdn.jsdelivr.net`. The `.page` top-level domain is on the browsers' HSTS preload list, so no `Strict-Transport-Security` header is needed.
- `_redirects` sends section paths without a version, such as `/get-started/`, to `/latest/`, and serves `/assets/` and `/search/` from `latest`. MkDocs builds each version's `404.html` with links from the site root, and the Worker serves that page for a missing page under the version, so without these rules its styles and links break. `TestDocsRedirectsCoverEverySection` (`deploy/cloudflare_test.go`) checks that every top-level section of `docs/` has a rule.
- `404.html` answers paths outside every version.

House style throughout. Pages:

- **Home**: what Hangar is, a 30-second demo (asciinema or GIF), quickstart.
- **Get started**: Docker, Compose, Helm, binary; first login; pull your first model; point `HF_ENDPOINT` at Hangar.
- **Concepts**: Arsenal, Repos and Revisions, Mirrors and Partial mirrors, Upstreams, Pulls, Dependencies, Jobs, permissions. Uses the vocabulary in `CONTEXT.md`.
- **Guides**: Python libraries, the `hf` CLI, uploading with and without `hf_xet`, datasets (`load_dataset`, pandas, Polars, DuckDB, the Viewer, partial and Parquet-only Pulls, pointing `datasets-server.<host>` at Hangar), git and git-lfs, Ollama, LM Studio, vLLM, the `hangar` CLI, uploading your own models, importing an existing cache, pull windows and bandwidth, air-gapped Instances fed from another Hangar, gated models, Collections, webhooks.
- **Operate**: configuration reference (generated), storage backends (each S3 provider tested), databases, scaling, OIDC providers (one page each), TLS and reverse proxies (Traefik, nginx, Caddy, Cloudflare Tunnel), observability (Elastic, Grafana stack, generic OTLP), backup and restore, upgrades, security hardening, troubleshooting (`hangar admin doctor`).
- **Reference**: Hub API compatibility matrix (every endpoint with supported, partial or not supported, generated from the compat suite results), native API (rendered from OpenAPI with Redoc), CLI reference (generated from cobra), webhook events, metrics.
- **Project**: architecture overview (adapted from this spec), contributing, security policy, changelog, licence.

## Demo

A demo of the web UI at `https://demo.hangar.page/`, on Cloudflare's free plan: the UI in front of the mock API in `ui/mock`, the one the Playwright suite runs against, as a Cloudflare Worker with static assets (`ui/demo`). It has no server, database or storage behind it.

- **Build.** `make demo` writes `ui/demo/gen`, the Worker's entry point with the OpenAPI document and its validators compiled ahead of time (Workers cannot compile code at run time, as Ajv does under Vite), and `ui/demo/dist`, the UI built with `VITE_HANGAR_DEMO=1`. The embedded build in `internal/web/dist` is not touched. The flag adds a banner, "This is a demo with sample data. Nothing you do here is saved.", with a link to the docs; Vite drops it from every other build. `wrangler` runs through `pnpm dlx` at the version the `Makefile` pins, so it is not a dependency of the UI.
- **Run and test.** `make demo-serve` runs the demo under `wrangler dev`. `make test-demo` runs the Playwright suite in `ui/e2e` against a second Worker that starts visitors signed out, as `vite preview` does, with the body limits raised to 64 MiB (`DEMO_TEST_LIMIT_MIB`) for the suite's 6 MiB uploads, and the demo's own checks in `ui/e2e-demo`: the banner, the headers and the body limit. `ui/mock/fetch.test.ts` and `ui/demo/worker.test.ts` test the Fetch API adapter, the session limits and the Worker.
- **State.** Visitors start signed in as `ada`. Each visitor gets a copy of the fixture data in the Worker isolate's memory, keyed by the `mock_session` cookie, and nothing is written anywhere else. It resets when Cloudflare recycles the isolate, after an hour unused, or when the isolate drops the least recently used sessions to keep what it holds under 64 MiB (each session counts as 128 KiB plus the request bodies it sent).
- **No shared or outbound content.** Because each visitor has their own data, nothing one visitor writes (Repos, cards, names, sign-out) reaches another, so the demo cannot be defaced or used to show others a phishing link. The mock's OIDC sign-in follows `redirect` only to a path on the demo, with the rules of `internal/auth.SafeRedirect` (a leading `/` but not `//` or `/\`, no control characters, at most 2000 characters, the same origin once resolved), so it is not an open redirect on `hangar.page`.
- **Limits.** The Worker answers 413 to a request body over 1 MiB before the mock sees it, and to writes from a session that has sent 8 MiB in all. Upload parts are counted and dropped, so no file bytes are kept, and the demo cannot be used to host files.
- **Headers.** Pages get the Go server's Content Security Policy with a fresh style nonce, and its other page headers; every response also gets `X-Robots-Tag: noindex`, because sample data is no use in search results, except the home page on `demo.hangar.page`. That one page is indexed, so a search for Hangar can lead to the demo, and gets its own title, description and canonical URL in place of the UI's title. Previews keep `noindex` on every page. The Worker also serves the files Google Search Console and Bing Webmaster Tools check ownership with, at `/google7049fba5ad0480df.html` and `/BingSiteAuth.xml`: `demo/generate.ts` builds them in from `deploy/cloudflare/docs-root/`, the same files `hangar.page` serves, because the Worker answers every other request for HTML with the UI. Files under `/assets/` skip the Worker and get their headers from `ui/demo/_headers`.
- **Deploy.** Workers Builds, Cloudflare's Git integration for Workers, deploys the `demo` branch, which `release.yml` moves to each release tag, so the demo matches the release that `/latest/` documents. It runs `make demo` and then `make demo-deploy` (`wrangler deploy`), which also attaches the custom domain in `ui/demo/wrangler.jsonc`. Every other branch gets a Preview: Workers Builds runs `make demo` and then `make demo-preview` (`wrangler preview`), served at `https://<name>.demo.hangar.page/`, because the route `demo.hangar.page` serves production and Previews. GitHub holds no Cloudflare credentials.

## Repository files

`README.md` (tagline, screenshot, feature summary, quickstart, links, licence), `LICENSE` (Elastic License 2.0), `NOTICE` (third-party notices, including Ollama templates), `CONTRIBUTING.md` (dev setup, `make` targets, tests, commit style, contributor licence agreement through the CLA Assistant GitHub app), `CODE_OF_CONDUCT.md` (Contributor Covenant), `SECURITY.md` (private vulnerability reporting through GitHub security advisories), `CHANGELOG.md` (Keep a Changelog, written by release-please), `.github/ISSUE_TEMPLATE/` (bug, feature, compatibility gap), `.github/pull_request_template.md`, `CODEOWNERS`, `.github/dependabot.yml` (Go modules, npm, Actions, Docker), `Makefile` (`dev`, `build`, `test`, `test-compat`, `lint`, `ui`, `docs`, `image`), `.devcontainer/`.

## Licence

Elastic License 2.0, matching Crockery. The README says what it means in plain words: you can use, change and self-host Hangar, including inside a company, but you cannot offer it to others as a hosted or managed service, and you cannot remove licence notices. ELv2 is source-available, not an OSI-approved open source licence, so the README and docs say "source-available" rather than "open source".

## CI (GitHub Actions)

| Workflow | Runs on | Does |
|---|---|---|
| `ci.yml` | PRs and `main` | Go lint (`golangci-lint` with `gosec`, `govet`, `staticcheck`, `errcheck`), `go test -race` with SQLite and with Postgres (service container), Blob store conformance against the filesystem and MinIO, `govulncheck`, UI lint, typecheck and unit tests, OpenAPI drift check (generated code matches the spec), config reference drift check, build for both architectures (`ubuntu-24.04` and `ubuntu-24.04-arm` runners). |
| `compat.yml` | PRs and nightly | Hub client matrix, git and git-lfs, Ollama, LM Studio install layout, end-to-end UI ([13](13-testing.md)). |
| `fuzz.yml` | nightly | 10 minutes per fuzz target. |
| `codeql.yml` | PRs, weekly | CodeQL for Go and TypeScript. |
| `release-please.yml` | `main` | Release PRs and changelog from Conventional Commits, as the `hangar-hub-release` GitHub App, so its pull requests run CI and its release tags start `release.yml`. |
| `release.yml` | tags `v*` (from release-please or pushed by hand) | GoReleaser, multi-arch image build and push, cosign signing, SBOM and provenance, Helm chart package and push, docs publish, moving the `demo` branch to the tag for the demo, Trivy scan of the image (fails on critical CVEs with fixes available). |
| `docs.yml` | `main` | Publish `dev` docs to `gh-pages`, which the docs Worker serves. |

## Release v1.0.0 checklist

1. Every feature in [01](01-product.md) implemented, with its tests.
2. `ci.yml`, `compat.yml` and `fuzz.yml` green on `main` for amd64 and arm64.
3. The quickstart, every Compose example and the Helm chart work from a clean machine, following only the docs.
4. Load test targets in [02](02-architecture.md) met on the reference setup ([13](13-testing.md)), with results committed to `docs/benchmarks.md`.
5. Security review done (threat model in `docs/security/threat-model.md`, findings fixed or recorded).
6. Docs complete with no "TODO" or placeholder pages; screenshots current.
7. James buys `hangar.page` and adds it as a zone to the Cloudflare account that hosts the sites, with Cloudflare's nameservers. Hangar needs no DNS records added by hand: Cloudflare adds them when it attaches the custom domains below.
8. James creates the public repository `github.com/hangar-hub/hangar` in the `hangar-hub` organisation, enables private vulnerability reporting and the CLA app, and pushes `main`; the `docs.yml` run that follows (start it by hand if it does not) creates the `gh-pages` branch with the `dev` docs and the root files. The repository must be in the organisation before the next steps, because Cloudflare connects to a repository by owner and name. The organisation's release GitHub App, `hangar-hub-release` (contents and pull requests write), is already set up with the organisation Actions variable `RELEASE_APP_ID` and secret `RELEASE_APP_PRIVATE_KEY`, and the tap `hangar-hub/homebrew-tap` exists. James adds the `hangar` repository to the app's installation (it must also include `homebrew-tap`) and to the selected repositories of the variable and the secret. Without them, release-please and the Homebrew cask step fail. No personal access token is needed.
9. James connects the docs in the Cloudflare dashboard: **Workers & Pages**, **Create application**, **Import a repository**. This installs the Cloudflare GitHub app on the repository's owner, with access to the Hangar repository only. The settings are the Worker name `hangar-docs` (the `name` in `deploy/cloudflare/docs-root/wrangler.jsonc`), root directory `/`, an empty build command, and the deploy command `npx wrangler@4.147.0 deploy` (the version the `Makefile` pins). The import builds the repository's default branch, `main`, which has no configuration at its root, so that first build fails. Under the Worker's **Settings**, **Build**, **Branch control**, James sets the production branch to `gh-pages`, turns on **Enable Preview Builds**, and sets the preview command to `./scripts/docs-preview.sh`. The next push to `gh-pages` (start `docs.yml` by hand if none is due) deploys the Worker and attaches `hangar.page`. If the build log says the domain was not attached, it goes under the Worker's **Settings**, **Domains & Routes**, **Custom domain**. For `www`, James adds a proxied `AAAA` record for `www` to `100::` and a redirect rule from the **Redirect from WWW to root** template.
10. James checks that `https://hangar.page/dev/` serves the docs with the headers in `deploy/cloudflare/docs-root/_headers`.
11. James pushes the tag.
12. When `release.yml` has pushed the `demo` branch, James connects the demo: **Workers & Pages**, **Create application**, **Import a repository**, the same repository, with the Worker name `hangar-demo` (the `name` in `ui/demo/wrangler.jsonc`), root directory `/`, build command `make demo`, deploy command `make demo-deploy`, and the build variable `NODE_VERSION` set to `24.19.0`; under **Settings**, **Build**, **Branch control**, the production branch is `demo`, **Enable Preview Builds** is on and the preview command is `make demo-preview`. The first build deploys the Worker and attaches `demo.hangar.page`. If the build log says the domain was not attached, it goes under the Worker's **Settings**, **Domains & Routes**, **Custom domain**, enabled for production and Previews.
13. James checks that `https://hangar.page/` shows the home page and its links reach `/latest/`, and that `https://demo.hangar.page/` shows the demo banner.

Neither site needs a Cloudflare API token in GitHub: Workers Builds deploys both from Cloudflare's side, through the GitHub app, and makes its own token for `wrangler deploy`.
