package blob

import (
	"context"
	"errors"
	"io"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/jamesagarside/hangar/internal/telemetry"
)

// Instrumented wraps a Store with a span per operation and the
// hangar.blob.operation.duration histogram (docs/spec/11-observability.md).
// Spans carry the backend, the key's top-level prefix and the bytes moved,
// never full keys or presigned URLs.
type Instrumented struct {
	inner    Store
	backend  string
	tracer   trace.Tracer
	duration metric.Float64Histogram
}

var (
	_ Store          = (*Instrumented)(nil)
	_ VerifiedPutter = (*Instrumented)(nil)
)

// Instrument wraps s. backend is "fs" or "s3". duration may be nil.
func Instrument(s Store, backend string, tracer trace.Tracer, duration metric.Float64Histogram) *Instrumented {
	return &Instrumented{inner: s, backend: backend, tracer: tracer, duration: duration}
}

// Unwrap returns the wrapped Store.
func (i *Instrumented) Unwrap() Store { return i.inner }

type op struct {
	i     *Instrumented
	name  string
	start time.Time
	span  trace.Span
}

func (i *Instrumented) begin(ctx context.Context, name, key string) (context.Context, *op) {
	attrs := []attribute.KeyValue{telemetry.AttrBlobBackend.String(i.backend)}
	if key != "" {
		attrs = append(attrs, telemetry.AttrBlobPrefix.String(telemetry.BlobKeyPrefix(key)))
	}
	ctx, span := i.tracer.Start(ctx, "blob."+name, trace.WithSpanKind(trace.SpanKindClient), trace.WithAttributes(attrs...))
	return ctx, &op{i: i, name: name, start: time.Now(), span: span}
}

func outcome(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, ErrNotFound):
		return "not_found"
	case errors.Is(err, ErrNotSupported):
		return "not_supported"
	default:
		return "error"
	}
}

func (o *op) end(ctx context.Context, err error, bytes int64) {
	if bytes >= 0 {
		o.span.SetAttributes(telemetry.AttrBlobBytes.Int64(bytes))
	}
	out := outcome(err)
	if out == "error" {
		o.span.RecordError(err)
		o.span.SetStatus(codes.Error, "blob operation failed")
	}
	o.span.End()
	if o.i.duration != nil {
		o.i.duration.Record(ctx, time.Since(o.start).Seconds(), metric.WithAttributes(
			attribute.String(telemetry.MetricBackend, o.i.backend),
			attribute.String(telemetry.MetricOperation, o.name),
			attribute.String(telemetry.MetricOutcome, out),
		))
	}
}

// Stat implements Store.
func (i *Instrumented) Stat(ctx context.Context, key string) (Info, error) {
	ctx, o := i.begin(ctx, "stat", key)
	info, err := i.inner.Stat(ctx, key)
	o.end(ctx, err, -1)
	return info, err
}

// Get implements Store. The span ends when Get returns; it records the
// number of bytes the response will carry.
func (i *Instrumented) Get(ctx context.Context, key string, rng *Range) (io.ReadCloser, Info, error) {
	ctx, o := i.begin(ctx, "get", key)
	rc, info, err := i.inner.Get(ctx, key, rng)
	n := int64(-1)
	if err == nil {
		n = info.Size
		if rng != nil {
			n = rng.Length(info.Size)
		}
	}
	o.end(ctx, err, n)
	return rc, info, err
}

// Put implements Store.
func (i *Instrumented) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	ctx, o := i.begin(ctx, "put", key)
	cr := &countingReader{r: r}
	err := i.inner.Put(ctx, key, cr, size)
	o.end(ctx, err, cr.n)
	return err
}

// PutVerified implements VerifiedPutter.
func (i *Instrumented) PutVerified(ctx context.Context, key string, r io.Reader, size int64, sum []byte) error {
	ctx, o := i.begin(ctx, "put", key)
	cr := &countingReader{r: r}
	var err error
	if vp, ok := i.inner.(VerifiedPutter); ok {
		err = vp.PutVerified(ctx, key, cr, size, sum)
	} else {
		err = PutVerifiedMultipart(ctx, i.inner, key, cr, size, sum)
	}
	o.end(ctx, err, cr.n)
	return err
}

// Delete implements Store.
func (i *Instrumented) Delete(ctx context.Context, key string) error {
	ctx, o := i.begin(ctx, "delete", key)
	err := i.inner.Delete(ctx, key)
	o.end(ctx, err, -1)
	return err
}

// List implements Store.
func (i *Instrumented) List(ctx context.Context, prefix string, fn func(Info) error) error {
	ctx, o := i.begin(ctx, "list", prefix)
	err := i.inner.List(ctx, prefix, fn)
	o.end(ctx, err, -1)
	return err
}

// CreateMultipart implements Store.
func (i *Instrumented) CreateMultipart(ctx context.Context, key string) (string, error) {
	ctx, o := i.begin(ctx, "multipart.create", key)
	id, err := i.inner.CreateMultipart(ctx, key)
	o.end(ctx, err, -1)
	return id, err
}

// PresignPart implements Store.
func (i *Instrumented) PresignPart(ctx context.Context, key, uploadID string, part int, ttl time.Duration) (string, error) {
	ctx, o := i.begin(ctx, "presign", key)
	u, err := i.inner.PresignPart(ctx, key, uploadID, part, ttl)
	o.end(ctx, err, -1)
	return u, err
}

// PutPart implements Store.
func (i *Instrumented) PutPart(ctx context.Context, key, uploadID string, part int, r io.Reader, size int64) (string, error) {
	ctx, o := i.begin(ctx, "multipart.part", key)
	cr := &countingReader{r: r}
	etag, err := i.inner.PutPart(ctx, key, uploadID, part, cr, size)
	o.end(ctx, err, cr.n)
	return etag, err
}

// ListParts implements Store.
func (i *Instrumented) ListParts(ctx context.Context, key, uploadID string) ([]Part, error) {
	ctx, o := i.begin(ctx, "multipart.list_parts", key)
	parts, err := i.inner.ListParts(ctx, key, uploadID)
	o.end(ctx, err, -1)
	return parts, err
}

// CompleteMultipart implements Store.
func (i *Instrumented) CompleteMultipart(ctx context.Context, key, uploadID string, parts []Part) error {
	ctx, o := i.begin(ctx, "multipart.complete", key)
	err := i.inner.CompleteMultipart(ctx, key, uploadID, parts)
	var n int64
	for _, p := range parts {
		n += p.Size
	}
	o.end(ctx, err, n)
	return err
}

// AbortMultipart implements Store.
func (i *Instrumented) AbortMultipart(ctx context.Context, key, uploadID string) error {
	ctx, o := i.begin(ctx, "multipart.abort", key)
	err := i.inner.AbortMultipart(ctx, key, uploadID)
	o.end(ctx, err, -1)
	return err
}

// PresignGet implements Store. The URL is never recorded.
func (i *Instrumented) PresignGet(ctx context.Context, key string, ttl time.Duration, filename string) (string, error) {
	ctx, o := i.begin(ctx, "presign", key)
	u, err := i.inner.PresignGet(ctx, key, ttl, filename)
	o.end(ctx, err, -1)
	return u, err
}

// PresignPut implements Store. The URL is never recorded.
func (i *Instrumented) PresignPut(ctx context.Context, key string, size int64, sum []byte, ttl time.Duration) (PresignedRequest, error) {
	ctx, o := i.begin(ctx, "presign", key)
	req, err := i.inner.PresignPut(ctx, key, size, sum, ttl)
	o.end(ctx, err, size)
	return req, err
}

// Copy implements Store.
func (i *Instrumented) Copy(ctx context.Context, srcKey, dstKey string) error {
	ctx, o := i.begin(ctx, "copy", dstKey)
	err := i.inner.Copy(ctx, srcKey, dstKey)
	o.end(ctx, err, -1)
	return err
}

var _ UploadLister = (*Instrumented)(nil)

// ListUploads implements UploadLister when the wrapped store does.
func (i *Instrumented) ListUploads(ctx context.Context, fn func(IncompleteUpload) error) error {
	ul, ok := i.inner.(UploadLister)
	if !ok {
		return ErrNotSupported
	}
	ctx, o := i.begin(ctx, "multipart.list_uploads", "")
	err := ul.ListUploads(ctx, fn)
	o.end(ctx, err, -1)
	return err
}
