package model

import (
	"crypto/sha1" //nolint:gosec // git object IDs are SHA-1 by definition.
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// CommitSHA is a full 40-character lower-case hex git SHA-1 of a Commit.
type CommitSHA string

// ZeroSHA is the all-zero SHA that git uses for "no object" in ref updates.
const ZeroSHA CommitSHA = "0000000000000000000000000000000000000000"

// ErrInvalidSHA is returned by ParseCommitSHA.
var ErrInvalidSHA = errors.New("invalid commit sha")

var fullSHARe = regexp.MustCompile(`^[0-9a-f]{40}$`)

// ParseCommitSHA accepts a full 40-character hex SHA in either case.
func ParseCommitSHA(s string) (CommitSHA, error) {
	l := strings.ToLower(s)
	if !fullSHARe.MatchString(l) {
		return "", fmt.Errorf("%w: %q", ErrInvalidSHA, s)
	}
	return CommitSHA(l), nil
}

// IsFullSHA reports whether s is a 40-character hex string.
func IsFullSHA(s string) bool { return fullSHARe.MatchString(strings.ToLower(s)) }

// String returns the SHA as a string.
func (c CommitSHA) String() string { return string(c) }

// IsZero reports whether c is empty or the all-zero SHA.
func (c CommitSHA) IsZero() bool { return c == "" || c == ZeroSHA }

// Short returns the first 8 characters, for display.
func (c CommitSHA) Short() string {
	if len(c) < 8 {
		return string(c)
	}
	return string(c[:8])
}

// Ref name prefixes.
const (
	RefHeads        = "refs/heads/"
	RefTags         = "refs/tags/"
	RefPR           = "refs/pr/"
	RefConvert      = "refs/convert/"
	RefParquet      = "refs/convert/parquet"
	RefHangar       = "refs/hangar/"
	DefaultBranch   = "main"
	DefaultHeadRef  = RefHeads + DefaultBranch
	ParquetRevision = "refs/convert/parquet"
)

// specialRefRe matches the special refs the Hub client accepts unencoded,
// from huggingface_hub hf_api.py SPECIAL_REFS_REVISION_REGEX.
var specialRefRe = regexp.MustCompile(`^refs/(convert|pr)/\w+$`)

// IsSpecialRef reports whether rev is a `refs/pr/N` or `refs/convert/<word>` ref.
func IsSpecialRef(rev string) bool { return specialRefRe.MatchString(rev) }

// PRRef returns the ref name of pull request num.
func PRRef(num int) string { return RefPR + strconv.Itoa(num) }

// ParsePRRef returns the pull request number of a `refs/pr/N` ref.
func ParsePRRef(ref string) (int, bool) {
	s, ok := strings.CutPrefix(ref, RefPR)
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// BranchRef returns the full ref name of a branch.
func BranchRef(branch string) string {
	if strings.HasPrefix(branch, "refs/") {
		return branch
	}
	return RefHeads + branch
}

// TagRef returns the full ref name of a tag.
func TagRef(tag string) string {
	if strings.HasPrefix(tag, "refs/") {
		return tag
	}
	return RefTags + tag
}

// ShortRefName strips `refs/heads/` or `refs/tags/` from a full ref name.
func ShortRefName(ref string) string {
	if s, ok := strings.CutPrefix(ref, RefHeads); ok {
		return s
	}
	if s, ok := strings.CutPrefix(ref, RefTags); ok {
		return s
	}
	return ref
}

// ErrInvalidRefName is returned by ValidateRefName.
var ErrInvalidRefName = errors.New("invalid ref name")

// ValidateRefName applies git check-ref-format rules to a full ref name.
func ValidateRefName(ref string) error {
	bad := func(why string) error { return fmt.Errorf("%w: %q %s", ErrInvalidRefName, ref, why) }
	if !strings.HasPrefix(ref, "refs/") {
		return bad("must start with refs/")
	}
	if strings.HasSuffix(ref, "/") || strings.HasSuffix(ref, ".") || strings.HasSuffix(ref, ".lock") {
		return bad("cannot end with '/', '.' or '.lock'")
	}
	if strings.Contains(ref, "..") || strings.Contains(ref, "//") || strings.Contains(ref, "@{") {
		return bad("cannot contain '..', '//' or '@{'")
	}
	for _, part := range strings.Split(ref, "/") {
		if part == "" || strings.HasPrefix(part, ".") {
			return bad("has an empty component or one starting with '.'")
		}
	}
	for _, r := range ref {
		if r < 0x20 || r == 0x7f || strings.ContainsRune(" ~^:?*[\\", r) {
			return bad("contains a forbidden character")
		}
	}
	return nil
}

// GitObjectID returns the git SHA-1 of an object with the given type and
// content: sha1("<type> <len>\x00" + content).
func GitObjectID(objType string, content []byte) string {
	h := sha1.New() //nolint:gosec // git object IDs are SHA-1 by definition.
	_, _ = fmt.Fprintf(h, "%s %d\x00", objType, len(content))
	_, _ = h.Write(content)
	return hex.EncodeToString(h.Sum(nil))
}

// GitBlobID returns the git blob SHA-1 of content, which the Hub uses as
// the ETag of regular files and as the `oid` of tree entries.
func GitBlobID(content []byte) string { return GitObjectID("blob", content) }
