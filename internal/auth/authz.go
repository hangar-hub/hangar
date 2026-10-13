package auth

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/jamesagarside/hangar/internal/model"
)

// Authorizer is the database implementation of Authz. It applies the
// permission table in docs/spec/05-auth.md:
//
//	Action                  Public Repo          Internal Repo   Private Repo
//	read, read_files        anyone if anonymous  signed in       Namespace members,
//	                        read is on, else                     collaborators,
//	                        signed in                            Instance admins
//	pull_mirror             write                write           write
//	write                   write                write           write
//	discuss                 signed in            signed in       readers
//	admin                   Namespace admin      same            same
//
// Callers who cannot read a Repo get ErrNotFound for every action, so
// private Repo names do not leak. Gated Repos need an accepted access
// request for read_files, unless the caller has a Role on the Repo.
// Token scopes (read < write < admin) and restrictions narrow these rules
// and never widen them.
type Authorizer struct{ s *Service }

var _ Authz = (*Authorizer)(nil)

// requiredScope is the Token scope an action needs.
func requiredScope(a Action) model.Scope {
	switch a {
	case ActionWrite, ActionPullMirror, ActionDiscuss, ActionCreateRepo:
		return model.ScopeWrite
	case ActionAdmin, ActionManageNamespace, ActionInstanceAdmin:
		return model.ScopeAdmin
	default:
		return model.ScopeRead
	}
}

// requiredRole is the Repo or Namespace Role an action needs, for
// actions that need one.
func requiredRole(a Action) model.Role {
	switch a {
	case ActionWrite, ActionPullMirror, ActionCreateRepo:
		return model.RoleWrite
	case ActionAdmin, ActionManageNamespace:
		return model.RoleAdmin
	default:
		return model.RoleRead
	}
}

// Can implements Authz.
func (a *Authorizer) Can(ctx context.Context, p model.Principal, action Action, t Target) error {
	switch action {
	case ActionRead, ActionReadFiles, ActionPullMirror, ActionWrite, ActionDiscuss, ActionAdmin,
		ActionCreateRepo, ActionManageNamespace, ActionInstanceAdmin:
	default:
		return fmt.Errorf("%w: unknown action %q", ErrForbidden, action)
	}
	if action == ActionInstanceAdmin {
		return a.canInstance(p)
	}
	switch {
	case t.Repo != nil:
		return a.canRepo(ctx, p, action, t.Repo)
	case t.Namespace != nil:
		return a.canNamespace(ctx, p, action, t.Namespace)
	default:
		return fmt.Errorf("%w: %s needs a Repo or Namespace target", ErrForbidden, action)
	}
}

func (a *Authorizer) canInstance(p model.Principal) error {
	if !p.IsAdmin {
		return fmt.Errorf("%w: only Instance admins can do this", ErrForbidden)
	}
	if p.Token != nil && (!p.Token.Scope.Includes(model.ScopeAdmin) || !p.Token.Restrictions.IsZero()) {
		return fmt.Errorf("%w: Instance administration needs an unrestricted token with the admin scope", ErrForbidden)
	}
	return nil
}

// role returns the Principal's Role in a Namespace and, when repoUUID is
// set, as a collaborator on that Repo: the higher of the two. Instance
// admins and the system user have admin everywhere.
func (a *Authorizer) role(ctx context.Context, p model.Principal, nsID uuid.UUID, repoUUID uuid.UUID) (model.Role, error) {
	if p.IsAnonymous() {
		return "", nil
	}
	if p.IsAdmin {
		return model.RoleAdmin, nil
	}
	var best model.Role
	if nsID != uuid.Nil {
		r, ok, err := a.s.orgs.RoleIn(ctx, nsID, p.UserID)
		if err != nil {
			return "", err
		}
		if ok {
			best = r
		}
	}
	if repoUUID != uuid.Nil {
		r, ok, err := a.s.orgs.CollaboratorRole(ctx, repoUUID, p.UserID)
		if err != nil {
			return "", err
		}
		if ok && !best.Includes(r) {
			best = r
		}
	}
	return best, nil
}

func (a *Authorizer) readable(ctx context.Context, p model.Principal, r *RepoTarget, role model.Role) (bool, error) {
	if !p.AllowsRepo(r.ID) {
		return false, nil
	}
	switch r.Visibility {
	case model.VisibilityPublic:
		if !p.IsAnonymous() {
			return true, nil
		}
		st, err := a.s.Settings.Get(ctx)
		if err != nil {
			return false, err
		}
		return st.AnonymousRead, nil
	case model.VisibilityInternal:
		return !p.IsAnonymous(), nil
	default:
		return role.Includes(model.RoleRead), nil
	}
}

func (a *Authorizer) canRepo(ctx context.Context, p model.Principal, action Action, r *RepoTarget) error {
	role, err := a.role(ctx, p, r.NamespaceID, r.UUID)
	if err != nil {
		return err
	}
	ok, err := a.readable(ctx, p, r, role)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: repo %s", ErrNotFound, r.ID)
	}
	if !p.IsAnonymous() && !p.MaxScope().Includes(requiredScope(action)) {
		return fmt.Errorf("%w: the token's %s scope does not allow this; it needs %s", ErrForbidden, p.MaxScope(), requiredScope(action))
	}
	switch action {
	case ActionRead:
		return nil
	case ActionReadFiles:
		if !r.Gated || role.Includes(model.RoleRead) {
			return nil
		}
		if p.IsAnonymous() {
			return fmt.Errorf("%w: sign in and request access", ErrGated)
		}
		accepted, err := a.s.AccessRequests.Accepted(ctx, r.UUID, p.UserID)
		if err != nil {
			return err
		}
		if accepted {
			return nil
		}
		return ErrGated
	case ActionDiscuss:
		if p.IsAnonymous() {
			return fmt.Errorf("%w: sign in to take part", ErrForbidden)
		}
		return nil
	default:
		need := requiredRole(action)
		if role.Includes(need) {
			return nil
		}
		if p.IsAnonymous() {
			return fmt.Errorf("%w: sign in first", ErrForbidden)
		}
		return fmt.Errorf("%w: this needs the %s role on %s", ErrForbidden, need, r.ID.Namespace)
	}
}

func (a *Authorizer) canNamespace(ctx context.Context, p model.Principal, action Action, ns *NamespaceTarget) error {
	switch action {
	case ActionCreateRepo, ActionManageNamespace, ActionAdmin, ActionWrite, ActionRead:
	default:
		return fmt.Errorf("%w: %s does not apply to a Namespace", ErrForbidden, action)
	}
	if action == ActionRead {
		// Namespace profiles are public; the Repos in them are checked one by one.
		return nil
	}
	if p.IsAnonymous() {
		return fmt.Errorf("%w: sign in first", ErrForbidden)
	}
	if p.Token != nil {
		if !p.Token.Scope.Includes(requiredScope(action)) {
			return fmt.Errorf("%w: the token's %s scope does not allow this; it needs %s", ErrForbidden, p.Token.Scope, requiredScope(action))
		}
		if !p.Token.Restrictions.IsZero() && !containsFold(p.Token.Restrictions.Namespaces, ns.Name) {
			return fmt.Errorf("%w: the token is restricted to other Namespaces", ErrForbidden)
		}
	}
	nsID := ns.ID
	if nsID == uuid.Nil && ns.Name != "" {
		got, err := a.s.orgs.Get(ctx, ns.Name)
		if err != nil {
			return fmt.Errorf("%w: namespace %s", ErrNotFound, ns.Name)
		}
		nsID = got.ID
	}
	role, err := a.role(ctx, p, nsID, uuid.Nil)
	if err != nil {
		return err
	}
	if need := requiredRole(action); !role.Includes(need) {
		return fmt.Errorf("%w: this needs the %s role in %s", ErrForbidden, need, ns.Name)
	}
	return nil
}

func containsFold(list []string, s string) bool {
	for _, v := range list {
		if strings.EqualFold(v, s) {
			return true
		}
	}
	return false
}
