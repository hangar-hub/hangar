package auth_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/auth"
	"github.com/jamesagarside/hangar/internal/auth/authtest"
	"github.com/jamesagarside/hangar/internal/config"
	"github.com/jamesagarside/hangar/internal/db"
	"github.com/jamesagarside/hangar/internal/db/dbtest"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/org"
)

// insertRepo adds a repos row for permission tests. Only internal/repo
// writes this table in production.
func insertRepo(t *testing.T, d *db.DB, nsID uuid.UUID, ns, name string, vis model.Visibility, gated bool) *auth.RepoTarget {
	t.Helper()
	gating := map[string]any{}
	if gated {
		gating["mode"] = "manual"
	}
	row := &db.Repo{ID: db.NewID(), NamespaceID: nsID, Name: name, Type: string(model.RepoTypeModel), Visibility: string(vis),
		DefaultBranch: "main", HeadRef: "refs/heads/main", Tags: []string{}, Settings: map[string]any{}, Gating: gating}
	_, err := d.NewInsert().Model(row).Exec(context.Background())
	require.NoError(t, err)
	return &auth.RepoTarget{ID: model.RepoID{Type: model.RepoTypeModel, Namespace: ns, Name: name}, UUID: row.ID,
		NamespaceID: nsID, Visibility: vis, Gated: gated}
}

// outcome letters in the matrix: A allowed, N not found (404), F
// forbidden (403), G gated.
func outcome(err error) string {
	switch {
	case err == nil:
		return "A"
	case errors.Is(err, auth.ErrNotFound):
		return "N"
	case errors.Is(err, auth.ErrGated):
		return "G"
	case errors.Is(err, auth.ErrForbidden):
		return "F"
	default:
		return "?" + err.Error()
	}
}

// repoActions are the columns of the permission table in 05-auth.md, in
// the order the matrix strings use.
var repoActions = []auth.Action{auth.ActionRead, auth.ActionReadFiles, auth.ActionPullMirror, auth.ActionWrite, auth.ActionDiscuss, auth.ActionAdmin, auth.ActionHubAdmin}

type matrixWorld struct {
	s       *auth.Service
	orgs    *org.DBService
	acme    *org.Namespace
	repos   map[model.Visibility]*auth.RepoTarget
	persons map[string]model.Principal
}

func buildWorld(t *testing.T, d *db.DB, anonymousRead bool) *matrixWorld {
	t.Helper()
	ctx := context.Background()
	s, orgs := authtest.NewService(t, d, authtest.RealOptions{Config: func(c *config.Config) { c.Security.AnonymousRead = anonymousRead }})
	w := &matrixWorld{s: s, orgs: orgs, repos: map[model.Visibility]*auth.RepoTarget{}, persons: map[string]model.Principal{}}
	user := func(name string, admin bool) model.Principal {
		u := mustUser(t, s, name, admin)
		return u.Principal(model.AuthSession)
	}
	owner := user("owner", false)
	acme, err := orgs.CreateOrg(ctx, "acme", "Acme", owner)
	require.NoError(t, err)
	w.acme = acme
	for _, v := range []model.Visibility{model.VisibilityPublic, model.VisibilityInternal, model.VisibilityPrivate} {
		w.repos[v] = insertRepo(t, d, acme.ID, "acme", "repo-"+string(v), v, false)
	}
	w.persons["anonymous"] = model.Anonymous()
	w.persons["outsider"] = user("outsider", false)
	for _, r := range []model.Role{model.RoleRead, model.RoleWrite, model.RoleAdmin} {
		p := user("member-"+string(r), false)
		require.NoError(t, orgs.SetMember(ctx, "acme", p.UserID, r, org.SourceManual, owner))
		w.persons["member-"+string(r)] = p
	}
	collabRead, collabWrite := user("collab-read", false), user("collab-write", false)
	for _, rt := range w.repos {
		require.NoError(t, orgs.SetCollaborators(ctx, rt.UUID, []org.Collaborator{
			{UserID: collabRead.UserID, Role: model.RoleRead}, {UserID: collabWrite.UserID, Role: model.RoleWrite}}, owner))
	}
	w.persons["collab-read"], w.persons["collab-write"] = collabRead, collabWrite
	w.persons["instance-admin"] = user("root", true)

	withToken := func(p model.Principal, scope model.Scope, r model.TokenRestrictions) model.Principal {
		p.Method = model.AuthToken
		p.Token = &model.TokenInfo{ID: uuid.New(), Name: "t", Scope: scope, Restrictions: r}
		return p
	}
	w.persons["member-write+read-token"] = withToken(w.persons["member-write"], model.ScopeRead, model.TokenRestrictions{})
	w.persons["member-admin+write-token"] = withToken(w.persons["member-admin"], model.ScopeWrite, model.TokenRestrictions{})
	w.persons["member-admin+admin-token"] = withToken(w.persons["member-admin"], model.ScopeAdmin, model.TokenRestrictions{})
	w.persons["member-write+token-other-ns"] = withToken(w.persons["member-write"], model.ScopeWrite, model.TokenRestrictions{Namespaces: []string{"elsewhere"}})
	w.persons["member-write+token-acme"] = withToken(w.persons["member-write"], model.ScopeWrite, model.TokenRestrictions{Namespaces: []string{"ACME"}})
	w.persons["member-write+token-one-repo"] = withToken(w.persons["member-write"], model.ScopeWrite,
		model.TokenRestrictions{Repos: []string{"model:acme/repo-private"}})
	ollama := w.persons["member-read"]
	ollama.Method = model.AuthOllamaKey
	ollama.Token = &model.TokenInfo{Name: "ollama", Scope: model.ScopeRead, Restrictions: model.TokenRestrictions{Repos: []string{"model:acme/repo-internal"}}}
	w.persons["ollama-registry-token"] = ollama
	return w
}

// TestPermissionMatrix checks every cell of the permission table in
// docs/spec/05-auth.md for every kind of caller, including Token scopes
// and restrictions, on SQLite and Postgres.
func TestPermissionMatrix(t *testing.T) {
	// persona -> visibility -> outcomes for read, read_files, pull_mirror,
	// write, discuss, admin, hub_admin. A write Token gets hub_admin (the
	// Hub API's delete, settings and same-Namespace move) on Repos its user
	// administers, and never admin (decided by James, 2026-10-17).
	type row map[model.Visibility]string
	pub, internal, private := model.VisibilityPublic, model.VisibilityInternal, model.VisibilityPrivate
	matrix := map[bool]map[string]row{
		true: { // anonymous_read on
			"anonymous":                   {pub: "AAFFFFF", internal: "NNNNNNN", private: "NNNNNNN"},
			"outsider":                    {pub: "AAFFAFF", internal: "AAFFAFF", private: "NNNNNNN"},
			"member-read":                 {pub: "AAFFAFF", internal: "AAFFAFF", private: "AAFFAFF"},
			"member-write":                {pub: "AAAAAFF", internal: "AAAAAFF", private: "AAAAAFF"},
			"member-admin":                {pub: "AAAAAAA", internal: "AAAAAAA", private: "AAAAAAA"},
			"collab-read":                 {pub: "AAFFAFF", internal: "AAFFAFF", private: "AAFFAFF"},
			"collab-write":                {pub: "AAFAAFF", internal: "AAFAAFF", private: "AAFAAFF"},
			"instance-admin":              {pub: "AAAAAAA", internal: "AAAAAAA", private: "AAAAAAA"},
			"member-write+read-token":     {pub: "AAFFFFF", internal: "AAFFFFF", private: "AAFFFFF"},
			"member-admin+write-token":    {pub: "AAAAAFA", internal: "AAAAAFA", private: "AAAAAFA"},
			"member-admin+admin-token":    {pub: "AAAAAAA", internal: "AAAAAAA", private: "AAAAAAA"},
			"member-write+token-other-ns": {pub: "NNNNNNN", internal: "NNNNNNN", private: "NNNNNNN"},
			"member-write+token-acme":     {pub: "AAAAAFF", internal: "AAAAAFF", private: "AAAAAFF"},
			"member-write+token-one-repo": {pub: "NNNNNNN", internal: "NNNNNNN", private: "AAAAAFF"},
			"ollama-registry-token":       {pub: "NNNNNNN", internal: "AAFFFFF", private: "NNNNNNN"},
		},
		false: { // anonymous_read off: only the anonymous row changes
			"anonymous": {pub: "NNNNNNN", internal: "NNNNNNN", private: "NNNNNNN"},
			"outsider":  {pub: "AAFFAFF", internal: "AAFFAFF", private: "NNNNNNN"},
		},
	}
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		for _, anonRead := range []bool{true, false} {
			w := buildWorld(t, d, anonRead)
			cells := 0
			for persona, rows := range matrix[anonRead] {
				p, ok := w.persons[persona]
				require.True(t, ok, persona)
				for vis, want := range rows {
					got := ""
					for _, a := range repoActions {
						got += outcome(w.s.Authorizer.Can(context.Background(), p, a, auth.Target{Repo: w.repos[vis]}))
						cells++
					}
					require.Equal(t, want, got, "anonymous_read=%v persona=%s visibility=%s (read, read_files, pull_mirror, write, discuss, admin, hub_admin)", anonRead, persona, vis)
				}
			}
			t.Logf("anonymous_read=%v: %d cells checked", anonRead, cells)
			// Fresh database per setting: the users would clash.
			d = dbtestReopen(t, d)
		}
	})
}

// dbtestReopen gives the next iteration a fresh database of the same driver.
func dbtestReopen(t *testing.T, d *db.DB) *db.DB {
	t.Helper()
	if d.IsSQLite() {
		return dbtest.SQLite(t)
	}
	return dbtest.Postgres(t)
}

func TestPermissionsGatedRepos(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		ctx := context.Background()
		w := buildWorld(t, d, true)
		gated := insertRepo(t, d, w.acme.ID, "acme", "gated", model.VisibilityPublic, true)
		require.NoError(t, w.orgs.SetCollaborators(ctx, gated.UUID, []org.Collaborator{{UserID: w.persons["collab-read"].UserID, Role: model.RoleRead}}, model.System()))
		can := func(p string, a auth.Action) string {
			return outcome(w.s.Authorizer.Can(ctx, w.persons[p], a, auth.Target{Repo: gated}))
		}
		require.Equal(t, "A", can("anonymous", auth.ActionRead), "metadata stays readable")
		require.Equal(t, "G", can("anonymous", auth.ActionReadFiles))
		require.Equal(t, "A", can("outsider", auth.ActionRead))
		require.Equal(t, "G", can("outsider", auth.ActionReadFiles))
		for _, p := range []string{"member-read", "collab-read", "instance-admin"} {
			require.Equal(t, "A", can(p, auth.ActionReadFiles), "%s has a Role on the Repo", p)
		}
		g := auth.GatedRepo{UUID: gated.UUID, ID: gated.ID, Mode: auth.GatingManual}
		req, err := w.s.AccessRequests.Request(ctx, g, w.persons["outsider"], nil)
		require.NoError(t, err)
		require.Equal(t, auth.AccessPending, req.Status)
		require.Equal(t, "G", can("outsider", auth.ActionReadFiles), "pending is not enough")
		_, err = w.s.AccessRequests.Decide(ctx, g, req.ID, auth.AccessAccepted, w.persons["member-admin"])
		require.NoError(t, err)
		require.Equal(t, "A", can("outsider", auth.ActionReadFiles))
		_, err = w.s.AccessRequests.Decide(ctx, g, req.ID, auth.AccessRejected, w.persons["member-admin"])
		require.NoError(t, err)
		require.Equal(t, "G", can("outsider", auth.ActionReadFiles), "a later rejection revokes access")
	})
}

// TestPermissionsPullMirror checks that pulling more files into a Mirror
// needs write on the Namespace, as docs/spec/05-auth.md says, and that a
// Repo collaborator Role never counts towards it.
func TestPermissionsPullMirror(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		ctx := context.Background()
		w := buildWorld(t, d, true)
		priv := w.repos[model.VisibilityPrivate]
		pull := func(p model.Principal, r *auth.RepoTarget) error {
			return w.s.Authorizer.Can(ctx, p, auth.ActionPullMirror, auth.Target{Repo: r})
		}

		// A write collaborator can push to the Repo but cannot pull into it.
		collab := w.persons["collab-write"]
		require.NoError(t, w.s.Authorizer.Can(ctx, collab, auth.ActionWrite, auth.Target{Repo: priv}))
		err := pull(collab, priv)
		require.ErrorIs(t, err, auth.ErrForbidden)
		require.Contains(t, err.Error(), "write role in the acme Namespace")

		// A read member who is also a write collaborator still cannot pull:
		// the Namespace Role is the one that counts.
		require.NoError(t, w.orgs.SetMember(ctx, "acme", collab.UserID, model.RoleRead, org.SourceManual, w.persons["member-admin"]))
		require.ErrorIs(t, pull(collab, priv), auth.ErrForbidden)
		require.NoError(t, w.orgs.SetMember(ctx, "acme", collab.UserID, model.RoleWrite, org.SourceManual, w.persons["member-admin"]))
		require.NoError(t, pull(collab, priv), "write on the Namespace is enough")

		// Instance admins, the system user and the owner of a personal
		// Namespace can pull.
		require.NoError(t, pull(w.persons["instance-admin"], priv))
		require.NoError(t, pull(model.System(), priv))
		outsiderNS, err := w.orgs.Get(ctx, "outsider")
		require.NoError(t, err)
		own := insertRepo(t, d, outsiderNS.ID, "outsider", "mirror", model.VisibilityPrivate, false)
		require.NoError(t, pull(w.persons["outsider"], own))

		// A write collaborator on someone's personal Repo cannot pull into it.
		guest := w.persons["collab-read"]
		require.NoError(t, w.orgs.SetCollaborators(ctx, own.UUID, []org.Collaborator{{UserID: guest.UserID, Role: model.RoleWrite}}, w.persons["outsider"]))
		require.NoError(t, w.s.Authorizer.Can(ctx, guest, auth.ActionWrite, auth.Target{Repo: own}))
		require.ErrorIs(t, pull(guest, own), auth.ErrForbidden)

		// Anonymous callers are told to sign in.
		err = pull(model.Anonymous(), w.repos[model.VisibilityPublic])
		require.ErrorIs(t, err, auth.ErrForbidden)
		require.Contains(t, err.Error(), "sign in")
	})
}

// TestNamespaceReadFollowsAnonymousRead checks James's decision of
// 2026-10-18: reading a Namespace (its profile, members and Collections)
// needs a signed-in caller while security.anonymous_read is off, and is
// open to anyone while it is on. The refusal is the same whether or not
// the Namespace exists.
func TestNamespaceReadFollowsAnonymousRead(t *testing.T) {
	for _, anonymousRead := range []bool{false, true} {
		t.Run(fmt.Sprintf("anonymous_read=%t", anonymousRead), func(t *testing.T) {
			dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
				ctx := context.Background()
				w := buildWorld(t, d, anonymousRead)
				targets := map[string]*auth.NamespaceTarget{
					"acme":    {ID: w.acme.ID, Name: "acme", Kind: org.KindOrg},
					"by name": {Name: "acme"},
					"missing": {Name: "nope"},
					"empty":   {},
				}
				for name, ns := range targets {
					err := w.s.Authorizer.Can(ctx, model.Anonymous(), auth.ActionRead, auth.Target{Namespace: ns})
					if anonymousRead {
						require.NoError(t, err, "anonymous caller on %s", name)
					} else {
						require.ErrorIs(t, err, auth.ErrForbidden, "anonymous caller on %s", name)
						require.Contains(t, err.Error(), "sign in")
					}
					for _, persona := range []string{"outsider", "member-write+read-token", "member-write+token-other-ns", "ollama-registry-token", "instance-admin"} {
						require.NoError(t, w.s.Authorizer.Can(ctx, w.persons[persona], auth.ActionRead, auth.Target{Namespace: ns}), "%s on %s", persona, name)
					}
				}
			})
		})
	}
}

func TestPermissionsNamespacesAndInstance(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		ctx := context.Background()
		w := buildWorld(t, d, true)
		acme := &auth.NamespaceTarget{ID: w.acme.ID, Name: "acme", Kind: org.KindOrg}
		outsiderNS, err := w.orgs.Get(ctx, "outsider")
		require.NoError(t, err)
		personal := &auth.NamespaceTarget{ID: outsiderNS.ID, Name: "outsider", Kind: org.KindUser}
		cases := []struct {
			persona string
			ns      *auth.NamespaceTarget
			want    string // create_repo, manage_namespace, read
		}{
			{"anonymous", acme, "FFA"},
			{"outsider", acme, "FFA"},
			{"member-read", acme, "FFA"},
			{"member-write", acme, "AFA"},
			{"member-admin", acme, "AAA"},
			{"collab-write", acme, "FFA"},
			{"instance-admin", acme, "AAA"},
			{"member-write+read-token", acme, "FFA"},
			{"member-admin+write-token", acme, "AFA"},
			{"member-admin+admin-token", acme, "AAA"},
			{"member-write+token-other-ns", acme, "FFA"},
			{"member-write+token-acme", acme, "AFA"},
			{"outsider", personal, "AAA"},
			{"member-admin", personal, "FFA"},
			{"instance-admin", personal, "AAA"},
		}
		for _, c := range cases {
			got := ""
			for _, a := range []auth.Action{auth.ActionCreateRepo, auth.ActionManageNamespace, auth.ActionRead} {
				got += outcome(w.s.Authorizer.Can(ctx, w.persons[c.persona], a, auth.Target{Namespace: c.ns}))
			}
			require.Equal(t, c.want, got, "%s on %s", c.persona, c.ns.Name)
		}
		// A Namespace target by name alone.
		require.NoError(t, w.s.Authorizer.Can(ctx, w.persons["member-write"], auth.ActionCreateRepo, auth.Target{Namespace: &auth.NamespaceTarget{Name: "acme"}}))
		require.ErrorIs(t, w.s.Authorizer.Can(ctx, w.persons["member-write"], auth.ActionCreateRepo, auth.Target{Namespace: &auth.NamespaceTarget{Name: "nope"}}), auth.ErrNotFound)

		for persona, want := range map[string]string{
			"anonymous": "F", "outsider": "F", "member-admin": "F", "instance-admin": "A",
		} {
			require.Equal(t, want, outcome(w.s.Authorizer.Can(ctx, w.persons[persona], auth.ActionInstanceAdmin, auth.Target{})), persona)
		}
		admin := w.persons["instance-admin"]
		for _, tc := range []struct {
			scope model.Scope
			restr model.TokenRestrictions
			want  string
		}{
			{model.ScopeAdmin, model.TokenRestrictions{}, "A"},
			{model.ScopeWrite, model.TokenRestrictions{}, "F"},
			{model.ScopeAdmin, model.TokenRestrictions{Namespaces: []string{"acme"}}, "F"},
		} {
			p := admin
			p.Token = &model.TokenInfo{Scope: tc.scope, Restrictions: tc.restr}
			require.Equal(t, tc.want, outcome(w.s.Authorizer.Can(ctx, p, auth.ActionInstanceAdmin, auth.Target{})), fmt.Sprint(tc))
		}
		require.Equal(t, "F", outcome(w.s.Authorizer.Can(ctx, model.System(), "bogus", auth.Target{})))
		require.Equal(t, "F", outcome(w.s.Authorizer.Can(ctx, admin, auth.ActionWrite, auth.Target{})), "no target")
		require.Equal(t, "A", outcome(w.s.Authorizer.Can(ctx, model.System(), auth.ActionAdmin, auth.Target{Repo: w.repos[model.VisibilityPrivate]})), "the system user")
	})
}
