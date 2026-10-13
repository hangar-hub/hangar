// Package blobtest provides an in-memory blob.Store for tests and the
// conformance suite that every Blob store backend must pass.
package blobtest

import (
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // ETags only, as S3 computes them.
	"encoding/hex"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/jamesagarside/hangar/internal/blob"
)

type object struct {
	data    []byte
	modTime time.Time
	etag    string
}

type upload struct {
	key   string
	parts map[int][]byte
}

// Fake is an in-memory blob.Store. It is safe for concurrent use. Set
// PresignBaseURL to make it support presigning, like S3; leave it empty to
// behave like the filesystem backend.
type Fake struct {
	// PresignBaseURL, when set, makes the Presign methods return URLs under
	// it instead of blob.ErrNotSupported.
	PresignBaseURL string
	// MaxPresignPut is the largest presigned PUT, like S3's 5 GiB. Zero
	// means no limit.
	MaxPresignPut int64

	mu      sync.Mutex
	objects map[string]*object
	uploads map[string]*upload
	// Calls counts calls per method name, for assertions.
	calls map[string]int
	now   func() time.Time
}

var _ blob.Store = (*Fake)(nil)

// NewFake returns an empty Fake that does not support presigning.
func NewFake() *Fake {
	return &Fake{objects: map[string]*object{}, uploads: map[string]*upload{}, calls: map[string]int{}, now: time.Now}
}

// Calls returns how many times method was called.
func (f *Fake) Calls(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[method]
}

// Keys returns every stored key in order.
func (f *Fake) Keys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	keys := make([]string, 0, len(f.objects))
	for k := range f.objects {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Bytes returns a copy of the object at key, for assertions.
func (f *Fake) Bytes(key string) ([]byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	o, ok := f.objects[key]
	if !ok {
		return nil, false
	}
	return bytes.Clone(o.data), true
}

func (f *Fake) call(method string) {
	f.mu.Lock()
	f.calls[method]++
	f.mu.Unlock()
}

func etagOf(b []byte) string {
	sum := md5.Sum(b) //nolint:gosec // ETags only.
	return hex.EncodeToString(sum[:])
}

// Stat implements blob.Store.
func (f *Fake) Stat(ctx context.Context, key string) (blob.Info, error) {
	f.call("Stat")
	if err := ctx.Err(); err != nil {
		return blob.Info{}, err
	}
	if err := blob.ValidateKey(key); err != nil {
		return blob.Info{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	o, ok := f.objects[key]
	if !ok {
		return blob.Info{}, fmt.Errorf("%w: %s", blob.ErrNotFound, key)
	}
	return blob.Info{Key: key, Size: int64(len(o.data)), ModTime: o.modTime, ETag: o.etag}, nil
}

// Get implements blob.Store.
func (f *Fake) Get(ctx context.Context, key string, rng *blob.Range) (io.ReadCloser, blob.Info, error) {
	f.call("Get")
	info, err := f.Stat(ctx, key)
	if err != nil {
		return nil, blob.Info{}, err
	}
	f.mu.Lock()
	data := f.objects[key].data
	f.mu.Unlock()
	if rng != nil {
		start, end, err := rng.Resolve(int64(len(data)))
		if err != nil {
			return nil, info, err
		}
		data = data[start : end+1]
	}
	return io.NopCloser(bytes.NewReader(bytes.Clone(data))), info, nil
}

// Put implements blob.Store.
func (f *Fake) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	f.call("Put")
	if err := blob.ValidateKey(key); err != nil {
		return err
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if size >= 0 && int64(len(data)) != size {
		return fmt.Errorf("%w: read %d bytes, want %d", blob.ErrSizeMismatch, len(data), size)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[key] = &object{data: data, modTime: f.now(), etag: etagOf(data)}
	return nil
}

// Delete implements blob.Store.
func (f *Fake) Delete(ctx context.Context, key string) error {
	f.call("Delete")
	if err := blob.ValidateKey(key); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.objects, key)
	return ctx.Err()
}

// List implements blob.Store.
func (f *Fake) List(ctx context.Context, prefix string, fn func(blob.Info) error) error {
	f.call("List")
	f.mu.Lock()
	var infos []blob.Info
	for k, o := range f.objects {
		if strings.HasPrefix(k, prefix) {
			infos = append(infos, blob.Info{Key: k, Size: int64(len(o.data)), ModTime: o.modTime, ETag: o.etag})
		}
	}
	f.mu.Unlock()
	sort.Slice(infos, func(i, j int) bool { return infos[i].Key < infos[j].Key })
	for _, info := range infos {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := fn(info); err != nil {
			return err
		}
	}
	return nil
}

// CreateMultipart implements blob.Store.
func (f *Fake) CreateMultipart(_ context.Context, key string) (string, error) {
	f.call("CreateMultipart")
	if err := blob.ValidateKey(key); err != nil {
		return "", err
	}
	id := uuid.NewString()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.uploads[id] = &upload{key: key, parts: map[int][]byte{}}
	return id, nil
}

func (f *Fake) upload(key, uploadID string) (*upload, error) {
	u, ok := f.uploads[uploadID]
	if !ok || u.key != key {
		return nil, fmt.Errorf("%w: upload %s", blob.ErrNotFound, uploadID)
	}
	return u, nil
}

// PresignPart implements blob.Store.
func (f *Fake) PresignPart(_ context.Context, key, uploadID string, part int, ttl time.Duration) (string, error) {
	f.call("PresignPart")
	if f.PresignBaseURL == "" {
		return "", blob.ErrNotSupported
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, err := f.upload(key, uploadID); err != nil {
		return "", err
	}
	q := url.Values{"partNumber": {strconv.Itoa(part)}, "uploadId": {uploadID}, "X-Expires": {strconv.FormatInt(f.now().Add(ttl).Unix(), 10)}}
	return strings.TrimRight(f.PresignBaseURL, "/") + "/" + key + "?" + q.Encode(), nil
}

// PutPart implements blob.Store.
func (f *Fake) PutPart(_ context.Context, key, uploadID string, part int, r io.Reader, size int64) (string, error) {
	f.call("PutPart")
	if part < 1 || part > 10000 {
		return "", fmt.Errorf("part number %d out of range 1-10000", part)
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return "", err
	}
	if size >= 0 && int64(len(data)) != size {
		return "", fmt.Errorf("%w: part %d read %d bytes, want %d", blob.ErrSizeMismatch, part, len(data), size)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	u, err := f.upload(key, uploadID)
	if err != nil {
		return "", err
	}
	u.parts[part] = data
	return `"` + etagOf(data) + `"`, nil
}

// ListParts implements blob.Store.
func (f *Fake) ListParts(_ context.Context, key, uploadID string) ([]blob.Part, error) {
	f.call("ListParts")
	f.mu.Lock()
	defer f.mu.Unlock()
	u, err := f.upload(key, uploadID)
	if err != nil {
		return nil, err
	}
	parts := make([]blob.Part, 0, len(u.parts))
	for n, d := range u.parts {
		parts = append(parts, blob.Part{Number: n, ETag: `"` + etagOf(d) + `"`, Size: int64(len(d))})
	}
	sort.Slice(parts, func(i, j int) bool { return parts[i].Number < parts[j].Number })
	return parts, nil
}

// CompleteMultipart implements blob.Store. Like S3, it checks that the
// listed parts exist with matching ETags and are in ascending order.
func (f *Fake) CompleteMultipart(_ context.Context, key, uploadID string, parts []blob.Part) error {
	f.call("CompleteMultipart")
	f.mu.Lock()
	defer f.mu.Unlock()
	u, err := f.upload(key, uploadID)
	if err != nil {
		return err
	}
	if len(parts) == 0 {
		return fmt.Errorf("complete multipart: no parts")
	}
	var buf bytes.Buffer
	prev := 0
	for _, p := range parts {
		if p.Number <= prev {
			return fmt.Errorf("complete multipart: parts must be in ascending order")
		}
		prev = p.Number
		d, ok := u.parts[p.Number]
		if !ok {
			return fmt.Errorf("%w: part %d", blob.ErrNotFound, p.Number)
		}
		if strings.Trim(p.ETag, `"`) != etagOf(d) {
			return fmt.Errorf("complete multipart: part %d ETag does not match", p.Number)
		}
		buf.Write(d)
	}
	f.objects[key] = &object{data: buf.Bytes(), modTime: f.now(), etag: etagOf(buf.Bytes())}
	delete(f.uploads, uploadID)
	return nil
}

// AbortMultipart implements blob.Store.
func (f *Fake) AbortMultipart(_ context.Context, key, uploadID string) error {
	f.call("AbortMultipart")
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, err := f.upload(key, uploadID); err != nil {
		return err
	}
	delete(f.uploads, uploadID)
	return nil
}

// PresignGet implements blob.Store.
func (f *Fake) PresignGet(ctx context.Context, key string, ttl time.Duration, filename string) (string, error) {
	f.call("PresignGet")
	if f.PresignBaseURL == "" {
		return "", blob.ErrNotSupported
	}
	if _, err := f.Stat(ctx, key); err != nil {
		return "", err
	}
	q := url.Values{"X-Expires": {strconv.FormatInt(f.now().Add(ttl).Unix(), 10)}}
	if filename != "" {
		q.Set("response-content-disposition", `attachment; filename="`+filename+`"`)
	}
	return strings.TrimRight(f.PresignBaseURL, "/") + "/" + key + "?" + q.Encode(), nil
}

// PresignPut implements blob.Store.
func (f *Fake) PresignPut(_ context.Context, key string, size int64, sha256 []byte, ttl time.Duration) (blob.PresignedRequest, error) {
	f.call("PresignPut")
	if f.PresignBaseURL == "" || (f.MaxPresignPut > 0 && size > f.MaxPresignPut) {
		return blob.PresignedRequest{}, blob.ErrNotSupported
	}
	if err := blob.ValidateKey(key); err != nil {
		return blob.PresignedRequest{}, err
	}
	q := url.Values{"X-Expires": {strconv.FormatInt(f.now().Add(ttl).Unix(), 10)}}
	req := blob.PresignedRequest{URL: strings.TrimRight(f.PresignBaseURL, "/") + "/" + key + "?" + q.Encode(), Headers: map[string]string{}}
	if sha256 != nil {
		req.Headers["x-amz-checksum-sha256"] = hex.EncodeToString(sha256)
	}
	return req, nil
}

// Copy implements blob.Store.
func (f *Fake) Copy(ctx context.Context, srcKey, dstKey string) error {
	f.call("Copy")
	if err := blob.ValidateKey(dstKey); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	o, ok := f.objects[srcKey]
	if !ok {
		return fmt.Errorf("%w: %s", blob.ErrNotFound, srcKey)
	}
	f.objects[dstKey] = &object{data: bytes.Clone(o.data), modTime: f.now(), etag: o.etag}
	return ctx.Err()
}
