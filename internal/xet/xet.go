// Package xet accepts Xet uploads from hf_xet (UP-7): it issues write
// tokens, verifies and stores xorbs, and on each shard reassembles every
// file into a whole LFS object addressed by SHA-256
// (docs/spec/03-hf-api.md "Xet", docs/research/xet-upload-research.md).
// Downloads never use Xet. The Hub API mounts the token endpoints through
// Ingestor; the CAS endpoints live under /xet-cas. xettest has a fake.
package xet

import (
	"context"
	"errors"
	"io"
	"regexp"
	"time"

	"github.com/google/uuid"

	"github.com/jamesagarside/hangar/internal/model"
)

// Errors, mapped to HTTP statuses by the CAS handlers.
var (
	// ErrInvalidToken means the CAS token is missing, malformed or expired (401).
	ErrInvalidToken = errors.New("xet: invalid or expired token")
	// ErrScope means the token does not allow the call (403).
	ErrScope = errors.New("xet: token scope does not allow this")
	// ErrRejected means hfapi.xet is reject (400 with RejectMessage).
	ErrRejected = errors.New("xet: uploads are turned off")
	// ErrBadXorb means a xorb failed verification (400).
	ErrBadXorb = errors.New("xet: invalid xorb")
	// ErrBadShard means a shard failed validation or reassembly (400).
	ErrBadShard = errors.New("xet: invalid shard")
	// ErrUnknownXorb means a shard refers to a xorb Hangar does not have (400
	// with UnknownXorbMessage).
	ErrUnknownXorb = errors.New("xet: shard refers to an unknown xorb")
	// ErrTooLarge means a xorb or shard is over 64 MiB (413).
	ErrTooLarge = errors.New("xet: object too large")
)

// Messages users see.
const (
	RejectMessage      = "Xet uploads are turned off on this Hangar. Set HF_HUB_DISABLE_XET=1 to upload."
	UnknownXorbMessage = "The upload refers to data this Hangar no longer has. Clear the hf_xet cache (~/.cache/huggingface/xet) or set HF_HUB_DISABLE_XET=1, then upload again."
)

// MaxObjectSize is the largest serialised xorb or shard.
const MaxObjectSize = 64 << 20

var hashRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ValidHash reports whether s is a 64-character lower-case hex Xet hash
// in API string form.
func ValidHash(s string) bool { return hashRe.MatchString(s) }

// Token is the JSON the xet-write-token endpoint returns, and the matching
// X-Xet-Cas-Url, X-Xet-Access-Token and X-Xet-Token-Expiration headers.
type Token struct {
	AccessToken string `json:"accessToken"`
	// Exp is Unix seconds. hf_xet refreshes 30 seconds before it and never
	// on a 401, so it must be accurate.
	Exp    int64  `json:"exp"`
	CASURL string `json:"casUrl"`
}

// Scopes a token may carry.
const (
	ScopeWrite = "write"
	ScopeRead  = "read"
)

// Claims are what a verified token says.
type Claims struct {
	UserID   uuid.UUID
	Username string
	Repo     model.RepoID
	RepoUUID uuid.UUID
	Revision string
	CreatePR bool
	Scope    string
	Expires  time.Time
}

// TokenRequest asks for a write token. The caller has already checked that
// the Principal may write to the Repo (or open a pull request when CreatePR).
type TokenRequest struct {
	Principal model.Principal
	Repo      model.RepoID
	RepoUUID  uuid.UUID
	// Revision is the decoded {rev}; "None" means the default branch.
	Revision string
	CreatePR bool
}

// ShardEvent is one line of the streamed /v2/shards response.
type ShardEvent struct {
	Type      string `json:"type"`
	Verified  int    `json:"verified,omitempty"`
	Total     int    `json:"total,omitempty"`
	Stage     string `json:"stage,omitempty"`
	Result    int    `json:"result,omitempty"`
	Message   string `json:"message,omitempty"`
	Retryable bool   `json:"retryable,omitempty"`
}

// Shard event types.
const (
	EventValidating = "validating"
	EventCommitting = "committing"
	EventResult     = "result"
	EventError      = "error"
)

// ReassembledFile is a file rebuilt from a shard and stored as an LFS object.
type ReassembledFile struct {
	XetHash string
	OID     string
	Size    int64
}

// ShardResult is the outcome of an accepted shard. Result is 0 when the
// shard was already known and 1 when it was processed.
type ShardResult struct {
	Result int
	Files  []ReassembledFile
}

// Ingestor is the Xet write path.
type Ingestor interface {
	// IssueWriteToken returns a short-lived token scoped to the user and
	// Repo. ErrRejected in reject mode.
	IssueWriteToken(ctx context.Context, req TokenRequest) (Token, error)
	// VerifyToken checks a CAS Bearer token.
	VerifyToken(ctx context.Context, token string) (*Claims, error)
	// PutXorb verifies a xorb (layout, decompression, chunk hashes, xorb
	// hash) and stores it under xet/xorbs/<hash>. It reports whether it
	// was new; an existing xorb gets its last-referenced time refreshed.
	PutXorb(ctx context.Context, c *Claims, hash string, r io.Reader, size int64) (inserted bool, err error)
	// AcceptShard validates a shard, reassembles every file it describes
	// into a verified LFS object, and reports progress through events
	// (which may be nil) so the handler can stream heartbeats.
	AcceptShard(ctx context.Context, c *Claims, r io.Reader, size int64, events func(ShardEvent)) (*ShardResult, error)
}
