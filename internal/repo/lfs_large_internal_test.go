package repo

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"math/rand/v2"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/blob"
	"github.com/hangar-hub/hangar/internal/blob/blobtest"
	"github.com/hangar-hub/hangar/internal/db"
	"github.com/hangar-hub/hangar/internal/db/dbtest"
	"github.com/hangar-hub/hangar/internal/model"
)

// largeSize is just over 2 GiB, so offsets pass 2^31 and the object
// spans three 1 GiB stretches.
const largeSize = 2<<30 + 12345

// nginxTempFileLimit is nginx's default proxy_max_temp_file_size. On
// bellatrix, MinIO behind Nginx Proxy Manager cut every read of a large
// object by a slow reader at this offset (finding 13).
const nginxTempFileLimit = 1 << 30

// pattern is the content of a generated object: these bytes repeated.
// Its length is not a power of two, so it does not line up with parts or
// with the place reads are cut.
var pattern = func() []byte {
	b := make([]byte, 1<<20+7)
	r := rand.New(rand.NewPCG(13, 8)) //nolint:gosec // test content, not a secret.
	for i := range b {
		b[i] = byte(r.Uint32())
	}
	return b
}()

// patternReader yields the generated object from off to size, at most
// chunk bytes per Read, like a slow network body. After cut bytes it
// fails with io.ErrUnexpectedEOF, as a body does when a proxy closes the
// connection; cut 0 never cuts.
type patternReader struct {
	off, size int64
	cut       int64
	chunk     int
	broken    bool
}

func (p *patternReader) Read(b []byte) (int, error) {
	if p.broken {
		return 0, io.ErrUnexpectedEOF
	}
	if p.off >= p.size {
		return 0, io.EOF
	}
	want := min(len(b), p.chunk, int(p.size-p.off))
	if p.cut > 0 {
		want = min(want, int(p.cut))
	}
	n := 0
	for n < want {
		c := copy(b[n:want], pattern[p.off%int64(len(pattern)):])
		n += c
		p.off += int64(c)
	}
	if p.cut > 0 {
		p.cut -= int64(n)
		if p.cut == 0 && p.off < p.size {
			p.broken = true
			return n, io.ErrUnexpectedEOF
		}
	}
	return n, nil
}

func (p *patternReader) Close() error { return nil }

// largeOID is the SHA-256 of the generated object of largeSize bytes.
var largeOID = sync.OnceValue(func() string {
	h := sha256.New()
	_, _ = io.Copy(h, &patternReader{size: largeSize, chunk: 1 << 20})
	return hex.EncodeToString(h.Sum(nil))
})

// generatedStore is a Blob store that holds one large object at key
// without keeping its bytes: CompleteMultipart of key "assembles" it,
// Get reads it back generated, cut after cut bytes per response, and
// Copy of it records the copy. Everything else goes to the Fake.
type generatedStore struct {
	*blobtest.Fake
	key  string
	size int64
	cut  int64

	mu        sync.Mutex
	assembled bool
	copied    map[string]bool
	gets      []*blob.Range
	readBytes int64
}

func (g *generatedStore) info(key string) blob.Info {
	return blob.Info{Key: key, Size: g.size, ETag: `"generated-2"`, ModTime: time.Unix(1, 0)}
}

func (g *generatedStore) Stat(ctx context.Context, key string) (blob.Info, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if key == g.key && g.assembled || g.copied[key] {
		return g.info(key), nil
	}
	return g.Fake.Stat(ctx, key)
}

func (g *generatedStore) CompleteMultipart(ctx context.Context, key, uploadID string, parts []blob.Part) error {
	if key != g.key {
		return g.Fake.CompleteMultipart(ctx, key, uploadID, parts)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.assembled = true
	return nil
}

func (g *generatedStore) Get(ctx context.Context, key string, rng *blob.Range) (io.ReadCloser, blob.Info, error) {
	if key != g.key {
		return g.Fake.Get(ctx, key, rng)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.assembled {
		return nil, blob.Info{}, blob.ErrNotFound
	}
	g.gets = append(g.gets, rng)
	start := int64(0)
	if rng != nil {
		start = rng.Start
	}
	return &countingBody{patternReader: patternReader{off: start, size: g.size, cut: g.cut, chunk: 256 << 10}, g: g}, g.info(key), nil
}

func (g *generatedStore) Copy(ctx context.Context, src, dst string) error {
	if src != g.key {
		return g.Fake.Copy(ctx, src, dst)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.assembled {
		return blob.ErrNotFound
	}
	if g.copied == nil {
		g.copied = map[string]bool{}
	}
	g.copied[dst] = true
	return nil
}

func (g *generatedStore) Delete(ctx context.Context, key string) error {
	if key == g.key {
		g.mu.Lock()
		g.assembled = false
		g.mu.Unlock()
		return nil
	}
	return g.Fake.Delete(ctx, key)
}

// countingBody counts the bytes read through the store.
type countingBody struct {
	patternReader
	g *generatedStore
}

func (c *countingBody) Read(b []byte) (int, error) {
	n, err := c.patternReader.Read(b)
	c.g.mu.Lock()
	c.g.readBytes += int64(n)
	c.g.mu.Unlock()
	return n, err
}

// TestCompleteUploadOfALargeObjectThroughACuttingProxy is the large-object
// test bellatrix finding 13 asked for: a file over 2 GiB, uploaded in
// 16 MiB parts, whose read back from the Blob store is cut every 1 GiB, as
// nginx's default proxy_max_temp_file_size did to a slow reader on
// bellatrix ("upload part 65/66: unexpected EOF"). The completion resumes
// the read from where each cut fell, hashes every byte once, copies the
// object into place server-side and writes nothing through Hangar.
func TestCompleteUploadOfALargeObjectThroughACuttingProxy(t *testing.T) {
	t.Parallel()
	oid := largeOID()
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		ctx := context.Background()
		gs := &generatedStore{Fake: blobtest.NewFake(), size: largeSize, cut: nginxTempFileLimit}
		l := NewLFS(d, gs)
		l.readBackoff = time.Millisecond
		const partSize = 16 << 20
		u, err := l.CreateUpload(ctx, UploadParams{OID: oid, Size: largeSize, Multipart: true, PartSize: partSize, TTL: time.Hour})
		require.NoError(t, err)
		gs.key = blob.PrefixTmp + "uploads/" + u.ID.String()
		count := (largeSize + partSize - 1) / partSize
		parts := make([]blob.Part, 0, count)
		for n := 1; n <= count; n++ {
			parts = append(parts, blob.Part{Number: n, ETag: fmt.Sprintf(`"part-%d"`, n)})
		}

		obj, err := l.CompleteUpload(ctx, u.ID, parts)
		require.NoError(t, err)
		require.Equal(t, LFSObject{OID: oid, Size: largeSize, StorageKey: model.LFSObjectKey(oid), Verified: true}, obj)
		require.Equal(t, []*blob.Range{nil, {Start: 1 << 30, End: -1}, {Start: 2 << 30, End: -1}}, gs.gets,
			"one whole read, then one from each cut")
		require.Equal(t, int64(largeSize), gs.readBytes, "every byte is read once")
		require.True(t, gs.copied[model.LFSObjectKey(oid)], "the object is copied into place server-side")
		for _, m := range []string{"Put", "PutPart"} {
			require.Zero(t, gs.Calls(m), "no bytes are written through Hangar (%s)", m)
		}
		_, err = gs.Stat(ctx, gs.key)
		require.ErrorIs(t, err, blob.ErrNotFound, "the staged object is removed")
		require.Zero(t, uploadRows(t, d))
	})
}

// TestCompleteUploadOfALargeObjectKeepsItWhenReadsKeepBreaking checks the
// other side: when every read of a 2 GiB+ object breaks without progress
// more often than the reader retries, the completion answers ErrRetryable
// and keeps the assembled object, so it is not uploaded again.
func TestCompleteUploadOfALargeObjectKeepsItWhenReadsKeepBreaking(t *testing.T) {
	t.Parallel()
	oid := largeOID()
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		ctx := context.Background()
		// Every response is cut after its first byte, and the ranged
		// reopens fail outright.
		gs := &generatedStore{Fake: blobtest.NewFake(), size: largeSize, cut: 1}
		bs := &failingReopens{generatedStore: gs}
		l := NewLFS(d, bs)
		l.readBackoff = time.Millisecond
		const partSize = 512 << 20
		u, err := l.CreateUpload(ctx, UploadParams{OID: oid, Size: largeSize, Multipart: true, PartSize: partSize, TTL: time.Hour})
		require.NoError(t, err)
		gs.key = blob.PrefixTmp + "uploads/" + u.ID.String()
		parts := []blob.Part{{Number: 1, ETag: `"1"`}, {Number: 2, ETag: `"2"`}, {Number: 3, ETag: `"3"`}, {Number: 4, ETag: `"4"`}, {Number: 5, ETag: `"5"`}}

		_, err = l.CompleteUpload(ctx, u.ID, parts)
		require.ErrorIs(t, err, ErrRetryable)
		_, err = gs.Stat(ctx, gs.key)
		require.NoError(t, err, "the assembled object is kept")
		got, err := l.GetUpload(ctx, u.ID)
		require.NoError(t, err)
		require.Len(t, got.Done, 5, "every part is reported done, so a client goes straight to completing")
		require.Equal(t, int64(1), gs.readBytes)
	})
}

// failingReopens fails every ranged Get, like a store that is unreachable
// after the first read broke.
type failingReopens struct {
	*generatedStore
}

func (f *failingReopens) Get(ctx context.Context, key string, rng *blob.Range) (io.ReadCloser, blob.Info, error) {
	if key == f.key && rng != nil {
		return nil, blob.Info{}, errTransient
	}
	return f.generatedStore.Get(ctx, key, rng)
}
