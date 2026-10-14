package repo

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"

	"github.com/jamesagarside/hangar/internal/blob"
	"github.com/jamesagarside/hangar/internal/db"
	"github.com/jamesagarside/hangar/internal/model"
)

// LFS stores LFS objects once per Instance (UP-6), addressed by SHA-256,
// with lfs_objects rows for verified objects and uploads rows for uploads in
// progress. It implements LFSStore.
//
// An object is only recorded once its SHA-256 and size have been checked,
// and Ingest never writes unverified bytes to an object's key, so a corrupt
// upload is never visible.
type LFS struct {
	db    *db.DB
	blobs blob.Store
	now   func() time.Time
	// streamThreshold is the size above which Ingest streams straight into
	// a multipart upload at the final key instead of staging a copy.
	streamThreshold int64
}

var _ LFSStore = (*LFS)(nil)

// Upload kinds in the uploads table.
const uploadKindLFS = "lfs"

// NewLFS returns the LFS store.
func NewLFS(d *db.DB, blobs blob.Store) *LFS {
	return &LFS{db: d, blobs: blobs, now: func() time.Time { return time.Now().UTC() }, streamThreshold: 64 << 20}
}

func toLFSObject(row *db.LFSObject) LFSObject {
	return LFSObject{OID: row.OID, Size: row.Size, StorageKey: row.StorageKey, Verified: row.VerifiedAt != nil}
}

// Stat implements LFSStore. Only verified objects are recorded, so a
// missing or unverified object gives ErrNotFound.
func (l *LFS) Stat(ctx context.Context, oid string) (LFSObject, error) {
	if !model.IsSHA256(oid) {
		return LFSObject{}, fmt.Errorf("%w: invalid oid %q", ErrInvalid, oid)
	}
	row := new(db.LFSObject)
	err := l.db.NewSelect().Model(row).Where("oid = ?", oid).Where("verified_at IS NOT NULL").Scan(ctx)
	if db.IsNotFound(err) {
		return LFSObject{}, fmt.Errorf("%w: lfs object %s", ErrNotFound, oid)
	}
	if err != nil {
		return LFSObject{}, fmt.Errorf("repo: stat lfs object: %w", err)
	}
	return toLFSObject(row), nil
}

// StatMany implements LFSStore.
func (l *LFS) StatMany(ctx context.Context, oids []string) (map[string]LFSObject, error) {
	out := map[string]LFSObject{}
	for start := 0; start < len(oids); start += 500 {
		chunk := oids[start:min(start+500, len(oids))]
		var rows []db.LFSObject
		if err := l.db.NewSelect().Model(&rows).Where("oid IN (?)", bun.List(chunk)).Where("verified_at IS NOT NULL").Scan(ctx); err != nil {
			return nil, fmt.Errorf("repo: stat lfs objects: %w", err)
		}
		for i := range rows {
			out[rows[i].OID] = toLFSObject(&rows[i])
		}
	}
	return out, nil
}

// record marks an object stored and verified.
func (l *LFS) record(ctx context.Context, oid string, size int64) (LFSObject, error) {
	now := l.now()
	row := &db.LFSObject{OID: oid, Size: size, StorageKey: model.LFSObjectKey(oid), VerifiedAt: &now, LastReferencedAt: now}
	_, err := l.db.NewInsert().Model(row).
		On("CONFLICT (oid) DO UPDATE").Set("size = EXCLUDED.size").Set("storage_key = EXCLUDED.storage_key").
		Set("verified_at = EXCLUDED.verified_at").Set("last_referenced_at = EXCLUDED.last_referenced_at").Set("updated_at = EXCLUDED.updated_at").
		Exec(ctx)
	if err != nil {
		return LFSObject{}, fmt.Errorf("repo: record lfs object: %w", err)
	}
	return LFSObject{OID: oid, Size: size, StorageKey: row.StorageKey, Verified: true}, nil
}

// hashingReader hashes and counts what it reads.
type hashingReader struct {
	r io.Reader
	h hash.Hash
	n int64
}

func (h *hashingReader) Read(p []byte) (int, error) {
	n, err := h.r.Read(p)
	if n > 0 {
		h.h.Write(p[:n])
		h.n += int64(n)
	}
	return n, err
}

// Ingest implements LFSStore. An object that is already stored is not
// stored again (UP-6): r is not read and the stored object is returned.
// Small objects are written to a temporary key, checked and copied into
// place; large ones stream into a multipart upload at the final key that
// completes only if the SHA-256 and size match, so nothing is buffered and
// a corrupt object never appears.
func (l *LFS) Ingest(ctx context.Context, oid string, size int64, r io.Reader) (LFSObject, error) {
	if !model.IsSHA256(oid) || size < 0 {
		return LFSObject{}, fmt.Errorf("%w: invalid oid or size", ErrInvalid)
	}
	if obj, err := l.Stat(ctx, oid); err == nil {
		if obj.Size != size {
			return LFSObject{}, fmt.Errorf("%w: %s is stored with size %d, not %d", ErrChecksumMismatch, oid, obj.Size, size)
		}
		return obj, nil
	}
	hr := &hashingReader{r: r, h: sha256.New()}
	key := model.LFSObjectKey(oid)
	if size > l.streamThreshold {
		if err := l.streamMultipart(ctx, key, oid, size, hr); err != nil {
			return LFSObject{}, err
		}
		return l.record(ctx, oid, size)
	}
	tmp := blob.PrefixTmp + uuid.NewString()
	defer func() { _ = l.blobs.Delete(context.WithoutCancel(ctx), tmp) }()
	if err := l.blobs.Put(ctx, tmp, hr, size); err != nil {
		if errors.Is(err, blob.ErrSizeMismatch) {
			return LFSObject{}, fmt.Errorf("%w: %w", ErrChecksumMismatch, err)
		}
		return LFSObject{}, fmt.Errorf("repo: store lfs object: %w", err)
	}
	if hr.n != size || hex.EncodeToString(hr.h.Sum(nil)) != oid {
		return LFSObject{}, fmt.Errorf("%w: sha256 does not match %s", ErrChecksumMismatch, oid)
	}
	if err := l.blobs.Copy(ctx, tmp, key); err != nil {
		return LFSObject{}, fmt.Errorf("repo: store lfs object: %w", err)
	}
	return l.record(ctx, oid, size)
}

// PartSize returns the multipart part size for an object: at least 16 MiB,
// and large enough to fit in 10,000 parts, rounded up to a MiB.
func PartSize(size int64) int64 {
	const mib = 1 << 20
	ps := max(int64(16*mib), (size+9999)/10000)
	return (ps + mib - 1) / mib * mib
}

func (l *LFS) streamMultipart(ctx context.Context, key, oid string, size int64, hr *hashingReader) error {
	uploadID, err := l.blobs.CreateMultipart(ctx, key)
	if err != nil {
		return fmt.Errorf("repo: start lfs upload: %w", err)
	}
	abort := func(cause error) error {
		_ = l.blobs.AbortMultipart(context.WithoutCancel(ctx), key, uploadID)
		return cause
	}
	ps := PartSize(size)
	var parts []blob.Part
	for n, done := 1, int64(0); done < size; n++ {
		want := min(ps, size-done)
		etag, err := l.blobs.PutPart(ctx, key, uploadID, n, io.LimitReader(hr, want), want)
		if err != nil {
			if errors.Is(err, blob.ErrSizeMismatch) {
				return abort(fmt.Errorf("%w: %w", ErrChecksumMismatch, err))
			}
			return abort(fmt.Errorf("repo: upload lfs part: %w", err))
		}
		parts = append(parts, blob.Part{Number: n, ETag: etag, Size: want})
		done += want
	}
	// The body must end exactly at size.
	if extra, _ := io.Copy(io.Discard, io.LimitReader(hr, 1)); extra > 0 || hr.n != size {
		return abort(fmt.Errorf("%w: body is longer than %d bytes", ErrChecksumMismatch, size))
	}
	if hex.EncodeToString(hr.h.Sum(nil)) != oid {
		return abort(fmt.Errorf("%w: sha256 does not match %s", ErrChecksumMismatch, oid))
	}
	if err := l.blobs.CompleteMultipart(ctx, key, uploadID, parts); err != nil {
		return abort(fmt.Errorf("repo: complete lfs upload: %w", err))
	}
	return nil
}

func toUpload(row *db.Upload) *Upload {
	u := &Upload{ID: row.ID, OID: row.OID, Size: row.Size, PartSize: row.PartSize, ExpiresAt: row.ExpiresAt.UTC()}
	if row.PartSize > 0 {
		u.PartCount = int((row.Size + row.PartSize - 1) / row.PartSize)
	}
	return u
}

// CreateUpload implements LFSStore. Multipart parts go to a staging key, so
// the object only appears at its OID after CompleteUpload has checked it.
func (l *LFS) CreateUpload(ctx context.Context, p UploadParams) (*Upload, error) {
	if !model.IsSHA256(p.OID) || p.Size < 0 {
		return nil, fmt.Errorf("%w: invalid oid or size", ErrInvalid)
	}
	ttl := p.TTL
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	id := db.NewID()
	row := &db.Upload{ID: id, OID: p.OID, Size: p.Size, Kind: uploadKindLFS, StorageKey: model.LFSObjectKey(p.OID),
		Parts: []db.UploadPart{}, ExpiresAt: l.now().Add(ttl)}
	if p.UserID != uuid.Nil {
		u := p.UserID
		row.UserID = &u
	}
	if p.RepoUUID != uuid.Nil {
		r := p.RepoUUID
		row.RepoID = &r
	}
	if p.Multipart {
		if p.PartSize <= 0 {
			return nil, fmt.Errorf("%w: multipart uploads need a part size", ErrInvalid)
		}
		if (p.Size+p.PartSize-1)/p.PartSize > 10000 {
			return nil, fmt.Errorf("%w: part size %d needs more than 10,000 parts", ErrInvalid, p.PartSize)
		}
		row.PartSize = p.PartSize
		row.StorageKey = blob.PrefixTmp + "uploads/" + id.String()
		uploadID, err := l.blobs.CreateMultipart(ctx, row.StorageKey)
		if err != nil {
			return nil, fmt.Errorf("repo: start upload: %w", err)
		}
		row.UploadID = uploadID
	}
	if _, err := l.db.NewInsert().Model(row).Exec(ctx); err != nil {
		if row.UploadID != "" {
			_ = l.blobs.AbortMultipart(context.WithoutCancel(ctx), row.StorageKey, row.UploadID)
		}
		return nil, fmt.Errorf("repo: record upload: %w", err)
	}
	return toUpload(row), nil
}

func (l *LFS) upload(ctx context.Context, id uuid.UUID) (*db.Upload, error) {
	row := new(db.Upload)
	err := l.db.NewSelect().Model(row).Where("id = ?", id).Scan(ctx)
	if db.IsNotFound(err) {
		return nil, fmt.Errorf("%w: upload %s", ErrNotFound, id)
	}
	if err != nil {
		return nil, fmt.Errorf("repo: read upload: %w", err)
	}
	if l.now().After(row.ExpiresAt) {
		return nil, fmt.Errorf("%w: upload %s has expired", ErrNotFound, id)
	}
	return row, nil
}

// UploadRepo returns the Repo an upload belongs to, so handlers can check
// that a part or completion request names the same Repo.
func (l *LFS) UploadRepo(ctx context.Context, id uuid.UUID) (uuid.UUID, error) {
	row, err := l.upload(ctx, id)
	if err != nil {
		return uuid.Nil, err
	}
	if row.RepoID == nil {
		return uuid.Nil, nil
	}
	return *row.RepoID, nil
}

func multipartOnly(row *db.Upload) error {
	if row.UploadID == "" {
		return fmt.Errorf("%w: not a multipart upload", ErrInvalid)
	}
	return nil
}

// GetUpload implements LFSStore.
func (l *LFS) GetUpload(ctx context.Context, id uuid.UUID) (*Upload, error) {
	row, err := l.upload(ctx, id)
	if err != nil {
		return nil, err
	}
	u := toUpload(row)
	if row.UploadID != "" {
		parts, err := l.blobs.ListParts(ctx, row.StorageKey, row.UploadID)
		if err != nil {
			return nil, fmt.Errorf("repo: list upload parts: %w", err)
		}
		u.Done = parts
	}
	return u, nil
}

// PresignPart implements LFSStore.
func (l *LFS) PresignPart(ctx context.Context, id uuid.UUID, part int, ttl time.Duration) (string, error) {
	row, err := l.upload(ctx, id)
	if err != nil {
		return "", err
	}
	if err := multipartOnly(row); err != nil {
		return "", err
	}
	if err := checkPart(row, part); err != nil {
		return "", err
	}
	return l.blobs.PresignPart(ctx, row.StorageKey, row.UploadID, part, ttl)
}

func checkPart(row *db.Upload, part int) error {
	count := int((row.Size + row.PartSize - 1) / row.PartSize)
	if part < 1 || part > max(count, 1) {
		return fmt.Errorf("%w: part %d is outside 1-%d", ErrInvalid, part, count)
	}
	return nil
}

// PutPart implements LFSStore. Every part but the last must be exactly the
// part size.
func (l *LFS) PutPart(ctx context.Context, id uuid.UUID, part int, r io.Reader, size int64) (string, error) {
	row, err := l.upload(ctx, id)
	if err != nil {
		return "", err
	}
	if err := multipartOnly(row); err != nil {
		return "", err
	}
	if err := checkPart(row, part); err != nil {
		return "", err
	}
	want := row.PartSize
	if last := int64(part) * row.PartSize; last > row.Size {
		want = row.Size - int64(part-1)*row.PartSize
	}
	if size >= 0 && size != want {
		return "", fmt.Errorf("%w: part %d must be %d bytes, not %d", ErrInvalid, part, want, size)
	}
	etag, err := l.blobs.PutPart(ctx, row.StorageKey, row.UploadID, part, r, want)
	if err != nil {
		if errors.Is(err, blob.ErrSizeMismatch) {
			return "", fmt.Errorf("%w: %w", ErrChecksumMismatch, err)
		}
		return "", fmt.Errorf("repo: upload part: %w", err)
	}
	return etag, nil
}

// CompleteUpload implements LFSStore: it assembles the parts at the staging
// key, then ingests the result, which checks the SHA-256 and size.
func (l *LFS) CompleteUpload(ctx context.Context, id uuid.UUID, parts []blob.Part) (LFSObject, error) {
	row, err := l.upload(ctx, id)
	if err != nil {
		return LFSObject{}, err
	}
	if err := multipartOnly(row); err != nil {
		return LFSObject{}, err
	}
	if obj, err := l.Stat(ctx, row.OID); err == nil {
		// Another upload stored the object meanwhile.
		_ = l.AbortUpload(ctx, id)
		return obj, nil
	}
	if err := l.blobs.CompleteMultipart(ctx, row.StorageKey, row.UploadID, parts); err != nil {
		return LFSObject{}, fmt.Errorf("repo: complete upload: %w", err)
	}
	defer func() { _ = l.blobs.Delete(context.WithoutCancel(ctx), row.StorageKey) }()
	rc, _, err := l.blobs.Get(ctx, row.StorageKey, nil)
	if err != nil {
		return LFSObject{}, fmt.Errorf("repo: read upload: %w", err)
	}
	defer rc.Close()
	obj, err := l.Ingest(ctx, row.OID, row.Size, rc)
	if _, derr := l.db.NewDelete().Model((*db.Upload)(nil)).Where("id = ?", id).Exec(context.WithoutCancel(ctx)); derr != nil && err == nil {
		err = derr
	}
	return obj, err
}

// AbortUpload implements LFSStore.
func (l *LFS) AbortUpload(ctx context.Context, id uuid.UUID) error {
	row := new(db.Upload)
	err := l.db.NewSelect().Model(row).Where("id = ?", id).Scan(ctx)
	if db.IsNotFound(err) {
		return fmt.Errorf("%w: upload %s", ErrNotFound, id)
	}
	if err != nil {
		return fmt.Errorf("repo: read upload: %w", err)
	}
	return l.abort(ctx, row)
}

func (l *LFS) abort(ctx context.Context, row *db.Upload) error {
	if row.UploadID != "" {
		if err := l.blobs.AbortMultipart(ctx, row.StorageKey, row.UploadID); err != nil && !errors.Is(err, blob.ErrNotFound) {
			return fmt.Errorf("repo: abort upload: %w", err)
		}
	}
	_, err := l.db.NewDelete().Model((*db.Upload)(nil)).Where("id = ?", row.ID).Exec(ctx)
	return err
}

// ExpireUploads aborts uploads past their expiry (the upload.expire Job)
// and returns how many it removed.
func (l *LFS) ExpireUploads(ctx context.Context) (int, error) {
	var rows []db.Upload
	if err := l.db.NewSelect().Model(&rows).Where("expires_at < ?", l.now()).Limit(1000).Scan(ctx); err != nil {
		return 0, err
	}
	for i := range rows {
		if err := l.abort(ctx, &rows[i]); err != nil {
			return i, err
		}
	}
	return len(rows), nil
}

// PresignPut implements LFSStore. The URL writes straight to the object's
// key; Verify checks the content before the object is recorded.
func (l *LFS) PresignPut(ctx context.Context, oid string, size int64, ttl time.Duration) (blob.PresignedRequest, error) {
	sum, err := hex.DecodeString(oid)
	if err != nil || len(sum) != sha256.Size || !model.IsSHA256(oid) {
		return blob.PresignedRequest{}, fmt.Errorf("%w: invalid oid", ErrInvalid)
	}
	return l.blobs.PresignPut(ctx, model.LFSObjectKey(oid), size, sum, ttl)
}

// Verify implements LFSStore: it re-hashes the stored object, records it if
// it matches, and deletes it if it does not.
func (l *LFS) Verify(ctx context.Context, oid string, size int64) (LFSObject, error) {
	if obj, err := l.Stat(ctx, oid); err == nil {
		if obj.Size != size {
			return LFSObject{}, fmt.Errorf("%w: %s has size %d, not %d", ErrChecksumMismatch, oid, obj.Size, size)
		}
		return obj, nil
	}
	key := model.LFSObjectKey(oid)
	rc, info, err := l.blobs.Get(ctx, key, nil)
	if err != nil {
		if errors.Is(err, blob.ErrNotFound) {
			return LFSObject{}, fmt.Errorf("%w: lfs object %s", ErrNotFound, oid)
		}
		return LFSObject{}, fmt.Errorf("repo: read lfs object: %w", err)
	}
	defer rc.Close()
	h := sha256.New()
	if _, err := io.Copy(h, rc); err != nil {
		return LFSObject{}, fmt.Errorf("repo: hash lfs object: %w", err)
	}
	if info.Size != size || hex.EncodeToString(h.Sum(nil)) != oid {
		_ = l.blobs.Delete(ctx, key)
		return LFSObject{}, fmt.Errorf("%w: %s", ErrChecksumMismatch, oid)
	}
	return l.record(ctx, oid, size)
}

// Open implements LFSStore.
func (l *LFS) Open(ctx context.Context, oid string, rng *blob.Range) (io.ReadCloser, LFSObject, error) {
	obj, err := l.Stat(ctx, oid)
	if err != nil {
		return nil, LFSObject{}, err
	}
	rc, _, err := l.blobs.Get(ctx, obj.StorageKey, rng)
	if err != nil {
		return nil, LFSObject{}, err
	}
	return rc, obj, nil
}

// PresignGet implements LFSStore.
func (l *LFS) PresignGet(ctx context.Context, oid string, ttl time.Duration, filename string) (string, error) {
	obj, err := l.Stat(ctx, oid)
	if err != nil {
		return "", err
	}
	return l.blobs.PresignGet(ctx, obj.StorageKey, ttl, filename)
}

// Touch records that oids are referenced now, so garbage collection's grace
// period starts from their last reference.
func Touch(ctx context.Context, idb bun.IDB, oids []string, at time.Time) error {
	for start := 0; start < len(oids); start += 500 {
		chunk := oids[start:min(start+500, len(oids))]
		if _, err := idb.NewUpdate().Model((*db.LFSObject)(nil)).Set("last_referenced_at = ?", at).
			Where("oid IN (?)", bun.List(chunk)).Exec(ctx); err != nil {
			return err
		}
	}
	return nil
}
