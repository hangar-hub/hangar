package communitydb_test

import (
	"bytes"
	"io"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/community"
	"github.com/jamesagarside/hangar/internal/community/communitydb"
	"github.com/jamesagarside/hangar/internal/db"
	"github.com/jamesagarside/hangar/internal/db/dbtest"
	"github.com/jamesagarside/hangar/internal/index/indextest"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/repo"
	"github.com/jamesagarside/hangar/internal/repo/repotest"
	"github.com/jamesagarside/hangar/internal/webhook"
	"github.com/jamesagarside/hangar/internal/webhook/webhooktest"
)

type env struct {
	db    *db.DB
	repos *repotest.Fake
	hooks *webhooktest.Fake
	svc   *communitydb.DB
	repo  *repo.Repo
	alice model.Principal
	bob   model.Principal
}

func newEnv(t *testing.T, d *db.DB) *env {
	t.Helper()
	e := &env{db: d, repos: repotest.NewFake(t), hooks: &webhooktest.Fake{}}
	e.alice, e.bob = indextest.Owner("alice"), indextest.Owner("bob")
	e.alice.IsAdmin = false
	indextest.InsertUser(t, d, e.alice)
	indextest.InsertUser(t, d, e.bob)
	id := model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "tiny"}
	e.repo = indextest.CreateRepo(t, e.repos, id, model.VisibilityPublic)
	indextest.InsertRepoRow(t, d, e.repo)
	e.svc = communitydb.New(communitydb.Options{DB: d, Repos: e.repos, Webhooks: e.hooks})
	return e
}

func (e *env) commit(t *testing.T, rev string, from model.CommitSHA, files map[string]string, deletes ...string) model.CommitSHA {
	t.Helper()
	var ops []repo.Operation
	for p, c := range files {
		ops = append(ops, repo.Operation{Kind: repo.OpAdd, Path: p, Content: bytes.NewReader([]byte(c))})
	}
	for _, p := range deletes {
		ops = append(ops, repo.Operation{Kind: repo.OpDelete, Path: p})
	}
	res, err := e.repos.Commit(t.Context(), repo.CommitParams{Repo: e.repo.ID, Revision: rev, CreateFrom: from, Summary: "change", Operations: ops, Actor: e.bob, AllowMirror: true})
	require.NoError(t, err)
	return res.SHA
}

func TestDiscussionsLifecycle(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		e := newEnv(t, d)
		ctx := t.Context()
		_, err := e.svc.Create(ctx, community.CreateParams{RepoUUID: e.repo.UUID, Title: " ", Author: e.alice})
		require.ErrorIs(t, err, community.ErrInvalid)
		_, err = e.svc.Create(ctx, community.CreateParams{RepoUUID: e.repo.UUID, Title: "x", Author: model.Anonymous()})
		require.ErrorIs(t, err, community.ErrInvalid)
		_, err = e.svc.Create(ctx, community.CreateParams{RepoUUID: e.repo.UUID, Title: string(make([]byte, 201)), Author: e.alice})
		require.ErrorIs(t, err, community.ErrInvalid)

		first, err := e.svc.Create(ctx, community.CreateParams{RepoUUID: e.repo.UUID, Repo: e.repo.ID, Title: "Question", Description: "How?", Author: e.alice})
		require.NoError(t, err)
		require.Equal(t, 1, first.Num)
		require.Equal(t, community.StatusOpen, first.Status)
		require.Equal(t, "alice", first.Author)
		require.Len(t, first.Events, 1)
		require.Equal(t, "How?", first.Events[0].Body)
		require.Equal(t, "alice", first.Events[0].Author)

		pr, err := e.svc.Create(ctx, community.CreateParams{RepoUUID: e.repo.UUID, Repo: e.repo.ID, Title: "Add weights", PullRequest: true, Author: e.bob})
		require.NoError(t, err)
		require.Equal(t, 2, pr.Num)
		require.Equal(t, community.KindPullRequest, pr.Kind)
		require.Equal(t, community.StatusDraft, pr.Status)
		require.Equal(t, "refs/pr/2", pr.PRRef)
		require.Equal(t, "main", pr.PRBase)
		require.Empty(t, pr.Events)

		c, err := e.svc.Comment(ctx, e.repo.UUID, 1, "Like this.", e.bob)
		require.NoError(t, err)
		require.Equal(t, "bob", c.Author)
		_, err = e.svc.Comment(ctx, e.repo.UUID, 1, "  ", e.bob)
		require.ErrorIs(t, err, community.ErrInvalid)
		_, err = e.svc.Comment(ctx, e.repo.UUID, 1, "hi", model.Anonymous())
		require.ErrorIs(t, err, community.ErrInvalid)
		_, err = e.svc.Comment(ctx, e.repo.UUID, 9, "hi", e.bob)
		require.ErrorIs(t, err, community.ErrNotFound)

		edited, err := e.svc.EditComment(ctx, e.repo.UUID, 1, c.ID, "Like this, please.", e.bob)
		require.NoError(t, err)
		require.True(t, edited.Edited)
		require.Equal(t, "Like this, please.", edited.Body)
		require.Equal(t, "bob", edited.Author)
		_, err = e.svc.EditComment(ctx, e.repo.UUID, 1, uuid.New(), "x", e.bob)
		require.ErrorIs(t, err, community.ErrNotFound)
		hidden, err := e.svc.HideComment(ctx, e.repo.UUID, 1, c.ID, e.alice)
		require.NoError(t, err)
		require.True(t, hidden.Hidden)
		require.Empty(t, hidden.Body)
		_, err = e.svc.EditComment(ctx, e.repo.UUID, 1, c.ID, "again", e.bob)
		require.ErrorIs(t, err, community.ErrInvalid)

		ev, err := e.svc.Rename(ctx, e.repo.UUID, 1, "Better question", e.alice)
		require.NoError(t, err)
		require.Equal(t, map[string]any{"to": "Better question", "from": "Question"}, ev.Data)
		_, err = e.svc.Rename(ctx, e.repo.UUID, 1, "", e.alice)
		require.ErrorIs(t, err, community.ErrInvalid)

		_, err = e.svc.SetStatus(ctx, e.repo.UUID, 1, "merged", "", e.alice)
		require.ErrorIs(t, err, community.ErrInvalid)
		closed, err := e.svc.SetStatus(ctx, e.repo.UUID, 1, community.StatusClosed, "Answered.", e.alice)
		require.NoError(t, err)
		require.Equal(t, "closed", closed.Data["status"])

		got, err := e.svc.Get(ctx, e.repo.UUID, 1)
		require.NoError(t, err)
		require.Equal(t, "Better question", got.Title)
		require.Equal(t, community.StatusClosed, got.Status)
		types := []string{}
		for _, ev := range got.Events {
			types = append(types, ev.Type)
		}
		require.Equal(t, []string{"comment", "comment", "title-change", "status-change"}, types)
		require.True(t, got.Events[1].Hidden)

		list, total, err := e.svc.List(ctx, e.repo.UUID, community.ListOptions{})
		require.NoError(t, err)
		require.Equal(t, 2, total)
		require.Equal(t, 2, list[0].Num, "newest first")
		list, total, err = e.svc.List(ctx, e.repo.UUID, community.ListOptions{Status: community.StatusOpen})
		require.NoError(t, err)
		require.Equal(t, 1, total)
		require.Equal(t, 2, list[0].Num, "drafts count as open")
		_, total, err = e.svc.List(ctx, e.repo.UUID, community.ListOptions{Status: community.StatusClosed, Type: community.KindDiscussion})
		require.NoError(t, err)
		require.Equal(t, 1, total)
		list, _, err = e.svc.List(ctx, e.repo.UUID, community.ListOptions{Author: "BOB"})
		require.NoError(t, err)
		require.Len(t, list, 1)
		list, total, err = e.svc.List(ctx, e.repo.UUID, community.ListOptions{Limit: 1, Page: 1})
		require.NoError(t, err)
		require.Equal(t, 2, total)
		require.Len(t, list, 1)
		require.Equal(t, 1, list[0].Num)

		require.Equal(t, []webhook.EventName{webhook.DiscussionCreated, webhook.DiscussionCreated, webhook.DiscussionCommented, webhook.DiscussionStatusChanged}, e.hooks.Names())

		// A pull request without commits can be discarded; a discussion cannot.
		require.ErrorIs(t, e.svc.Discard(ctx, e.repo.UUID, 1), community.ErrNotPullRequest)
		require.NoError(t, e.svc.Discard(ctx, e.repo.UUID, 2))
		_, err = e.svc.Get(ctx, e.repo.UUID, 2)
		require.ErrorIs(t, err, community.ErrNotFound)
		next, err := e.svc.Create(ctx, community.CreateParams{RepoUUID: e.repo.UUID, Title: "Next", Author: e.alice})
		require.NoError(t, err)
		require.Equal(t, 2, next.Num, "numbers continue from the highest left")
	})
}

func TestMergeAppliesThePullRequest(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		e := newEnv(t, d)
		ctx := t.Context()
		base := e.commit(t, "main", "", map[string]string{"README.md": "# v1", "keep.txt": "keep", "old.txt": "old"})
		pr, err := e.svc.Create(ctx, community.CreateParams{RepoUUID: e.repo.UUID, Repo: e.repo.ID, Title: "Update docs", Description: "Please merge", PullRequest: true, Author: e.bob})
		require.NoError(t, err)
		_, err = e.svc.Merge(ctx, e.repo.UUID, pr.Num, "", e.alice)
		require.ErrorIs(t, err, community.ErrInvalid, "no commits yet")

		lfs, err := e.repos.LFS.Put(ctx, []byte("weights"))
		require.NoError(t, err)
		_, err = e.repos.Commit(ctx, repo.CommitParams{Repo: e.repo.ID, Revision: pr.PRRef, CreateFrom: base, Summary: "pr change", Actor: e.bob, Operations: []repo.Operation{
			{Kind: repo.OpAdd, Path: "README.md", Content: bytes.NewReader([]byte("# v2"))},
			{Kind: repo.OpAddLFS, Path: "model.safetensors", OID: lfs.OID, Size: lfs.Size},
			{Kind: repo.OpDelete, Path: "old.txt"},
		}})
		require.NoError(t, err)
		// main moves on without touching the same files.
		e.commit(t, "main", "", map[string]string{"other.txt": "other"})

		changes, err := communitydb.PRChanges(ctx, e.repos, e.repo.ID, pr, "main")
		require.NoError(t, err)
		status := map[string]repo.ChangeStatus{}
		for _, c := range changes {
			status[c.Path] = c.Status
		}
		require.Equal(t, map[string]repo.ChangeStatus{"README.md": repo.ChangeModified, "model.safetensors": repo.ChangeAdded, "old.txt": repo.ChangeDeleted}, status)

		ev, err := e.svc.Merge(ctx, e.repo.UUID, pr.Num, "Thanks", e.alice)
		require.NoError(t, err)
		require.Equal(t, community.StatusMerged, ev.Data["status"])
		sha, _ := ev.Data["oid"].(string)
		require.True(t, model.IsFullSHA(sha))

		tip, err := e.repos.Resolve(ctx, e.repo.ID, "main")
		require.NoError(t, err)
		require.Equal(t, model.CommitSHA(sha), tip)
		info, err := e.repos.GetCommit(ctx, e.repo.ID, tip)
		require.NoError(t, err)
		require.Equal(t, "Update docs (#1)", info.Title)
		read := func(p string) string {
			rc, _, err := e.repos.OpenFile(ctx, e.repo.ID, tip, p, nil)
			require.NoError(t, err)
			defer rc.Close()
			b, _ := io.ReadAll(rc)
			return string(b)
		}
		require.Equal(t, "# v2", read("README.md"))
		require.Equal(t, "weights", read("model.safetensors"))
		require.Equal(t, "other", read("other.txt"))
		_, err = e.repos.Entry(ctx, e.repo.ID, tip, "old.txt")
		require.ErrorIs(t, err, repo.ErrEntryNotFound)

		got, err := e.svc.Get(ctx, e.repo.UUID, pr.Num)
		require.NoError(t, err)
		require.Equal(t, community.StatusMerged, got.Status)
		_, err = e.svc.Merge(ctx, e.repo.UUID, pr.Num, "", e.alice)
		require.ErrorIs(t, err, community.ErrNotOpen)
		_, err = e.svc.SetStatus(ctx, e.repo.UUID, pr.Num, community.StatusOpen, "", e.alice)
		require.ErrorIs(t, err, community.ErrNotOpen)
		require.Contains(t, e.hooks.Names(), webhook.PRMerged)

		disc, err := e.svc.Create(ctx, community.CreateParams{RepoUUID: e.repo.UUID, Title: "Not a PR", Author: e.alice})
		require.NoError(t, err)
		_, err = e.svc.Merge(ctx, e.repo.UUID, disc.Num, "", e.alice)
		require.ErrorIs(t, err, community.ErrNotPullRequest)
		_, err = communitydb.PRChanges(ctx, e.repos, e.repo.ID, disc, "main")
		require.ErrorIs(t, err, community.ErrNotPullRequest)
	})
}

func TestMergeRefusesConflicts(t *testing.T) {
	d := dbtest.SQLite(t)
	e := newEnv(t, d)
	ctx := t.Context()
	base := e.commit(t, "main", "", map[string]string{"README.md": "# v1"})
	pr, err := e.svc.Create(ctx, community.CreateParams{RepoUUID: e.repo.UUID, Title: "Edit", PullRequest: true, Base: "main", Author: e.bob})
	require.NoError(t, err)
	e.commit(t, pr.PRRef, base, map[string]string{"README.md": "# from the PR"})
	main := e.commit(t, "main", "", map[string]string{"README.md": "# from main"})
	_, err = e.svc.Merge(ctx, e.repo.UUID, pr.Num, "", e.alice)
	require.ErrorIs(t, err, community.ErrConflict)
	require.ErrorContains(t, err, "README.md")
	tip, err := e.repos.Resolve(ctx, e.repo.ID, "main")
	require.NoError(t, err)
	require.Equal(t, main, tip, "nothing changed")

	// A pull request that reverts to what main already has is empty.
	same, err := e.svc.Create(ctx, community.CreateParams{RepoUUID: e.repo.UUID, Title: "Same", PullRequest: true, Author: e.bob})
	require.NoError(t, err)
	e.commit(t, same.PRRef, main, map[string]string{"README.md": "# from main"})
	_, err = e.svc.Merge(ctx, e.repo.UUID, same.Num, "", e.alice)
	require.ErrorIs(t, err, community.ErrInvalid)
}

func TestLikesAndDownloads(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		e := newEnv(t, d)
		ctx := t.Context()
		require.ErrorIs(t, e.svc.Like(ctx, uuid.Nil, e.repo.UUID), community.ErrInvalid)
		require.NoError(t, e.svc.Like(ctx, e.bob.UserID, e.repo.UUID))
		require.NoError(t, e.svc.Like(ctx, e.bob.UserID, e.repo.UUID))
		require.NoError(t, e.svc.Like(ctx, e.alice.UserID, e.repo.UUID))
		n, err := e.svc.Likes(ctx, e.repo.UUID)
		require.NoError(t, err)
		require.Equal(t, int64(2), n)
		liked, err := e.svc.Liked(ctx, e.bob.UserID, e.repo.UUID)
		require.NoError(t, err)
		require.True(t, liked)
		names, err := e.svc.Likers(ctx, e.repo.UUID)
		require.NoError(t, err)
		require.Equal(t, []string{"alice", "bob"}, names)
		repos, err := e.svc.LikedBy(ctx, e.bob.UserID)
		require.NoError(t, err)
		require.Equal(t, []uuid.UUID{e.repo.UUID}, repos)
		require.NoError(t, e.svc.Unlike(ctx, e.bob.UserID, e.repo.UUID))
		liked, err = e.svc.Liked(ctx, e.bob.UserID, e.repo.UUID)
		require.NoError(t, err)
		require.False(t, liked)
		n, _ = e.svc.Likes(ctx, e.repo.UUID)
		require.Equal(t, int64(1), n)

		day1 := time.Date(2026, 10, 1, 23, 59, 0, 0, time.UTC)
		day2 := time.Date(2026, 10, 2, 0, 1, 0, 0, time.FixedZone("x", 3600))
		for _, at := range []time.Time{day1, day1, day1.Add(time.Minute), day2.Add(24 * time.Hour)} {
			require.NoError(t, e.svc.RecordDownload(ctx, e.repo.UUID, at))
		}
		days, err := e.svc.Downloads(ctx, e.repo.UUID, time.Time{})
		require.NoError(t, err)
		require.Equal(t, []community.DayCount{
			{Day: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), Count: 2},
			{Day: time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC), Count: 2},
		}, days)
		recent, err := e.svc.Downloads(ctx, e.repo.UUID, time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC))
		require.NoError(t, err)
		require.Len(t, recent, 1)
		require.Equal(t, int64(2), recent[0].Count)
	})
}
