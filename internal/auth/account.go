package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/uptrace/bun"

	"github.com/jamesagarside/hangar/internal/audit"
	"github.com/jamesagarside/hangar/internal/db"
	"github.com/jamesagarside/hangar/internal/model"
)

// Self-service account changes: PATCH /me and POST /me/password (decided
// by James on 2026-10-18, W8 8; docs/spec/05-auth.md "Local accounts").

// Errors from self-service account changes.
var (
	// ErrNoLocalPassword means the User has no Hangar password: they sign
	// in only through an OIDC provider, which owns their password.
	ErrNoLocalPassword = fmt.Errorf("%w: this account signs in with a sign-in provider and has no Hangar password; change your password with your provider", ErrForbidden)
	// ErrPasswordNeedsSession means a Token tried to change a password.
	ErrPasswordNeedsSession = fmt.Errorf("%w: change your password in the web UI; a token cannot change it", ErrForbidden)
	// ErrWrongPassword means the current password given for a change is
	// wrong. Handlers answer 403, not 401, so clients do not read it as an
	// ended session.
	ErrWrongPassword = errors.New("the current password is wrong")
)

// ProfileUpdate is a User's change to their own profile. Nil fields are
// unchanged; an empty string clears the field.
type ProfileUpdate struct {
	DisplayName *string
	Email       *string
}

// UpdateProfile changes the signed-in User's display name and email with
// the same checks as the admin user editor (UserStore.Update), and records
// user.profile_changed naming the fields that changed. Users who sign in
// with OIDC may change both: Hangar copies them from the provider's claims
// only when it creates the account.
func (s *Service) UpdateProfile(ctx context.Context, p model.Principal, up ProfileUpdate, ip, userAgent string) (*User, error) {
	if !p.IsUser() {
		return nil, fmt.Errorf("%w: not signed in", ErrInvalidCredentials)
	}
	before, err := s.Users.Get(ctx, p.UserID)
	if err != nil {
		return nil, err
	}
	u, err := s.Users.Update(ctx, p.UserID, UserUpdate{DisplayName: up.DisplayName, Email: up.Email}, p)
	if err != nil {
		return nil, err
	}
	var fields []string
	if u.DisplayName != before.DisplayName {
		fields = append(fields, "display_name")
	}
	if !strings.EqualFold(u.Email, before.Email) {
		fields = append(fields, "email")
	}
	if len(fields) > 0 {
		s.record(ctx, audit.Event{Action: audit.UserProfileChanged, Actor: p, TargetType: "user", TargetID: u.Username,
			IP: ip, UserAgent: userAgent, Data: map[string]any{"fields": fields}})
	}
	return u, nil
}

// PasswordChange is a signed-in User's request to change their password.
type PasswordChange struct {
	Current   string
	New       string
	IP        string
	UserAgent string
}

// ChangePassword changes the signed-in User's password. Only a web UI
// session may do it; Tokens get ErrPasswordNeedsSession. The current
// password must be right: a wrong one counts towards the sign-in throttle
// for the username and IP address, as a failed sign-in does, and while
// that throttle is locked every attempt gets a ThrottledError. The new
// password must meet CheckPasswordPolicy. On success every other session
// of the User ends, the current session and the User's Tokens stay, the
// username's throttle resets and user.password_changed is recorded.
// Failures record user.password_change_failed with the reason.
func (s *Service) ChangePassword(ctx context.Context, p model.Principal, c PasswordChange) error {
	if !p.IsUser() {
		return fmt.Errorf("%w: not signed in", ErrInvalidCredentials)
	}
	if p.Method != model.AuthSession || p.Token != nil || p.SessionID == uuid.Nil {
		return ErrPasswordNeedsSession
	}
	u, err := s.Users.Get(ctx, p.UserID)
	if err != nil {
		return err
	}
	ev := audit.Event{Actor: p, TargetType: "user", TargetID: u.Username, IP: c.IP, UserAgent: c.UserAgent}
	failed := func(reason string) {
		e := ev
		e.Action, e.Data = audit.UserPasswordChangeFailed, map[string]any{"reason": reason}
		s.record(ctx, e)
	}
	if !u.HasPassword || u.IsSystem {
		failed("no_password")
		return ErrNoLocalPassword
	}
	keys := throttleKeys(u.Username, c.IP)
	wait, err := s.Login.throttled(ctx, keys)
	if err != nil {
		return err
	}
	if wait > 0 {
		failed("throttled")
		return &ThrottledError{RetryAfter: wait}
	}
	ok := false
	if c.Current != "" {
		ok, err = VerifyPassword(ctx, u.passwordHash, c.Current)
		if err != nil && ctx.Err() != nil {
			return err
		}
		if err != nil {
			s.log.ErrorContext(ctx, "stored password hash is unreadable", "user_id", u.ID.String())
			ok = false
		}
	}
	if !ok {
		if err := s.Login.fail(ctx, keys); err != nil {
			return err
		}
		failed("wrong_password")
		return ErrWrongPassword
	}
	if err := CheckPasswordPolicy(c.New, u.Username); err != nil {
		failed("weak_password")
		return err
	}
	var revoked int64
	err = s.db.InTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		if err := s.Users.setPasswordTx(ctx, tx, u, c.New); err != nil {
			return err
		}
		res, err := tx.NewDelete().Model((*db.Session)(nil)).Where("user_id = ? AND id <> ?", u.ID, p.SessionID).Exec(ctx)
		if err != nil {
			return err
		}
		revoked, _ = res.RowsAffected()
		_, err = tx.NewDelete().Model((*db.LoginAttempt)(nil)).Where("key = ?", keys[0]).Exec(ctx)
		return err
	})
	if err != nil {
		return err
	}
	e := ev
	e.Action, e.Data = audit.UserPasswordChanged, map[string]any{"sessions_ended": revoked}
	s.record(ctx, e)
	return nil
}
