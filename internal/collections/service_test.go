package collections_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/auth/authtest"
	"github.com/jamesagarside/hangar/internal/collections"
	"github.com/jamesagarside/hangar/internal/db"
	"github.com/jamesagarside/hangar/internal/db/dbtest"
	"github.com/jamesagarside/hangar/internal/index/indextest"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/org/orgtest"
	"github.com/jamesagarside/hangar/internal/repo"
	"github.com/jamesagarside/hangar/internal/repo/repotest"
)

type env struct {
	svc               *collections.DB
	alice, bob, carol model.Principal
	authz             *authtest.Authz
	team              uuid.UUID
}

func newEnv(t *testing.T, d *db.DB) *env {
	t.Helper()
	ctx := context.Background()
	orgs := orgtest.NewFake()
	e := &env{authz: authtest.NewAuthz()}
	var err error
	for _, p := range []*model.Principal{&e.alice, &e.bob, &e.carol} {
		name := map[*model.Principal]string{&e.alice: "alice", &e.bob: "bob", &e.carol: "carol"}[p]
		*p, err = orgs.AddUser(name)
		require.NoError(t, err)
		indextest.InsertUser(t, d, *p)
	}
	team, err := orgs.CreateOrg(ctx, "team", "Team", e.alice)
	require.NoError(t, err)
	e.team = team.ID
	_, err = d.NewInsert().Model(&db.Namespace{ID: team.ID, Name: "team", Kind: "org", DisplayName: "Team"}).Exec(ctx)
	require.NoError(t, err)
	require.NoError(t, orgs.SetMember(ctx, "team", e.bob.UserID, model.RoleRead, "manual", e.alice))
	e.authz.SetRole(e.alice.UserID, team.ID, model.RoleAdmin)
	e.authz.SetRole(e.bob.UserID, team.ID, model.RoleRead)

	repos := repotest.NewFake(t)
	for _, r := range []struct {
		id  model.RepoID
		vis model.Visibility
	}{
		{model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "public-model"}, model.VisibilityPublic},
		{model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "secret-model"}, model.VisibilityPrivate},
		{model.RepoID{Type: model.RepoTypeDataset, Namespace: "alice", Name: "data"}, model.VisibilityPublic},
	} {
		_, err := repos.Create(ctx, repo.CreateParams{ID: r.id, Visibility: r.vis, Actor: e.alice})
		require.NoError(t, err)
	}
	e.svc = collections.New(collections.Options{DB: d, Orgs: orgs, Repos: repos, Authz: e.authz})
	return e
}

func TestCollectionsLifecycle(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		e := newEnv(t, d)
		ctx := t.Context()
		_, err := e.svc.Create(ctx, e.alice, collections.CreateParams{Namespace: "alice", Title: "!!!"})
		require.ErrorIs(t, err, collections.ErrInvalid)
		_, err = e.svc.Create(ctx, e.bob, collections.CreateParams{Namespace: "alice", Title: "Mine"})
		require.ErrorIs(t, err, collections.ErrForbidden)
		_, err = e.svc.Create(ctx, e.alice, collections.CreateParams{Namespace: "nobody", Title: "x"})
		require.ErrorIs(t, err, collections.ErrNotFound)
		_, err = e.svc.Create(ctx, e.alice, collections.CreateParams{Namespace: "alice", Title: "x", Visibility: "secret"})
		require.ErrorIs(t, err, collections.ErrInvalid)

		c, err := e.svc.Create(ctx, e.alice, collections.CreateParams{Namespace: "alice", Title: "Best Models 2026", Description: "Picks"})
		require.NoError(t, err)
		require.Equal(t, "best-models-2026", c.Slug)
		require.Equal(t, "alice", c.Namespace)
		require.Equal(t, model.VisibilityPublic, c.Visibility)
		_, err = e.svc.Create(ctx, e.alice, collections.CreateParams{Namespace: "alice", Title: "best models 2026"})
		require.ErrorIs(t, err, collections.ErrExists)
		same, err := e.svc.Create(ctx, e.alice, collections.CreateParams{Namespace: "alice", Title: "Best Models 2026", ExistOK: true})
		require.NoError(t, err)
		require.Equal(t, c.ID, same.ID)

		c, err = e.svc.AddItem(ctx, e.alice, "alice", c.Slug, collections.ItemParams{Type: "model", ItemID: "alice/public-model", Note: "Fast"})
		require.NoError(t, err)
		c, err = e.svc.AddItem(ctx, e.alice, "alice", c.Slug, collections.ItemParams{Type: "model", ItemID: "alice/secret-model"})
		require.NoError(t, err)
		c, err = e.svc.AddItem(ctx, e.alice, "alice", c.Slug, collections.ItemParams{Type: "dataset", ItemID: "ALICE/data"})
		require.NoError(t, err)
		require.Len(t, c.Items, 3)
		require.Equal(t, "alice/data", c.Items[2].ItemID, "the canonical ID is stored")
		_, err = e.svc.AddItem(ctx, e.alice, "alice", c.Slug, collections.ItemParams{Type: "model", ItemID: "alice/public-model"})
		require.ErrorIs(t, err, collections.ErrExists)
		again, err := e.svc.AddItem(ctx, e.alice, "alice", c.Slug, collections.ItemParams{Type: "model", ItemID: "alice/public-model", ExistOK: true})
		require.NoError(t, err)
		require.Len(t, again.Items, 3)
		for _, bad := range []collections.ItemParams{
			{Type: "model", ItemID: "alice/missing"}, {Type: "space", ItemID: "alice/x"}, {Type: "model", ItemID: "no-slash"},
			{Type: "collection", ItemID: "alice/best-models-2026"}, {Type: "collection", ItemID: "alice/missing"}, {Type: "collection", ItemID: "x"},
		} {
			_, err := e.svc.AddItem(ctx, e.alice, "alice", c.Slug, bad)
			require.ErrorIs(t, err, collections.ErrInvalid, "%+v", bad)
		}
		_, err = e.svc.AddItem(ctx, e.bob, "alice", c.Slug, collections.ItemParams{Type: "model", ItemID: "alice/public-model"})
		require.ErrorIs(t, err, collections.ErrForbidden)

		// Bob cannot see the private Repo in Alice's public Collection.
		seen, err := e.svc.Get(ctx, e.bob, "alice", c.Slug)
		require.NoError(t, err)
		require.Len(t, seen.Items, 2)
		// While anonymous reads are off (the fake Authz's default), a public
		// Collection is closed to anonymous callers, like its Namespace.
		_, err = e.svc.Get(ctx, model.Anonymous(), "alice", c.Slug)
		require.ErrorIs(t, err, collections.ErrNotFound)
		e.authz.AnonymousRead = true
		anon, err := e.svc.Get(ctx, model.Anonymous(), "alice", c.Slug)
		require.NoError(t, err)
		require.Len(t, anon.Items, 2, "anonymous callers see the public Repos once anonymous reads are on")
		e.authz.AnonymousRead = false

		// Reorder and change notes.
		third := c.Items[2].ID
		pos := 0
		note := "Data first"
		c, err = e.svc.UpdateItem(ctx, e.alice, "alice", c.Slug, third, collections.ItemUpdate{Position: &pos, Note: &note})
		require.NoError(t, err)
		require.Equal(t, third, c.Items[0].ID)
		require.Equal(t, "Data first", c.Items[0].Note)
		for i, it := range c.Items {
			require.Equal(t, i, it.Position)
		}
		_, err = e.svc.UpdateItem(ctx, e.alice, "alice", c.Slug, uuid.New(), collections.ItemUpdate{Note: &note})
		require.ErrorIs(t, err, collections.ErrNotFound)
		require.NoError(t, e.svc.DeleteItem(ctx, e.alice, "alice", c.Slug, third))
		require.ErrorIs(t, e.svc.DeleteItem(ctx, e.alice, "alice", c.Slug, third), collections.ErrNotFound)
		c, err = e.svc.Get(ctx, e.alice, "alice", c.Slug)
		require.NoError(t, err)
		require.Len(t, c.Items, 2)
		require.Equal(t, 0, c.Items[0].Position)

		title, desc, theme, vis := "Renamed", "New", "pink", model.VisibilityPrivate
		c, err = e.svc.Update(ctx, e.alice, "alice", c.Slug, collections.Update{Title: &title, Description: &desc, Theme: &theme, Visibility: &vis})
		require.NoError(t, err)
		require.Equal(t, "Renamed", c.Title)
		require.Equal(t, "best-models-2026", c.Slug, "the slug stays")
		_, err = e.svc.Get(ctx, e.bob, "alice", c.Slug)
		require.ErrorIs(t, err, collections.ErrNotFound, "private Collections are hidden from non-members")
		empty := " "
		_, err = e.svc.Update(ctx, e.alice, "alice", c.Slug, collections.Update{Title: &empty})
		require.ErrorIs(t, err, collections.ErrInvalid)
		badVis := model.Visibility("x")
		_, err = e.svc.Update(ctx, e.alice, "alice", c.Slug, collections.Update{Visibility: &badVis})
		require.ErrorIs(t, err, collections.ErrInvalid)

		require.ErrorIs(t, e.svc.Delete(ctx, e.bob, "alice", c.Slug), collections.ErrNotFound)
		require.NoError(t, e.svc.Delete(ctx, e.alice, "alice", c.Slug))
		_, err = e.svc.Get(ctx, e.alice, "alice", c.Slug)
		require.ErrorIs(t, err, collections.ErrNotFound)
	})
}

func TestListFiltersAndVisibility(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		e := newEnv(t, d)
		ctx := t.Context()
		pub, err := e.svc.Create(ctx, e.alice, collections.CreateParams{Namespace: "alice", Title: "Public picks"})
		require.NoError(t, err)
		_, err = e.svc.AddItem(ctx, e.alice, "alice", pub.Slug, collections.ItemParams{Type: "model", ItemID: "alice/public-model"})
		require.NoError(t, err)
		_, err = e.svc.Create(ctx, e.alice, collections.CreateParams{Namespace: "alice", Title: "Internal 50%_off", Visibility: model.VisibilityInternal})
		require.NoError(t, err)
		teamCol, err := e.svc.Create(ctx, e.alice, collections.CreateParams{Namespace: "team", Title: "Team only", Visibility: model.VisibilityPrivate})
		require.NoError(t, err)
		_, err = e.svc.AddItem(ctx, e.alice, "team", teamCol.Slug, collections.ItemParams{Type: "collection", ItemID: "alice/public-picks"})
		require.NoError(t, err)

		titles := func(viewer model.Principal, o collections.ListOptions) []string {
			list, err := e.svc.List(ctx, viewer, o)
			require.NoError(t, err)
			out := []string{}
			for _, c := range list {
				out = append(out, c.Namespace+"/"+c.Slug)
			}
			return out
		}
		require.Equal(t, []string{"team/team-only", "alice/internal-50-off", "alice/public-picks"}, titles(e.bob, collections.ListOptions{}))
		require.Equal(t, []string{"alice/internal-50-off", "alice/public-picks"}, titles(e.carol, collections.ListOptions{}))
		require.Empty(t, titles(model.Anonymous(), collections.ListOptions{}), "public Collections are closed to anonymous callers while anonymous reads are off")
		e.authz.AnonymousRead = true
		require.Equal(t, []string{"alice/public-picks"}, titles(model.Anonymous(), collections.ListOptions{}))
		e.authz.AnonymousRead = false
		require.Equal(t, []string{"team/team-only"}, titles(e.bob, collections.ListOptions{Owner: "team"}))
		require.Empty(t, titles(e.bob, collections.ListOptions{Owner: "nobody"}))
		require.Equal(t, []string{"alice/internal-50-off"}, titles(e.bob, collections.ListOptions{Query: "50%_"}))
		require.Empty(t, titles(e.bob, collections.ListOptions{Query: "%z"}))
		require.Equal(t, []string{"alice/public-picks"}, titles(e.bob, collections.ListOptions{Item: "models/alice/public-model"}))
		require.Equal(t, []string{"team/team-only"}, titles(e.bob, collections.ListOptions{Item: "collections/alice/public-picks"}))
		require.Len(t, titles(e.bob, collections.ListOptions{Limit: 1}), 1)
		_, err = e.svc.List(ctx, e.bob, collections.ListOptions{Item: "bad"})
		require.ErrorIs(t, err, collections.ErrInvalid)

		got, err := e.svc.Get(ctx, e.bob, "team", teamCol.Slug)
		require.NoError(t, err)
		require.Len(t, got.Items, 1)
		require.Equal(t, "collection", got.Items[0].Type)
		require.Equal(t, "best-models-2026", collections.Slugify(" Best   Models 2026! "))
	})
}
