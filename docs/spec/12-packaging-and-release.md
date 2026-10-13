# 12. Packaging, documentation and release

## Container image

- `ghcr.io/jamesagarside/hangar:<version>` and `:latest`, for `linux/amd64` and `linux/arm64`.
- Multi-stage Dockerfile: Node stage builds the UI, Go stage builds a static binary (`CGO_ENABLED=0`, `-trimpath`, version stamped with `-ldflags`), final stage Chainguard `wolfi-base` with `git` installed (the server runs `git upload-pack`; see [04](04-git.md)), no shell tools beyond what `git` needs.
- Runs as UID 65532, read-only root filesystem (with a writable `/var/cache/hangar` and `/tmp`), data in `/var/lib/hangar` (volume), config at `/etc/hangar/hangar.yaml` (optional; env vars are enough).
- Compose and Helm use `/readyz` for health. The image also declares `HEALTHCHECK CMD ["hangar", "healthcheck"]` (a subcommand that calls `/readyz` on localhost).
- Image is signed with cosign keyless (GitHub OIDC), with an SPDX SBOM attestation and SLSA build provenance.

## Quickstart (must work in under 5 minutes on a clean machine)

```sh
docker run -d --name hangar -p 8080:8080 -v hangar:/var/lib/hangar \
  -e HANGAR_SERVER_PUBLIC_URL=http://localhost:8080 \
  ghcr.io/jamesagarside/hangar:latest
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

- Published as an OCI chart at `oci://ghcr.io/jamesagarside/charts/hangar`.
- Values: image, `config` (rendered to a ConfigMap), `secrets` (or `existingSecret`), `web.replicas`, `worker.replicas`, autoscaling (HPA on CPU for both), resources, PodDisruptionBudgets, topology spread, `persistence` (for SQLite and filesystem mode; forces `web.replicas: 1` and `worker.replicas: 0` with role `all`, and the chart fails to render otherwise), Ingress and Gateway API `HTTPRoute`, Service, ServiceAccount (with annotations for IRSA or Workload Identity), NetworkPolicy (optional), ServiceMonitor (optional, for the Prometheus exporter), extra env and volumes.
- Migrations run in a pre-install and pre-upgrade Job (`hangar admin migrate`); `web` and `worker` pods wait for the expected schema version in `/readyz`.
- Security context: non-root, read-only root filesystem, no privilege escalation, all capabilities dropped, `RuntimeDefault` seccomp.
- `helm lint`, `kubeconform` and `ct install` against kind in CI.

## Binaries

GoReleaser builds `hangar` for linux, darwin and windows on amd64 and arm64, with checksums, SBOMs and cosign signatures. Install options in the docs: release archives, `go install github.com/jamesagarside/hangar/cmd/hangar@latest`, the container image, and a Homebrew tap (`jamesagarside/homebrew-tap`, updated by GoReleaser; creating the tap repo is a release step that needs James).

## Documentation site

MkDocs Material in `docs/`, published to GitHub Pages at `https://jamesagarside.github.io/hangar/` on every release (and `dev` from `main`). House style throughout. Pages:

- **Home**: what Hangar is, a 30-second demo (asciinema or GIF), quickstart.
- **Get started**: Docker, Compose, Helm, binary; first login; pull your first model; point `HF_ENDPOINT` at Hangar.
- **Concepts**: Arsenal, Repos and Revisions, Mirrors and Partial mirrors, Upstreams, Pulls, Dependencies, Jobs, permissions. Uses the vocabulary in `CONTEXT.md`.
- **Guides**: Python libraries, the `hf` CLI, uploading with and without `hf_xet`, datasets (`load_dataset`, pandas, Polars, DuckDB, the Viewer, partial and Parquet-only Pulls, pointing `datasets-server.<host>` at Hangar), git and git-lfs, Ollama, LM Studio, vLLM, the `hangar` CLI, uploading your own models, importing an existing cache, pull windows and bandwidth, air-gapped Instances fed from another Hangar, gated models, Collections, webhooks.
- **Operate**: configuration reference (generated), storage backends (each S3 provider tested), databases, scaling, OIDC providers (one page each), TLS and reverse proxies (Traefik, nginx, Caddy, Cloudflare Tunnel), observability (Elastic, Grafana stack, generic OTLP), backup and restore, upgrades, security hardening, troubleshooting (`hangar admin doctor`).
- **Reference**: Hub API compatibility matrix (every endpoint with supported, partial or not supported, generated from the compat suite results), native API (rendered from OpenAPI with Redoc), CLI reference (generated from cobra), webhook events, metrics.
- **Project**: architecture overview (adapted from this spec), contributing, security policy, changelog, licence.

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
| `release-please.yml` | `main` | Release PRs and changelog from Conventional Commits. |
| `release.yml` | tags `v*` | GoReleaser, multi-arch image build and push, cosign signing, SBOM and provenance, Helm chart package and push, docs publish, Trivy scan of the image (fails on critical CVEs with fixes available). |
| `docs.yml` | `main` | Publish `dev` docs. |

## Release v1.0.0 checklist

1. Every feature in [01](01-product.md) implemented, with its tests.
2. `ci.yml`, `compat.yml` and `fuzz.yml` green on `main` for amd64 and arm64.
3. The quickstart, every Compose example and the Helm chart work from a clean machine, following only the docs.
4. Load test targets in [02](02-architecture.md) met on the reference setup ([13](13-testing.md)), with results committed to `docs/benchmarks.md`.
5. Security review done (threat model in `docs/security/threat-model.md`, findings fixed or recorded).
6. Docs complete with no "TODO" or placeholder pages; screenshots current.
7. James creates the public GitHub repository, enables GitHub Pages, private vulnerability reporting and the CLA app, then pushes the tag.
