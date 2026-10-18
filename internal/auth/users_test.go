package auth_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/audit"
	"github.com/jamesagarside/hangar/internal/audit/audittest"
	"github.com/jamesagarside/hangar/internal/auth"
	"github.com/jamesagarside/hangar/internal/auth/authtest"
	"github.com/jamesagarside/hangar/internal/config"
	"github.com/jamesagarside/hangar/internal/db"
	"github.com/jamesagarside/hangar/internal/db/dbtest"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/org"
)

func TestUsers(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		ctx := context.Background()
		rec := audittest.NewFake()
		s, orgs := authtest.NewService(t, d, authtest.RealOptions{Audit: rec})
		admin := mustUser(t, s, "root", true).Principal(model.AuthSession)

		u, err := s.Users.Create(ctx, auth.NewUser{Username: "Alice", Email: "alice@example.com", DisplayName: "Alice A"}, admin)
		require.NoError(t, err)
		require.Equal(t, "Alice", u.Username)
		require.Equal(t, "Alice A", u.DisplayName)
		require.False(t, u.HasPassword)
		require.Contains(t, rec.Actions(), audit.UserCreated)
		ns, err := orgs.Get(ctx, "alice")
		require.NoError(t, err)
		require.Equal(t, org.KindUser, ns.Kind)
		require.Equal(t, u.ID, ns.UserID)

		for name, nu := range map[string]auth.NewUser{
			"reserved":      {Username: "admin"},
			"bad chars":     {Username: "a b"},
			"too short":     {Username: "a"},
			"leading dot":   {Username: ".hidden"},
			"bad email":     {Username: "bob", Email: "not an email"},
			"weak password": {Username: "bob", Password: "password123"},
			"long display":  {Username: "bob", DisplayName: strings.Repeat("x", 101)},
		} {
			_, err := s.Users.Create(ctx, nu, admin)
			require.ErrorIs(t, err, auth.ErrInvalid, name)
		}
		_, err = s.Users.Create(ctx, auth.NewUser{Username: "ALICE"}, admin)
		require.ErrorIs(t, err, auth.ErrConflict, "usernames are unique case-insensitively")
		_, err = s.Users.Create(ctx, auth.NewUser{Username: "alice2", Email: "ALICE@example.com"}, admin)
		require.ErrorIs(t, err, auth.ErrConflict, "emails are unique case-insensitively")
		_, err = orgs.CreateOrg(ctx, "acme", "", admin)
		require.NoError(t, err)
		_, err = s.Users.Create(ctx, auth.NewUser{Username: "acme"}, admin)
		require.ErrorIs(t, err, auth.ErrConflict, "users and Organisations share one name space")

		got, err := s.Users.GetByEmail(ctx, "Alice@Example.com")
		require.NoError(t, err)
		require.Equal(t, u.ID, got.ID)
		_, err = s.Users.GetByEmail(ctx, "")
		require.ErrorIs(t, err, auth.ErrNotFound)
		_, err = s.Users.Get(ctx, uuid.New())
		require.ErrorIs(t, err, auth.ErrNotFound)

		// Listing, search and paging.
		for _, n := range []string{"carol", "dave", "erin"} {
			mustUser(t, s, n, false)
		}
		page, next, err := s.Users.List(ctx, "", "", 2)
		require.NoError(t, err)
		require.Len(t, page, 2)
		require.Equal(t, []string{"Alice", "carol"}, []string{page[0].Username, page[1].Username})
		require.NotEmpty(t, next)
		page, next, err = s.Users.List(ctx, "", next, 2)
		require.NoError(t, err)
		require.Equal(t, []string{"dave", "erin"}, []string{page[0].Username, page[1].Username})
		page, _, err = s.Users.List(ctx, "", next, 2)
		require.NoError(t, err)
		require.Equal(t, "root", page[0].Username)
		page, _, err = s.Users.List(ctx, "EXAMPLE.com", "", 10)
		require.NoError(t, err)
		require.Len(t, page, 1, "search covers email")
		page, _, err = s.Users.List(ctx, "100%_", "", 10)
		require.NoError(t, err)
		require.Empty(t, page, "LIKE wildcards are escaped")
		_, _, err = s.Users.List(ctx, "", "!!", 10)
		require.ErrorIs(t, err, auth.ErrInvalid)
		n, err := s.Users.Count(ctx)
		require.NoError(t, err)
		require.Equal(t, int64(5), n)

		// Updates and their audit events.
		yes, no := true, false
		email, name := "a@example.org", "Alice B"
		u2, err := s.Users.Update(ctx, u.ID, auth.UserUpdate{IsAdmin: &yes, Email: &email, DisplayName: &name}, admin)
		require.NoError(t, err)
		require.True(t, u2.IsAdmin)
		require.Equal(t, email, u2.Email)
		require.Equal(t, name, u2.DisplayName)
		_, err = s.Users.Update(ctx, u.ID, auth.UserUpdate{Disabled: &yes}, admin)
		require.NoError(t, err, "another admin remains")
		_, err = s.Users.Update(ctx, u.ID, auth.UserUpdate{Disabled: &no}, admin)
		require.NoError(t, err)
		bad := "nope"
		_, err = s.Users.Update(ctx, u.ID, auth.UserUpdate{Email: &bad}, admin)
		require.ErrorIs(t, err, auth.ErrInvalid)
		empty := ""
		u3, err := s.Users.Update(ctx, u.ID, auth.UserUpdate{Email: &empty}, admin)
		require.NoError(t, err)
		require.Empty(t, u3.Email)
		actions := rec.Actions()
		require.Contains(t, actions, audit.UserAdminChanged)
		require.Contains(t, actions, audit.UserDisabled)
		require.Contains(t, actions, audit.UserEnabled)
	})
}

func TestInvitesRegistrationAndResets(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		ctx := context.Background()
		clock := authtest.NewClock(start())
		rec := audittest.NewFake()
		s, orgs := authtest.NewService(t, d, authtest.RealOptions{Clock: clock, Audit: rec})
		admin := mustUser(t, s, "root", true).Principal(model.AuthSession)
		_, err := orgs.CreateOrg(ctx, "acme", "", admin)
		require.NoError(t, err)

		// Registration is closed by default.
		_, err = s.Register(ctx, auth.Registration{Username: "walkin", Password: goodPassword})
		require.ErrorIs(t, err, auth.ErrRegistrationClosed)
		reg := "invite"
		_, err = s.Settings.Update(ctx, auth.SettingsPatch{Registration: &reg}, admin)
		require.NoError(t, err)
		_, err = s.Register(ctx, auth.Registration{Username: "walkin", Password: goodPassword})
		require.ErrorIs(t, err, auth.ErrRegistrationClosed, "invite mode needs an invite")

		link, err := s.CreateInvite(ctx, auth.InviteParams{Email: "new@example.com", Org: "acme", Role: model.RoleWrite}, admin)
		require.NoError(t, err)
		require.True(t, strings.HasPrefix(link.URL, "http://hangar.test/auth/invite/"))
		require.Equal(t, clock.Now().Add(72*time.Hour), link.ExpiresAt)
		code := strings.TrimPrefix(link.URL, "http://hangar.test/auth/invite/")
		for _, e := range rec.Events() {
			for _, v := range e.Data {
				require.NotContains(t, fmt.Sprint(v), code, "the invite code is never in the audit log")
			}
		}

		_, err = s.Register(ctx, auth.Registration{Username: "newbie", Password: "short", InviteCode: code})
		require.ErrorIs(t, err, auth.ErrInvalid)
		u, err := s.Register(ctx, auth.Registration{Username: "newbie", Password: goodPassword, Email: "ignored@example.com", InviteCode: code})
		require.NoError(t, err, "a failed attempt does not use up the invite")
		require.Equal(t, "new@example.com", u.Email, "the invite's email wins")
		require.True(t, u.EmailVerified)
		m, err := orgs.Member(ctx, "acme", "newbie")
		require.NoError(t, err)
		require.Equal(t, model.RoleWrite, m.Role)
		_, err = s.Register(ctx, auth.Registration{Username: "second", Password: goodPassword, InviteCode: code})
		require.ErrorIs(t, err, auth.ErrLinkInvalid, "single use")

		adminLink, err := s.CreateInvite(ctx, auth.InviteParams{MakeAdmin: true}, admin)
		require.NoError(t, err)
		clock.Advance(72 * time.Hour)
		_, err = s.Register(ctx, auth.Registration{Username: "late", Password: goodPassword, InviteCode: strings.TrimPrefix(adminLink.URL, "http://hangar.test/auth/invite/")})
		require.ErrorIs(t, err, auth.ErrLinkInvalid, "expired after 72 hours")
		adminLink, err = s.CreateInvite(ctx, auth.InviteParams{MakeAdmin: true}, admin)
		require.NoError(t, err)
		boss, err := s.Register(ctx, auth.Registration{Username: "boss", Password: goodPassword, InviteCode: strings.TrimPrefix(adminLink.URL, "http://hangar.test/auth/invite/")})
		require.NoError(t, err)
		require.True(t, boss.IsAdmin)

		for name, p := range map[string]auth.InviteParams{
			"unknown org":   {Org: "nope"},
			"role, no org":  {Role: model.RoleRead},
			"bad role":      {Org: "acme", Role: "owner"},
			"bad email":     {Email: "x"},
			"user, not org": {Org: "root"},
		} {
			_, err := s.CreateInvite(ctx, p, admin)
			require.ErrorIs(t, err, auth.ErrInvalid, name)
		}

		open := "open"
		_, err = s.Settings.Update(ctx, auth.SettingsPatch{Registration: &open}, admin)
		require.NoError(t, err)
		_, err = s.Register(ctx, auth.Registration{Username: "walkin", Password: goodPassword})
		require.NoError(t, err)
		_, err = s.Register(ctx, auth.Registration{Username: "nopass"})
		require.ErrorIs(t, err, auth.ErrInvalid)

		// Password reset.
		sess, _, err := s.Sessions.Create(ctx, u.ID, "", "")
		require.NoError(t, err)
		first, err := s.CreateResetLink(ctx, u.ID, admin)
		require.NoError(t, err)
		reset, err := s.CreateResetLink(ctx, u.ID, admin)
		require.NoError(t, err)
		_, err = s.ResetPassword(ctx, strings.TrimPrefix(first.URL, "http://hangar.test/auth/reset/"), "another good password 7", "", "")
		require.ErrorIs(t, err, auth.ErrLinkInvalid, "a newer link replaces older ones")
		rcode := strings.TrimPrefix(reset.URL, "http://hangar.test/auth/reset/")
		_, err = s.ResetPassword(ctx, rcode, "password123", "", "")
		require.ErrorIs(t, err, auth.ErrInvalid)
		_, err = s.ResetPassword(ctx, rcode, "another good password 7", "", "")
		require.NoError(t, err)
		_, err = s.Sessions.Get(ctx, sess.ID)
		require.ErrorIs(t, err, auth.ErrNotFound, "a reset signs the user out everywhere")
		_, err = s.Login.Password(ctx, auth.LoginAttempt{Username: "newbie", Password: "another good password 7"})
		require.NoError(t, err)
		_, err = s.ResetPassword(ctx, rcode, "yet another password 8", "", "")
		require.ErrorIs(t, err, auth.ErrLinkInvalid, "single use")
		_, err = s.CreateResetLink(ctx, uuid.New(), admin)
		require.ErrorIs(t, err, auth.ErrNotFound)

		actions := rec.Actions()
		require.Contains(t, actions, audit.InviteCreated)
		require.Contains(t, actions, audit.UserPasswordReset)
		require.Contains(t, actions, audit.MembershipChanged)
	})
}

func TestSettings(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		ctx := context.Background()
		clock := authtest.NewClock(start())
		rec := audittest.NewFake()
		s, _ := authtest.NewService(t, d, authtest.RealOptions{Clock: clock, Audit: rec, Config: func(c *config.Config) {
			c.Pull.Concurrency = 3
			c.Pull.Windows = []string{"01:00-07:00"}
		}})
		st, err := s.Settings.Get(ctx)
		require.NoError(t, err)
		require.Equal(t, s.Settings.Defaults(), st)
		require.Equal(t, 3, st.PullConcurrency)
		require.Equal(t, "closed", st.Registration)
		require.True(t, st.UIExternalImages)
		require.True(t, st.WebhooksAllowPrivateTargets)
		require.Equal(t, auth.ScanBlockLevelUnsafe, st.ScanBlockLevel, "unsafe is the default level")

		yes, conc, wins, mr, level := true, 8, []string{}, "anonymous", auth.ScanBlockLevelSuspicious
		st, err = s.Settings.Update(ctx, auth.SettingsPatch{AnonymousRead: &yes, PullConcurrency: &conc, PullWindows: &wins, PullThroughMinRole: &mr,
			ScanBlockLevel: &level}, model.System())
		require.NoError(t, err)
		require.Equal(t, auth.ScanBlockLevelSuspicious, st.ScanBlockLevel)
		require.True(t, st.AnonymousRead)
		require.Equal(t, 8, st.PullConcurrency)
		require.Empty(t, st.PullWindows)
		require.Equal(t, "anonymous", st.PullThroughMinRole)
		require.Equal(t, audit.SettingsChanged, rec.Actions()[len(rec.Actions())-1])

		// A second Service on the same database (another replica) sees
		// the change once its cache expires.
		s2, _ := authtest.NewService(t, d, authtest.RealOptions{Clock: clock})
		st2, err := s2.Settings.Get(ctx)
		require.NoError(t, err)
		require.True(t, st2.AnonymousRead)

		no := false
		_, err = s.Settings.Update(ctx, auth.SettingsPatch{AnonymousRead: &no}, model.System())
		require.NoError(t, err)
		st2, _ = s2.Settings.Get(ctx)
		require.True(t, st2.AnonymousRead, "cached for a few seconds")
		clock.Advance(6 * time.Second)
		st2, _ = s2.Settings.Get(ctx)
		require.False(t, st2.AnonymousRead)

		badReg, badRole, neg, zero, badLevel := "maybe", "admin", int64(-1), 0, "error"
		badWin, badGlob := []string{"25:00-01:00"}, []string{"["}
		_, err = s.Settings.Update(ctx, auth.SettingsPatch{Registration: &badReg, PullThroughMinRole: &badRole, PullBandwidthLimit: &neg,
			PullConcurrency: &zero, PullWindows: &badWin, PullThroughAllow: &badGlob, ScanBlockLevel: &badLevel}, model.System())
		require.ErrorIs(t, err, auth.ErrInvalidSettings)
		for _, part := range []string{"registration", "pull_through_min_role", "pull_bandwidth_limit", "pull_concurrency", "pull_windows", "pull_through_allow", "scan_block_level"} {
			require.ErrorContains(t, err, part, "every error is reported")
		}
		before := len(rec.Actions())
		_, err = s.Settings.Update(ctx, auth.SettingsPatch{}, model.System())
		require.NoError(t, err)
		require.Len(t, rec.Actions(), before, "no change, no audit event")
	})
}

func TestSetPassword(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		ctx := context.Background()
		rec := audittest.NewFake()
		s, _ := authtest.NewService(t, d, authtest.RealOptions{Audit: rec})
		u, err := s.Users.Create(ctx, auth.NewUser{Username: "Locked", Password: "first good password 1"}, model.System())
		require.NoError(t, err)
		_, _, err = s.Sessions.Create(ctx, u.ID, "", "")
		require.NoError(t, err)
		for range 6 {
			_, err = s.Login.Password(ctx, auth.LoginAttempt{Username: "locked", Password: "wrong password here", IP: "10.0.0.9"})
			require.Error(t, err)
		}

		_, err = s.Users.SetPassword(ctx, u.ID, "short", model.System())
		require.ErrorIs(t, err, auth.ErrInvalid, "the password policy applies")
		_, err = s.Users.SetPassword(ctx, uuid.New(), "second good password 2", model.System())
		require.ErrorIs(t, err, auth.ErrNotFound)

		_, err = s.Users.SetPassword(ctx, u.ID, "second good password 2", model.System())
		require.NoError(t, err)
		require.Equal(t, audit.UserPasswordReset, rec.Actions()[len(rec.Actions())-1])
		sessions, err := s.Sessions.List(ctx, u.ID)
		require.NoError(t, err)
		require.Empty(t, sessions, "the User is signed out everywhere")
		_, err = s.Login.Password(ctx, auth.LoginAttempt{Username: "locked", Password: "second good password 2", IP: "10.0.0.10"})
		require.NoError(t, err, "the throttle is cleared and the new password works")
	})
}
