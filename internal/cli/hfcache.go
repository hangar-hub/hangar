package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/hangar-hub/hangar/internal/model"
)

// hfCache is a Hugging Face cache directory in the layout huggingface_hub
// uses (docs/spec/08-local-tools.md "Local cache"):
// <root>/models--{ns}--{name}/{blobs,snapshots,refs}. Blobs are named by
// ETag: the SHA-256 of an LFS object, or the git blob SHA-1 of a regular
// file, so Python libraries and the CLI share the files.
type hfCache struct{ root string }

// defaultHFCacheDir follows huggingface_hub: $HF_HUB_CACHE, then
// $HF_HOME/hub, then $XDG_CACHE_HOME/huggingface/hub, then
// ~/.cache/huggingface/hub.
func defaultHFCacheDir() string {
	if d := os.Getenv("HF_HUB_CACHE"); d != "" {
		return d
	}
	if d := os.Getenv("HF_HOME"); d != "" {
		return filepath.Join(d, "hub")
	}
	if d := os.Getenv("XDG_CACHE_HOME"); d != "" {
		return filepath.Join(d, "huggingface", "hub")
	}
	return filepath.Join(userHome(), ".cache", "huggingface", "hub")
}

// repoFolderName returns `models--ns--name` or `datasets--ns--name`.
func repoFolderName(id model.RepoID) string {
	return id.Type.Plural() + "--" + id.Namespace + "--" + id.Name
}

// parseRepoFolderName is the reverse of repoFolderName.
func parseRepoFolderName(name string) (model.RepoID, bool) {
	parts := strings.Split(name, "--")
	if len(parts) != 3 {
		return model.RepoID{}, false
	}
	t, err := model.ParseRepoType(parts[0])
	if err != nil || (parts[0] != "models" && parts[0] != "datasets") {
		return model.RepoID{}, false
	}
	id := model.RepoID{Type: t, Namespace: parts[1], Name: parts[2]}
	if id.Validate() != nil {
		return model.RepoID{}, false
	}
	return id, true
}

func (h hfCache) repoDir(id model.RepoID) string {
	return filepath.Join(h.root, repoFolderName(id))
}

func (h hfCache) blobPath(id model.RepoID, etag string) string {
	return filepath.Join(h.repoDir(id), "blobs", etag)
}

func (h hfCache) snapshotPath(id model.RepoID, commit, file string) string {
	return filepath.Join(h.repoDir(id), "snapshots", commit, filepath.FromSlash(file))
}

// link points snapshots/<commit>/<file> at blobs/<etag> with a relative
// symlink, as huggingface_hub does, or copies the blob where symlinks are
// not allowed.
func (h hfCache) link(id model.RepoID, commit, file, etag string) (string, error) {
	dst := h.snapshotPath(id, commit, file)
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return "", err
	}
	blob := h.blobPath(id, etag)
	rel, err := filepath.Rel(filepath.Dir(dst), blob)
	if err != nil {
		return "", err
	}
	if cur, err := os.Readlink(dst); err == nil && cur == rel {
		return dst, nil
	}
	_ = os.Remove(dst)
	if err := os.Symlink(rel, dst); err != nil {
		if err := copyFile(blob, dst); err != nil {
			return "", err
		}
	}
	return dst, nil
}

// fullSHA matches a 40-hex Commit SHA.
var fullSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

// writeRef records which Commit a branch or tag pointed at, unless the
// Revision is itself a Commit SHA.
func (h hfCache) writeRef(id model.RepoID, revision, commit string) error {
	if revision == "" || fullSHA.MatchString(revision) {
		return nil
	}
	p := filepath.Join(h.repoDir(id), "refs", filepath.FromSlash(revision))
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(commit), 0o644) // #nosec G306 -- huggingface_hub writes refs world-readable too.
}

// readRef returns the Commit a cached branch or tag points at.
func (h hfCache) readRef(id model.RepoID, revision string) (string, error) {
	b, err := os.ReadFile(filepath.Join(h.repoDir(id), "refs", filepath.FromSlash(revision))) // #nosec G304 -- inside the cache.
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// copyFile copies src to dst through a temporary file.
func copyFile(src, dst string) error {
	in, err := os.Open(src) // #nosec G304 -- paths the CLI chose.
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644) // #nosec G302 G304 -- model files are not secret.
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

// fileExists reports whether a regular file exists.
func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}

// cachedSnapshot is one snapshot in an existing Hugging Face cache.
type cachedSnapshot struct {
	ID     model.RepoID
	Commit string
	// Refs names the branches and tags that point at the Commit.
	Refs []string
	// ModTime is when the snapshot folder last changed, which is when
	// huggingface_hub last added a file to it.
	ModTime time.Time
	// Files maps paths in the Repo to their resolved blob files.
	Files map[string]string
	// Outside lists files that are symbolic links out of the Repo's cache
	// folder, which are not read.
	Outside []string
}

// snapshots lists every complete snapshot in the cache.
func (h hfCache) snapshots() ([]cachedSnapshot, error) {
	dirs, err := os.ReadDir(h.root)
	if err != nil {
		return nil, fmt.Errorf("read the Hugging Face cache %s: %w", h.root, err)
	}
	var out []cachedSnapshot
	for _, d := range dirs {
		id, ok := parseRepoFolderName(d.Name())
		if !ok || !d.IsDir() {
			continue
		}
		refs := map[string][]string{}
		refDir := filepath.Join(h.repoDir(id), "refs")
		_ = filepath.WalkDir(refDir, func(p string, e os.DirEntry, err error) error {
			if err != nil {
				return nil //nolint:nilerr // a Repo without refs still has snapshots to read.
			}
			if e.IsDir() {
				return nil
			}
			rel, _ := filepath.Rel(refDir, p)
			if c, err := h.readRef(id, filepath.ToSlash(rel)); err == nil {
				refs[c] = append(refs[c], filepath.ToSlash(rel))
			}
			return nil
		})
		snapDir := filepath.Join(h.repoDir(id), "snapshots")
		commits, err := os.ReadDir(snapDir)
		if err != nil {
			continue
		}
		// Snapshot files may only link into this Repo's cache folder.
		scope, err := linkScope(h.repoDir(id))
		if err != nil {
			continue
		}
		for _, c := range commits {
			if !c.IsDir() || !fullSHA.MatchString(c.Name()) {
				continue
			}
			s := cachedSnapshot{ID: id, Commit: c.Name(), Refs: refs[c.Name()], Files: map[string]string{}}
			if info, err := c.Info(); err == nil {
				s.ModTime = info.ModTime()
			}
			root := filepath.Join(snapDir, c.Name())
			err := filepath.WalkDir(root, func(p string, e os.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if e.IsDir() {
					return nil
				}
				rel, _ := filepath.Rel(root, p)
				if _, err := os.Stat(p); err != nil {
					return nil //nolint:nilerr // a dangling link: the blob was deleted, so skip the file.
				}
				real, ok := resolveInScope(p, scope)
				if !ok {
					s.Outside = append(s.Outside, filepath.ToSlash(rel))
					return nil
				}
				s.Files[filepath.ToSlash(rel)] = real
				return nil
			})
			if err != nil {
				return nil, err
			}
			if len(s.Files) > 0 {
				out = append(out, s)
			}
		}
	}
	return out, nil
}
