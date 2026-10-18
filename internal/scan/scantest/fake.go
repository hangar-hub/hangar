// Package scantest provides an in-memory scan.Scanner. Tests set the result
// each path should get; ScanCommit reads the Commit's files from a
// repo.Service so the fake only reports files that exist.
package scantest

import (
	"context"
	"fmt"
	"io"
	"sync"

	"github.com/google/uuid"

	"github.com/hangar-hub/hangar/internal/model"
	"github.com/hangar-hub/hangar/internal/repo"
	"github.com/hangar-hub/hangar/internal/scan"
)

// Fake is a scan.Scanner whose verdicts come from SetResult. Files with no
// verdict are safe.
type Fake struct {
	Repos repo.Service

	mu       sync.Mutex
	verdicts map[string]scan.FileResult
	results  map[string]*scan.CommitResult
}

var _ scan.Scanner = (*Fake)(nil)

// NewFake returns a Fake that lists Commit files through repos.
func NewFake(repos repo.Service) *Fake {
	return &Fake{Repos: repos, verdicts: map[string]scan.FileResult{}, results: map[string]*scan.CommitResult{}}
}

// SetResult sets the verdict for a path.
func (f *Fake) SetResult(path string, status scan.Status, findings ...scan.Finding) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.verdicts[path] = scan.FileResult{Path: path, Status: status, Findings: findings}
}

// ScanFile implements scan.Scanner. It reads r to the end, as the real
// scanner streams whole files.
func (f *Fake) ScanFile(ctx context.Context, path string, r io.Reader) (scan.FileResult, error) {
	if _, err := io.Copy(io.Discard, r); err != nil {
		return scan.FileResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return scan.FileResult{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if v, ok := f.verdicts[path]; ok {
		return v, nil
	}
	return scan.FileResult{Path: path, Status: scan.StatusSafe}, nil
}

// ScanCommit implements scan.Scanner.
func (f *Fake) ScanCommit(ctx context.Context, repoUUID uuid.UUID, id model.RepoID, commit model.CommitSHA) (*scan.CommitResult, error) {
	entries, err := f.Repos.Tree(ctx, id, commit, "", repo.TreeOpts{Recursive: true})
	if err != nil {
		return nil, err
	}
	res := &scan.CommitResult{Status: scan.StatusSafe}
	for _, e := range entries {
		if e.Type != model.EntryFile || !e.Present {
			continue
		}
		rc, _, err := f.Repos.OpenFile(ctx, id, commit, e.Path, nil)
		if err != nil {
			return nil, err
		}
		fr, err := f.ScanFile(ctx, e.Path, rc)
		_ = rc.Close()
		if err != nil {
			return nil, err
		}
		res.Files = append(res.Files, fr)
		res.Status = scan.Worst(res.Status, fr.Status)
	}
	f.mu.Lock()
	f.results[repoUUID.String()+string(commit)] = res
	f.mu.Unlock()
	return res, nil
}

// Results implements scan.Scanner.
func (f *Fake) Results(_ context.Context, repoUUID uuid.UUID, commit model.CommitSHA) (*scan.CommitResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.results[repoUUID.String()+string(commit)]
	if !ok {
		return nil, fmt.Errorf("%w: %s", scan.ErrNotScanned, commit)
	}
	return r, nil
}
