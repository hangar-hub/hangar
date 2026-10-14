// Package blob defines the Blob store: where LFS objects, git packs, upload
// staging, Xet xorbs, avatars and exports live. Backends are a local
// filesystem (fs/) and any S3-compatible store (s3/), with the same key
// layout under an optional prefix (docs/spec/02-architecture.md "Blob store
// layout"). Every backend must pass the conformance suite in blobtest.
package blob

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// Errors returned by every backend.
var (
	// ErrNotFound means the key or multipart upload does not exist.
	ErrNotFound = errors.New("blob: not found")
	// ErrNotSupported means the backend cannot do this, such as presigning
	// on the filesystem backend. Callers fall back to Hangar-served URLs.
	ErrNotSupported = errors.New("blob: not supported by this backend")
	// ErrInvalidRange means the range starts past the end of the object.
	ErrInvalidRange = errors.New("blob: range not satisfiable")
	// ErrSizeMismatch means a Put read a different number of bytes than declared.
	ErrSizeMismatch = errors.New("blob: size does not match")
	// ErrInvalidKey means the key is empty, absolute or contains `..`.
	ErrInvalidKey = errors.New("blob: invalid key")
)

// Info describes a stored object.
type Info struct {
	Key     string
	Size    int64
	ModTime time.Time
	// ETag is the backend's entity tag, without quotes. It is not a content
	// hash on every backend; never use it as one.
	ETag string
}

// Range is a byte range of an object. End is inclusive; -1 means to the end.
type Range struct {
	Start int64
	End   int64
}

// Length returns the number of bytes the range covers in an object of size.
func (r Range) Length(size int64) int64 {
	end := r.End
	if end < 0 || end >= size {
		end = size - 1
	}
	if r.Start > end {
		return 0
	}
	return end - r.Start + 1
}

// Resolve clamps the range to an object of size and returns its first and
// last byte. It returns ErrInvalidRange if the range starts past the end.
func (r Range) Resolve(size int64) (start, end int64, err error) {
	if r.Start < 0 || r.Start >= size {
		return 0, 0, ErrInvalidRange
	}
	end = r.End
	if end < 0 || end >= size {
		end = size - 1
	}
	if end < r.Start {
		return 0, 0, ErrInvalidRange
	}
	return r.Start, end, nil
}

// ContentRange returns the Content-Range header value for the range.
func (r Range) ContentRange(size int64) string {
	start, end, err := r.Resolve(size)
	if err != nil {
		return "bytes */" + strconv.FormatInt(size, 10)
	}
	return fmt.Sprintf("bytes %d-%d/%d", start, end, size)
}

// ParseRange parses a single-range HTTP Range header ("bytes=a-b",
// "bytes=a-" or "bytes=-n") against an object of size. It returns nil and no
// error for an empty header. Multiple ranges are refused, as the Hub
// client never sends them.
func ParseRange(header string, size int64) (*Range, error) {
	if header == "" {
		return nil, nil
	}
	spec, ok := strings.CutPrefix(strings.TrimSpace(header), "bytes=")
	if !ok || strings.Contains(spec, ",") {
		return nil, ErrInvalidRange
	}
	a, b, ok := strings.Cut(spec, "-")
	if !ok {
		return nil, ErrInvalidRange
	}
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	if a == "" {
		n, err := strconv.ParseInt(b, 10, 64)
		if err != nil || n <= 0 {
			return nil, ErrInvalidRange
		}
		if size == 0 {
			return nil, ErrInvalidRange
		}
		return &Range{Start: max(0, size-n), End: size - 1}, nil
	}
	start, err := strconv.ParseInt(a, 10, 64)
	if err != nil || start < 0 {
		return nil, ErrInvalidRange
	}
	r := &Range{Start: start, End: -1}
	if b != "" {
		end, err := strconv.ParseInt(b, 10, 64)
		if err != nil || end < start {
			return nil, ErrInvalidRange
		}
		r.End = end
	}
	if _, _, err := r.Resolve(size); err != nil {
		return nil, err
	}
	return r, nil
}

// Part is a completed part of a multipart upload.
type Part struct {
	// Number is 1-based.
	Number int
	// ETag is the value the part upload returned, passed back verbatim.
	ETag string
	// Size is the part size, when known.
	Size int64
}

// PresignedRequest is a URL and the headers a client must send with it.
type PresignedRequest struct {
	URL     string
	Headers map[string]string
}

// Store is the Blob store. Keys use `/` separators and never start with `/`.
// Implementations must be safe for concurrent use, never expose partly
// written objects to readers, and stream: no method may buffer a whole
// object in memory.
type Store interface {
	// Stat returns the object's metadata, or ErrNotFound.
	Stat(ctx context.Context, key string) (Info, error)
	// Get opens the object, or a range of it. rng nil means the whole
	// object. Info always describes the whole object.
	Get(ctx context.Context, key string, rng *Range) (io.ReadCloser, Info, error)
	// Put stores r under key, replacing any existing object atomically. size
	// is the exact length, or -1 if unknown; with a known size, Put returns
	// ErrSizeMismatch if r yields a different length, and stores nothing.
	Put(ctx context.Context, key string, r io.Reader, size int64) error
	// Delete removes the object. Deleting a missing key is not an error.
	Delete(ctx context.Context, key string) error
	// List calls fn for every object whose key starts with prefix, in key
	// order. Returning an error from fn stops the listing and returns it.
	List(ctx context.Context, prefix string, fn func(Info) error) error

	// CreateMultipart starts a multipart upload to key. S3 uses native
	// multipart; the filesystem backend keeps part files under uploads/.
	CreateMultipart(ctx context.Context, key string) (uploadID string, err error)
	// PresignPart returns a URL a client can PUT part bytes to without
	// credentials. ErrNotSupported on the filesystem backend.
	PresignPart(ctx context.Context, key, uploadID string, part int, ttl time.Duration) (url string, err error)
	// PutPart uploads a part through Hangar and returns its ETag.
	PutPart(ctx context.Context, key, uploadID string, part int, r io.Reader, size int64) (etag string, err error)
	// ListParts returns the parts uploaded so far, in part order, so uploads
	// can resume.
	ListParts(ctx context.Context, key, uploadID string) ([]Part, error)
	// CompleteMultipart assembles the parts into the object at key. The
	// object becomes visible only when this returns successfully.
	CompleteMultipart(ctx context.Context, key, uploadID string, parts []Part) error
	// AbortMultipart discards the upload and its parts.
	AbortMultipart(ctx context.Context, key, uploadID string) error

	// PresignGet returns a URL that downloads the object without credentials,
	// with Content-Disposition naming filename. ErrNotSupported on the
	// filesystem backend.
	PresignGet(ctx context.Context, key string, ttl time.Duration, filename string) (url string, err error)
	// PresignPut returns a URL and headers that upload exactly size bytes to
	// key without credentials. When sha256 is not nil and the backend
	// supports it, the store checks the content against it. ErrNotSupported
	// on the filesystem backend, and above the backend's single PUT limit.
	PresignPut(ctx context.Context, key string, size int64, sha256 []byte, ttl time.Duration) (PresignedRequest, error)

	// Copy copies srcKey to dstKey, server-side where the backend can.
	Copy(ctx context.Context, srcKey, dstKey string) error
}

// ValidateKey checks that key is relative, non-empty and has no `..` or
// empty components.
func ValidateKey(key string) error {
	if key == "" || strings.HasPrefix(key, "/") || strings.HasSuffix(key, "/") || strings.ContainsRune(key, 0) || strings.Contains(key, "\\") {
		return fmt.Errorf("%w: %q", ErrInvalidKey, key)
	}
	for _, part := range strings.Split(key, "/") {
		if part == "" || part == "." || part == ".." {
			return fmt.Errorf("%w: %q", ErrInvalidKey, key)
		}
	}
	return nil
}

// Key prefixes from docs/spec/02-architecture.md "Blob store layout".
const (
	PrefixLFS     = "lfs/sha256/"
	PrefixGit     = "git/"
	PrefixUploads = "uploads/"
	PrefixTmp     = "tmp/"
	PrefixXorbs   = "xet/xorbs/"
	PrefixAvatars = "avatars/"
	PrefixExports = "exports/"
	// SentinelKey is the object /readyz stats to check the store is reachable.
	SentinelKey = "hangar/sentinel"
)

// IncompleteUpload is a multipart upload that was started and neither
// completed nor aborted.
type IncompleteUpload struct {
	Key       string
	UploadID  string
	Initiated time.Time
}

// UploadLister is implemented by stores that can list incomplete multipart
// uploads, so garbage collection can abort abandoned ones. Stores that
// cannot return ErrNotSupported.
type UploadLister interface {
	ListUploads(ctx context.Context, fn func(IncompleteUpload) error) error
}

// StagingSweeper is implemented by stores that stage writes outside the key
// space, such as the filesystem backend's <root>/.staging, where a crash or
// a killed process can leave a partial file that List never shows. Garbage
// collection calls it to remove them. Stores that stage nothing return
// ErrNotSupported.
type StagingSweeper interface {
	// SweepStaging removes staging files last written before cutoff and
	// returns how many it removed. A write in progress keeps its file's
	// modification time current, so a cutoff of hours never touches one.
	SweepStaging(ctx context.Context, cutoff time.Time) (int, error)
}
