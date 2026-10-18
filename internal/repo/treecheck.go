package repo

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/hangar-hub/hangar/internal/gitstore"
	"github.com/hangar-hub/hangar/internal/model"
)

// DefaultMaxTreePaths is the most files and folders one Commit may hold:
// ten times the 100,000 files per Repo that 02-architecture.md targets.
// git stores a folder once however many times a tree names it, so without
// a bound a pack of a few hundred bytes can describe a Commit of billions
// of paths, which the tree cache, indexing and zip export would each list
// in full.
const DefaultMaxTreePaths = 1_000_000

// maxTreeObject bounds the commit and tree objects checkTrees reads into
// memory, as gitstore.PackLimitsFor bounds pushed objects.
const maxTreeObject = gitstore.MinPackObjectSize

// errTreeTooLarge is a Commit over MaxTreePaths or with a path over
// model.MaxPathLength, found while listing it.
var errTreeTooLarge = errors.New("the commit has too many files or too long a path to list")

// treeEntry is one entry of a git tree object.
type treeEntry struct {
	name string
	// sub is the object ID of a subtree, or "" for anything else (a file,
	// a symbolic link or a submodule).
	sub string
}

// treeStat is what a tree expands to.
type treeStat struct {
	// paths counts the files and folders under the tree, capped at one
	// more than the limit so it cannot overflow.
	paths int64
	// longest is the length of the longest path under the tree.
	longest int
}

// checkTrees checks the tree of every Commit in commits, which are in the
// git repository at dir: every name is one model.ValidPathComponent
// accepts (when names is set), no path is longer than model.MaxPathLength,
// and the Commit holds at most Options.MaxTreePaths files and folders.
// Each tree is read once, however often it is named, so the check costs
// what the distinct trees cost, not what they expand to.
func (s *Store) checkTrees(ctx context.Context, dir string, commits []string, names bool) error {
	if len(commits) == 0 {
		return nil
	}
	batch, err := s.git.Git().OpenBatch(ctx, dir)
	if err != nil {
		return err
	}
	defer func() { _ = batch.Close() }()
	roots := make(map[string]string, len(commits))
	for _, c := range commits {
		info, content, err := batch.Read(c, maxTreeObject)
		if err != nil {
			return err
		}
		tree, ok := commitTreeOID(content)
		if info.Missing || !ok {
			return fmt.Errorf("%w: commit %s has no tree", ErrInvalid, c)
		}
		roots[c] = tree
	}
	trees := map[string][]treeEntry{}
	queued := map[string]bool{}
	var level []string
	for _, t := range roots {
		if !queued[t] {
			queued[t] = true
			level = append(level, t)
		}
	}
	// Each level of folders adds at least two bytes ("a/") to a path.
	for depth := 0; len(level) > 0; depth++ {
		if depth > model.MaxPathLength/2 {
			return &PolicyError{Message: fmt.Sprintf("a Commit in the push has folders nested more than %d deep, so its paths are longer than the limit of %d bytes", model.MaxPathLength/2, model.MaxPathLength)}
		}
		var next []string
		for _, oid := range level {
			info, content, err := batch.Read(oid, maxTreeObject)
			if errors.Is(err, gitstore.ErrObjectTooLarge) {
				return &PolicyError{Message: fmt.Sprintf("tree %s is %d bytes, over the limit of %d for one folder", oid, info.Size, maxTreeObject)}
			}
			if err != nil {
				return err
			}
			if info.Missing || info.Type != "tree" {
				return fmt.Errorf("%w: tree %s is missing", ErrInvalid, oid)
			}
			ents, err := parseTreeObject(content, names)
			if err != nil {
				return err
			}
			trees[oid] = ents
			for _, e := range ents {
				if e.sub != "" && !queued[e.sub] {
					queued[e.sub] = true
					next = append(next, e.sub)
				}
			}
		}
		level = next
	}
	limit := s.opts.MaxTreePaths
	memo := map[string]treeStat{}
	var visit func(oid string) treeStat
	visit = func(oid string) treeStat {
		if st, ok := memo[oid]; ok {
			return st
		}
		var st treeStat
		for _, e := range trees[oid] {
			st.paths++
			n := len(e.name)
			if e.sub != "" {
				sub := visit(e.sub)
				st.paths += sub.paths
				if sub.longest > 0 {
					n += 1 + sub.longest
				}
			}
			st.paths = min(st.paths, limit+1)
			st.longest = max(st.longest, n)
		}
		memo[oid] = st
		return st
	}
	for commit, tree := range roots {
		st := visit(tree)
		if st.paths > limit {
			return &PolicyError{Message: fmt.Sprintf("Commit %s has more than %d files and folders, the most one Commit may hold", commit, limit)}
		}
		if st.longest > model.MaxPathLength {
			return &PolicyError{Message: fmt.Sprintf("Commit %s has a path of %d bytes, over the limit of %d", commit, st.longest, model.MaxPathLength)}
		}
	}
	return nil
}

// commitTreeOID reads the tree line of a commit object.
func commitTreeOID(content []byte) (string, bool) {
	line, _, _ := bytes.Cut(content, []byte{'\n'})
	oid, ok := strings.CutPrefix(string(line), "tree ")
	return oid, ok && model.IsFullSHA(oid)
}

// parseTreeObject parses a git tree object ("<mode> <name>\0<20-byte ID>"
// per entry) and, when names is set, refuses names a Repo cannot hold.
func parseTreeObject(b []byte, names bool) ([]treeEntry, error) {
	var out []treeEntry
	for len(b) > 0 {
		sp := bytes.IndexByte(b, ' ')
		nul := bytes.IndexByte(b, 0)
		if sp < 0 || nul < sp || len(b) < nul+21 {
			return nil, fmt.Errorf("%w: a tree in the push is corrupt", ErrInvalid)
		}
		mode, name := string(b[:sp]), string(b[sp+1:nul])
		oid := hex.EncodeToString(b[nul+1 : nul+21])
		b = b[nul+21:]
		if err := model.ValidPathComponent(name); names && err != nil {
			return nil, &PolicyError{Message: fmt.Sprintf("the push has a file or folder named %q, which a Repo cannot hold: it %s", name, err.Error())}
		}
		e := treeEntry{name: name}
		if mode == "40000" || mode == "040000" {
			e.sub = oid
		}
		out = append(out, e)
	}
	return out, nil
}
