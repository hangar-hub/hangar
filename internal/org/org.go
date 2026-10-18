// Package org manages Namespaces: Users' personal Namespaces, Organisations,
// their Members and Roles, and per-Repo collaborators (AUTH-5). orgtest has
// an in-memory fake.
package org

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/hangar-hub/hangar/internal/model"
)

// Namespace kinds.
const (
	KindUser = "user"
	KindOrg  = "org"
)

// Membership sources: set by hand, or by an OIDC group mapping.
const (
	SourceManual = "manual"
	SourceOIDC   = "oidc"
)

// Errors.
var (
	ErrNotFound = errors.New("namespace not found")
	ErrExists   = errors.New("namespace name is taken")
	// ErrLastAdmin stops removing or demoting an Organisation's last admin.
	ErrLastAdmin = errors.New("an organisation needs at least one admin")
	ErrInvalid   = errors.New("invalid request")
)

// Namespace is a User's or an Organisation's Namespace.
type Namespace struct {
	ID          uuid.UUID
	Name        string
	Kind        string
	DisplayName string
	Description string
	AvatarKey   string
	// ReservedMirror is true for Organisations created by a Pull to hold
	// Mirrors under their upstream Namespace.
	ReservedMirror bool
	// UserID is set for user Namespaces.
	UserID    uuid.UUID
	CreatedAt time.Time
}

// Member is a Member of an Organisation.
type Member struct {
	UserID      uuid.UUID
	Username    string
	DisplayName string
	Role        model.Role
	Source      string
}

// Membership is one of a User's Organisations, with their Role.
type Membership struct {
	Org  Namespace
	Role model.Role
}

// Collaborator has a Role on one Repo.
type Collaborator struct {
	UserID   uuid.UUID
	Username string
	Role     model.Role
}

// OrgUpdate changes an Organisation. Nil fields are unchanged.
type OrgUpdate struct {
	DisplayName *string
	Description *string
	AvatarKey   *string
}

// Service manages Namespaces, Members and collaborators.
type Service interface {
	// Get returns a Namespace by name, case-insensitively.
	Get(ctx context.Context, name string) (*Namespace, error)
	// GetByID returns a Namespace by ID.
	GetByID(ctx context.Context, id uuid.UUID) (*Namespace, error)
	// CreateOrg creates an Organisation with the creator as its admin.
	CreateOrg(ctx context.Context, name, displayName string, creator model.Principal) (*Namespace, error)
	// EnsureMirrorNamespace returns the Namespace for Mirrors from an
	// Upstream Namespace, creating it as an Instance-owned Organisation with
	// ReservedMirror set when it does not exist.
	EnsureMirrorNamespace(ctx context.Context, name string) (*Namespace, error)
	// UpdateOrg changes an Organisation.
	UpdateOrg(ctx context.Context, name string, u OrgUpdate, actor model.Principal) (*Namespace, error)
	// DeleteOrg deletes an empty Organisation.
	DeleteOrg(ctx context.Context, name string, actor model.Principal) error
	// Members lists an Organisation's Members.
	Members(ctx context.Context, org string) ([]Member, error)
	// SetMember adds a Member or changes their Role.
	SetMember(ctx context.Context, org string, userID uuid.UUID, role model.Role, source string, actor model.Principal) error
	// RemoveMember removes a Member.
	RemoveMember(ctx context.Context, org string, userID uuid.UUID, actor model.Principal) error
	// MembershipsOf lists a User's Organisations.
	MembershipsOf(ctx context.Context, userID uuid.UUID) ([]Membership, error)
	// RoleIn returns a User's Role in a Namespace: admin for their own
	// Namespace, their Member Role in an Organisation, or false.
	RoleIn(ctx context.Context, namespaceID, userID uuid.UUID) (model.Role, bool, error)
	// Collaborators lists a Repo's collaborators.
	Collaborators(ctx context.Context, repoUUID uuid.UUID) ([]Collaborator, error)
	// SetCollaborators replaces a Repo's collaborators.
	SetCollaborators(ctx context.Context, repoUUID uuid.UUID, cs []Collaborator, actor model.Principal) error
}
