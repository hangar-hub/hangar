package repotest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/jamesagarside/hangar/internal/blob"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/repo"
)

// FakeLFS is an in-memory repo.LFSStore over a blob.Store (usually a
// blobtest.Fake). It verifies SHA-256 and size exactly as the real store
// must, so tests of upload handlers catch corrupt uploads.
type FakeLFS struct {
	Store blob.Store

	mu      sync.Mutex
	objects map[string]repo.LFSObject
	uploads map[uuid.UUID]*fakeUpload
	now     func() time.Time
}

type fakeUpload struct {
	repo.Upload
	key      string
	uploadID string
	repoUUID uuid.UUID
}

var _ repo.LFSStore = (*FakeLFS)(nil)

// NewFakeLFS returns an empty FakeLFS writing to store.
func NewFakeLFS(store blob.Store) *FakeLFS {
	return &FakeLFS{Store: store, objects: map[string]repo.LFSObject{}, uploads: map[uuid.UUID]*fakeUpload{}, now: time.Now}
}

// Put stores content as a verified object and returns its OID, for test setup.
func (f *FakeLFS) Put(ctx context.Context, content []byte) (repo.LFSObject, error) {
	sum := sha256.Sum256(content)
	return f.Ingest(ctx, hex.EncodeToString(sum[:]), int64(len(content)), bytes.NewReader(content))
}

// Stat implements repo.LFSStore.
func (f *FakeLFS) Stat(_ context.Context, oid string) (repo.LFSObject, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	o, ok := f.objects[oid]
	if !ok {
		return repo.LFSObject{}, fmt.Errorf("%w: lfs object %s", repo.ErrNotFound, oid)
	}
	return o, nil
}

// StatMany implements repo.LFSStore.
func (f *FakeLFS) StatMany(_ context.Context, oids []string) (map[string]repo.LFSObject, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]repo.LFSObject{}
	for _, oid := range oids {
		if o, ok := f.objects[oid]; ok {
			out[oid] = o
		}
	}
	return out, nil
}

// Ingest implements repo.LFSStore. It hashes while it writes to a temporary
// key and moves the object into place only if it matches.
func (f *FakeLFS) Ingest(ctx context.Context, oid string, size int64, r io.Reader) (repo.LFSObject, error) {
	if !model.IsSHA256(oid) {
		return repo.LFSObject{}, fmt.Errorf("%w: invalid oid %q", repo.ErrInvalid, oid)
	}
	h := sha256.New()
	tmp := blob.PrefixTmp + uuid.NewString()
	if err := f.Store.Put(ctx, tmp, io.TeeReader(r, h), size); err != nil {
		_ = f.Store.Delete(ctx, tmp)
		if errors.Is(err, blob.ErrSizeMismatch) {
			return repo.LFSObject{}, fmt.Errorf("%w: %w", repo.ErrChecksumMismatch, err)
		}
		return repo.LFSObject{}, err
	}
	defer func() { _ = f.Store.Delete(context.WithoutCancel(ctx), tmp) }()
	if hex.EncodeToString(h.Sum(nil)) != oid {
		return repo.LFSObject{}, fmt.Errorf("%w: sha256 does not match %s", repo.ErrChecksumMismatch, oid)
	}
	key := model.LFSObjectKey(oid)
	if err := f.Store.Copy(ctx, tmp, key); err != nil {
		return repo.LFSObject{}, err
	}
	return f.record(oid, size, key), nil
}

func (f *FakeLFS) record(oid string, size int64, key string) repo.LFSObject {
	o := repo.LFSObject{OID: oid, Size: size, StorageKey: key, Verified: true}
	f.mu.Lock()
	f.objects[oid] = o
	f.mu.Unlock()
	return o
}

// CreateUpload implements repo.LFSStore.
func (f *FakeLFS) CreateUpload(ctx context.Context, p repo.UploadParams) (*repo.Upload, error) {
	if !model.IsSHA256(p.OID) || p.Size < 0 {
		return nil, fmt.Errorf("%w: invalid oid or size", repo.ErrInvalid)
	}
	ttl := p.TTL
	if ttl == 0 {
		ttl = 24 * time.Hour
	}
	u := &fakeUpload{Upload: repo.Upload{ID: uuid.New(), OID: p.OID, Size: p.Size, ExpiresAt: f.now().Add(ttl)}, key: model.LFSObjectKey(p.OID), repoUUID: p.RepoUUID}
	if p.Multipart {
		if p.PartSize <= 0 {
			return nil, fmt.Errorf("%w: multipart uploads need a part size", repo.ErrInvalid)
		}
		u.PartSize = p.PartSize
		u.PartCount = int((p.Size + p.PartSize - 1) / p.PartSize)
		// Parts go to a staging key, so a corrupt object never appears at its OID.
		u.key = blob.PrefixTmp + "uploads/" + u.ID.String()
		id, err := f.Store.CreateMultipart(ctx, u.key)
		if err != nil {
			return nil, err
		}
		u.uploadID = id
	}
	f.mu.Lock()
	f.uploads[u.ID] = u
	f.mu.Unlock()
	out := u.Upload
	return &out, nil
}

func (f *FakeLFS) upload(id uuid.UUID) (*fakeUpload, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.uploads[id]
	if !ok {
		return nil, fmt.Errorf("%w: upload %s", repo.ErrNotFound, id)
	}
	return u, nil
}

// GetUpload implements repo.LFSStore.
func (f *FakeLFS) GetUpload(ctx context.Context, id uuid.UUID) (*repo.Upload, error) {
	u, err := f.upload(id)
	if err != nil {
		return nil, err
	}
	out := u.Upload
	if u.uploadID != "" {
		parts, err := f.Store.ListParts(ctx, u.key, u.uploadID)
		if err != nil {
			return nil, err
		}
		out.Done = parts
	}
	return &out, nil
}

// UploadRepo implements repo.LFSStore.
func (f *FakeLFS) UploadRepo(_ context.Context, id uuid.UUID) (uuid.UUID, error) {
	u, err := f.upload(id)
	if err != nil {
		return uuid.Nil, err
	}
	return u.repoUUID, nil
}

// PresignPart implements repo.LFSStore.
func (f *FakeLFS) PresignPart(ctx context.Context, id uuid.UUID, part int, ttl time.Duration) (string, error) {
	u, err := f.upload(id)
	if err != nil {
		return "", err
	}
	if u.uploadID == "" {
		return "", fmt.Errorf("%w: not a multipart upload", repo.ErrInvalid)
	}
	return f.Store.PresignPart(ctx, u.key, u.uploadID, part, ttl)
}

// PutPart implements repo.LFSStore.
func (f *FakeLFS) PutPart(ctx context.Context, id uuid.UUID, part int, r io.Reader, size int64) (string, error) {
	u, err := f.upload(id)
	if err != nil {
		return "", err
	}
	if u.uploadID == "" {
		return "", fmt.Errorf("%w: not a multipart upload", repo.ErrInvalid)
	}
	if part < 1 || part > u.PartCount {
		return "", fmt.Errorf("%w: part %d is outside 1-%d", repo.ErrInvalid, part, u.PartCount)
	}
	return f.Store.PutPart(ctx, u.key, u.uploadID, part, r, size)
}

// CompleteUpload implements repo.LFSStore.
func (f *FakeLFS) CompleteUpload(ctx context.Context, id uuid.UUID, parts []blob.Part) (repo.LFSObject, error) {
	u, err := f.upload(id)
	if err != nil {
		return repo.LFSObject{}, err
	}
	if u.uploadID == "" {
		return repo.LFSObject{}, fmt.Errorf("%w: not a multipart upload", repo.ErrInvalid)
	}
	if err := f.Store.CompleteMultipart(ctx, u.key, u.uploadID, parts); err != nil {
		return repo.LFSObject{}, err
	}
	defer func() { _ = f.Store.Delete(context.WithoutCancel(ctx), u.key) }()
	rc, _, err := f.Store.Get(ctx, u.key, nil)
	if err != nil {
		return repo.LFSObject{}, err
	}
	defer rc.Close()
	obj, err := f.Ingest(ctx, u.OID, u.Size, rc)
	f.mu.Lock()
	delete(f.uploads, id)
	f.mu.Unlock()
	return obj, err
}

// AbortUpload implements repo.LFSStore.
func (f *FakeLFS) AbortUpload(ctx context.Context, id uuid.UUID) error {
	u, err := f.upload(id)
	if err != nil {
		return err
	}
	if u.uploadID != "" {
		if err := f.Store.AbortMultipart(ctx, u.key, u.uploadID); err != nil {
			return err
		}
	}
	f.mu.Lock()
	delete(f.uploads, id)
	f.mu.Unlock()
	return nil
}

// PresignPut implements repo.LFSStore.
func (f *FakeLFS) PresignPut(ctx context.Context, oid string, size int64, ttl time.Duration) (blob.PresignedRequest, error) {
	sum, err := hex.DecodeString(oid)
	if err != nil || len(sum) != sha256.Size {
		return blob.PresignedRequest{}, fmt.Errorf("%w: invalid oid", repo.ErrInvalid)
	}
	return f.Store.PresignPut(ctx, model.LFSObjectKey(oid), size, sum, ttl)
}

// Verify implements repo.LFSStore: it re-hashes the stored object.
func (f *FakeLFS) Verify(ctx context.Context, oid string, size int64) (repo.LFSObject, error) {
	key := model.LFSObjectKey(oid)
	rc, info, err := f.Store.Get(ctx, key, nil)
	if err != nil {
		if errors.Is(err, blob.ErrNotFound) {
			return repo.LFSObject{}, fmt.Errorf("%w: lfs object %s", repo.ErrNotFound, oid)
		}
		return repo.LFSObject{}, err
	}
	defer rc.Close()
	h := sha256.New()
	if _, err := io.Copy(h, rc); err != nil {
		return repo.LFSObject{}, err
	}
	if info.Size != size || hex.EncodeToString(h.Sum(nil)) != oid {
		_ = f.Store.Delete(ctx, key)
		return repo.LFSObject{}, repo.ErrChecksumMismatch
	}
	return f.record(oid, size, key), nil
}

// Open implements repo.LFSStore.
func (f *FakeLFS) Open(ctx context.Context, oid string, rng *blob.Range) (io.ReadCloser, repo.LFSObject, error) {
	o, err := f.Stat(ctx, oid)
	if err != nil {
		return nil, repo.LFSObject{}, err
	}
	rc, _, err := f.Store.Get(ctx, o.StorageKey, rng)
	return rc, o, err
}

// PresignGet implements repo.LFSStore.
func (f *FakeLFS) PresignGet(ctx context.Context, oid string, ttl time.Duration, filename string) (string, error) {
	o, err := f.Stat(ctx, oid)
	if err != nil {
		return "", err
	}
	return f.Store.PresignGet(ctx, o.StorageKey, ttl, filename)
}
