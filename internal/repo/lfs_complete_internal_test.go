package repo

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/blob"
	"github.com/jamesagarside/hangar/internal/blob/blobtest"
	"github.com/jamesagarside/hangar/internal/db"
	"github.com/jamesagarside/hangar/internal/db/dbtest"
	"github.com/jamesagarside/hangar/internal/model"
)

// breakingStore is a Blob store whose Gets of one key break part way, the
// way an S3 GET body does when the connection drops: after `after` bytes
// the body returns io.ErrUnexpectedEOF, or ends early with a clean io.EOF
// when short is set. Only the first `breaks` Gets break; the rest read
// normally. The next `fails` ranged Gets of the key fail outright with
// errTransient, the way a reopen does when the store is unreachable.
type breakingStore struct {
	*blobtest.Fake
	key   string
	after int
	short bool

	mu     sync.Mutex
	breaks int
	fails  int
	gets   []*blob.Range
}

var errTransient = errors.New("connection reset by peer")

func (s *breakingStore) setFails(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fails = n
}

func (s *breakingStore) setBreaks(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.breaks = n
}

func (s *breakingStore) ranges() []*blob.Range {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*blob.Range(nil), s.gets...)
}

func (s *breakingStore) Get(ctx context.Context, key string, rng *blob.Range) (io.ReadCloser, blob.Info, error) {
	rc, info, err := s.Fake.Get(ctx, key, rng)
	if err != nil || key != s.key {
		return rc, info, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gets = append(s.gets, rng)
	if rng != nil && s.fails > 0 {
		s.fails--
		_ = rc.Close()
		return nil, blob.Info{}, errTransient
	}
	if s.breaks == 0 {
		return rc, info, nil
	}
	s.breaks--
	return &brokenBody{rc: rc, left: s.after, short: s.short}, info, nil
}

type brokenBody struct {
	rc    io.ReadCloser
	left  int
	short bool
}

func (b *brokenBody) Read(p []byte) (int, error) {
	if b.left == 0 {
		if b.short {
			return 0, io.EOF
		}
		return 0, io.ErrUnexpectedEOF
	}
	n, err := b.rc.Read(p[:min(len(p), b.left)])
	b.left -= n
	return n, err
}

func (b *brokenBody) Close() error { return b.rc.Close() }

// startUpload uploads content in 100-byte parts and returns the upload's ID
// and parts, ready to complete.
func startUpload(t *testing.T, l *LFS, content []byte) (string, *Upload, []blob.Part) {
	t.Helper()
	ctx := context.Background()
	sum := sha256.Sum256(content)
	oid := hex.EncodeToString(sum[:])
	u, err := l.CreateUpload(ctx, UploadParams{OID: oid, Size: int64(len(content)), Multipart: true, PartSize: 100, TTL: time.Hour})
	require.NoError(t, err)
	var parts []blob.Part
	for i := 0; i*100 < len(content); i++ {
		chunk := content[i*100 : min((i+1)*100, len(content))]
		etag, err := l.PutPart(ctx, u.ID, i+1, bytes.NewReader(chunk), int64(len(chunk)))
		require.NoError(t, err)
		parts = append(parts, blob.Part{Number: i + 1, ETag: etag})
	}
	return oid, u, parts
}

func newBreakingLFS(t *testing.T, d *db.DB) (*LFS, *breakingStore) {
	t.Helper()
	bs := &breakingStore{Fake: blobtest.NewFake(), after: 120}
	l := NewLFS(d, bs)
	l.readBackoff = time.Millisecond
	return l, bs
}

func uploadRows(t *testing.T, d *db.DB) int64 {
	t.Helper()
	n, err := d.NewSelect().Model((*db.Upload)(nil)).Count(context.Background())
	require.NoError(t, err)
	return n
}

// TestCompleteUploadResumesABrokenRead is the regression test for a 20 GB
// import that failed with HTTP 500: reading the assembled parts back broke
// part way, and the completion threw the parts away. The read now resumes
// from the byte it reached with a ranged Get.
func TestCompleteUploadResumesABrokenRead(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		short     bool
		threshold int64
	}{
		{name: "unexpected EOF, staged copy", threshold: 64 << 20},
		{name: "unexpected EOF, streamed multipart", threshold: 10},
		{name: "body ends early", short: true, threshold: 64 << 20},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
				ctx := context.Background()
				l, bs := newBreakingLFS(t, d)
				l.streamThreshold = tc.threshold
				bs.short = tc.short
				content := bytes.Repeat([]byte("0123456789"), 25)
				oid, u, parts := startUpload(t, l, content)
				bs.key = blob.PrefixTmp + "uploads/" + u.ID.String()
				bs.setBreaks(2)

				obj, err := l.CompleteUpload(ctx, u.ID, parts)
				require.NoError(t, err)
				require.Equal(t, LFSObject{OID: oid, Size: int64(len(content)), StorageKey: model.LFSObjectKey(oid), Verified: true}, obj)
				got, ok := bs.Bytes(model.LFSObjectKey(oid))
				require.True(t, ok)
				require.Equal(t, content, got)
				// One whole read, then reads from where each broke.
				require.Equal(t, []*blob.Range{nil, {Start: 120, End: -1}, {Start: 240, End: -1}}, bs.ranges())
				_, ok = bs.Bytes(bs.key)
				require.False(t, ok, "the staged object is removed once the object is stored")
				require.Zero(t, uploadRows(t, d))
			})
		})
	}
}

// TestCompleteUploadKeepsPartsWhenTheReadFails covers a read that keeps
// breaking: the completion answers ErrRetryable and keeps the upload and its
// assembled parts, so sending it again stores the object.
func TestCompleteUploadKeepsPartsWhenTheReadFails(t *testing.T) {
	t.Parallel()
	for name, threshold := range map[string]int64{"staged copy": 64 << 20, "streamed multipart": 10} {
		t.Run(name, func(t *testing.T) {
			dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
				ctx := context.Background()
				l, bs := newBreakingLFS(t, d)
				l.streamThreshold = threshold
				content := bytes.Repeat([]byte("0123456789"), 25)
				oid, u, parts := startUpload(t, l, content)
				bs.key = blob.PrefixTmp + "uploads/" + u.ID.String()
				bs.after = 0
				bs.setBreaks(1000)

				_, err := l.CompleteUpload(ctx, u.ID, parts)
				require.ErrorIs(t, err, ErrRetryable)
				require.ErrorIs(t, err, io.ErrUnexpectedEOF)
				require.NotErrorIs(t, err, ErrChecksumMismatch)
				require.Len(t, bs.ranges(), 1+defaultReadRetries)
				_, err = l.Stat(ctx, oid)
				require.ErrorIs(t, err, ErrNotFound)
				staged, ok := bs.Bytes(bs.key)
				require.True(t, ok, "the assembled parts are kept")
				require.Equal(t, content, staged)

				// The upload still exists and reports every part done, so a
				// client that resumes goes straight to completing.
				got, err := l.GetUpload(ctx, u.ID)
				require.NoError(t, err)
				require.Equal(t, []blob.Part{{Number: 1, ETag: parts[0].ETag, Size: 100}, {Number: 2, ETag: parts[1].ETag, Size: 100},
					{Number: 3, ETag: parts[2].ETag, Size: 50}}, got.Done)

				bs.setBreaks(0)
				obj, err := l.CompleteUpload(ctx, u.ID, parts)
				require.NoError(t, err)
				require.Equal(t, oid, obj.OID)
				stored, ok := bs.Bytes(model.LFSObjectKey(oid))
				require.True(t, ok)
				require.Equal(t, content, stored)
				_, ok = bs.Bytes(bs.key)
				require.False(t, ok)
				require.Zero(t, uploadRows(t, d))
				_, err = l.CompleteUpload(ctx, u.ID, parts)
				require.ErrorIs(t, err, ErrNotFound)
			})
		})
	}
}

// TestFailedCompletionIsCleanedUp covers what happens to an upload whose
// completion failed after assembling the parts: aborting it or letting it
// expire removes the assembled object too, and a corrupt one is discarded
// on the next completion.
func TestFailedCompletionIsCleanedUp(t *testing.T) {
	t.Parallel()
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		ctx := context.Background()
		l, bs := newBreakingLFS(t, d)
		bs.after = 0
		fail := func(content []byte, ttl time.Duration) (*Upload, []blob.Part, string) {
			_, u, parts := startUpload(t, l, content)
			bs.key = blob.PrefixTmp + "uploads/" + u.ID.String()
			bs.setBreaks(1000)
			_, err := l.CompleteUpload(ctx, u.ID, parts)
			require.ErrorIs(t, err, ErrRetryable)
			bs.setBreaks(0)
			_, ok := bs.Bytes(bs.key)
			require.True(t, ok)
			if ttl > 0 {
				_, err := d.NewUpdate().Model((*db.Upload)(nil)).Set("expires_at = ?", time.Now().Add(-ttl)).Where("id = ?", u.ID).Exec(ctx)
				require.NoError(t, err)
			}
			return u, parts, bs.key
		}

		u, _, key := fail([]byte("aborted upload"), 0)
		require.NoError(t, l.AbortUpload(ctx, u.ID))
		_, ok := bs.Bytes(key)
		require.False(t, ok)

		_, _, key = fail([]byte("expired upload"), time.Hour)
		n, err := l.ExpireUploads(ctx)
		require.NoError(t, err)
		require.Equal(t, 1, n)
		_, ok = bs.Bytes(key)
		require.False(t, ok)

		u, parts, key := fail([]byte("corrupted upload"), 0)
		require.NoError(t, bs.Put(ctx, key, bytes.NewReader([]byte("something else")), 14))
		_, err = l.CompleteUpload(ctx, u.ID, parts)
		require.ErrorIs(t, err, ErrChecksumMismatch)
		_, ok = bs.Bytes(key)
		require.False(t, ok)

		// A completion whose assembled object has gone asks for the file
		// again instead of failing for ever.
		u, parts, key = fail([]byte("lost upload"), 0)
		require.NoError(t, bs.Delete(ctx, key))
		_, err = l.CompleteUpload(ctx, u.ID, parts)
		require.ErrorIs(t, err, ErrNotFound)
		require.Zero(t, uploadRows(t, d))
	})
}

// TestResumingReader covers the reader's limits: it gives up after its
// retries, stops when the context ends, and refuses an object that changed.
func TestResumingReader(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	bs := &breakingStore{Fake: blobtest.NewFake(), key: "k", after: 3}
	content := []byte("0123456789")
	require.NoError(t, bs.Put(ctx, "k", bytes.NewReader(content), 10))

	// Progress resets the count, so many breaks with progress still finish.
	bs.setBreaks(3)
	rr, info, err := openResuming(ctx, bs, "k", 1, time.Millisecond)
	require.NoError(t, err)
	require.Equal(t, int64(10), info.Size)
	got, err := io.ReadAll(rr)
	require.NoError(t, err)
	require.Equal(t, content, got)
	require.NoError(t, rr.Close())

	// No progress: it gives up after the retries.
	bs.after = 0
	bs.setBreaks(100)
	rr, _, err = openResuming(ctx, bs, "k", 2, time.Millisecond)
	require.NoError(t, err)
	_, err = io.ReadAll(rr)
	require.ErrorIs(t, err, io.ErrUnexpectedEOF)
	require.Equal(t, err, rr.err)
	_, err = rr.Read(make([]byte, 1))
	require.ErrorIs(t, err, io.ErrUnexpectedEOF)

	// A cancelled context stops it at once.
	cctx, cancel := context.WithCancel(ctx)
	rr, _, err = openResuming(cctx, bs, "k", 5, time.Hour)
	require.NoError(t, err)
	cancel()
	_, err = io.ReadAll(rr)
	require.ErrorIs(t, err, context.Canceled)

	// An object replaced while it is read is refused.
	bs.after = 4
	bs.setBreaks(1)
	rr, _, err = openResuming(ctx, bs, "k", 5, time.Millisecond)
	require.NoError(t, err)
	buf := make([]byte, 4)
	_, err = io.ReadFull(rr, buf)
	require.NoError(t, err)
	require.NoError(t, bs.Put(ctx, "k", bytes.NewReader([]byte("abcdefghij")), 10))
	_, err = io.ReadAll(rr)
	require.ErrorContains(t, err, "changed while it was being read")

	// A missing object is not retried.
	require.NoError(t, bs.Delete(ctx, "k"))
	_, _, err = openResuming(ctx, bs, "k", 5, time.Millisecond)
	require.True(t, errors.Is(err, blob.ErrNotFound))
}

// TestResumingReaderReopen covers a reopen that fails: it is retried until
// it works or the retries run out, it stops when the context ends while it
// waits, and it does not retry an object that is gone or has a new size.
func TestResumingReaderReopen(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	content := []byte("0123456789")
	newStore := func(t *testing.T) *breakingStore {
		t.Helper()
		bs := &breakingStore{Fake: blobtest.NewFake(), key: "k", after: 4}
		require.NoError(t, bs.Put(ctx, "k", bytes.NewReader(content), 10))
		bs.setBreaks(1)
		return bs
	}
	// readHead opens k and reads the 4 bytes before the break.
	readHead := func(t *testing.T, ctx context.Context, bs *breakingStore, retries int, backoff time.Duration) *resumingReader {
		t.Helper()
		rr, _, err := openResuming(ctx, bs, "k", retries, backoff)
		require.NoError(t, err)
		buf := make([]byte, 4)
		_, err = io.ReadFull(rr, buf)
		require.NoError(t, err)
		require.Equal(t, content[:4], buf)
		return rr
	}

	t.Run("failed reopens are retried", func(t *testing.T) {
		t.Parallel()
		bs := newStore(t)
		bs.setFails(2)
		rr, _, err := openResuming(ctx, bs, "k", 3, time.Millisecond)
		require.NoError(t, err)
		got, err := io.ReadAll(rr)
		require.NoError(t, err)
		require.Equal(t, content, got)
		require.NoError(t, rr.Close())
		require.NoError(t, rr.Close())
		// The first Get, then three reopens from byte 4: two fail, one works.
		rngs := bs.ranges()
		require.Len(t, rngs, 4)
		require.Nil(t, rngs[0])
		for _, rng := range rngs[1:] {
			require.Equal(t, blob.Range{Start: 4, End: -1}, *rng)
		}
	})

	t.Run("reopens that keep failing give up", func(t *testing.T) {
		t.Parallel()
		bs := newStore(t)
		bs.setFails(100)
		rr := readHead(t, ctx, bs, 2, time.Millisecond)
		_, err := io.ReadAll(rr)
		require.ErrorIs(t, err, errTransient)
		require.ErrorContains(t, err, "reopen blob k at byte 4 of 10")
		// The broken read and two failed reopens use up the two retries.
		require.Len(t, bs.ranges(), 3)
		_, err = rr.Read(make([]byte, 1))
		require.ErrorIs(t, err, errTransient)
		require.NoError(t, rr.Close())
	})

	t.Run("a cancelled context stops the wait", func(t *testing.T) {
		t.Parallel()
		bs := newStore(t)
		cctx, cancel := context.WithCancel(ctx)
		defer cancel()
		rr := readHead(t, cctx, bs, 5, time.Hour)
		time.AfterFunc(20*time.Millisecond, cancel)
		_, err := io.ReadAll(rr)
		require.ErrorIs(t, err, context.Canceled)
		// Only the first Get: it never reopened.
		require.Len(t, bs.ranges(), 1)
	})

	t.Run("an object that is gone is not retried", func(t *testing.T) {
		t.Parallel()
		bs := newStore(t)
		rr := readHead(t, ctx, bs, 5, time.Millisecond)
		require.NoError(t, bs.Delete(ctx, "k"))
		_, err := io.ReadAll(rr)
		require.ErrorIs(t, err, blob.ErrNotFound)
	})

	t.Run("an object with a new size is refused", func(t *testing.T) {
		t.Parallel()
		bs := newStore(t)
		rr := readHead(t, ctx, bs, 5, time.Millisecond)
		require.NoError(t, bs.Put(ctx, "k", bytes.NewReader([]byte("0123456789ab")), 12))
		_, err := io.ReadAll(rr)
		require.ErrorContains(t, err, "blob k changed while it was being read")
	})
}

// TestCompleteUploadCopiesServerSide is the regression test for bellatrix
// finding 13b: completing an upload read the assembled object and wrote
// every byte again to the object's key through Hangar, which took 184
// seconds for a 5 GB shard on a Raspberry Pi and failed when a proxy cut
// the read. The completion now hashes the staged object and copies it into
// place with the Blob store's server-side Copy.
func TestCompleteUploadCopiesServerSide(t *testing.T) {
	t.Parallel()
	for name, threshold := range map[string]int64{"small object": 64 << 20, "large object": 10} {
		t.Run(name, func(t *testing.T) {
			dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
				ctx := context.Background()
				l, bs := newBreakingLFS(t, d)
				l.streamThreshold = threshold
				content := bytes.Repeat([]byte("0123456789"), 25)
				oid, u, parts := startUpload(t, l, content)
				staged := blob.PrefixTmp + "uploads/" + u.ID.String()
				before := map[string]int{}
				for _, m := range []string{"Put", "PutPart", "CreateMultipart", "Copy", "Get"} {
					before[m] = bs.Calls(m)
				}

				obj, err := l.CompleteUpload(ctx, u.ID, parts)
				require.NoError(t, err)
				require.Equal(t, oid, obj.OID)
				require.Equal(t, before["Put"], bs.Calls("Put"), "no bytes are written through Hangar")
				require.Equal(t, before["PutPart"], bs.Calls("PutPart"))
				require.Equal(t, before["CreateMultipart"], bs.Calls("CreateMultipart"))
				require.Equal(t, before["Copy"]+1, bs.Calls("Copy"), "one server-side copy")
				require.Equal(t, before["Get"]+1, bs.Calls("Get"), "one read, to hash")
				stored, ok := bs.Bytes(model.LFSObjectKey(oid))
				require.True(t, ok)
				require.Equal(t, content, stored)
				_, ok = bs.Bytes(staged)
				require.False(t, ok)
			})
		})
	}
}

// failingCopyStore fails every Copy, like a Blob store that is briefly
// unreachable.
type failingCopyStore struct{ *blobtest.Fake }

func (failingCopyStore) Copy(context.Context, string, string) error { return errTransient }

// TestCompleteUploadKeepsPartsWhenTheCopyFails covers bellatrix finding
// 13c for the copy: a completion whose copy fails keeps the upload and its
// assembled parts, so sending it again finishes without uploading again.
func TestCompleteUploadKeepsPartsWhenTheCopyFails(t *testing.T) {
	t.Parallel()
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		ctx := context.Background()
		fake := blobtest.NewFake()
		l := NewLFS(d, failingCopyStore{fake})
		content := bytes.Repeat([]byte("abcdefghij"), 25)
		oid, u, parts := startUpload(t, l, content)
		_, err := l.CompleteUpload(ctx, u.ID, parts)
		require.ErrorIs(t, err, ErrRetryable)
		require.ErrorIs(t, err, errTransient)
		_, err = l.Stat(ctx, oid)
		require.ErrorIs(t, err, ErrNotFound)
		got, err := l.GetUpload(ctx, u.ID)
		require.NoError(t, err)
		require.Len(t, got.Done, 3, "every part is reported done")

		ok := NewLFS(d, fake)
		obj, err := ok.CompleteUpload(ctx, u.ID, parts)
		require.NoError(t, err)
		require.Equal(t, oid, obj.OID)
		require.Zero(t, uploadRows(t, d))
	})
}
