# Dashboards

Two dashboards cover the same ground (11-observability.md "Dashboards"): request rate, latency and errors by protocol, downloads and uploads, Pull throughput and outcomes, the Job queue and failures, storage growth, Repos, Blob store latency, authentication failures, git pushes and webhook deliveries. `scripts/dashboards.py` generates both from one panel list.

| File | For |
|---|---|
| `grafana/hangar.json` | Grafana with a Prometheus data source: Hangar's Prometheus exporter (`telemetry.metrics: prometheus`), or Prometheus's OTLP receiver. |
| `kibana/hangar.ndjson` | Kibana 9.4 or later, with Hangar's telemetry in the Elastic OpenTelemetry data streams (`logs-*.otel-*` and `metrics-*.otel-*`), through an EDOT Collector, the Elastic managed OTLP endpoint or Elasticsearch's OTLP endpoint. |
| `kibana/hangar-dashboard.json` | The same Kibana dashboard as a body for the Dashboards API. |

Import them as the documentation describes in Operate, Observability. After changing `scripts/dashboards.py`, run it, import `kibana/hangar-dashboard.json` into a Kibana with `PUT /api/dashboards/hangar-overview`, and export `kibana/hangar.ndjson` from it with `POST /api/saved_objects/_export`.

The Kibana traffic panels read Hangar's access log, so they are exact whatever metric temporality you use. The other panels read metrics and expect cumulative counters, the OpenTelemetry SDK default; do not set `OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE=delta`. The Job and Blob store latency percentiles come from cumulative histograms, so they move slowly after a change.
