package gitserver_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/gitstore/gitstoretest"
	"github.com/hangar-hub/hangar/internal/model"
)

// receivePack posts a raw receive-pack request and returns the response
// body, with the peak heap in use while the server handled it.
func (f *fixture) receivePack(t *testing.T, id model.RepoID, body []byte) (string, uint64) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, f.inst.URL+"/"+id.Namespace+"/"+id.Name+".git/git-receive-pack", bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/x-git-receive-pack-request")
	req.SetBasicAuth("x", tokAlice)
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	var peak atomic.Uint64
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			var m runtime.MemStats
			runtime.ReadMemStats(&m)
			if m.HeapInuse > before.HeapInuse && m.HeapInuse-before.HeapInuse > peak.Load() {
				peak.Store(m.HeapInuse - before.HeapInuse)
			}
			select {
			case <-stop:
				return
			case <-time.After(5 * time.Millisecond):
			}
		}
	}()
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	out, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	close(stop)
	<-done
	require.NoError(t, err)
	return string(out), peak.Load()
}

func updateMain(f *fixture, t *testing.T, newOID string) string {
	return gitstoretest.PktLine(f.resolve(t, publicID, "main")+" "+newOID+" refs/heads/main\x00report-status\n") + gitstoretest.Flush
}

// TestPushRefusesDeltaBomb sends a pack of about 16 KiB whose one delta
// rebuilds 1 GiB by copying a 16 MiB base 64 times. go-git rebuilt it in
// memory before any policy ran (1,064 MiB of heap); it is now refused while
// the pack streams in.
func TestPushRefusesDeltaBomb(t *testing.T) {
	f := newFixture(t)
	pack := gitstoretest.Pack(gitstoretest.Blob(make([]byte, 0xffffff)), gitstoretest.CopyDelta(0, 0xffffff, 64))
	require.Less(t, len(pack), 20<<10)
	before := f.resolve(t, publicID, "main")
	out, peak := f.receivePack(t, publicID, append([]byte(updateMain(f, t, strings.Repeat("1", 40))), pack...))
	require.Contains(t, out, "a delta that rebuilds an object of 1024 MiB, over the limit of 64 MiB for one object")
	require.Less(t, peak, uint64(256<<20), "the delta is never rebuilt")
	require.Equal(t, before, f.resolve(t, publicID, "main"))
}

func TestPushRefusesTooManyRefUpdates(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	main := f.resolve(t, publicID, "main")
	var b strings.Builder
	for i := range 10_001 {
		line := strings.Repeat("0", 40) + " " + main + " refs/heads/b" + itoa(i)
		if i == 0 {
			line += "\x00report-status"
		}
		b.WriteString(gitstoretest.PktLine(line + "\n"))
	}
	b.WriteString(gitstoretest.Flush)
	out, _ := f.receivePack(t, publicID, []byte(b.String()))
	require.Contains(t, out, "the push updates more than 10000 refs")
	refs, err := f.repos.Refs(context.Background(), publicID)
	require.NoError(t, err)
	require.Len(t, refs.Branches, 1, "no branch was created")
}

func TestPushRefusesMalformedPack(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	out, _ := f.receivePack(t, publicID, []byte(updateMain(f, t, strings.Repeat("1", 40))+"PACK\x00\x00\x00\x02\x00\x00\x00\x01\xff"))
	require.Contains(t, out, "the pack is not valid")
}

func itoa(i int) string {
	const digits = "0123456789"
	if i == 0 {
		return "0"
	}
	var b []byte
	for ; i > 0; i /= 10 {
		b = append([]byte{digits[i%10]}, b...)
	}
	return string(b)
}

// TestPushWithOptionsAndDeltas checks that the pack check reads past push
// options and accepts the deltas git sends for a growing file.
func TestPushWithOptionsAndDeltas(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	dir := f.clone(t, publicID, tokAlice)
	text := strings.Repeat("a line of text that repeats, so git sends deltas\n", 4000)
	var tip string
	for i := range 4 {
		tip = f.commit(t, dir, "notes.txt", text+strings.Repeat("more\n", i), "edit")
	}
	f.git.Must(t, dir, "push", "-q", "-o", "ci.skip", "origin", "main")
	require.Equal(t, tip, f.resolve(t, publicID, "main"))
}
