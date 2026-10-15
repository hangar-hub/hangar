package scan

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"
	"sync"

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
// that serves file bytes: the Hub API, git-lfs, Pull-through links, the
// Ollama registry and zip exports, so the setting has no way around it.
// With the setting on it refuses files the scan marked unsafe, and fails
// closed for files that can run code when loaded (pickle, joblib and NumPy
// files) that have no result, because their scan is pending or its Job
// failed. A file the scan read but could not parse (status error) is not
// refused: the scanner records the imports it saw before the error, and
// many .bin files are not pickles.
//
// A Commit with no result for a file, such as an intermediate Commit of a
// push, takes the result of the same file (path and git object) in a
// Commit the scan has read.
//
// Downloads that name an object instead of a path (git-lfs, Pull-through
// links, Ollama blobs) are decided from the files that hold the object: in
// the tip of every ref and in every Commit with a file marked unsafe, or,
// when none of those holds it, in the Commits that added it. The object is
// refused when any of those files is refused, and when no Commit reachable
// from a ref places it, because then nothing says whether loading it can
// run code.
//
// git-upload-pack is not gated: a git pack holds whole Commits, and the
// Hub's default .gitattributes store every pickle format in LFS.
type Gate struct {
	// Block reads scan.block_unsafe_downloads. Nil never blocks.
	Block func(ctx context.Context) (bool, error)
	// Results returns a Commit's stored results, or ErrNotScanned.
	Results func(ctx context.Context, repoUUID uuid.UUID, commit model.CommitSHA) (*CommitResult, error)
	// Repos resolves paths to Entries, lists refs and reads history.
	Repos repo.Service
	// Flagged returns the Commits of a Repo with a file the scan marked
	// unsafe, so object downloads are checked against every scanned Commit,
	// not only ref tips. Optional.
	Flagged func(ctx context.Context, repoUUID uuid.UUID) ([]model.CommitSHA, error)
	// Scanned returns the stored results for a path in the Commits of a
	// Repo, so a Commit without one takes the result of the same file in
	// another Commit. Optional.
	Scanned func(ctx context.Context, repoUUID uuid.UUID, path string) ([]ScannedFile, error)

	mu sync.Mutex
	// history caches, per Repo, where its history adds LFS files.
	history map[uuid.UUID]*historyIndex
}

// Limits on the work one check does. Objects the history walk cannot place
// within them are refused, so the limits never let a file through.
const (
	// maxHistoryCommits bounds the Commits read to place objects that no
	// ref tip holds.
	maxHistoryCommits = 20000
	// maxHistoryPlaces bounds the LFS files one Repo's history index holds.
	maxHistoryPlaces = 50000
	// maxCachedHistories bounds how many Repos' history a Gate keeps.
	maxCachedHistories = 16
	// maxSameFileCommits bounds the scanned Commits searched for the same
	// file.
	maxSameFileCommits = 32
)

// NewGate returns the Gate over the Service's stored results.
func (s *Service) NewGate(block func(ctx context.Context) (bool, error)) *Gate {
	return &Gate{Block: block, Results: s.Results, Repos: s.o.Repos, Flagged: s.Flagged, Scanned: s.Scanned}
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

// isMissing reports whether err says a Commit or path does not exist.
func isMissing(err error) bool {
	return errors.Is(err, repo.ErrEntryNotFound) || errors.Is(err, repo.ErrRevisionNotFound) || errors.Is(err, repo.ErrNotFound)
}

// verdict decides one file from its Commit's results (nil when the Commit
// is not scanned). blobOID is the file's git object (for LFS files, the
// pointer's); when empty it is read only if needed.
func (g *Gate) verdict(ctx context.Context, repoUUID uuid.UUID, id model.RepoID, commit model.CommitSHA, res *CommitResult, p, blobOID string) (*BlockedError, error) {
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
	// No result for a file that can run code: its Commit is not scanned,
	// or the file was not present when it was, as in a Partial mirror. The
	// same file in a Commit the scan has read decides it.
	status, found, err := g.sameFile(ctx, repoUUID, id, commit, p, blobOID)
	if err != nil {
		return nil, err
	}
	if found {
		if status == StatusUnsafe {
			return &BlockedError{Path: p, Reason: BlockUnsafe}, nil
		}
		return nil, nil
	}
	return &BlockedError{Path: p, Reason: BlockNotScanned}, nil
}

// sameFile returns the stored status of the file at p with the same git
// object in another scanned Commit of the Repo, the worst one when there
// are several, and whether there is one.
func (g *Gate) sameFile(ctx context.Context, repoUUID uuid.UUID, id model.RepoID, commit model.CommitSHA, p, blobOID string) (Status, bool, error) {
	if g.Scanned == nil || g.Repos == nil {
		return "", false, nil
	}
	scanned, err := g.Scanned(ctx, repoUUID, p)
	if err != nil {
		return "", false, err
	}
	var status Status
	found := false
	for _, sf := range scanned {
		if sf.Commit == commit {
			continue
		}
		if blobOID == "" {
			e, err := g.Repos.Entry(ctx, id, commit, p)
			if isMissing(err) {
				return "", false, nil
			}
			if err != nil {
				return "", false, err
			}
			blobOID = e.OID
		}
		e, err := g.Repos.Entry(ctx, id, sf.Commit, p)
		if isMissing(err) {
			continue
		}
		if err != nil {
			return "", false, err
		}
		if e.OID != blobOID {
			continue
		}
		if !found {
			status = sf.Status
		}
		status, found = Worst(status, sf.Status), true
	}
	return status, found, nil
}

func (g *Gate) results(ctx context.Context, repoUUID uuid.UUID, commit model.CommitSHA) (*CommitResult, error) {
	res, err := g.Results(ctx, repoUUID, commit)
	if errors.Is(err, ErrNotScanned) {
		return nil, nil
	}
	return res, err
}

// CheckFile returns a *BlockedError when the file at p of commit must not
// download, nil when it may, and other errors when the check failed.
func (g *Gate) CheckFile(ctx context.Context, repoUUID uuid.UUID, id model.RepoID, commit model.CommitSHA, p string) error {
	on, err := g.enabled(ctx)
	if err != nil || !on {
		return err
	}
	res, err := g.results(ctx, repoUUID, commit)
	if err != nil {
		return err
	}
	blocked, err := g.verdict(ctx, repoUUID, id, commit, res, p, "")
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
	res, err := g.results(ctx, repoUUID, commit)
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
		blocked, err := g.verdict(ctx, repoUUID, id, commit, res, e.Path, e.OID)
		if err != nil {
			return err
		}
		if blocked != nil {
			return blocked
		}
	}
	return nil
}

// refTips returns the Commit of every ref, each once.
func refTips(refs *repo.Refs) []model.CommitSHA {
	var tips []model.CommitSHA
	seen := map[model.CommitSHA]bool{}
	for _, group := range [][]repo.Ref{refs.Branches, refs.Tags, refs.PullRequests, refs.Converts, refs.Other} {
		for _, r := range group {
			if !r.Target.IsZero() && !seen[r.Target] {
				seen[r.Target] = true
				tips = append(tips, r.Target)
			}
		}
	}
	return tips
}

// CheckObjects checks LFS objects of a Repo that a client asks for by OID,
// as git-lfs, Pull-through links and the Ollama registry do. It returns a
// *BlockedError for each refused OID; OIDs missing from the map may
// download. See Gate for how an object is decided.
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
	refs, err := g.Repos.Refs(ctx, id)
	if err != nil {
		return nil, err
	}
	tips := refTips(refs)
	commits := slices.Clone(tips)
	isTip := map[model.CommitSHA]bool{}
	for _, c := range tips {
		isTip[c] = true
	}
	if g.Flagged != nil {
		flagged, err := g.Flagged(ctx, repoUUID)
		if err != nil {
			return nil, err
		}
		for _, c := range flagged {
			if !isTip[c] {
				commits = append(commits, c)
			}
		}
	}
	placed := map[string]bool{}
	for _, commit := range commits {
		entries, err := g.Repos.Tree(ctx, id, commit, "", repo.TreeOpts{Recursive: true})
		if !isTip[commit] && errors.Is(err, repo.ErrRevisionNotFound) {
			// A Flagged Commit that git no longer holds places nothing.
			continue
		}
		if err != nil {
			return nil, err
		}
		var res *CommitResult
		loaded := false
		for _, e := range entries {
			if e.Type != model.EntryFile || e.LFS == nil || !want[e.LFS.OID] {
				continue
			}
			oid := e.LFS.OID
			placed[oid] = true
			if out[oid] != nil {
				continue
			}
			if !loaded {
				if res, err = g.results(ctx, repoUUID, commit); err != nil {
					return nil, err
				}
				loaded = true
			}
			blocked, err := g.verdict(ctx, repoUUID, id, commit, res, e.Path, e.OID)
			if err != nil {
				return nil, err
			}
			if blocked != nil {
				out[oid] = blocked
			}
		}
	}
	var rest []string
	for _, o := range oids {
		if !placed[o] {
			placed[o] = true
			rest = append(rest, o)
		}
	}
	if len(rest) > 0 {
		if err := g.checkHistory(ctx, repoUUID, id, tips, rest, out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// filePlace is a Commit that added or changed an LFS file, and its path.
type filePlace struct {
	commit model.CommitSHA
	path   string
}

// historyIndex lists, by the git object of the LFS pointer, where the
// Commits reachable from a set of ref tips add LFS files. History reachable
// from fixed tips never changes, so the index is reused until a ref moves.
type historyIndex struct {
	key    string
	places map[string][]filePlace
}

// checkHistory decides objects that no ref tip or Flagged Commit holds,
// such as an object that only an intermediate Commit of a push or a file
// later deleted holds, from the Commits that added them.
func (g *Gate) checkHistory(ctx context.Context, repoUUID uuid.UUID, id model.RepoID, tips []model.CommitSHA, oids []string, out map[string]error) error {
	lfsRefs, err := g.Repos.LFSRefs(ctx, id, oids)
	if err != nil {
		return err
	}
	h, err := g.historyOf(ctx, id, repoUUID, tips)
	if err != nil {
		return err
	}
	results := map[model.CommitSHA]*CommitResult{}
	for _, oid := range oids {
		ref, ok := lfsRefs[oid]
		var pointer string
		var places []filePlace
		if ok {
			pointer = model.LFSPointer{OID: oid, Size: ref.Size}.GitOID()
			places = h.places[pointer]
		}
		if len(places) == 0 {
			// No Commit says what the object is, so nothing says whether
			// loading it can run code.
			out[oid] = &BlockedError{Reason: BlockNotScanned}
			continue
		}
		for _, pl := range places {
			res, done := results[pl.commit]
			if !done {
				if res, err = g.results(ctx, repoUUID, pl.commit); err != nil {
					return err
				}
				results[pl.commit] = res
			}
			blocked, err := g.verdict(ctx, repoUUID, id, pl.commit, res, pl.path, pointer)
			if err != nil {
				return err
			}
			if blocked != nil {
				out[oid] = blocked
				break
			}
		}
	}
	return nil
}

// historyOf returns the history index of the Commits reachable from tips,
// from the cache when no ref has moved since it was built.
func (g *Gate) historyOf(ctx context.Context, id model.RepoID, repoUUID uuid.UUID, tips []model.CommitSHA) (*historyIndex, error) {
	sorted := make([]string, 0, len(tips))
	for _, t := range tips {
		sorted = append(sorted, string(t))
	}
	slices.Sort(sorted)
	sum := sha256.Sum256([]byte(strings.Join(sorted, "\n")))
	key := hex.EncodeToString(sum[:])
	g.mu.Lock()
	h := g.history[repoUUID]
	g.mu.Unlock()
	if h != nil && h.key == key {
		return h, nil
	}
	h = &historyIndex{key: key, places: map[string][]filePlace{}}
	visited := map[model.CommitSHA]bool{}
	n := 0
walk:
	for _, tip := range sorted {
		if visited[model.CommitSHA(tip)] {
			continue
		}
		log, err := g.Repos.Log(ctx, id, tip, repo.LogOpts{Limit: maxHistoryCommits})
		if err != nil {
			return nil, err
		}
		for _, c := range log {
			if visited[c.SHA] {
				continue
			}
			if len(visited) >= maxHistoryCommits {
				break walk
			}
			visited[c.SHA] = true
			changes, err := g.Repos.Diff(ctx, id, c.SHA)
			if err != nil {
				return nil, err
			}
			for _, ch := range changes {
				if ch.Status == repo.ChangeDeleted || !ch.LFS || ch.NewOID == "" {
					continue
				}
				if n >= maxHistoryPlaces {
					break walk
				}
				h.places[ch.NewOID] = append(h.places[ch.NewOID], filePlace{commit: c.SHA, path: ch.Path})
				n++
			}
		}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.history == nil {
		g.history = map[uuid.UUID]*historyIndex{}
	}
	if _, ok := g.history[repoUUID]; !ok && len(g.history) >= maxCachedHistories {
		for k := range g.history {
			delete(g.history, k)
			break
		}
	}
	g.history[repoUUID] = h
	return h, nil
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

// ScannedFile is the stored result of a file in one Commit.
type ScannedFile struct {
	Commit model.CommitSHA
	Status Status
}

// Scanned returns the stored results of the file at p in the Commits of a
// Repo, newest scanner version for each Commit, most recently scanned
// first.
func (s *Service) Scanned(ctx context.Context, repoUUID uuid.UUID, p string) ([]ScannedFile, error) {
	var rows []db.ScanResult
	err := s.o.DB.NewSelect().Model(&rows).Column("commit_sha", "status", "scanner_version").
		Where("repo_id = ?", repoUUID).Where("path = ?", p).
		OrderExpr("created_at DESC").Limit(4 * maxSameFileCommits).Scan(ctx)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("scan results of %s: %w", repoUUID, err)
	}
	at := map[string]int{}
	versions := map[string]int{}
	var out []ScannedFile
	for _, r := range rows {
		i, seen := at[r.Commit]
		switch {
		case !seen:
			if len(out) == maxSameFileCommits {
				continue
			}
			at[r.Commit] = len(out)
			out = append(out, ScannedFile{Commit: model.CommitSHA(r.Commit), Status: Status(r.Status)})
			versions[r.Commit] = r.ScannerVersion
		case r.ScannerVersion > versions[r.Commit]:
			out[i].Status = Status(r.Status)
			versions[r.Commit] = r.ScannerVersion
		}
	}
	return out, nil
}
