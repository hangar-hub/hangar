# 11. Observability

All telemetry is OpenTelemetry. Hangar exports traces, metrics and logs over OTLP and needs no vendor-specific agent.

## Setup

- `internal/telemetry` initialises the SDK once at start-up with `contrib/exporters/autoexport`, so standard variables work: `OTEL_EXPORTER_OTLP_ENDPOINT`, `OTEL_EXPORTER_OTLP_PROTOCOL` (`grpc` or `http/protobuf`), `OTEL_EXPORTER_OTLP_HEADERS`, `OTEL_TRACES_EXPORTER`, `OTEL_METRICS_EXPORTER` (including `prometheus`, which serves `/metrics` on `telemetry.prometheus_listen`, default `:9464`), `OTEL_LOGS_EXPORTER`, `OTEL_RESOURCE_ATTRIBUTES`, `OTEL_TRACES_SAMPLER`. Standard variables win over the YAML config.
- Default with nothing configured: no exporters, logs to stdout as JSON, so a quickstart produces no connection errors.
- Resource: `service.name` (default `hangar`), `service.version`, `service.instance.id` (generated per process), `deployment.environment.name` if set, `hangar.role`, plus host and container detectors.
- Shutdown flushes all providers with a 10-second limit.

## Traces

- Inbound HTTP: `otelhttp` on the router, span names `{method} {route template}` (for example `GET /{ns}/{name}/resolve/{rev}/{path}`), never raw paths. Attributes follow HTTP semantic conventions, plus `hangar.repo.id`, `hangar.repo.type`, `hangar.protocol` (`hub`, `git`, `lfs`, `ollama`, `native`, `ui`) and `enduser.id` when authenticated.
- Database: bun's OpenTelemetry hook (`bunotel`), with statement text but no parameter values.
- Blob store: a span per operation (`blob.get`, `blob.put`, `blob.multipart.complete`, `blob.presign`) with `hangar.blob.backend`, key prefix (never full keys of private content), bytes.
- Upstream: outbound `otelhttp` client spans.
- Git: spans for ref advertisement, pack generation, pack receive and ref update.
- Jobs: one span per Job run, linked to the span that enqueued it (store the trace context in the Job row), with `hangar.job.kind`, `hangar.job.id` and attempt.
- Never record tokens, passwords, cookies, presigned URLs or `Authorization` headers on spans or logs. A test asserts this by scanning exported spans and logs from the compat suite.

## Metrics

HTTP server and client metrics come from semantic conventions (`http.server.request.duration`, `http.server.active_requests`, request and response body sizes). Go runtime metrics come from `contrib/instrumentation/runtime`. Hangar adds:

| Instrument | Type | Unit | Attributes |
|---|---|---|---|
| `hangar.download.bytes` | counter | By | `protocol`, `mode` (`redirect`, `proxy`), `repo.type` |
| `hangar.download.files` | counter | {file} | `protocol`, `repo.type` |
| `hangar.upload.bytes` | counter | By | `protocol` |
| `hangar.pull.bytes` | counter | By | `upstream` |
| `hangar.pull.rate_limited.duration` | counter | s | `upstream` |
| `hangar.pull.files` | counter | {file} | `upstream`, `outcome` (`ok`, `retry`, `failed`, `deduplicated`) |
| `hangar.jobs.queued` | gauge | {job} | `kind`, `status` |
| `hangar.jobs.duration` | histogram | s | `kind`, `outcome` |
| `hangar.jobs.lease.expired` | counter | {job} | `kind` |
| `hangar.git.push` | counter | {push} | `outcome` (`ok`, `rejected`, `conflict`, `error`) |
| `hangar.git.pack.duration` | histogram | s | `service` (`upload-pack`, `receive-pack`) |
| `hangar.git.cache.requests` | counter | {request} | `outcome` (`hit`, `miss`) |
| `hangar.git.cache.bytes` | gauge | By | |
| `hangar.git.packs` | gauge | {pack} | `state`, reported by compaction runs |
| `hangar.xet.xorbs` | counter | {xorb} | `outcome` (`inserted`, `existing`, `rejected`) |
| `hangar.xet.reassembly.duration` | histogram | s | `outcome` |
| `hangar.xet.reassembly.bytes` | counter | By | |
| `hangar.blob.operation.duration` | histogram | s | `backend`, `operation`, `outcome` |
| `hangar.storage.bytes` | gauge | By | `kind` (`lfs`, `git`), reported by GC runs |
| `hangar.repos` | gauge | {repo} | `type`, `visibility`, `mirror` |
| `hangar.users` | gauge | {user} | |
| `hangar.auth.attempts` | counter | {attempt} | `method` (`password`, `oidc`, `token`, `ollama_key`), `outcome` |
| `hangar.webhook.deliveries` | counter | {delivery} | `outcome` |
| `hangar.index.duration` | histogram | s | `outcome` |
| `hangar.scan.files` | counter | {file} | `status` |

Gauges that need a database count are computed by one worker at a time (leader by Job lease) every 60 seconds, not by every replica.

Repo IDs are never metric attributes (unbounded cardinality); they belong on spans and logs.

## Logs

- `log/slog` everywhere, bridged to OTLP with `otelslog` and also written to stdout as JSON (`telemetry.log_format: json|text`, `telemetry.log_level`).
- Every log line inside a request or Job carries `trace_id` and `span_id`.
- Access log: one line per request at `info` with method, route template, status, duration, bytes, user ID, protocol and repo ID.
- Audit events are also emitted as log records with `event.name: hangar.audit.<action>` and the same fields as the table.

## Health

- `/healthz`: process is alive (always 200 unless shutting down).
- `/readyz`: database reachable, migrations at the expected version, Blob store reachable (a cached `Stat` on a sentinel key, refreshed every 10 seconds). Returns 503 with a JSON list of failed checks.
- Graceful shutdown on SIGTERM: stop accepting, mark not ready, finish in-flight requests up to `server.shutdown_timeout` (default 30 seconds; long downloads are cut after that), release Job leases.

## Dashboards

Ship `deploy/dashboards/grafana/hangar.json` (Grafana, Prometheus or OTLP-to-Prometheus metric names) and `deploy/dashboards/kibana/hangar.ndjson` (Kibana, for the Elastic OpenTelemetry data streams), covering request rate and latency by protocol, downloads and uploads, Pull throughput and queue, Job failures, storage growth and auth failures. Document how to import both.
