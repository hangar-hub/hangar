# Observability

Hangar exports traces, metrics and logs with OpenTelemetry, over OTLP, and needs no vendor agent. With nothing configured it makes no connections and writes JSON logs to standard output.

## Turn it on

The standard OpenTelemetry variables work and win over Hangar's own settings:

```sh
OTEL_EXPORTER_OTLP_ENDPOINT=https://otlp.example.com:4318
OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf          # or grpc
OTEL_EXPORTER_OTLP_HEADERS="Authorization=Bearer ..."
OTEL_RESOURCE_ATTRIBUTES=deployment.environment.name=production
```

Or in configuration: `telemetry.traces`, `telemetry.metrics` and `telemetry.logs` (`otlp`, `console` or `none`; metrics also `prometheus`), `telemetry.service_name`, `telemetry.environment`, `telemetry.log_format` and `telemetry.log_level`. With `telemetry.metrics: prometheus`, Hangar serves `/metrics` on `telemetry.prometheus_listen` (`:9464` by default).

What Hangar sends:

- **Traces:** one span per request, named after the route template (such as `GET /{ns}/{name}/resolve/{rev}/{path}`), with child spans for database queries, Blob store operations, Upstream requests, git work and Jobs. Spans carry `hangar.protocol`, `hangar.repo.id` and the user ID when signed in.
- **Metrics:** HTTP server metrics by `hangar.protocol`, Go runtime metrics, and Hangar's own, listed in the [metrics reference](../reference/metrics.md).
- **Logs:** every log line, with `trace_id` and `span_id` inside a request or Job and `request_id` inside a request; one access log line per request; and audit events as `hangar.audit.<action>` records.

Hangar never records tokens, passwords, cookies, presigned URLs or `Authorization` headers on spans or logs. Repo IDs are on spans and logs, never metric attributes.

## Health

- `/healthz` answers 200 while the process is alive.
- `/readyz` answers 200 when the database is reachable, migrations are at the expected version, the Blob store answers and the server has finished starting, and 503 with the failed checks otherwise. Load balancers and Kubernetes should use it. The Blob store check runs in the background with its own 5-second timeout and its result is reused for 10 seconds, so a probe that gives up early does not make the next probes fail.

## Elastic

Send OTLP to an EDOT Collector, the Elastic managed OTLP endpoint, or an Elastic Observability project:

```sh
OTEL_EXPORTER_OTLP_ENDPOINT=https://<project>.ingest.<region>.elastic.cloud:443
OTEL_EXPORTER_OTLP_HEADERS="Authorization=ApiKey <key>"
```

Traces appear in APM as the `hangar` service. Import the Kibana dashboard: **Stack Management**, **Saved objects**, **Import**, and choose [`deploy/dashboards/kibana/hangar.ndjson`](https://github.com/jamesagarside/hangar/blob/main/deploy/dashboards/kibana/hangar.ndjson). It needs Kibana 9.4 or later and reads the `logs-*.otel-*` and `metrics-*.otel-*` data streams. Keep the default cumulative temporality for metrics; the dashboard computes rates from cumulative counters.

## Grafana stack

Send OTLP to Grafana Alloy or an OpenTelemetry Collector that writes to Tempo, Prometheus or Mimir, and Loki; or scrape Hangar's Prometheus endpoint directly. Import [`deploy/dashboards/grafana/hangar.json`](https://github.com/jamesagarside/hangar/blob/main/deploy/dashboards/grafana/hangar.json) under **Dashboards**, **New**, **Import**, and choose your Prometheus data source.

The dashboard uses the names the Prometheus exporter gives (`hangar_download_bytes_total`, `http_server_request_duration_seconds` and so on), which are also what Prometheus's own OTLP receiver produces with its default name translation.

## Any OTLP backend

Point `OTEL_EXPORTER_OTLP_ENDPOINT` at it. The scaled Compose example runs an OpenTelemetry Collector that prints everything it receives, which is a quick way to see the data.
