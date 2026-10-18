package fs

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	iofs "io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/hangar-hub/hangar/internal/blob"
)

// Multipart uploads keep their parts under uploads/<upload-id>/ as
// part-NNNNN files, with the part's ETag in part-NNNNN.etag and the target
// key in the target file. Completing an upload concatenates the parts into
// a staged file and renames it into place.

func partName(n int) string { return fmt.Sprintf("part-%05d", n) }

func (s *Store) uploadDir(uploadID string) (string, error) {
	if _, err := uuid.Parse(uploadID); err != nil {
		return "", fmt.Errorf("%w: upload %s", blob.ErrNotFound, uploadID)
	}
	return filepath.Join(s.root, filepath.FromSlash(blob.PrefixUploads), uploadID), nil
}

// openUpload returns the upload's directory after checking that it exists
// and targets key.
func (s *Store) openUpload(key, uploadID string) (string, error) {
	if err := blob.ValidateKey(key); err != nil {
		return "", err
	}
	dir, err := s.uploadDir(uploadID)
	if err != nil {
		return "", err
	}
	target, err := os.ReadFile(filepath.Join(dir, uploadTargetFile)) //nolint:gosec // Built from a parsed UUID under the root.
	if err != nil || string(target) != key {
		return "", fmt.Errorf("%w: upload %s", blob.ErrNotFound, uploadID)
	}
	return dir, nil
}

// writeSmallFile writes data to path atomically through the staging directory.
func (s *Store) writeSmallFile(ctx context.Context, path string, data []byte) error {
	tmp, _, err := s.writeTemp(ctx, bytes.NewReader(data), int64(len(data)), nil)
	if err != nil {
		return err
	}
	return s.install(tmp, path)
}

// CreateMultipart implements blob.Store.
func (s *Store) CreateMultipart(ctx context.Context, key string) (string, error) {
	if _, err := s.path(key); err != nil {
		return "", err
	}
	id := uuid.NewString()
	dir, err := s.uploadDir(id)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", err
	}
	if err := s.writeSmallFile(ctx, filepath.Join(dir, uploadTargetFile), []byte(key)); err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	return id, nil
}

// PutPart implements blob.Store. The ETag is the hex of the first 16 bytes
// of the part's SHA-256, so uploading the same bytes again gives the same
// ETag.
func (s *Store) PutPart(ctx context.Context, key, uploadID string, part int, r io.Reader, size int64) (string, error) {
	if part < 1 || part > blob.MaxParts {
		return "", fmt.Errorf("part number %d is out of range 1-%d", part, blob.MaxParts)
	}
	if size > blob.MaxPartSize {
		return "", fmt.Errorf("part %d is larger than 5 GiB", part)
	}
	dir, err := s.openUpload(key, uploadID)
	if err != nil {
		return "", err
	}
	tmp, sum, err := s.writeTemp(ctx, r, size, nil)
	if err != nil {
		return "", err
	}
	etag := hex.EncodeToString(sum[:16])
	if err := s.install(tmp, filepath.Join(dir, partName(part))); err != nil {
		return "", err
	}
	if err := s.writeSmallFile(ctx, filepath.Join(dir, partName(part)+".etag"), []byte(etag)); err != nil {
		return "", err
	}
	return etag, nil
}

// partETag returns a part's ETag from its sidecar file, or recomputes it
// if a crash left the sidecar missing or stale.
func partETag(path string) (string, error) {
	f, err := os.Open(path) //nolint:gosec // A part file under the root.
	if err != nil {
		return "", err
	}
	defer f.Close()
	hr := blob.NewHashingReader(f)
	if _, err := io.Copy(io.Discard, hr); err != nil {
		return "", err
	}
	return hex.EncodeToString(hr.Sum()[:16]), nil
}

// ListParts implements blob.Store.
func (s *Store) ListParts(_ context.Context, key, uploadID string) ([]blob.Part, error) {
	dir, err := s.openUpload(key, uploadID)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var parts []blob.Part
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "part-") || strings.HasSuffix(name, ".etag") {
			continue
		}
		n, err := strconv.Atoi(strings.TrimPrefix(name, "part-"))
		if err != nil {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			return nil, err
		}
		etag, err := s.readPartETag(dir, name, fi)
		if err != nil {
			return nil, err
		}
		parts = append(parts, blob.Part{Number: n, ETag: etag, Size: fi.Size()})
	}
	sort.Slice(parts, func(i, j int) bool { return parts[i].Number < parts[j].Number })
	return parts, nil
}

// readPartETag trusts the sidecar only if it is at least as new as the part.
func (s *Store) readPartETag(dir, name string, part os.FileInfo) (string, error) {
	sidecar := filepath.Join(dir, name+".etag")
	if fi, err := os.Stat(sidecar); err == nil && !fi.ModTime().Before(part.ModTime()) {
		if b, err := os.ReadFile(sidecar); err == nil && len(b) > 0 { //nolint:gosec // A sidecar under the root.
			return string(b), nil
		}
	}
	return partETag(filepath.Join(dir, name))
}

// CompleteMultipart implements blob.Store. Like S3, it checks that the
// listed parts exist with matching ETags, in ascending order.
func (s *Store) CompleteMultipart(ctx context.Context, key, uploadID string, parts []blob.Part) error {
	dir, err := s.openUpload(key, uploadID)
	if err != nil {
		return err
	}
	dst, err := s.path(key)
	if err != nil {
		return err
	}
	if len(parts) == 0 {
		return errors.New("complete multipart upload: no parts")
	}
	files := make([]*os.File, 0, len(parts))
	defer func() {
		for _, f := range files {
			_ = f.Close()
		}
	}()
	readers := make([]io.Reader, 0, len(parts))
	prev := 0
	for _, p := range parts {
		if p.Number <= prev {
			return errors.New("complete multipart upload: parts must be in ascending order")
		}
		prev = p.Number
		name := partName(p.Number)
		f, err := os.Open(filepath.Join(dir, name)) //nolint:gosec // A part file under the root.
		if errors.Is(err, iofs.ErrNotExist) {
			return fmt.Errorf("%w: part %d", blob.ErrNotFound, p.Number)
		}
		if err != nil {
			return err
		}
		files = append(files, f)
		fi, err := f.Stat()
		if err != nil {
			return err
		}
		etag, err := s.readPartETag(dir, name, fi)
		if err != nil {
			return err
		}
		if strings.Trim(p.ETag, `"`) != etag {
			return fmt.Errorf("complete multipart upload: part %d ETag does not match", p.Number)
		}
		readers = append(readers, f)
	}
	tmp, _, err := s.writeTemp(ctx, io.MultiReader(readers...), -1, nil)
	if err != nil {
		return err
	}
	if err := s.install(tmp, dst); err != nil {
		return err
	}
	return os.RemoveAll(dir)
}

// AbortMultipart implements blob.Store.
func (s *Store) AbortMultipart(_ context.Context, key, uploadID string) error {
	dir, err := s.openUpload(key, uploadID)
	if err != nil {
		return err
	}
	return os.RemoveAll(dir)
}

var _ blob.UploadLister = (*Store)(nil)

// ListUploads implements blob.UploadLister. Initiated is when the upload
// was created.
func (s *Store) ListUploads(ctx context.Context, fn func(blob.IncompleteUpload) error) error {
	dir := filepath.Join(s.root, filepath.FromSlash(blob.PrefixUploads))
	entries, err := os.ReadDir(dir)
	if errors.Is(err, iofs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !e.IsDir() {
			continue
		}
		if _, err := uuid.Parse(e.Name()); err != nil {
			continue
		}
		targetPath := filepath.Join(dir, e.Name(), uploadTargetFile)
		target, err := os.ReadFile(targetPath) //nolint:gosec // Built from a parsed UUID under the root.
		if errors.Is(err, iofs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		fi, err := os.Stat(targetPath)
		if err != nil {
			continue
		}
		if err := fn(blob.IncompleteUpload{Key: string(target), UploadID: e.Name(), Initiated: fi.ModTime().UTC()}); err != nil {
			return err
		}
	}
	return nil
}
