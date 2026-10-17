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
// into LM Studio's folder are (06-upstreams-and-pulls.md "Bulk import"). A
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
// <cache>/<type>s--<ns>--<name>/blobs/<etag>.
func isCacheBlob(path string) bool {
	blobs := filepath.Dir(path)
	if filepath.Base(blobs) != "blobs" {
		return false
	}
	_, ok := parseRepoFolderName(filepath.Base(filepath.Dir(blobs)))
	return ok
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
