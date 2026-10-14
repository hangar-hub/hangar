#!/usr/bin/env python3
"""Generates the Hangar dashboards (docs/spec/11-observability.md "Dashboards").

One panel list gives both:

- deploy/dashboards/grafana/hangar.json: Grafana, PromQL over the metric
  names the Prometheus exporter (or Prometheus's OTLP receiver) gives.
- deploy/dashboards/kibana/hangar-dashboard.json: the body for Kibana's
  Dashboards API (9.4 and later), ES|QL over the Elastic OpenTelemetry
  data streams: traffic from the access log (logs-*.otel-*), which gives
  exact counts and latencies whatever the metric temporality, and the rest
  from metrics-*.otel-*, whose counters must be cumulative (the SDK default). deploy/dashboards/kibana/hangar.ndjson is
  the same dashboard exported as saved objects, made by importing this body
  into Kibana and exporting it (see deploy/dashboards/README.md).

Run: python3 scripts/dashboards.py
"""

import json
import pathlib

ROOT = pathlib.Path(__file__).resolve().parent.parent
TS = 'TS metrics-*.otel-* | WHERE service.name == "hangar"'
FROM = 'FROM metrics-*.otel-* | WHERE service.name == "hangar"'
LOGS = 'FROM logs-*.otel-* | WHERE service.name == "hangar" AND body.text == "request" AND protocol != "health"'
BUCKET = "BUCKET(@timestamp, 75, ?_tstart, ?_tend)"


def rate(field, by, alias="v"):
    """ES|QL: per-second rate of a counter, summed by attributes."""
    return f"{TS} | STATS {alias} = SUM(RATE({field})) BY t = {BUCKET}{', ' + by if by else ''}"


def last(field, by, alias="v"):
    """ES|QL: the latest value of a gauge."""
    return f"{TS} | STATS {alias} = MAX(LAST_OVER_TIME({field})) BY t = {BUCKET}{', ' + by if by else ''}"


def p95(field, by, alias="p95"):
    """ES|QL: 95th percentile of a histogram."""
    return f"{FROM} | STATS {alias} = PERCENTILE({field}, 95) BY t = {BUCKET}{', ' + by if by else ''}"


# Each panel: section, title, Grafana unit, PromQL targets (expr, legend),
# ES|QL query, value column, breakdown column.
PANELS = [
    ("Traffic", "Requests by protocol", "reqps",
     [('sum by (hangar_protocol) (rate(http_server_request_duration_seconds_count[$__rate_interval]))', "{{hangar_protocol}}")],
     f"{LOGS} | STATS requests = COUNT(*) BY t = {BUCKET}, protocol", "requests", "protocol"),
    ("Traffic", "Request latency, 95th percentile", "s",
     [('histogram_quantile(0.95, sum by (le, hangar_protocol) (rate(http_server_request_duration_seconds_bucket[$__rate_interval])))', "{{hangar_protocol}}")],
     f"{LOGS} | STATS p95 = PERCENTILE(duration_ms, 95) / 1000 BY t = {BUCKET}, protocol", "p95", "protocol"),
    ("Traffic", "Server errors (5xx) by protocol", "reqps",
     [('sum by (hangar_protocol) (rate(http_server_request_duration_seconds_count{http_response_status_code=~"5.."}[$__rate_interval]))', "{{hangar_protocol}}")],
     f"{LOGS} AND status >= 500 | STATS errors = COUNT(*) BY t = {BUCKET}, protocol", "errors", "protocol"),
    ("Downloads and uploads", "Download throughput", "Bps",
     [('sum by (protocol, mode) (rate(hangar_download_bytes_total[$__rate_interval]))', "{{protocol}} {{mode}}")],
     rate("hangar.download.bytes", "protocol"), "v", "protocol"),
    ("Downloads and uploads", "Upload throughput", "Bps",
     [('sum by (protocol) (rate(hangar_upload_bytes_total[$__rate_interval]))', "{{protocol}}")],
     rate("hangar.upload.bytes", "protocol"), "v", "protocol"),
    ("Downloads and uploads", "Files downloaded", "ops",
     [('sum by (protocol) (rate(hangar_download_files_total[$__rate_interval]))', "{{protocol}}")],
     rate("hangar.download.files", "protocol"), "v", "protocol"),
    ("Pulls and Jobs", "Pull throughput by Upstream", "Bps",
     [('sum by (upstream) (rate(hangar_pull_bytes_total[$__rate_interval]))', "{{upstream}}")],
     rate("hangar.pull.bytes", "upstream"), "v", "upstream"),
    ("Pulls and Jobs", "Pulled files by outcome", "ops",
     [('sum by (outcome) (rate(hangar_pull_files_total[$__rate_interval]))', "{{outcome}}")],
     rate("hangar.pull.files", "outcome"), "v", "outcome"),
    ("Pulls and Jobs", "Time waiting for the bandwidth limit", "percentunit",
     [('sum by (upstream) (rate(hangar_pull_rate_limited_duration_seconds_total[$__rate_interval]))', "{{upstream}}")],
     rate("hangar.pull.rate_limited.duration", "upstream"), "v", "upstream"),
    ("Pulls and Jobs", "Job queue by kind and status", "short",
     [('max by (kind, status) (hangar_jobs_queued)', "{{kind}} {{status}}")],
     last("hangar.jobs.queued", "kind, status") + " | EVAL series = CONCAT(kind, \" \", status)", "v", "series"),
    ("Pulls and Jobs", "Failed Jobs by kind", "ops",
     [('sum by (kind) (rate(hangar_jobs_duration_seconds_count{outcome!="ok"}[$__rate_interval]))', "{{kind}}")],
     f"{FROM} AND outcome != \"ok\" | STATS failed = COUNT(hangar.jobs.duration) BY t = {BUCKET}, kind", "failed", "kind"),
    ("Pulls and Jobs", "Job duration, 95th percentile", "s",
     [('histogram_quantile(0.95, sum by (le, kind) (rate(hangar_jobs_duration_seconds_bucket[$__rate_interval])))', "{{kind}}")],
     p95("hangar.jobs.duration", "kind"), "p95", "kind"),
    ("Storage", "Stored bytes by kind", "bytes",
     [('max by (kind) (hangar_storage_bytes)', "{{kind}}")],
     last("hangar.storage.bytes", "kind"), "v", "kind"),
    ("Storage", "Repos by type", "short",
     [('sum by (type) (hangar_repos)', "{{type}}"), ('max(hangar_users)', "users")],
     last("hangar.repos", "type"), "v", "type"),
    ("Storage", "Blob store latency, 95th percentile", "s",
     [('histogram_quantile(0.95, sum by (le, operation) (rate(hangar_blob_operation_duration_seconds_bucket[$__rate_interval])))', "{{operation}}")],
     p95("hangar.blob.operation.duration", "operation"), "p95", "operation"),
    ("Security and events", "Authentication failures by method", "ops",
     [('sum by (method) (rate(hangar_auth_attempts_total{outcome!="ok"}[$__rate_interval]))', "{{method}}")],
     f"{TS} AND outcome != \"ok\" | STATS v = SUM(RATE(hangar.auth.attempts)) BY t = {BUCKET}, method", "v", "method"),
    ("Security and events", "git pushes by outcome", "ops",
     [('sum by (outcome) (rate(hangar_git_push_total[$__rate_interval]))', "{{outcome}}")],
     rate("hangar.git.push", "outcome"), "v", "outcome"),
    ("Security and events", "Webhook deliveries by outcome", "ops",
     [('sum by (outcome) (rate(hangar_webhook_deliveries_total[$__rate_interval]))', "{{outcome}}")],
     rate("hangar.webhook.deliveries", "outcome"), "v", "outcome"),
]


def grafana():
    panels, pid, y, section = [], 1, 0, None
    x = 0
    for sec, title, unit, targets, *_ in PANELS:
        if sec != section:
            if x:
                y += 8
            panels.append({"type": "row", "id": pid, "title": sec, "collapsed": False,
                           "gridPos": {"h": 1, "w": 24, "x": 0, "y": y}, "panels": []})
            pid, y, x, section = pid + 1, y + 1, 0, sec
        panels.append({
            "type": "timeseries", "id": pid, "title": title,
            "datasource": {"type": "prometheus", "uid": "${datasource}"},
            "gridPos": {"h": 8, "w": 8, "x": x, "y": y},
            "fieldConfig": {"defaults": {"unit": unit, "custom": {"lineWidth": 1, "fillOpacity": 10}}, "overrides": []},
            "options": {"legend": {"displayMode": "list", "placement": "bottom", "showLegend": True},
                        "tooltip": {"mode": "multi", "sort": "desc"}},
            "targets": [{"refId": chr(65 + i), "expr": e, "legendFormat": leg,
                         "datasource": {"type": "prometheus", "uid": "${datasource}"}} for i, (e, leg) in enumerate(targets)],
        })
        pid += 1
        x += 8
        if x == 24:
            x, y = 0, y + 8
    return {
        "__inputs": [], "__requires": [{"type": "datasource", "id": "prometheus", "name": "Prometheus", "version": "1.0.0"}],
        "title": "Hangar", "uid": "hangar-overview", "description": "Hangar: requests, transfers, Pulls, Jobs, storage and security.",
        "tags": ["hangar", "opentelemetry"], "timezone": "browser", "schemaVersion": 41, "version": 1,
        "editable": True, "graphTooltip": 1, "time": {"from": "now-6h", "to": "now"}, "refresh": "1m",
        "templating": {"list": [{"name": "datasource", "label": "Data source", "type": "datasource", "query": "prometheus",
                                 "current": {}, "hide": 0, "refresh": 1}]},
        "annotations": {"list": []}, "links": [], "panels": panels,
    }


def kibana():
    panels, x, y = [], 0, 0
    for i, (_, title, unit, _t, query, col, by) in enumerate(PANELS):
        layer = {
            "type": "line",
            "data_source": {"type": "esql", "query": query},
            "x": {"column": "t", "label": "@timestamp"},
            "y": [{"column": col}],
            "breakdown_by": {"column": by},
        }
        panels.append({
            "type": "vis", "id": f"hangar-{i + 1}", "grid": {"x": x, "y": y, "w": 16, "h": 12},
            "config": {"title": title, "type": "xy",
                       "axis": {"x": {"title": {"visible": False}, "scale": "temporal", "domain": {"type": "fit", "rounding": False}},
                                "y": {"title": {"visible": False}}},
                       "layers": [layer]},
        })
        x += 16
        if x == 48:
            x, y = 0, y + 12
    return {"title": "Hangar", "description": "Hangar: requests, transfers, Pulls, Jobs, storage and security, from the Elastic OpenTelemetry data streams.",
            "panels": panels, "time_range": {"from": "now-6h", "to": "now"}}


def write(path, doc):
    p = ROOT / path
    p.parent.mkdir(parents=True, exist_ok=True)
    p.write_text(json.dumps(doc, indent=2) + "\n")
    print("wrote", path)


if __name__ == "__main__":
    write("deploy/dashboards/grafana/hangar.json", grafana())
    write("deploy/dashboards/kibana/hangar-dashboard.json", kibana())
