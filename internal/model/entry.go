package model

import (
	"errors"
	"fmt"
	"path"
	"strings"
	"time"
)

// EntryType is the type of a tree Entry.
type EntryType string

// Entry types, as the Hub API names them.
const (
	EntryFile      EntryType = "file"
	EntryDirectory EntryType = "directory"
)

// LFSInfo describes the LFS object behind a file Entry.
type LFSInfo struct {
	// OID is the SHA-256 of the content.
	OID string
	// Size is the content size in bytes.
	Size int64
	// PointerSize is the size of the pointer file stored in git.
	PointerSize int
}

// CommitSummary is the short form of a Commit used in tree listings
// (`lastCommit` in the Hub API).
type CommitSummary struct {
	SHA   CommitSHA
	Title string
	Date  time.Time
}

// Entry is one file or directory in a Commit's tree.
type Entry struct {
	// Path is relative to the Repo root, with no leading slash.
	Path string
	// Type is file or directory.
	Type EntryType
	// Size is the content size: for LFS files, the object size, not the
	// pointer size. Directories have size 0.
	Size int64
	// OID is the git SHA-1 of the blob (for LFS files, of the pointer file)
	// or of the tree.
	OID string
	// LFS is set for files stored in LFS.
	LFS *LFSInfo
	// Present is false for LFS files whose object is not in the Arsenal, as
	// in a Partial mirror. It is always true for regular files and directories.
	Present bool
	// LastCommit is the Commit that last changed the path, when requested.
	LastCommit *CommitSummary
}

// IsLFS reports whether the Entry is a file stored in LFS.
func (e Entry) IsLFS() bool { return e.LFS != nil }

// ETag returns the value the Hub uses as the file's ETag: the SHA-256 for
// LFS files and the git blob SHA-1 otherwise.
func (e Entry) ETag() string {
	if e.LFS != nil {
		return e.LFS.OID
	}
	return e.OID
}

// ErrInvalidPath is returned by CleanRepoPath.
var ErrInvalidPath = errors.New("invalid path in repo")

// MaxPathLength is the longest path, in bytes, a Repo may hold: PATH_MAX on
// Linux. Pushes, Pulls and Commits with a longer path are refused, which
// also bounds how deep a tree can nest (04-git.md "Push").
const MaxPathLength = 4096

// CleanRepoPath validates and normalises a path inside a Repo. It refuses
// absolute paths, `..` components, `.git` components and NUL bytes, as
// huggingface_hub does in _commit_api.py _validate_path_in_repo, as well as
// backslashes, paths longer than MaxPathLength and the names
// ValidPathComponent refuses. A trailing slash is removed. The empty string
// and "." mean the root and return "".
func CleanRepoPath(p string) (string, error) {
	if strings.ContainsRune(p, 0) || strings.Contains(p, "\\") {
		return "", fmt.Errorf("%w: %q", ErrInvalidPath, p)
	}
	if strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("%w: %q must not start with '/'", ErrInvalidPath, p)
	}
	p = strings.TrimSuffix(p, "/")
	if p == "" || p == "." {
		return "", nil
	}
	if len(p) > MaxPathLength {
		return "", fmt.Errorf("%w: the path is %d bytes long, over the limit of %d", ErrInvalidPath, len(p), MaxPathLength)
	}
	for _, part := range strings.Split(p, "/") {
		if err := ValidPathComponent(part); err != nil {
			return "", fmt.Errorf("%w: %q %s", ErrInvalidPath, p, err.Error())
		}
	}
	return path.Clean(p), nil
}

// ValidPathComponent checks one file or folder name in a Repo, as git's
// fsck does for tree entries (fsck.c fsck_tree: hasDot, hasDotdot,
// hasDotgit, fullPathname, plus Hangar's refusal of backslashes): it
// refuses empty names, "." and "..", names with a slash, backslash or NUL,
// and every name a client file system could treat as ".git": any case,
// the NTFS short name "git~1", trailing dots and spaces, and HFS+
// ignorable code points (git path.c is_ntfs_dotgit and utf8.c
// is_hfs_dotgit). A Commit holding such a name could write into a
// client's .git folder on checkout (CVE-2014-9390) or outside the folder a
// client downloads into.
func ValidPathComponent(name string) error {
	switch {
	case name == "", name == ".", name == "..":
		return errors.New("has an empty, '.' or '..' component")
	case strings.ContainsAny(name, "/\\\x00"):
		return errors.New("has a name with a slash, backslash or NUL byte")
	case isNTFSDotGit(name), isHFSDotGit(name):
		return errors.New("has a .git component")
	}
	return nil
}

// isNTFSDotGit reports whether name is ".git" or "git~1" in any case,
// followed only by dots and spaces (which NTFS drops), as git's
// is_ntfs_dotgit does for one path component.
func isNTFSDotGit(name string) bool {
	lower := strings.ToLower(name)
	var rest string
	switch {
	case strings.HasPrefix(lower, ".git"):
		rest = lower[len(".git"):]
	case strings.HasPrefix(lower, "git~1"):
		rest = lower[len("git~1"):]
	default:
		return false
	}
	// NTFS ignores what follows a colon (an alternate data stream).
	i := strings.IndexFunc(rest, func(r rune) bool { return r != '.' && r != ' ' })
	return i < 0 || rest[i] == ':'
}

// isHFSDotGit reports whether name reads as ".git" once HFS+ drops the
// code points it ignores, in any ASCII case, as git's is_hfs_dotgit does.
func isHFSDotGit(name string) bool {
	var b strings.Builder
	for _, r := range name {
		if hfsIgnorable(r) {
			continue
		}
		if r > 127 {
			return false
		}
		b.WriteRune(r)
	}
	return strings.EqualFold(b.String(), ".git")
}

// hfsIgnorable lists the code points HFS+ ignores in names (git utf8.c
// next_hfs_char).
func hfsIgnorable(r rune) bool {
	switch {
	case r >= 0x200c && r <= 0x200f, // ZERO WIDTH NON-JOINER to RIGHT-TO-LEFT MARK
		r >= 0x202a && r <= 0x202e, // LEFT-TO-RIGHT EMBEDDING to RIGHT-TO-LEFT OVERRIDE
		r >= 0x206a && r <= 0x206f, // INHIBIT SYMMETRIC SWAPPING to NOMINAL DIGIT SHAPES
		r == 0xfeff:                // ZERO WIDTH NO-BREAK SPACE
		return true
	}
	return false
}
