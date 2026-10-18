// Package fs is the filesystem Blob store backend. Objects live at
// <root>/<key>, so an admin can recover any file with standard tools.
// Writes go to a temporary file under <root>/.staging and are renamed into
// place, so readers never see partial objects. Multipart uploads keep their
// parts under uploads/<upload-id>/ (docs/spec/02-architecture.md "Blob
// store layout").
//
// The backend relies only on atomic rename within one filesystem, never on
// file locks, so it works on NFS and SMB mounts shared by several Hangar
// processes; the database coordinates everything else.
package fs

import (
	"bytes"
	"context"
	"crypto/sha256"
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
	"syscall"
	"time"

	"github.com/google/uuid"

	"github.com/hangar-hub/hangar/internal/blob"
)

// stagingDir is the directory under the root that holds temporary files.
// It is not part of the key space.
const stagingDir = ".staging"

// uploadTargetFile records the key a multipart upload writes to.
const uploadTargetFile = "target"

// Store is the filesystem Blob store.
type Store struct {
	root    string
	staging string
}

var (
	_ blob.Store          = (*Store)(nil)
	_ blob.VerifiedPutter = (*Store)(nil)
	_ blob.StagingSweeper = (*Store)(nil)
)

// New opens a filesystem Blob store rooted at root, creating the directory
// if needed and checking that it is writable.
func New(root string) (*Store, error) {
	if root == "" {
		return nil, errors.New("filesystem blob store: the path is empty")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("filesystem blob store: %w", err)
	}
	s := &Store{root: abs, staging: filepath.Join(abs, stagingDir)}
	if err := os.MkdirAll(s.staging, 0o750); err != nil {
		return nil, fmt.Errorf("filesystem blob store: create %s: %w", s.staging, err)
	}
	f, err := os.CreateTemp(s.staging, "probe-*")
	if err != nil {
		return nil, fmt.Errorf("filesystem blob store: %s is not writable: %w", abs, err)
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
	return s, nil
}

// Root returns the store's root directory.
func (s *Store) Root() string { return s.root }

func (s *Store) path(key string) (string, error) {
	if err := blob.ValidateKey(key); err != nil {
		return "", err
	}
	if key == stagingDir || strings.HasPrefix(key, stagingDir+"/") {
		return "", fmt.Errorf("%w: %q is reserved", blob.ErrInvalidKey, key)
	}
	return filepath.Join(s.root, filepath.FromSlash(key)), nil
}

func etagOf(info os.FileInfo) string {
	return strconv.FormatInt(info.ModTime().UnixNano(), 16) + "-" + strconv.FormatInt(info.Size(), 16)
}

func notFound(key string) error { return fmt.Errorf("%w: %s", blob.ErrNotFound, key) }

// Stat implements blob.Store.
func (s *Store) Stat(ctx context.Context, key string) (blob.Info, error) {
	if err := ctx.Err(); err != nil {
		return blob.Info{}, err
	}
	p, err := s.path(key)
	if err != nil {
		return blob.Info{}, err
	}
	fi, err := os.Stat(p)
	if errors.Is(err, iofs.ErrNotExist) || (err == nil && fi.IsDir()) {
		return blob.Info{}, notFound(key)
	}
	if err != nil {
		return blob.Info{}, err
	}
	return blob.Info{Key: key, Size: fi.Size(), ModTime: fi.ModTime().UTC(), ETag: etagOf(fi)}, nil
}

// Get implements blob.Store.
func (s *Store) Get(ctx context.Context, key string, rng *blob.Range) (io.ReadCloser, blob.Info, error) {
	if err := ctx.Err(); err != nil {
		return nil, blob.Info{}, err
	}
	p, err := s.path(key)
	if err != nil {
		return nil, blob.Info{}, err
	}
	f, err := os.Open(p) //nolint:gosec // The path is a validated key under the root.
	if errors.Is(err, iofs.ErrNotExist) {
		return nil, blob.Info{}, notFound(key)
	}
	if err != nil {
		return nil, blob.Info{}, err
	}
	fi, err := f.Stat()
	if err != nil || fi.IsDir() {
		_ = f.Close()
		if err == nil {
			err = notFound(key)
		}
		return nil, blob.Info{}, err
	}
	info := blob.Info{Key: key, Size: fi.Size(), ModTime: fi.ModTime().UTC(), ETag: etagOf(fi)}
	if rng == nil {
		return f, info, nil
	}
	start, end, err := rng.Resolve(fi.Size())
	if err != nil {
		_ = f.Close()
		return nil, info, err
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		_ = f.Close()
		return nil, info, err
	}
	return &rangeReader{Reader: io.LimitReader(f, end-start+1), f: f}, info, nil
}

type rangeReader struct {
	io.Reader
	f *os.File
}

func (r *rangeReader) Close() error { return r.f.Close() }

// writeTemp streams r into a new file under the staging directory and
// returns its path. With a known size it fails with ErrSizeMismatch on any
// other length. If sum is not nil, it fails with ErrChecksumMismatch unless
// the content's SHA-256 is sum. The file is synced and closed.
func (s *Store) writeTemp(ctx context.Context, r io.Reader, size int64, sum []byte) (string, []byte, error) {
	f, err := os.CreateTemp(s.staging, "put-*")
	if err != nil {
		return "", nil, err
	}
	name := f.Name()
	fail := func(err error) (string, []byte, error) {
		_ = f.Close()
		_ = os.Remove(name)
		return "", nil, err
	}
	hr := blob.NewHashingReader(blob.ContextReader(ctx, r))
	var src io.Reader = hr
	if size >= 0 {
		src = io.LimitReader(hr, size+1)
	}
	n, err := io.Copy(f, src)
	if err != nil {
		return fail(err)
	}
	if size >= 0 && n != size {
		return fail(fmt.Errorf("%w: read %d bytes, want %d", blob.ErrSizeMismatch, n, size))
	}
	got := hr.Sum()
	if sum != nil && !bytes.Equal(got, sum) {
		return fail(fmt.Errorf("%w: got %s, want %s", blob.ErrChecksumMismatch, hex.EncodeToString(got), hex.EncodeToString(sum)))
	}
	if err := f.Sync(); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(name)
		return "", nil, err
	}
	return name, got, nil
}

// SweepStaging implements blob.StagingSweeper: it removes files under
// <root>/.staging last modified before cutoff, which writes that a crash or
// a killed process interrupted leave behind. A write in progress keeps its
// file's modification time current, so a cutoff of hours never removes one,
// even when several processes share the directory over NFS.
func (s *Store) SweepStaging(ctx context.Context, cutoff time.Time) (int, error) {
	entries, err := os.ReadDir(s.staging)
	if errors.Is(err, iofs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	removed := 0
	var errs []error
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return removed, err
		}
		if !e.Type().IsRegular() {
			continue
		}
		fi, err := e.Info()
		if errors.Is(err, iofs.ErrNotExist) {
			continue
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if !fi.ModTime().Before(cutoff) {
			continue
		}
		if err := os.Remove(filepath.Join(s.staging, e.Name())); err != nil {
			if !errors.Is(err, iofs.ErrNotExist) {
				errs = append(errs, err)
			}
			continue
		}
		removed++
	}
	return removed, errors.Join(errs...)
}

// installAttempts bounds how often install retries after a concurrent
// Delete pruned the destination's directory.
const installAttempts = 10

// install renames a staged file into place at dst, creating parent
// directories. A concurrent Delete may remove an empty parent between
// MkdirAll and Rename, or while MkdirAll creates it, so it retries while the
// error is one that a vanished directory causes. The error it returns names
// dst relative to the root, never an absolute path.
func (s *Store) install(tmp, dst string) error {
	var err error
	for range installAttempts {
		if err = os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
			if lostDirectory(err) || errors.Is(err, iofs.ErrExist) {
				continue
			}
			break
		}
		if err = os.Rename(tmp, dst); err == nil {
			return nil
		}
		if !lostDirectory(err) {
			break
		}
	}
	_ = os.Remove(tmp)
	return installError(s.relative(dst), err)
}

// lostDirectory reports whether err is what MkdirAll or Rename return when
// a directory on the path was removed during the call: ENOENT, or EINVAL,
// which macOS returns for a directory being removed. MkdirAll can also
// return EEXIST when the directory vanished between its mkdir and the check
// that followed; install retries that for MkdirAll only, because EEXIST from
// Rename means the destination is a directory, which retrying cannot fix.
func lostDirectory(err error) bool {
	return errors.Is(err, iofs.ErrNotExist) || errors.Is(err, syscall.EINVAL)
}

// relative returns path relative to the root with forward slashes: the key,
// for an object.
func (s *Store) relative(path string) string {
	rel, err := filepath.Rel(s.root, path)
	if err != nil {
		return filepath.Base(path)
	}
	return filepath.ToSlash(rel)
}

// installError reports a failed install at key without the paths that
// os.MkdirAll and os.Rename put in their errors.
func installError(key string, err error) error {
	errno, ok := errors.AsType[syscall.Errno](err)
	if !ok {
		return fmt.Errorf("blob fs: store %s: %w", key, err)
	}
	if errno == syscall.EEXIST || errno == syscall.EISDIR || errno == syscall.ENOTEMPTY {
		return fmt.Errorf("blob fs: store %s: a directory already has this key's path: %w", key, errno)
	}
	return fmt.Errorf("blob fs: store %s: %w", key, errno)
}

// Put implements blob.Store.
func (s *Store) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	dst, err := s.path(key)
	if err != nil {
		return err
	}
	if size > blob.MaxObjectSize {
		return blob.ErrTooLarge
	}
	tmp, _, err := s.writeTemp(ctx, r, size, nil)
	if err != nil {
		return err
	}
	return s.install(tmp, dst)
}

// PutVerified implements blob.VerifiedPutter: it hashes while it writes the
// temporary file and renames it into place only if the SHA-256 matches.
func (s *Store) PutVerified(ctx context.Context, key string, r io.Reader, size int64, sum []byte) error {
	dst, err := s.path(key)
	if err != nil {
		return err
	}
	if len(sum) != sha256.Size {
		return fmt.Errorf("put verified %s: want a %d-byte SHA-256", key, sha256.Size)
	}
	if size < 0 {
		return fmt.Errorf("put verified %s: the size must be known", key)
	}
	tmp, _, err := s.writeTemp(ctx, r, size, sum)
	if err != nil {
		return err
	}
	return s.install(tmp, dst)
}

// Delete implements blob.Store. It also removes parent directories that
// become empty, up to the root.
func (s *Store) Delete(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p, err := s.path(key)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, iofs.ErrNotExist) {
		return err
	}
	s.pruneEmptyParents(filepath.Dir(p))
	return nil
}

func (s *Store) pruneEmptyParents(dir string) {
	for dir != s.root && strings.HasPrefix(dir, s.root+string(filepath.Separator)) {
		// Remove fails on a non-empty directory, which ends the walk.
		if os.Remove(dir) != nil {
			return
		}
		dir = filepath.Dir(dir)
	}
}

// List implements blob.Store. It walks only the directory that holds the
// prefix and visits entries in key order, holding one directory listing in
// memory at a time.
func (s *Store) List(ctx context.Context, prefix string, fn func(blob.Info) error) error {
	dirKey := ""
	if i := strings.LastIndex(prefix, "/"); i >= 0 {
		dirKey = prefix[:i]
	}
	dir := s.root
	if dirKey != "" {
		if err := blob.ValidateKey(dirKey); err != nil {
			return nil //nolint:nilerr // No valid key has this prefix, so the listing is empty.
		}
		dir = filepath.Join(s.root, filepath.FromSlash(dirKey))
	}
	err := s.walk(ctx, dir, dirKey, prefix, fn)
	if errors.Is(err, iofs.ErrNotExist) {
		return nil
	}
	return err
}

func (s *Store) walk(ctx context.Context, dir, dirKey, prefix string, fn func(blob.Info) error) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	type item struct {
		sortKey string
		key     string
		entry   os.DirEntry
	}
	items := make([]item, 0, len(entries))
	for _, e := range entries {
		key := e.Name()
		if dirKey != "" {
			key = dirKey + "/" + key
		}
		if dirKey == "" && e.Name() == stagingDir {
			continue
		}
		sortKey := key
		if e.IsDir() {
			sortKey += "/"
		}
		// Skip entries that cannot contain a key with the prefix.
		if !strings.HasPrefix(sortKey, prefix) && !strings.HasPrefix(prefix, sortKey) {
			continue
		}
		items = append(items, item{sortKey: sortKey, key: key, entry: e})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].sortKey < items[j].sortKey })
	for _, it := range items {
		if err := ctx.Err(); err != nil {
			return err
		}
		if it.entry.IsDir() {
			err := s.walk(ctx, filepath.Join(dir, it.entry.Name()), it.key, prefix, fn)
			if err != nil && !errors.Is(err, iofs.ErrNotExist) {
				return err
			}
			continue
		}
		if !strings.HasPrefix(it.key, prefix) || !it.entry.Type().IsRegular() {
			continue
		}
		fi, err := it.entry.Info()
		if errors.Is(err, iofs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if err := fn(blob.Info{Key: it.key, Size: fi.Size(), ModTime: fi.ModTime().UTC(), ETag: etagOf(fi)}); err != nil {
			return err
		}
	}
	return nil
}

// Copy implements blob.Store. It hard-links the source where the
// filesystem allows, which is safe because objects are never modified in
// place, and copies otherwise.
func (s *Store) Copy(ctx context.Context, srcKey, dstKey string) error {
	src, err := s.path(srcKey)
	if err != nil {
		return err
	}
	dst, err := s.path(dstKey)
	if err != nil {
		return err
	}
	if fi, err := os.Stat(src); errors.Is(err, iofs.ErrNotExist) || (err == nil && fi.IsDir()) {
		return notFound(srcKey)
	} else if err != nil {
		return err
	}
	tmp := filepath.Join(s.staging, "link-"+uuid.NewString())
	if err := os.Link(src, tmp); err == nil {
		return s.install(tmp, dst)
	}
	f, err := os.Open(src) //nolint:gosec // The path is a validated key under the root.
	if errors.Is(err, iofs.ErrNotExist) {
		return notFound(srcKey)
	}
	if err != nil {
		return err
	}
	defer f.Close()
	staged, _, err := s.writeTemp(ctx, f, -1, nil)
	if err != nil {
		return err
	}
	return s.install(staged, dst)
}

// PresignGet implements blob.Store. The filesystem backend cannot presign.
func (s *Store) PresignGet(context.Context, string, time.Duration, string) (string, error) {
	return "", blob.ErrNotSupported
}

// PresignPart implements blob.Store. The filesystem backend cannot presign.
func (s *Store) PresignPart(context.Context, string, string, int, time.Duration) (string, error) {
	return "", blob.ErrNotSupported
}

// PresignPut implements blob.Store. The filesystem backend cannot presign.
func (s *Store) PresignPut(context.Context, string, int64, []byte, time.Duration) (blob.PresignedRequest, error) {
	return blob.PresignedRequest{}, blob.ErrNotSupported
}
