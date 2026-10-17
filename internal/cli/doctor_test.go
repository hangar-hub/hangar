package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/blob"
	"github.com/jamesagarside/hangar/internal/blob/blobtest"
	"github.com/jamesagarside/hangar/internal/config"
)

// cuttingStore cuts every read of the large read check's object after cut
// bytes, as nginx in front of MinIO did on bellatrix once its 1024 MiB
// temporary file was full (finding 13). With short set the body ends
// cleanly, as it does when the proxy closes a response that had no
// Content-Length; otherwise it fails with io.ErrUnexpectedEOF.
type cuttingStore struct {
	*blobtest.Fake
	cut   int64
	short bool
}

func (c cuttingStore) Get(ctx context.Context, key string, rng *blob.Range) (io.ReadCloser, blob.Info, error) {
	rc, info, err := c.Fake.Get(ctx, key, rng)
	if err != nil || !strings.HasPrefix(key, "tmp/doctor-large-") {
		return rc, info, err
	}
	end := io.ErrUnexpectedEOF
	if c.short {
		end = io.EOF
	}
	return cutBody{Reader: io.MultiReader(io.LimitReader(rc, c.cut), errReader{end}), c: rc}, info, nil
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

type cutBody struct {
	io.Reader
	c io.Closer
}

func (b cutBody) Close() error { return b.c.Close() }

func noLargeReadPacing(t *testing.T) {
	t.Helper()
	orig := largeReadRate
	largeReadRate = 0
	t.Cleanup(func() { largeReadRate = orig })
}

// TestLargeReadCheckFindsAProxyThatCutsReads covers the doctor check that
// bellatrix finding 13d proposed: a read of a large object that a proxy
// cuts short fails the check with the nginx settings that fix it, and a
// read that arrives whole passes. Either way the object is deleted.
func TestLargeReadCheckFindsAProxyThatCutsReads(t *testing.T) {
	ctx := context.Background()
	const size = 3<<20 + 5

	fake := blobtest.NewFake()
	msg, err := largeReadCheck(ctx, fake, size, 0)
	require.NoError(t, err)
	require.Contains(t, msg, "read 3.0 MiB back in one request")
	require.Empty(t, fake.Keys(), "the object is deleted")

	for _, short := range []bool{false, true} {
		cut := cuttingStore{Fake: blobtest.NewFake(), cut: 1 << 20, short: short}
		_, err := largeReadCheck(ctx, cut, size, 0)
		require.Error(t, err)
		require.Contains(t, err.Error(), "a read of 3.0 MiB stopped after 1.0 MiB")
		require.Contains(t, err.Error(), "proxy_buffering off and proxy_max_temp_file_size 0")
		require.Empty(t, cut.Keys(), "the object is deleted")
	}
}

// TestLargeReadCheckReadsSlowly checks the check reads no faster than its
// rate, so a buffering proxy fills up as it does for a slow server.
func TestLargeReadCheckReadsSlowly(t *testing.T) {
	waits := recordWaits(t)
	start := time.Now()
	_, err := largeReadCheck(context.Background(), blobtest.NewFake(), 4<<20, 1<<20)
	require.NoError(t, err)
	require.NotEmpty(t, *waits)
	last := (*waits)[len(*waits)-1]
	elapsed := time.Since(start)
	require.Greater(t, last, 4*time.Second-elapsed-time.Second/2, "the last byte is due 4 seconds after the read began")
	require.LessOrEqual(t, last, 4*time.Second)
}

func TestDoctorLargeRead(t *testing.T) {
	noLargeReadPacing(t)
	e := newEnv(t)
	dsn := filepath.Join(e.home, "hangar.db")
	d := openForTest(t, dsn)
	require.NoError(t, d.Close())
	var store blob.Store = blobtest.NewFake()
	e.deps = AdminDeps{OpenBlobStore: func(context.Context, *config.Config) (blob.Store, error) { return store, nil }}
	flags := append(dbFlags(t, "sqlite", dsn, e.home), "--set", "security.instance_key="+base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{3}, 32)),
		"--set", "security.bootstrap_admin.password=a-long-enough-password")

	r := e.run(append([]string{"admin", "doctor"}, flags...)...)
	require.NoError(t, r.err, r.stdout)
	require.NotContains(t, r.stdout, "large reads", "the check runs only when asked for")

	r = e.run(append([]string{"admin", "doctor", "--large-read=2MiB"}, flags...)...)
	require.NoError(t, r.err, r.stdout)
	require.Contains(t, r.stdout, "large reads")
	require.Contains(t, r.stdout, "read 2.0 MiB back in one request")

	store = cuttingStore{Fake: blobtest.NewFake(), cut: 1 << 20}
	r = e.run(append([]string{"admin", "doctor", "--large-read=2MiB"}, flags...)...)
	require.Equal(t, ExitFailure, r.code)
	require.Contains(t, r.stdout, "FAIL")
	require.Contains(t, r.stdout, "stopped after 1.0 MiB")

	require.Equal(t, ExitUsage, e.run(append([]string{"admin", "doctor", "--large-read=lots"}, flags...)...).code)
	require.Equal(t, ExitUsage, e.run(append([]string{"admin", "doctor", "--large-read=0"}, flags...)...).code)
}
