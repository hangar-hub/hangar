// Package telemetry sets up OpenTelemetry traces, metrics and logs for a
// Hangar process (docs/spec/11-observability.md): exporters chosen by the
// standard OTEL_* variables or the telemetry config, the resource, slog
// logging to stdout and OTLP with trace IDs on every record, Hangar's own
// metric instruments, and shutdown that flushes everything within 10 seconds.
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/contrib/exporters/autoexport"
	otelruntime "go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel"
	promexporter "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/exporters/stdout/stdoutlog"
	"go.opentelemetry.io/otel/exporters/stdout/stdoutmetric"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	lognoop "go.opentelemetry.io/otel/log/noop"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/jamesagarside/hangar/internal/config"
)

// ScopeName is the instrumentation scope of Hangar's own telemetry.
const ScopeName = "github.com/jamesagarside/hangar"

// ShutdownTimeout bounds how long Shutdown waits for exporters to flush.
const ShutdownTimeout = 10 * time.Second

// Options configures Setup.
type Options struct {
	Config  config.TelemetryConfig
	Version string
	Role    string
	// Stdout receives log lines and console exporter output. Nil means os.Stdout.
	Stdout io.Writer
	// Getenv reads environment variables. Nil means os.Getenv.
	Getenv func(string) string
}

// Telemetry holds the providers for one process.
type Telemetry struct {
	TracerProvider *sdktrace.TracerProvider
	MeterProvider  *sdkmetric.MeterProvider
	LoggerProvider *sdklog.LoggerProvider
	// Logger writes to stdout and, when a log exporter is configured, OTLP.
	Logger *slog.Logger
	// Instruments are Hangar's metric instruments.
	Instruments *Instruments
	// Resource describes the process.
	Resource *resource.Resource
	// Exporters records which exporter each signal uses: "otlp", "console",
	// "prometheus" or "none".
	Exporters map[string]string

	promServer *http.Server
}

// Setup creates the providers, registers them globally and returns them.
func Setup(ctx context.Context, opts Options) (*Telemetry, error) {
	getenv := opts.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	stdout := opts.Stdout
	if stdout == nil {
		stdout = os.Stdout
	}
	cfg := opts.Config
	t := &Telemetry{Exporters: map[string]string{}}

	res, err := newResource(ctx, cfg, opts.Version, opts.Role)
	if err != nil {
		return nil, err
	}
	t.Resource = res

	// Traces.
	spanExp, kind, err := spanExporter(ctx, cfg.Traces, getenv, stdout)
	if err != nil {
		return nil, fmt.Errorf("trace exporter: %w", err)
	}
	t.Exporters["traces"] = kind
	tpOpts := []sdktrace.TracerProviderOption{sdktrace.WithResource(res)}
	if spanExp != nil {
		tpOpts = append(tpOpts, sdktrace.WithBatcher(spanExp))
	}
	t.TracerProvider = sdktrace.NewTracerProvider(tpOpts...)

	// Metrics.
	reader, kind, promHandler, err := metricReader(ctx, cfg.Metrics, getenv, stdout)
	if err != nil {
		return nil, fmt.Errorf("metric reader: %w", err)
	}
	t.Exporters["metrics"] = kind
	mpOpts := []sdkmetric.Option{sdkmetric.WithResource(res)}
	if reader != nil {
		mpOpts = append(mpOpts, sdkmetric.WithReader(reader))
	}
	t.MeterProvider = sdkmetric.NewMeterProvider(mpOpts...)
	if promHandler != nil {
		t.promServer = &http.Server{Addr: cfg.PrometheusListen, Handler: promHandler, ReadHeaderTimeout: 30 * time.Second}
		ln, err := net.Listen("tcp", cfg.PrometheusListen)
		if err != nil {
			return nil, fmt.Errorf("listen for Prometheus on %s: %w", cfg.PrometheusListen, err)
		}
		go func() { _ = t.promServer.Serve(ln) }()
	}

	// Logs.
	logExp, kind, err := logExporter(ctx, cfg.Logs, getenv, stdout)
	if err != nil {
		return nil, fmt.Errorf("log exporter: %w", err)
	}
	t.Exporters["logs"] = kind
	lpOpts := []sdklog.LoggerProviderOption{sdklog.WithResource(res)}
	if logExp != nil {
		lpOpts = append(lpOpts, sdklog.WithProcessor(sdklog.NewBatchProcessor(logExp)))
	}
	t.LoggerProvider = sdklog.NewLoggerProvider(lpOpts...)

	otel.SetTracerProvider(t.TracerProvider)
	otel.SetMeterProvider(t.MeterProvider)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))
	if logExp != nil {
		otel.SetLoggerProvider(t.LoggerProvider)
	} else {
		otel.SetLoggerProvider(lognoop.NewLoggerProvider())
	}

	if err := otelruntime.Start(otelruntime.WithMeterProvider(t.MeterProvider)); err != nil {
		return nil, fmt.Errorf("runtime metrics: %w", err)
	}
	t.Instruments, err = NewInstruments(t.MeterProvider.Meter(ScopeName))
	if err != nil {
		return nil, err
	}

	level, err := ParseLevel(cfg.LogLevel)
	if err != nil {
		return nil, err
	}
	handlers := []slog.Handler{NewStdoutHandler(stdout, cfg.LogFormat, level)}
	if logExp != nil {
		handlers = append(handlers, levelHandler{min: level, Handler: otelslog.NewHandler(ScopeName, otelslog.WithLoggerProvider(t.LoggerProvider))})
	}
	t.Logger = slog.New(NewFanout(handlers...))
	return t, nil
}

// Tracer returns Hangar's tracer.
func (t *Telemetry) Tracer() trace.Tracer { return t.TracerProvider.Tracer(ScopeName) }

// Meter returns Hangar's meter.
func (t *Telemetry) Meter() metric.Meter { return t.MeterProvider.Meter(ScopeName) }

// Shutdown flushes and stops every provider, waiting at most ShutdownTimeout.
func (t *Telemetry) Shutdown(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), ShutdownTimeout)
	defer cancel()
	var errs []error
	if t.promServer != nil {
		errs = append(errs, t.promServer.Shutdown(ctx))
	}
	errs = append(errs,
		t.TracerProvider.Shutdown(ctx),
		t.MeterProvider.Shutdown(ctx),
		t.LoggerProvider.Shutdown(ctx),
	)
	return errors.Join(errs...)
}

func newResource(ctx context.Context, cfg config.TelemetryConfig, version, role string) (*resource.Resource, error) {
	attrs := []resource.Option{
		resource.WithAttributes(
			semconv.ServiceName(cfg.ServiceName),
			semconv.ServiceVersion(version),
			semconv.ServiceInstanceID(uuid.NewString()),
			AttrRole.String(role),
		),
	}
	if cfg.Environment != "" {
		attrs = append(attrs, resource.WithAttributes(semconv.DeploymentEnvironmentNameKey.String(cfg.Environment)))
	}
	// Detectors run in order and later ones win, so OTEL_RESOURCE_ATTRIBUTES
	// and OTEL_SERVICE_NAME override the configuration.
	attrs = append(attrs, resource.WithHost(), resource.WithContainer(), resource.WithFromEnv(), resource.WithTelemetrySDK())
	res, err := resource.New(ctx, attrs...)
	if err != nil && !errors.Is(err, resource.ErrPartialResource) {
		return nil, fmt.Errorf("telemetry resource: %w", err)
	}
	return res, nil
}

// hasOTLPEndpoint reports whether an OTLP endpoint is configured for the signal.
func hasOTLPEndpoint(getenv func(string) string, signal string) bool {
	return getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" || getenv("OTEL_EXPORTER_OTLP_"+signal+"_ENDPOINT") != ""
}

func spanExporter(ctx context.Context, choice string, getenv func(string) string, stdout io.Writer) (sdktrace.SpanExporter, string, error) {
	if v := getenv("OTEL_TRACES_EXPORTER"); v != "" {
		e, err := autoexport.NewSpanExporter(ctx)
		if err != nil || autoexport.IsNoneSpanExporter(e) {
			return nil, "none", err
		}
		return e, firstValue(v), nil
	}
	switch choice {
	case "console":
		e, err := stdouttrace.New(stdouttrace.WithWriter(stdout))
		return e, "console", err
	case "otlp":
		if !hasOTLPEndpoint(getenv, "TRACES") {
			return nil, "none", nil
		}
		e, err := autoexport.NewSpanExporter(ctx)
		return e, "otlp", err
	default:
		return nil, "none", nil
	}
}

func metricReader(ctx context.Context, choice string, getenv func(string) string, stdout io.Writer) (sdkmetric.Reader, string, http.Handler, error) {
	if v := getenv("OTEL_METRICS_EXPORTER"); v != "" {
		r, err := autoexport.NewMetricReader(ctx)
		if err != nil || autoexport.IsNoneMetricReader(r) {
			return nil, "none", nil, err
		}
		return r, firstValue(v), nil, nil
	}
	switch choice {
	case "console":
		e, err := stdoutmetric.New(stdoutmetric.WithWriter(stdout))
		if err != nil {
			return nil, "", nil, err
		}
		return sdkmetric.NewPeriodicReader(e), "console", nil, nil
	case "prometheus":
		reg := prometheus.NewRegistry()
		e, err := promexporter.New(promexporter.WithRegisterer(reg))
		if err != nil {
			return nil, "", nil, err
		}
		mux := http.NewServeMux()
		mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
		return e, "prometheus", mux, nil
	case "otlp":
		if !hasOTLPEndpoint(getenv, "METRICS") {
			return nil, "none", nil, nil
		}
		r, err := autoexport.NewMetricReader(ctx)
		return r, "otlp", nil, err
	default:
		return nil, "none", nil, nil
	}
}

func logExporter(ctx context.Context, choice string, getenv func(string) string, stdout io.Writer) (sdklog.Exporter, string, error) {
	if v := getenv("OTEL_LOGS_EXPORTER"); v != "" {
		e, err := autoexport.NewLogExporter(ctx)
		if err != nil || autoexport.IsNoneLogExporter(e) {
			return nil, "none", err
		}
		return e, firstValue(v), nil
	}
	switch choice {
	case "console":
		e, err := stdoutlog.New(stdoutlog.WithWriter(stdout))
		return e, "console", err
	case "otlp":
		if !hasOTLPEndpoint(getenv, "LOGS") {
			return nil, "none", nil
		}
		e, err := autoexport.NewLogExporter(ctx)
		return e, "otlp", err
	default:
		return nil, "none", nil
	}
}

func firstValue(v string) string {
	first, _, _ := strings.Cut(v, ",")
	return strings.TrimSpace(first)
}
