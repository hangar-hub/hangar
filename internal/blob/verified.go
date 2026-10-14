package blob

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
)

// Size limits shared by every backend. They are S3's limits, so an object
// that fits the filesystem backend also fits S3 (UP-5).
const (
	// MaxObjectSize is the largest object: 5 TiB.
	MaxObjectSize int64 = 5 << 40
	// MaxSinglePut is the largest single-request upload: 5 GiB.
	MaxSinglePut int64 = 5 << 30
	// MaxParts is the most parts a multipart upload can have.
	MaxParts = 10000
	// MinPartSize is the smallest non-final part S3 accepts.
	MinPartSize int64 = 5 << 20
	// MaxPartSize is the largest part S3 accepts.
	MaxPartSize int64 = 5 << 30
	// DefaultPartSize is the part size for streamed uploads of objects
	// that fit in MaxParts parts of this size (docs/spec/04-git.md).
	DefaultPartSize int64 = 16 << 20
)

// ErrChecksumMismatch means the content's SHA-256 is not the expected one.
var ErrChecksumMismatch = errors.New("blob: SHA-256 does not match")

// ErrTooLarge means the object is larger than MaxObjectSize.
var ErrTooLarge = errors.New("blob: object is larger than 5 TiB")

// PartSize returns the part size for streaming an object of size bytes
// through a multipart upload: max(16 MiB, ceil(size/10000)), so any object
// up to MaxObjectSize fits in MaxParts parts.
func PartSize(size int64) int64 {
	if size <= 0 {
		return DefaultPartSize
	}
	return max(DefaultPartSize, (size+MaxParts-1)/MaxParts)
}

// VerifiedPutter is implemented by stores that can check a SHA-256 while
// they stream a Put, without a second pass over the data.
type VerifiedPutter interface {
	// PutVerified stores exactly size bytes from r under key only if their
	// SHA-256 is sum. On a mismatch it stores nothing and returns
	// ErrChecksumMismatch; on a different length, ErrSizeMismatch.
	PutVerified(ctx context.Context, key string, r io.Reader, size int64, sum []byte) error
}

// PutVerified streams exactly size bytes from r into key and makes the
// object visible only if their SHA-256 is sum. It never buffers the whole
// object: stores that implement VerifiedPutter check the hash as they
// write, and the rest stream through a multipart upload that is completed
// only when the hash matches, and aborted otherwise. LFS uploads, Pulls and
// Xet reassembly use it so a corrupt object is never visible.
func PutVerified(ctx context.Context, s Store, key string, r io.Reader, size int64, sum []byte) error {
	if len(sum) != sha256.Size {
		return fmt.Errorf("put verified %s: want a %d-byte SHA-256, got %d bytes", key, sha256.Size, len(sum))
	}
	if size < 0 {
		return fmt.Errorf("put verified %s: the size must be known", key)
	}
	if size > MaxObjectSize {
		return ErrTooLarge
	}
	if vp, ok := s.(VerifiedPutter); ok {
		return vp.PutVerified(ctx, key, r, size, sum)
	}
	return PutVerifiedMultipart(ctx, s, key, r, size, sum)
}

// PutVerifiedMultipart is the generic PutVerified: it streams r into a
// multipart upload in PartSize(size) parts, hashing as it goes, and
// completes the upload only if the size and SHA-256 match. Backends call it
// from their own PutVerified when the object is too large to check first.
func PutVerifiedMultipart(ctx context.Context, s Store, key string, r io.Reader, size int64, sum []byte) error {
	if len(sum) != sha256.Size {
		return fmt.Errorf("put verified %s: want a %d-byte SHA-256, got %d bytes", key, sha256.Size, len(sum))
	}
	return putMultipart(ctx, s, key, r, size, sum)
}

// PutMultipart streams exactly size bytes from r into key through a
// multipart upload in PartSize(size) parts, holding no more than one part's
// read buffer in memory. The object becomes visible only if r yields
// exactly size bytes. Backends use it for Puts too large for one request.
func PutMultipart(ctx context.Context, s Store, key string, r io.Reader, size int64) error {
	return putMultipart(ctx, s, key, r, size, nil)
}

func putMultipart(ctx context.Context, s Store, key string, r io.Reader, size int64, sum []byte) error {
	if size < 0 {
		return fmt.Errorf("put %s: the size must be known", key)
	}
	if size > MaxObjectSize {
		return ErrTooLarge
	}
	if err := ValidateKey(key); err != nil {
		return err
	}
	if size == 0 {
		if err := expectEOF(r); err != nil {
			return err
		}
		if empty := sha256.Sum256(nil); sum != nil && !bytes.Equal(empty[:], sum) {
			return fmt.Errorf("%w: got %s, want %s", ErrChecksumMismatch, hex.EncodeToString(empty[:]), hex.EncodeToString(sum))
		}
		return s.Put(ctx, key, bytes.NewReader(nil), 0)
	}
	id, err := s.CreateMultipart(ctx, key)
	if err != nil {
		return err
	}
	abort := func(cause error) error {
		if aerr := s.AbortMultipart(context.WithoutCancel(ctx), key, id); aerr != nil && !errors.Is(aerr, ErrNotFound) {
			return errors.Join(cause, fmt.Errorf("abort multipart upload: %w", aerr))
		}
		return cause
	}
	h := sha256.New()
	src := io.TeeReader(r, h)
	partSize := PartSize(size)
	var parts []Part
	remaining := size
	for n := 1; remaining > 0; n++ {
		if n > MaxParts {
			return abort(ErrTooLarge)
		}
		want := min(partSize, remaining)
		lr := &countingReader{r: io.LimitReader(src, want)}
		etag, err := s.PutPart(ctx, key, id, n, lr, want)
		if err != nil {
			if lr.eof && lr.n < want && !errors.Is(err, ErrSizeMismatch) {
				err = fmt.Errorf("%w: the body ended after %d bytes, want %d", ErrSizeMismatch, size-remaining+lr.n, size)
			}
			return abort(err)
		}
		parts = append(parts, Part{Number: n, ETag: etag, Size: want})
		remaining -= want
	}
	if err := expectEOF(src); err != nil {
		return abort(err)
	}
	if sum != nil && !bytes.Equal(h.Sum(nil), sum) {
		return abort(fmt.Errorf("%w: got %s, want %s", ErrChecksumMismatch, hex.EncodeToString(h.Sum(nil)), hex.EncodeToString(sum)))
	}
	if err := s.CompleteMultipart(ctx, key, id, parts); err != nil {
		return abort(err)
	}
	return nil
}

// expectEOF returns ErrSizeMismatch if r has more bytes.
func expectEOF(r io.Reader) error {
	var b [1]byte
	n, err := io.ReadFull(r, b[:])
	if n > 0 {
		return fmt.Errorf("%w: the body is longer than declared", ErrSizeMismatch)
	}
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return err
	}
	return nil
}

type countingReader struct {
	r   io.Reader
	n   int64
	eof bool
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	if errors.Is(err, io.EOF) {
		c.eof = true
	}
	return n, err
}

// HashingReader hashes what it reads and counts the bytes. Backends use it
// to check sizes and SHA-256 values while streaming.
type HashingReader struct {
	r io.Reader
	h hash.Hash
	n int64
}

// NewHashingReader returns a reader that computes the SHA-256 of r.
func NewHashingReader(r io.Reader) *HashingReader {
	return &HashingReader{r: r, h: sha256.New()}
}

// Read implements io.Reader.
func (h *HashingReader) Read(p []byte) (int, error) {
	n, err := h.r.Read(p)
	if n > 0 {
		h.h.Write(p[:n])
		h.n += int64(n)
	}
	return n, err
}

// Sum returns the SHA-256 of the bytes read so far.
func (h *HashingReader) Sum() []byte { return h.h.Sum(nil) }

// N returns the number of bytes read so far.
func (h *HashingReader) N() int64 { return h.n }

// ContextReader returns a reader that fails with the context's error once
// ctx ends, so a long copy stops when the client goes away.
func ContextReader(ctx context.Context, r io.Reader) io.Reader {
	return &ctxReader{ctx: ctx, r: r}
}

type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
