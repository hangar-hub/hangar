// Package auth authenticates requests and decides what Principals may do:
// local accounts, OIDC, sessions, Tokens, Ollama keys, device-code login and
// the permission rules in docs/spec/05-auth.md. authtest has fakes of every
// interface here.
package auth

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/jamesagarside/hangar/internal/model"
)

// Errors.
var (
	// ErrInvalidCredentials means a credential was presented but is wrong,
	// expired or revoked. Handlers answer 401.
	ErrInvalidCredentials = errors.New("invalid credentials")
	// ErrForbidden means the caller can see the target but may not do this.
	ErrForbidden = errors.New("forbidden")
	// ErrNotFound means the caller may not know the target exists; handlers
	// answer 404 (or 401 to anonymous callers) so private names do not leak.
	ErrNotFound = errors.New("not found")
	// ErrGated means the Repo is gated and the caller has no approved
	// access request: metadata is readable, files are not.
	ErrGated = errors.New("gated repo: access request required")
)

// Authenticator turns request credentials into a Principal: the session
// cookie (with the CSRF header on state-changing native API calls), a
// Bearer Token, a Token as an HTTP Basic password (git), or an Ollama
// registry JWT. With no credentials it returns an anonymous Principal and no
// error. A bad credential returns an error wrapping ErrInvalidCredentials.
// It has the same shape as httpx.Authenticator.
type Authenticator interface {
	Authenticate(r *http.Request) (model.Principal, error)
}

// Action is something a Principal wants to do.
type Action string

// Actions, matching the rows of the permission table in 05-auth.md.
const (
	// ActionRead reads Repo metadata and history.
	ActionRead Action = "read"
	// ActionReadFiles reads file content; gated Repos need an approved request.
	ActionReadFiles Action = "read_files"
	// ActionPullMirror pulls more files into a Mirror.
	ActionPullMirror Action = "pull_mirror"
	// ActionWrite pushes, commits, and creates or deletes branches and tags.
	ActionWrite Action = "write"
	// ActionDiscuss opens discussions and pull requests, comments and likes.
	ActionDiscuss Action = "discuss"
	// ActionAdmin changes settings, renames, transfers, deletes and manages webhooks.
	ActionAdmin Action = "admin"
	// ActionCreateRepo creates a Repo in a Namespace.
	ActionCreateRepo Action = "create_repo"
	// ActionManageNamespace manages an Organisation's Members and settings.
	ActionManageNamespace Action = "manage_namespace"
	// ActionInstanceAdmin manages Upstreams, Instance settings, quotas and the audit log.
	ActionInstanceAdmin Action = "instance_admin"
)

// RepoTarget describes a Repo for a permission check.
type RepoTarget struct {
	ID          model.RepoID
	UUID        uuid.UUID
	NamespaceID uuid.UUID
	Visibility  model.Visibility
	Gated       bool
	Mirror      bool
}

// NamespaceTarget describes a Namespace for a permission check.
type NamespaceTarget struct {
	ID   uuid.UUID
	Name string
	// Kind is "user" or "org".
	Kind string
}

// Target is what an Action applies to: a Repo, a Namespace, or neither
// (Instance-level actions).
type Target struct {
	Repo      *RepoTarget
	Namespace *NamespaceTarget
}

// Authz decides permissions. Can returns nil, or an error wrapping
// ErrForbidden, ErrNotFound or ErrGated. A caller who cannot read a Repo
// gets ErrNotFound for every action on it, so ErrForbidden or ErrGated for
// a Repo means the caller can read it. Token scopes and restrictions
// narrow the rules and never widen them.
type Authz interface {
	Can(ctx context.Context, p model.Principal, action Action, target Target) error
}

// NewToken describes a Token to create.
type NewToken struct {
	UserID       uuid.UUID
	Name         string
	Scope        model.Scope
	Restrictions model.TokenRestrictions
	// ExpiresAt is optional.
	ExpiresAt *time.Time
}

// Token describes a stored Token. The secret is never stored or returned
// after creation.
type Token struct {
	ID           uuid.UUID
	UserID       uuid.UUID
	Username     string
	Name         string
	Prefix       string
	Scope        model.Scope
	Restrictions model.TokenRestrictions
	ExpiresAt    *time.Time
	LastUsedAt   *time.Time
	CreatedAt    time.Time
}

// Tokens manages API and git Tokens (AUTH-4). Secrets have the form
// `hgr_` followed by 40 base62 characters.
type Tokens interface {
	// Create makes a Token and returns it with its secret, which is shown once.
	Create(ctx context.Context, t NewToken) (*Token, string, error)
	// List returns a user's Tokens, or every Token when userID is uuid.Nil.
	List(ctx context.Context, userID uuid.UUID) ([]Token, error)
	// Revoke revokes a Token. ErrNotFound if it does not exist.
	Revoke(ctx context.Context, id uuid.UUID, actor model.Principal) error
}

// DeviceCode is the start of a device-code login (RFC 8628), for
// interactive `hf auth login`.
type DeviceCode struct {
	DeviceCode              string
	UserCode                string
	VerificationURI         string
	VerificationURIComplete string
	Interval                time.Duration
	ExpiresIn               time.Duration
}

// Device-code polling errors, named as in RFC 8628.
var (
	ErrAuthorizationPending = errors.New("authorization_pending")
	ErrSlowDown             = errors.New("slow_down")
	ErrExpiredToken         = errors.New("expired_token")
	ErrAccessDenied         = errors.New("access_denied")
)

// DeviceFlow runs device-code logins backed by Hangar Tokens. The Hub API
// serves it at /oauth/device and /oauth/token; the UI approves user codes.
type DeviceFlow interface {
	// Start begins a login for a client.
	Start(ctx context.Context, clientID string) (*DeviceCode, error)
	// Approve lets the signed-in user grant a pending user code, which
	// creates a write Token named after the client.
	Approve(ctx context.Context, userCode string, p model.Principal) error
	// Deny refuses a pending user code.
	Deny(ctx context.Context, userCode string, p model.Principal) error
	// Poll exchanges a device code for a Token secret once approved, or
	// returns one of the RFC 8628 errors.
	Poll(ctx context.Context, deviceCode string) (string, error)
}

// OllamaTokenRequest is Ollama's request to the registry token endpoint
// (docs/spec/08-local-tools.md "Auth").
type OllamaTokenRequest struct {
	// Method is the HTTP method, signed as part of the challenge.
	Method string
	// URL is the full realm URL including its query, as signed.
	URL string
	// Authorization is the `<base64 public key>:<base64 signature>` header.
	Authorization string
	// Repo is the Repo named by the scope query parameter.
	Repo model.RepoID
}

// OllamaAuth verifies Ollama key signatures and issues short-lived registry
// tokens that the Authenticator accepts as Bearer tokens.
type OllamaAuth interface {
	// IssueRegistryToken verifies the request signature against the user's
	// registered keys, rejects stale timestamps and reused nonces, checks the
	// user can read the Repo, and returns a JWT valid for 5 minutes.
	IssueRegistryToken(ctx context.Context, req OllamaTokenRequest) (token string, expiresAt time.Time, err error)
}
