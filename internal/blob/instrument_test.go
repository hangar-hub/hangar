package blob_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/jamesagarside/hangar/internal/blob"
	"github.com/jamesagarside/hangar/internal/blob/blobtest"
)

func TestInstrumentedConformance(t *testing.T) {
	tp := sdktrace.NewTracerProvider()
	blobtest.RunConformance(t, blobtest.Capabilities{}, func(*testing.T) blob.Store {
		return blob.Instrument(blobtest.NewFake(), "fs", tp.Tracer("test"), nil)
	})
}

func TestInstrumentedSpansAndMetrics(t *testing.T) {
	ctx := context.Background()
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	hist, err := mp.Meter("test").Float64Histogram("hangar.blob.operation.duration")
	require.NoError(t, err)

	fake := blobtest.NewFake()
	fake.PresignBaseURL = "https://blobs.example.com/bucket"
	s := blob.Instrument(fake, "s3", tp.Tracer("test"), hist)
	require.Same(t, blob.Store(fake), s.Unwrap())
	oid := strings.Repeat("ab", 32)
	key := "lfs/sha256/ab/ab/" + oid
	data := []byte("weights")
	sum := sha256.Sum256(data)
	require.NoError(t, blob.PutVerified(ctx, s, key, bytes.NewReader(data), int64(len(data)), sum[:]))
	_, err = s.Stat(ctx, "missing/key")
	require.ErrorIs(t, err, blob.ErrNotFound)
	rc, _, err := s.Get(ctx, key, &blob.Range{Start: 1, End: 2})
	require.NoError(t, err)
	_ = rc.Close()
	u, err := s.PresignGet(ctx, key, time.Hour, "w.bin")
	require.NoError(t, err)

	spans := rec.Ended()
	names := map[string]bool{}
	for _, sp := range spans {
		names[sp.Name()] = true
		for _, a := range sp.Attributes() {
			require.NotContains(t, a.Value.String(), oid, "span %s must not record full keys", sp.Name())
			require.NotContains(t, a.Value.String(), u, "span %s must not record presigned URLs", sp.Name())
		}
	}
	for _, n := range []string{"blob.put", "blob.stat", "blob.get", "blob.presign"} {
		require.True(t, names[n], "span %s", n)
	}

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(ctx, &rm))
	require.Len(t, rm.ScopeMetrics, 1)
	h := rm.ScopeMetrics[0].Metrics[0].Data.(metricdata.Histogram[float64])
	outcomes := map[string]bool{}
	for _, dp := range h.DataPoints {
		v, _ := dp.Attributes.Value("outcome")
		outcomes[v.AsString()] = true
	}
	require.True(t, outcomes["ok"])
	require.True(t, outcomes["not_found"])
}

func TestDownloads(t *testing.T) {
	ctx := context.Background()
	fake := blobtest.NewFake()
	require.NoError(t, fake.Put(ctx, "lfs/x", strings.NewReader("x"), 1))
	d := blob.Downloads{Store: fake}
	_, ok, err := d.RedirectURL(ctx, "lfs/x", "x")
	require.NoError(t, err)
	require.False(t, ok, "a store that cannot presign streams")
	require.Equal(t, 3600, d.ExpiresIn())

	fake.PresignBaseURL = "https://s3.example.com/b"
	u, ok, err := d.RedirectURL(ctx, "lfs/x", "x")
	require.NoError(t, err)
	require.True(t, ok)
	require.Contains(t, u, "https://s3.example.com/b/lfs/x")
	_, _, err = d.RedirectURL(ctx, "lfs/missing", "x")
	require.ErrorIs(t, err, blob.ErrNotFound)

	d.Mode = blob.DownloadProxy
	_, ok, err = d.RedirectURL(ctx, "lfs/x", "x")
	require.NoError(t, err)
	require.False(t, ok)
}

func TestPartSize(t *testing.T) {
	require.Equal(t, blob.DefaultPartSize, blob.PartSize(0))
	require.Equal(t, blob.DefaultPartSize, blob.PartSize(50<<30))
	require.Equal(t, int64(549755814), blob.PartSize(blob.MaxObjectSize))
	require.LessOrEqual(t, blob.PartSize(blob.MaxObjectSize)*blob.MaxParts, blob.MaxObjectSize+blob.MaxParts)
	require.GreaterOrEqual(t, blob.PartSize(blob.MaxObjectSize)*blob.MaxParts, blob.MaxObjectSize)
}

func TestPutVerifiedLimits(t *testing.T) {
	ctx := context.Background()
	sum := sha256.Sum256(nil)
	require.ErrorIs(t, blob.PutVerified(ctx, blobtest.NewFake(), "lfs/x", strings.NewReader(""), blob.MaxObjectSize+1, sum[:]), blob.ErrTooLarge)
	require.ErrorIs(t, blob.PutMultipart(ctx, blobtest.NewFake(), "lfs/x", strings.NewReader(""), blob.MaxObjectSize+1), blob.ErrTooLarge)
	require.Error(t, blob.PutMultipart(ctx, blobtest.NewFake(), "lfs/x", strings.NewReader(""), -1))
	require.ErrorIs(t, blob.PutMultipart(ctx, blobtest.NewFake(), "../x", strings.NewReader(""), 0), blob.ErrInvalidKey)
	require.Error(t, blob.PutVerifiedMultipart(ctx, blobtest.NewFake(), "lfs/x", strings.NewReader(""), 0, []byte{1}))
}
