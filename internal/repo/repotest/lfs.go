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

	"github.com/hangar-hub/hangar/internal/blob"
	"github.com/hangar-hub/hangar/internal/model"
	"github.com/hangar-hub/hangar/internal/repo"
)

// FakeLFS is an in-memory repo.LFSStore over a blob.Store (usually a
// blobtest.Fake). It verifies SHA-256 and size exactly as the real store
// must, so tests of upload handlers catch corrupt uploads, and records
// uploaders as the real store does.
type FakeLFS struct {
	Store blob.Store

	mu        sync.Mutex
	objects   map[string]repo.LFSObject
	uploads   map[uuid.UUID]*fakeUpload
	uploaders map[string]map[uuid.UUID]bool
	// staged are presigned PUTs, newest last.
	staged []stagedPut
	now    func() time.Time
}

// stagedPut is where a presigned PUT writes until Verify checks it.
type stagedPut struct {
	oid    string
	size   int64
	key    string
	userID uuid.UUID
}

type fakeUpload struct {
	repo.Upload
	key      string
	uploadID string
	repoUUID uuid.UUID
	userID   uuid.UUID
}

var _ repo.LFSStore = (*FakeLFS)(nil)

// NewFakeLFS returns an empty FakeLFS writing to store.
func NewFakeLFS(store blob.Store) *FakeLFS {
	return &FakeLFS{Store: store, objects: map[string]repo.LFSObject{}, uploads: map[uuid.UUID]*fakeUpload{},
		uploaders: map[string]map[uuid.UUID]bool{}, now: time.Now}
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
// key and moves the object into place only if it matches. An object that
// is already stored is hashed and checked but not written again.
func (f *FakeLFS) Ingest(ctx context.Context, oid string, size int64, r io.Reader) (repo.LFSObject, error) {
	if !model.IsSHA256(oid) {
		return repo.LFSObject{}, fmt.Errorf("%w: invalid oid %q", repo.ErrInvalid, oid)
	}
	h := sha256.New()
	if o, err := f.Stat(ctx, oid); err == nil {
		n, err := io.Copy(h, io.LimitReader(r, size+1))
		if err != nil {
			return repo.LFSObject{}, err
		}
		if n != size || hex.EncodeToString(h.Sum(nil)) != oid {
			return repo.LFSObject{}, fmt.Errorf("%w: the content does not match SHA-256 %s and %d bytes", repo.ErrChecksumMismatch, oid, size)
		}
		return o, nil
	}
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

// RecordUploader implements repo.LFSStore.
func (f *FakeLFS) RecordUploader(_ context.Context, oid string, userID uuid.UUID) error {
	if userID == uuid.Nil {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.objects[oid]; !ok {
		return fmt.Errorf("%w: lfs object %s", repo.ErrNotFound, oid)
	}
	if f.uploaders[oid] == nil {
		f.uploaders[oid] = map[uuid.UUID]bool{}
	}
	f.uploaders[oid][userID] = true
	return nil
}

// Uploaded implements repo.LFSStore.
func (f *FakeLFS) Uploaded(_ context.Context, userID uuid.UUID, oids []string) (map[string]bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]bool{}
	for _, o := range oids {
		if userID != uuid.Nil && f.uploaders[o][userID] {
			out[o] = true
		}
	}
	return out, nil
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
	if p.Resume && p.Multipart && (p.UserID != uuid.Nil) == (p.RepoUUID != uuid.Nil) {
		f.mu.Lock()
		var found *fakeUpload
		for _, u := range f.uploads {
			if u.OID == p.OID && u.Size == p.Size && u.PartSize == p.PartSize && u.uploadID != "" && u.userID == p.UserID &&
				u.repoUUID == p.RepoUUID && f.now().Before(u.ExpiresAt) && (found == nil || u.ExpiresAt.After(found.ExpiresAt)) {
				found = u
			}
		}
		if found != nil {
			found.ExpiresAt = f.now().Add(ttl)
			out := found.Upload
			f.mu.Unlock()
			return &out, nil
		}
		f.mu.Unlock()
	}
	u := &fakeUpload{Upload: repo.Upload{ID: uuid.New(), OID: p.OID, Size: p.Size, ExpiresAt: f.now().Add(ttl)}, key: model.LFSObjectKey(p.OID),
		repoUUID: p.RepoUUID, userID: p.UserID}
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
		if errors.Is(err, blob.ErrNotFound) {
			// The Blob store expired the multipart upload; like repo.LFS,
			// the upload is gone.
			f.mu.Lock()
			delete(f.uploads, id)
			f.mu.Unlock()
			return nil, fmt.Errorf("%w: upload %s lost its parts; upload the file again", repo.ErrNotFound, id)
		}
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
	if err == nil {
		err = f.RecordUploader(ctx, u.OID, u.userID)
	}
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

// PresignPut implements repo.LFSStore. Like repo.LFS, the URL writes to a
// staging key of its own that Verify checks.
func (f *FakeLFS) PresignPut(ctx context.Context, oid string, size int64, ttl time.Duration, userID uuid.UUID) (blob.PresignedRequest, error) {
	if !model.IsSHA256(oid) {
		return blob.PresignedRequest{}, fmt.Errorf("%w: invalid oid", repo.ErrInvalid)
	}
	key := blob.PrefixTmp + "uploads/" + uuid.NewString()
	// Like repo.LFS, sign no headers: huggingface_hub sends none on a
	// basic upload. Verify checks the content.
	req, err := f.Store.PresignPut(ctx, key, size, nil, ttl)
	if err != nil {
		return blob.PresignedRequest{}, err
	}
	f.mu.Lock()
	f.staged = append(f.staged, stagedPut{oid: oid, size: size, key: key, userID: userID})
	f.mu.Unlock()
	return req, nil
}

// Stage writes content where a presigned PUT by userID for oid writes,
// as a client would, for tests whose Blob store cannot presign. Verify
// then checks it.
func (f *FakeLFS) Stage(ctx context.Context, oid string, content []byte, userID uuid.UUID) error {
	key := blob.PrefixTmp + "uploads/" + uuid.NewString()
	if err := f.Store.Put(ctx, key, bytes.NewReader(content), int64(len(content))); err != nil {
		return err
	}
	f.mu.Lock()
	f.staged = append(f.staged, stagedPut{oid: oid, size: int64(len(content)), key: key, userID: userID})
	f.mu.Unlock()
	return nil
}

// Verify implements repo.LFSStore: it checks what userID staged for the
// object (anyone, for uuid.Nil), newest first, moves the first match into
// place, or only proves it when the object is stored, and records the
// uploader. With nothing staged it gives repo.ErrNotFound.
func (f *FakeLFS) Verify(ctx context.Context, oid string, size int64, userID uuid.UUID) (repo.LFSObject, error) {
	f.mu.Lock()
	var tries []stagedPut
	for i := len(f.staged) - 1; i >= 0; i-- {
		sp := f.staged[i]
		if sp.oid == oid && sp.size == size && (userID == uuid.Nil || sp.userID == userID) {
			tries = append(tries, sp)
		}
	}
	f.mu.Unlock()
	mismatch := false
	for _, sp := range tries {
		obj, err := f.proveStaged(ctx, sp)
		switch {
		case err == nil:
			return obj, f.RecordUploader(ctx, oid, sp.userID)
		case errors.Is(err, blob.ErrNotFound):
		case errors.Is(err, repo.ErrChecksumMismatch):
			mismatch = true
		default:
			return repo.LFSObject{}, err
		}
	}
	if mismatch {
		return repo.LFSObject{}, fmt.Errorf("%w: %s", repo.ErrChecksumMismatch, oid)
	}
	return repo.LFSObject{}, fmt.Errorf("%w: no upload of lfs object %s to verify", repo.ErrNotFound, oid)
}

// proveStaged hashes a staged PUT, then stores it when the object is not
// stored yet, and removes it.
func (f *FakeLFS) proveStaged(ctx context.Context, sp stagedPut) (repo.LFSObject, error) {
	rc, info, err := f.Store.Get(ctx, sp.key, nil)
	if err != nil {
		return repo.LFSObject{}, err
	}
	h := sha256.New()
	_, err = io.Copy(h, rc)
	_ = rc.Close()
	if err != nil {
		return repo.LFSObject{}, err
	}
	defer func() {
		_ = f.Store.Delete(context.WithoutCancel(ctx), sp.key)
		f.mu.Lock()
		for i := range f.staged {
			if f.staged[i].key == sp.key {
				f.staged = append(f.staged[:i], f.staged[i+1:]...)
				break
			}
		}
		f.mu.Unlock()
	}()
	if info.Size != sp.size || hex.EncodeToString(h.Sum(nil)) != sp.oid {
		return repo.LFSObject{}, repo.ErrChecksumMismatch
	}
	if o, err := f.Stat(ctx, sp.oid); err == nil {
		return o, nil
	}
	key := model.LFSObjectKey(sp.oid)
	if err := f.Store.Copy(ctx, sp.key, key); err != nil {
		return repo.LFSObject{}, err
	}
	return f.record(sp.oid, sp.size, key), nil
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
