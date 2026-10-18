package gitstore

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/google/uuid"
	"github.com/uptrace/bun"

	"github.com/hangar-hub/hangar/internal/db"
	"github.com/hangar-hub/hangar/internal/model"
)

// ErrRefConflict means a compare-and-swap found a ref at a different value
// than expected, so nothing was changed.
var ErrRefConflict = errors.New("reference has changed concurrently, fetch first")

// ErrInvalidUpdate means a ref update names an invalid ref or has neither an
// old nor a new value.
var ErrInvalidUpdate = errors.New("invalid ref update")

// RefUpdate is one compare-and-swap: Old zero creates the ref, New zero
// deletes it, and both set updates it.
type RefUpdate struct {
	Name string
	Old  model.CommitSHA
	New  model.CommitSHA
}

// UpdateRefs applies every update in tx with compare-and-swap on
// refs.target (docs/spec/04-git.md "Ref updates"), in name order so
// concurrent multi-ref updates cannot deadlock. If any update finds the ref
// at a different value it returns ErrRefConflict naming the ref; the caller
// must roll the transaction back, so either every update applies or none.
func UpdateRefs(ctx context.Context, tx bun.IDB, repoID uuid.UUID, updates []RefUpdate) error {
	sorted := append([]RefUpdate(nil), updates...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	seen := map[string]bool{}
	now := db.Now()
	for _, u := range sorted {
		if err := model.ValidateRefName(u.Name); err != nil {
			return fmt.Errorf("%w: %w", ErrInvalidUpdate, err)
		}
		if seen[u.Name] {
			return fmt.Errorf("%w: %s is updated twice", ErrInvalidUpdate, u.Name)
		}
		seen[u.Name] = true
		for _, sha := range []model.CommitSHA{u.Old, u.New} {
			if !sha.IsZero() && !model.IsFullSHA(string(sha)) {
				return fmt.Errorf("%w: %q is not a full SHA", ErrInvalidUpdate, sha)
			}
		}
		var (
			n   int64
			err error
		)
		switch {
		case u.Old.IsZero() && u.New.IsZero():
			return fmt.Errorf("%w: %s has neither an old nor a new value", ErrInvalidUpdate, u.Name)
		case u.Old.IsZero():
			ref := &db.Ref{RepoID: repoID, Name: u.Name, Target: string(u.New), Timestamps: db.Timestamps{CreatedAt: now, UpdatedAt: now}}
			n, err = rows(tx.NewInsert().Model(ref).On("CONFLICT DO NOTHING").Exec(ctx))
		case u.New.IsZero():
			n, err = rows(tx.NewDelete().Model((*db.Ref)(nil)).
				Where("repo_id = ?", repoID).Where("name = ?", u.Name).Where("target = ?", string(u.Old)).Exec(ctx))
		default:
			n, err = rows(tx.NewUpdate().Model((*db.Ref)(nil)).
				Set("target = ?", string(u.New)).Set("updated_at = ?", now).
				Where("repo_id = ?", repoID).Where("name = ?", u.Name).Where("target = ?", string(u.Old)).Exec(ctx))
		}
		if err != nil {
			return fmt.Errorf("gitstore: update %s: %w", u.Name, err)
		}
		if n != 1 {
			return fmt.Errorf("%w: %s", ErrRefConflict, u.Name)
		}
	}
	return nil
}

func rows(res interface{ RowsAffected() (int64, error) }, err error) (int64, error) {
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// Ref is a ref name and the object it points at, without peeling.
type Ref struct {
	Name   string
	Target model.CommitSHA
}

// Refs returns a snapshot of a Repo's refs in name order, from one query.
func Refs(ctx context.Context, idb bun.IDB, repoID uuid.UUID) ([]Ref, error) {
	var rowsOut []db.Ref
	if err := idb.NewSelect().Model(&rowsOut).Where("repo_id = ?", repoID).Order("name").Scan(ctx); err != nil {
		return nil, fmt.Errorf("gitstore: list refs: %w", err)
	}
	out := make([]Ref, 0, len(rowsOut))
	for _, r := range rowsOut {
		out = append(out, Ref{Name: r.Name, Target: model.CommitSHA(r.Target)})
	}
	return out, nil
}

// LookupRef returns one ref's target, or "" and false.
func LookupRef(ctx context.Context, idb bun.IDB, repoID uuid.UUID, name string) (model.CommitSHA, bool, error) {
	var target string
	err := idb.NewSelect().Model((*db.Ref)(nil)).Column("target").
		Where("repo_id = ?", repoID).Where("name = ?", name).Scan(ctx, &target)
	if db.IsNotFound(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("gitstore: read ref %s: %w", name, err)
	}
	return model.CommitSHA(target), true, nil
}
