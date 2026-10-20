package cli

import (
	"os"
	"path/filepath"
	"strings"
)

// Symbolic links in folders the CLI uploads are followed only when they
// point inside the folder, or, in a Hugging Face cache snapshot, inside
// the Repo's cache folder, whose blobs folder the snapshot links into, or
// at a blob of any Hugging Face cache, as the files hangar lmstudio links
// into LM Studio's folder are (06-upstreams-and-pulls.md "Bulk import"),
// or at an entry of a cache's shared blob store, which huggingface_hub 2.x
// links each Repo's blobs into. A
// link to anything else, such as ../../.ssh/id_ed25519 in a cloned model
// folder, is skipped and named, so pushing or importing a folder never
// uploads a file from elsewhere on the machine.

// linkScope returns the folder that symbolic links under dir may resolve
// into, with symbolic links in its own path resolved.
func linkScope(dir string) (string, error) {
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", err
	}
	real, err = filepath.Abs(real)
	if err != nil {
		return "", err
	}
	// <cache>/<type>s--<ns>--<name>/snapshots/<commit>
	snapshots := filepath.Dir(real)
	if filepath.Base(snapshots) == "snapshots" {
		if _, ok := parseRepoFolderName(filepath.Base(filepath.Dir(snapshots))); ok {
			return filepath.Dir(snapshots), nil
		}
	}
	return real, nil
}

// resolveInScope resolves p, which may be a symbolic link, and reports
// whether the file it names is inside scope (from linkScope).
func resolveInScope(p, scope string) (string, bool) {
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", false
	}
	real, err = filepath.Abs(real)
	if err != nil {
		return "", false
	}
	return real, within(real, scope) || isCacheBlob(real)
}

// isCacheBlob reports whether path is a blob in a Hugging Face cache:
// <cache>/<type>s--<ns>--<name>/blobs/<etag>, or an entry of the cache's
// shared blob store (isSharedBlob).
func isCacheBlob(path string) bool {
	blobs := filepath.Dir(path)
	if filepath.Base(blobs) != "blobs" {
		return isSharedBlob(path)
	}
	_, ok := parseRepoFolderName(filepath.Base(filepath.Dir(blobs)))
	return ok
}

// sharedBlobsMarker is the file huggingface_hub 2.x writes in a cache's
// blobs folder when it keeps a shared blob store there.
const sharedBlobsMarker = ".huggingface-shared-blobs"

// isSharedBlob reports whether path is an entry of a Hugging Face cache's
// shared blob store, <cache>/blobs/<first two hex digits>/<Xet hash>, in
// which huggingface_hub 2.x keeps each Xet file once per cache and links
// every Repo's blobs/<etag> to it. The blobs folder must hold the store's
// marker file, so a hash-named file elsewhere is not followed.
func isSharedBlob(path string) bool {
	name := filepath.Base(path)
	prefix := filepath.Dir(path)
	blobs := filepath.Dir(prefix)
	if len(name) != 64 || !isLowerHex(name) || filepath.Base(prefix) != name[:2] || filepath.Base(blobs) != "blobs" {
		return false
	}
	fi, err := os.Lstat(filepath.Join(blobs, sharedBlobsMarker))
	return err == nil && fi.Mode().IsRegular()
}

func isLowerHex(s string) bool {
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// within reports whether path is dir or inside it. Both must be clean,
// absolute and free of symbolic links.
func within(path, dir string) bool {
	if path == dir {
		return true
	}
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)) && !filepath.IsAbs(rel)
}

// warnOutside names the symbolic links walkFolder skipped in dir.
func (a *app) warnOutside(dir string, outside []string) {
	for _, p := range outside {
		a.logf("Skipped %s: it is a symbolic link to a file outside %s. Copy the file into the folder to upload it.\n", p, dir)
	}
}
