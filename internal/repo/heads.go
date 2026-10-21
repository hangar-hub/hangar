package repo

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/uptrace/bun"

	"github.com/hangar-hub/hangar/internal/db"
	"github.com/hangar-hub/hangar/internal/model"
)

// HeadResolver is implemented by Repo services that resolve the default
// branch heads of many Repos at once, for pages of Repos.
type HeadResolver interface {
	// ResolveHeads returns what Resolve(ctx, r.ID, "") returns for each
	// Repo, leaving out Repos it cannot resolve.
	ResolveHeads(ctx context.Context, repos []*Repo) (map[uuid.UUID]model.CommitSHA, error)
}

var _ HeadResolver = (*Store)(nil)

// ResolveHeads implements HeadResolver with two queries, one for the refs
// and one for the Commits they point at. A Repo whose ref points at
// something other than a known Commit, such as an annotated tag, or whose
// default branch is a full SHA, is resolved on its own as Resolve does.
func (s *Store) ResolveHeads(ctx context.Context, repos []*Repo) (map[uuid.UUID]model.CommitSHA, error) {
	out := make(map[uuid.UUID]model.CommitSHA, len(repos))
	if len(repos) == 0 {
		return out, nil
	}
	// Resolve tries refs/heads/<branch> and then refs/tags/<branch>.
	candidates := map[uuid.UUID][]string{}
	var ids []uuid.UUID
	names := map[string]bool{}
	var alone []*Repo
	for _, r := range repos {
		rev := r.DefaultBranch
		if model.IsFullSHA(rev) {
			alone = append(alone, r)
			continue
		}
		cs := []string{model.RefHeads + rev, model.RefTags + rev}
		if strings.HasPrefix(rev, "refs/") {
			cs = []string{rev}
		}
		var valid []string
		for _, c := range cs {
			if model.ValidateRefName(c) == nil {
				valid = append(valid, c)
				names[c] = true
			}
		}
		if len(valid) > 0 {
			candidates[r.UUID] = valid
			ids = append(ids, r.UUID)
		}
	}
	targets := map[uuid.UUID]map[string]string{}
	if len(ids) > 0 {
		nameList := make([]string, 0, len(names))
		for n := range names {
			nameList = append(nameList, n)
		}
		var refs []db.Ref
		if err := s.db.NewSelect().Model(&refs).Column("repo_id", "name", "target").Where("repo_id IN (?)", bun.List(ids)).
			Where("name IN (?)", bun.List(nameList)).Scan(ctx); err != nil {
			return nil, fmt.Errorf("repo: resolve heads: %w", err)
		}
		for _, ref := range refs {
			if targets[ref.RepoID] == nil {
				targets[ref.RepoID] = map[string]string{}
			}
			targets[ref.RepoID][ref.Name] = ref.Target
		}
	}
	found := map[uuid.UUID]string{}
	var shas []string
	for id, cs := range candidates {
		for _, c := range cs {
			if t, ok := targets[id][c]; ok {
				found[id] = t
				shas = append(shas, t)
				break
			}
		}
	}
	if len(found) > 0 {
		var known []db.Commit
		foundIDs := make([]uuid.UUID, 0, len(found))
		for id := range found {
			foundIDs = append(foundIDs, id)
		}
		if err := s.db.NewSelect().Model(&known).Column("repo_id", "sha").Where("repo_id IN (?)", bun.List(foundIDs)).
			Where("sha IN (?)", bun.List(shas)).Scan(ctx); err != nil {
			return nil, fmt.Errorf("repo: resolve heads: %w", err)
		}
		isKnown := map[uuid.UUID]map[string]bool{}
		for _, c := range known {
			if isKnown[c.RepoID] == nil {
				isKnown[c.RepoID] = map[string]bool{}
			}
			isKnown[c.RepoID][c.SHA] = true
		}
		for _, r := range repos {
			t, ok := found[r.UUID]
			if !ok {
				continue
			}
			if isKnown[r.UUID][t] {
				out[r.UUID] = model.CommitSHA(t)
			} else {
				alone = append(alone, r)
			}
		}
	}
	for _, r := range alone {
		sha, err := s.Resolve(ctx, r.ID, "")
		switch {
		case err == nil:
			out[r.UUID] = sha
		case errors.Is(err, ErrRevisionNotFound), errors.Is(err, ErrNotFound):
		default:
			return nil, err
		}
	}
	return out, nil
}
