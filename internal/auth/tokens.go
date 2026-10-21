package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"

	"github.com/hangar-hub/hangar/internal/audit"
	"github.com/hangar-hub/hangar/internal/db"
	"github.com/hangar-hub/hangar/internal/model"
)

// tokenTouchInterval limits last_used_at writes to one a minute per Token.
const tokenTouchInterval = time.Minute

// maxTokenNameLength is the longest Token name.
const maxTokenNameLength = 100

// TokenStore is the database implementation of Tokens. It owns the tokens
// table.
type TokenStore struct{ s *Service }

var _ Tokens = (*TokenStore)(nil)

type tokenRow struct {
	db.Token `bun:",extend"`
	Username string `bun:"username"`
}

func (r *tokenRow) toToken() Token {
	return Token{
		ID: r.ID, UserID: r.UserID, Username: r.Username, Name: r.Name, Prefix: r.Prefix, Scope: model.Scope(r.Scope),
		Restrictions: model.TokenRestrictions{Namespaces: r.Restrictions.Namespaces, Repos: r.Restrictions.Repos},
		ExpiresAt:    r.ExpiresAt, LastUsedAt: r.LastUsedAt, CreatedAt: r.CreatedAt,
	}
}

func (ts *TokenStore) query(idb bun.IDB) *bun.SelectQuery {
	return idb.NewSelect().TableExpr("tokens AS t").ColumnExpr("t.*").ColumnExpr("n.name AS username").
		Join("JOIN users AS u ON u.id = t.user_id").Join("JOIN namespaces AS n ON n.id = u.namespace_id")
}

// validateRestrictions checks Namespace names and `type:namespace/name`
// Repo entries.
func validateRestrictions(r model.TokenRestrictions) (model.TokenRestrictions, error) {
	out := model.TokenRestrictions{}
	for _, ns := range r.Namespaces {
		ns = strings.TrimSpace(ns)
		if err := model.ValidateNamespaceName(ns); err != nil {
			return out, fmt.Errorf("%w: restriction namespace: %w", ErrInvalid, err)
		}
		out.Namespaces = append(out.Namespaces, ns)
	}
	for _, rp := range r.Repos {
		typ, id, ok := strings.Cut(strings.TrimSpace(rp), ":")
		if !ok {
			return out, fmt.Errorf("%w: restriction repo %q must have the form type:namespace/name", ErrInvalid, rp)
		}
		rt, err := model.ParseRepoType(typ)
		if err != nil {
			return out, fmt.Errorf("%w: restriction repo %q: %w", ErrInvalid, rp, err)
		}
		ns, name, ok := strings.Cut(id, "/")
		if !ok || model.ValidateNamespaceName(ns) != nil || name == "" || strings.Contains(name, "/") {
			return out, fmt.Errorf("%w: restriction repo %q must have the form type:namespace/name", ErrInvalid, rp)
		}
		out.Repos = append(out.Repos, string(rt)+":"+ns+"/"+name)
	}
	if len(out.Namespaces)+len(out.Repos) > 100 {
		return out, fmt.Errorf("%w: at most 100 restrictions", ErrInvalid)
	}
	return out, nil
}

// Create implements Tokens. It records a token.created audit event, with
// the Principal of the token's owner as the actor.
func (ts *TokenStore) Create(ctx context.Context, nt NewToken) (*Token, string, error) {
	return ts.create(ctx, nt, model.Principal{})
}

// CreateAs is Create with an explicit actor for the audit event.
func (ts *TokenStore) CreateAs(ctx context.Context, nt NewToken, actor model.Principal) (*Token, string, error) {
	return ts.create(ctx, nt, actor)
}

func (ts *TokenStore) create(ctx context.Context, nt NewToken, actor model.Principal) (*Token, string, error) {
	nt.Name = strings.TrimSpace(nt.Name)
	if nt.Name == "" || len(nt.Name) > maxTokenNameLength {
		return nil, "", fmt.Errorf("%w: a token needs a name of 1 to %d characters", ErrInvalid, maxTokenNameLength)
	}
	if !nt.Scope.Valid() {
		return nil, "", fmt.Errorf("%w: scope must be read, write or admin", ErrInvalid)
	}
	restr, err := validateRestrictions(nt.Restrictions)
	if err != nil {
		return nil, "", err
	}
	if nt.ExpiresAt != nil && !nt.ExpiresAt.After(ts.s.now()) {
		return nil, "", fmt.Errorf("%w: the expiry must be in the future", ErrInvalid)
	}
	u, err := ts.s.Users.Get(ctx, nt.UserID)
	if err != nil {
		return nil, "", err
	}
	if u.Disabled() {
		return nil, "", ErrDisabled
	}
	secret, err := NewTokenSecret()
	if err != nil {
		return nil, "", err
	}
	row := &db.Token{
		ID: db.NewID(), UserID: nt.UserID, Name: nt.Name, Hash: hashSecret(secret), Prefix: secret[:8], Scope: string(nt.Scope),
		Restrictions: db.TokenRestrictionsColumn{Namespaces: restr.Namespaces, Repos: restr.Repos},
	}
	if nt.ExpiresAt != nil {
		t := nt.ExpiresAt.UTC().Truncate(time.Microsecond)
		row.ExpiresAt = &t
	}
	if _, err := ts.s.db.NewInsert().Model(row).Exec(ctx); err != nil {
		return nil, "", err
	}
	if actor.IsAnonymous() {
		actor = u.Principal(model.AuthSession)
	}
	ts.s.record(ctx, audit.Event{Action: audit.TokenCreated, Actor: actor, TargetType: "token", TargetID: row.ID.String(),
		Data: map[string]any{"name": row.Name, "scope": row.Scope, "user": u.Username, "restricted": !restr.IsZero()}})
	tok := Token{ID: row.ID, UserID: row.UserID, Username: u.Username, Name: row.Name, Prefix: row.Prefix, Scope: nt.Scope,
		Restrictions: restr, ExpiresAt: row.ExpiresAt, CreatedAt: row.CreatedAt}
	return &tok, secret, nil
}

// List implements Tokens: live (not revoked) Tokens, newest first.
func (ts *TokenStore) List(ctx context.Context, userID uuid.UUID) ([]Token, error) {
	q := ts.query(ts.s.db).Where("t.revoked_at IS NULL")
	if userID != uuid.Nil {
		q = q.Where("t.user_id = ?", userID)
	}
	var rows []tokenRow
	if err := q.OrderExpr("t.created_at DESC, t.id DESC").Scan(ctx, &rows); err != nil {
		return nil, err
	}
	out := make([]Token, 0, len(rows))
	for i := range rows {
		out = append(out, rows[i].toToken())
	}
	return out, nil
}

// ListPage lists every live Token, newest first, a page at a time, for
// the admin UI.
func (ts *TokenStore) ListPage(ctx context.Context, cursor string, limit int) ([]Token, string, error) {
	if limit <= 0 {
		limit = 50
	}
	limit = min(limit, 500)
	q := ts.query(ts.s.db).Where("t.revoked_at IS NULL")
	if cursor != "" {
		raw, err := base64.RawURLEncoding.DecodeString(cursor)
		var c struct {
			T time.Time `json:"t"`
			I uuid.UUID `json:"i"`
		}
		if err != nil || json.Unmarshal(raw, &c) != nil {
			return nil, "", fmt.Errorf("%w: bad cursor", ErrInvalid)
		}
		q = q.Where("(t.created_at < ? OR (t.created_at = ? AND t.id < ?))", c.T, c.T, c.I)
	}
	var rows []tokenRow
	if err := q.OrderExpr("t.created_at DESC, t.id DESC").Limit(int64(limit+1)).Scan(ctx, &rows); err != nil {
		return nil, "", err
	}
	next := ""
	if len(rows) > limit {
		rows = rows[:limit]
		last := rows[len(rows)-1]
		b, _ := json.Marshal(map[string]any{"t": last.CreatedAt, "i": last.ID})
		next = base64.RawURLEncoding.EncodeToString(b)
	}
	out := make([]Token, 0, len(rows))
	for i := range rows {
		out = append(out, rows[i].toToken())
	}
	return out, next, nil
}

// Revoke implements Tokens. Users revoke their own Tokens; Instance admins
// revoke any. Others get ErrNotFound, so Token IDs do not leak.
func (ts *TokenStore) Revoke(ctx context.Context, id uuid.UUID, actor model.Principal) error {
	var row tokenRow
	err := ts.query(ts.s.db).Where("t.id = ? AND t.revoked_at IS NULL", id).Scan(ctx, &row)
	if db.IsNotFound(err) {
		return fmt.Errorf("%w: token", ErrNotFound)
	}
	if err != nil {
		return err
	}
	if !actor.IsAdmin && actor.UserID != row.UserID {
		return fmt.Errorf("%w: token", ErrNotFound)
	}
	if _, err := ts.s.db.NewUpdate().Model((*db.Token)(nil)).Where("id = ? AND revoked_at IS NULL", id).
		Set("revoked_at = ?", ts.s.nowUTC()).Set("updated_at = ?", ts.s.nowUTC()).Exec(ctx); err != nil {
		return err
	}
	ts.s.record(ctx, audit.Event{Action: audit.TokenRevoked, Actor: actor, TargetType: "token", TargetID: id.String(),
		Data: map[string]any{"name": row.Name, "user": row.Username}})
	return nil
}

// Get returns a live Token by ID.
func (ts *TokenStore) Get(ctx context.Context, id uuid.UUID) (*Token, error) {
	var row tokenRow
	err := ts.query(ts.s.db).Where("t.id = ? AND t.revoked_at IS NULL", id).Scan(ctx, &row)
	if db.IsNotFound(err) {
		return nil, fmt.Errorf("%w: token", ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	t := row.toToken()
	return &t, nil
}

// authenticate turns a Token secret into a Principal. It returns
// ErrInvalidCredentials for unknown, revoked or expired Tokens and for
// disabled users.
func (ts *TokenStore) authenticate(ctx context.Context, secret string) (model.Principal, error) {
	if !LooksLikeToken(secret) {
		return model.Principal{}, fmt.Errorf("%w: malformed token", ErrInvalidCredentials)
	}
	var row db.Token
	err := ts.s.db.NewSelect().Model(&row).Where("hash = ?", hashSecret(secret)).Scan(ctx)
	if db.IsNotFound(err) {
		return model.Principal{}, fmt.Errorf("%w: unknown token", ErrInvalidCredentials)
	}
	if err != nil {
		return model.Principal{}, err
	}
	now := ts.s.nowUTC()
	if row.RevokedAt != nil {
		return model.Principal{}, fmt.Errorf("%w: revoked token", ErrInvalidCredentials)
	}
	if row.ExpiresAt != nil && !now.Before(*row.ExpiresAt) {
		return model.Principal{}, fmt.Errorf("%w: expired token", ErrInvalidCredentials)
	}
	u, err := ts.s.Users.forRequest(ctx, row.UserID)
	if err != nil {
		return model.Principal{}, fmt.Errorf("%w: unknown user", ErrInvalidCredentials)
	}
	if u.Disabled() {
		return model.Principal{}, fmt.Errorf("%w: %w", ErrInvalidCredentials, ErrDisabled)
	}
	if row.LastUsedAt == nil || now.Sub(*row.LastUsedAt) >= tokenTouchInterval {
		_, err := ts.s.db.NewUpdate().Model((*db.Token)(nil)).
			Where("id = ? AND (last_used_at IS NULL OR last_used_at < ?)", row.ID, now.Add(-tokenTouchInterval)).
			Set("last_used_at = ?", now).Exec(ctx)
		if err != nil {
			ts.s.log.WarnContext(ctx, "record token use", "error", err)
		}
	}
	p := u.Principal(model.AuthToken)
	p.Token = tokenInfo(&row)
	return p, nil
}

// tokenInfo is the part of a Token a Principal carries.
func tokenInfo(row *db.Token) *model.TokenInfo {
	return &model.TokenInfo{ID: row.ID, Name: row.Name, Scope: model.Scope(row.Scope),
		Restrictions: model.TokenRestrictions{Namespaces: row.Restrictions.Namespaces, Repos: row.Restrictions.Repos}, ExpiresAt: row.ExpiresAt}
}
