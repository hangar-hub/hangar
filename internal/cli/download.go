package cli

import (
	"context"
	"crypto/sha1" // #nosec G505 -- git blob IDs are SHA-1 by definition.
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/sync/errgroup"

	"github.com/hangar-hub/hangar/internal/api/gen"
	"github.com/hangar-hub/hangar/internal/model"
)

// downloadConcurrency is how many files the CLI downloads at once.
const downloadConcurrency = 4

// wantFile is one file to download.
type wantFile struct {
	Path string
	Size int64
	// ETag is the LFS SHA-256, or the git blob SHA-1 for regular files.
	ETag string
	LFS  bool
}

// fileOf describes a tree entry for download.
func fileOf(e gen.TreeEntry) wantFile {
	if e.Lfs != nil {
		return wantFile{Path: e.Path, Size: e.Lfs.Size, ETag: e.Lfs.Oid, LFS: true}
	}
	return wantFile{Path: e.Path, Size: e.Size, ETag: e.Oid}
}

// downloaded is a file the CLI fetched or found already present.
type downloaded struct {
	Path      string `json:"path"`
	Size      int64  `json:"size"`
	LocalPath string `json:"local_path"`
	Cached    bool   `json:"cached"`
}

// downloader fetches files of one Commit into the Hugging Face cache or a folder.
type downloader struct {
	c      *client
	id     model.RepoID
	commit string
	// cache is used when dir is empty.
	cache hfCache
	dir   string
	prog  *progress
	locks keyedLocks
}

// fetchAll downloads every file, several at once.
func (d *downloader) fetchAll(ctx context.Context, files []wantFile) ([]downloaded, error) {
	out := make([]downloaded, len(files))
	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(downloadConcurrency)
	for i, f := range files {
		g.Go(func() error {
			r, err := d.fetch(ctx, f)
			if err != nil {
				return fmt.Errorf("%s: %w", f.Path, err)
			}
			out[i] = r
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	return out, nil
}

// fetch downloads one file unless it is already there.
func (d *downloader) fetch(ctx context.Context, f wantFile) (downloaded, error) {
	if err := checkRepoPath(f.Path); err != nil {
		return downloaded{}, err
	}
	var target string
	if d.dir != "" {
		target = filepath.Join(d.dir, filepath.FromSlash(f.Path))
		if fileExists(target) && hashMatches(target, f) {
			d.prog.add(f.Size)
			return downloaded{Path: f.Path, Size: f.Size, LocalPath: target, Cached: true}, nil
		}
	} else {
		defer d.locks.lock(f.ETag)()
		target = d.cache.blobPath(d.id, f.ETag)
		if fileExists(target) {
			d.prog.add(f.Size)
			link, err := d.cache.link(d.id, d.commit, f.Path, f.ETag)
			return downloaded{Path: f.Path, Size: f.Size, LocalPath: link, Cached: true}, err
		}
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
		return downloaded{}, err
	}
	if err := d.get(ctx, f, target); err != nil {
		return downloaded{}, err
	}
	local := target
	if d.dir == "" {
		link, err := d.cache.link(d.id, d.commit, f.Path, f.ETag)
		if err != nil {
			return downloaded{}, err
		}
		local = link
	}
	return downloaded{Path: f.Path, Size: f.Size, LocalPath: local}, nil
}

// get downloads f into target through target.incomplete, resuming a
// partial download with a Range request and verifying the hash before the
// rename. A download that breaks off for a reason that may pass
// (transient) resumes from where it stopped after a growing wait, up to
// transferAttempts times. Progress counts each byte once.
func (d *downloader) get(ctx context.Context, f wantFile, target string) error {
	for attempt := 1; ; attempt++ {
		counted, err := d.getOnce(ctx, f, target)
		if err == nil {
			return nil
		}
		// The next attempt counts what is on disk again.
		d.prog.add(-counted)
		if !transient(ctx, err) || attempt >= transferAttempts {
			return err
		}
		if werr := waitRetry(ctx, retryWait(err, attempt)); werr != nil {
			return err
		}
	}
}

// getOnce makes one download request for f, from the end of any partial
// file, and returns how many bytes it added to the progress.
func (d *downloader) getOnce(ctx context.Context, f wantFile, target string) (int64, error) {
	tmp := target + ".incomplete"
	h := newFileHash(f)
	var have int64
	if st, err := os.Stat(tmp); err == nil && st.Size() < f.Size {
		in, err := os.Open(tmp) // #nosec G304 -- our own partial file.
		if err != nil {
			return 0, err
		}
		have, err = io.Copy(h, in)
		_ = in.Close()
		if err != nil {
			return 0, err
		}
	} else {
		_ = os.Remove(tmp)
	}
	header := http.Header{}
	if have > 0 {
		header.Set("Range", "bytes="+strconv.FormatInt(have, 10)+"-")
	}
	resp, err := d.c.do(ctx, http.MethodGet, d.c.url(resolvePath(d.id, d.commit, f.Path)), nil, header, http.StatusOK, http.StatusPartialContent)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	flags := os.O_CREATE | os.O_WRONLY | os.O_APPEND
	if resp.StatusCode == http.StatusOK && have > 0 {
		// The server ignored the range: start again.
		h = newFileHash(f)
		have = 0
		flags = os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	}
	d.prog.add(have)
	out, err := os.OpenFile(tmp, flags, 0o644) // #nosec G302 G304 -- model files are not secret.
	if err != nil {
		return have, err
	}
	body := &countingProgress{r: resp.Body, p: d.prog}
	n, err := io.Copy(io.MultiWriter(out, h), body)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	counted := have + body.n
	if err != nil {
		if ctx.Err() == nil && !errors.Is(err, os.ErrClosed) {
			// The body broke off: the connection, not the disk.
			var pe *os.PathError
			if !errors.As(err, &pe) {
				err = &transportError{err: err}
			}
		}
		return counted, fmt.Errorf("download interrupted after %s; run the command again to resume: %w", formatBytes(have+n), err)
	}
	if have+n < f.Size {
		// The body ended early, as one cut off by a proxy does.
		return counted, fmt.Errorf("received %d bytes, expected %d; run the command again to resume: %w", have+n, f.Size,
			&transportError{err: io.ErrUnexpectedEOF})
	}
	if have+n > f.Size {
		_ = os.Remove(tmp)
		return counted, fmt.Errorf("received %d bytes, expected %d", have+n, f.Size)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != f.ETag {
		_ = os.Remove(tmp)
		return counted, fmt.Errorf("the checksum is %s, expected %s", got, f.ETag)
	}
	return counted, os.Rename(tmp, target)
}

// newFileHash returns SHA-256 for LFS files and the git blob hasher for
// regular files, matching the ETag.
func newFileHash(f wantFile) hash.Hash {
	if f.LFS {
		return sha256.New()
	}
	h := sha1.New() // #nosec G401 -- git blob IDs are SHA-1.
	fmt.Fprintf(h, "blob %d\x00", f.Size)
	return h
}

// hashMatches reports whether an existing file has the expected hash.
func hashMatches(p string, f wantFile) bool {
	st, err := os.Stat(p)
	if err != nil || st.Size() != f.Size {
		return false
	}
	in, err := os.Open(p) // #nosec G304 -- a file the CLI wrote.
	if err != nil {
		return false
	}
	defer in.Close()
	h := newFileHash(f)
	if _, err := io.Copy(h, in); err != nil {
		return false
	}
	return hex.EncodeToString(h.Sum(nil)) == f.ETag
}

// checkRepoPath refuses paths that would escape the target folder.
func checkRepoPath(p string) error {
	if _, err := model.CleanRepoPath(p); err != nil || strings.Contains(p, "\\") {
		return fmt.Errorf("refusing unsafe path %q", p)
	}
	return nil
}

// globMatcher matches Repo paths against fnmatch-style patterns, as
// huggingface_hub's allow_patterns do: `*` also matches `/`, and a pattern
// ending in `/` matches everything under that folder.
type globMatcher struct{ res []*regexp.Regexp }

func newGlobMatcher(patterns []string) (globMatcher, error) {
	var m globMatcher
	for _, p := range patterns {
		if strings.HasSuffix(p, "/") {
			p += "*"
		}
		var b strings.Builder
		b.WriteString("^")
		for i := 0; i < len(p); i++ {
			switch ch := p[i]; ch {
			case '*':
				b.WriteString(".*")
			case '?':
				b.WriteString(".")
			case '[':
				j := strings.IndexByte(p[i:], ']')
				if j < 0 {
					b.WriteString(`\[`)
					continue
				}
				class := p[i+1 : i+j]
				if strings.HasPrefix(class, "!") {
					class = "^" + class[1:]
				}
				b.WriteString("[" + class + "]")
				i += j
			default:
				b.WriteString(regexp.QuoteMeta(string(ch)))
			}
		}
		b.WriteString("$")
		re, err := regexp.Compile(b.String())
		if err != nil {
			return m, usagef("bad pattern %q: %v", p, err)
		}
		m.res = append(m.res, re)
	}
	return m, nil
}

func (m globMatcher) empty() bool { return len(m.res) == 0 }

func (m globMatcher) match(p string) bool {
	for _, re := range m.res {
		if re.MatchString(p) {
			return true
		}
	}
	return false
}

// keyedLocks serialises work on the same key, such as two files with the
// same content that share one cache blob.
type keyedLocks struct {
	mu sync.Mutex
	m  map[string]*sync.Mutex
}

func (k *keyedLocks) lock(key string) func() {
	k.mu.Lock()
	if k.m == nil {
		k.m = map[string]*sync.Mutex{}
	}
	l, ok := k.m[key]
	if !ok {
		l = &sync.Mutex{}
		k.m[key] = l
	}
	k.mu.Unlock()
	l.Lock()
	return l.Unlock
}
