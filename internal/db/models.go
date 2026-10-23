package db

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"
)

// Timestamps is embedded in every model. Its hook sets created_at on insert
// and updated_at on insert and update, in UTC, so both databases store the
// same format.
type Timestamps struct {
	CreatedAt time.Time `bun:"created_at,notnull"`
	UpdatedAt time.Time `bun:"updated_at,notnull"`
}

var _ bun.BeforeAppendModelHook = (*Timestamps)(nil)

// BeforeAppendModel implements bun.BeforeAppendModelHook.
func (t *Timestamps) BeforeAppendModel(_ context.Context, q bun.Query) error {
	now := Now()
	switch q.(type) {
	case *bun.InsertQuery:
		if t.CreatedAt.IsZero() {
			t.CreatedAt = now
		}
		if t.UpdatedAt.IsZero() {
			t.UpdatedAt = now
		}
	case *bun.UpdateQuery:
		t.UpdatedAt = now
	}
	return nil
}

// JSON is a raw JSON column value.
type JSON = json.RawMessage

// Namespace is a row of namespaces: Users and Organisations share one name space.
type Namespace struct {
	bun.BaseModel  `bun:"table:namespaces"`
	ID             uuid.UUID `bun:"id,pk,type:uuid"`
	Name           string    `bun:"name,notnull"`
	Kind           string    `bun:"kind,notnull"`
	DisplayName    string    `bun:"display_name,notnull"`
	Description    string    `bun:"description,notnull"`
	AvatarKey      *string   `bun:"avatar_key"`
	ReservedMirror bool      `bun:"reserved_mirror,notnull"`
	Timestamps
}

// User is a row of users.
type User struct {
	bun.BaseModel `bun:"table:users"`
	ID            uuid.UUID  `bun:"id,pk,type:uuid"`
	NamespaceID   uuid.UUID  `bun:"namespace_id,notnull,type:uuid"`
	Email         *string    `bun:"email"`
	EmailVerified bool       `bun:"email_verified,notnull"`
	PasswordHash  *string    `bun:"password_hash"`
	IsAdmin       bool       `bun:"is_admin,notnull"`
	IsSystem      bool       `bun:"is_system,notnull"`
	DisabledAt    *time.Time `bun:"disabled_at"`
	LastLoginAt   *time.Time `bun:"last_login_at"`
	Timestamps
}

// Identity is a row of identities: an OIDC link.
type Identity struct {
	bun.BaseModel `bun:"table:identities"`
	ID            uuid.UUID      `bun:"id,pk,type:uuid"`
	UserID        uuid.UUID      `bun:"user_id,notnull,type:uuid"`
	Provider      string         `bun:"provider,notnull"`
	Subject       string         `bun:"subject,notnull"`
	Email         *string        `bun:"email"`
	Claims        map[string]any `bun:"claims,notnull"`
	LastLoginAt   *time.Time     `bun:"last_login_at"`
	Timestamps
}

// Session is a row of sessions.
type Session struct {
	bun.BaseModel `bun:"table:sessions"`
	ID            uuid.UUID  `bun:"id,pk,type:uuid"`
	IDHash        string     `bun:"id_hash,notnull"`
	UserID        uuid.UUID  `bun:"user_id,notnull,type:uuid"`
	CSRFSecret    string     `bun:"csrf_secret,notnull"`
	ExpiresAt     time.Time  `bun:"expires_at,notnull"`
	LastSeenAt    *time.Time `bun:"last_seen_at"`
	UserAgent     string     `bun:"user_agent,notnull"`
	IP            string     `bun:"ip,notnull"`
	Timestamps
}

// Token is a row of tokens. Hash is the SHA-256 of the secret.
type Token struct {
	bun.BaseModel `bun:"table:tokens"`
	ID            uuid.UUID               `bun:"id,pk,type:uuid"`
	UserID        uuid.UUID               `bun:"user_id,notnull,type:uuid"`
	Name          string                  `bun:"name,notnull"`
	Hash          string                  `bun:"hash,notnull"`
	Prefix        string                  `bun:"prefix,notnull"`
	Scope         string                  `bun:"scope,notnull"`
	Restrictions  TokenRestrictionsColumn `bun:"restrictions,notnull"`
	ExpiresAt     *time.Time              `bun:"expires_at"`
	LastUsedAt    *time.Time              `bun:"last_used_at"`
	RevokedAt     *time.Time              `bun:"revoked_at"`
	Timestamps
}

// TokenRestrictionsColumn is the JSON shape of tokens.restrictions.
type TokenRestrictionsColumn struct {
	Namespaces []string `json:"namespaces,omitempty"`
	Repos      []string `json:"repos,omitempty"`
}

// OllamaKey is a row of ollama_keys. TokenID is the Token that registered
// the key, if a Token did.
type OllamaKey struct {
	bun.BaseModel `bun:"table:ollama_keys"`
	ID            uuid.UUID  `bun:"id,pk,type:uuid"`
	UserID        uuid.UUID  `bun:"user_id,notnull,type:uuid"`
	Name          string     `bun:"name,notnull"`
	PublicKey     string     `bun:"public_key,notnull"`
	Fingerprint   string     `bun:"fingerprint,notnull"`
	TokenID       *uuid.UUID `bun:"token_id,type:uuid"`
	LastUsedAt    *time.Time `bun:"last_used_at"`
	Timestamps
}

// OllamaNonce is a row of ollama_nonces.
type OllamaNonce struct {
	bun.BaseModel `bun:"table:ollama_nonces"`
	Nonce         string    `bun:"nonce,pk"`
	ExpiresAt     time.Time `bun:"expires_at,notnull"`
	Timestamps
}

// OllamaBlob is a row of ollama_blobs.
type OllamaBlob struct {
	bun.BaseModel `bun:"table:ollama_blobs"`
	Digest        string `bun:"digest,pk"`
	MediaType     string `bun:"media_type,notnull"`
	Size          int64  `bun:"size,notnull"`
	Data          []byte `bun:"data,notnull,type:bytea"`
	Timestamps
}

// Membership is a row of memberships.
type Membership struct {
	bun.BaseModel  `bun:"table:memberships"`
	ID             uuid.UUID `bun:"id,pk,type:uuid"`
	OrgNamespaceID uuid.UUID `bun:"org_namespace_id,notnull,type:uuid"`
	UserID         uuid.UUID `bun:"user_id,notnull,type:uuid"`
	Role           string    `bun:"role,notnull"`
	Source         string    `bun:"source,notnull"`
	SourceProvider *string   `bun:"source_provider"`
	Timestamps
}

// Invite is a row of invites.
type Invite struct {
	bun.BaseModel  `bun:"table:invites"`
	ID             uuid.UUID  `bun:"id,pk,type:uuid"`
	CodeHash       string     `bun:"code_hash,notnull"`
	CreatedBy      *uuid.UUID `bun:"created_by,type:uuid"`
	Email          string     `bun:"email,notnull"`
	OrgNamespaceID *uuid.UUID `bun:"org_namespace_id,type:uuid"`
	Role           *string    `bun:"role"`
	MakeAdmin      bool       `bun:"make_admin,notnull"`
	ExpiresAt      time.Time  `bun:"expires_at,notnull"`
	UsedAt         *time.Time `bun:"used_at"`
	UsedBy         *uuid.UUID `bun:"used_by,type:uuid"`
	Timestamps
}

// PasswordReset is a row of password_resets.
type PasswordReset struct {
	bun.BaseModel `bun:"table:password_resets"`
	ID            uuid.UUID  `bun:"id,pk,type:uuid"`
	CodeHash      string     `bun:"code_hash,notnull"`
	UserID        uuid.UUID  `bun:"user_id,notnull,type:uuid"`
	CreatedBy     *uuid.UUID `bun:"created_by,type:uuid"`
	ExpiresAt     time.Time  `bun:"expires_at,notnull"`
	UsedAt        *time.Time `bun:"used_at"`
	Timestamps
}

// EmailChange is a row of email_changes: an email address a User asked
// for and has not confirmed. CodeHash and ExpiresAt are set once an admin
// creates the confirmation link.
type EmailChange struct {
	bun.BaseModel `bun:"table:email_changes"`
	ID            uuid.UUID  `bun:"id,pk,type:uuid"`
	UserID        uuid.UUID  `bun:"user_id,notnull,type:uuid"`
	Email         string     `bun:"email,notnull"`
	CodeHash      *string    `bun:"code_hash"`
	CreatedBy     *uuid.UUID `bun:"created_by,type:uuid"`
	ExpiresAt     *time.Time `bun:"expires_at"`
	Timestamps
}

// LoginAttempt is a row of login_attempts, keyed by "user:<name>" or "ip:<addr>".
type LoginAttempt struct {
	bun.BaseModel `bun:"table:login_attempts"`
	Key           string     `bun:"key,pk"`
	Failures      int        `bun:"failures,notnull"`
	LastFailureAt *time.Time `bun:"last_failure_at"`
	LockedUntil   *time.Time `bun:"locked_until"`
	Timestamps
}

// OIDCState is a row of oidc_states.
type OIDCState struct {
	bun.BaseModel `bun:"table:oidc_states"`
	StateHash     string     `bun:"state_hash,pk"`
	Provider      string     `bun:"provider,notnull"`
	Nonce         string     `bun:"nonce,notnull"`
	PKCEVerifier  string     `bun:"pkce_verifier,notnull"`
	RedirectTo    string     `bun:"redirect_to,notnull"`
	LinkUserID    *uuid.UUID `bun:"link_user_id,type:uuid"`
	BrowserHash   string     `bun:"browser_hash,notnull"`
	ExpiresAt     time.Time  `bun:"expires_at,notnull"`
	Timestamps
}

// OAuthDeviceCode is a row of oauth_device_codes.
type OAuthDeviceCode struct {
	bun.BaseModel  `bun:"table:oauth_device_codes"`
	DeviceCodeHash string     `bun:"device_code_hash,pk"`
	UserCode       string     `bun:"user_code,notnull"`
	ClientID       string     `bun:"client_id,notnull"`
	Status         string     `bun:"status,notnull"`
	UserID         *uuid.UUID `bun:"user_id,type:uuid"`
	TokenID        *uuid.UUID `bun:"token_id,type:uuid"`
	PollInterval   int        `bun:"poll_interval,notnull"`
	LastPolledAt   *time.Time `bun:"last_polled_at"`
	ExpiresAt      time.Time  `bun:"expires_at,notnull"`
	Timestamps
}

// Repo is a row of repos.
type Repo struct {
	bun.BaseModel  `bun:"table:repos"`
	ID             uuid.UUID      `bun:"id,pk,type:uuid"`
	NamespaceID    uuid.UUID      `bun:"namespace_id,notnull,type:uuid"`
	Name           string         `bun:"name,notnull"`
	Type           string         `bun:"type,notnull"`
	Visibility     string         `bun:"visibility,notnull"`
	DefaultBranch  string         `bun:"default_branch,notnull"`
	HeadRef        string         `bun:"head_ref,notnull"`
	Description    string         `bun:"description,notnull"`
	Tags           []string       `bun:"tags,notnull"`
	Settings       map[string]any `bun:"settings,notnull"`
	Gating         map[string]any `bun:"gating,notnull"`
	CreatedBy      *uuid.UUID     `bun:"created_by,type:uuid"`
	LastModifiedAt *time.Time     `bun:"last_modified_at"`
	DeletedAt      *time.Time     `bun:"deleted_at"`
	SizeBytes      int64          `bun:"size_bytes,notnull"`
	LFSBytes       int64          `bun:"lfs_bytes,notnull"`
	Timestamps
}

// RepoRedirect is a row of repo_redirects.
type RepoRedirect struct {
	bun.BaseModel `bun:"table:repo_redirects"`
	ID            uuid.UUID `bun:"id,pk,type:uuid"`
	OldNamespace  string    `bun:"old_namespace,notnull"`
	OldName       string    `bun:"old_name,notnull"`
	Type          string    `bun:"type,notnull"`
	RepoID        uuid.UUID `bun:"repo_id,notnull,type:uuid"`
	Timestamps
}

// Collaborator is a row of collaborators.
type Collaborator struct {
	bun.BaseModel `bun:"table:collaborators"`
	ID            uuid.UUID `bun:"id,pk,type:uuid"`
	RepoID        uuid.UUID `bun:"repo_id,notnull,type:uuid"`
	UserID        uuid.UUID `bun:"user_id,notnull,type:uuid"`
	Role          string    `bun:"role,notnull"`
	Timestamps
}

// Ref is a row of refs. Updates are compare-and-swap on Target.
type Ref struct {
	bun.BaseModel `bun:"table:refs"`
	RepoID        uuid.UUID `bun:"repo_id,pk,type:uuid"`
	Name          string    `bun:"name,pk"`
	Target        string    `bun:"target,notnull"`
	Timestamps
}

// Pack is a row of packs.
type Pack struct {
	bun.BaseModel `bun:"table:packs"`
	RepoID        uuid.UUID  `bun:"repo_id,pk,type:uuid"`
	PackSHA1      string     `bun:"pack_sha1,pk"`
	Size          int64      `bun:"size,notnull"`
	ObjectCount   int64      `bun:"object_count,notnull"`
	State         string     `bun:"state,notnull"`
	RetiredAt     *time.Time `bun:"retired_at"`
	Timestamps
}

// GitSignature is the JSON shape of commits.author and commits.committer.
type GitSignature struct {
	Name  string    `json:"name"`
	Email string    `json:"email"`
	When  time.Time `json:"when"`
}

// Commit is a row of commits, the Commit metadata cache.
type Commit struct {
	bun.BaseModel `bun:"table:commits"`
	RepoID        uuid.UUID    `bun:"repo_id,pk,type:uuid"`
	SHA           string       `bun:"sha,pk"`
	Tree          string       `bun:"tree,notnull"`
	Parents       []string     `bun:"parents,notnull"`
	Author        GitSignature `bun:"author,notnull"`
	Committer     GitSignature `bun:"committer,notnull"`
	Title         string       `bun:"title,notnull"`
	Message       string       `bun:"message,notnull"`
	AuthoredAt    time.Time    `bun:"authored_at,notnull"`
	CommittedAt   time.Time    `bun:"committed_at,notnull"`
	Timestamps
}

// TreeEntry is a row of tree_entries, the flattened file list of a Commit.
type TreeEntry struct {
	bun.BaseModel `bun:"table:tree_entries"`
	RepoID        uuid.UUID `bun:"repo_id,pk,type:uuid"`
	Commit        string    `bun:"commit_sha,pk"`
	Path          string    `bun:"path,pk"`
	Parent        string    `bun:"parent,notnull"`
	Type          string    `bun:"type,notnull"`
	Size          int64     `bun:"size,notnull"`
	GitOID        string    `bun:"git_oid,notnull"`
	LFSOID        *string   `bun:"lfs_oid"`
	LFSSize       *int64    `bun:"lfs_size"`
	PointerSize   *int      `bun:"pointer_size"`
	LastCommit    *string   `bun:"last_commit"`
	Timestamps
}

// LFSObject is a row of lfs_objects.
type LFSObject struct {
	bun.BaseModel    `bun:"table:lfs_objects"`
	OID              string     `bun:"oid,pk"`
	Size             int64      `bun:"size,notnull"`
	StorageKey       string     `bun:"storage_key,notnull"`
	VerifiedAt       *time.Time `bun:"verified_at"`
	LastReferencedAt time.Time  `bun:"last_referenced_at,notnull"`
	Timestamps
}

// RepoLFS is a row of repo_lfs.
type RepoLFS struct {
	bun.BaseModel `bun:"table:repo_lfs"`
	RepoID        uuid.UUID `bun:"repo_id,pk,type:uuid"`
	OID           string    `bun:"oid,pk"`
	Size          int64     `bun:"size,notnull"`
	Present       bool      `bun:"present,notnull"`
	Timestamps
}

// LFSUploader is a row of lfs_uploaders: a User who uploaded the bytes of
// an LFS object, so their Commits may add pointers to it.
type LFSUploader struct {
	bun.BaseModel `bun:"table:lfs_uploaders"`
	OID           string    `bun:"oid,pk"`
	UserID        uuid.UUID `bun:"user_id,pk,type:uuid"`
	Timestamps
}

// UploadPart is one uploaded part in uploads.parts.
type UploadPart struct {
	Number int    `json:"number"`
	ETag   string `json:"etag"`
	Size   int64  `json:"size"`
}

// Upload is a row of uploads.
type Upload struct {
	bun.BaseModel `bun:"table:uploads"`
	ID            uuid.UUID    `bun:"id,pk,type:uuid"`
	OID           string       `bun:"oid,notnull"`
	Size          int64        `bun:"size,notnull"`
	Kind          string       `bun:"kind,notnull"`
	UploadID      string       `bun:"upload_id,notnull"`
	StorageKey    string       `bun:"storage_key,notnull"`
	PartSize      int64        `bun:"part_size,notnull"`
	Parts         []UploadPart `bun:"parts,notnull"`
	UserID        *uuid.UUID   `bun:"user_id,type:uuid"`
	RepoID        *uuid.UUID   `bun:"repo_id,type:uuid"`
	ExpiresAt     time.Time    `bun:"expires_at,notnull"`
	Timestamps
}

// Upstream is a row of upstreams.
type Upstream struct {
	bun.BaseModel           `bun:"table:upstreams"`
	ID                      uuid.UUID `bun:"id,pk,type:uuid"`
	Name                    string    `bun:"name,notnull"`
	DisplayName             string    `bun:"display_name,notnull"`
	BaseURL                 string    `bun:"base_url,notnull"`
	ViewerURL               string    `bun:"viewer_url,notnull"`
	Kind                    string    `bun:"kind,notnull"`
	Enabled                 bool      `bun:"enabled,notnull"`
	NamespacePrefix         string    `bun:"namespace_prefix,notnull"`
	Source                  string    `bun:"source,notnull"`
	InstanceTokenCiphertext *string   `bun:"instance_token_ciphertext"`
	Timestamps
}

// UpstreamToken is a row of upstream_tokens.
type UpstreamToken struct {
	bun.BaseModel `bun:"table:upstream_tokens"`
	ID            uuid.UUID `bun:"id,pk,type:uuid"`
	UserID        uuid.UUID `bun:"user_id,notnull,type:uuid"`
	UpstreamID    uuid.UUID `bun:"upstream_id,notnull,type:uuid"`
	Ciphertext    string    `bun:"ciphertext,notnull"`
	Timestamps
}

// Mirror is a row of mirrors.
type Mirror struct {
	bun.BaseModel    `bun:"table:mirrors"`
	RepoID           uuid.UUID      `bun:"repo_id,pk,type:uuid"`
	UpstreamID       uuid.UUID      `bun:"upstream_id,notnull,type:uuid"`
	UpstreamRepo     string         `bun:"upstream_repo,notnull"`
	Include          []string       `bun:"include,notnull"`
	Exclude          []string       `bun:"exclude,notnull"`
	Selection        map[string]any `bun:"selection,notnull"`
	Track            bool           `bun:"track,notnull"`
	TrackRef         string         `bun:"track_ref,notnull"`
	Schedule         string         `bun:"schedule,notnull"`
	LastSyncedCommit *string        `bun:"last_synced_commit"`
	LastSyncedAt     *time.Time     `bun:"last_synced_at"`
	Status           string         `bun:"status,notnull"`
	LastError        string         `bun:"last_error,notnull"`
	DetachedAt       *time.Time     `bun:"detached_at"`
	Timestamps
}

// Dependency is a row of dependencies.
type Dependency struct {
	bun.BaseModel  `bun:"table:dependencies"`
	ID             uuid.UUID `bun:"id,pk,type:uuid"`
	RepoID         uuid.UUID `bun:"repo_id,notnull,type:uuid"`
	Commit         string    `bun:"commit_sha,notnull"`
	TargetType     string    `bun:"target_type,notnull"`
	TargetRepo     string    `bun:"target_repo,notnull"`
	TargetRevision string    `bun:"target_revision,notnull"`
	Kind           string    `bun:"kind,notnull"`
	Required       bool      `bun:"required,notnull"`
	SourcePath     string    `bun:"source_path,notnull"`
	SourceKey      string    `bun:"source_key,notnull"`
	Timestamps
}

// Metadata is a row of metadata: index results for a Commit.
type Metadata struct {
	bun.BaseModel  `bun:"table:metadata"`
	RepoID         uuid.UUID `bun:"repo_id,pk,type:uuid"`
	Commit         string    `bun:"commit_sha,pk"`
	IndexerVersion int       `bun:"indexer_version,pk"`
	Summary        JSON      `bun:"summary,notnull,type:jsonb"`
	IndexedAt      time.Time `bun:"indexed_at,notnull"`
	Timestamps
}

// RepoFacts is a row of repo_facts: what Repo listings filter and sort
// on, from the index results at the default branch head.
type RepoFacts struct {
	bun.BaseModel `bun:"table:repo_facts"`
	RepoID        uuid.UUID `bun:"repo_id,pk,type:uuid"`
	Version       int       `bun:"version,notnull"`
	Parameters    int64     `bun:"parameters,notnull"`
	ContextLength int64     `bun:"context_length,notnull"`
	NumRows       int64     `bun:"num_rows,notnull"`
	Timestamps
}

// RepoFactsChange is a row of repo_facts_changes: a Repo whose repo_facts
// row is out of date. Triggers add them.
type RepoFactsChange struct {
	bun.BaseModel `bun:"table:repo_facts_changes"`
	ID            int64     `bun:"id,pk,autoincrement"`
	RepoID        uuid.UUID `bun:"repo_id,notnull,type:uuid"`
	Timestamps
}

// RepoFacetValue is a row of repo_facet_values: one facet value of a Repo.
type RepoFacetValue struct {
	bun.BaseModel `bun:"table:repo_facet_values"`
	RepoID        uuid.UUID `bun:"repo_id,pk,type:uuid"`
	Facet         string    `bun:"facet,pk"`
	Value         string    `bun:"value,pk"`
	Folded        string    `bun:"folded,notnull"`
	Timestamps
}

// ScanResult is a row of scan_results.
type ScanResult struct {
	bun.BaseModel  `bun:"table:scan_results"`
	RepoID         uuid.UUID `bun:"repo_id,pk,type:uuid"`
	Commit         string    `bun:"commit_sha,pk"`
	Path           string    `bun:"path,pk"`
	ScannerVersion int       `bun:"scanner_version,pk"`
	Status         string    `bun:"status,notnull"`
	Findings       JSON      `bun:"findings,notnull,type:jsonb"`
	Timestamps
}

// Job is a row of jobs.
type Job struct {
	bun.BaseModel  `bun:"table:jobs"`
	ID             uuid.UUID         `bun:"id,pk,type:uuid"`
	Kind           string            `bun:"kind,notnull"`
	DedupeKey      *string           `bun:"dedupe_key"`
	Payload        JSON              `bun:"payload,notnull,type:jsonb"`
	Status         string            `bun:"status,notnull"`
	Priority       int               `bun:"priority,notnull"`
	Attempts       int               `bun:"attempts,notnull"`
	MaxAttempts    int               `bun:"max_attempts,notnull"`
	RunAfter       time.Time         `bun:"run_after,notnull"`
	LeaseOwner     *string           `bun:"lease_owner"`
	LeaseExpiresAt *time.Time        `bun:"lease_expires_at"`
	Progress       JSON              `bun:"progress,notnull,type:jsonb"`
	Result         JSON              `bun:"result,notnull,type:jsonb"`
	Error          string            `bun:"error,notnull"`
	CreatedBy      *uuid.UUID        `bun:"created_by,type:uuid"`
	ParentID       *uuid.UUID        `bun:"parent_id,type:uuid"`
	RepoID         *uuid.UUID        `bun:"repo_id,type:uuid"`
	TraceContext   map[string]string `bun:"trace_context,notnull"`
	StartedAt      *time.Time        `bun:"started_at"`
	FinishedAt     *time.Time        `bun:"finished_at"`
	Timestamps
}

// Schedule is a row of schedules.
type Schedule struct {
	bun.BaseModel `bun:"table:schedules"`
	ID            uuid.UUID  `bun:"id,pk,type:uuid"`
	Kind          string     `bun:"kind,notnull"`
	Key           string     `bun:"key,notnull"`
	Cron          string     `bun:"cron,notnull"`
	Payload       JSON       `bun:"payload,notnull,type:jsonb"`
	Enabled       bool       `bun:"enabled,notnull"`
	NextRunAt     time.Time  `bun:"next_run_at,notnull"`
	LastRunAt     *time.Time `bun:"last_run_at"`
	Timestamps
}

// RateLimitLease is a row of rate_limit_leases: one worker's slice of a
// shared bandwidth budget.
type RateLimitLease struct {
	bun.BaseModel  `bun:"table:rate_limit_leases"`
	Name           string    `bun:"name,pk"`
	Owner          string    `bun:"owner,pk"`
	BytesPerSecond int64     `bun:"bytes_per_second,notnull"`
	ExpiresAt      time.Time `bun:"expires_at,notnull"`
	Timestamps
}

// Collection is a row of collections.
type Collection struct {
	bun.BaseModel `bun:"table:collections"`
	ID            uuid.UUID `bun:"id,pk,type:uuid"`
	NamespaceID   uuid.UUID `bun:"namespace_id,notnull,type:uuid"`
	Slug          string    `bun:"slug,notnull"`
	Title         string    `bun:"title,notnull"`
	Description   string    `bun:"description,notnull"`
	Visibility    string    `bun:"visibility,notnull"`
	Theme         string    `bun:"theme,notnull"`
	Position      int       `bun:"position,notnull"`
	Timestamps
}

// CollectionItem is a row of collection_items.
type CollectionItem struct {
	bun.BaseModel `bun:"table:collection_items"`
	ID            uuid.UUID `bun:"id,pk,type:uuid"`
	CollectionID  uuid.UUID `bun:"collection_id,notnull,type:uuid"`
	ItemType      string    `bun:"item_type,notnull"`
	ItemID        string    `bun:"item_id,notnull"`
	Position      int       `bun:"position,notnull"`
	Note          string    `bun:"note,notnull"`
	Timestamps
}

// Discussion is a row of discussions: a discussion or a pull request.
type Discussion struct {
	bun.BaseModel `bun:"table:discussions"`
	ID            uuid.UUID  `bun:"id,pk,type:uuid"`
	RepoID        uuid.UUID  `bun:"repo_id,notnull,type:uuid"`
	Num           int        `bun:"num,notnull"`
	Kind          string     `bun:"kind,notnull"`
	Title         string     `bun:"title,notnull"`
	Status        string     `bun:"status,notnull"`
	AuthorID      *uuid.UUID `bun:"author_id,type:uuid"`
	PRRef         *string    `bun:"pr_ref"`
	PRBase        *string    `bun:"pr_base"`
	Timestamps
}

// DiscussionEvent is a row of discussion_events.
type DiscussionEvent struct {
	bun.BaseModel `bun:"table:discussion_events"`
	ID            uuid.UUID  `bun:"id,pk,type:uuid"`
	DiscussionID  uuid.UUID  `bun:"discussion_id,notnull,type:uuid"`
	Type          string     `bun:"type,notnull"`
	AuthorID      *uuid.UUID `bun:"author_id,type:uuid"`
	Body          string     `bun:"body,notnull"`
	Data          JSON       `bun:"data,notnull,type:jsonb"`
	Hidden        bool       `bun:"hidden,notnull"`
	Timestamps
}

// Like is a row of likes.
type Like struct {
	bun.BaseModel `bun:"table:likes"`
	UserID        uuid.UUID `bun:"user_id,pk,type:uuid"`
	RepoID        uuid.UUID `bun:"repo_id,pk,type:uuid"`
	Timestamps
}

// DownloadCount is a row of download_counts. Day is midnight UTC.
type DownloadCount struct {
	bun.BaseModel `bun:"table:download_counts"`
	RepoID        uuid.UUID `bun:"repo_id,pk,type:uuid"`
	Day           time.Time `bun:"day,pk,type:date"`
	Count         int64     `bun:"count,notnull"`
	Timestamps
}

// AccessRequest is a row of access_requests.
type AccessRequest struct {
	bun.BaseModel `bun:"table:access_requests"`
	ID            uuid.UUID      `bun:"id,pk,type:uuid"`
	RepoID        uuid.UUID      `bun:"repo_id,notnull,type:uuid"`
	UserID        uuid.UUID      `bun:"user_id,notnull,type:uuid"`
	Status        string         `bun:"status,notnull"`
	Fields        map[string]any `bun:"fields,notnull"`
	DecidedBy     *uuid.UUID     `bun:"decided_by,type:uuid"`
	DecidedAt     *time.Time     `bun:"decided_at"`
	Timestamps
}

// Webhook is a row of webhooks.
type Webhook struct {
	bun.BaseModel    `bun:"table:webhooks"`
	ID               uuid.UUID  `bun:"id,pk,type:uuid"`
	ScopeNamespaceID *uuid.UUID `bun:"scope_namespace_id,type:uuid"`
	ScopeRepoID      *uuid.UUID `bun:"scope_repo_id,type:uuid"`
	URL              string     `bun:"url,notnull"`
	SecretCiphertext string     `bun:"secret_ciphertext,notnull"`
	Events           []string   `bun:"events,notnull"`
	Enabled          bool       `bun:"enabled,notnull"`
	CreatedBy        *uuid.UUID `bun:"created_by,type:uuid"`
	Timestamps
}

// WebhookDelivery is a row of webhook_deliveries.
type WebhookDelivery struct {
	bun.BaseModel `bun:"table:webhook_deliveries"`
	ID            uuid.UUID  `bun:"id,pk,type:uuid"`
	WebhookID     uuid.UUID  `bun:"webhook_id,notnull,type:uuid"`
	Event         string     `bun:"event,notnull"`
	EventID       uuid.UUID  `bun:"event_id,notnull,type:uuid"`
	Payload       string     `bun:"payload,notnull"`
	StatusCode    *int       `bun:"status_code"`
	Attempt       int        `bun:"attempt,notnull"`
	NextAttemptAt *time.Time `bun:"next_attempt_at"`
	DurationMS    *int       `bun:"duration_ms"`
	Error         string     `bun:"error,notnull"`
	DeliveredAt   *time.Time `bun:"delivered_at"`
	Timestamps
}

// AuditEvent is a row of audit_events.
type AuditEvent struct {
	bun.BaseModel `bun:"table:audit_events"`
	ID            uuid.UUID  `bun:"id,pk,type:uuid"`
	ActorUserID   *uuid.UUID `bun:"actor_user_id,type:uuid"`
	ActorTokenID  *uuid.UUID `bun:"actor_token_id,type:uuid"`
	Action        string     `bun:"action,notnull"`
	TargetType    string     `bun:"target_type,notnull"`
	TargetID      string     `bun:"target_id,notnull"`
	IP            string     `bun:"ip,notnull"`
	UserAgent     string     `bun:"user_agent,notnull"`
	Data          JSON       `bun:"data,notnull,type:jsonb"`
	Timestamps
}

// Quota is a row of quotas.
type Quota struct {
	bun.BaseModel `bun:"table:quotas"`
	NamespaceID   uuid.UUID `bun:"namespace_id,pk,type:uuid"`
	BytesLimit    int64     `bun:"bytes_limit,notnull"`
	Timestamps
}

// Setting is a row of settings: an Instance setting editable in the admin UI.
type Setting struct {
	bun.BaseModel `bun:"table:settings"`
	Key           string `bun:"key,pk"`
	Value         JSON   `bun:"value,notnull,type:jsonb"`
	Timestamps
}

// XetXorb is a row of xet_xorbs.
type XetXorb struct {
	bun.BaseModel    `bun:"table:xet_xorbs"`
	Hash             string    `bun:"hash,pk"`
	Size             int64     `bun:"size,notnull"`
	StorageKey       string    `bun:"storage_key,notnull"`
	LastReferencedAt time.Time `bun:"last_referenced_at,notnull"`
	Timestamps
}

// XetFile is a row of xet_files: a Xet file hash mapped to an LFS object.
type XetFile struct {
	bun.BaseModel `bun:"table:xet_files"`
	XetHash       string `bun:"xet_hash,pk"`
	OID           string `bun:"oid,notnull"`
	Size          int64  `bun:"size,notnull"`
	Timestamps
}

// DatasetSplit is a row of dataset_splits.
type DatasetSplit struct {
	bun.BaseModel   `bun:"table:dataset_splits"`
	RepoID          uuid.UUID `bun:"repo_id,pk,type:uuid"`
	Commit          string    `bun:"commit_sha,pk"`
	Config          string    `bun:"config,pk"`
	Split           string    `bun:"split,pk"`
	NumRows         *int64    `bun:"num_rows"`
	NumBytesSource  *int64    `bun:"num_bytes_source"`
	NumBytesParquet *int64    `bun:"num_bytes_parquet"`
	Features        JSON      `bun:"features,notnull,type:jsonb"`
	Status          string    `bun:"status,notnull"`
	Error           string    `bun:"error,notnull"`
	ConvertedCommit *string   `bun:"converted_commit"`
	Timestamps
}

// DatasetStat is a row of dataset_stats.
type DatasetStat struct {
	bun.BaseModel `bun:"table:dataset_stats"`
	RepoID        uuid.UUID `bun:"repo_id,pk,type:uuid"`
	Commit        string    `bun:"commit_sha,pk"`
	Config        string    `bun:"config,pk"`
	Split         string    `bun:"split,pk"`
	ColumnName    string    `bun:"column_name,pk"`
	Stats         JSON      `bun:"stats,notnull,type:jsonb"`
	Timestamps
}

// DerivedModels returns the models of the tables that hold data the Repo
// service derives from other tables and rebuilds when they change: the
// Repo listing facts and the records of which are out of date. Backups
// leave them out; after a restore, every restored Repo is recorded as out
// of date, so its facts are rebuilt.
func DerivedModels() []any {
	return []any{(*RepoFacts)(nil), (*RepoFacetValue)(nil), (*RepoFactsChange)(nil)}
}

// BackupModels returns AllModels without DerivedModels, in creation order.
func BackupModels() []any {
	derived := map[any]bool{}
	for _, m := range DerivedModels() {
		derived[m] = true
	}
	var out []any
	for _, m := range AllModels() {
		if !derived[m] {
			out = append(out, m)
		}
	}
	return out
}

// AllModels returns a pointer to the zero value of every model, in
// creation order. Tests use it to check that every table round-trips.
func AllModels() []any {
	return []any{
		(*Namespace)(nil), (*User)(nil), (*Identity)(nil), (*Session)(nil), (*Token)(nil),
		(*OllamaKey)(nil), (*OllamaNonce)(nil), (*OllamaBlob)(nil), (*Membership)(nil),
		(*Invite)(nil), (*PasswordReset)(nil), (*EmailChange)(nil), (*LoginAttempt)(nil), (*OIDCState)(nil),
		(*OAuthDeviceCode)(nil), (*Repo)(nil), (*RepoRedirect)(nil), (*Collaborator)(nil),
		(*Ref)(nil), (*Pack)(nil), (*Commit)(nil), (*TreeEntry)(nil), (*LFSObject)(nil),
		(*LFSUploader)(nil), (*RepoLFS)(nil), (*Upload)(nil), (*Upstream)(nil), (*UpstreamToken)(nil),
		(*Mirror)(nil), (*Dependency)(nil), (*Metadata)(nil), (*ScanResult)(nil),
		(*Job)(nil), (*Schedule)(nil), (*RateLimitLease)(nil), (*Collection)(nil),
		(*CollectionItem)(nil), (*Discussion)(nil), (*DiscussionEvent)(nil), (*Like)(nil),
		(*DownloadCount)(nil), (*AccessRequest)(nil), (*Webhook)(nil), (*WebhookDelivery)(nil),
		(*AuditEvent)(nil), (*Quota)(nil), (*Setting)(nil), (*XetXorb)(nil), (*XetFile)(nil),
		(*DatasetSplit)(nil), (*DatasetStat)(nil), (*RepoFacts)(nil), (*RepoFacetValue)(nil),
		(*RepoFactsChange)(nil),
	}
}
