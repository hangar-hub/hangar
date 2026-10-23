package repo_test

import (
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/db"
	"github.com/hangar-hub/hangar/internal/model"
	"github.com/hangar-hub/hangar/internal/repo"
)

// ResolveHeads gives what Resolve gives for each Repo's default branch: the
// branch head, a tag of that name when there is no such branch, a head the
// Commit cache does not know yet, and nothing for a Repo with no branch.
func TestResolveHeadsMatchesResolve(t *testing.T) {
	forEach(t, func(t *testing.T, e *env) {
		alice := e.addUser("alice")
		mk := func(name string, commit bool) *repo.Repo {
			id := model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: name}
			r := e.create(repo.CreateParams{ID: id, Actor: alice})
			if commit {
				e.commit(id, alice, add("README.md", name))
			}
			return r
		}
		branch, empty, tagged, uncached := mk("branch", true), mk("empty", false), mk("tagged", true), mk("uncached", true)

		// empty has no branch.
		_, err := e.d.NewDelete().Model((*db.Ref)(nil)).Where("repo_id = ?", empty.UUID).Exec(e.ctx)
		require.NoError(t, err)
		// tagged's default branch names a tag, not a branch.
		head, err := e.svc.Resolve(e.ctx, tagged.ID, "")
		require.NoError(t, err)
		_, err = e.d.NewInsert().Model(&db.Ref{RepoID: tagged.UUID, Name: model.RefTags + "v1", Target: string(head)}).Exec(e.ctx)
		require.NoError(t, err)
		_, err = e.d.NewUpdate().Model((*db.Repo)(nil)).Set("default_branch = 'v1'").Where("id = ?", tagged.UUID).Exec(e.ctx)
		require.NoError(t, err)
		// uncached's head is not in the Commit cache.
		head, err = e.svc.Resolve(e.ctx, uncached.ID, "")
		require.NoError(t, err)
		_, err = e.d.NewDelete().Model((*db.Commit)(nil)).Where("repo_id = ?", uncached.UUID).Where("sha = ?", string(head)).Exec(e.ctx)
		require.NoError(t, err)

		var repos []*repo.Repo
		for _, r := range []*repo.Repo{branch, empty, tagged, uncached} {
			got, err := e.svc.GetByUUID(e.ctx, r.UUID)
			require.NoError(t, err)
			repos = append(repos, got)
		}
		heads, err := e.svc.ResolveHeads(e.ctx, repos)
		require.NoError(t, err)
		want := map[uuid.UUID]model.CommitSHA{}
		for _, r := range repos {
			sha, err := e.svc.Resolve(e.ctx, r.ID, "")
			if errors.Is(err, repo.ErrRevisionNotFound) {
				continue
			}
			require.NoError(t, err, r.ID.String())
			want[r.UUID] = sha
		}
		require.Len(t, want, 3)
		require.Equal(t, want, heads)
	})
}
