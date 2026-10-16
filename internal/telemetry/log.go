package telemetry

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"go.opentelemetry.io/otel/trace"
)

// ParseLevel parses debug, info, warn or error.
func ParseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug, nil
	case "", "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("unknown log level %q", s)
	}
}

// NewStdoutHandler returns a JSON or text handler that also adds trace_id
// and span_id to every record logged with a context that carries a span,
// and request_id to every record logged with a context from WithRequestID.
func NewStdoutHandler(w io.Writer, format string, level slog.Level) slog.Handler {
	opts := &slog.HandlerOptions{Level: level}
	var h slog.Handler
	if format == "text" {
		h = slog.NewTextHandler(w, opts)
	} else {
		h = slog.NewJSONHandler(w, opts)
	}
	return contextHandler{Handler: h, traceIDs: true}
}

// LogRequestIDs wraps h so it adds request_id to every record logged with
// a context from WithRequestID. The OpenTelemetry log bridge needs it: it
// takes the trace and span IDs from the context itself, but not the
// request ID.
func LogRequestIDs(h slog.Handler) slog.Handler { return contextHandler{Handler: h} }

type requestIDKey struct{}

// WithRequestID returns a context whose log records carry id as
// request_id. httpx calls it for every request, so a log line can be
// matched to the request ID an error response shows.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}

// RequestIDFromContext returns the request ID WithRequestID stored, or "".
func RequestIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

// contextHandler adds the request ID and, with traceIDs, the current
// span's IDs to each record.
type contextHandler struct {
	slog.Handler
	traceIDs bool
}

func (h contextHandler) Handle(ctx context.Context, r slog.Record) error {
	id := RequestIDFromContext(ctx)
	sc := trace.SpanContextFromContext(ctx)
	withTrace := h.traceIDs && sc.IsValid()
	if id != "" || withTrace {
		r = r.Clone()
	}
	if withTrace {
		r.AddAttrs(slog.String("trace_id", sc.TraceID().String()), slog.String("span_id", sc.SpanID().String()))
	}
	if id != "" {
		r.AddAttrs(slog.String("request_id", id))
	}
	return h.Handler.Handle(ctx, r)
}

func (h contextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return contextHandler{Handler: h.Handler.WithAttrs(attrs), traceIDs: h.traceIDs}
}

func (h contextHandler) WithGroup(name string) slog.Handler {
	return contextHandler{Handler: h.Handler.WithGroup(name), traceIDs: h.traceIDs}
}

// levelHandler drops records below a minimum level before passing them on.
type levelHandler struct {
	min slog.Level
	slog.Handler
}

func (h levelHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return l >= h.min && h.Handler.Enabled(ctx, l)
}

func (h levelHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return levelHandler{min: h.min, Handler: h.Handler.WithAttrs(attrs)}
}

func (h levelHandler) WithGroup(name string) slog.Handler {
	return levelHandler{min: h.min, Handler: h.Handler.WithGroup(name)}
}

// Fanout sends each record to every handler that is enabled for its level.
type Fanout struct{ handlers []slog.Handler }

// NewFanout returns a handler that writes to all of hs.
func NewFanout(hs ...slog.Handler) *Fanout { return &Fanout{handlers: hs} }

// Enabled implements slog.Handler.
func (f *Fanout) Enabled(ctx context.Context, l slog.Level) bool {
	for _, h := range f.handlers {
		if h.Enabled(ctx, l) {
			return true
		}
	}
	return false
}

// Handle implements slog.Handler.
func (f *Fanout) Handle(ctx context.Context, r slog.Record) error {
	var errs []error
	for _, h := range f.handlers {
		if h.Enabled(ctx, r.Level) {
			errs = append(errs, h.Handle(ctx, r.Clone()))
		}
	}
	return errors.Join(errs...)
}

// WithAttrs implements slog.Handler.
func (f *Fanout) WithAttrs(attrs []slog.Attr) slog.Handler {
	out := make([]slog.Handler, len(f.handlers))
	for i, h := range f.handlers {
		out[i] = h.WithAttrs(attrs)
	}
	return &Fanout{handlers: out}
}

// WithGroup implements slog.Handler.
func (f *Fanout) WithGroup(name string) slog.Handler {
	out := make([]slog.Handler, len(f.handlers))
	for i, h := range f.handlers {
		out[i] = h.WithGroup(name)
	}
	return &Fanout{handlers: out}
}
