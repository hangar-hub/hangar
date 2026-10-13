package telemetry_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/jamesagarside/hangar/internal/config"
	"github.com/jamesagarside/hangar/internal/telemetry"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestDefaultsExportNothingAndLogJSON(t *testing.T) {
	var out bytes.Buffer
	tel, err := telemetry.Setup(context.Background(), telemetry.Options{
		Config: config.Default().Telemetry, Version: "1.2.3", Role: "all", Stdout: &out, Getenv: env(nil),
	})
	require.NoError(t, err)
	defer func() { require.NoError(t, tel.Shutdown(context.Background())) }()
	require.Equal(t, map[string]string{"traces": "none", "metrics": "none", "logs": "none"}, tel.Exporters,
		"otlp needs an endpoint, so a quickstart makes no connections")

	ctx, span := tel.Tracer().Start(context.Background(), "test")
	tel.Logger.InfoContext(ctx, "hello", "k", "v")
	tel.Logger.DebugContext(ctx, "hidden at info level")
	span.End()

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	require.Len(t, lines, 1)
	var rec map[string]any
	require.NoError(t, json.Unmarshal([]byte(lines[0]), &rec))
	require.Equal(t, "hello", rec["msg"])
	require.Equal(t, span.SpanContext().TraceID().String(), rec["trace_id"])
	require.Equal(t, span.SpanContext().SpanID().String(), rec["span_id"])

	attrs := map[string]string{}
	for _, kv := range tel.Resource.Attributes() {
		attrs[string(kv.Key)] = kv.Value.Emit()
	}
	require.Equal(t, "hangar", attrs["service.name"])
	require.Equal(t, "1.2.3", attrs["service.version"])
	require.Equal(t, "all", attrs["hangar.role"])
	require.NotEmpty(t, attrs["service.instance.id"])
}

func TestOTLPNeedsAnEndpoint(t *testing.T) {
	cfg := config.Default().Telemetry
	tel, err := telemetry.Setup(context.Background(), telemetry.Options{
		Config: cfg, Role: "web", Stdout: io.Discard,
		Getenv: env(map[string]string{"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "http://127.0.0.1:1/v1/traces", "OTEL_EXPORTER_OTLP_PROTOCOL": "http/protobuf"}),
	})
	require.NoError(t, err)
	require.Equal(t, "otlp", tel.Exporters["traces"])
	require.Equal(t, "none", tel.Exporters["metrics"])
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	// The endpoint is unreachable; shutdown must still return within its limit.
	_ = tel.Shutdown(ctx)
}

func TestConsoleExportersAndTextLogs(t *testing.T) {
	var out bytes.Buffer
	cfg := config.Default().Telemetry
	cfg.Traces, cfg.Metrics, cfg.Logs, cfg.LogFormat, cfg.LogLevel = "console", "console", "console", "text", "debug"
	tel, err := telemetry.Setup(context.Background(), telemetry.Options{Config: cfg, Stdout: &out, Getenv: env(nil)})
	require.NoError(t, err)
	require.Equal(t, map[string]string{"traces": "console", "metrics": "console", "logs": "console"}, tel.Exporters)
	tel.Logger.Debug("debug line")
	_, span := tel.Tracer().Start(context.Background(), "console-span")
	span.End()
	require.NoError(t, tel.Shutdown(context.Background()))
	require.Contains(t, out.String(), "msg=\"debug line\"")
	require.Contains(t, out.String(), "console-span")
}

func TestStandardEnvVarsWin(t *testing.T) {
	cfg := config.Default().Telemetry
	cfg.Traces = "console"
	tel, err := telemetry.Setup(context.Background(), telemetry.Options{
		Config: cfg, Stdout: io.Discard,
		Getenv: env(map[string]string{"OTEL_TRACES_EXPORTER": "none"}),
	})
	require.NoError(t, err)
	require.Equal(t, "none", tel.Exporters["traces"])
	require.NoError(t, tel.Shutdown(context.Background()))
}

func TestPrometheusServesMetrics(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())

	cfg := config.Default().Telemetry
	cfg.Metrics = "prometheus"
	cfg.PrometheusListen = addr
	tel, err := telemetry.Setup(context.Background(), telemetry.Options{Config: cfg, Stdout: io.Discard, Getenv: env(nil)})
	require.NoError(t, err)
	defer func() { require.NoError(t, tel.Shutdown(context.Background())) }()
	tel.Instruments.DownloadFiles.Add(context.Background(), 2, metric.WithAttributes(attribute.String(telemetry.MetricProtocol, "hub"), attribute.String(telemetry.MetricRepoType, "model")))

	resp, err := http.Get("http://" + addr + "/metrics")
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Contains(t, string(body), "hangar_download_files")
	require.Contains(t, string(body), `protocol="hub"`)
}

func TestInstrumentsHaveSpecNames(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	inst, err := telemetry.NewInstruments(mp.Meter("test"))
	require.NoError(t, err)
	ctx := context.Background()
	inst.DownloadBytes.Add(ctx, 1)
	inst.DownloadFiles.Add(ctx, 1)
	inst.UploadBytes.Add(ctx, 1)
	inst.PullBytes.Add(ctx, 1)
	inst.PullRateLimitedSeconds.Add(ctx, 1)
	inst.PullFiles.Add(ctx, 1)
	inst.JobsQueued.Record(ctx, 1)
	inst.JobsDuration.Record(ctx, 1)
	inst.JobsLeaseExpired.Add(ctx, 1)
	inst.GitPush.Add(ctx, 1)
	inst.GitPackDuration.Record(ctx, 1)
	inst.GitCacheRequests.Add(ctx, 1)
	inst.GitCacheBytes.Record(ctx, 1)
	inst.GitPacks.Record(ctx, 1)
	inst.XetXorbs.Add(ctx, 1)
	inst.XetReassemblyDuration.Record(ctx, 1)
	inst.XetReassemblyBytes.Add(ctx, 1)
	inst.BlobOperationDuration.Record(ctx, 1)
	inst.StorageBytes.Record(ctx, 1)
	inst.Repos.Record(ctx, 1)
	inst.Users.Record(ctx, 1)
	inst.AuthAttempts.Add(ctx, 1)
	inst.WebhookDeliveries.Add(ctx, 1)
	inst.IndexDuration.Record(ctx, 1)
	inst.ScanFiles.Add(ctx, 1)

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(ctx, &rm))
	got := map[string]string{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			got[m.Name] = m.Unit
		}
	}
	want := map[string]string{
		"hangar.download.bytes": "By", "hangar.download.files": "{file}", "hangar.upload.bytes": "By",
		"hangar.pull.bytes": "By", "hangar.pull.rate_limited.duration": "s", "hangar.pull.files": "{file}",
		"hangar.jobs.queued": "{job}", "hangar.jobs.duration": "s", "hangar.jobs.lease.expired": "{job}",
		"hangar.git.push": "{push}", "hangar.git.pack.duration": "s", "hangar.git.cache.requests": "{request}",
		"hangar.git.cache.bytes": "By", "hangar.git.packs": "{pack}", "hangar.xet.xorbs": "{xorb}",
		"hangar.xet.reassembly.duration": "s", "hangar.xet.reassembly.bytes": "By",
		"hangar.blob.operation.duration": "s", "hangar.storage.bytes": "By", "hangar.repos": "{repo}",
		"hangar.users": "{user}", "hangar.auth.attempts": "{attempt}", "hangar.webhook.deliveries": "{delivery}",
		"hangar.index.duration": "s", "hangar.scan.files": "{file}",
	}
	require.Equal(t, want, got)
}

func TestParseLevel(t *testing.T) {
	for in, want := range map[string]slog.Level{"debug": slog.LevelDebug, "": slog.LevelInfo, "WARN": slog.LevelWarn, "error": slog.LevelError} {
		got, err := telemetry.ParseLevel(in)
		require.NoError(t, err)
		require.Equal(t, want, got)
	}
	_, err := telemetry.ParseLevel("trace")
	require.Error(t, err)
}

func TestFanoutAndHandlers(t *testing.T) {
	var a, b bytes.Buffer
	h := telemetry.NewFanout(
		telemetry.NewStdoutHandler(&a, "json", slog.LevelInfo),
		telemetry.NewStdoutHandler(&b, "text", slog.LevelWarn),
	)
	l := slog.New(h).With("svc", "x").WithGroup("g")
	l.Info("info", "k", 1)
	l.Warn("warn", "k", 2)
	require.Equal(t, 2, strings.Count(a.String(), "\n"))
	require.Equal(t, 1, strings.Count(b.String(), "\n"))
	require.Contains(t, a.String(), `"g":{"k":1}`)
	require.Contains(t, b.String(), "svc=x")
}

func TestBlobKeyPrefix(t *testing.T) {
	for in, want := range map[string]string{
		"lfs/sha256/ab/cd/abcd":     "lfs/sha256",
		"git/uuid/pack/pack-x.pack": "git",
		"xet/xorbs/abc":             "xet/xorbs",
		"uploads/uuid/part-1":       "uploads",
		"exports/job.zip":           "exports",
		"avatars/uuid":              "avatars",
		"lfs":                       "lfs",
	} {
		require.Equal(t, want, telemetry.BlobKeyPrefix(in), in)
	}
}
