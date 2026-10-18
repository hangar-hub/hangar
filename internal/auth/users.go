package auth

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"

	"github.com/jamesagarside/hangar/internal/audit"
	"github.com/jamesagarside/hangar/internal/db"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/org"
)

// More errors.
var (
	// ErrInvalid wraps validation failures; handlers answer 400 or 422.
	ErrInvalid = errors.New("invalid request")
	// ErrConflict means a username or email is already taken.
	ErrConflict = errors.New("already exists")
	// ErrLastInstanceAdmin stops disabling or demoting the last enabled
	// Instance admin, which would lock everyone out of the admin UI.
	ErrLastInstanceAdmin = errors.New("the instance needs at least one enabled admin")
	// ErrDisabled means the user account is disabled.
	ErrDisabled = errors.New("account is disabled")
)

// User is a User's account.
type User struct {
	ID            uuid.UUID
	NamespaceID   uuid.UUID
	Username      string
	DisplayName   string
	Email         string
	EmailVerified bool
	// PendingEmail is an email address the User asked for that waits for
	// its confirmation link (email_changes); it does not match anything.
	PendingEmail string
	HasPassword  bool
	IsAdmin      bool
	IsSystem     bool
	DisabledAt   *time.Time
	LastLoginAt  *time.Time
	CreatedAt    time.Time
	// OIDCProviders lists the providers linked to the account.
	OIDCProviders []string

	passwordHash string
}

// Disabled reports whether the account is disabled.
func (u *User) Disabled() bool { return u.DisabledAt != nil }

// Principal returns the Principal for a User signed in with method.
func (u *User) Principal(method model.AuthMethod) model.Principal {
	return model.Principal{
		Kind: model.PrincipalUser, UserID: u.ID, NamespaceID: u.NamespaceID, Username: u.Username,
		DisplayName: u.DisplayName, Email: u.Email, IsAdmin: u.IsAdmin, Method: method,
	}
}

// NewUser describes a User to create.
type NewUser struct {
	Username      string
	Email         string
	EmailVerified bool
	DisplayName   string
	// Password is optional; OIDC-only users have none.
	Password string
	IsAdmin  bool
}

// UserUpdate changes a User. Nil fields are unchanged.
type UserUpdate struct {
	Disabled    *bool
	IsAdmin     *bool
	Email       *string
	DisplayName *string
}

// UserStore manages Users. It owns the users table and creates personal
// Namespaces through internal/org.
type UserStore struct{ s *Service }

type userRow struct {
	db.User      `bun:",extend"`
	Username     string  `bun:"username"`
	DisplayName  string  `bun:"display_name"`
	PendingEmail *string `bun:"pending_email"`
}

func (us *UserStore) query(idb bun.IDB) *bun.SelectQuery {
	return idb.NewSelect().
		TableExpr("users AS u").
		ColumnExpr("u.*").
		ColumnExpr("n.name AS username").
		ColumnExpr("n.display_name AS display_name").
		ColumnExpr("ec.email AS pending_email").
		Join("JOIN namespaces AS n ON n.id = u.namespace_id").
		Join("LEFT JOIN email_changes AS ec ON ec.user_id = u.id")
}

func (r *userRow) toUser() *User {
	u := &User{
		ID: r.ID, NamespaceID: r.NamespaceID, Username: r.Username, DisplayName: r.DisplayName,
		EmailVerified: r.EmailVerified, IsAdmin: r.IsAdmin, IsSystem: r.IsSystem, DisabledAt: r.DisabledAt,
		LastLoginAt: r.LastLoginAt, CreatedAt: r.CreatedAt,
	}
	if r.Email != nil {
		u.Email = *r.Email
	}
	if r.PendingEmail != nil {
		u.PendingEmail = *r.PendingEmail
	}
	if r.PasswordHash != nil {
		u.passwordHash = *r.PasswordHash
		u.HasPassword = true
	}
	return u
}

func (us *UserStore) one(ctx context.Context, idb bun.IDB, where string, args ...any) (*User, error) {
	u, err := us.row(ctx, idb, where, args...)
	if err != nil {
		return nil, err
	}
	if err := us.loadProviders(ctx, idb, []*User{u}); err != nil {
		return nil, err
	}
	return u, nil
}

// row returns one User without OIDCProviders.
func (us *UserStore) row(ctx context.Context, idb bun.IDB, where string, args ...any) (*User, error) {
	var row userRow
	if err := us.query(idb).Where(where, args...).Limit(1).Scan(ctx, &row); err != nil {
		if db.IsNotFound(err) {
			return nil, fmt.Errorf("%w: user", ErrNotFound)
		}
		return nil, err
	}
	return row.toUser(), nil
}

// forRequest returns the User a session or Token belongs to, for
// authenticating a request. It leaves OIDCProviders empty: a Principal
// does not use them, and every authenticated request pays this lookup.
func (us *UserStore) forRequest(ctx context.Context, id uuid.UUID) (*User, error) {
	return us.row(ctx, us.s.db, "u.id = ?", id)
}

func (us *UserStore) loadProviders(ctx context.Context, idb bun.IDB, users []*User) error {
	if len(users) == 0 {
		return nil
	}
	ids := make([]uuid.UUID, 0, len(users))
	byID := map[uuid.UUID]*User{}
	for _, u := range users {
		ids = append(ids, u.ID)
		byID[u.ID] = u
	}
	var rows []db.Identity
	if err := idb.NewSelect().Model(&rows).Column("user_id", "provider").Where("user_id IN (?)", bun.List(ids)).Order("provider").Scan(ctx); err != nil {
		return err
	}
	for _, r := range rows {
		if u := byID[r.UserID]; u != nil {
			u.OIDCProviders = append(u.OIDCProviders, r.Provider)
		}
	}
	return nil
}

// Get returns a User by ID.
func (us *UserStore) Get(ctx context.Context, id uuid.UUID) (*User, error) {
	return us.one(ctx, us.s.db, "u.id = ?", id)
}

// GetByUsername returns a User by username, case-insensitively.
func (us *UserStore) GetByUsername(ctx context.Context, username string) (*User, error) {
	return us.one(ctx, us.s.db, "lower(n.name) = lower(?)", username)
}

// GetByEmail returns a User by email address, case-insensitively.
func (us *UserStore) GetByEmail(ctx context.Context, email string) (*User, error) {
	if email == "" {
		return nil, fmt.Errorf("%w: user", ErrNotFound)
	}
	return us.one(ctx, us.s.db, "lower(u.email) = lower(?)", email)
}

// List returns Users ordered by username, optionally filtered by a
// substring of the username, display name or email. The cursor is opaque.
func (us *UserStore) List(ctx context.Context, q, cursor string, limit int) ([]*User, string, error) {
	if limit <= 0 {
		limit = 50
	}
	limit = min(limit, 500)
	query := us.query(us.s.db).Where("u.is_system = ?", false)
	if q != "" {
		like := "%" + escapeLike(strings.ToLower(q)) + "%"
		query = query.Where("(lower(n.name) LIKE ? ESCAPE '\\' OR lower(n.display_name) LIKE ? ESCAPE '\\' OR lower(COALESCE(u.email, '')) LIKE ? ESCAPE '\\')", like, like, like)
	}
	if cursor != "" {
		b, err := base64.RawURLEncoding.DecodeString(cursor)
		if err != nil {
			return nil, "", fmt.Errorf("%w: bad cursor", ErrInvalid)
		}
		query = query.Where("lower(n.name) > ?", string(b))
	}
	var rows []userRow
	if err := query.OrderExpr("lower(n.name) ASC").Limit(int64(limit+1)).Scan(ctx, &rows); err != nil {
		return nil, "", err
	}
	next := ""
	if len(rows) > limit {
		rows = rows[:limit]
		next = base64.RawURLEncoding.EncodeToString([]byte(strings.ToLower(rows[len(rows)-1].Username)))
	}
	out := make([]*User, 0, len(rows))
	for i := range rows {
		out = append(out, rows[i].toUser())
	}
	return out, next, us.loadProviders(ctx, us.s.db, out)
}

func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// normaliseEmail validates an optional email address.
func normaliseEmail(email string) (string, error) {
	email = strings.TrimSpace(email)
	if email == "" {
		return "", nil
	}
	a, err := mail.ParseAddress(email)
	if err != nil || a.Address != email || len(email) > 254 {
		return "", fmt.Errorf("%w: %q is not a valid email address", ErrInvalid, email)
	}
	return email, nil
}

// Create creates a User with a personal Namespace and records a
// user.created audit event.
func (us *UserStore) Create(ctx context.Context, nu NewUser, actor model.Principal) (*User, error) {
	var id uuid.UUID
	err := us.s.db.InTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		var err error
		id, err = us.createTx(ctx, tx, nu)
		return err
	})
	if err != nil {
		return nil, err
	}
	u, err := us.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	us.s.record(ctx, audit.Event{Action: audit.UserCreated, Actor: actor, TargetType: "user", TargetID: u.Username,
		Data: map[string]any{"is_admin": u.IsAdmin}})
	return u, nil
}

// createTx inserts the Namespace and user rows in tx.
func (us *UserStore) createTx(ctx context.Context, tx bun.Tx, nu NewUser) (uuid.UUID, error) {
	if err := model.ValidateNewNamespaceName(nu.Username); err != nil {
		return uuid.Nil, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	email, err := normaliseEmail(nu.Email)
	if err != nil {
		return uuid.Nil, err
	}
	if len(nu.DisplayName) > 100 {
		return uuid.Nil, fmt.Errorf("%w: the display name can be at most 100 characters", ErrInvalid)
	}
	var hash *string
	if nu.Password != "" {
		if err := CheckPasswordPolicy(nu.Password, nu.Username); err != nil {
			return uuid.Nil, fmt.Errorf("%w: %w", ErrInvalid, err)
		}
		h, err := HashPassword(ctx, nu.Password)
		if err != nil {
			return uuid.Nil, err
		}
		hash = &h
	}
	nsID, err := org.InsertUserNamespace(ctx, tx, nu.Username, nu.DisplayName)
	if errors.Is(err, org.ErrExists) {
		return uuid.Nil, fmt.Errorf("%w: the name %q is taken", ErrConflict, nu.Username)
	}
	if errors.Is(err, org.ErrInvalid) {
		return uuid.Nil, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if err != nil {
		return uuid.Nil, err
	}
	row := &db.User{ID: db.NewID(), NamespaceID: nsID, EmailVerified: nu.EmailVerified, PasswordHash: hash, IsAdmin: nu.IsAdmin}
	if email != "" {
		row.Email = &email
	}
	if _, err := tx.NewInsert().Model(row).Exec(ctx); err != nil {
		if db.IsUniqueViolation(err) {
			return uuid.Nil, fmt.Errorf("%w: the email address is already used", ErrConflict)
		}
		return uuid.Nil, err
	}
	return row.ID, nil
}

// countOtherEnabledAdmins counts enabled, non-system Instance admins other
// than userID.
func countOtherEnabledAdmins(ctx context.Context, idb bun.IDB, userID uuid.UUID) (int64, error) {
	return idb.NewSelect().Model((*db.User)(nil)).
		Where("is_admin = ? AND is_system = ? AND disabled_at IS NULL AND id <> ?", true, false, userID).Count(ctx)
}

// Update changes a User and records user.disabled, user.enabled and
// user.admin_changed audit events. It is the admin user editor: an email it
// sets is verified, and replaces any email change the User asked for. Disabling a User signs them out
// everywhere. It refuses to disable or demote the last enabled admin.
func (us *UserStore) Update(ctx context.Context, id uuid.UUID, up UserUpdate, actor model.Principal) (*User, error) {
	var events []audit.Event
	err := us.s.db.InTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		u, err := us.one(ctx, tx, "u.id = ?", id)
		if err != nil {
			return err
		}
		if u.IsSystem {
			return fmt.Errorf("%w: the system user cannot be changed", ErrInvalid)
		}
		target := audit.Event{Actor: actor, TargetType: "user", TargetID: u.Username}
		q := tx.NewUpdate().Model((*db.User)(nil)).Where("id = ?", id).Set("updated_at = ?", us.s.nowUTC())
		dirty := false
		losesAdmin := (up.Disabled != nil && *up.Disabled && !u.Disabled() && u.IsAdmin) ||
			(up.IsAdmin != nil && !*up.IsAdmin && u.IsAdmin && !u.Disabled())
		if losesAdmin {
			n, err := countOtherEnabledAdmins(ctx, tx, id)
			if err != nil {
				return err
			}
			if n == 0 {
				return ErrLastInstanceAdmin
			}
		}
		if up.Disabled != nil && *up.Disabled != u.Disabled() {
			dirty = true
			e := target
			if *up.Disabled {
				q = q.Set("disabled_at = ?", us.s.nowUTC())
				e.Action = audit.UserDisabled
				if _, err := tx.NewDelete().Model((*db.Session)(nil)).Where("user_id = ?", id).Exec(ctx); err != nil {
					return err
				}
			} else {
				q = q.Set("disabled_at = NULL")
				e.Action = audit.UserEnabled
			}
			events = append(events, e)
		}
		if up.IsAdmin != nil && *up.IsAdmin != u.IsAdmin {
			dirty = true
			q = q.Set("is_admin = ?", *up.IsAdmin)
			e := target
			e.Action = audit.UserAdminChanged
			e.Data = map[string]any{"is_admin": *up.IsAdmin}
			events = append(events, e)
		}
		if up.Email != nil {
			email, err := normaliseEmail(*up.Email)
			if err != nil {
				return err
			}
			if !strings.EqualFold(email, u.Email) {
				dirty = true
				if email == "" {
					q = q.Set("email = NULL")
				} else {
					q = q.Set("email = ?", email)
				}
				// An admin vouches for the address, as for one set
				// when creating a User.
				q = q.Set("email_verified = ?", email != "")
			}
			if _, err := tx.NewDelete().Model((*db.EmailChange)(nil)).Where("user_id = ?", id).Exec(ctx); err != nil {
				return err
			}
		}
		if dirty {
			if _, err := q.Exec(ctx); err != nil {
				if db.IsUniqueViolation(err) {
					return fmt.Errorf("%w: the email address is already used", ErrConflict)
				}
				return err
			}
		}
		if up.DisplayName != nil && *up.DisplayName != u.DisplayName {
			if len(*up.DisplayName) > 100 {
				return fmt.Errorf("%w: the display name can be at most 100 characters", ErrInvalid)
			}
			if _, err := tx.NewUpdate().Model((*db.Namespace)(nil)).Where("id = ?", u.NamespaceID).
				Set("display_name = ?", *up.DisplayName).Set("updated_at = ?", us.s.nowUTC()).Exec(ctx); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, e := range events {
		us.s.record(ctx, e)
	}
	return us.Get(ctx, id)
}

// SetPassword sets a User's password directly, for `hangar admin
// reset-password` on the server host. It checks the password policy, signs
// the User out everywhere, clears sign-in throttling for the username and
// records user.password_reset with actor.
func (us *UserStore) SetPassword(ctx context.Context, id uuid.UUID, password string, actor model.Principal) (*User, error) {
	var u *User
	err := us.s.db.InTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		var err error
		if u, err = us.one(ctx, tx, "u.id = ?", id); err != nil {
			return err
		}
		if u.IsSystem {
			return fmt.Errorf("%w: the system user has no password", ErrInvalid)
		}
		if err := us.setPasswordTx(ctx, tx, u, password); err != nil {
			return err
		}
		if _, err := tx.NewDelete().Model((*db.Session)(nil)).Where("user_id = ?", id).Exec(ctx); err != nil {
			return err
		}
		_, err = tx.NewDelete().Model((*db.LoginAttempt)(nil)).Where("key = ?", throttleKeys(u.Username, "")[0]).Exec(ctx)
		return err
	})
	if err != nil {
		return nil, err
	}
	us.s.record(ctx, audit.Event{Action: audit.UserPasswordReset, Actor: actor, TargetType: "user", TargetID: u.Username})
	return us.Get(ctx, id)
}

// setPasswordTx stores a new password hash after checking the policy.
func (us *UserStore) setPasswordTx(ctx context.Context, tx bun.IDB, u *User, pw string) error {
	if err := CheckPasswordPolicy(pw, u.Username); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	h, err := HashPassword(ctx, pw)
	if err != nil {
		return err
	}
	_, err = tx.NewUpdate().Model((*db.User)(nil)).Where("id = ?", u.ID).
		Set("password_hash = ?", h).Set("updated_at = ?", us.s.nowUTC()).Exec(ctx)
	return err
}

// touchLogin records a successful sign-in time.
func (us *UserStore) touchLogin(ctx context.Context, id uuid.UUID) {
	_, err := us.s.db.NewUpdate().Model((*db.User)(nil)).Where("id = ?", id).Set("last_login_at = ?", us.s.nowUTC()).Exec(ctx)
	if err != nil {
		us.s.log.WarnContext(ctx, "record last sign-in", "error", err)
	}
}

// Count returns the number of Users, excluding the system user.
func (us *UserStore) Count(ctx context.Context) (int64, error) {
	return us.s.db.NewSelect().Model((*db.User)(nil)).Where("is_system = ?", false).Count(ctx)
}
