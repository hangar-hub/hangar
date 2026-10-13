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

// CleanRepoPath validates and normalises a path inside a Repo. It refuses
// absolute paths, `..` components, `.git` components and NUL bytes, as
// huggingface_hub does in _commit_api.py _validate_path_in_repo. A trailing
// slash is removed. The empty string and "." mean the root and return "".
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
	for _, part := range strings.Split(p, "/") {
		switch part {
		case "", ".", "..":
			return "", fmt.Errorf("%w: %q has an empty, '.' or '..' component", ErrInvalidPath, p)
		case ".git":
			return "", fmt.Errorf("%w: %q has a .git component", ErrInvalidPath, p)
		}
	}
	return path.Clean(p), nil
}
