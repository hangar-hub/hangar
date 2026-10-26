package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"

	"github.com/hangar-hub/hangar/internal/audit"
	"github.com/hangar-hub/hangar/internal/db"
	"github.com/hangar-hub/hangar/internal/model"
)

// Access request statuses.
const (
	AccessPending  = "pending"
	AccessAccepted = "accepted"
	AccessRejected = "rejected"
)

// Gating modes, as in repo.Gating.
const (
	GatingAuto   = "auto"
	GatingManual = "manual"
)

// GatedRepo describes a gated Repo for access requests. Handlers fill it
// from repo.Repo, so this package does not depend on internal/repo.
type GatedRepo struct {
	UUID uuid.UUID
	ID   model.RepoID
	// Mode is "auto" (requests are accepted at once) or "manual".
	Mode string
	// Fields are the request form's fields.
	Fields []GatingField
}

// GatingField is one field of the access request form.
type GatingField struct {
	Name     string
	Type     string
	Required bool
}

// AccessRequest is a request for access to a gated Repo (REPO-8).
type AccessRequest struct {
	ID        uuid.UUID
	RepoID    uuid.UUID
	UserID    uuid.UUID
	Username  string
	Status    string
	Fields    map[string]any
	CreatedAt time.Time
	DecidedAt *time.Time
	// DecidedBy is the username of the admin who decided, if any.
	DecidedBy string
}

// AccessRequestStore manages access requests. It owns the access_requests
// table.
type AccessRequestStore struct{ s *Service }

type accessRow struct {
	db.AccessRequest `bun:",extend"`
	Username         string  `bun:"username"`
	DecidedByName    *string `bun:"decided_by_name"`
}

func (r *accessRow) toRequest() AccessRequest {
	out := AccessRequest{ID: r.ID, RepoID: r.RepoID, UserID: r.UserID, Username: r.Username, Status: r.Status,
		Fields: r.Fields, CreatedAt: r.CreatedAt, DecidedAt: r.DecidedAt}
	if out.Fields == nil {
		out.Fields = map[string]any{}
	}
	if r.DecidedByName != nil {
		out.DecidedBy = *r.DecidedByName
	}
	return out
}

func (as *AccessRequestStore) query(idb bun.IDB) *bun.SelectQuery {
	return idb.NewSelect().TableExpr("access_requests AS ar").ColumnExpr("ar.*").
		ColumnExpr("n.name AS username").ColumnExpr("dn.name AS decided_by_name").
		Join("JOIN users AS u ON u.id = ar.user_id").Join("JOIN namespaces AS n ON n.id = u.namespace_id").
		Join("LEFT JOIN users AS du ON du.id = ar.decided_by").Join("LEFT JOIN namespaces AS dn ON dn.id = du.namespace_id")
}

func (as *AccessRequestStore) get(ctx context.Context, idb bun.IDB, where string, args ...any) (*AccessRequest, error) {
	var row accessRow
	err := as.query(idb).Where(where, args...).Limit(1).Scan(ctx, &row)
	if db.IsNotFound(err) {
		return nil, fmt.Errorf("%w: access request", ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	r := row.toRequest()
	return &r, nil
}

// validateFields checks a request against the form: required fields are
// present, unknown fields are refused, and values are short scalars.
func validateFields(form []GatingField, fields map[string]any) (map[string]any, error) {
	out := map[string]any{}
	known := map[string]GatingField{}
	for _, f := range form {
		known[f.Name] = f
	}
	for k, v := range fields {
		if _, ok := known[k]; !ok {
			return nil, fmt.Errorf("%w: the form has no field %q", ErrInvalid, k)
		}
		switch tv := v.(type) {
		case string:
			if len(tv) > 2000 {
				return nil, fmt.Errorf("%w: field %q is longer than 2,000 characters", ErrInvalid, k)
			}
		case bool, float64, nil:
		default:
			return nil, fmt.Errorf("%w: field %q must be text, a number or a checkbox", ErrInvalid, k)
		}
		out[k] = v
	}
	for _, f := range form {
		v, ok := out[f.Name]
		if !f.Required {
			continue
		}
		if !ok || v == nil || v == "" || v == false {
			return nil, fmt.Errorf("%w: field %q is required", ErrInvalid, f.Name)
		}
	}
	return out, nil
}

// Request asks for access. In auto mode the request is accepted at once.
// An existing pending or accepted request is returned unchanged; a
// rejected one gives ErrConflict.
func (as *AccessRequestStore) Request(ctx context.Context, repo GatedRepo, p model.Principal, fields map[string]any) (*AccessRequest, error) {
	if !p.IsUser() {
		return nil, fmt.Errorf("%w: sign in to request access", ErrForbidden)
	}
	if repo.Mode != GatingAuto && repo.Mode != GatingManual {
		return nil, fmt.Errorf("%w: %s is not gated", ErrInvalid, repo.ID)
	}
	clean, err := validateFields(repo.Fields, fields)
	if err != nil {
		return nil, err
	}
	if existing, err := as.get(ctx, as.s.db, "ar.repo_id = ? AND ar.user_id = ?", repo.UUID, p.UserID); err == nil {
		if existing.Status == AccessRejected {
			return nil, fmt.Errorf("%w: your request was rejected", ErrConflict)
		}
		return existing, nil
	}
	row := &db.AccessRequest{ID: db.NewID(), RepoID: repo.UUID, UserID: p.UserID, Status: AccessPending, Fields: clean}
	if repo.Mode == GatingAuto {
		now := as.s.nowUTC()
		row.Status, row.DecidedAt = AccessAccepted, &now
	}
	if _, err := as.s.db.NewInsert().Model(row).Exec(ctx); err != nil {
		if db.IsUniqueViolation(err) {
			return as.get(ctx, as.s.db, "ar.repo_id = ? AND ar.user_id = ?", repo.UUID, p.UserID)
		}
		return nil, err
	}
	if repo.Mode == GatingAuto {
		as.s.record(ctx, audit.Event{Action: audit.AccessRequestDecided, Actor: p, TargetType: "repo", TargetID: repoKey(repo.ID),
			Data: map[string]any{"user": p.Username, "status": AccessAccepted, "mode": GatingAuto}})
	}
	return as.get(ctx, as.s.db, "ar.id = ?", row.ID)
}

func repoKey(id model.RepoID) string { return string(id.Type) + ":" + id.String() }

// List returns a Repo's requests, oldest first, optionally by status.
func (as *AccessRequestStore) List(ctx context.Context, repoUUID uuid.UUID, status, cursor string, limit int) ([]AccessRequest, string, error) {
	if limit <= 0 {
		limit = 50
	}
	limit = min(limit, 500)
	q := as.query(as.s.db).Where("ar.repo_id = ?", repoUUID)
	if status != "" {
		switch status {
		case AccessPending, AccessAccepted, AccessRejected:
		default:
			return nil, "", fmt.Errorf("%w: status must be pending, accepted or rejected", ErrInvalid)
		}
		q = q.Where("ar.status = ?", status)
	}
	if cursor != "" {
		raw, err := base64.RawURLEncoding.DecodeString(cursor)
		var c struct {
			T time.Time `json:"t"`
			I uuid.UUID `json:"i"`
		}
		if err != nil || json.Unmarshal(raw, &c) != nil {
			return nil, "", fmt.Errorf("%w: bad cursor", ErrInvalid)
		}
		q = q.Where("(ar.created_at > ? OR (ar.created_at = ? AND ar.id > ?))", c.T, c.T, c.I)
	}
	var rows []accessRow
	if err := q.OrderExpr("ar.created_at ASC, ar.id ASC").Limit(int64(limit+1)).Scan(ctx, &rows); err != nil {
		return nil, "", err
	}
	next := ""
	if len(rows) > limit {
		rows = rows[:limit]
		last := rows[len(rows)-1]
		b, _ := json.Marshal(map[string]any{"t": last.CreatedAt, "i": last.ID})
		next = base64.RawURLEncoding.EncodeToString(b)
	}
	out := make([]AccessRequest, 0, len(rows))
	for i := range rows {
		out = append(out, rows[i].toRequest())
	}
	return out, next, nil
}

// Decide accepts or rejects a request and records a
// repo.access_request_decided audit event. The caller checks that actor
// is an admin of the Repo.
func (as *AccessRequestStore) Decide(ctx context.Context, repo GatedRepo, id uuid.UUID, status string, actor model.Principal) (*AccessRequest, error) {
	if status != AccessAccepted && status != AccessRejected {
		return nil, fmt.Errorf("%w: status must be accepted or rejected", ErrInvalid)
	}
	cur, err := as.get(ctx, as.s.db, "ar.id = ? AND ar.repo_id = ?", id, repo.UUID)
	if err != nil {
		return nil, err
	}
	now := as.s.nowUTC()
	q := as.s.db.NewUpdate().Model((*db.AccessRequest)(nil)).Where("id = ?", id).
		Set("status = ?", status).Set("decided_at = ?", now).Set("updated_at = ?", now)
	if actor.UserID != uuid.Nil {
		q = q.Set("decided_by = ?", actor.UserID)
	}
	if _, err := q.Exec(ctx); err != nil {
		return nil, err
	}
	as.s.record(ctx, audit.Event{Action: audit.AccessRequestDecided, Actor: actor, TargetType: "repo", TargetID: repoKey(repo.ID),
		Data: map[string]any{"user": cur.Username, "status": status, "previous_status": cur.Status}})
	return as.get(ctx, as.s.db, "ar.id = ?", id)
}

// Accepted reports whether a user has an accepted request for a Repo.
func (as *AccessRequestStore) Accepted(ctx context.Context, repoUUID, userID uuid.UUID) (bool, error) {
	n, err := as.s.db.NewSelect().Model((*db.AccessRequest)(nil)).
		Where("repo_id = ? AND user_id = ? AND status = ?", repoUUID, userID, AccessAccepted).Count(ctx)
	return n > 0, err
}

// Mine returns the caller's request for a Repo, or ErrNotFound.
func (as *AccessRequestStore) Mine(ctx context.Context, repoUUID, userID uuid.UUID) (*AccessRequest, error) {
	return as.get(ctx, as.s.db, "ar.repo_id = ? AND ar.user_id = ?", repoUUID, userID)
}
