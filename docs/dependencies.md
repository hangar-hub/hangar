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
| `github.com/go-git/go-git/v6` | v6.0.0-beta.1 | git receive-pack, server-side Commits and Mirror fetches (04-git.md). A pinned pre-release, kept behind `internal/gitstore`. |
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
| `github.com/parquet-go/parquet-go` | v0.32.0 | Parquet reading and writing for datasets (Apache-2.0). |
| `github.com/klauspost/compress` | v1.20.1 | zstd for zstd-compressed dataset files and Arrow IPC buffers, and Snappy and zstd for checking Parquet pages before parquet-go reads them (Apache-2.0, BSD-3-Clause). Already required by parquet-go. |
| `github.com/pierrec/lz4/v4` | v4.1.21 | LZ4 for checking LZ4_RAW Parquet pages before parquet-go reads them (BSD-3-Clause). Already required by parquet-go. |
| `github.com/andybalholm/brotli` | v1.1.1 | Brotli for checking Brotli Parquet pages before parquet-go reads them (MIT). Already required by parquet-go. |
| `go.yaml.in/yaml/v3` | v3.0.5 | Ordered YAML nodes for dataset card metadata, so `configs` and features keep their order (Apache-2.0, MIT). Already required by koanf. |
| `github.com/oapi-codegen/runtime` | v1.7.0 | Runtime helpers for the generated native API code. |
| `github.com/oapi-codegen/oapi-codegen/v2` (tool) | v2.8.0 | Generates `internal/api/gen` from `api/openapi.yaml`. |
| `github.com/getkin/kin-openapi` | v0.142.0 | OpenAPI document model used by the generated embedded spec. |
| `github.com/stretchr/testify` | v1.12.1 | Test assertions. |
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
