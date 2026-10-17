package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/uptrace/bun"

	"github.com/jamesagarside/hangar/internal/audit"
	"github.com/jamesagarside/hangar/internal/db"
	"github.com/jamesagarside/hangar/internal/model"
)

// Login throttling (docs/spec/05-auth.md "Local accounts"): after 5
// failures for a username or an IP address, each further attempt waits
// twice as long as the last, from 1 second up to 15 minutes. Counters
// reset an hour after the last failure, and a username's counter resets
// on success. They live in the database so every replica sees them.
const (
	throttleFree     = 5
	throttleBase     = time.Second
	throttleMax      = 15 * time.Minute
	throttleDecay    = time.Hour
	maxUsernameInput = 256
)

// Login errors.
var (
	// ErrLoginFailed is the one message for a wrong username or password,
	// a disabled account or an account without a password.
	ErrLoginFailed = fmt.Errorf("%w: wrong username or password", ErrInvalidCredentials)
	// ErrLocalLoginDisabled means password sign-in is off and the user is
	// not the bootstrap admin.
	ErrLocalLoginDisabled = errors.New("password sign-in is turned off; use your organisation's sign-in provider")
)

// ThrottledError means too many recent failures; try again after RetryAfter.
type ThrottledError struct{ RetryAfter time.Duration }

func (e *ThrottledError) Error() string {
	return fmt.Sprintf("too many failed sign-in attempts; try again in %s", e.RetryAfter.Round(time.Second))
}

// Message is the error as a sentence for users, as problem details carry it.
func (e *ThrottledError) Message() string {
	return fmt.Sprintf("Too many failed sign-in attempts. Try again in %s.", e.RetryAfter.Round(time.Second))
}

// LoginService runs password sign-in.
type LoginService struct{ s *Service }

// LoginAttempt is a password sign-in attempt.
type LoginAttempt struct {
	Username  string
	Password  string
	IP        string
	UserAgent string
}

func throttleKeys(username, ip string) []string {
	keys := []string{"user:" + strings.ToLower(username)}
	if ip != "" {
		keys = append(keys, "ip:"+ip)
	}
	return keys
}

// backoff returns the lock time after n failures.
func backoff(n int) time.Duration {
	if n < throttleFree {
		return 0
	}
	d := throttleBase
	for i := throttleFree; i < n && d < throttleMax; i++ {
		d *= 2
	}
	return min(d, throttleMax)
}

// throttled returns how long the caller must wait, or zero.
func (l *LoginService) throttled(ctx context.Context, keys []string) (time.Duration, error) {
	var rows []db.LoginAttempt
	if err := l.s.db.NewSelect().Model(&rows).Where("key IN (?)", bun.List(keys)).Scan(ctx); err != nil {
		return 0, err
	}
	now := l.s.nowUTC()
	var wait time.Duration
	for _, r := range rows {
		if r.LockedUntil != nil && now.Before(*r.LockedUntil) {
			wait = max(wait, r.LockedUntil.Sub(now))
		}
	}
	return wait, nil
}

// fail counts a failure against each key.
func (l *LoginService) fail(ctx context.Context, keys []string) error {
	now := l.s.nowUTC()
	return l.s.db.InTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		for _, k := range keys {
			var row db.LoginAttempt
			err := tx.NewSelect().Model(&row).Where("key = ?", k).Scan(ctx)
			switch {
			case db.IsNotFound(err):
				row = db.LoginAttempt{Key: k}
			case err != nil:
				return err
			}
			if row.LastFailureAt != nil && now.Sub(*row.LastFailureAt) > throttleDecay {
				row.Failures = 0
			}
			row.Failures++
			row.LastFailureAt = &now
			if d := backoff(row.Failures); d > 0 {
				until := now.Add(d)
				row.LockedUntil = &until
			}
			if _, err := tx.NewInsert().Model(&row).On("CONFLICT (key) DO UPDATE").
				Set("failures = EXCLUDED.failures").Set("last_failure_at = EXCLUDED.last_failure_at").
				Set("locked_until = EXCLUDED.locked_until").Set("updated_at = EXCLUDED.updated_at").Exec(ctx); err != nil {
				return err
			}
		}
		return nil
	})
}

// Password checks a username and password and returns the User. It never
// says which part was wrong. It records login.succeeded or login.failed.
func (l *LoginService) Password(ctx context.Context, a LoginAttempt) (*User, error) {
	u, err := l.password(ctx, a)
	ev := audit.Event{IP: a.IP, UserAgent: a.UserAgent, TargetType: "user", TargetID: truncate(a.Username, maxUsernameInput)}
	if err != nil {
		var te *ThrottledError
		reason := "wrong_credentials"
		switch {
		case errors.As(err, &te):
			reason = "throttled"
		case errors.Is(err, ErrLocalLoginDisabled):
			reason = "local_login_disabled"
		case errors.Is(err, ErrDisabled):
			reason = "disabled"
			err = ErrLoginFailed
		case !errors.Is(err, ErrInvalidCredentials):
			return nil, err
		}
		ev.Action, ev.Actor = audit.LoginFailed, model.Anonymous()
		ev.Data = map[string]any{"method": string(model.AuthPassword), "reason": reason}
		l.s.record(ctx, ev)
		l.s.countAttempt(ctx, model.AuthPassword, "failure")
		return nil, err
	}
	ev.Action, ev.Actor = audit.LoginSucceeded, u.Principal(model.AuthPassword)
	ev.TargetID = u.Username
	ev.Data = map[string]any{"method": string(model.AuthPassword)}
	l.s.record(ctx, ev)
	l.s.countAttempt(ctx, model.AuthPassword, "success")
	return u, nil
}

func (l *LoginService) password(ctx context.Context, a LoginAttempt) (*User, error) {
	if a.Username == "" || a.Password == "" || len(a.Username) > maxUsernameInput {
		return nil, ErrLoginFailed
	}
	keys := throttleKeys(a.Username, a.IP)
	wait, err := l.throttled(ctx, keys)
	if err != nil {
		return nil, err
	}
	if wait > 0 {
		return nil, &ThrottledError{RetryAfter: wait}
	}
	st, err := l.s.Settings.Get(ctx)
	if err != nil {
		return nil, err
	}
	if !st.LocalLogin && !strings.EqualFold(a.Username, l.s.cfg.Security.BootstrapAdmin.Username) {
		return nil, ErrLocalLoginDisabled
	}
	u, err := l.s.Users.GetByUsername(ctx, a.Username)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	ok := false
	if u != nil && u.HasPassword && !u.IsSystem {
		ok, err = VerifyPassword(ctx, u.passwordHash, a.Password)
		if err != nil && ctx.Err() != nil {
			return nil, err
		}
		if err != nil {
			l.s.log.ErrorContext(ctx, "stored password hash is unreadable", "user_id", u.ID.String())
			ok = false
		}
	} else if err := timingSafeDummyVerify(ctx, a.Password); err != nil {
		return nil, err
	}
	if !ok {
		if err := l.fail(ctx, keys); err != nil {
			return nil, err
		}
		return nil, ErrLoginFailed
	}
	if u.Disabled() {
		return nil, ErrDisabled
	}
	if _, err := l.s.db.NewDelete().Model((*db.LoginAttempt)(nil)).Where("key = ?", keys[0]).Exec(ctx); err != nil {
		return nil, err
	}
	return u, nil
}
