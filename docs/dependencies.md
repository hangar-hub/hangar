# Dependencies

Every direct dependency and why Hangar uses it. Add a line here when you add a dependency (docs/spec/02-architecture.md "Stack"). Versions are pinned exactly in `go.mod` and `ui/package.json`.

## Go

| Module | Version | Why |
|---|---|---|
| `github.com/go-chi/chi/v5` | v5.4.0 | HTTP router for the native API and the protocol handlers. |
| `github.com/uptrace/bun` (and `dialect/pgdialect`, `dialect/sqlitedialect`, `driver/pgdriver`, `driver/sqliteshim`) | v1.3.0 | Database access and migrations for SQLite and Postgres. |
| `github.com/uptrace/bun/extra/bunotel` | v1.3.0 | OpenTelemetry spans for database queries, without parameter values. |
| `modernc.org/sqlite` | v1.60.1 | Pure Go SQLite, so builds need no cgo. |
| `github.com/minio/minio-go/v7` | v7.4.1 | S3-compatible Blob store with presigning and multipart. |
| `github.com/go-git/go-git/v6` | v6.0.0-beta.1 | git receive-pack (W3) and Mirror fetches (W6) (04-git.md). Server-side Commits use the git binary instead (W2 request 1). A pinned pre-release. Its pack index iterator leaves stray bytes in SHA-1 object IDs, so the git server rebuilds the IDs it reads from a push's index. |
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
| `go.yaml.in/yaml/v3` | v3.0.5 | Model card front matter (`base_model`, `datasets`) for Dependency resolution. Already in the module graph through koanf. |
| `github.com/google/uuid` | v1.6.0 | UUIDv7 row IDs. |
| `github.com/gpustack/gguf-parser-go` | v0.26.4 | ggml tensor type sizes and GGUF file type names (MIT). `internal/index/gguf` reads the header itself over ranged reads, because the library parses only local paths and URLs. |
| `github.com/parquet-go/parquet-go` | v0.32.0 | Parquet reading and writing for datasets (Apache-2.0). |
| `github.com/klauspost/compress` | v1.20.1 | zstd for zstd-compressed dataset files and Arrow IPC buffers, and Snappy and zstd for checking Parquet pages before parquet-go reads them (Apache-2.0, BSD-3-Clause). Already required by parquet-go. |
| `github.com/pierrec/lz4/v4` | v4.1.21 | Decodes the LZ4 frames in Xet xorb chunks (UP-7) and checks LZ4_RAW Parquet pages before parquet-go reads them (BSD-3-Clause). Already required by parquet-go. |
| `github.com/andybalholm/brotli` | v1.1.1 | Brotli for checking Brotli Parquet pages before parquet-go reads them (MIT). Already required by parquet-go. |
| `github.com/zeebo/blake3` | v0.2.4 | Keyed BLAKE3 for Xet chunk, xorb, file and verification hashes (UP-7); CC0-1.0. |
| `github.com/oapi-codegen/runtime` | v1.7.0 | Runtime helpers for the generated native API code. |
| `github.com/oapi-codegen/oapi-codegen/v2` (tool) | v2.8.0 | Generates `internal/api/gen` from `api/openapi.yaml`. |
| `github.com/getkin/kin-openapi` | v0.144.0 | OpenAPI document model used by the generated embedded spec. |
| `github.com/stretchr/testify` | v1.12.1 | Test assertions. |
| `github.com/zalando/go-keyring` | v0.2.8 | Keeps the CLI's Token in the OS keychain (macOS Keychain, Windows Credential Manager, the Secret Service on Linux) for `hangar login` (08-local-tools.md). |
| `go.yaml.in/yaml/v3` | v3.0.5 | Model card front matter in the Hub API's validate-yaml (W4), Upstream card metadata for Dependencies (W6), ordered dataset card metadata so `configs` and features keep their order (W10), and the CLI's `~/.config/hangar/config.yaml` and `secrets.yaml` in `hangar admin rotate-key` (W9) (Apache-2.0, MIT). |
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
| `radix-ui` | Accessible primitives (dialog, menu, tabs, switch, tooltip) under the shadcn/ui-style components. |
| `class-variance-authority` | Component variants for the shadcn/ui-style buttons and badges. |
| `lucide-react` | Icons. |
| `cmdk` | The command palette behind global search (BROWSE-6). |
| `react-markdown`, `remark-gfm`, `rehype-raw`, `rehype-sanitize` | Model cards and discussions, with raw HTML sanitised to the Hub's allowed subset. |
| `shiki` | Syntax highlighting, loaded lazily, with its JavaScript regex engine so the Content Security Policy needs no WebAssembly. |
| `@tanstack/react-virtual` | Virtualised lists and the Dataset Viewer table. |
| `@noble/hashes` | Streaming SHA-256 in a Web Worker for browser uploads; pure JavaScript, so no WebAssembly. |
| `yaml` | Reading and writing card metadata, and the mock server's copy of `api/openapi.yaml`. |
| `@dnd-kit/core`, `@dnd-kit/sortable`, `@dnd-kit/utilities` | Drag and keyboard reordering of Collection items. |
| `get-nonce` | Hands the per-response style nonce to the dialog libraries, which add a style element to lock page scrolling. |
| `ajv`, `ajv-formats` | The development mock server validates every request and response against `api/openapi.yaml`. |

## Build, release and documentation tools

| Tool | Version | Why |
|---|---|---|
| `mkdocs`, `mkdocs-material`, `mike` (`docs/requirements.txt`) | 1.6.1, 9.7.7, 2.2.0 | The documentation site and its versions on GitHub Pages. |
| GoReleaser | v2 | Release archives, checksums, SBOMs, signatures and the Homebrew cask (`.goreleaser.yaml`). |
| cosign | 3.0.6 | Keyless signing in `release.yml`, into Sigstore bundles. `make release-rehearsal` runs the same steps with a throwaway key. |
| syft | latest at release time | SPDX SBOMs for the release archives and the image. |
| Helm, chart-testing, kind | Helm v4.1.3 | Chart lint and install tests in CI. |
| k6 (`grafana/k6` image) | 1.4.2 | The load tests in `loadtest/`. |
| Redoc (loaded from jsDelivr on the native API page) | 2.5.4 | Renders `api/openapi.yaml` in the documentation. |
