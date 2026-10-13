package blob_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/jamesagarside/hangar/internal/blob"
	"github.com/jamesagarside/hangar/internal/blob/blobtest"
	blobfs "github.com/jamesagarside/hangar/internal/blob/fs"
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

func TestInstrumentedSweepStaging(t *testing.T) {
	ctx := context.Background()
	tp := sdktrace.NewTracerProvider()
	_, err := blob.Instrument(blobtest.NewFake(), "s3", tp.Tracer("test"), nil).SweepStaging(ctx, time.Now())
	require.ErrorIs(t, err, blob.ErrNotSupported, "a store with no staging area")

	fs, err := blobfs.New(filepath.Join(t.TempDir(), "blobs"))
	require.NoError(t, err)
	old := filepath.Join(fs.Root(), ".staging", "put-1")
	require.NoError(t, os.WriteFile(old, []byte("partial"), 0o600))
	at := time.Now().Add(-48 * time.Hour)
	require.NoError(t, os.Chtimes(old, at, at))
	n, err := blob.Instrument(fs, "fs", tp.Tracer("test"), nil).SweepStaging(ctx, time.Now().Add(-24*time.Hour))
	require.NoError(t, err)
	require.Equal(t, 1, n)
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

// failingStore fails every Put with err.
type failingStore struct {
	blob.Store
	err error
}

func (f failingStore) Put(context.Context, string, io.Reader, int64) error { return f.err }

type codedError struct{ code, text string }

func (e codedError) Error() string     { return e.text }
func (e codedError) ErrorCode() string { return e.code }

type timeoutError struct{ text string }

func (e timeoutError) Error() string { return e.text }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return false }

func TestInstrumentedErrorsRecordNoKeysOrPaths(t *testing.T) {
	ctx := context.Background()
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	hist, err := mp.Meter("test").Float64Histogram("hangar.blob.operation.duration")
	require.NoError(t, err)

	fs, err := blobfs.New(filepath.Join(t.TempDir(), "blobs"))
	require.NoError(t, err)
	oid := strings.Repeat("cd", 32)
	key := "lfs/sha256/cd/cd/" + oid
	// A directory at the key's path makes the rename into place fail.
	require.NoError(t, fs.Put(ctx, key+"/child", strings.NewReader("x"), 1))
	s := blob.Instrument(fs, "fs", tp.Tracer("test"), hist)

	err = s.Put(ctx, key, strings.NewReader("weights"), 7)
	require.Error(t, err)
	wrong := sha256.Sum256([]byte("other"))
	copy(wrong[:], bytes.Repeat([]byte{0xcd}, 32))
	err = s.PutVerified(ctx, "lfs/sha256/cd/cd/new-"+oid, strings.NewReader("weights"), 7, wrong[:])
	require.ErrorIs(t, err, blob.ErrChecksumMismatch)
	require.Contains(t, err.Error(), oid, "the returned error keeps its detail for the caller")

	failures := []struct {
		err  error
		want string
	}{
		{codedError{code: "s3.AccessDenied", text: "access denied to " + key}, "error code s3.AccessDenied"},
		{codedError{code: key, text: "denied"}, "blob operation failed"},
		{fmt.Errorf("get %s: %w", key, timeoutError{text: "dial tcp: i/o timeout"}), "network timeout"},
		{fmt.Errorf("get %s: %w", key, &net.OpError{Op: "dial", Err: errors.New("refused " + key)}), "network error"},
		{fmt.Errorf("put %s: %w", key, context.DeadlineExceeded), context.DeadlineExceeded.Error()},
		{errors.New("unexpected reply for " + key), "blob operation failed"},
	}
	for _, f := range failures {
		fsx := blob.Instrument(failingStore{Store: blobtest.NewFake(), err: f.err}, "s3", tp.Tracer("test"), hist)
		require.ErrorIs(t, fsx.Put(ctx, key, strings.NewReader("x"), 1), f.err)
	}

	var messages []string
	for _, sp := range rec.Ended() {
		if sp.Name() != "blob.put" {
			continue
		}
		require.NotContains(t, sp.Status().Description, oid)
		require.NotEmpty(t, sp.Events(), "a failed operation records an exception event")
		for _, ev := range sp.Events() {
			require.Equal(t, "exception", ev.Name)
			for _, a := range ev.Attributes {
				v := a.Value.String()
				require.NotContains(t, v, oid, "event attribute %s must not record the key", a.Key)
				require.NotContains(t, v, fs.Root(), "event attribute %s must not record the root", a.Key)
				require.NotContains(t, v, ".staging", "event attribute %s must not record staging paths", a.Key)
				if a.Key == "exception.message" {
					messages = append(messages, v)
				}
			}
		}
	}
	want := []string{"file exists", blob.ErrChecksumMismatch.Error()}
	for _, f := range failures {
		want = append(want, f.want)
	}
	require.Equal(t, want, messages)

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(ctx, &rm))
	h := rm.ScopeMetrics[0].Metrics[0].Data.(metricdata.Histogram[float64])
	var errorsSeen uint64
	for _, dp := range h.DataPoints {
		if v, _ := dp.Attributes.Value("outcome"); v.AsString() == "error" {
			errorsSeen += dp.Count
		}
	}
	require.Equal(t, uint64(len(want)), errorsSeen)
}

func TestInstrumentedErrorType(t *testing.T) {
	ctx := context.Background()
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	s := blob.Instrument(failingStore{Store: blobtest.NewFake(), err: fmt.Errorf("put x: %w", syscall.ENOSPC)}, "fs", tp.Tracer("test"), nil)
	require.Error(t, s.Put(ctx, "lfs/x", strings.NewReader("x"), 1))
	ev := rec.Ended()[0].Events()[0]
	got := map[string]string{}
	for _, a := range ev.Attributes {
		got[string(a.Key)] = a.Value.String()
	}
	require.Equal(t, map[string]string{"exception.type": "syscall.Errno", "exception.message": syscall.ENOSPC.Error()}, got)
}
