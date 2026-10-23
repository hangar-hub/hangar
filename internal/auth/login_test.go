package auth_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
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

		// Wrong password and unknown user look the same. (From an address
		// alice has not signed in from, so it counts for her username.)
		_, err = s.Login.Password(ctx, auth.LoginAttempt{Username: "alice", Password: "wrong-password-1", IP: "10.0.0.2"})
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

// TestUsernameLockSparesKnownAddresses covers security review finding 11:
// anyone could keep a named account, such as the break-glass admin,
// locked by sending a wrong password for it every time the lock ended.
// The username's lock now applies only from IP addresses the User has not
// signed in from, so its owner still signs in from a known address, while
// guesses from anywhere else stay throttled.
func TestUsernameLockSparesKnownAddresses(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		ctx := context.Background()
		clock := authtest.NewClock(start())
		s, _ := authtest.NewService(t, d, authtest.RealOptions{Clock: clock})
		_, err := s.Users.Create(ctx, auth.NewUser{Username: "hangar-admin", Password: goodPassword}, model.System())
		require.NoError(t, err)
		const home = "10.9.0.1"
		_, err = s.Login.Password(ctx, auth.LoginAttempt{Username: "hangar-admin", Password: goodPassword, IP: home})
		require.NoError(t, err)
		var te *auth.ThrottledError
		failOrLocked := func(ip string) {
			t.Helper()
			_, err := s.Login.Password(ctx, auth.LoginAttempt{Username: "hangar-admin", Password: "wrong-password-1", IP: ip})
			if !errors.Is(err, auth.ErrLoginFailed) {
				require.True(t, errors.As(err, &te), "%v", err)
			}
		}

		// An attacker sends wrong passwords from many addresses.
		for i := range 8 {
			failOrLocked(fmt.Sprintf("203.0.113.%d", i))
		}
		_, err = s.Login.Password(ctx, auth.LoginAttempt{Username: "hangar-admin", Password: goodPassword, IP: "198.51.100.1"})
		require.True(t, errors.As(err, &te), "a new address is still locked out: %v", err)
		_, err = s.Login.Password(ctx, auth.LoginAttempt{Username: "hangar-admin", Password: goodPassword, IP: home})
		require.NoError(t, err, "the owner signs in from an address they used before")

		// From a known address only that address's counter applies, so
		// it is still limited to five guesses.
		for range 5 {
			_, err = s.Login.Password(ctx, auth.LoginAttempt{Username: "hangar-admin", Password: "wrong-password-1", IP: home})
			require.ErrorIs(t, err, auth.ErrLoginFailed)
		}
		_, err = s.Login.Password(ctx, auth.LoginAttempt{Username: "hangar-admin", Password: goodPassword, IP: home})
		require.True(t, errors.As(err, &te), "the known address is throttled by its own counter")

		// Known addresses are forgotten 90 days after their last success.
		clock.Advance(91 * 24 * time.Hour)
		for range 5 {
			failOrLocked("203.0.113.50")
		}
		_, err = s.Login.Password(ctx, auth.LoginAttempt{Username: "hangar-admin", Password: goodPassword, IP: home})
		require.True(t, errors.As(err, &te), "after 90 days the address is new again")
		res, err := s.Cleanup(ctx)
		require.NoError(t, err)
		require.Equal(t, int64(1), res.KnownClients, "the stale known address is removed")
	})
}

// TestClearThrottle covers hangar admin unlock-sign-in: clearing a
// username's or an address's counters lets it sign in at once, and the
// change is audited.
func TestClearThrottle(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		ctx := context.Background()
		rec := audittest.NewFake()
		s, _ := authtest.NewService(t, d, authtest.RealOptions{Audit: rec})
		_, err := s.Users.Create(ctx, auth.NewUser{Username: "alice", Password: goodPassword}, model.System())
		require.NoError(t, err)
		for range 5 {
			_, err = s.Login.Password(ctx, auth.LoginAttempt{Username: "alice", Password: "wrong-password-1", IP: "203.0.113.1"})
			require.ErrorIs(t, err, auth.ErrLoginFailed)
		}
		var te *auth.ThrottledError
		_, err = s.Login.Password(ctx, auth.LoginAttempt{Username: "alice", Password: goodPassword, IP: "198.51.100.1"})
		require.True(t, errors.As(err, &te))

		_, err = s.Login.ClearThrottle(ctx, auth.ThrottleTarget{}, model.System())
		require.ErrorIs(t, err, auth.ErrInvalid, "say what to clear")
		n, err := s.Login.ClearThrottle(ctx, auth.ThrottleTarget{Username: "ALICE"}, model.System())
		require.NoError(t, err)
		require.Equal(t, int64(1), n)
		require.Equal(t, audit.LoginThrottleCleared, rec.Actions()[len(rec.Actions())-1])
		_, err = s.Login.Password(ctx, auth.LoginAttempt{Username: "alice", Password: goodPassword, IP: "198.51.100.1"})
		require.NoError(t, err)

		_, err = s.Login.Password(ctx, auth.LoginAttempt{Username: "alice", Password: goodPassword, IP: "203.0.113.1"})
		require.True(t, errors.As(err, &te), "the address that sent the guesses is still locked")
		n, err = s.Login.ClearThrottle(ctx, auth.ThrottleTarget{IP: "203.0.113.1"}, model.System())
		require.NoError(t, err)
		require.Equal(t, int64(1), n)
		_, err = s.Login.Password(ctx, auth.LoginAttempt{Username: "alice", Password: goodPassword, IP: "203.0.113.1"})
		require.NoError(t, err)

		_, err = s.Login.Password(ctx, auth.LoginAttempt{Username: "bob", Password: "wrong-password-1", IP: "203.0.113.2"})
		require.ErrorIs(t, err, auth.ErrLoginFailed)
		n, err = s.Login.ClearThrottle(ctx, auth.ThrottleTarget{All: true}, model.System())
		require.NoError(t, err)
		require.Equal(t, int64(2), n, "user:bob and ip:203.0.113.2")
	})
}

// TestLoginThrottleLockStartsAfterTheHash checks that the lock a failure
// sets runs from when the password was found wrong, not from when the
// attempt was counted before the hash. A hash slower than the first
// one-second lock used to leave no lock at all by the next request.
func TestLoginThrottleLockStartsAfterTheHash(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		ctx := context.Background()
		clock := authtest.NewClock(start())
		s, _ := authtest.NewService(t, d, authtest.RealOptions{Clock: clock})
		_, err := s.Users.Create(ctx, auth.NewUser{Username: "alice", Password: goodPassword}, model.System())
		require.NoError(t, err)
		for i := range 4 {
			_, err = s.Login.Password(ctx, auth.LoginAttempt{Username: "alice", Password: "wrong-password-1", IP: fmt.Sprintf("10.3.0.%d", i)})
			require.ErrorIs(t, err, auth.ErrLoginFailed)
		}
		// The fifth failure locks the username for a second. Each read of
		// the clock now moves it on two seconds, so the check takes longer
		// than the lock.
		clock.SetTick(2 * time.Second)
		_, err = s.Login.Password(ctx, auth.LoginAttempt{Username: "alice", Password: "wrong-password-1", IP: "10.3.0.9"})
		clock.SetTick(0)
		require.ErrorIs(t, err, auth.ErrLoginFailed)
		var row db.LoginAttempt
		require.NoError(t, d.NewSelect().Model(&row).Where("key = ?", "user:alice").Scan(ctx))
		require.Equal(t, 5, row.Failures)
		require.NotNil(t, row.LastFailureAt)
		require.NotNil(t, row.LockedUntil)
		require.Greater(t, row.LockedUntil.Sub(*row.LastFailureAt), time.Second,
			"the lock runs from the end of the check, after the attempt was counted")
	})
}

// TestLoginThrottleHoldsUnderParallelAttempts covers security review
// finding 3: the throttle was checked before the password hash and the
// failure written after it, so a burst of parallel guesses all passed the
// check while the first ones were hashing. Each attempt now counts before
// the hash, so a burst gets the same five guesses as a sequence.
func TestLoginThrottleHoldsUnderParallelAttempts(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		ctx := context.Background()
		clock := authtest.NewClock(start())
		s, _ := authtest.NewService(t, d, authtest.RealOptions{Clock: clock})
		alice, err := s.Users.Create(ctx, auth.NewUser{Username: "alice", Password: goodPassword}, model.System())
		require.NoError(t, err)
		const burst = 24
		run := func(attempt func(i int) error) (failed, throttled int) {
			errs := make(chan error, burst)
			var wg sync.WaitGroup
			for i := range burst {
				wg.Go(func() { errs <- attempt(i) })
			}
			wg.Wait()
			close(errs)
			for err := range errs {
				var te *auth.ThrottledError
				switch {
				case errors.As(err, &te):
					throttled++
				case errors.Is(err, auth.ErrLoginFailed), errors.Is(err, auth.ErrWrongPassword):
					failed++
				default:
					t.Fatalf("unexpected result: %v", err)
				}
			}
			return failed, throttled
		}

		// One username from many IP addresses: only the username's counter
		// decides.
		failed, throttled := run(func(i int) error {
			_, err := s.Login.Password(ctx, auth.LoginAttempt{Username: "alice", Password: "wrong-password-1", IP: fmt.Sprintf("10.9.0.%d", i)})
			return err
		})
		require.Equal(t, 5, failed, "only five guesses are checked")
		require.Equal(t, burst-5, throttled)

		// Many usernames from one IP address (password spraying).
		failed, throttled = run(func(i int) error {
			_, err := s.Login.Password(ctx, auth.LoginAttempt{Username: fmt.Sprintf("spray%d", i), Password: "wrong-password-1", IP: "192.0.2.50"})
			return err
		})
		require.Equal(t, 5, failed, "only five guesses are checked")
		require.Equal(t, burst-5, throttled)

		// Changing a password through the account page counts the same way.
		bob, err := s.Users.Create(ctx, auth.NewUser{Username: "bob", Password: goodPassword}, model.System())
		require.NoError(t, err)
		sess, _, err := s.Sessions.Create(ctx, bob.ID, "", "")
		require.NoError(t, err)
		p := bob.Principal(model.AuthSession)
		p.SessionID = sess.ID
		failed, throttled = run(func(i int) error {
			return s.ChangePassword(ctx, p, auth.PasswordChange{Current: "wrong-password-1", New: "another good password 9", IP: fmt.Sprintf("10.8.0.%d", i)})
		})
		require.Equal(t, 5, failed, "only five guesses are checked")
		require.Equal(t, burst-5, throttled)

		// A right password takes its count back: four failures and a
		// success from one IP address leave it unlocked.
		clock.Advance(2 * time.Hour)
		for i := range 4 {
			_, err := s.Login.Password(ctx, auth.LoginAttempt{Username: fmt.Sprintf("guess%d", i), Password: "wrong-password-1", IP: "198.51.100.1"})
			require.ErrorIs(t, err, auth.ErrLoginFailed)
		}
		for range 3 {
			u, err := s.Login.Password(ctx, auth.LoginAttempt{Username: "alice", Password: goodPassword, IP: "198.51.100.1"})
			require.NoError(t, err)
			require.Equal(t, alice.ID, u.ID)
		}
		// A success from an address with no failures leaves no row behind.
		_, err = s.Login.Password(ctx, auth.LoginAttempt{Username: "alice", Password: goodPassword, IP: "198.51.100.77"})
		require.NoError(t, err)
		n, err := d.NewSelect().Model((*db.LoginAttempt)(nil)).Where("key = ?", "ip:198.51.100.77").Count(ctx)
		require.NoError(t, err)
		require.Zero(t, n)
		_, err = s.Login.Password(ctx, auth.LoginAttempt{Username: "guess9", Password: "wrong-password-1", IP: "198.51.100.1"})
		require.ErrorIs(t, err, auth.ErrLoginFailed, "the fifth failure is checked")
		_, err = s.Login.Password(ctx, auth.LoginAttempt{Username: "alice", Password: goodPassword, IP: "198.51.100.1"})
		var te *auth.ThrottledError
		require.True(t, errors.As(err, &te), "and then the IP address is locked: %v", err)
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
