package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"

	"github.com/hangar-hub/hangar/internal/audit"
	"github.com/hangar-hub/hangar/internal/config"
	"github.com/hangar-hub/hangar/internal/db"
	"github.com/hangar-hub/hangar/internal/model"
	"github.com/hangar-hub/hangar/internal/org"
)

// LinkTTL is how long invite and password reset links work.
const LinkTTL = 72 * time.Hour

// Link paths in the web UI.
const (
	InvitePath = "/auth/invite/"
	ResetPath  = "/auth/reset/"
	EmailPath  = "/auth/email/"
)

// ErrRegistrationClosed means self-registration is not allowed.
var ErrRegistrationClosed = errors.New("registration is closed; ask an admin for an invite link")

// ErrLinkInvalid means an invite or reset link is unknown, used or expired.
var ErrLinkInvalid = fmt.Errorf("%w: the link is invalid, used or expired; ask an admin for a new one", ErrNotFound)

// Link is a single-use invite or reset link. The URL holds the secret and
// is shown once.
type Link struct {
	URL       string
	ExpiresAt time.Time
	// Email is the address an email confirmation link is bound to.
	Email string
}

// InviteParams describes an invite.
type InviteParams struct {
	// Email, if set, becomes the new User's email.
	Email string
	// Org and Role, if set, make the new User a Member.
	Org  string
	Role model.Role
	// MakeAdmin makes the new User an Instance admin.
	MakeAdmin bool
}

// CreateInvite makes a single-use invite link that expires in 72 hours,
// and records invite.created. The caller checks that actor is an Instance
// admin.
func (s *Service) CreateInvite(ctx context.Context, p InviteParams, actor model.Principal) (*Link, error) {
	email, err := normaliseEmail(p.Email)
	if err != nil {
		return nil, err
	}
	row := &db.Invite{ID: db.NewID(), Email: email, MakeAdmin: p.MakeAdmin}
	if actor.UserID != uuid.Nil {
		id := actor.UserID
		row.CreatedBy = &id
	}
	if p.Org != "" {
		ns, err := s.orgs.Get(ctx, p.Org)
		if err != nil || ns.Kind != org.KindOrg {
			return nil, fmt.Errorf("%w: organisation %q does not exist", ErrInvalid, p.Org)
		}
		role := p.Role
		if role == "" {
			role = model.RoleRead
		}
		if !role.Valid() {
			return nil, fmt.Errorf("%w: role must be read, write or admin", ErrInvalid)
		}
		r := string(role)
		row.OrgNamespaceID, row.Role = &ns.ID, &r
	} else if p.Role != "" {
		return nil, fmt.Errorf("%w: a role needs an organisation", ErrInvalid)
	}
	code, err := randomString(32)
	if err != nil {
		return nil, err
	}
	row.CodeHash = hashSecret(code)
	row.ExpiresAt = s.nowUTC().Add(LinkTTL)
	if _, err := s.db.NewInsert().Model(row).Exec(ctx); err != nil {
		return nil, err
	}
	s.record(ctx, audit.Event{Action: audit.InviteCreated, Actor: actor, TargetType: "invite", TargetID: row.ID.String(),
		Data: map[string]any{"email": email, "org": p.Org, "role": string(p.Role), "is_admin": p.MakeAdmin}})
	return &Link{URL: s.publicURL() + InvitePath + code, ExpiresAt: row.ExpiresAt}, nil
}

// Registration describes a new account from the sign-up or invite page.
type Registration struct {
	Username    string
	Password    string
	Email       string
	DisplayName string
	// InviteCode is the code from an invite link; required unless
	// registration is open.
	InviteCode string
	IP         string
	UserAgent  string
}

// Register creates an account. With an invite code it uses the invite
// (whatever the registration setting); without one, registration must be
// open. New accounts always need a password.
func (s *Service) Register(ctx context.Context, reg Registration) (*User, error) {
	if reg.Password == "" {
		return nil, fmt.Errorf("%w: choose a password", ErrInvalid)
	}
	// An email the person types is theirs to prove: it waits as the
	// User's pending email until a confirmation link is used, like one
	// set under Settings (UpdateProfile). Only an invite's email, which
	// an admin chose, is set and verified at once.
	selfEmail, err := normaliseEmail(reg.Email)
	if err != nil {
		return nil, err
	}
	nu := NewUser{Username: reg.Username, Password: reg.Password, DisplayName: reg.DisplayName}
	actor := model.Anonymous()
	if reg.InviteCode == "" {
		st, err := s.Settings.Get(ctx)
		if err != nil {
			return nil, err
		}
		if st.Registration != config.RegistrationOpen {
			return nil, ErrRegistrationClosed
		}
		u, err := s.Users.Create(ctx, nu, actor)
		if err != nil {
			return nil, err
		}
		if selfEmail == "" {
			return u, nil
		}
		if err := setPendingEmail(ctx, s.db, u.ID, selfEmail); err != nil {
			return nil, err
		}
		return s.Users.Get(ctx, u.ID)
	}
	var (
		id     uuid.UUID
		invite db.Invite
	)
	err = s.db.InTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		err := tx.NewSelect().Model(&invite).Where("code_hash = ?", hashSecret(reg.InviteCode)).Scan(ctx)
		if db.IsNotFound(err) {
			return ErrLinkInvalid
		}
		if err != nil {
			return err
		}
		if invite.UsedAt != nil || !s.nowUTC().Before(invite.ExpiresAt) {
			return ErrLinkInvalid
		}
		if invite.Email != "" {
			nu.Email = invite.Email
			nu.EmailVerified = true
		}
		nu.IsAdmin = invite.MakeAdmin
		id, err = s.Users.createTx(ctx, tx, nu)
		if err != nil {
			return err
		}
		if invite.Email == "" && selfEmail != "" {
			if err := setPendingEmail(ctx, tx, id, selfEmail); err != nil {
				return err
			}
		}
		res, err := tx.NewUpdate().Model((*db.Invite)(nil)).Where("id = ? AND used_at IS NULL", invite.ID).
			Set("used_at = ?", s.nowUTC()).Set("used_by = ?", id).Set("updated_at = ?", s.nowUTC()).Exec(ctx)
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
	u, err := s.Users.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	s.record(ctx, audit.Event{Action: audit.UserCreated, Actor: u.Principal(model.AuthPassword), TargetType: "user", TargetID: u.Username,
		IP: reg.IP, UserAgent: reg.UserAgent, Data: map[string]any{"invite_id": invite.ID.String(), "is_admin": u.IsAdmin}})
	if invite.OrgNamespaceID != nil && invite.Role != nil {
		ns, err := s.orgs.GetByID(ctx, *invite.OrgNamespaceID)
		if err == nil {
			err = s.orgs.SetMember(ctx, ns.Name, u.ID, model.Role(*invite.Role), org.SourceManual, u.Principal(model.AuthPassword))
		}
		if err != nil {
			s.log.WarnContext(ctx, "add invited user to organisation", "error", err)
		}
	}
	return u, nil
}

// CreateResetLink makes a single-use password reset link for a User that
// expires in 72 hours, and records user.password_reset. Earlier unused
// links for the User stop working.
func (s *Service) CreateResetLink(ctx context.Context, userID uuid.UUID, actor model.Principal) (*Link, error) {
	u, err := s.Users.Get(ctx, userID)
	if err != nil {
		return nil, err
	}
	if u.IsSystem {
		return nil, fmt.Errorf("%w: the system user has no password", ErrInvalid)
	}
	code, err := randomString(32)
	if err != nil {
		return nil, err
	}
	row := &db.PasswordReset{ID: db.NewID(), CodeHash: hashSecret(code), UserID: userID, ExpiresAt: s.nowUTC().Add(LinkTTL)}
	if actor.UserID != uuid.Nil {
		id := actor.UserID
		row.CreatedBy = &id
	}
	err = s.db.InTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.NewUpdate().Model((*db.PasswordReset)(nil)).Where("user_id = ? AND used_at IS NULL", userID).
			Set("expires_at = ?", s.nowUTC()).Exec(ctx); err != nil {
			return err
		}
		_, err := tx.NewInsert().Model(row).Exec(ctx)
		return err
	})
	if err != nil {
		return nil, err
	}
	s.record(ctx, audit.Event{Action: audit.UserPasswordReset, Actor: actor, TargetType: "user", TargetID: u.Username,
		Data: map[string]any{"step": "link_created"}})
	return &Link{URL: s.publicURL() + ResetPath + code, ExpiresAt: row.ExpiresAt}, nil
}

// ResetPassword sets a new password with a reset link's code, signs the
// User out everywhere and records user.password_reset.
func (s *Service) ResetPassword(ctx context.Context, code, password, ip, userAgent string) (*User, error) {
	var u *User
	err := s.db.InTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		var row db.PasswordReset
		err := tx.NewSelect().Model(&row).Where("code_hash = ?", hashSecret(code)).Scan(ctx)
		if db.IsNotFound(err) {
			return ErrLinkInvalid
		}
		if err != nil {
			return err
		}
		if row.UsedAt != nil || !s.nowUTC().Before(row.ExpiresAt) {
			return ErrLinkInvalid
		}
		u, err = s.Users.one(ctx, tx, "u.id = ?", row.UserID)
		if err != nil {
			return err
		}
		if err := s.Users.setPasswordTx(ctx, tx, u, password); err != nil {
			return err
		}
		res, err := tx.NewUpdate().Model((*db.PasswordReset)(nil)).Where("id = ? AND used_at IS NULL", row.ID).
			Set("used_at = ?", s.nowUTC()).Set("updated_at = ?", s.nowUTC()).Exec(ctx)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrLinkInvalid
		}
		_, err = tx.NewDelete().Model((*db.Session)(nil)).Where("user_id = ?", row.UserID).Exec(ctx)
		return err
	})
	if err != nil {
		return nil, err
	}
	s.record(ctx, audit.Event{Action: audit.UserPasswordReset, Actor: u.Principal(model.AuthPassword), TargetType: "user",
		TargetID: u.Username, IP: ip, UserAgent: userAgent, Data: map[string]any{"step": "completed"}})
	// Clear the throttle so the user can sign in at once.
	if _, err := s.db.NewDelete().Model((*db.LoginAttempt)(nil)).Where("key = ?", throttleKeys(u.Username, "")[0]).Exec(ctx); err != nil {
		s.log.WarnContext(ctx, "clear sign-in throttle", "error", err)
	}
	return s.Users.Get(ctx, u.ID)
}
