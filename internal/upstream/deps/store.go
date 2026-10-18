package deps

import (
	"context"

	"github.com/google/uuid"
	"github.com/uptrace/bun"

	"github.com/hangar-hub/hangar/internal/db"
	"github.com/hangar-hub/hangar/internal/model"
)

// Store keeps Dependencies in the dependencies table.
type Store struct{ db *db.DB }

// NewStore returns a Store.
func NewStore(d *db.DB) *Store { return &Store{db: d} }

// Replace sets the Dependencies of a Repo at a Commit, replacing earlier
// rows for that Commit.
func (s *Store) Replace(ctx context.Context, repoUUID uuid.UUID, commit model.CommitSHA, ds []Dependency) error {
	return s.db.InTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.NewDelete().Model((*db.Dependency)(nil)).Where("repo_id = ? AND commit_sha = ?", repoUUID, string(commit)).Exec(ctx); err != nil {
			return err
		}
		if len(ds) == 0 {
			return nil
		}
		rows := make([]db.Dependency, 0, len(ds))
		now := db.Now()
		for _, d := range ds {
			rows = append(rows, db.Dependency{ID: db.NewID(), RepoID: repoUUID, Commit: string(commit), TargetType: string(d.Target.Type),
				TargetRepo: d.Target.String(), TargetRevision: d.Revision, Kind: string(d.Kind), Required: d.Required,
				SourcePath: d.SourcePath, SourceKey: d.SourceKey, Timestamps: db.Timestamps{CreatedAt: now, UpdatedAt: now}})
		}
		_, err := tx.NewInsert().Model(&rows).Exec(ctx)
		return err
	})
}

// Needs returns the Dependencies of a Repo at a Commit.
func (s *Store) Needs(ctx context.Context, repoUUID uuid.UUID, commit model.CommitSHA) ([]Dependency, error) {
	var rows []db.Dependency
	if err := s.db.NewSelect().Model(&rows).Where("repo_id = ? AND commit_sha = ?", repoUUID, string(commit)).OrderExpr("source_path, source_key, target_repo").Scan(ctx); err != nil {
		return nil, err
	}
	out := make([]Dependency, 0, len(rows))
	for _, r := range rows {
		id, err := model.ParseRepoID(model.RepoType(r.TargetType), r.TargetRepo)
		if err != nil {
			continue
		}
		out = append(out, Dependency{Target: id, Revision: r.TargetRevision, Kind: Kind(r.Kind), Required: r.Required, SourcePath: r.SourcePath, SourceKey: r.SourceKey})
	}
	return out, nil
}

// Dependent is a Repo Commit that needs a target.
type Dependent struct {
	RepoUUID uuid.UUID
	Commit   model.CommitSHA
	Dependency
}

// NeededBy returns every row, at any Commit, whose target is id.
// Callers keep the rows of each Repo's current head.
func (s *Store) NeededBy(ctx context.Context, id model.RepoID) ([]Dependent, error) {
	var rows []db.Dependency
	if err := s.db.NewSelect().Model(&rows).Where("target_type = ? AND lower(target_repo) = lower(?)", string(id.Type), id.String()).
		OrderExpr("repo_id, commit_sha").Scan(ctx); err != nil {
		return nil, err
	}
	out := make([]Dependent, 0, len(rows))
	for _, r := range rows {
		out = append(out, Dependent{RepoUUID: r.RepoID, Commit: model.CommitSHA(r.Commit),
			Dependency: Dependency{Target: id, Revision: r.TargetRevision, Kind: Kind(r.Kind), Required: r.Required, SourcePath: r.SourcePath, SourceKey: r.SourceKey}})
	}
	return out, nil
}
