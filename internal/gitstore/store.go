// Package gitstore stores git data for every Repo: immutable packfiles in
// the Blob store, refs and the pack list in the database, and a disposable
// local pack cache on each replica (docs/spec/04-git.md and
// docs/research/git-server-research.md).
//
// A ref may only point at a Commit whose closure is in live packs. Pack
// bytes are durable in the Blob store before the transaction that marks the
// pack live and moves refs with compare-and-swap, so any SHA a client sees
// can be fetched from any replica.
//
// gitstore drives the git binary (2.39 or later) for everything that touches
// objects. Only internal/repo calls it; other packages go through
// repo.Service.
package gitstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"golang.org/x/sync/singleflight"

	"github.com/hangar-hub/hangar/internal/blob"
	"github.com/hangar-hub/hangar/internal/db"
	"github.com/hangar-hub/hangar/internal/telemetry"
)

// Pack states (the packs table).
const (
	PackPending  = "pending"
	PackLive     = "live"
	PackRetired  = "retired"
	PackOrphaned = "orphaned"
	// PackDeleting is a pack a sweep has claimed and is deleting
	// (DeletePack).
	PackDeleting = "deleting"
)

// Options configure a Store.
type Options struct {
	DB    *db.DB
	Blobs blob.Store
	// CacheDir is the local pack cache (git.cache_dir). It is created if
	// missing and can be deleted at any time while Hangar is stopped.
	CacheDir string
	// CacheSize bounds the cache (git.cache_size). Zero means 10 GiB.
	CacheSize int64
	// Timeout bounds one git command (git.timeout). Zero means one hour.
	Timeout time.Duration
	// Instruments, when set, record hangar.git.cache.* metrics.
	Instruments *telemetry.Instruments
	Log         *slog.Logger
}

// Store is the git storage of every Repo. It is safe for concurrent use.
type Store struct {
	db        *db.DB
	blobs     blob.Store
	dir       string
	cacheSize int64
	git       Git
	inst      *telemetry.Instruments
	log       *slog.Logger

	sf      singleflight.Group
	mu      sync.Mutex
	entries map[uuid.UUID]*cacheEntry
	total   int64
}

type cacheEntry struct {
	size     int64
	lastUsed time.Time
	inUse    int
}

// New opens a Store, creating the cache directory and counting what an
// earlier run left in it.
func New(opts Options) (*Store, error) {
	if opts.DB == nil || opts.Blobs == nil {
		return nil, errors.New("gitstore: a database and a Blob store are required")
	}
	if opts.CacheDir == "" {
		return nil, errors.New("gitstore: git.cache_dir is required")
	}
	dir, err := filepath.Abs(opts.CacheDir)
	if err != nil {
		return nil, err
	}
	for _, sub := range []string{"repos", "views", "staging", "home"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o700); err != nil {
			return nil, fmt.Errorf("gitstore: create cache directory: %w", err)
		}
	}
	// Views and staging directories from a crashed run are never reused.
	for _, sub := range []string{"views", "staging"} {
		olds, _ := os.ReadDir(filepath.Join(dir, sub))
		for _, o := range olds {
			_ = os.RemoveAll(filepath.Join(dir, sub, o.Name()))
		}
	}
	size := opts.CacheSize
	if size <= 0 {
		size = 10 << 30
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = time.Hour
	}
	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	s := &Store{
		db: opts.DB, blobs: opts.Blobs, dir: dir, cacheSize: size,
		git: Git{Home: filepath.Join(dir, "home"), Timeout: timeout}, inst: opts.Instruments, log: log,
		entries: map[uuid.UUID]*cacheEntry{},
	}
	repos, _ := os.ReadDir(filepath.Join(dir, "repos"))
	for _, r := range repos {
		id, err := uuid.Parse(r.Name())
		if err != nil {
			continue
		}
		info, _ := r.Info()
		e := &cacheEntry{size: dirSize(filepath.Join(dir, "repos", r.Name(), "objects", "pack"))}
		if info != nil {
			e.lastUsed = info.ModTime()
		}
		s.entries[id] = e
		s.total += e.size
	}
	s.recordCacheBytes()
	return s, nil
}

// Git returns the git runner the Store uses.
func (s *Store) Git() Git { return s.git }

// PackKey returns the Blob store key of a pack file: ext is "pack", "idx"
// or "rev".
func PackKey(repoID uuid.UUID, sha, ext string) string {
	return blob.PrefixGit + repoID.String() + "/pack/pack-" + sha + "." + ext
}

// RepoPrefix returns the Blob store prefix holding a Repo's git data.
func RepoPrefix(repoID uuid.UUID) string { return blob.PrefixGit + repoID.String() + "/" }

// repoDir is the cache directory of a Repo: a bare repository holding only
// packs, with no refs.
func (s *Store) repoDir(repoID uuid.UUID) string {
	return filepath.Join(s.dir, "repos", repoID.String())
}

// Objects is a Repo's object store in the local cache, held open until
// Release. Dir is a bare repository with every live pack and no refs, so
// git commands that address objects by SHA can run in it.
type Objects struct {
	Dir     string
	release func()
	once    sync.Once
}

// Release lets the cache evict the Repo again.
func (o *Objects) Release() {
	if o == nil {
		return
	}
	o.once.Do(o.release)
}

// Acquire makes sure every live pack of a Repo is in the local cache and
// keeps the Repo's cache directory from eviction until Release. Concurrent
// callers for the same Repo share one download.
func (s *Store) Acquire(ctx context.Context, repoID uuid.UUID) (*Objects, error) {
	// A caller that has already given up gets an error, not whichever of
	// the finished sync and its cancellation the select below picks.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	e := s.entries[repoID]
	if e == nil {
		e = &cacheEntry{}
		s.entries[repoID] = e
	}
	e.inUse++
	e.lastUsed = time.Now()
	s.mu.Unlock()
	release := func() {
		s.mu.Lock()
		e.inUse--
		e.lastUsed = time.Now()
		s.mu.Unlock()
		s.evict()
	}
	ch := s.sf.DoChan(repoID.String(), func() (any, error) {
		// Shared work outlives any one caller's cancellation.
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.git.Timeout)
		defer cancel()
		return nil, s.sync(sctx, repoID)
	})
	select {
	case res := <-ch:
		if res.Err != nil {
			release()
			return nil, res.Err
		}
	case <-ctx.Done():
		release()
		return nil, ctx.Err()
	}
	return &Objects{Dir: s.repoDir(repoID), release: release}, nil
}

// initBare writes the files that make dir a bare repository.
func initBare(dir, headRef string) error {
	for _, sub := range []string{"objects/pack", "objects/info", "refs/heads", "refs/tags"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o700); err != nil {
			return err
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "HEAD"), []byte("ref: "+headRef+"\n"), 0o600); err != nil {
		return err
	}
	const config = "[core]\n\trepositoryformatversion = 0\n\tfilemode = true\n\tbare = true\n[gc]\n\tauto = 0\n[receive]\n\tautogc = false\n"
	return os.WriteFile(filepath.Join(dir, "config"), []byte(config), 0o600)
}

// sync downloads missing live packs into the Repo's cache directory.
func (s *Store) sync(ctx context.Context, repoID uuid.UUID) error {
	dir := s.repoDir(repoID)
	if _, err := os.Stat(filepath.Join(dir, "HEAD")); err != nil {
		if err := initBare(dir, "refs/heads/main"); err != nil {
			return fmt.Errorf("gitstore: create cache for %s: %w", repoID, err)
		}
	}
	var packs []string
	if err := s.db.NewSelect().Model((*db.Pack)(nil)).Column("pack_sha1").
		Where("repo_id = ?", repoID).Where("state = ?", PackLive).Scan(ctx, &packs); err != nil {
		return fmt.Errorf("gitstore: list packs: %w", err)
	}
	var added int64
	hit := true
	for _, sha := range packs {
		if _, err := os.Stat(filepath.Join(dir, "objects", "pack", "pack-"+sha+".idx")); err == nil {
			continue
		}
		hit = false
		n, err := s.download(ctx, repoID, sha, dir)
		if err != nil {
			return err
		}
		added += n
	}
	s.recordCacheRequest(hit)
	if added > 0 {
		s.mu.Lock()
		if e := s.entries[repoID]; e != nil {
			e.size += added
		}
		s.total += added
		s.mu.Unlock()
		s.recordCacheBytes()
	}
	return nil
}

// download fetches one pack's files, the .idx last.
func (s *Store) download(ctx context.Context, repoID uuid.UUID, sha, dir string) (int64, error) {
	var total int64
	for _, ext := range packExts {
		key := PackKey(repoID, sha, ext)
		rc, info, err := s.blobs.Get(ctx, key, nil)
		if err != nil {
			if optionalExt(ext) && errors.Is(err, blob.ErrNotFound) {
				continue
			}
			return total, fmt.Errorf("gitstore: download %s: %w", key, err)
		}
		dst := filepath.Join(dir, "objects", "pack", "pack-"+sha+"."+ext)
		err = writeFileAtomic(dst, rc)
		_ = rc.Close()
		if err != nil {
			return total, fmt.Errorf("gitstore: write %s: %w", dst, err)
		}
		total += info.Size
	}
	return total, nil
}

func writeFileAtomic(dst string, r io.Reader) error {
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".tmp-*")
	if err != nil {
		return err
	}
	if _, err := io.Copy(tmp, r); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o400); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), dst)
}

// addLocal copies a pack the Store just wrote into the Repo's cache
// directory, so the next read does not download it again.
func (s *Store) addLocal(repoID uuid.UUID, srcDir, sha string) {
	dir := s.repoDir(repoID)
	if _, err := os.Stat(filepath.Join(dir, "HEAD")); err != nil {
		if err := initBare(dir, "refs/heads/main"); err != nil {
			return
		}
	}
	var added int64
	for _, ext := range packExts {
		src := filepath.Join(srcDir, "pack-"+sha+"."+ext)
		dst := filepath.Join(dir, "objects", "pack", "pack-"+sha+"."+ext)
		if _, err := os.Stat(dst); err == nil {
			continue
		}
		f, err := os.Open(src) //nolint:gosec // src is a pack file in a staging directory Hangar created.
		if err != nil {
			continue
		}
		info, _ := f.Stat()
		if err := writeFileAtomic(dst, f); err == nil && info != nil {
			added += info.Size()
		}
		_ = f.Close()
	}
	s.mu.Lock()
	e := s.entries[repoID]
	if e == nil {
		e = &cacheEntry{lastUsed: time.Now()}
		s.entries[repoID] = e
	}
	e.size += added
	s.total += added
	s.mu.Unlock()
	s.recordCacheBytes()
	s.evict()
}

// evict removes the least recently used Repos that nobody is reading until
// the cache fits its size limit.
func (s *Store) evict() {
	s.mu.Lock()
	if s.total <= s.cacheSize {
		s.mu.Unlock()
		return
	}
	type cand struct {
		id uuid.UUID
		e  *cacheEntry
	}
	var cands []cand
	for id, e := range s.entries {
		if e.inUse == 0 {
			cands = append(cands, cand{id, e})
		}
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].e.lastUsed.Before(cands[j].e.lastUsed) })
	var trash []string
	for _, c := range cands {
		if s.total <= s.cacheSize {
			break
		}
		// Move the directory aside while holding the lock, so an Acquire
		// that starts afterwards rebuilds it from the Blob store instead of
		// reading a directory that is being deleted.
		t := filepath.Join(s.dir, "staging", "evict-"+uuid.NewString())
		if err := os.Rename(s.repoDir(c.id), t); err != nil && !errors.Is(err, os.ErrNotExist) {
			continue
		}
		s.total -= c.e.size
		delete(s.entries, c.id)
		trash = append(trash, t)
	}
	s.mu.Unlock()
	for _, t := range trash {
		_ = os.RemoveAll(t)
	}
	if len(trash) > 0 {
		s.recordCacheBytes()
	}
}

// Forget removes a Repo's cache directory, for purges.
func (s *Store) Forget(repoID uuid.UUID) {
	s.mu.Lock()
	if e := s.entries[repoID]; e != nil {
		s.total -= e.size
		delete(s.entries, repoID)
	}
	t := filepath.Join(s.dir, "staging", "forget-"+uuid.NewString())
	err := os.Rename(s.repoDir(repoID), t)
	s.mu.Unlock()
	if err == nil {
		_ = os.RemoveAll(t)
	}
	s.recordCacheBytes()
}

// CacheBytes reports the size of the local pack cache.
func (s *Store) CacheBytes() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.total
}

func (s *Store) recordCacheRequest(hit bool) {
	if s.inst == nil || s.inst.GitCacheRequests == nil {
		return
	}
	outcome := "miss"
	if hit {
		outcome = "hit"
	}
	s.inst.GitCacheRequests.Add(context.Background(), 1, metric.WithAttributes(attribute.String(telemetry.MetricOutcome, outcome)))
}

func (s *Store) recordCacheBytes() {
	if s.inst == nil || s.inst.GitCacheBytes == nil {
		return
	}
	s.inst.GitCacheBytes.Record(context.Background(), s.CacheBytes())
}

func dirSize(dir string) int64 {
	var n int64
	_ = filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if info, err := d.Info(); err == nil {
			n += info.Size()
		}
		return nil
	})
	return n
}

// tempDir makes a new directory under the cache for a view or staging repo.
func (s *Store) tempDir(kind string) (string, error) {
	dir := filepath.Join(s.dir, kind, uuid.NewString())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

// alternatesFor writes objects/info/alternates in dir pointing at the
// Repo's cache objects.
func alternatesFor(dir, objectsDir string) error {
	return os.WriteFile(filepath.Join(dir, "objects", "info", "alternates"), []byte(filepath.Join(objectsDir, "objects")+"\n"), 0o600)
}

// cleanName keeps ref names on a single line inside packed-refs.
func cleanName(s string) bool { return !strings.ContainsAny(s, "\n\r\x00 ") }
