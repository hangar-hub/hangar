package gitserver

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/storer"
	"github.com/go-git/go-git/v6/storage/memory"

	"github.com/hangar-hub/hangar/internal/model"
)

// viewRefs is a snapshot of a view's refs: HEAD's target and every ref's
// value, as git stores them (annotated tags are not peeled).
type viewRefs struct {
	head string
	refs map[string]string
}

// readViewRefs reads HEAD, packed-refs and loose refs from a bare
// repository directory. A view written by internal/repo holds only HEAD and
// packed-refs; a plain bare repository may have loose refs, which win over
// packed ones as in git.
func readViewRefs(dir string) (*viewRefs, error) {
	v := &viewRefs{refs: map[string]string{}}
	head, err := os.ReadFile(filepath.Join(dir, "HEAD")) //nolint:gosec // the view directory comes from repo.Service, not the client.
	if err != nil {
		return nil, fmt.Errorf("read HEAD: %w", err)
	}
	if target, ok := strings.CutPrefix(strings.TrimSpace(string(head)), "ref: "); ok {
		v.head = target
	}
	if err := readPackedRefs(filepath.Join(dir, "packed-refs"), v.refs); err != nil {
		return nil, err
	}
	root := filepath.Join(dir, "refs")
	//nolint:gosec // the view directory comes from repo.Service, not the client.
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.IsDir() || strings.HasSuffix(path, ".lock") {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		sha := strings.TrimSpace(string(b))
		if model.IsFullSHA(sha) {
			v.refs[filepath.ToSlash(rel)] = strings.ToLower(sha)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read loose refs: %w", err)
	}
	return v, nil
}

func readPackedRefs(path string, into map[string]string) error {
	f, err := os.Open(path) //nolint:gosec // the view directory comes from repo.Service, not the client.
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read packed-refs: %w", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || line[0] == '#' || line[0] == '^' {
			continue
		}
		sha, name, ok := strings.Cut(line, " ")
		if ok && model.IsFullSHA(sha) && strings.HasPrefix(name, "refs/") {
			into[name] = strings.ToLower(sha)
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("read packed-refs: %w", err)
	}
	return nil
}

// pushable reports whether a ref is advertised to and writable by git push.
// Pull request refs, Parquet exports and Hangar's own refs are written only
// by Hangar, so they are hidden from receive-pack, as git's
// receive.hideRefs would hide them.
func pushable(name string) bool {
	return strings.HasPrefix(name, model.RefHeads) || strings.HasPrefix(name, model.RefTags)
}

// pushRefs builds the in-memory ref store a receive-pack session works on:
// branches, tags and HEAD (when its branch exists).
func (v *viewRefs) pushRefs() memory.ReferenceStorage {
	refs := memory.ReferenceStorage{}
	for name, sha := range v.refs {
		if pushable(name) {
			n := plumbing.ReferenceName(name)
			refs[n] = plumbing.NewHashReference(n, plumbing.NewHash(sha))
		}
	}
	if v.head != "" {
		if _, ok := refs[plumbing.ReferenceName(v.head)]; ok {
			refs[plumbing.HEAD] = plumbing.NewSymbolicReference(plumbing.HEAD, plumbing.ReferenceName(v.head))
		}
	}
	return refs
}

// memoryRefs is the ReferenceStorer half of a push storer. go-git's
// receive-pack moves these refs only after the pre-receive hook has already
// applied the same updates to the database through repo.Service.ApplyPack,
// so nothing here is ever written to disk.
type memoryRefs struct {
	refs memory.ReferenceStorage
}

var _ storer.ReferenceStorer = (*memoryRefs)(nil)

func (m *memoryRefs) SetReference(r *plumbing.Reference) error { return m.refs.SetReference(r) }

func (m *memoryRefs) CheckAndSetReference(newRef, old *plumbing.Reference) error {
	return m.refs.CheckAndSetReference(newRef, old)
}

func (m *memoryRefs) Reference(n plumbing.ReferenceName) (*plumbing.Reference, error) {
	return m.refs.Reference(n)
}

func (m *memoryRefs) IterReferences() (storer.ReferenceIter, error) { return m.refs.IterReferences() }

func (m *memoryRefs) RemoveReference(n plumbing.ReferenceName) error {
	return m.refs.RemoveReference(n)
}

func (m *memoryRefs) CountLooseRefs() (int, error) { return m.refs.CountLooseRefs() }

func (m *memoryRefs) PackRefs() error { return m.refs.PackRefs() }

// reset sets each ref named by updates back to its old value, or removes it
// when the old value is zero. After ApplyPack has moved the refs in the
// database, this lets go-git's own compare-and-swap against the snapshot
// succeed, so report-status says ok for every ref.
func (m *memoryRefs) reset(names []plumbing.ReferenceName, olds []plumbing.Hash) {
	for i, n := range names {
		if olds[i].IsZero() {
			delete(m.refs, n)
			continue
		}
		m.refs[n] = plumbing.NewHashReference(n, olds[i])
	}
}
