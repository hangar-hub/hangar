// Package orgtest provides an in-memory fake of org.Service.
package orgtest

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/hangar-hub/hangar/internal/model"
	"github.com/hangar-hub/hangar/internal/org"
)

// Fake is an in-memory org.Service. Add users with AddUser.
type Fake struct {
	mu        sync.Mutex
	byID      map[uuid.UUID]*org.Namespace
	members   map[uuid.UUID]map[uuid.UUID]org.Member
	collabs   map[uuid.UUID][]org.Collaborator
	usernames map[uuid.UUID]string
}

var _ org.Service = (*Fake)(nil)

// NewFake returns an empty Fake.
func NewFake() *Fake {
	return &Fake{byID: map[uuid.UUID]*org.Namespace{}, members: map[uuid.UUID]map[uuid.UUID]org.Member{},
		collabs: map[uuid.UUID][]org.Collaborator{}, usernames: map[uuid.UUID]string{}}
}

// AddUser creates a user's personal Namespace and returns a Principal for them.
func (f *Fake) AddUser(username string) (model.Principal, error) {
	if err := model.ValidateNamespaceName(username); err != nil {
		return model.Principal{}, fmt.Errorf("%w: %w", org.ErrInvalid, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.byNameLocked(username) != nil {
		return model.Principal{}, org.ErrExists
	}
	userID := uuid.New()
	ns := &org.Namespace{ID: uuid.New(), Name: username, Kind: org.KindUser, DisplayName: username, UserID: userID, CreatedAt: time.Now().UTC()}
	f.byID[ns.ID] = ns
	f.usernames[userID] = username
	return model.Principal{Kind: model.PrincipalUser, UserID: userID, NamespaceID: ns.ID, Username: username, DisplayName: username, Method: model.AuthSession}, nil
}

func (f *Fake) byNameLocked(name string) *org.Namespace {
	for _, ns := range f.byID {
		if strings.EqualFold(ns.Name, name) {
			return ns
		}
	}
	return nil
}

func (f *Fake) orgLocked(name string) (*org.Namespace, error) {
	ns := f.byNameLocked(name)
	if ns == nil || ns.Kind != org.KindOrg {
		return nil, fmt.Errorf("%w: %s", org.ErrNotFound, name)
	}
	return ns, nil
}

// Get implements org.Service.
func (f *Fake) Get(_ context.Context, name string) (*org.Namespace, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ns := f.byNameLocked(name)
	if ns == nil {
		return nil, fmt.Errorf("%w: %s", org.ErrNotFound, name)
	}
	out := *ns
	return &out, nil
}

// GetByID implements org.Service.
func (f *Fake) GetByID(_ context.Context, id uuid.UUID) (*org.Namespace, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ns, ok := f.byID[id]
	if !ok {
		return nil, fmt.Errorf("%w: %s", org.ErrNotFound, id)
	}
	out := *ns
	return &out, nil
}

func (f *Fake) createOrgLocked(name, display string, reserved bool) (*org.Namespace, error) {
	if f.byNameLocked(name) != nil {
		return nil, fmt.Errorf("%w: %s", org.ErrExists, name)
	}
	ns := &org.Namespace{ID: uuid.New(), Name: name, Kind: org.KindOrg, DisplayName: display, ReservedMirror: reserved, CreatedAt: time.Now().UTC()}
	f.byID[ns.ID] = ns
	f.members[ns.ID] = map[uuid.UUID]org.Member{}
	return ns, nil
}

// CreateOrg implements org.Service.
func (f *Fake) CreateOrg(_ context.Context, name, displayName string, creator model.Principal) (*org.Namespace, error) {
	if err := model.ValidateNewNamespaceName(name); err != nil {
		return nil, fmt.Errorf("%w: %w", org.ErrInvalid, err)
	}
	if creator.IsAnonymous() {
		return nil, fmt.Errorf("%w: sign in to create an organisation", org.ErrInvalid)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	ns, err := f.createOrgLocked(name, orDefault(displayName, name), false)
	if err != nil {
		return nil, err
	}
	f.members[ns.ID][creator.UserID] = org.Member{UserID: creator.UserID, Username: creator.Username, Role: model.RoleAdmin, Source: org.SourceManual}
	out := *ns
	return &out, nil
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// EnsureMirrorNamespace implements org.Service.
func (f *Fake) EnsureMirrorNamespace(_ context.Context, name string) (*org.Namespace, error) {
	if err := model.ValidateNamespaceName(name); err != nil {
		return nil, fmt.Errorf("%w: %w", org.ErrInvalid, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if ns := f.byNameLocked(name); ns != nil {
		if !ns.ReservedMirror {
			return nil, fmt.Errorf("%w: %s is a local namespace; set a namespace_prefix for this Upstream", org.ErrExists, name)
		}
		out := *ns
		return &out, nil
	}
	ns, err := f.createOrgLocked(name, name, true)
	if err != nil {
		return nil, err
	}
	out := *ns
	return &out, nil
}

// UpdateOrg implements org.Service.
func (f *Fake) UpdateOrg(_ context.Context, name string, u org.OrgUpdate, _ model.Principal) (*org.Namespace, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ns, err := f.orgLocked(name)
	if err != nil {
		return nil, err
	}
	if u.DisplayName != nil {
		ns.DisplayName = *u.DisplayName
	}
	if u.Description != nil {
		ns.Description = *u.Description
	}
	if u.AvatarKey != nil {
		ns.AvatarKey = *u.AvatarKey
	}
	out := *ns
	return &out, nil
}

// DeleteOrg implements org.Service.
func (f *Fake) DeleteOrg(_ context.Context, name string, _ model.Principal) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	ns, err := f.orgLocked(name)
	if err != nil {
		return err
	}
	delete(f.byID, ns.ID)
	delete(f.members, ns.ID)
	return nil
}

// Members implements org.Service.
func (f *Fake) Members(_ context.Context, name string) ([]org.Member, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ns, err := f.orgLocked(name)
	if err != nil {
		return nil, err
	}
	out := make([]org.Member, 0, len(f.members[ns.ID]))
	for _, m := range f.members[ns.ID] {
		out = append(out, m)
	}
	slices.SortFunc(out, func(a, b org.Member) int { return strings.Compare(a.Username, b.Username) })
	return out, nil
}

func (f *Fake) admins(nsID uuid.UUID) int {
	n := 0
	for _, m := range f.members[nsID] {
		if m.Role == model.RoleAdmin {
			n++
		}
	}
	return n
}

// SetMember implements org.Service.
func (f *Fake) SetMember(_ context.Context, name string, userID uuid.UUID, role model.Role, source string, _ model.Principal) error {
	if !role.Valid() {
		return fmt.Errorf("%w: role %q", org.ErrInvalid, role)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	ns, err := f.orgLocked(name)
	if err != nil {
		return err
	}
	username, ok := f.usernames[userID]
	if !ok {
		return fmt.Errorf("%w: user %s", org.ErrNotFound, userID)
	}
	if old, ok := f.members[ns.ID][userID]; ok && old.Role == model.RoleAdmin && role != model.RoleAdmin && f.admins(ns.ID) == 1 {
		return org.ErrLastAdmin
	}
	f.members[ns.ID][userID] = org.Member{UserID: userID, Username: username, Role: role, Source: orDefault(source, org.SourceManual)}
	return nil
}

// RemoveMember implements org.Service.
func (f *Fake) RemoveMember(_ context.Context, name string, userID uuid.UUID, _ model.Principal) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	ns, err := f.orgLocked(name)
	if err != nil {
		return err
	}
	m, ok := f.members[ns.ID][userID]
	if !ok {
		return fmt.Errorf("%w: member", org.ErrNotFound)
	}
	if m.Role == model.RoleAdmin && f.admins(ns.ID) == 1 {
		return org.ErrLastAdmin
	}
	delete(f.members[ns.ID], userID)
	return nil
}

// MembershipsOf implements org.Service.
func (f *Fake) MembershipsOf(_ context.Context, userID uuid.UUID) ([]org.Membership, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []org.Membership
	for nsID, members := range f.members {
		if m, ok := members[userID]; ok {
			out = append(out, org.Membership{Org: *f.byID[nsID], Role: m.Role})
		}
	}
	slices.SortFunc(out, func(a, b org.Membership) int { return strings.Compare(a.Org.Name, b.Org.Name) })
	return out, nil
}

// RoleIn implements org.Service.
func (f *Fake) RoleIn(_ context.Context, namespaceID, userID uuid.UUID) (model.Role, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ns, ok := f.byID[namespaceID]
	if !ok {
		return "", false, fmt.Errorf("%w: %s", org.ErrNotFound, namespaceID)
	}
	if ns.Kind == org.KindUser {
		if ns.UserID == userID {
			return model.RoleAdmin, true, nil
		}
		return "", false, nil
	}
	m, ok := f.members[namespaceID][userID]
	return m.Role, ok, nil
}

// Collaborators implements org.Service.
func (f *Fake) Collaborators(_ context.Context, repoUUID uuid.UUID) ([]org.Collaborator, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.collabs[repoUUID]), nil
}

// SetCollaborators implements org.Service.
func (f *Fake) SetCollaborators(_ context.Context, repoUUID uuid.UUID, cs []org.Collaborator, _ model.Principal) error {
	for _, c := range cs {
		if c.Role != model.RoleRead && c.Role != model.RoleWrite {
			return fmt.Errorf("%w: collaborators get read or write, not %q", org.ErrInvalid, c.Role)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]org.Collaborator, 0, len(cs))
	for _, c := range cs {
		username, ok := f.usernames[c.UserID]
		if !ok {
			return fmt.Errorf("%w: user %s", org.ErrNotFound, c.UserID)
		}
		c.Username = username
		out = append(out, c)
	}
	f.collabs[repoUUID] = out
	return nil
}
