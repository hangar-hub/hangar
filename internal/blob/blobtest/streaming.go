package blobtest

import (
	"context"
	"crypto/sha256"
	"io"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/blob"
)

// genReader yields n deterministic pseudo-random bytes without holding
// them in memory.
type genReader struct {
	n, off int64
	state  uint64
}

func newGenReader(n int64, seed uint64) *genReader { return &genReader{n: n, state: seed | 1} }

func (g *genReader) Read(p []byte) (int, error) {
	if g.off >= g.n {
		return 0, io.EOF
	}
	p = p[:min(int64(len(p)), g.n-g.off)]
	for i := range p {
		g.state ^= g.state << 13
		g.state ^= g.state >> 7
		g.state ^= g.state << 17
		p[i] = byte(g.state)
	}
	g.off += int64(len(p))
	return len(p), nil
}

// heapPeak samples the heap until stopped and returns the peak growth over
// the baseline.
func heapPeak() (stop func() uint64) {
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	base := ms.HeapAlloc
	var peak atomic.Uint64
	sample := func() {
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		if m.HeapAlloc > base && m.HeapAlloc-base > peak.Load() {
			peak.Store(m.HeapAlloc - base)
		}
	}
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(2 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				sample()
			}
		}
	}()
	return func() uint64 {
		close(done)
		wg.Wait()
		// A last sample catches an operation that finished, still holding
		// what it buffered, before the first tick.
		sample()
		return peak.Load()
	}
}

// RunStreaming checks that a backend streams (UP-5): a verified write, a
// plain write of known size and a read of an object several parts long
// never hold the object in memory. maxHeap is the allowed heap growth.
func RunStreaming(t testing.TB, s blob.Store, size int64, maxHeap uint64) {
	t.Helper()
	ctx := context.Background()
	h := sha256.New()
	_, err := io.Copy(h, newGenReader(size, 42))
	require.NoError(t, err)
	sum := h.Sum(nil)

	stop := heapPeak()
	require.NoError(t, blob.PutVerified(ctx, s, "lfs/sha256/stream/verified", newGenReader(size, 42), size, sum))
	peak := stop()
	t.Logf("PutVerified of %d MiB: peak heap growth %d MiB", size>>20, peak>>20)
	require.Less(t, peak, maxHeap, "PutVerified streams")

	stop = heapPeak()
	require.NoError(t, s.Put(ctx, "lfs/sha256/stream/plain", newGenReader(size, 7), size))
	peak = stop()
	t.Logf("Put of %d MiB: peak heap growth %d MiB", size>>20, peak>>20)
	require.Less(t, peak, maxHeap, "Put with a known size streams")

	stop = heapPeak()
	rc, info, err := s.Get(ctx, "lfs/sha256/stream/verified", nil)
	require.NoError(t, err)
	require.Equal(t, size, info.Size)
	rh := sha256.New()
	_, err = io.Copy(rh, rc)
	require.NoError(t, err)
	require.NoError(t, rc.Close())
	peak = stop()
	require.Equal(t, sum, rh.Sum(nil))
	t.Logf("Get of %d MiB: peak heap growth %d MiB", size>>20, peak>>20)
	require.Less(t, peak, maxHeap, "Get streams")

	for _, k := range []string{"lfs/sha256/stream/verified", "lfs/sha256/stream/plain"} {
		require.NoError(t, s.Delete(ctx, k))
	}
}
