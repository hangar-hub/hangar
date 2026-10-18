package auth_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/audit"
	"github.com/hangar-hub/hangar/internal/audit/audittest"
	"github.com/hangar-hub/hangar/internal/auth"
	"github.com/hangar-hub/hangar/internal/auth/authtest"
	"github.com/hangar-hub/hangar/internal/config"
	"github.com/hangar-hub/hangar/internal/db"
	"github.com/hangar-hub/hangar/internal/db/dbtest"
	"github.com/hangar-hub/hangar/internal/model"
)

// TestChangePassword covers auth.Service.ChangePassword (W8 8): only a
// session may change a password, the current one must be right and the
// new one must meet the policy, wrong ones feed the sign-in throttle, and
// success ends every other session and resets the username's throttle.
func TestChangePassword(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		ctx := context.Background()
		rec := audittest.NewFake()
		s, _ := authtest.NewService(t, d, authtest.RealOptions{Audit: rec})
		const first, second = "first good password 1", "second good password 2"
		u, err := s.Users.Create(ctx, auth.NewUser{Username: "carol", Password: first}, model.System())
		require.NoError(t, err)
		current, _, err := s.Sessions.Create(ctx, u.ID, "", "")
		require.NoError(t, err)
		_, _, err = s.Sessions.Create(ctx, u.ID, "", "")
		require.NoError(t, err)
		p := u.Principal(model.AuthSession)
		p.SessionID = current.ID
		ip := "10.0.0.7"
		change := func(p model.Principal, cur, next string) error {
			return s.ChangePassword(ctx, p, auth.PasswordChange{Current: cur, New: next, IP: ip})
		}

		require.ErrorIs(t, change(model.Anonymous(), first, second), auth.ErrInvalidCredentials)
		tok := u.Principal(model.AuthToken)
		tok.Token = &model.TokenInfo{ID: uuid.New(), Scope: model.ScopeAdmin}
		require.ErrorIs(t, change(tok, first, second), auth.ErrPasswordNeedsSession)
		noSession := u.Principal(model.AuthSession)
		require.ErrorIs(t, change(noSession, first, second), auth.ErrPasswordNeedsSession, "a session Principal names its session")

		require.ErrorIs(t, change(p, "wrong password 99", second), auth.ErrWrongPassword)
		require.ErrorIs(t, change(p, first, "short"), auth.ErrWeakPassword)
		require.ErrorIs(t, change(p, first, "Carol"), auth.ErrWeakPassword, "the password cannot be the username")

		require.NoError(t, change(p, first, second))
		sessions, err := s.Sessions.List(ctx, u.ID)
		require.NoError(t, err)
		require.Len(t, sessions, 1)
		require.Equal(t, current.ID, sessions[0].ID, "the current session stays")
		_, err = s.Login.Password(ctx, auth.LoginAttempt{Username: "carol", Password: second, IP: "10.0.0.8"})
		require.NoError(t, err)
		require.Equal(t, audit.UserPasswordChanged, rec.Actions()[len(rec.Actions())-2], "then login.succeeded")

		// Five wrong current passwords lock the throttle for the username,
		// for sign-in as well. A new IP address, so only the username's
		// counter, which success reset, decides.
		ip = "10.0.0.20"
		for range 5 {
			require.ErrorIs(t, change(p, "wrong password 99", first), auth.ErrWrongPassword)
		}
		var te *auth.ThrottledError
		require.True(t, errors.As(change(p, second, first), &te), "locked even with the right password")
		_, err = s.Login.Password(ctx, auth.LoginAttempt{Username: "carol", Password: second, IP: "10.0.0.99"})
		require.True(t, errors.As(err, &te), "sign-in for the username is locked too")

		// Users without a Hangar password, who sign in with OIDC.
		o, err := s.Users.Create(ctx, auth.NewUser{Username: "olivia"}, model.System())
		require.NoError(t, err)
		osess, _, err := s.Sessions.Create(ctx, o.ID, "", "")
		require.NoError(t, err)
		op := o.Principal(model.AuthSession)
		op.SessionID = osess.ID
		require.ErrorIs(t, change(op, "", second), auth.ErrNoLocalPassword)
		require.ErrorIs(t, change(op, "", second), auth.ErrForbidden)
	})
}

// TestUpdateProfile covers auth.Service.UpdateProfile (W8 8): the admin
// editor's checks and errors, a new email waiting for confirmation, and
// user.profile_changed naming the fields.
func TestUpdateProfile(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		ctx := context.Background()
		rec := audittest.NewFake()
		s, _ := authtest.NewService(t, d, authtest.RealOptions{Audit: rec})
		u, err := s.Users.Create(ctx, auth.NewUser{Username: "dave", Email: "dave@example.com", EmailVerified: true}, model.System())
		require.NoError(t, err)
		_, err = s.Users.Create(ctx, auth.NewUser{Username: "erin", Email: "erin@example.com"}, model.System())
		require.NoError(t, err)
		p := u.Principal(model.AuthSession)

		_, err = s.UpdateProfile(ctx, model.Anonymous(), auth.ProfileUpdate{DisplayName: ptr("x")}, "", "")
		require.ErrorIs(t, err, auth.ErrInvalidCredentials)
		_, err = s.UpdateProfile(ctx, p, auth.ProfileUpdate{Email: ptr("ERIN@example.com")}, "", "")
		require.ErrorIs(t, err, auth.ErrConflict, "another User's email, verified or not")
		_, err = s.UpdateProfile(ctx, p, auth.ProfileUpdate{Email: ptr("nope")}, "", "")
		require.ErrorIs(t, err, auth.ErrInvalid)
		_, err = s.UpdateProfile(ctx, p, auth.ProfileUpdate{DisplayName: ptr(strings.Repeat("x", 101))}, "", "")
		require.ErrorIs(t, err, auth.ErrInvalid)

		before := len(rec.Actions())
		got, err := s.UpdateProfile(ctx, p, auth.ProfileUpdate{Email: ptr("DAVE@example.com")}, "", "")
		require.NoError(t, err)
		require.Equal(t, "dave@example.com", got.Email, "a change of case only is no change")
		require.Len(t, rec.Actions(), before, "no change, no audit event")

		got, err = s.UpdateProfile(ctx, p, auth.ProfileUpdate{DisplayName: ptr("Dave D"), Email: ptr("dave@home.example")}, "10.0.0.1", "test")
		require.NoError(t, err)
		require.Equal(t, "Dave D", got.DisplayName)
		require.Equal(t, "dave@example.com", got.Email, "the new address waits for confirmation")
		require.True(t, got.EmailVerified)
		require.Equal(t, "dave@home.example", got.PendingEmail)
		events := rec.Events()
		last := events[len(events)-1]
		require.Equal(t, audit.UserProfileChanged, last.Action)
		require.Equal(t, []string{"display_name", "email"}, last.Data["fields"])
		require.Equal(t, true, last.Data["email_pending"])
		require.Equal(t, "10.0.0.1", last.IP)

		// The current address cancels the pending change; an empty one
		// removes the email at once.
		got, err = s.UpdateProfile(ctx, p, auth.ProfileUpdate{Email: ptr("dave@example.com")}, "", "")
		require.NoError(t, err)
		require.Empty(t, got.PendingEmail)
		got, err = s.UpdateProfile(ctx, p, auth.ProfileUpdate{Email: ptr("")}, "", "")
		require.NoError(t, err)
		require.Empty(t, got.Email)
		require.False(t, got.EmailVerified)
	})
}

// TestSelfSetEmailsNeedConfirmation covers the rule from the security
// review of W8 8: an email a user sets for themselves takes effect only
// through a confirmation link an admin creates and sends to it, so it
// cannot squat an address. A pending email blocks no OIDC first sign-in,
// admin change or other user's request; the link applies it once, and an
// expired, used or replaced link fails.
func TestSelfSetEmailsNeedConfirmation(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		ctx := context.Background()
		rec := audittest.NewFake()
		s, _ := authtest.NewService(t, d, authtest.RealOptions{Audit: rec})
		root := model.System()
		mallory, err := s.Users.Create(ctx, auth.NewUser{Username: "mallory"}, root)
		require.NoError(t, err)
		bob, err := s.Users.Create(ctx, auth.NewUser{Username: "bob"}, root)
		require.NoError(t, err)
		mp, bp := mallory.Principal(model.AuthSession), bob.Principal(model.AuthSession)
		const victim = "victim@corp.example"

		// Squatting: the address stays pending and matches nothing.
		got, err := s.UpdateProfile(ctx, mp, auth.ProfileUpdate{Email: ptr(victim)}, "", "")
		require.NoError(t, err)
		require.Empty(t, got.Email)
		require.Equal(t, victim, got.PendingEmail)
		_, err = s.Users.GetByEmail(ctx, victim)
		require.ErrorIs(t, err, auth.ErrNotFound)
		_, err = s.CreateEmailLink(ctx, bob.ID, "anything@example.com", root)
		require.ErrorIs(t, err, auth.ErrNoEmailChange)

		// It blocks no other user's request for the address.
		got, err = s.UpdateProfile(ctx, bp, auth.ProfileUpdate{Email: ptr(victim)}, "", "")
		require.NoError(t, err)
		require.Equal(t, victim, got.PendingEmail)

		// It does not block the owner's OIDC first sign-in, which gets the
		// verified address.
		sso := config.OIDCProvider{ID: "sso", Name: "SSO", AutoCreateUsers: true}
		res, err := s.OIDC.SignInWithClaims(ctx, sso, map[string]any{"sub": "v-1", "preferred_username": "victim",
			"email": victim, "email_verified": true})
		require.NoError(t, err)
		require.Equal(t, victim, res.User.Email)
		require.True(t, res.User.EmailVerified)

		// Mallory's link now fails: the address is taken. The link stays
		// until it expires, and confirming it keeps failing.
		link, err := s.CreateEmailLink(ctx, mallory.ID, victim, root)
		require.NoError(t, err)
		code := link.URL[strings.LastIndex(link.URL, "/")+1:]
		require.Contains(t, link.URL, auth.EmailPath)
		_, err = s.ConfirmEmail(ctx, code, "", "")
		require.ErrorIs(t, err, auth.ErrConflict)

		// A verified email blocks an OIDC first sign-in with the same
		// address until its owner links the provider; an unverified one,
		// which only open registration and unverified claims store, does
		// not, and the new account does not get the address.
		_, err = s.Users.Create(ctx, auth.NewUser{Username: "carol", Email: "carol@corp.example", EmailVerified: true}, root)
		require.NoError(t, err)
		_, err = s.OIDC.SignInWithClaims(ctx, sso, map[string]any{"sub": "c-1", "preferred_username": "carol2",
			"email": "carol@corp.example", "email_verified": true})
		require.ErrorIs(t, err, auth.ErrOIDCLinkRequired)
		_, err = s.Users.Create(ctx, auth.NewUser{Username: "dan", Email: "dan@corp.example"}, root)
		require.NoError(t, err)
		res, err = s.OIDC.SignInWithClaims(ctx, sso, map[string]any{"sub": "d-1", "preferred_username": "dan2",
			"email": "dan@corp.example", "email_verified": true})
		require.NoError(t, err)
		require.Empty(t, res.User.Email)

		// Bob confirms an address he owns: the link works once.
		_, err = s.UpdateProfile(ctx, bp, auth.ProfileUpdate{Email: ptr("bob@home.example")}, "", "")
		require.NoError(t, err)
		first, err := s.CreateEmailLink(ctx, bob.ID, "bob@home.example", root)
		require.NoError(t, err)
		require.Equal(t, "bob@home.example", first.Email, "the link carries the address it is bound to")
		second, err := s.CreateEmailLink(ctx, bob.ID, "bob@home.example", root)
		require.NoError(t, err)
		codeOf := func(l *auth.Link) string { return l.URL[strings.LastIndex(l.URL, "/")+1:] }
		_, err = s.ConfirmEmail(ctx, codeOf(first), "", "")
		require.ErrorIs(t, err, auth.ErrLinkInvalid, "a new link replaces the earlier one")
		u, err := s.ConfirmEmail(ctx, codeOf(second), "10.0.0.2", "test")
		require.NoError(t, err)
		require.Equal(t, "bob@home.example", u.Email)
		require.True(t, u.EmailVerified)
		require.Empty(t, u.PendingEmail)
		_, err = s.ConfirmEmail(ctx, codeOf(second), "", "")
		require.ErrorIs(t, err, auth.ErrLinkInvalid, "a used link fails")
		require.Equal(t, audit.UserEmailConfirmation, rec.Actions()[len(rec.Actions())-1])

		// An expired link fails.
		_, err = s.UpdateProfile(ctx, bp, auth.ProfileUpdate{Email: ptr("bob@work.example")}, "", "")
		require.NoError(t, err)
		expiring, err := s.CreateEmailLink(ctx, bob.ID, "bob@work.example", root)
		require.NoError(t, err)
		_, err = d.NewUpdate().Model((*db.EmailChange)(nil)).Where("user_id = ?", bob.ID).
			Set("expires_at = ?", time.Now().UTC().Add(-time.Minute)).Exec(ctx)
		require.NoError(t, err)
		_, err = s.ConfirmEmail(ctx, codeOf(expiring), "", "")
		require.ErrorIs(t, err, auth.ErrLinkInvalid)
		_, err = s.ConfirmEmail(ctx, "made-up-code", "", "")
		require.ErrorIs(t, err, auth.ErrLinkInvalid)

		// The admin approves the address they saw: if the user changed
		// their pending email since, no link is made. A new pending email
		// ends an earlier link, even for the same address.
		_, err = s.UpdateProfile(ctx, bp, auth.ProfileUpdate{Email: ptr("bob@a.example")}, "", "")
		require.NoError(t, err)
		seen := "bob@a.example"
		early, err := s.CreateEmailLink(ctx, bob.ID, seen, root)
		require.NoError(t, err)
		_, err = s.UpdateProfile(ctx, bp, auth.ProfileUpdate{Email: ptr("bob@b.example")}, "", "")
		require.NoError(t, err)
		_, err = s.CreateEmailLink(ctx, bob.ID, seen, root)
		require.ErrorIs(t, err, auth.ErrPendingEmailChanged)
		require.ErrorIs(t, err, auth.ErrConflict)
		_, err = s.ConfirmEmail(ctx, codeOf(early), "", "")
		require.ErrorIs(t, err, auth.ErrLinkInvalid, "a link for an earlier pending email stops working")
		_, err = s.UpdateProfile(ctx, bp, auth.ProfileUpdate{Email: ptr("bob@a.example")}, "", "")
		require.NoError(t, err)
		_, err = s.ConfirmEmail(ctx, codeOf(early), "", "")
		require.ErrorIs(t, err, auth.ErrLinkInvalid, "asking for the same address again makes a new request")
		u, err = s.Users.Get(ctx, bob.ID)
		require.NoError(t, err)
		require.Equal(t, "bob@home.example", u.Email)

		// An admin sets an email at once, verified, and it replaces the
		// pending change.
		u, err = s.Users.Update(ctx, bob.ID, auth.UserUpdate{Email: ptr("bob@admin.example")}, root)
		require.NoError(t, err)
		require.Equal(t, "bob@admin.example", u.Email)
		require.True(t, u.EmailVerified)
		require.Empty(t, u.PendingEmail)
	})
}

// TestRegistrationEmailsNeedConfirmation: the email a person types when
// they register is theirs to prove too; an invite's email, which an admin
// chose, is set and verified at once.
func TestRegistrationEmailsNeedConfirmation(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		ctx := context.Background()
		s, _ := authtest.NewService(t, d, authtest.RealOptions{Audit: audittest.NewFake()})
		_, err := s.Settings.Update(ctx, auth.SettingsPatch{Registration: ptr("open")}, model.System())
		require.NoError(t, err)
		u, err := s.Register(ctx, auth.Registration{Username: "frank", Password: "a good long password 1", Email: "ceo@corp.example"})
		require.NoError(t, err)
		require.Empty(t, u.Email)
		require.Equal(t, "ceo@corp.example", u.PendingEmail)

		inv, err := s.CreateInvite(ctx, auth.InviteParams{}, model.System())
		require.NoError(t, err)
		u, err = s.Register(ctx, auth.Registration{Username: "gina", Password: "a good long password 2", Email: "gina@corp.example",
			InviteCode: inv.URL[strings.LastIndex(inv.URL, "/")+1:]})
		require.NoError(t, err)
		require.Empty(t, u.Email)
		require.Equal(t, "gina@corp.example", u.PendingEmail)

		inv, err = s.CreateInvite(ctx, auth.InviteParams{Email: "hal@corp.example"}, model.System())
		require.NoError(t, err)
		u, err = s.Register(ctx, auth.Registration{Username: "hal", Password: "a good long password 3", Email: "other@corp.example",
			InviteCode: inv.URL[strings.LastIndex(inv.URL, "/")+1:]})
		require.NoError(t, err)
		require.Equal(t, "hal@corp.example", u.Email)
		require.True(t, u.EmailVerified)
		require.Empty(t, u.PendingEmail)
	})
}

func ptr[T any](v T) *T { return &v }
