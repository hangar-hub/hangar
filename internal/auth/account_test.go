package auth_test

import (
	"context"
	"errors"
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
// editor's checks and errors, and user.profile_changed naming the fields.
func TestUpdateProfile(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		ctx := context.Background()
		rec := audittest.NewFake()
		s, _ := authtest.NewService(t, d, authtest.RealOptions{Audit: rec})
		u, err := s.Users.Create(ctx, auth.NewUser{Username: "dave", Email: "dave@example.com"}, model.System())
		require.NoError(t, err)
		_, err = s.Users.Create(ctx, auth.NewUser{Username: "erin", Email: "erin@example.com"}, model.System())
		require.NoError(t, err)
		p := u.Principal(model.AuthSession)
		str := func(s string) *string { return &s }

		_, err = s.UpdateProfile(ctx, model.Anonymous(), auth.ProfileUpdate{DisplayName: str("x")}, "", "")
		require.ErrorIs(t, err, auth.ErrInvalidCredentials)
		_, err = s.UpdateProfile(ctx, p, auth.ProfileUpdate{Email: str("ERIN@example.com")}, "", "")
		require.ErrorIs(t, err, auth.ErrConflict)
		_, err = s.UpdateProfile(ctx, p, auth.ProfileUpdate{Email: str("nope")}, "", "")
		require.ErrorIs(t, err, auth.ErrInvalid)

		before := len(rec.Actions())
		got, err := s.UpdateProfile(ctx, p, auth.ProfileUpdate{Email: str("DAVE@example.com")}, "", "")
		require.NoError(t, err)
		require.Equal(t, "dave@example.com", got.Email, "a change of case only is no change")
		require.Len(t, rec.Actions(), before, "no change, no audit event")

		got, err = s.UpdateProfile(ctx, p, auth.ProfileUpdate{DisplayName: str("Dave D"), Email: str("dave@home.example")}, "10.0.0.1", "test")
		require.NoError(t, err)
		require.Equal(t, "Dave D", got.DisplayName)
		require.Equal(t, "dave@home.example", got.Email)
		events := rec.Events()
		last := events[len(events)-1]
		require.Equal(t, audit.UserProfileChanged, last.Action)
		require.Equal(t, []string{"display_name", "email"}, last.Data["fields"])
		require.Equal(t, "10.0.0.1", last.IP)
	})
}
