package scan

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/repo"
)

// carryDepth is how many first-parent ancestors Current looks through for
// stored results. A scan Job usually finishes within seconds of its
// Commit, so only a burst of Commits leaves more than a few unscanned.
const carryDepth = 16

// ResultsFunc returns a Commit's stored results, or ErrNotScanned, as
// Scanner.Results does.
type ResultsFunc func(ctx context.Context, repoUUID uuid.UUID, commit model.CommitSHA) (*CommitResult, error)

// View is a Commit's scan results as readers see them, including while
// the Commit's scan Job is still pending.
type View struct {
	// Scanned is true when the Commit has stored results.
	Scanned bool
	// From is the Commit whose stored results Files comes from: the Commit
	// itself when Scanned, the nearest scanned ancestor while the scan is
	// pending, and empty when neither is scanned.
	From model.CommitSHA
	// Status is the Commit's status when Scanned. While the scan is
	// pending it is the worst status of the carried files only.
	Status Status
	// Files has the result of each file the scan checked. While the scan
	// is pending it has only the files whose content and type are
	// unchanged since From, because their result cannot change.
	Files []FileResult
}

// File returns the result for path p, and whether there is one.
func (v *View) File(p string) (FileResult, bool) {
	for _, f := range v.Files {
		if f.Path == p {
			return f, true
		}
	}
	return FileResult{}, false
}

// Pending reports whether p is a file the scan reads whose result is not
// known yet: the Commit's scan has not finished and p changed since the
// nearest scanned ancestor.
func (v *View) Pending(p string) bool {
	if v.Scanned || KindOf(p) == KindNone {
		return false
	}
	_, ok := v.File(p)
	return !ok
}

// result returns the View as stored results, or nil when nothing is
// stored for the Commit or an ancestor.
func (v *View) result() *CommitResult {
	if v.From == "" {
		return nil
	}
	return &CommitResult{Status: v.Status, Files: v.Files}
}

// Current returns the scan results of a Commit as readers see them. When
// the Commit is scanned, the View holds its stored results. While its scan
// Job is pending, the View carries forward the results of the nearest
// scanned first-parent ancestor for each file whose blob is the same in
// both Commits, so an unchanged file keeps its status from the moment a
// new Commit lands. paths limits the files the View holds; nil means all.
// repos may be nil, and then nothing is carried.
func Current(ctx context.Context, results ResultsFunc, repos repo.Service, repoUUID uuid.UUID, id model.RepoID, commit model.CommitSHA, paths []string) (*View, error) {
	want := func(string) bool { return true }
	if paths != nil {
		set := make(map[string]bool, len(paths))
		for _, p := range paths {
			set[p] = true
		}
		want = func(p string) bool { return set[p] }
	}
	res, err := results(ctx, repoUUID, commit)
	if err == nil {
		v := &View{Scanned: true, From: commit, Status: res.Status}
		for _, f := range res.Files {
			if want(f.Path) {
				v.Files = append(v.Files, f)
			}
		}
		return v, nil
	}
	if !errors.Is(err, ErrNotScanned) {
		return nil, err
	}
	v := &View{Status: StatusSafe}
	if repos == nil {
		return v, nil
	}
	from, stored, err := scannedAncestor(ctx, results, repos, repoUUID, id, commit)
	if err != nil || stored == nil {
		return v, err
	}
	v.From = from
	var candidates []FileResult
	for _, f := range stored.Files {
		if want(f.Path) {
			candidates = append(candidates, f)
		}
	}
	if len(candidates) == 0 {
		return v, nil
	}
	carried, err := unchanged(ctx, repos, id, from, commit, candidates)
	if err != nil {
		return nil, err
	}
	for _, f := range carried {
		v.Files = append(v.Files, f)
		v.Status = Worst(v.Status, f.Status)
	}
	return v, nil
}

// scannedAncestor walks first parents from commit and returns the nearest
// ancestor with stored results, or a nil result when none is within
// carryDepth or the history ends (as in a shallow Mirror).
func scannedAncestor(ctx context.Context, results ResultsFunc, repos repo.Service, repoUUID uuid.UUID, id model.RepoID, commit model.CommitSHA) (model.CommitSHA, *CommitResult, error) {
	at := commit
	for range carryDepth {
		info, err := repos.GetCommit(ctx, id, at)
		if errors.Is(err, repo.ErrRevisionNotFound) || errors.Is(err, repo.ErrNotFound) {
			return "", nil, nil
		}
		if err != nil {
			return "", nil, err
		}
		if len(info.Parents) == 0 {
			return "", nil, nil
		}
		at = info.Parents[0]
		res, err := results(ctx, repoUUID, at)
		if errors.Is(err, ErrNotScanned) {
			continue
		}
		if err != nil {
			return "", nil, err
		}
		return at, res, nil
	}
	return "", nil, nil
}

// unchanged returns the results of from whose file has the same blob and
// the same presence at commit, so scanning commit would give the same
// result: the scan decides by the file's name and content alone.
func unchanged(ctx context.Context, repos repo.Service, id model.RepoID, from, commit model.CommitSHA, candidates []FileResult) ([]FileResult, error) {
	paths := make([]string, 0, len(candidates))
	for _, f := range candidates {
		paths = append(paths, f.Path)
	}
	index := func(c model.CommitSHA) (map[string]model.Entry, error) {
		entries, err := repos.Entries(ctx, id, c, paths, false)
		if err != nil {
			return nil, err
		}
		out := make(map[string]model.Entry, len(entries))
		for _, e := range entries {
			out[e.Path] = e
		}
		return out, nil
	}
	before, err := index(from)
	if err != nil {
		return nil, err
	}
	now, err := index(commit)
	if err != nil {
		return nil, err
	}
	var out []FileResult
	for _, f := range candidates {
		b, okB := before[f.Path]
		n, okN := now[f.Path]
		if !okB || !okN || n.Type != model.EntryFile || b.OID != n.OID || b.Present != n.Present {
			continue
		}
		out = append(out, f)
	}
	return out, nil
}
