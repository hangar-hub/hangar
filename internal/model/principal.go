package model

import (
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// Role is a Member's permission level in an Organisation, or a
// collaborator's level on a Repo.
type Role string

// Roles, from lowest to highest.
const (
	RoleRead  Role = "read"
	RoleWrite Role = "write"
	RoleAdmin Role = "admin"
)

func roleRank(r Role) int {
	switch r {
	case RoleRead:
		return 1
	case RoleWrite:
		return 2
	case RoleAdmin:
		return 3
	default:
		return 0
	}
}

// ParseRole parses a Role.
func ParseRole(s string) (Role, error) {
	r := Role(strings.ToLower(s))
	if roleRank(r) == 0 {
		return "", fmt.Errorf("invalid role %q: use read, write or admin", s)
	}
	return r, nil
}

// Valid reports whether r is a known Role.
func (r Role) Valid() bool { return roleRank(r) > 0 }

// Includes reports whether r grants at least other: admin includes write,
// and write includes read.
func (r Role) Includes(other Role) bool { return roleRank(r) >= roleRank(other) && roleRank(other) > 0 }

// Scope is a Token scope. Scopes nest like Roles.
type Scope string

// Token scopes.
const (
	ScopeRead  Scope = "read"
	ScopeWrite Scope = "write"
	ScopeAdmin Scope = "admin"
)

// ParseScope parses a Scope.
func ParseScope(s string) (Scope, error) {
	sc := Scope(strings.ToLower(s))
	if !sc.Valid() {
		return "", fmt.Errorf("invalid scope %q: use read, write or admin", s)
	}
	return sc, nil
}

// Valid reports whether s is a known Scope.
func (s Scope) Valid() bool { return roleRank(Role(s)) > 0 }

// Includes reports whether s grants at least other.
func (s Scope) Includes(other Scope) bool { return Role(s).Includes(Role(other)) }

// PrincipalKind says who is making a request.
type PrincipalKind string

// Principal kinds.
const (
	// PrincipalAnonymous is a request with no credentials.
	PrincipalAnonymous PrincipalKind = "anonymous"
	// PrincipalUser is a signed-in User, through a session, a Token or an Ollama key.
	PrincipalUser PrincipalKind = "user"
	// PrincipalSystem is Hangar itself, for example `hangar-bot` writing
	// Parquet exports or a worker running a Pull.
	PrincipalSystem PrincipalKind = "system"
)

// AuthMethod is how a Principal authenticated.
type AuthMethod string

// Authentication methods, matching the `method` attribute of the
// `hangar.auth.attempts` metric.
const (
	AuthNone      AuthMethod = ""
	AuthSession   AuthMethod = "session"
	AuthPassword  AuthMethod = "password"
	AuthOIDC      AuthMethod = "oidc"
	AuthToken     AuthMethod = "token"
	AuthOllamaKey AuthMethod = "ollama_key"
	AuthXet       AuthMethod = "xet"
)

// TokenRestrictions narrow what a Token can reach. Empty lists mean no
// restriction of that kind.
type TokenRestrictions struct {
	// Namespaces the Token may act in.
	Namespaces []string `json:"namespaces,omitempty"`
	// Repos the Token may act on, as `type:namespace/name`.
	Repos []string `json:"repos,omitempty"`
}

// IsZero reports whether there are no restrictions.
func (t TokenRestrictions) IsZero() bool { return len(t.Namespaces) == 0 && len(t.Repos) == 0 }

// Allows reports whether the restrictions allow access to the Repo.
func (t TokenRestrictions) Allows(id RepoID) bool {
	if t.IsZero() {
		return true
	}
	for _, ns := range t.Namespaces {
		if strings.EqualFold(ns, id.Namespace) {
			return true
		}
	}
	key := string(id.Type) + ":" + id.String()
	for _, r := range t.Repos {
		if strings.EqualFold(r, key) {
			return true
		}
	}
	return false
}

// TokenInfo describes the Token behind a token-authenticated request.
type TokenInfo struct {
	ID           uuid.UUID
	Name         string
	Scope        Scope
	Restrictions TokenRestrictions
}

// Principal is the caller of a request or the actor of a Job.
type Principal struct {
	Kind PrincipalKind
	// UserID and NamespaceID are set for users and for the system user.
	UserID      uuid.UUID
	NamespaceID uuid.UUID
	// Username is the name of the User's personal Namespace.
	Username    string
	DisplayName string
	Email       string
	// IsAdmin is true for Instance admins.
	IsAdmin bool
	// Method is how the request authenticated.
	Method AuthMethod
	// Token is set when Method is AuthToken.
	Token *TokenInfo
	// SessionID is set when Method is AuthSession; it is the session row ID,
	// never the cookie secret.
	SessionID uuid.UUID
}

// Anonymous returns the Principal of a request without credentials.
func Anonymous() Principal { return Principal{Kind: PrincipalAnonymous} }

// SystemUsername is the name of the system user that writes Parquet exports
// and other automated Commits.
const SystemUsername = "hangar-bot"

// System returns the Principal Hangar uses for its own writes.
func System() Principal {
	return Principal{Kind: PrincipalSystem, Username: SystemUsername, DisplayName: "Hangar", IsAdmin: true}
}

// IsAnonymous reports whether the Principal has no identity.
func (p Principal) IsAnonymous() bool { return p.Kind == PrincipalAnonymous || p.Kind == "" }

// IsUser reports whether the Principal is a signed-in User.
func (p Principal) IsUser() bool { return p.Kind == PrincipalUser }

// MaxScope returns the widest Scope the Principal may use: the Token's scope
// for Token requests, and admin for sessions and the system user.
func (p Principal) MaxScope() Scope {
	switch {
	case p.IsAnonymous():
		return ScopeRead
	case p.Token != nil:
		return p.Token.Scope
	default:
		return ScopeAdmin
	}
}

// AllowsRepo reports whether any Token restrictions allow the Repo.
func (p Principal) AllowsRepo(id RepoID) bool {
	if p.Token == nil {
		return true
	}
	return p.Token.Restrictions.Allows(id)
}

// EndUserID returns the value for the `enduser.id` span attribute: the
// User's UUID, or an empty string for anonymous callers.
func (p Principal) EndUserID() string {
	if p.UserID == uuid.Nil {
		return ""
	}
	return p.UserID.String()
}
