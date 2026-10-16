package repo

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/jamesagarside/hangar/internal/blob"
)

// Defaults for resumingReader: how many reads in a row may fail before it
// gives up, and the wait before the first retry, which doubles each time up
// to maxReadBackoff.
const (
	defaultReadRetries = 5
	defaultReadBackoff = 500 * time.Millisecond
	maxReadBackoff     = 8 * time.Second
)

// resumingReader reads a whole stored object. A Blob store's Get body
// cannot resume: on S3 it is one HTTP response, and a connection dropped
// part way through a large object ends it with an error or a short body.
// When that happens, resumingReader reopens the object with a ranged Get
// from the first byte it has not read yet, so a transient failure costs a
// request instead of the whole read. It gives up after retries reads in a
// row fail without progress, and refuses to continue if the object's size
// or ETag changes underneath it.
type resumingReader struct {
	ctx     context.Context
	blobs   blob.Store
	key     string
	info    blob.Info
	rc      io.ReadCloser
	off     int64
	retries int
	backoff time.Duration
	// failures counts failed reads and reopens since the last byte read.
	failures int
	// err is set when the reader gave up, so callers can tell a failed
	// source read from a failure of whatever consumed the bytes.
	err error
}

// openResuming opens key with an unranged Get and returns a resumingReader
// over it and the object's Info.
func openResuming(ctx context.Context, blobs blob.Store, key string, retries int, backoff time.Duration) (*resumingReader, blob.Info, error) {
	rc, info, err := blobs.Get(ctx, key, nil)
	if err != nil {
		return nil, blob.Info{}, err
	}
	return &resumingReader{ctx: ctx, blobs: blobs, key: key, info: info, rc: rc, retries: retries, backoff: backoff}, info, nil
}

// Read implements io.Reader.
func (r *resumingReader) Read(p []byte) (int, error) {
	if r.err != nil {
		return 0, r.err
	}
	if len(p) == 0 {
		return 0, nil
	}
	for {
		if r.rc == nil {
			if r.off >= r.info.Size {
				return 0, io.EOF
			}
			if err := r.reopen(); err != nil {
				return 0, r.fail(err)
			}
		}
		n, err := r.rc.Read(p)
		r.off += int64(n)
		if n > 0 {
			r.failures = 0
		}
		switch {
		case err == nil:
			return n, nil
		case r.off > r.info.Size:
			return n, r.fail(fmt.Errorf("blob %s is longer than its %d bytes", r.key, r.info.Size))
		case errors.Is(err, io.EOF) && r.off == r.info.Size:
			return n, io.EOF
		case errors.Is(err, io.EOF):
			err = io.ErrUnexpectedEOF
		}
		_ = r.rc.Close()
		r.rc = nil
		if cerr := r.ctx.Err(); cerr != nil {
			return n, r.fail(cerr)
		}
		r.failures++
		if r.failures > r.retries {
			return n, r.fail(fmt.Errorf("read blob %s at byte %d of %d: %w", r.key, r.off, r.info.Size, err))
		}
		if n > 0 {
			// Hand over what was read; the next Read reopens.
			return n, nil
		}
	}
}

// reopen waits, then opens the object again from r.off. A reopen that
// fails counts as a failed read and is retried in the same way.
func (r *resumingReader) reopen() error {
	for {
		wait := r.backoff << max(r.failures-1, 0)
		if wait > maxReadBackoff || wait < 0 {
			wait = maxReadBackoff
		}
		t := time.NewTimer(wait)
		select {
		case <-r.ctx.Done():
			t.Stop()
			return r.ctx.Err()
		case <-t.C:
		}
		rc, info, err := r.blobs.Get(r.ctx, r.key, &blob.Range{Start: r.off, End: -1})
		if err == nil {
			if info.Size != r.info.Size || (info.ETag != "" && r.info.ETag != "" && info.ETag != r.info.ETag) {
				_ = rc.Close()
				return fmt.Errorf("blob %s changed while it was being read", r.key)
			}
			r.rc = rc
			return nil
		}
		if errors.Is(err, blob.ErrNotFound) || errors.Is(err, blob.ErrInvalidRange) || r.ctx.Err() != nil {
			return err
		}
		r.failures++
		if r.failures > r.retries {
			return fmt.Errorf("reopen blob %s at byte %d of %d: %w", r.key, r.off, r.info.Size, err)
		}
	}
}

func (r *resumingReader) fail(err error) error {
	r.err = err
	return err
}

// Close implements io.Closer.
func (r *resumingReader) Close() error {
	if r.rc == nil {
		return nil
	}
	err := r.rc.Close()
	r.rc = nil
	return err
}
