package scan

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/google/uuid"

	"github.com/jamesagarside/hangar/internal/db"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/repo"
)

// BlockReason says why the Gate refused a download.
type BlockReason string

// Reasons.
const (
	// BlockUnsafe: the scan found imports that run code.
	BlockUnsafe BlockReason = "unsafe"
	// BlockNotScanned: the file can run code when loaded and has no scan
	// result: the scan is pending, or its Job failed.
	BlockNotScanned BlockReason = "not_scanned"
)

// BlockedError is a download the Gate refused. Error is a message for
// users that says why; protocols that take accept_risk=1 add how to go
// ahead.
type BlockedError struct {
	Path   string
	Reason BlockReason
}

func (e *BlockedError) Error() string {
	name := e.Path
	if name == "" {
		name = "This file"
	}
	switch e.Reason {
	case BlockNotScanned:
		return name + " has not been scanned by Hangar's security scan yet, and loading it can run code. " +
			"An admin blocks unsafe downloads on this Instance, so it downloads once the scan finds it safe."
	default:
		return name + " is marked unsafe by Hangar's security scan: loading it can run code. " +
			"An admin blocks unsafe downloads on this Instance."
	}
}

// IsBlocked reports whether err is a refusal from the Gate.
func IsBlocked(err error) bool {
	var be *BlockedError
	return errors.As(err, &be)
}

// Gate enforces the Instance setting scan.block_unsafe_downloads
// (07-indexing-and-scanning.md "Where results show") for every protocol
// that serves file bytes: the Hub API, git-lfs, the Ollama registry, zip
// exports and signed Pull-through links, so the setting has no way around
// it. Signed links are checked again when they are used. With the setting on it
// refuses files the scan marked unsafe, and fails closed for files that can
// run code when loaded (pickle, joblib and NumPy files) that have no result,
// because their scan is pending or its Job failed. A file the scan read but
// could not parse (status error) is not refused: the scanner records the
// imports it saw before the error, and many .bin files are not pickles.
// While a Commit's scan is pending, a path whose file is unchanged since
// the nearest scanned ancestor keeps that ancestor's result (Current).
// Downloads that name an object instead of a
// path (git-lfs, Ollama blobs) are refused when any scanned path of the
// Repo with that object is refused.
//
// git-upload-pack is not gated: a git pack holds whole Commits, and the
// Hub's default .gitattributes store every pickle format in LFS.
type Gate struct {
	// Block reads scan.block_unsafe_downloads. Nil never blocks.
	Block func(ctx context.Context) (bool, error)
	// Results returns a Commit's stored results, or ErrNotScanned.
	Results func(ctx context.Context, repoUUID uuid.UUID, commit model.CommitSHA) (*CommitResult, error)
	// Repos resolves paths to Entries and lists branch heads.
	Repos repo.Service
	// Flagged returns the Commits of a Repo with a file the scan marked
	// unsafe, so object downloads are checked against every scanned Commit,
	// not only branch heads. Optional.
	Flagged func(ctx context.Context, repoUUID uuid.UUID) ([]model.CommitSHA, error)
}

// NewGate returns the Gate over the Service's stored results.
func (s *Service) NewGate(block func(ctx context.Context) (bool, error)) *Gate {
	return &Gate{Block: block, Results: s.Results, Repos: s.o.Repos, Flagged: s.Flagged}
}

// runsCode reports whether loading a file of this path can run code, so
// the Gate fails closed for it.
func runsCode(p string) bool {
	switch KindOf(p) {
	case KindPickle, KindJoblib, KindNumpy:
		return true
	}
	return false
}

func (g *Gate) enabled(ctx context.Context) (bool, error) {
	if g == nil || g.Block == nil {
		return false, nil
	}
	return g.Block(ctx)
}

// verdict decides one file from its Commit's results (nil when the Commit
// is not scanned).
func (g *Gate) verdict(ctx context.Context, id model.RepoID, commit model.CommitSHA, res *CommitResult, p string) (*BlockedError, error) {
	if res != nil {
		for _, f := range res.Files {
			if f.Path != p {
				continue
			}
			if f.Status == StatusUnsafe {
				return &BlockedError{Path: p, Reason: BlockUnsafe}, nil
			}
			return nil, nil
		}
	}
	if !runsCode(p) {
		return nil, nil
	}
	if res != nil && strings.EqualFold(path.Ext(p), ".bin") && g.Repos != nil {
		// OpenVINO weights beside an .xml graph are raw numbers, which the
		// scanner skips (scanEntry).
		if _, err := g.Repos.Entry(ctx, id, commit, strings.TrimSuffix(p, path.Ext(p))+".xml"); err == nil {
			return nil, nil
		} else if !errors.Is(err, repo.ErrEntryNotFound) && !errors.Is(err, repo.ErrNotFound) {
			return nil, err
		}
	}
	// No result for a file that can run code: its Commit is not scanned
	// yet, or the file was not present when it was, as in a Partial mirror.
	return &BlockedError{Path: p, Reason: BlockNotScanned}, nil
}

func (g *Gate) results(ctx context.Context, repoUUID uuid.UUID, commit model.CommitSHA) (*CommitResult, error) {
	res, err := g.Results(ctx, repoUUID, commit)
	if errors.Is(err, ErrNotScanned) {
		return nil, nil
	}
	return res, err
}

// current returns a Commit's results for CheckFile and CheckTree: its
// stored results or, while its scan is pending, the results carried from
// the nearest scanned ancestor for unchanged files (Current). A file that
// changed has no result, so a file that can run code still fails closed.
// It is nil when nothing is stored for the Commit or an ancestor.
func (g *Gate) current(ctx context.Context, repoUUID uuid.UUID, id model.RepoID, commit model.CommitSHA, paths []string) (*CommitResult, error) {
	v, err := Current(ctx, g.Results, g.Repos, repoUUID, id, commit, paths)
	if err != nil {
		return nil, err
	}
	return v.result(), nil
}

// CheckFile returns a *BlockedError when the file at p of commit must not
// download, nil when it may, and other errors when the check failed.
func (g *Gate) CheckFile(ctx context.Context, repoUUID uuid.UUID, id model.RepoID, commit model.CommitSHA, p string) error {
	on, err := g.enabled(ctx)
	if err != nil || !on {
		return err
	}
	res, err := g.current(ctx, repoUUID, id, commit, []string{p})
	if err != nil {
		return err
	}
	blocked, err := g.verdict(ctx, id, commit, res, p)
	if err != nil || blocked == nil {
		return err
	}
	return blocked
}

// CheckTree checks every present file under dir of commit, for a zip
// export, and returns the first refusal.
func (g *Gate) CheckTree(ctx context.Context, repoUUID uuid.UUID, id model.RepoID, commit model.CommitSHA, dir string) error {
	on, err := g.enabled(ctx)
	if err != nil || !on {
		return err
	}
	res, err := g.current(ctx, repoUUID, id, commit, nil)
	if err != nil {
		return err
	}
	entries, err := g.Repos.Tree(ctx, id, commit, dir, repo.TreeOpts{Recursive: true})
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.Type != model.EntryFile || (e.LFS != nil && !e.Present) {
			continue
		}
		blocked, err := g.verdict(ctx, id, commit, res, e.Path)
		if err != nil {
			return err
		}
		if blocked != nil {
			return blocked
		}
	}
	return nil
}

// CheckObjects checks LFS objects of a Repo that a client asks for by OID,
// as git-lfs and the Ollama registry do. It returns a *BlockedError for
// each refused OID; OIDs missing from the map may download. An object is
// refused when a file with it is refused in a Commit with stored results
// (Flagged), or in a branch head that is not scanned yet.
func (g *Gate) CheckObjects(ctx context.Context, repoUUID uuid.UUID, id model.RepoID, oids []string) (map[string]error, error) {
	out := map[string]error{}
	on, err := g.enabled(ctx)
	if err != nil || !on || len(oids) == 0 {
		return out, err
	}
	want := make(map[string]bool, len(oids))
	for _, o := range oids {
		want[o] = true
	}
	commits := map[model.CommitSHA]bool{}
	if g.Flagged != nil {
		flagged, err := g.Flagged(ctx, repoUUID)
		if err != nil {
			return nil, err
		}
		for _, c := range flagged {
			commits[c] = true
		}
	}
	refs, err := g.Repos.Refs(ctx, id)
	if err != nil {
		return nil, err
	}
	for _, b := range refs.Branches {
		commits[b.Target] = true
	}
	for commit := range commits {
		res, err := g.results(ctx, repoUUID, commit)
		if err != nil {
			return nil, err
		}
		var paths []string
		if res != nil {
			for _, f := range res.Files {
				if f.Status == StatusUnsafe {
					paths = append(paths, f.Path)
				}
			}
		} else {
			// A branch head that is not scanned yet: check the files that
			// can run code.
			entries, err := g.Repos.Tree(ctx, id, commit, "", repo.TreeOpts{Recursive: true})
			if err != nil {
				return nil, err
			}
			for _, e := range entries {
				if e.LFS != nil && want[e.LFS.OID] && runsCode(e.Path) {
					if _, done := out[e.LFS.OID]; !done {
						out[e.LFS.OID] = &BlockedError{Path: e.Path, Reason: BlockNotScanned}
					}
				}
			}
		}
		for _, p := range paths {
			e, err := g.Repos.Entry(ctx, id, commit, p)
			if errors.Is(err, repo.ErrEntryNotFound) {
				continue
			}
			if err != nil {
				return nil, err
			}
			if e.LFS == nil || !want[e.LFS.OID] {
				continue
			}
			blocked, err := g.verdict(ctx, id, commit, res, p)
			if err != nil {
				return nil, err
			}
			if blocked != nil {
				out[e.LFS.OID] = blocked
			}
		}
	}
	return out, nil
}

// Flagged returns the Commits of a Repo with a file the scan marked
// unsafe, in any stored scanner version; the Gate reads each Commit's
// newest results.
func (s *Service) Flagged(ctx context.Context, repoUUID uuid.UUID) ([]model.CommitSHA, error) {
	var commits []string
	err := s.o.DB.NewSelect().Model((*db.ScanResult)(nil)).ColumnExpr("DISTINCT commit_sha").
		Where("repo_id = ?", repoUUID).Where("path <> ?", commitRow).
		Where("status = ?", string(StatusUnsafe)).Scan(ctx, &commits)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("scan results of %s: %w", repoUUID, err)
	}
	out := make([]model.CommitSHA, 0, len(commits))
	for _, c := range commits {
		out = append(out, model.CommitSHA(c))
	}
	return out, nil
}
