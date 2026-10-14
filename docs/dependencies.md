# Dependencies

Every direct dependency and why Hangar uses it. Add a line here when you add a dependency (docs/spec/02-architecture.md "Stack"). Versions are pinned exactly in `go.mod` and `ui/package.json`.

## Go

| Module | Version | Why |
|---|---|---|
| `github.com/go-chi/chi/v5` | v5.4.0 | HTTP router for the native API and the protocol handlers. |
| `github.com/uptrace/bun` (and `dialect/pgdialect`, `dialect/sqlitedialect`, `driver/pgdriver`, `driver/sqliteshim`) | v1.3.0 | Database access and migrations for SQLite and Postgres. |
| `github.com/uptrace/bun/extra/bunotel` | v1.3.0 | OpenTelemetry spans for database queries, without parameter values. |
| `modernc.org/sqlite` | v1.60.1 | Pure Go SQLite, so builds need no cgo. |
| `github.com/minio/minio-go/v7` | v7.4.1 | S3-compatible Blob store with presigning and multipart. Pinned in `internal/pins` until W1 imports it. |
| `github.com/go-git/go-git/v6` | v6.0.0-beta.1 | git receive-pack (W3) and Mirror fetches (W6) (04-git.md). Server-side Commits use the git binary instead (W2 request 1). A pinned pre-release. |
| `github.com/go-git/go-billy/v6` | v6.0.0-beta.1 | The filesystem go-git's storer uses for the push quarantine; released with go-git. |
| `github.com/coreos/go-oidc/v3` | v3.21.0 | OIDC discovery and ID token verification. |
| `golang.org/x/oauth2` | v0.37.0 | OAuth 2.0 authorisation code flow with PKCE. |
| `golang.org/x/crypto` | v0.58.0 | argon2id password hashing and HKDF key derivation. |
| `golang.org/x/sync` | v0.24.0 | singleflight for pack cache downloads. |
| `golang.org/x/sys` | v0.49.0 | File locks that stop a second process opening a SQLite database. |
| `go.opentelemetry.io/otel` (and `metric`, `trace`, `log`, `sdk`, `sdk/metric`, `sdk/log`) | v1.47.0 | OpenTelemetry API and SDK. |
| `go.opentelemetry.io/contrib/exporters/autoexport` | v0.72.0 | Exporters chosen by the standard `OTEL_*` variables. |
| `go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp` | v0.72.0 | HTTP server and client spans and metrics. |
| `go.opentelemetry.io/contrib/instrumentation/runtime` | v0.72.0 | Go runtime metrics. |
| `go.opentelemetry.io/contrib/bridges/otelslog` | v0.21.0 | Sends `log/slog` records to OTLP. |
| `go.opentelemetry.io/otel/exporters/prometheus` | v0.69.0 | Prometheus `/metrics` when `telemetry.metrics` is `prometheus`. |
| `go.opentelemetry.io/otel/exporters/stdout/*` | v1.47.0 / v0.23.0 | The `console` exporters. |
| `github.com/prometheus/client_golang` | v1.25.0 | The registry and handler behind the Prometheus exporter. |
| `github.com/spf13/cobra` | v1.10.2 | The `hangar` command line. |
| `github.com/knadh/koanf/v2` (and `parsers/yaml`, `providers/file`, `providers/confmap`) | v2.3.8 | Configuration from YAML, environment variables and flags. |
| `github.com/go-viper/mapstructure/v2` | v2.5.0 | Decoding configuration into structs with size and duration hooks. |
| `github.com/robfig/cron/v3` | v3.0.1 | Parsing the 5-field cron schedules in configuration and the `schedules` table. |
| `github.com/google/uuid` | v1.6.0 | UUIDv7 row IDs. |
| `github.com/gpustack/gguf-parser-go` | v0.26.4 | GGUF header parsing with ranged reads (MIT). Pinned until W7 imports it. |
| `github.com/parquet-go/parquet-go` | v0.32.0 | Parquet reading and writing for datasets (Apache-2.0). Pinned until W10 imports it. |
| `github.com/oapi-codegen/runtime` | v1.7.0 | Runtime helpers for the generated native API code. |
| `github.com/oapi-codegen/oapi-codegen/v2` (tool) | v2.8.0 | Generates `internal/api/gen` from `api/openapi.yaml`. |
| `github.com/getkin/kin-openapi` | v0.142.0 | OpenAPI document model used by the generated embedded spec. |
| `github.com/stretchr/testify` | v1.12.1 | Test assertions. |
| `github.com/zalando/go-keyring` | v0.2.8 | Keeps the CLI's Token in the OS keychain (macOS Keychain, Windows Credential Manager, the Secret Service on Linux) for `hangar login` (08-local-tools.md). |
| `go.yaml.in/yaml/v3` | v3.0.5 | Model card front matter in the Hub API's validate-yaml (W4); the CLI's `~/.config/hangar/config.yaml` and `secrets.yaml` in `hangar admin rotate-key` (W9). |
| `github.com/yannh/kubeconform` (run with `go run`, not in `go.mod`) | v0.7.0 | Validates the rendered Helm chart in `make lint-deploy`. |
| `github.com/testcontainers/testcontainers-go` (and `modules/postgres`, `modules/minio`) | v0.44.0 | Postgres and MinIO containers for conformance and integration tests. |

## UI

| Package | Why |
|---|---|
| `react`, `react-dom` | The UI library. |
| `@tanstack/react-router`, `@tanstack/router-plugin` | File-based routing with type-safe parameters. |
| `@tanstack/react-query` | Server state and polling. |
| `openapi-fetch`, `openapi-typescript` | The typed native API client, generated from `api/openapi.yaml`. |
| `tailwindcss`, `@tailwindcss/vite` | Styling. |
| `clsx`, `tailwind-merge` | The `cn` helper that shadcn/ui components use. |
| `vite`, `@vitejs/plugin-react`, `typescript` | Build and type checking. TypeScript stays on 5.x because `openapi-typescript` needs it. |
| `@biomejs/biome` | Lint and format. |
| `vitest`, `jsdom`, `@testing-library/*` | Unit tests. |
| `@playwright/test`, `@axe-core/playwright` | End-to-end and accessibility tests. |

## Build, release and documentation tools

| Tool | Version | Why |
|---|---|---|
| `mkdocs`, `mkdocs-material`, `mike` (`docs/requirements.txt`) | 1.6.1, 9.7.7, 2.2.0 | The documentation site and its versions on GitHub Pages. |
| GoReleaser | v2 | Release archives, checksums, SBOMs, signatures and the Homebrew cask (`.goreleaser.yaml`). |
| cosign, syft | latest at release time | Keyless signing and SPDX SBOMs in `release.yml`. |
| Helm, chart-testing, kind | Helm v4.1.3 | Chart lint and install tests in CI. |
| k6 (`grafana/k6` image) | 1.4.2 | The load tests in `loadtest/`. |
| Redoc (loaded from jsDelivr on the native API page) | 2.5.4 | Renders `api/openapi.yaml` in the documentation. |
