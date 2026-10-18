package repo

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/google/uuid"
	"github.com/uptrace/bun"

	"github.com/jamesagarside/hangar/internal/db"
	"github.com/jamesagarside/hangar/internal/model"
)

// maxReferenceCandidates bounds the other Repos Referable asks about for
// one call. Repos that hold an object publicly come first, so a common
// object is decided by the first few.
const maxReferenceCandidates = 200

// Referable implements Service. LFS objects are stored once per Instance
// (UP-6), but a Commit may add a pointer to an object only when p uploaded
// its bytes or can already read them, decided by James on 2026-10-18
// ("check at Commit"). An object is referable when:
//
//   - r already holds it (a copy or a move within the Repo);
//   - p sent its bytes, through any upload path (lfs_uploaders);
//   - p can read the files of another Repo that holds it (Options.CanReadFiles).
//
// Hangar itself (model.System) may reference any object. Everything else
// must be uploaded, which stores no second copy of bytes that are already
// stored. The answer is the same for an object that does not exist, so it
// says nothing about objects p cannot read.
func (s *Store) Referable(ctx context.Context, p model.Principal, r *Repo, oids []string) (map[string]bool, error) {
	out := make(map[string]bool, len(oids))
	var want []string
	for _, o := range oids {
		if model.IsSHA256(o) && !slices.Contains(want, o) {
			want = append(want, o)
		}
	}
	if len(want) == 0 {
		return out, nil
	}
	if p.Kind == model.PrincipalSystem {
		for _, o := range want {
			out[o] = true
		}
		return out, nil
	}
	rest := func() []string {
		var left []string
		for _, o := range want {
			if !out[o] {
				left = append(left, o)
			}
		}
		return left
	}
	if r != nil {
		for start := 0; start < len(want); start += 500 {
			var held []string
			if err := s.db.NewSelect().Model((*db.RepoLFS)(nil)).Column("oid").Where("repo_id = ?", r.UUID).
				Where("oid IN (?)", bun.List(want[start:min(start+500, len(want))])).Where("present").Scan(ctx, &held); err != nil && !db.IsNotFound(err) {
				return nil, fmt.Errorf("repo: read lfs objects of %s: %w", r.ID, err)
			}
			for _, o := range held {
				out[o] = true
			}
		}
	}
	if left := rest(); len(left) > 0 && p.UserID != uuid.Nil {
		up, err := s.lfs.Uploaded(ctx, p.UserID, left)
		if err != nil {
			return nil, err
		}
		for o, ok := range up {
			if ok {
				out[o] = true
			}
		}
	}
	left := rest()
	if len(left) == 0 || s.opts.CanReadFiles == nil {
		return out, nil
	}
	var rows []struct {
		RepoID uuid.UUID `bun:"repo_id"`
		OID    string    `bun:"oid"`
	}
	q := s.db.NewSelect().TableExpr("repo_lfs AS rl").ColumnExpr("rl.repo_id, rl.oid").
		Join("JOIN repos AS repo ON repo.id = rl.repo_id").
		Where("rl.oid IN (?)", bun.List(left)).Where("rl.present").Where("repo.deleted_at IS NULL").
		OrderExpr("CASE WHEN repo.visibility = ? THEN 0 WHEN repo.visibility = ? THEN 1 ELSE 2 END",
			string(model.VisibilityPublic), string(model.VisibilityInternal)).
		OrderExpr("rl.repo_id").Limit(maxReferenceCandidates)
	if r != nil {
		q = q.Where("rl.repo_id <> ?", r.UUID)
	}
	if err := q.Scan(ctx, &rows); err != nil && !db.IsNotFound(err) {
		return nil, fmt.Errorf("repo: find Repos holding lfs objects: %w", err)
	}
	readable := map[uuid.UUID]bool{}
	for _, row := range rows {
		if out[row.OID] {
			continue
		}
		ok, seen := readable[row.RepoID]
		if !seen {
			holder, err := s.GetByUUID(ctx, row.RepoID)
			if err != nil {
				if errors.Is(err, ErrNotFound) {
					readable[row.RepoID] = false
					continue
				}
				return nil, err
			}
			if ok, err = s.opts.CanReadFiles(ctx, p, holder); err != nil {
				return nil, err
			}
			readable[row.RepoID] = ok
		}
		if ok {
			out[row.OID] = true
		}
	}
	return out, nil
}
