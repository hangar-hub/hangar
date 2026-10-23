package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/uptrace/bun"

	"github.com/hangar-hub/hangar/internal/audit"
	"github.com/hangar-hub/hangar/internal/db"
	"github.com/hangar-hub/hangar/internal/model"
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

// ErrNoEmailChange means an admin asked for a confirmation link for a User
// who has no email change waiting.
var ErrNoEmailChange = fmt.Errorf("%w: this user has no email change waiting for confirmation", ErrInvalid)

// UpdateProfile changes the signed-in User's display name with the admin
// user editor's checks (UserStore.Update), and handles their email:
//
//   - The same address as now (in any case) cancels a pending change.
//   - An empty string removes the email at once, and any pending change.
//   - Another address must be valid and not another User's email (the
//     admin editor's ErrConflict). It does not take effect: it is stored as
//     the User's pending email, outside users.email and its uniqueness,
//     replacing any earlier one, until the confirmation link that an admin
//     creates (CreateEmailLink) and sends to that address is used
//     (ConfirmEmail). Hangar sends no mail, so the admin delivers the link,
//     as for reset links. A pending email matches no OIDC sign-in, invite
//     or clash check.
//
// It records user.profile_changed naming the fields that changed, with
// email_pending when the email waits for confirmation. Users who sign in
// with OIDC may do all of this: Hangar copies the claims only when it
// creates the account.
func (s *Service) UpdateProfile(ctx context.Context, p model.Principal, up ProfileUpdate, ip, userAgent string) (*User, error) {
	if !p.IsUser() {
		return nil, fmt.Errorf("%w: not signed in", ErrInvalidCredentials)
	}
	before, err := s.Users.Get(ctx, p.UserID)
	if err != nil {
		return nil, err
	}
	if before.IsSystem {
		return nil, fmt.Errorf("%w: the system user cannot be changed", ErrInvalid)
	}
	var email string
	if up.Email != nil {
		if email, err = normaliseEmail(*up.Email); err != nil {
			return nil, err
		}
		if email != "" && !strings.EqualFold(email, before.Email) {
			other, err := s.Users.GetByEmail(ctx, email)
			switch {
			case err == nil && other.ID != before.ID:
				return nil, fmt.Errorf("%w: the email address is already used", ErrConflict)
			case err != nil && !errors.Is(err, ErrNotFound):
				return nil, err
			}
		}
	}
	if up.DisplayName != nil {
		if _, err := s.Users.Update(ctx, p.UserID, UserUpdate{DisplayName: up.DisplayName}, p); err != nil {
			return nil, err
		}
	}
	pending := false
	if up.Email != nil {
		err := s.db.InTx(ctx, func(ctx context.Context, tx bun.Tx) error {
			if _, err := tx.NewDelete().Model((*db.EmailChange)(nil)).Where("user_id = ?", before.ID).Exec(ctx); err != nil {
				return err
			}
			switch {
			case email == "":
				_, err := tx.NewUpdate().Model((*db.User)(nil)).Where("id = ?", before.ID).Set("email = NULL").
					Set("email_verified = ?", false).Set("updated_at = ?", s.nowUTC()).Exec(ctx)
				return err
			case strings.EqualFold(email, before.Email):
				return nil
			}
			pending = true
			return setPendingEmail(ctx, tx, before.ID, email)
		})
		if err != nil {
			return nil, err
		}
	}
	u, err := s.Users.Get(ctx, p.UserID)
	if err != nil {
		return nil, err
	}
	var fields []string
	if u.DisplayName != before.DisplayName {
		fields = append(fields, "display_name")
	}
	if !strings.EqualFold(u.Email, before.Email) || !strings.EqualFold(u.PendingEmail, before.PendingEmail) {
		fields = append(fields, "email")
	}
	if len(fields) > 0 {
		data := map[string]any{"fields": fields}
		if pending {
			data["email_pending"] = true
		}
		s.record(ctx, audit.Event{Action: audit.UserProfileChanged, Actor: p, TargetType: "user", TargetID: u.Username,
			IP: ip, UserAgent: userAgent, Data: data})
	}
	return u, nil
}

// ErrPendingEmailChanged means the User's pending email is no longer the
// one the admin approved.
var ErrPendingEmailChanged = fmt.Errorf("%w: the user's pending email has changed since you looked; reload and check the new address", ErrConflict)

// CreateEmailLink makes the single-use link that confirms a User's pending
// email, for an admin to send to that address. expected is the pending
// email the admin saw: the link is bound to it, in one statement that
// checks the row still holds it, so a change the User makes meanwhile gives
// ErrPendingEmailChanged instead of a link for another address. The link
// expires in 72 hours, like reset links, and replaces any earlier link for
// the User; a new pending email (setPendingEmail) ends it too. The Link
// carries the bound address. It records user.email_confirmation with step
// link_created.
func (s *Service) CreateEmailLink(ctx context.Context, userID uuid.UUID, expected string, actor model.Principal) (*Link, error) {
	u, err := s.Users.Get(ctx, userID)
	if err != nil {
		return nil, err
	}
	code, err := randomString(32)
	if err != nil {
		return nil, err
	}
	hash, expires := hashSecret(code), s.nowUTC().Add(LinkTTL)
	var bound string
	err = s.db.InTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		q := tx.NewUpdate().Model((*db.EmailChange)(nil)).Where("user_id = ? AND email = ?", userID, strings.TrimSpace(expected)).
			Set("code_hash = ?", hash).Set("expires_at = ?", expires).Set("updated_at = ?", s.nowUTC())
		if actor.UserID != uuid.Nil {
			q = q.Set("created_by = ?", actor.UserID)
		}
		res, err := q.Exec(ctx)
		if err != nil {
			return err
		}
		var row db.EmailChange
		err = tx.NewSelect().Model(&row).Where("user_id = ?", userID).Scan(ctx)
		switch {
		case db.IsNotFound(err):
			return ErrNoEmailChange
		case err != nil:
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 || row.CodeHash == nil || *row.CodeHash != hash {
			return ErrPendingEmailChanged
		}
		bound = row.Email
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.record(ctx, audit.Event{Action: audit.UserEmailConfirmation, Actor: actor, TargetType: "user", TargetID: u.Username,
		Data: map[string]any{"step": "link_created"}})
	return &Link{URL: s.publicURL() + EmailPath + code, ExpiresAt: expires, Email: bound}, nil
}

// ConfirmEmail applies the pending email that a confirmation link's code
// names: it becomes the User's verified email and the pending change ends,
// so the link works once. An unknown, used or expired code gives
// ErrLinkInvalid; an address another User took meanwhile gives the admin
// editor's ErrConflict and keeps the link. It records user.email_confirmation
// with step confirmed.
func (s *Service) ConfirmEmail(ctx context.Context, code, ip, userAgent string) (*User, error) {
	var row db.EmailChange
	err := s.db.InTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		err := tx.NewSelect().Model(&row).Where("code_hash = ?", hashSecret(code)).Scan(ctx)
		if db.IsNotFound(err) {
			return ErrLinkInvalid
		}
		if err != nil {
			return err
		}
		if row.ExpiresAt == nil || !s.nowUTC().Before(*row.ExpiresAt) {
			return ErrLinkInvalid
		}
		if _, err := tx.NewUpdate().Model((*db.User)(nil)).Where("id = ?", row.UserID).Set("email = ?", row.Email).
			Set("email_verified = ?", true).Set("updated_at = ?", s.nowUTC()).Exec(ctx); err != nil {
			if db.IsUniqueViolation(err) {
				return fmt.Errorf("%w: the email address is already used", ErrConflict)
			}
			return err
		}
		res, err := tx.NewDelete().Model((*db.EmailChange)(nil)).Where("id = ? AND code_hash = ?", row.ID, hashSecret(code)).Exec(ctx)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrLinkInvalid
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	u, err := s.Users.Get(ctx, row.UserID)
	if err != nil {
		return nil, err
	}
	s.record(ctx, audit.Event{Action: audit.UserEmailConfirmation, Actor: u.Principal(model.AuthPassword), TargetType: "user",
		TargetID: u.Username, IP: ip, UserAgent: userAgent, Data: map[string]any{"step": "confirmed"}})
	return u, nil
}

// setPendingEmail stores email as the User's pending email in idb,
// replacing any earlier one.
func setPendingEmail(ctx context.Context, idb bun.IDB, userID uuid.UUID, email string) error {
	if _, err := idb.NewDelete().Model((*db.EmailChange)(nil)).Where("user_id = ?", userID).Exec(ctx); err != nil {
		return err
	}
	_, err := idb.NewInsert().Model(&db.EmailChange{ID: db.NewID(), UserID: userID, Email: email}).Exec(ctx)
	return err
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
	// As for sign-in, the attempt counts before the hash, so parallel
	// guesses cannot all pass the check, and a right password takes it back.
	att, err := s.Login.reserve(ctx, keys)
	var te *ThrottledError
	if errors.As(err, &te) {
		failed("throttled")
		return err
	}
	if err != nil {
		return err
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
		failed("wrong_password")
		if err := s.Login.confirm(ctx, att); err != nil {
			return err
		}
		return ErrWrongPassword
	}
	if err := s.Login.release(ctx, att); err != nil {
		return err
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
