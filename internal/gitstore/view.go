package gitstore

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"

	"github.com/hangar-hub/hangar/internal/model"
)

// View is a temporary bare repository for one request: HEAD, a minimal
// config, alternates pointing at the Repo's cache objects, and packed-refs
// written from one snapshot of the refs table. `git upload-pack` serves it
// as is. Close removes it.
type View struct {
	Dir  string
	Refs []Ref
	objs *Objects
}

// Close removes the view and releases the cache.
func (v *View) Close() error {
	if v == nil {
		return nil
	}
	err := os.RemoveAll(v.Dir)
	v.objs.Release()
	return err
}

// OpenView materialises a view of a Repo (docs/spec/04-git.md "Reading a
// Repo on a replica").
func (s *Store) OpenView(ctx context.Context, repoID uuid.UUID, headRef string) (*View, error) {
	objs, err := s.Acquire(ctx, repoID)
	if err != nil {
		return nil, err
	}
	refs, err := Refs(ctx, s.db, repoID)
	if err != nil {
		objs.Release()
		return nil, err
	}
	dir, err := s.tempDir("views")
	if err != nil {
		objs.Release()
		return nil, err
	}
	v := &View{Dir: dir, Refs: refs, objs: objs}
	if err := writeView(dir, objs.Dir, headRef, refs); err != nil {
		_ = v.Close()
		return nil, fmt.Errorf("gitstore: build view: %w", err)
	}
	return v, nil
}

func writeView(dir, objectsDir, headRef string, refs []Ref) error {
	if headRef == "" || !cleanName(headRef) {
		headRef = model.DefaultHeadRef
	}
	if err := initBare(dir, headRef); err != nil {
		return err
	}
	if err := alternatesFor(dir, objectsDir); err != nil {
		return err
	}
	var b strings.Builder
	b.WriteString("# pack-refs with: sorted \n")
	for _, r := range refs {
		if !cleanName(r.Name) {
			continue
		}
		b.WriteString(string(r.Target) + " " + r.Name + "\n")
	}
	return os.WriteFile(filepath.Join(dir, "packed-refs"), []byte(b.String()), 0o600)
}

// Staging is a temporary bare repository whose alternates point at a Repo's
// cache objects. Server-side Commits write their new objects into it, and
// received packs are indexed in it, so only new objects end up in the pack
// that is stored. Close removes it.
type Staging struct {
	Dir     string
	Objects *Objects
	store   *Store
	repoID  uuid.UUID
}

// NewStaging creates a staging repository for a Repo.
func (s *Store) NewStaging(ctx context.Context, repoID uuid.UUID) (*Staging, error) {
	objs, err := s.Acquire(ctx, repoID)
	if err != nil {
		return nil, err
	}
	dir, err := s.tempDir("staging")
	if err != nil {
		objs.Release()
		return nil, err
	}
	st := &Staging{Dir: dir, Objects: objs, store: s, repoID: repoID}
	if err := initBare(dir, model.DefaultHeadRef); err != nil {
		_ = st.Close()
		return nil, err
	}
	if err := alternatesFor(dir, objs.Dir); err != nil {
		_ = st.Close()
		return nil, err
	}
	return st, nil
}

// Close removes the staging repository.
func (st *Staging) Close() error {
	if st == nil {
		return nil
	}
	err := os.RemoveAll(st.Dir)
	st.Objects.Release()
	return err
}

// Run runs git in the staging repository.
func (st *Staging) Run(ctx context.Context, stdin io.Reader, env []string, args ...string) ([]byte, error) {
	return st.store.git.Run(ctx, st.Dir, stdin, env, args...)
}

// PackDir is where packs written in the staging repository land.
func (st *Staging) PackDir() string { return filepath.Join(st.Dir, "objects", "pack") }
