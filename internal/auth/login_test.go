package auth_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

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

const goodPassword = "correct horse battery staple 42"

func TestPasswordHashing(t *testing.T) {
	h, err := auth.HashPassword(t.Context(), goodPassword)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(h, "$argon2id$v=19$m=65536,t=3,p=2$"), "parameters from 05-auth.md are stored in the hash: %s", h)
	ok, err := auth.VerifyPassword(t.Context(), h, goodPassword)
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = auth.VerifyPassword(t.Context(), h, goodPassword+"x")
	require.NoError(t, err)
	require.False(t, ok)
	h2, err := auth.HashPassword(t.Context(), goodPassword)
	require.NoError(t, err)
	require.NotEqual(t, h, h2, "salted")

	// Hashes with other parameters still verify, so parameters can change.
	parts := strings.Split(h, "$")
	for _, bad := range []string{
		"", "plain", "$bcrypt$x$y$z$w", "$argon2id$v=18" + "$" + parts[3] + "$" + parts[4] + "$" + parts[5],
		"$argon2id$v=19$m=99999999,t=3,p=2$" + parts[4] + "$" + parts[5],
		"$argon2id$v=19$m=65536,t=3,p=2$!!!$" + parts[5],
	} {
		_, err := auth.VerifyPassword(t.Context(), bad, goodPassword)
		require.Error(t, err, bad)
	}
}

func TestPasswordPolicy(t *testing.T) {
	require.NoError(t, auth.CheckPasswordPolicy(goodPassword, "alice"))
	for name, pw := range map[string]string{
		"too short":   "short1",
		"nine chars":  "abcdefgh1",
		"common":      "1234567890",
		"common case": "QWERTYUIOP",
		"is username": "alice-the-user",
		"too long":    strings.Repeat("a", 1025),
	} {
		require.ErrorIs(t, auth.CheckPasswordPolicy(pw, "alice-the-user"), auth.ErrWeakPassword, name)
	}
	require.True(t, auth.IsCommonPassword("password123"))
	require.False(t, auth.IsCommonPassword(goodPassword))
	require.NoError(t, auth.CheckPasswordPolicy("ünïcödé-pässwörd", ""), "length counts characters, not bytes")
}

func TestPasswordLoginAndThrottling(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		ctx := context.Background()
		clock := authtest.NewClock(start())
		rec := audittest.NewFake()
		s, _ := authtest.NewService(t, d, authtest.RealOptions{Clock: clock, Audit: rec})
		alice, err := s.Users.Create(ctx, auth.NewUser{Username: "alice", Password: goodPassword}, model.System())
		require.NoError(t, err)
		require.True(t, alice.HasPassword)

		u, err := s.Login.Password(ctx, auth.LoginAttempt{Username: "ALICE", Password: goodPassword, IP: "10.0.0.1", UserAgent: "ua"})
		require.NoError(t, err, "usernames are case-insensitive")
		require.Equal(t, alice.ID, u.ID)
		last := rec.Events()[len(rec.Events())-1]
		require.Equal(t, audit.LoginSucceeded, last.Action)
		require.Equal(t, "10.0.0.1", last.IP)

		// Wrong password and unknown user look the same.
		_, err = s.Login.Password(ctx, auth.LoginAttempt{Username: "alice", Password: "wrong-password-1", IP: "10.0.0.1"})
		require.ErrorIs(t, err, auth.ErrLoginFailed)
		_, err = s.Login.Password(ctx, auth.LoginAttempt{Username: "nobody", Password: "wrong-password-1", IP: "10.0.0.9"})
		require.ErrorIs(t, err, auth.ErrLoginFailed)
		require.Equal(t, audit.LoginFailed, rec.Events()[len(rec.Events())-1].Action)
		require.Equal(t, "nobody", rec.Events()[len(rec.Events())-1].TargetID)
		for _, e := range rec.Events() {
			for k := range e.Data {
				require.NotContains(t, strings.ToLower(k), "password")
			}
		}

		// Four more failures for alice from different IPs: the fifth locks
		// the username for a second, then 2s, 4s.
		for i := range 4 {
			_, err = s.Login.Password(ctx, auth.LoginAttempt{Username: "alice", Password: "wrong-password-1", IP: "10.1.0." + string(rune('1'+i))})
			require.ErrorIs(t, err, auth.ErrLoginFailed)
		}
		_, err = s.Login.Password(ctx, auth.LoginAttempt{Username: "alice", Password: goodPassword, IP: "10.2.0.1"})
		var te *auth.ThrottledError
		require.True(t, errors.As(err, &te), "locked even with the right password: %v", err)
		require.Equal(t, time.Second, te.RetryAfter)
		clock.Advance(time.Second)
		_, err = s.Login.Password(ctx, auth.LoginAttempt{Username: "alice", Password: "wrong-password-1", IP: "10.2.0.1"})
		require.ErrorIs(t, err, auth.ErrLoginFailed)
		_, err = s.Login.Password(ctx, auth.LoginAttempt{Username: "alice", Password: goodPassword, IP: "10.2.0.1"})
		require.True(t, errors.As(err, &te))
		require.Equal(t, 2*time.Second, te.RetryAfter, "exponential backoff")
		clock.Advance(2 * time.Second)
		_, err = s.Login.Password(ctx, auth.LoginAttempt{Username: "alice", Password: goodPassword, IP: "10.2.0.1"})
		require.NoError(t, err, "success after the wait resets the username counter")
		_, err = s.Login.Password(ctx, auth.LoginAttempt{Username: "alice", Password: "wrong-password-1", IP: "10.2.0.1"})
		require.ErrorIs(t, err, auth.ErrLoginFailed, "one failure after a reset does not lock")

		// Per-IP throttling covers spraying many usernames.
		for i := range 5 {
			_, err = s.Login.Password(ctx, auth.LoginAttempt{Username: "spray" + string(rune('a'+i)), Password: "wrong-password-1", IP: "192.0.2.7"})
			require.ErrorIs(t, err, auth.ErrLoginFailed)
		}
		_, err = s.Login.Password(ctx, auth.LoginAttempt{Username: "alice", Password: goodPassword, IP: "192.0.2.7"})
		require.True(t, errors.As(err, &te), "the IP is locked")

		// Counters decay after an hour.
		clock.Advance(2 * time.Hour)
		_, err = s.Login.Password(ctx, auth.LoginAttempt{Username: "alice", Password: goodPassword, IP: "192.0.2.7"})
		require.NoError(t, err)

		// Disabled accounts and accounts without passwords fail the same way.
		bob := mustUser(t, s, "bob", false)
		_, err = s.Login.Password(ctx, auth.LoginAttempt{Username: "bob", Password: goodPassword})
		require.ErrorIs(t, err, auth.ErrLoginFailed)
		yes := true
		_, err = s.Users.Update(ctx, alice.ID, auth.UserUpdate{Disabled: &yes}, model.System())
		require.NoError(t, err)
		_, err = s.Login.Password(ctx, auth.LoginAttempt{Username: "alice", Password: goodPassword})
		require.ErrorIs(t, err, auth.ErrLoginFailed)
		_ = bob
	})
}

func TestLocalLoginOff(t *testing.T) {
	ctx := context.Background()
	s, _ := authtest.NewService(t, dbtest.SQLite(t), authtest.RealOptions{Config: func(c *config.Config) {
		c.Security.LocalLogin = false
		c.Security.BootstrapAdmin.Password = goodPassword
	}})
	_, created, err := s.Bootstrap(ctx)
	require.NoError(t, err)
	require.True(t, created)
	_, err = s.Users.Create(ctx, auth.NewUser{Username: "alice", Password: goodPassword}, model.System())
	require.NoError(t, err)
	_, err = s.Login.Password(ctx, auth.LoginAttempt{Username: "alice", Password: goodPassword})
	require.ErrorIs(t, err, auth.ErrLocalLoginDisabled)
	u, err := s.Login.Password(ctx, auth.LoginAttempt{Username: "hangar-admin", Password: goodPassword})
	require.NoError(t, err, "the bootstrap admin can always sign in (break glass)")
	require.True(t, u.IsAdmin)
}

func TestBootstrapAdmin(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		ctx := context.Background()
		s, _ := authtest.NewService(t, d, authtest.RealOptions{Config: func(c *config.Config) {
			c.Security.BootstrapAdmin.Password = goodPassword
			c.Security.BootstrapAdmin.Email = "admin@example.com"
		}})
		u, created, err := s.Bootstrap(ctx)
		require.NoError(t, err)
		require.True(t, created)
		require.Equal(t, "hangar-admin", u.Username)
		require.True(t, u.IsAdmin)
		require.Equal(t, "admin@example.com", u.Email)
		_, created, err = s.Bootstrap(ctx)
		require.NoError(t, err)
		require.False(t, created, "an admin exists, so nothing happens")

		// Disabling the last admin is refused, so bootstrap never needs to
		// run again by accident.
		yes := true
		_, err = s.Users.Update(ctx, u.ID, auth.UserUpdate{Disabled: &yes}, model.System())
		require.ErrorIs(t, err, auth.ErrLastInstanceAdmin)
		no := false
		_, err = s.Users.Update(ctx, u.ID, auth.UserUpdate{IsAdmin: &no}, model.System())
		require.ErrorIs(t, err, auth.ErrLastInstanceAdmin)
	})
	t.Run("weak password", func(t *testing.T) {
		s, _ := authtest.NewService(t, dbtest.SQLite(t), authtest.RealOptions{Config: func(c *config.Config) { c.Security.BootstrapAdmin.Password = "password123" }})
		_, _, err := s.Bootstrap(context.Background())
		require.ErrorIs(t, err, auth.ErrInvalid)
	})
	t.Run("name taken by a non-admin", func(t *testing.T) {
		s, _ := authtest.NewService(t, dbtest.SQLite(t), authtest.RealOptions{Config: func(c *config.Config) { c.Security.BootstrapAdmin.Password = goodPassword }})
		mustUser(t, s, "hangar-admin", false)
		_, _, err := s.Bootstrap(context.Background())
		require.ErrorContains(t, err, "not an enabled admin")
	})
	t.Run("no password", func(t *testing.T) {
		s, _ := authtest.NewService(t, dbtest.SQLite(t), authtest.RealOptions{})
		_, _, err := s.Bootstrap(context.Background())
		require.ErrorContains(t, err, "password is empty")
	})
}
