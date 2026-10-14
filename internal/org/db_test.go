package org_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/audit"
	"github.com/jamesagarside/hangar/internal/audit/audittest"
	"github.com/jamesagarside/hangar/internal/auth"
	"github.com/jamesagarside/hangar/internal/auth/authtest"
	"github.com/jamesagarside/hangar/internal/db"
	"github.com/jamesagarside/hangar/internal/db/dbtest"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/org"
)

func setup(t *testing.T, d *db.DB) (*auth.Service, *org.DBService, *audittest.Fake, map[string]model.Principal) {
	t.Helper()
	rec := audittest.NewFake()
	s, o := authtest.NewService(t, d, authtest.RealOptions{Audit: rec})
	users := map[string]model.Principal{}
	for _, n := range []string{"alice", "bob", "carol"} {
		u, err := s.Users.Create(context.Background(), auth.NewUser{Username: n}, model.System())
		require.NoError(t, err)
		users[n] = u.Principal(model.AuthSession)
	}
	return s, o, rec, users
}

func TestOrganisations(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		ctx := context.Background()
		_, o, rec, u := setup(t, d)
		alice, bob, carol := u["alice"], u["bob"], u["carol"]

		ml, err := o.CreateOrg(ctx, "ML-Team", "", alice)
		require.NoError(t, err)
		require.Equal(t, org.KindOrg, ml.Kind)
		require.Equal(t, "ML-Team", ml.DisplayName)
		_, err = o.CreateOrg(ctx, "ml-team", "", bob)
		require.ErrorIs(t, err, org.ErrExists)
		_, err = o.CreateOrg(ctx, "Bob", "", alice)
		require.ErrorIs(t, err, org.ErrExists, "users and Organisations share one name space")
		_, err = o.CreateOrg(ctx, "api", "", alice)
		require.ErrorIs(t, err, org.ErrInvalid)
		_, err = o.CreateOrg(ctx, "anon", "", model.Anonymous())
		require.ErrorIs(t, err, org.ErrInvalid)
		got, err := o.Get(ctx, "ml-team")
		require.NoError(t, err)
		require.Equal(t, ml.ID, got.ID)
		got, err = o.GetByID(ctx, ml.ID)
		require.NoError(t, err)
		require.Equal(t, "ML-Team", got.Name)
		_, err = o.Get(ctx, "nope")
		require.ErrorIs(t, err, org.ErrNotFound)

		dn, desc := "Machine Learning", "We train things."
		up, err := o.UpdateOrg(ctx, "ml-team", org.OrgUpdate{DisplayName: &dn, Description: &desc}, alice)
		require.NoError(t, err)
		require.Equal(t, dn, up.DisplayName)
		require.Equal(t, desc, up.Description)
		_, err = o.UpdateOrg(ctx, "alice", org.OrgUpdate{DisplayName: &dn}, alice)
		require.ErrorIs(t, err, org.ErrNotFound, "user Namespaces are not Organisations")

		require.NoError(t, o.SetMember(ctx, "ml-team", bob.UserID, model.RoleWrite, "", alice))
		require.ErrorIs(t, o.SetMember(ctx, "ml-team", uuid.New(), model.RoleRead, "", alice), org.ErrNotFound)
		require.ErrorIs(t, o.SetMember(ctx, "ml-team", bob.UserID, "owner", "", alice), org.ErrInvalid)
		require.ErrorIs(t, o.SetMember(ctx, "ml-team", bob.UserID, model.RoleRead, "ldap", alice), org.ErrInvalid)
		members, err := o.Members(ctx, "ml-team")
		require.NoError(t, err)
		require.Len(t, members, 2)
		require.Equal(t, "alice", members[0].Username)
		require.Equal(t, model.RoleAdmin, members[0].Role)
		require.Equal(t, org.SourceManual, members[1].Source)
		m, err := o.Member(ctx, "ml-team", "BOB")
		require.NoError(t, err)
		require.Equal(t, model.RoleWrite, m.Role)
		_, err = o.Member(ctx, "ml-team", "carol")
		require.ErrorIs(t, err, org.ErrNotFound)
		n, err := o.MemberCount(ctx, ml.ID)
		require.NoError(t, err)
		require.Equal(t, int64(2), n)

		role, ok, err := o.RoleIn(ctx, ml.ID, bob.UserID)
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, model.RoleWrite, role)
		_, ok, err = o.RoleIn(ctx, ml.ID, carol.UserID)
		require.NoError(t, err)
		require.False(t, ok)
		role, ok, err = o.RoleIn(ctx, alice.NamespaceID, alice.UserID)
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, model.RoleAdmin, role, "users are admins of their own Namespace")
		_, ok, err = o.RoleIn(ctx, alice.NamespaceID, bob.UserID)
		require.NoError(t, err)
		require.False(t, ok)

		ms, err := o.MembershipsOf(ctx, bob.UserID)
		require.NoError(t, err)
		require.Len(t, ms, 1)
		require.Equal(t, "ML-Team", ms[0].Org.Name)

		// The last admin cannot leave or be demoted.
		require.ErrorIs(t, o.SetMember(ctx, "ml-team", alice.UserID, model.RoleWrite, "", alice), org.ErrLastAdmin)
		require.ErrorIs(t, o.RemoveMember(ctx, "ml-team", alice.UserID, alice), org.ErrLastAdmin)
		require.NoError(t, o.SetMember(ctx, "ml-team", bob.UserID, model.RoleAdmin, "", alice))
		require.NoError(t, o.RemoveMember(ctx, "ml-team", alice.UserID, bob))
		require.ErrorIs(t, o.RemoveMember(ctx, "ml-team", alice.UserID, bob), org.ErrNotFound)

		// Concurrent demotions of the last two admins: one must fail.
		require.NoError(t, o.SetMember(ctx, "ml-team", carol.UserID, model.RoleAdmin, "", bob))
		var wg sync.WaitGroup
		errs := make([]error, 2)
		for i, p := range []model.Principal{bob, carol} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				errs[i] = o.SetMember(ctx, "ml-team", p.UserID, model.RoleRead, "", p)
			}()
		}
		wg.Wait()
		failed := 0
		for _, err := range errs {
			if errors.Is(err, org.ErrLastAdmin) {
				failed++
			} else {
				require.NoError(t, err)
			}
		}
		require.Equal(t, 1, failed, "exactly one demotion wins")

		actions := rec.Actions()
		require.Contains(t, actions, audit.OrgCreated)
		require.Contains(t, actions, audit.MembershipChanged)
		require.Contains(t, actions, audit.MembershipRemoved)
	})
}

func TestDeleteOrgAndMirrors(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		ctx := context.Background()
		_, o, rec, u := setup(t, d)
		acme, err := o.CreateOrg(ctx, "acme", "", u["alice"])
		require.NoError(t, err)
		repo := &db.Repo{ID: db.NewID(), NamespaceID: acme.ID, Name: "r", Type: "model", Visibility: "public", DefaultBranch: "main",
			HeadRef: "refs/heads/main", Tags: []string{}, Settings: map[string]any{}, Gating: map[string]any{}}
		_, err = d.NewInsert().Model(repo).Exec(ctx)
		require.NoError(t, err)
		require.ErrorIs(t, o.DeleteOrg(ctx, "acme", u["alice"]), org.ErrNotEmpty)
		_, err = d.NewDelete().Model((*db.Repo)(nil)).Where("id = ?", repo.ID).Exec(ctx)
		require.NoError(t, err)
		require.NoError(t, o.DeleteOrg(ctx, "acme", u["alice"]))
		_, err = o.Get(ctx, "acme")
		require.ErrorIs(t, err, org.ErrNotFound)
		require.ErrorIs(t, o.DeleteOrg(ctx, "alice", u["alice"]), org.ErrNotFound, "users cannot be deleted as Organisations")
		require.Contains(t, rec.Actions(), audit.OrgDeleted)

		m, err := o.EnsureMirrorNamespace(ctx, "bartowski")
		require.NoError(t, err)
		require.True(t, m.ReservedMirror)
		m2, err := o.EnsureMirrorNamespace(ctx, "BARTOWSKI")
		require.NoError(t, err)
		require.Equal(t, m.ID, m2.ID)
		_, err = o.EnsureMirrorNamespace(ctx, "alice")
		require.ErrorIs(t, err, org.ErrExists, "a local Namespace is never taken over")
		_, err = o.EnsureMirrorNamespace(ctx, "-bad")
		require.ErrorIs(t, err, org.ErrInvalid)
		api, err := o.EnsureMirrorNamespace(ctx, "datasets")
		require.NoError(t, err, "upstream names may match reserved ones")
		require.True(t, api.ReservedMirror)
	})
}

func TestCollaborators(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		ctx := context.Background()
		_, o, rec, u := setup(t, d)
		repo := &db.Repo{ID: db.NewID(), NamespaceID: u["alice"].NamespaceID, Name: "r", Type: "model", Visibility: "private", DefaultBranch: "main",
			HeadRef: "refs/heads/main", Tags: []string{}, Settings: map[string]any{}, Gating: map[string]any{}}
		_, err := d.NewInsert().Model(repo).Exec(ctx)
		require.NoError(t, err)
		require.NoError(t, o.SetCollaborators(ctx, repo.ID, []org.Collaborator{{UserID: u["carol"].UserID, Role: model.RoleWrite}, {UserID: u["bob"].UserID, Role: model.RoleRead}}, u["alice"]))
		cs, err := o.Collaborators(ctx, repo.ID)
		require.NoError(t, err)
		require.Len(t, cs, 2)
		require.Equal(t, "bob", cs[0].Username)
		role, ok, err := o.CollaboratorRole(ctx, repo.ID, u["carol"].UserID)
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, model.RoleWrite, role)
		require.ErrorIs(t, o.SetCollaborators(ctx, repo.ID, []org.Collaborator{{UserID: u["bob"].UserID, Role: model.RoleAdmin}}, u["alice"]), org.ErrInvalid)
		require.ErrorIs(t, o.SetCollaborators(ctx, repo.ID, []org.Collaborator{{UserID: u["bob"].UserID, Role: model.RoleRead}, {UserID: u["bob"].UserID, Role: model.RoleWrite}}, u["alice"]), org.ErrInvalid)
		require.ErrorIs(t, o.SetCollaborators(ctx, repo.ID, []org.Collaborator{{UserID: uuid.New(), Role: model.RoleRead}}, u["alice"]), org.ErrNotFound)
		require.ErrorIs(t, o.SetCollaborators(ctx, uuid.New(), []org.Collaborator{{UserID: u["bob"].UserID, Role: model.RoleRead}}, u["alice"]), org.ErrNotFound)
		require.NoError(t, o.SetCollaborators(ctx, repo.ID, nil, u["alice"]))
		cs, err = o.Collaborators(ctx, repo.ID)
		require.NoError(t, err)
		require.Empty(t, cs)
		_, ok, err = o.CollaboratorRole(ctx, repo.ID, u["carol"].UserID)
		require.NoError(t, err)
		require.False(t, ok)
		require.Contains(t, rec.Actions(), audit.CollaboratorsChanged)
	})
}

func TestSyncOIDCMemberships(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		ctx := context.Background()
		_, o, _, u := setup(t, d)
		alice, bob := u["alice"], u["bob"]
		_, err := o.CreateOrg(ctx, "manual-org", "", bob)
		require.NoError(t, err)
		require.NoError(t, o.SetMember(ctx, "manual-org", alice.UserID, model.RoleRead, org.SourceManual, bob))

		want := []org.OIDCMapping{{Org: "ml", Role: model.RoleRead}, {Org: "ml", Role: model.RoleWrite}, {Org: "manual-org", Role: model.RoleAdmin}}
		require.NoError(t, o.SyncOIDCMemberships(ctx, alice.UserID, "kc", want, model.System()))
		m, err := o.Member(ctx, "ml", "alice")
		require.NoError(t, err, "a missing Organisation is created")
		require.Equal(t, model.RoleWrite, m.Role, "the highest mapped Role wins")
		require.Equal(t, org.SourceOIDC, m.Source)
		m, err = o.Member(ctx, "manual-org", "alice")
		require.NoError(t, err)
		require.Equal(t, model.RoleRead, m.Role, "manual Memberships are never touched")

		require.NoError(t, o.SyncOIDCMemberships(ctx, alice.UserID, "kc", []org.OIDCMapping{{Org: "ml", Role: model.RoleAdmin}}, model.System()))
		m, _ = o.Member(ctx, "ml", "alice")
		require.Equal(t, model.RoleAdmin, m.Role, "re-evaluated at every login")

		require.NoError(t, o.SyncOIDCMemberships(ctx, alice.UserID, "other-idp", nil, model.System()))
		_, err = o.Member(ctx, "ml", "alice")
		require.NoError(t, err, "another provider's sync leaves this provider's Memberships")

		require.NoError(t, o.SyncOIDCMemberships(ctx, alice.UserID, "kc", nil, model.System()))
		_, err = o.Member(ctx, "ml", "alice")
		require.ErrorIs(t, err, org.ErrNotFound, "removed when the group goes away")
		_, err = o.Member(ctx, "manual-org", "alice")
		require.NoError(t, err)

		// A manual change takes over a mapped Membership.
		require.NoError(t, o.SyncOIDCMemberships(ctx, alice.UserID, "kc", []org.OIDCMapping{{Org: "ml", Role: model.RoleRead}}, model.System()))
		require.NoError(t, o.SetMember(ctx, "ml", alice.UserID, model.RoleWrite, org.SourceManual, bob))
		require.NoError(t, o.SyncOIDCMemberships(ctx, alice.UserID, "kc", nil, model.System()))
		m, err = o.Member(ctx, "ml", "alice")
		require.NoError(t, err)
		require.Equal(t, org.SourceManual, m.Source)

		require.Error(t, o.SyncOIDCMemberships(ctx, alice.UserID, "kc", []org.OIDCMapping{{Org: "bob", Role: model.RoleRead}}, model.System()),
			"a mapping to a user Namespace fails")
	})
}

func TestSearchNamespaces(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		ctx := context.Background()
		_, o, _, u := setup(t, d)
		_, err := o.CreateOrg(ctx, "ML-Team", "Machine learning", u["alice"])
		require.NoError(t, err)
		_, err = o.CreateOrg(ctx, "under_score", "", u["alice"])
		require.NoError(t, err)

		names := func(q string, limit int) []string {
			t.Helper()
			nss, err := o.SearchNamespaces(ctx, q, limit)
			require.NoError(t, err)
			out := []string{}
			for _, ns := range nss {
				out = append(out, ns.Name)
			}
			return out
		}
		require.Equal(t, []string{"ML-Team"}, names("ml-", 10), "names match without case")
		require.Equal(t, []string{"ML-Team"}, names("LEARNING", 10), "display names match too")
		require.Equal(t, []string{"alice", "bob", "carol"}, names("", 10)[:3], "name order")
		require.Len(t, names("", 2), 2, "the limit applies")
		require.Equal(t, []string{"under_score"}, names("_", 10), "LIKE wildcards are literal")
		require.Empty(t, names("%", 10))
		ns, err := o.SearchNamespaces(ctx, "carol", 10)
		require.NoError(t, err)
		require.Len(t, ns, 1)
		require.Equal(t, u["carol"].UserID, ns[0].UserID)
	})
}
