package org

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/uptrace/bun"

	"github.com/hangar-hub/hangar/internal/audit"
	"github.com/hangar-hub/hangar/internal/db"
	"github.com/hangar-hub/hangar/internal/model"
)

// ErrNotEmpty stops deleting an Organisation that still owns Repos,
// including soft-deleted Repos that have not been purged yet.
var ErrNotEmpty = errors.New("the Organisation still owns Repos; delete or transfer them, and wait for deleted Repos to be purged")

// DBService is the database implementation of Service. It owns the
// namespaces, memberships and collaborators tables. It reads the users
// table, which internal/auth owns, only to show usernames; both packages
// belong to the same workstream.
type DBService struct {
	db    *db.DB
	audit audit.Recorder
}

var _ Service = (*DBService)(nil)

// NewDB returns a DBService. rec records audit events for Organisation,
// Membership and collaborator changes.
func NewDB(d *db.DB, rec audit.Recorder) *DBService {
	return &DBService{db: d, audit: rec}
}

type nsRow struct {
	db.Namespace `bun:",extend"`
	UserID       *uuid.UUID `bun:"user_id"`
}

func (r *nsRow) toNamespace() *Namespace {
	ns := &Namespace{
		ID: r.ID, Name: r.Name, Kind: r.Kind, DisplayName: r.DisplayName, Description: r.Description,
		ReservedMirror: r.ReservedMirror, CreatedAt: r.CreatedAt,
	}
	if r.AvatarKey != nil {
		ns.AvatarKey = *r.AvatarKey
	}
	if r.UserID != nil {
		ns.UserID = *r.UserID
	}
	return ns
}

const nsSelect = `SELECT n.id, n.name, n.kind, n.display_name, n.description, n.avatar_key, n.reserved_mirror,
	n.created_at, n.updated_at, u.id AS user_id FROM namespaces AS n LEFT JOIN users AS u ON u.namespace_id = n.id `

func (o *DBService) getWhere(ctx context.Context, idb bun.IDB, where string, arg any) (*Namespace, error) {
	var row nsRow
	if err := idb.NewRaw(nsSelect+where, arg).Scan(ctx, &row); err != nil {
		if db.IsNotFound(err) {
			return nil, fmt.Errorf("%w: %v", ErrNotFound, arg)
		}
		return nil, err
	}
	return row.toNamespace(), nil
}

// Get implements Service.
func (o *DBService) Get(ctx context.Context, name string) (*Namespace, error) {
	return o.getWhere(ctx, o.db, "WHERE lower(n.name) = lower(?)", name)
}

// GetByID implements Service.
func (o *DBService) GetByID(ctx context.Context, id uuid.UUID) (*Namespace, error) {
	return o.getWhere(ctx, o.db, "WHERE n.id = ?", id)
}

// InsertUserNamespace creates a User's personal Namespace inside tx and
// returns its ID. internal/auth calls it when it creates a User. It refuses
// reserved names and returns ErrExists when the name is taken.
func InsertUserNamespace(ctx context.Context, tx bun.IDB, name, displayName string) (uuid.UUID, error) {
	if err := model.ValidateNewNamespaceName(name); err != nil {
		return uuid.Nil, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	ns := &db.Namespace{ID: db.NewID(), Name: name, Kind: KindUser, DisplayName: orDefault(displayName, name)}
	if _, err := tx.NewInsert().Model(ns).Exec(ctx); err != nil {
		if db.IsUniqueViolation(err) {
			return uuid.Nil, fmt.Errorf("%w: %s", ErrExists, name)
		}
		return uuid.Nil, err
	}
	return ns.ID, nil
}

// NameTaken reports whether a Namespace with this name exists,
// case-insensitively.
func NameTaken(ctx context.Context, idb bun.IDB, name string) (bool, error) {
	n, err := idb.NewSelect().Model((*db.Namespace)(nil)).Where("lower(name) = lower(?)", name).Count(ctx)
	return n > 0, err
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func (o *DBService) insertOrg(ctx context.Context, tx bun.IDB, name, display string, reserved bool) (*db.Namespace, error) {
	ns := &db.Namespace{ID: db.NewID(), Name: name, Kind: KindOrg, DisplayName: orDefault(display, name), ReservedMirror: reserved}
	if _, err := tx.NewInsert().Model(ns).Exec(ctx); err != nil {
		if db.IsUniqueViolation(err) {
			return nil, fmt.Errorf("%w: %s", ErrExists, name)
		}
		return nil, err
	}
	return ns, nil
}

// CreateOrg implements Service.
func (o *DBService) CreateOrg(ctx context.Context, name, displayName string, creator model.Principal) (*Namespace, error) {
	if err := model.ValidateNewNamespaceName(name); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if !creator.IsUser() || creator.UserID == uuid.Nil {
		return nil, fmt.Errorf("%w: sign in to create an Organisation", ErrInvalid)
	}
	var created *db.Namespace
	err := o.db.InTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		ns, err := o.insertOrg(ctx, tx, name, displayName, false)
		if err != nil {
			return err
		}
		created = ns
		m := &db.Membership{ID: db.NewID(), OrgNamespaceID: ns.ID, UserID: creator.UserID, Role: string(model.RoleAdmin), Source: SourceManual}
		_, err = tx.NewInsert().Model(m).Exec(ctx)
		return err
	})
	if err != nil {
		return nil, err
	}
	o.record(ctx, audit.Event{Action: audit.OrgCreated, Actor: creator, TargetType: "namespace", TargetID: name})
	return o.GetByID(ctx, created.ID)
}

// EnsureOrg returns an Organisation by name, creating it with no Members
// when it does not exist. OIDC org mappings use it. It refuses a name that
// belongs to a User.
func (o *DBService) EnsureOrg(ctx context.Context, name string, actor model.Principal) (*Namespace, error) {
	ns, err := o.Get(ctx, name)
	switch {
	case err == nil:
		if ns.Kind != KindOrg {
			return nil, fmt.Errorf("%w: %s is a user", ErrExists, name)
		}
		return ns, nil
	case !errors.Is(err, ErrNotFound):
		return nil, err
	}
	if err := model.ValidateNewNamespaceName(name); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	row, err := o.insertOrg(ctx, o.db, name, name, false)
	if errors.Is(err, ErrExists) {
		return o.Get(ctx, name)
	}
	if err != nil {
		return nil, err
	}
	o.record(ctx, audit.Event{Action: audit.OrgCreated, Actor: actor, TargetType: "namespace", TargetID: name, Data: map[string]any{"source": SourceOIDC}})
	return o.GetByID(ctx, row.ID)
}

// EnsureMirrorNamespace implements Service.
func (o *DBService) EnsureMirrorNamespace(ctx context.Context, name string) (*Namespace, error) {
	if err := model.ValidateNamespaceName(name); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	check := func(ns *Namespace) (*Namespace, error) {
		if !ns.ReservedMirror {
			return nil, fmt.Errorf("%w: %s is a local Namespace; set a namespace_prefix for this Upstream", ErrExists, name)
		}
		return ns, nil
	}
	if ns, err := o.Get(ctx, name); err == nil {
		return check(ns)
	} else if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	row, err := o.insertOrg(ctx, o.db, name, name, true)
	if errors.Is(err, ErrExists) {
		// Another Pull created it at the same time.
		ns, gerr := o.Get(ctx, name)
		if gerr != nil {
			return nil, gerr
		}
		return check(ns)
	}
	if err != nil {
		return nil, err
	}
	return o.GetByID(ctx, row.ID)
}

func (o *DBService) getOrg(ctx context.Context, idb bun.IDB, name string) (*Namespace, error) {
	ns, err := o.getWhere(ctx, idb, "WHERE lower(n.name) = lower(?)", name)
	if err != nil {
		return nil, err
	}
	if ns.Kind != KindOrg {
		return nil, fmt.Errorf("%w: %s is not an Organisation", ErrNotFound, name)
	}
	return ns, nil
}

// UpdateOrg implements Service.
func (o *DBService) UpdateOrg(ctx context.Context, name string, u OrgUpdate, actor model.Principal) (*Namespace, error) {
	ns, err := o.getOrg(ctx, o.db, name)
	if err != nil {
		return nil, err
	}
	q := o.db.NewUpdate().Model((*db.Namespace)(nil)).Where("id = ?", ns.ID).Set("updated_at = ?", db.Now())
	changed := map[string]any{}
	if u.DisplayName != nil {
		if len(*u.DisplayName) > 100 {
			return nil, fmt.Errorf("%w: the display name can be at most 100 characters", ErrInvalid)
		}
		q = q.Set("display_name = ?", *u.DisplayName)
		changed["display_name"] = *u.DisplayName
	}
	if u.Description != nil {
		if len(*u.Description) > 2000 {
			return nil, fmt.Errorf("%w: the description can be at most 2,000 characters", ErrInvalid)
		}
		q = q.Set("description = ?", *u.Description)
		changed["description"] = true
	}
	if u.AvatarKey != nil {
		q = q.Set("avatar_key = ?", *u.AvatarKey)
		changed["avatar"] = true
	}
	if len(changed) > 0 {
		if _, err := q.Exec(ctx); err != nil {
			return nil, err
		}
	}
	return o.GetByID(ctx, ns.ID)
}

// DeleteOrg implements Service. It returns ErrNotEmpty while the
// Organisation owns any Repo row.
func (o *DBService) DeleteOrg(ctx context.Context, name string, actor model.Principal) error {
	ns, err := o.getOrg(ctx, o.db, name)
	if err != nil {
		return err
	}
	err = o.db.InTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		_, err := tx.NewDelete().Model((*db.Namespace)(nil)).Where("id = ?", ns.ID).Exec(ctx)
		return err
	})
	if db.IsForeignKeyViolation(err) {
		return ErrNotEmpty
	}
	if err != nil {
		return err
	}
	o.record(ctx, audit.Event{Action: audit.OrgDeleted, Actor: actor, TargetType: "namespace", TargetID: ns.Name})
	return nil
}

type memberRow struct {
	UserID      uuid.UUID `bun:"user_id"`
	Username    string    `bun:"username"`
	DisplayName string    `bun:"display_name"`
	Role        string    `bun:"role"`
	Source      string    `bun:"source"`
}

const memberSelect = `SELECT m.user_id, n.name AS username, n.display_name, m.role, m.source
	FROM memberships AS m JOIN users AS u ON u.id = m.user_id JOIN namespaces AS n ON n.id = u.namespace_id `

func (r memberRow) toMember() Member {
	return Member{UserID: r.UserID, Username: r.Username, DisplayName: r.DisplayName, Role: model.Role(r.Role), Source: r.Source}
}

// Members implements Service, ordered by username.
func (o *DBService) Members(ctx context.Context, orgName string) ([]Member, error) {
	ns, err := o.getOrg(ctx, o.db, orgName)
	if err != nil {
		return nil, err
	}
	var rows []memberRow
	if err := o.db.NewRaw(memberSelect+"WHERE m.org_namespace_id = ? ORDER BY lower(n.name)", ns.ID).Scan(ctx, &rows); err != nil {
		return nil, err
	}
	out := make([]Member, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.toMember())
	}
	return out, nil
}

// Member returns one Member of an Organisation by username. It returns
// ErrNotFound when the Organisation or the Membership does not exist.
func (o *DBService) Member(ctx context.Context, orgName, username string) (*Member, error) {
	ns, err := o.getOrg(ctx, o.db, orgName)
	if err != nil {
		return nil, err
	}
	var row memberRow
	err = o.db.NewRaw(memberSelect+"WHERE m.org_namespace_id = ? AND lower(n.name) = lower(?)", ns.ID, username).Scan(ctx, &row)
	if db.IsNotFound(err) {
		return nil, fmt.Errorf("%w: %s is not a member of %s", ErrNotFound, username, ns.Name)
	}
	if err != nil {
		return nil, err
	}
	m := row.toMember()
	return &m, nil
}

// lockOrg serialises Membership changes of one Organisation, so the
// last-admin check holds under concurrency. SQLite transactions already
// hold the write lock.
func (o *DBService) lockOrg(ctx context.Context, tx bun.Tx, id uuid.UUID) error {
	if o.db.IsSQLite() {
		return nil
	}
	_, err := tx.ExecContext(ctx, "SELECT id FROM namespaces WHERE id = ? FOR UPDATE", id)
	return err
}

func countAdmins(ctx context.Context, tx bun.Tx, orgID uuid.UUID) (int64, error) {
	return tx.NewSelect().Model((*db.Membership)(nil)).Where("org_namespace_id = ? AND role = ?", orgID, string(model.RoleAdmin)).Count(ctx)
}

func userExists(ctx context.Context, idb bun.IDB, userID uuid.UUID) (string, error) {
	var name string
	err := idb.NewRaw("SELECT n.name FROM users AS u JOIN namespaces AS n ON n.id = u.namespace_id WHERE u.id = ?", userID).Scan(ctx, &name)
	if db.IsNotFound(err) {
		return "", fmt.Errorf("%w: user %s", ErrNotFound, userID)
	}
	return name, err
}

// SetMember implements Service.
func (o *DBService) SetMember(ctx context.Context, orgName string, userID uuid.UUID, role model.Role, source string, actor model.Principal) error {
	if !role.Valid() {
		return fmt.Errorf("%w: role %q: use read, write or admin", ErrInvalid, role)
	}
	source = orDefault(source, SourceManual)
	if source != SourceManual && source != SourceOIDC {
		return fmt.Errorf("%w: source %q", ErrInvalid, source)
	}
	ns, err := o.getOrg(ctx, o.db, orgName)
	if err != nil {
		return err
	}
	var username, oldRole string
	changed := false
	err = o.db.InTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		if err := o.lockOrg(ctx, tx, ns.ID); err != nil {
			return err
		}
		username, err = userExists(ctx, tx, userID)
		if err != nil {
			return err
		}
		var cur db.Membership
		err := tx.NewSelect().Model(&cur).Where("org_namespace_id = ? AND user_id = ?", ns.ID, userID).Scan(ctx)
		switch {
		case db.IsNotFound(err):
			m := &db.Membership{ID: db.NewID(), OrgNamespaceID: ns.ID, UserID: userID, Role: string(role), Source: source}
			_, err = tx.NewInsert().Model(m).Exec(ctx)
			changed = true
			return err
		case err != nil:
			return err
		}
		oldRole = cur.Role
		if cur.Role == string(model.RoleAdmin) && role != model.RoleAdmin {
			n, err := countAdmins(ctx, tx, ns.ID)
			if err != nil {
				return err
			}
			if n <= 1 {
				return ErrLastAdmin
			}
		}
		// A manual change takes over a mapped Membership, so later
		// sign-ins leave it alone.
		if cur.Role == string(role) && cur.Source == source {
			return nil
		}
		changed = true
		_, err = tx.NewUpdate().Model((*db.Membership)(nil)).Where("id = ?", cur.ID).
			Set("role = ?", string(role)).Set("source = ?", source).Set("updated_at = ?", db.Now()).Exec(ctx)
		return err
	})
	if err != nil {
		return err
	}
	if changed {
		o.record(ctx, audit.Event{Action: audit.MembershipChanged, Actor: actor, TargetType: "namespace", TargetID: ns.Name,
			Data: map[string]any{"user": username, "role": string(role), "previous_role": oldRole, "source": source}})
	}
	return nil
}

// RemoveMember implements Service.
func (o *DBService) RemoveMember(ctx context.Context, orgName string, userID uuid.UUID, actor model.Principal) error {
	ns, err := o.getOrg(ctx, o.db, orgName)
	if err != nil {
		return err
	}
	var username string
	err = o.db.InTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		if err := o.lockOrg(ctx, tx, ns.ID); err != nil {
			return err
		}
		var cur db.Membership
		err := tx.NewSelect().Model(&cur).Where("org_namespace_id = ? AND user_id = ?", ns.ID, userID).Scan(ctx)
		if db.IsNotFound(err) {
			return fmt.Errorf("%w: member", ErrNotFound)
		}
		if err != nil {
			return err
		}
		if cur.Role == string(model.RoleAdmin) {
			n, err := countAdmins(ctx, tx, ns.ID)
			if err != nil {
				return err
			}
			if n <= 1 {
				return ErrLastAdmin
			}
		}
		username, _ = userExists(ctx, tx, userID)
		_, err = tx.NewDelete().Model((*db.Membership)(nil)).Where("id = ?", cur.ID).Exec(ctx)
		return err
	})
	if err != nil {
		return err
	}
	o.record(ctx, audit.Event{Action: audit.MembershipRemoved, Actor: actor, TargetType: "namespace", TargetID: ns.Name, Data: map[string]any{"user": username}})
	return nil
}

// MembershipsOf implements Service, ordered by Organisation name.
func (o *DBService) MembershipsOf(ctx context.Context, userID uuid.UUID) ([]Membership, error) {
	var rows []struct {
		nsRow
		Role string `bun:"role"`
	}
	err := o.db.NewRaw(`SELECT n.id, n.name, n.kind, n.display_name, n.description, n.avatar_key, n.reserved_mirror,
		n.created_at, n.updated_at, m.role FROM memberships AS m JOIN namespaces AS n ON n.id = m.org_namespace_id
		WHERE m.user_id = ? ORDER BY lower(n.name)`, userID).Scan(ctx, &rows)
	if err != nil {
		return nil, err
	}
	out := make([]Membership, 0, len(rows))
	for _, r := range rows {
		out = append(out, Membership{Org: *r.toNamespace(), Role: model.Role(r.Role)})
	}
	return out, nil
}

// RoleIn implements Service.
func (o *DBService) RoleIn(ctx context.Context, namespaceID, userID uuid.UUID) (model.Role, bool, error) {
	if userID == uuid.Nil {
		return "", false, nil
	}
	if pr := preloaded(ctx, userID); pr != nil {
		r, ok := pr.roleIn(namespaceID)
		return r, ok, nil
	}
	var owner uuid.UUID
	err := o.db.NewRaw("SELECT id FROM users WHERE namespace_id = ?", namespaceID).Scan(ctx, &owner)
	switch {
	case err == nil:
		if owner == userID {
			return model.RoleAdmin, true, nil
		}
		return "", false, nil
	case !db.IsNotFound(err):
		return "", false, err
	}
	var role string
	err = o.db.NewSelect().Model((*db.Membership)(nil)).Column("role").
		Where("org_namespace_id = ? AND user_id = ?", namespaceID, userID).Scan(ctx, &role)
	if db.IsNotFound(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return model.Role(role), true, nil
}

// CollaboratorRole returns a user's collaborator Role on a Repo, or false.
func (o *DBService) CollaboratorRole(ctx context.Context, repoUUID, userID uuid.UUID) (model.Role, bool, error) {
	if userID == uuid.Nil {
		return "", false, nil
	}
	if pr := preloaded(ctx, userID); pr != nil {
		if r, ok, known := pr.collaboratorRole(repoUUID); known {
			return r, ok, nil
		}
	}
	var role string
	err := o.db.NewSelect().Model((*db.Collaborator)(nil)).Column("role").
		Where("repo_id = ? AND user_id = ?", repoUUID, userID).Scan(ctx, &role)
	if db.IsNotFound(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return model.Role(role), true, nil
}

// Collaborators implements Service, ordered by username.
func (o *DBService) Collaborators(ctx context.Context, repoUUID uuid.UUID) ([]Collaborator, error) {
	var rows []struct {
		UserID   uuid.UUID `bun:"user_id"`
		Username string    `bun:"username"`
		Role     string    `bun:"role"`
	}
	err := o.db.NewRaw(`SELECT c.user_id, n.name AS username, c.role FROM collaborators AS c
		JOIN users AS u ON u.id = c.user_id JOIN namespaces AS n ON n.id = u.namespace_id
		WHERE c.repo_id = ? ORDER BY lower(n.name)`, repoUUID).Scan(ctx, &rows)
	if err != nil {
		return nil, err
	}
	out := make([]Collaborator, 0, len(rows))
	for _, r := range rows {
		out = append(out, Collaborator{UserID: r.UserID, Username: r.Username, Role: model.Role(r.Role)})
	}
	return out, nil
}

// SetCollaborators implements Service. Collaborators get read or write.
func (o *DBService) SetCollaborators(ctx context.Context, repoUUID uuid.UUID, cs []Collaborator, actor model.Principal) error {
	seen := map[uuid.UUID]bool{}
	for _, c := range cs {
		if c.Role != model.RoleRead && c.Role != model.RoleWrite {
			return fmt.Errorf("%w: collaborators get read or write, not %q", ErrInvalid, c.Role)
		}
		if seen[c.UserID] {
			return fmt.Errorf("%w: a user is listed twice", ErrInvalid)
		}
		seen[c.UserID] = true
	}
	var names []string
	err := o.db.InTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		for _, c := range cs {
			name, err := userExists(ctx, tx, c.UserID)
			if err != nil {
				return err
			}
			names = append(names, name+":"+string(c.Role))
		}
		if _, err := tx.NewDelete().Model((*db.Collaborator)(nil)).Where("repo_id = ?", repoUUID).Exec(ctx); err != nil {
			return err
		}
		for _, c := range cs {
			row := &db.Collaborator{ID: db.NewID(), RepoID: repoUUID, UserID: c.UserID, Role: string(c.Role)}
			if _, err := tx.NewInsert().Model(row).Exec(ctx); err != nil {
				if db.IsForeignKeyViolation(err) {
					return fmt.Errorf("%w: repo %s", ErrNotFound, repoUUID)
				}
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	slices.Sort(names)
	o.record(ctx, audit.Event{Action: audit.CollaboratorsChanged, Actor: actor, TargetType: "repo", TargetID: repoUUID.String(),
		Data: map[string]any{"collaborators": strings.Join(names, ",")}})
	return nil
}

// OIDCMapping is one Organisation Role granted by an OIDC group mapping.
type OIDCMapping struct {
	Org  string
	Role model.Role
}

// SyncOIDCMemberships makes a user's mapped Memberships from provider
// match want: it adds or updates the mapped ones, and removes Memberships
// that came from this provider's mappings but are no longer wanted.
// Manual Memberships are never touched. When a user is granted several
// Roles in one Organisation, the highest wins. Missing Organisations are
// created.
func (o *DBService) SyncOIDCMemberships(ctx context.Context, userID uuid.UUID, provider string, want []OIDCMapping, actor model.Principal) error {
	best := map[string]OIDCMapping{}
	for _, m := range want {
		key := strings.ToLower(m.Org)
		if cur, ok := best[key]; !ok || m.Role.Includes(cur.Role) {
			best[key] = m
		}
	}
	wantIDs := map[uuid.UUID]model.Role{}
	for _, m := range best {
		ns, err := o.EnsureOrg(ctx, m.Org, actor)
		if err != nil {
			return fmt.Errorf("org mapping %s: %w", m.Org, err)
		}
		wantIDs[ns.ID] = m.Role
	}
	username, err := userExists(ctx, o.db, userID)
	if err != nil {
		return err
	}
	var current []db.Membership
	if err := o.db.NewSelect().Model(&current).Where("user_id = ?", userID).Scan(ctx); err != nil {
		return err
	}
	have := map[uuid.UUID]db.Membership{}
	for _, m := range current {
		have[m.OrgNamespaceID] = m
	}
	for nsID, role := range wantIDs {
		cur, ok := have[nsID]
		if ok && cur.Source == SourceManual {
			continue
		}
		if ok && cur.Role == string(role) {
			continue
		}
		ns, err := o.GetByID(ctx, nsID)
		if err != nil {
			return err
		}
		if ok {
			if _, err := o.db.NewUpdate().Model((*db.Membership)(nil)).Where("id = ?", cur.ID).
				Set("role = ?", string(role)).Set("source_provider = ?", provider).Set("updated_at = ?", db.Now()).Exec(ctx); err != nil {
				return err
			}
		} else {
			p := provider
			m := &db.Membership{ID: db.NewID(), OrgNamespaceID: nsID, UserID: userID, Role: string(role), Source: SourceOIDC, SourceProvider: &p}
			if _, err := o.db.NewInsert().Model(m).Exec(ctx); err != nil {
				return err
			}
		}
		o.record(ctx, audit.Event{Action: audit.MembershipChanged, Actor: actor, TargetType: "namespace", TargetID: ns.Name,
			Data: map[string]any{"user": username, "role": string(role), "previous_role": cur.Role, "source": SourceOIDC, "provider": provider}})
	}
	for nsID, cur := range have {
		if cur.Source != SourceOIDC || cur.SourceProvider == nil || *cur.SourceProvider != provider {
			continue
		}
		if _, ok := wantIDs[nsID]; ok {
			continue
		}
		if _, err := o.db.NewDelete().Model((*db.Membership)(nil)).Where("id = ?", cur.ID).Exec(ctx); err != nil {
			return err
		}
		name := nsID.String()
		if ns, err := o.GetByID(ctx, nsID); err == nil {
			name = ns.Name
		}
		o.record(ctx, audit.Event{Action: audit.MembershipRemoved, Actor: actor, TargetType: "namespace", TargetID: name,
			Data: map[string]any{"user": username, "source": SourceOIDC, "provider": provider}})
	}
	return nil
}

// SearchNamespaces returns up to limit Namespaces whose name or display
// name contains query, ignoring case, in name order. The system user's
// Namespace is never returned. It serves the Namespace part of /search.
func (o *DBService) SearchNamespaces(ctx context.Context, query string, limit int) ([]Namespace, error) {
	if limit <= 0 {
		limit = 20
	}
	pattern := "%" + likeEscaper.Replace(strings.ToLower(query)) + "%"
	var rows []nsRow
	err := o.db.NewRaw(nsSelect+`WHERE (lower(n.name) LIKE ? ESCAPE '\' OR lower(n.display_name) LIKE ? ESCAPE '\')
	AND (u.id IS NULL OR u.is_system = ?) ORDER BY lower(n.name) LIMIT ?`, pattern, pattern, false, limit).Scan(ctx, &rows)
	if err != nil {
		return nil, err
	}
	out := make([]Namespace, 0, len(rows))
	for i := range rows {
		out = append(out, *rows[i].toNamespace())
	}
	return out, nil
}

// ListOrgs returns Organisations in name order, those whose name or
// display name contains q (ignoring case) when q is set, at most limit
// (default 50, at most 500) after cursor, and the cursor of the next page
// or "".
func (o *DBService) ListOrgs(ctx context.Context, q, cursor string, limit int) ([]Namespace, string, error) {
	if limit <= 0 {
		limit = 50
	}
	limit = min(limit, 500)
	where := "WHERE n.kind = ?"
	args := []any{KindOrg}
	if q != "" {
		pattern := "%" + likeEscaper.Replace(strings.ToLower(q)) + "%"
		where += ` AND (lower(n.name) LIKE ? ESCAPE '\' OR lower(n.display_name) LIKE ? ESCAPE '\')`
		args = append(args, pattern, pattern)
	}
	if cursor != "" {
		after, err := base64.RawURLEncoding.DecodeString(cursor)
		if err != nil {
			return nil, "", fmt.Errorf("%w: the cursor is not valid", ErrInvalid)
		}
		where += " AND lower(n.name) > ?"
		args = append(args, string(after))
	}
	args = append(args, limit+1)
	var rows []nsRow
	if err := o.db.NewRaw(nsSelect+where+" ORDER BY lower(n.name) LIMIT ?", args...).Scan(ctx, &rows); err != nil {
		return nil, "", err
	}
	next := ""
	if len(rows) > limit {
		rows = rows[:limit]
		next = base64.RawURLEncoding.EncodeToString([]byte(strings.ToLower(rows[len(rows)-1].Name)))
	}
	out := make([]Namespace, 0, len(rows))
	for i := range rows {
		out = append(out, *rows[i].toNamespace())
	}
	return out, next, nil
}

// likeEscaper escapes the LIKE wildcards and the escape character itself.
var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// MemberCount returns the number of Members of an Organisation.
func (o *DBService) MemberCount(ctx context.Context, namespaceID uuid.UUID) (int64, error) {
	return o.db.NewSelect().Model((*db.Membership)(nil)).Where("org_namespace_id = ?", namespaceID).Count(ctx)
}

func (o *DBService) record(ctx context.Context, e audit.Event) {
	if o.audit == nil {
		return
	}
	_ = o.audit.Record(ctx, e)
}
