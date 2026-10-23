package model

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// LFSPointerVersion is the only pointer version git-lfs writes.
const LFSPointerVersion = "https://git-lfs.github.com/spec/v1"

// MaxLFSPointerSize is the largest pointer file git-lfs will parse.
const MaxLFSPointerSize = 1024

// LFSPointer is the content of a git-lfs pointer file.
type LFSPointer struct {
	// OID is the lower-case hex SHA-256 of the object.
	OID string
	// Size is the object size in bytes.
	Size int64
}

// ErrNotLFSPointer is returned when data is not a valid pointer file.
var ErrNotLFSPointer = errors.New("not a git-lfs pointer")

var sha256Re = regexp.MustCompile(`^[0-9a-f]{64}$`)

// IsSHA256 reports whether s is a 64-character lower-case hex string.
func IsSHA256(s string) bool { return sha256Re.MatchString(s) }

// Bytes returns the pointer file byte-for-byte as `git lfs pointer` writes it.
func (p LFSPointer) Bytes() []byte {
	return fmt.Appendf(nil, "version %s\noid sha256:%s\nsize %d\n", LFSPointerVersion, p.OID, p.Size)
}

// GitOID returns the git blob SHA-1 of the pointer file.
func (p LFSPointer) GitOID() string { return GitBlobID(p.Bytes()) }

// PointerSize returns the size of the pointer file, which the Hub reports
// as `pointerSize`.
func (p LFSPointer) PointerSize() int { return len(p.Bytes()) }

// Validate checks the OID format and size.
func (p LFSPointer) Validate() error {
	if !IsSHA256(p.OID) {
		return fmt.Errorf("%w: oid %q is not a lowercase sha256", ErrNotLFSPointer, p.OID)
	}
	if p.Size < 0 {
		return fmt.Errorf("%w: negative size", ErrNotLFSPointer)
	}
	return nil
}

// ParseLFSPointer parses a pointer file following the git-lfs specification:
// UTF-8 `key value` lines sorted by key with `version` first, ending in a
// newline, at most MaxLFSPointerSize bytes. Extension keys (`ext-*`) are
// accepted and ignored.
func ParseLFSPointer(data []byte) (LFSPointer, error) {
	if len(data) == 0 || len(data) > MaxLFSPointerSize {
		return LFSPointer{}, ErrNotLFSPointer
	}
	if data[len(data)-1] != '\n' {
		return LFSPointer{}, fmt.Errorf("%w: missing final newline", ErrNotLFSPointer)
	}
	lines := bytes.Split(data[:len(data)-1], []byte{'\n'})
	var (
		p                 LFSPointer
		haveOID, haveSize bool
		prevKey           string
	)
	for i, raw := range lines {
		key, value, ok := strings.Cut(string(raw), " ")
		if !ok || key == "" {
			return LFSPointer{}, fmt.Errorf("%w: malformed line %d", ErrNotLFSPointer, i+1)
		}
		if i == 0 {
			if key != "version" || value != LFSPointerVersion {
				return LFSPointer{}, fmt.Errorf("%w: unsupported version", ErrNotLFSPointer)
			}
			prevKey = key
			continue
		}
		if key <= prevKey && prevKey != "version" {
			return LFSPointer{}, fmt.Errorf("%w: keys are not sorted", ErrNotLFSPointer)
		}
		prevKey = key
		switch key {
		case "version":
			return LFSPointer{}, fmt.Errorf("%w: duplicate version", ErrNotLFSPointer)
		case "oid":
			hex, ok := strings.CutPrefix(value, "sha256:")
			if !ok || !IsSHA256(hex) {
				return LFSPointer{}, fmt.Errorf("%w: oid must be sha256:<64 lowercase hex>", ErrNotLFSPointer)
			}
			p.OID, haveOID = hex, true
		case "size":
			n, err := strconv.ParseInt(value, 10, 64)
			if err != nil || n < 0 || strconv.FormatInt(n, 10) != value {
				return LFSPointer{}, fmt.Errorf("%w: invalid size", ErrNotLFSPointer)
			}
			p.Size, haveSize = n, true
		default:
			if !strings.HasPrefix(key, "ext-") {
				return LFSPointer{}, fmt.Errorf("%w: unknown key %q", ErrNotLFSPointer, key)
			}
		}
	}
	if !haveOID || !haveSize {
		return LFSPointer{}, fmt.Errorf("%w: missing oid or size", ErrNotLFSPointer)
	}
	return p, nil
}

// LFSObjectKey returns the Blob store key of an LFS object:
// `lfs/sha256/<oid[0:2]>/<oid[2:4]>/<oid>`.
func LFSObjectKey(oid string) string {
	if len(oid) < 4 {
		return "lfs/sha256/" + oid
	}
	return "lfs/sha256/" + oid[0:2] + "/" + oid[2:4] + "/" + oid
}
