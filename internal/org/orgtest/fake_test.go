package orgtest_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/model"
	"github.com/hangar-hub/hangar/internal/org"
	"github.com/hangar-hub/hangar/internal/org/orgtest"
)

func TestFakeOrganisations(t *testing.T) {
	ctx := context.Background()
	f := orgtest.NewFake()
	alice, err := f.AddUser("alice")
	require.NoError(t, err)
	bob, err := f.AddUser("bob")
	require.NoError(t, err)
	_, err = f.AddUser("Alice")
	require.ErrorIs(t, err, org.ErrExists, "names are unique case-insensitively")
	_, err = f.AddUser("x")
	require.ErrorIs(t, err, org.ErrInvalid)

	ml, err := f.CreateOrg(ctx, "ml-team", "", alice)
	require.NoError(t, err)
	require.Equal(t, "ml-team", ml.DisplayName)
	_, err = f.CreateOrg(ctx, "api", "", alice)
	require.ErrorIs(t, err, org.ErrInvalid, "reserved names are refused")
	_, err = f.CreateOrg(ctx, "bob", "", alice)
	require.ErrorIs(t, err, org.ErrExists, "users and Organisations share one name space")
	_, err = f.CreateOrg(ctx, "anon-org", "", model.Anonymous())
	require.ErrorIs(t, err, org.ErrInvalid)

	require.NoError(t, f.SetMember(ctx, "ml-team", bob.UserID, model.RoleWrite, "", alice))
	require.ErrorIs(t, f.SetMember(ctx, "ml-team", uuid.New(), model.RoleRead, "", alice), org.ErrNotFound)
	require.ErrorIs(t, f.SetMember(ctx, "ml-team", bob.UserID, "owner", "", alice), org.ErrInvalid)
	members, err := f.Members(ctx, "ml-team")
	require.NoError(t, err)
	require.Len(t, members, 2)
	require.Equal(t, "alice", members[0].Username)
	require.Equal(t, org.SourceManual, members[1].Source)

	require.ErrorIs(t, f.SetMember(ctx, "ml-team", alice.UserID, model.RoleWrite, "", alice), org.ErrLastAdmin)
	require.ErrorIs(t, f.RemoveMember(ctx, "ml-team", alice.UserID, alice), org.ErrLastAdmin)
	require.NoError(t, f.RemoveMember(ctx, "ml-team", bob.UserID, alice))
	require.ErrorIs(t, f.RemoveMember(ctx, "ml-team", bob.UserID, alice), org.ErrNotFound)

	role, ok, err := f.RoleIn(ctx, ml.ID, alice.UserID)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, model.RoleAdmin, role)
	_, ok, err = f.RoleIn(ctx, ml.ID, bob.UserID)
	require.NoError(t, err)
	require.False(t, ok)
	role, ok, err = f.RoleIn(ctx, bob.NamespaceID, bob.UserID)
	require.NoError(t, err)
	require.True(t, ok, "users are admins of their own Namespace")
	require.Equal(t, model.RoleAdmin, role)
	_, ok, err = f.RoleIn(ctx, bob.NamespaceID, alice.UserID)
	require.NoError(t, err)
	require.False(t, ok)
	_, _, err = f.RoleIn(ctx, uuid.New(), alice.UserID)
	require.ErrorIs(t, err, org.ErrNotFound)

	ms, err := f.MembershipsOf(ctx, alice.UserID)
	require.NoError(t, err)
	require.Len(t, ms, 1)
	require.Equal(t, "ml-team", ms[0].Org.Name)

	desc := "Machine learning"
	updated, err := f.UpdateOrg(ctx, "ML-TEAM", org.OrgUpdate{Description: &desc}, alice)
	require.NoError(t, err)
	require.Equal(t, desc, updated.Description)
	_, err = f.UpdateOrg(ctx, "alice", org.OrgUpdate{}, alice)
	require.ErrorIs(t, err, org.ErrNotFound, "user Namespaces are not Organisations")

	got, err := f.Get(ctx, "Ml-Team")
	require.NoError(t, err)
	require.Equal(t, ml.ID, got.ID)
	byID, err := f.GetByID(ctx, ml.ID)
	require.NoError(t, err)
	require.Equal(t, "ml-team", byID.Name)
	_, err = f.Get(ctx, "nobody")
	require.ErrorIs(t, err, org.ErrNotFound)
	_, err = f.GetByID(ctx, uuid.New())
	require.ErrorIs(t, err, org.ErrNotFound)

	require.NoError(t, f.DeleteOrg(ctx, "ml-team", alice))
	_, err = f.Members(ctx, "ml-team")
	require.ErrorIs(t, err, org.ErrNotFound)
}

func TestFakeMirrorNamespacesAndCollaborators(t *testing.T) {
	ctx := context.Background()
	f := orgtest.NewFake()
	alice, err := f.AddUser("alice")
	require.NoError(t, err)

	ns, err := f.EnsureMirrorNamespace(ctx, "jinaai")
	require.NoError(t, err)
	require.True(t, ns.ReservedMirror)
	again, err := f.EnsureMirrorNamespace(ctx, "JinaAI")
	require.NoError(t, err)
	require.Equal(t, ns.ID, again.ID)
	_, err = f.EnsureMirrorNamespace(ctx, "alice")
	require.ErrorIs(t, err, org.ErrExists, "local Namespaces are not taken over")
	_, err = f.EnsureMirrorNamespace(ctx, "!")
	require.ErrorIs(t, err, org.ErrInvalid)

	repo := uuid.New()
	require.NoError(t, f.SetCollaborators(ctx, repo, []org.Collaborator{{UserID: alice.UserID, Role: model.RoleWrite}}, alice))
	cs, err := f.Collaborators(ctx, repo)
	require.NoError(t, err)
	require.Equal(t, []org.Collaborator{{UserID: alice.UserID, Username: "alice", Role: model.RoleWrite}}, cs)
	require.ErrorIs(t, f.SetCollaborators(ctx, repo, []org.Collaborator{{UserID: alice.UserID, Role: model.RoleAdmin}}, alice), org.ErrInvalid)
	require.ErrorIs(t, f.SetCollaborators(ctx, repo, []org.Collaborator{{UserID: uuid.New(), Role: model.RoleRead}}, alice), org.ErrNotFound)
}
