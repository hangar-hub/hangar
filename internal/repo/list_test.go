package repo_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/community/communitytest"
	"github.com/jamesagarside/hangar/internal/db"
	"github.com/jamesagarside/hangar/internal/index"
	"github.com/jamesagarside/hangar/internal/index/indextest"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/org"
	"github.com/jamesagarside/hangar/internal/repo"
)

func (e *env) addOrg(name string, members map[uuid.UUID]model.Role) uuid.UUID {
	e.t.Helper()
	ns := &db.Namespace{ID: db.NewID(), Name: name, Kind: org.KindOrg, DisplayName: name}
	_, err := e.d.NewInsert().Model(ns).Exec(e.ctx)
	require.NoError(e.t, err)
	for u, r := range members {
		_, err := e.d.NewInsert().Model(&db.Membership{ID: db.NewID(), OrgNamespaceID: ns.ID, UserID: u, Role: string(r), Source: "manual"}).Exec(e.ctx)
		require.NoError(e.t, err)
	}
	return ns.ID
}

func ids(l *repo.ListResult) []string {
	out := []string{}
	for _, r := range l.Repos {
		out = append(out, string(r.ID.Type)+":"+r.ID.String())
	}
	return out
}

func TestListVisibilityFiltersAndPages(t *testing.T) {
	forEach(t, func(t *testing.T, e *env) {
		alice, bob, carol := e.addUser("alice"), e.addUser("bob"), e.addUser("carol")
		e.addOrg("lab", map[uuid.UUID]model.Role{bob.UserID: model.RoleRead})
		mk := func(t model.RepoType, ns, name string, vis model.Visibility, mirror bool) *repo.Repo {
			e.clock.Add(time.Minute)
			return e.create(repo.CreateParams{ID: model.RepoID{Type: t, Namespace: ns, Name: name}, Visibility: vis, Actor: alice, Mirror: mirror})
		}
		mk(model.RepoTypeModel, "alice", "pub", model.VisibilityPublic, false)
		mk(model.RepoTypeModel, "alice", "internal", model.VisibilityInternal, false)
		secret := mk(model.RepoTypeModel, "alice", "secret", model.VisibilityPrivate, false)
		mk(model.RepoTypeModel, "lab", "private-lab", model.VisibilityPrivate, false)
		mk(model.RepoTypeDataset, "lab", "data", model.VisibilityPublic, true)
		tags := []string{"llm"}
		_, err := e.svc.UpdateSettings(e.ctx, model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "pub"}, repo.SettingsUpdate{Tags: &tags}, alice)
		require.NoError(t, err)
		_, err = e.d.NewInsert().Model(&db.Collaborator{ID: db.NewID(), RepoID: secret.UUID, UserID: carol.UserID, Role: "read"}).Exec(e.ctx)
		require.NoError(t, err)

		list := func(o repo.ListOptions) []string {
			t.Helper()
			res, err := e.svc.List(e.ctx, o)
			require.NoError(t, err)
			return ids(res)
		}
		require.Empty(t, list(repo.ListOptions{Viewer: model.Anonymous()}), "anonymous_read is off")
		require.Equal(t, []string{"model:alice/internal", "model:alice/pub", "dataset:lab/data"}, list(repo.ListOptions{Viewer: e.addUser("dave")}))
		require.Equal(t, []string{"model:alice/internal", "model:alice/pub", "dataset:lab/data", "model:lab/private-lab"}, list(repo.ListOptions{Viewer: bob}))
		require.Equal(t, []string{"model:alice/internal", "model:alice/pub", "model:alice/secret", "dataset:lab/data"}, list(repo.ListOptions{Viewer: carol}))
		require.Len(t, list(repo.ListOptions{Viewer: alice}), 4)
		require.Len(t, list(repo.ListOptions{Viewer: model.Principal{Kind: model.PrincipalUser, UserID: uuid.New(), IsAdmin: true}}), 5)

		tok := bob
		tok.Token = &model.TokenInfo{Scope: model.ScopeRead, Restrictions: model.TokenRestrictions{Namespaces: []string{"LAB"}, Repos: []string{"model:alice/pub"}}}
		require.Equal(t, []string{"model:alice/pub", "dataset:lab/data", "model:lab/private-lab"}, list(repo.ListOptions{Viewer: tok}))

		yes, no := true, false
		require.Equal(t, []string{"dataset:lab/data"}, list(repo.ListOptions{Viewer: bob, Mirror: &yes}))
		require.Len(t, list(repo.ListOptions{Viewer: bob, Mirror: &no}), 3)
		require.Equal(t, []string{"dataset:lab/data"}, list(repo.ListOptions{Viewer: bob, Type: model.RepoTypeDataset}))
		require.Equal(t, []string{"dataset:lab/data", "model:lab/private-lab"}, list(repo.ListOptions{Viewer: bob, Namespaces: []string{"Lab"}}))
		require.Equal(t, []string{"model:lab/private-lab"}, list(repo.ListOptions{Viewer: bob, Search: "LAB/PRIV"}))
		require.Equal(t, []string{"model:alice/pub"}, list(repo.ListOptions{Viewer: bob, Tags: []string{"llm"}}))
		require.Equal(t, []string{"dataset:lab/data", "model:lab/private-lab"}, list(repo.ListOptions{Viewer: bob, Sort: "created", Desc: true, Limit: 2}))

		// Pages.
		res, err := e.svc.List(e.ctx, repo.ListOptions{Viewer: bob, Limit: 3})
		require.NoError(t, err)
		require.Len(t, res.Repos, 3)
		require.NotEmpty(t, res.NextCursor)
		res, err = e.svc.List(e.ctx, repo.ListOptions{Viewer: bob, Limit: 3, Cursor: res.NextCursor})
		require.NoError(t, err)
		require.Len(t, res.Repos, 1)
		require.Empty(t, res.NextCursor)

		for _, o := range []repo.ListOptions{{Sort: "best"}, {Cursor: "x"}, {Type: "space"}, {Formats: []string{"gguf"}}} {
			_, err := e.svc.List(e.ctx, o)
			require.ErrorIs(t, err, repo.ErrInvalid)
		}
		for _, s := range []string{"name", "updated", "size"} {
			require.Len(t, list(repo.ListOptions{Viewer: bob, Sort: s}), 4)
		}

		n, err := e.svc.CountByNamespace(e.ctx, alice.NamespaceID, bob)
		require.NoError(t, err)
		require.Equal(t, 2, n)
	}, func(o *repo.Options) { o.AnonymousRead = false })
}

func TestListMetadataFiltersAndFacets(t *testing.T) {
	idx := indextest.NewFake()
	comm := communitytest.NewFake()
	forEach(t, func(t *testing.T, e *env) {
		alice := e.addUser("alice")
		mk := func(name string, s *index.Summary) *repo.Repo {
			r := e.create(repo.CreateParams{ID: model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: name}, Actor: alice})
			if s != nil {
				head, err := e.svc.Resolve(e.ctx, r.ID, "")
				require.NoError(t, err)
				idx.SetSummary(r.UUID, head, s)
				_, err = idx.Index(e.ctx, r.UUID, r.ID, head)
				require.NoError(t, err)
			}
			return r
		}
		mk("gguf", &index.Summary{Formats: []string{"gguf"}, Quantisations: []index.Quantisation{{Name: "Q4_K_M"}, {Name: "Q8_0"}},
			Parameters: &index.Parameters{Total: 7e9}, ContextLength: 8192, PipelineTag: "text-generation", Licence: index.Licence{ID: "apache-2.0"}})
		st := mk("st", &index.Summary{Formats: []string{"safetensors"}, Parameters: &index.Parameters{Total: 1e8}, ContextLength: 512,
			LibraryName: "transformers", PipelineTag: "fill-mask"})
		mk("plain", nil)
		ds := e.create(repo.CreateParams{ID: model.RepoID{Type: model.RepoTypeDataset, Namespace: "alice", Name: "ds"}, Actor: alice})
		head, err := e.svc.Resolve(e.ctx, ds.ID, "")
		require.NoError(t, err)
		idx.SetSummary(ds.UUID, head, &index.Summary{Dataset: &index.DatasetSummary{Modalities: []string{"text"}, Languages: []string{"en"},
			SizeCategory: "n<1K", Formats: []string{"parquet"}, Tasks: []string{"text-classification"}}})
		_, err = idx.Index(e.ctx, ds.UUID, ds.ID, head)
		require.NoError(t, err)
		require.NoError(t, comm.Like(e.ctx, alice.UserID, st.UUID))

		list := func(o repo.ListOptions) *repo.ListResult {
			t.Helper()
			o.Viewer = alice
			res, err := e.svc.List(e.ctx, o)
			require.NoError(t, err)
			return res
		}
		require.Equal(t, []string{"model:alice/gguf"}, ids(list(repo.ListOptions{Formats: []string{"GGUF"}})))
		require.Equal(t, []string{"model:alice/gguf"}, ids(list(repo.ListOptions{Quants: []string{"q8_0"}})))
		require.Equal(t, []string{"model:alice/gguf"}, ids(list(repo.ListOptions{ParamsMin: 1e9, ContextMin: 4096})))
		require.Equal(t, []string{"model:alice/st"}, ids(list(repo.ListOptions{ParamsMax: 1e9})))
		require.Equal(t, []string{"model:alice/st"}, ids(list(repo.ListOptions{Libraries: []string{"transformers"}})))
		require.Equal(t, []string{"model:alice/gguf"}, ids(list(repo.ListOptions{Tasks: []string{"text-generation"}, Licences: []string{"apache-2.0"}})))
		require.Equal(t, []string{"dataset:alice/ds"}, ids(list(repo.ListOptions{Modalities: []string{"text"}, Languages: []string{"en"}, SizeCategories: []string{"n<1K"}})))
		require.Equal(t, []string{"model:alice/gguf", "model:alice/st"}, ids(list(repo.ListOptions{Sort: "parameters", Desc: true, Formats: []string{"gguf", "safetensors"}})))
		require.Equal(t, "model:alice/st", ids(list(repo.ListOptions{Sort: "likes", Desc: true}))[0])
		require.Len(t, ids(list(repo.ListOptions{Sort: "downloads"})), 4)

		res := list(repo.ListOptions{IncludeFacets: true, Limit: 1})
		require.Len(t, res.Repos, 1)
		require.Equal(t, "1", res.NextCursor)
		require.Equal(t, 1, res.Facets[repo.FacetFormat]["gguf"])
		require.Equal(t, 1, res.Facets[repo.FacetFormat]["parquet"])
		require.Equal(t, 1, res.Facets[repo.FacetQuant]["Q4_K_M"])
		require.Equal(t, 3, res.Facets[repo.FacetType]["model"])
		require.Equal(t, 4, res.Facets[repo.FacetMirror]["false"])
		require.Equal(t, 4, res.Facets[repo.FacetNamespaces]["alice"])
	}, func(o *repo.Options) { o.Index = idx; o.Community = comm })
}
