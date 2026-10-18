package repo

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/jamesagarside/hangar/internal/gitstore"
	"github.com/jamesagarside/hangar/internal/model"
)

// errPackTooLarge is returned by limitedReader past the limit.
var errPackTooLarge = errors.New("pack is too large")

type limitedReader struct {
	r     io.Reader
	limit int64
	n     int64
}

func (l *limitedReader) Read(p []byte) (int, error) {
	n, err := l.r.Read(p)
	l.n += int64(n)
	if l.n > l.limit {
		return n, errPackTooLarge
	}
	return n, err
}

// ApplyPack implements Service (04-git.md "Push"). It indexes the pack
// into a staging repository (completing thin packs from the Repo's
// objects), checks the push policy, stores the pack, and then moves every
// ref in one compare-and-swap transaction: all succeed or none do.
func (s *Store) ApplyPack(ctx context.Context, p ApplyPackParams) error {
	row, err := s.lookup(ctx, s.db, p.Repo)
	if err != nil {
		return err
	}
	r := toRepo(row)
	if p.Source == SourcePush && r.Mirror {
		return ErrReadOnly
	}
	if len(p.Updates) == 0 {
		return nil
	}
	for _, u := range p.Updates {
		if err := model.ValidateRefName(u.Name); err != nil {
			return &PolicyError{Ref: u.Name, Message: "invalid ref name"}
		}
		if p.Source == SourcePush {
			if !strings.HasPrefix(u.Name, model.RefHeads) && !strings.HasPrefix(u.Name, model.RefTags) {
				return &PolicyError{Ref: u.Name, Message: "only refs/heads/* and refs/tags/* can be pushed; pull request refs are written through pull requests"}
			}
			if u.Name == model.BranchRef(r.DefaultBranch) && u.New.IsZero() {
				return &PolicyError{Ref: u.Name, Message: "the default branch cannot be deleted"}
			}
		}
	}
	st, err := s.git.NewStaging(ctx, row.ID)
	if err != nil {
		return err
	}
	defer st.Close()

	var (
		pack    string
		objects []string
	)
	if p.Pack != nil {
		br := bufio.NewReader(p.Pack)
		if _, err := br.Peek(1); err == nil {
			lr := &limitedReader{r: br, limit: s.opts.MaxPackSize}
			// git index-pack rebuilds every delta in memory, so the pack
			// is checked against the limits before it gets the trailer.
			// Mirrors keep the Upstream's files as they are: only their
			// deltas are bounded.
			limits := gitstore.PackLimitsFor(s.opts.MaxBlobSize, s.opts.MaxPackSize)
			if p.Source != SourcePush {
				limits.MaxObjectSize, limits.MaxObjects = 0, 0
			}
			lp := gitstore.LimitPack(lr, limits)
			pack, err = st.IndexPack(ctx, lp, p.Source == SourcePush)
			_ = lp.Close()
			if err != nil {
				var limit *gitstore.PackLimitError
				checked := lp.Err()
				switch {
				case errors.As(checked, &limit):
					return &PolicyError{Message: limit.Message}
				case errors.Is(err, errPackTooLarge) || lr.n > lr.limit:
					return &PolicyError{Message: fmt.Sprintf("the push is larger than the %d byte limit; push fewer commits at a time or store large files with git lfs", s.opts.MaxPackSize)}
				case errors.Is(checked, gitstore.ErrMalformedPack):
					return fmt.Errorf("%w: unpack failed: %s", ErrInvalid, checked.Error())
				}
				return fmt.Errorf("%w: unpack failed: %s", ErrInvalid, gitMessage(err))
			}
			objects, err = st.PackObjects(ctx, pack)
			if err != nil {
				return err
			}
		}
	}
	// Every new target must exist, in the new pack or the Repo.
	var news []string
	for _, u := range p.Updates {
		if !u.New.IsZero() {
			news = append(news, string(u.New))
		}
	}
	if len(news) > 0 {
		info, err := s.git.Git().BatchCheck(ctx, st.Dir, news)
		if err != nil {
			return err
		}
		for i, inf := range info {
			if inf.Missing {
				return &PolicyError{Ref: p.Updates[i].Name, Message: "the pushed object " + inf.OID + " is missing"}
			}
		}
	}
	if p.Source == SourcePush && !p.AllowForce && !s.opts.AllowForcePush {
		for _, u := range p.Updates {
			if u.Name != model.BranchRef(r.DefaultBranch) || u.Old.IsZero() || u.New.IsZero() {
				continue
			}
			if _, err := st.Run(ctx, nil, nil, "merge-base", "--is-ancestor", string(u.Old), string(u.New)); err != nil {
				return &PolicyError{Ref: u.Name, Message: "force-pushing the default branch is not allowed"}
			}
		}
	}
	lfsRefs, added, commits, err := s.checkPackObjects(ctx, st, row, p.Source, p.Actor, objects)
	if err != nil {
		return err
	}
	if err := s.checkTrees(ctx, st.Dir, commits, true); err != nil {
		return err
	}
	if s.opts.Quota != nil && p.Source == SourcePush && added > 0 {
		if err := s.opts.Quota.CheckQuota(ctx, row.NamespaceID, added); err != nil {
			return &PolicyError{Message: err.Error()}
		}
	}
	gus := make([]gitstore.RefUpdate, 0, len(p.Updates))
	for _, u := range p.Updates {
		gus = append(gus, gitstore.RefUpdate{Name: u.Name, Old: u.Old, New: u.New})
	}
	return s.apply(ctx, applyParams{row: row, updates: gus, st: st, pack: pack, lfs: lfsRefs, actor: p.Actor})
}

// checkPackObjects applies the blob policy to the objects of a new pack:
// pushes may not add non-LFS files over git.max_blob_size, and every LFS
// pointer they add must name a stored, verified object that the pusher
// uploaded or can already read (Referable); the refusal is the same for an
// object that is stored but not referable as for one that does not exist.
// Mirrors record the pointers they bring as present or not. It also
// returns the Commits in the pack.
func (s *Store) checkPackObjects(ctx context.Context, st *gitstore.Staging, row *repoRow, source PackSource, actor model.Principal, objects []string) ([]LFSRef, int64, []string, error) {
	if len(objects) == 0 {
		return nil, 0, nil, nil
	}
	g := s.git.Git()
	info, err := g.BatchCheck(ctx, st.Dir, objects)
	if err != nil {
		return nil, 0, nil, err
	}
	var (
		small   []string
		commits []string
		added   int64
	)
	for _, i := range info {
		if i.Type == "commit" {
			commits = append(commits, i.OID)
		}
		if i.Type != "blob" {
			continue
		}
		if i.Size <= model.MaxLFSPointerSize {
			small = append(small, i.OID)
			continue
		}
		added += i.Size
		if source == SourcePush && i.Size > s.opts.MaxBlobSize {
			return nil, 0, nil, &PolicyError{Message: fmt.Sprintf("a file in the push (blob %s) is %d bytes, larger than the %d byte limit for files outside LFS. Track large files with git lfs track \"*.ext\", commit .gitattributes and push again", i.OID, i.Size, s.opts.MaxBlobSize)}
		}
	}
	var ptrs []model.LFSPointer
	err = g.BatchRead(ctx, st.Dir, small, func(_ gitstore.ObjectInfo, content []byte) error {
		if p, err := model.ParseLFSPointer(content); err == nil {
			ptrs = append(ptrs, p)
		} else {
			added += int64(len(content))
		}
		return nil
	})
	if err != nil {
		return nil, 0, nil, err
	}
	if len(ptrs) == 0 {
		return nil, added, commits, nil
	}
	oids := make([]string, 0, len(ptrs))
	for _, p := range ptrs {
		oids = append(oids, p.OID)
	}
	stored, err := s.lfs.StatMany(ctx, oids)
	if err != nil {
		return nil, 0, nil, err
	}
	var referable map[string]bool
	if source == SourcePush {
		if referable, err = s.Referable(ctx, actor, toRepo(row), oids); err != nil {
			return nil, 0, nil, err
		}
	}
	refs := make([]LFSRef, 0, len(ptrs))
	for _, p := range ptrs {
		obj, ok := stored[p.OID]
		if source == SourcePush && (!ok || !referable[p.OID]) {
			// The pusher's own upload: one written by a presigned PUT may
			// not be verified yet. A stored object the pusher may not
			// reference counts as not uploaded.
			v, err := s.verifyLFS(ctx, p.OID, p.Size, actor.UserID)
			if errors.Is(err, errVerifyWait) {
				return nil, 0, nil, &PolicyError{Message: err.Error()}
			}
			obj, ok = v, err == nil
		}
		present := ok && obj.Size == p.Size
		if source == SourcePush && !present {
			return nil, 0, nil, &PolicyError{Message: fmt.Sprintf("the push refers to LFS object %s (%d bytes), which has not been uploaded; run git lfs push --all and push again", p.OID, p.Size)}
		}
		refs = append(refs, LFSRef{OID: p.OID, Size: p.Size, Present: present})
		if present {
			added += p.Size
		}
	}
	return refs, added, commits, nil
}
