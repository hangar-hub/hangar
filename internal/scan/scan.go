// Package scan checks a Commit's files for security problems: dangerous
// imports in pickle files (META-2) and integrity of safetensors, GGUF and
// LFS objects (META-3). scantest has an in-memory fake.
package scan

import (
	"context"
	"errors"
	"io"

	"github.com/google/uuid"

	"github.com/hangar-hub/hangar/internal/model"
)

// Status is a scan result, from best to worst.
type Status string

// Statuses.
const (
	StatusSafe       Status = "safe"
	StatusSuspicious Status = "suspicious"
	StatusUnsafe     Status = "unsafe"
	StatusError      Status = "error"
)

func rank(s Status) int {
	switch s {
	case StatusSafe:
		return 1
	case StatusError:
		return 2
	case StatusSuspicious:
		return 3
	case StatusUnsafe:
		return 4
	default:
		return 0
	}
}

// Worst returns the worse of two statuses. A Repo's status is the worst of
// its files'.
func Worst(a, b Status) Status {
	if rank(b) > rank(a) {
		return b
	}
	return a
}

// ErrNotScanned means no results exist yet for the Commit.
var ErrNotScanned = errors.New("commit is not scanned yet")

// Finding is one import or integrity problem.
type Finding struct {
	// Module and Name are the imported global, such as `os` and `system`.
	Module string `json:"module,omitempty"`
	Name   string `json:"name,omitempty"`
	// Opcode is the pickle opcode that imported it (GLOBAL, STACK_GLOBAL, INST).
	Opcode string `json:"opcode,omitempty"`
	Status Status `json:"status"`
	// Detail explains integrity findings.
	Detail string `json:"detail,omitempty"`
}

// FileResult is the result for one file.
type FileResult struct {
	Path     string    `json:"path"`
	Status   Status    `json:"status"`
	Findings []Finding `json:"findings,omitempty"`
	Error    string    `json:"error,omitempty"`
	// Partial is set when the scan could not read every part of the file
	// that a loader runs, such as a pickle compressed with xz or a joblib
	// array it cannot measure: the findings may be incomplete, so the Gate
	// fails closed for it.
	Partial bool `json:"partial,omitempty"`
}

// CommitResult is the result for a Commit.
type CommitResult struct {
	Status Status       `json:"status"`
	Files  []FileResult `json:"files"`
}

// Scanner scans files and Commits.
type Scanner interface {
	// ScanFile scans one file's content without executing it. path and
	// the content decide the file type.
	ScanFile(ctx context.Context, path string, r io.Reader) (FileResult, error)
	// ScanCommit scans every present file of a Commit that needs scanning,
	// stores the results and returns them.
	ScanCommit(ctx context.Context, repoUUID uuid.UUID, id model.RepoID, commit model.CommitSHA) (*CommitResult, error)
	// Results returns stored results, or ErrNotScanned.
	Results(ctx context.Context, repoUUID uuid.UUID, commit model.CommitSHA) (*CommitResult, error)
}
