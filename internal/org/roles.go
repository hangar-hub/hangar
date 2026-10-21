package org

import (
	"context"

	"github.com/google/uuid"
	"github.com/uptrace/bun"

	"github.com/hangar-hub/hangar/internal/db"
	"github.com/hangar-hub/hangar/internal/model"
)

// RolePreloader is implemented by services that read one user's Roles for
// a page of Repos at once.
type RolePreloader interface {
	// PreloadRoles returns a context in which RoleIn and CollaboratorRole
	// answer for userID from Roles read up front: in every Namespace, and
	// as a collaborator on repoUUIDs. Other users and Repos are read as
	// usual. Use it for one request's permission checks, not across
	// changes to Roles.
	PreloadRoles(ctx context.Context, userID uuid.UUID, repoUUIDs []uuid.UUID) (context.Context, error)
}

var _ RolePreloader = (*DBService)(nil)

type rolesKey struct{}

// preloadedRoles are one user's Roles, as RoleIn and CollaboratorRole
// would read them.
type preloadedRoles struct {
	userID uuid.UUID
	// owned is the user's own Namespace, where they are admin.
	owned uuid.UUID
	// member holds the Roles in Organisations.
	member map[uuid.UUID]model.Role
	// repos are the Repos read, and collab the collaborator Roles among
	// them.
	repos  map[uuid.UUID]bool
	collab map[uuid.UUID]model.Role
}

// preloadBatch is how many Repos one PreloadRoles query reads.
const preloadBatch = 500

// PreloadRoles implements RolePreloader with one query for the user's
// Namespace Roles and one per 500 Repos for collaborator Roles.
func (o *DBService) PreloadRoles(ctx context.Context, userID uuid.UUID, repoUUIDs []uuid.UUID) (context.Context, error) {
	if userID == uuid.Nil {
		return ctx, nil
	}
	pr := &preloadedRoles{userID: userID, member: map[uuid.UUID]model.Role{}, repos: map[uuid.UUID]bool{}, collab: map[uuid.UUID]model.Role{}}
	// RoleIn gives a user admin in their own Namespace and no Role in any
	// other user's, whatever memberships say.
	var rows []struct {
		NamespaceID uuid.UUID `bun:"namespace_id"`
		Role        string    `bun:"role"`
	}
	if err := o.db.NewRaw(`SELECT namespace_id, '' AS role FROM users WHERE id = ?
		UNION ALL
		SELECT m.org_namespace_id AS namespace_id, m.role FROM memberships AS m
		WHERE m.user_id = ? AND NOT EXISTS (SELECT 1 FROM users AS u WHERE u.namespace_id = m.org_namespace_id)`, userID, userID).
		Scan(ctx, &rows); err != nil {
		return nil, err
	}
	for _, r := range rows {
		if r.Role == "" {
			pr.owned = r.NamespaceID
		} else {
			pr.member[r.NamespaceID] = model.Role(r.Role)
		}
	}
	for start := 0; start < len(repoUUIDs); start += preloadBatch {
		part := repoUUIDs[start:min(start+preloadBatch, len(repoUUIDs))]
		var collab []db.Collaborator
		if err := o.db.NewSelect().Model(&collab).Column("repo_id", "role").Where("user_id = ?", userID).
			Where("repo_id IN (?)", bun.List(part)).Scan(ctx); err != nil {
			return nil, err
		}
		for _, id := range part {
			pr.repos[id] = true
		}
		for _, c := range collab {
			pr.collab[c.RepoID] = model.Role(c.Role)
		}
	}
	return context.WithValue(ctx, rolesKey{}, pr), nil
}

// preloaded returns the preloaded Roles of userID, or nil.
func preloaded(ctx context.Context, userID uuid.UUID) *preloadedRoles {
	pr, _ := ctx.Value(rolesKey{}).(*preloadedRoles)
	if pr == nil || pr.userID != userID {
		return nil
	}
	return pr
}

func (pr *preloadedRoles) roleIn(namespaceID uuid.UUID) (model.Role, bool) {
	if namespaceID == pr.owned && pr.owned != uuid.Nil {
		return model.RoleAdmin, true
	}
	r, ok := pr.member[namespaceID]
	return r, ok
}

func (pr *preloadedRoles) collaboratorRole(repoUUID uuid.UUID) (role model.Role, ok, known bool) {
	if !pr.repos[repoUUID] {
		return "", false, false
	}
	r, ok := pr.collab[repoUUID]
	return r, ok, true
}
