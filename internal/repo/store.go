package repo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"
	"golang.org/x/sync/singleflight"

	"github.com/jamesagarside/hangar/internal/audit"
	"github.com/jamesagarside/hangar/internal/community"
	"github.com/jamesagarside/hangar/internal/db"
	"github.com/jamesagarside/hangar/internal/gitstore"
	"github.com/jamesagarside/hangar/internal/index"
	"github.com/jamesagarside/hangar/internal/jobs"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/org"
	"github.com/jamesagarside/hangar/internal/webhook"
)

// Namespaces resolves Namespace names and a User's Organisations.
// org.Service implements it.
type Namespaces interface {
	Get(ctx context.Context, name string) (*org.Namespace, error)
	GetByID(ctx context.Context, id uuid.UUID) (*org.Namespace, error)
	MembershipsOf(ctx context.Context, userID uuid.UUID) ([]org.Membership, error)
}

// QuotaChecker refuses writes that would take a Namespace over its storage
// quota (OPS-4). It returns an error explaining the quota when addBytes
// does not fit.
type QuotaChecker interface {
	CheckQuota(ctx context.Context, namespaceID uuid.UUID, addBytes int64) error
}

// Options configure a Store.
type Options struct {
	DB         *db.DB
	Git        *gitstore.Store
	LFS        LFSStore
	Namespaces Namespaces
	// Jobs, when set, receives index, scan and repo.purge Jobs.
	Jobs jobs.Queue
	// Webhooks, when set, receives Repo events.
	Webhooks webhook.Emitter
	// Audit, when set, records Repo create, visibility change, rename,
	// transfer, delete, restore and purge (05-auth.md "Audit events").
	Audit audit.Recorder
	// Quota, when set, is checked before pushes and Commits.
	Quota QuotaChecker
	// Index and Community, when set, give List its metadata filters,
	// facets and the downloads, likes and parameters sorts.
	Index     index.Indexer
	Community community.Service
	// AnonymousRead lets anonymous users list public Repos
	// (security.anonymous_read).
	AnonymousRead bool
	// RestoreWindow is how long a deleted Repo can be restored
	// (repos.restore_window). Zero means 7 days.
	RestoreWindow time.Duration
	// MaxBlobSize is the largest non-LFS file a push may add
	// (git.max_blob_size). Zero means 10 MiB.
	MaxBlobSize int64
	// MaxPackSize is the largest pack a push may send (git.max_pack_size).
	// Zero means 2 GiB.
	MaxPackSize int64
	// AllowForcePush lets anyone with write access force-push the default
	// branch (git.allow_force_push).
	AllowForcePush bool
	// CompactThreshold is git.compact_threshold: a Commit or push that
	// leaves more live packs than this enqueues a git.compact Job.
	CompactThreshold int
	// VerifyWait is lfs.verify_wait: how long a Commit or push waits for an
	// LFS object written through a presigned URL to be verified before it
	// fails. Zero means 10 minutes.
	VerifyWait time.Duration
	Log        *slog.Logger
	// Now overrides the clock, for tests. Times must have at most
	// microsecond precision, which both databases store exactly.
	Now func() time.Time
}

// Store is the Repo service backed by the database, gitstore and the LFS
// store. It implements Service.
type Store struct {
	db   *db.DB
	git  *gitstore.Store
	lfs  LFSStore
	ns   Namespaces
	opts Options
	log  *slog.Logger
	now  func() time.Time
	// fills shares one tree cache build between concurrent readers.
	fills singleflight.Group
	// verifies shares one LFS verification between concurrent writers.
	verifies singleflight.Group
	// blobs keeps small git blobs in memory.
	blobs *blobCache
}

var _ Service = (*Store)(nil)

// NewStore returns the Repo service.
func NewStore(opts Options) (*Store, error) {
	if opts.DB == nil || opts.Git == nil || opts.LFS == nil || opts.Namespaces == nil {
		return nil, errors.New("repo: DB, Git, LFS and Namespaces are required")
	}
	if opts.RestoreWindow <= 0 {
		opts.RestoreWindow = 7 * 24 * time.Hour
	}
	if opts.MaxBlobSize <= 0 {
		opts.MaxBlobSize = 10 << 20
	}
	if opts.MaxPackSize <= 0 {
		opts.MaxPackSize = 2 << 30
	}
	if opts.CompactThreshold <= 0 {
		opts.CompactThreshold = 20
	}
	if opts.VerifyWait <= 0 {
		opts.VerifyWait = 10 * time.Minute
	}
	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	now := opts.Now
	if now == nil {
		now = db.Now
	}
	return &Store{db: opts.DB, git: opts.Git, lfs: opts.LFS, ns: opts.Namespaces, opts: opts, log: log, now: now,
		blobs: newBlobCache(blobCacheSize)}, nil
}

// repoRow is a repos row with its Namespace name.
type repoRow struct {
	db.Repo       `bun:",extend"`
	NamespaceName string `bun:"ns_name,scanonly"`
}

// settings keys in repos.settings.
const (
	settingLicence = "licence"
	settingMirror  = "mirror"
)

func (s *Store) selectRepo(idb bun.IDB) *bun.SelectQuery {
	return idb.NewSelect().Model((*repoRow)(nil)).ColumnExpr("repo.*").ColumnExpr("ns.name AS ns_name").
		Join("JOIN namespaces AS ns ON ns.id = repo.namespace_id")
}

func toRepo(row *repoRow) *Repo {
	r := &Repo{
		UUID:          row.ID,
		ID:            model.RepoID{Type: model.RepoType(row.Type), Namespace: row.NamespaceName, Name: row.Name},
		NamespaceID:   row.NamespaceID,
		Visibility:    model.Visibility(row.Visibility),
		DefaultBranch: row.DefaultBranch,
		HeadRef:       row.HeadRef,
		Description:   row.Description,
		Tags:          append([]string{}, row.Tags...),
		CreatedAt:     row.CreatedAt.UTC(),
		UpdatedAt:     row.UpdatedAt.UTC(),
		SizeBytes:     row.SizeBytes,
		LFSBytes:      row.LFSBytes,
	}
	if row.CreatedBy != nil {
		r.CreatedBy = *row.CreatedBy
	}
	if row.LastModifiedAt != nil {
		r.LastModified = row.LastModifiedAt.UTC()
	} else {
		r.LastModified = r.CreatedAt
	}
	if row.DeletedAt != nil {
		t := row.DeletedAt.UTC()
		r.DeletedAt = &t
	}
	if v, ok := row.Settings[settingLicence].(string); ok {
		r.Licence = v
	}
	if v, ok := row.Settings[settingMirror].(bool); ok {
		r.Mirror = v
	}
	if len(row.Gating) > 0 {
		if b, err := json.Marshal(row.Gating); err == nil {
			_ = json.Unmarshal(b, &r.Gating)
		}
	}
	return r
}

// lookup finds a live Repo by ID, following redirects from renames and
// transfers.
func (s *Store) lookup(ctx context.Context, idb bun.IDB, id model.RepoID) (*repoRow, error) {
	if !id.Type.Valid() {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, id.Path())
	}
	row := new(repoRow)
	err := s.selectRepo(idb).Where("lower(ns.name) = lower(?)", id.Namespace).Where("repo.type = ?", string(id.Type)).
		Where("lower(repo.name) = lower(?)", id.Name).Where("repo.deleted_at IS NULL").Limit(1).Scan(ctx, row)
	if err == nil {
		return row, nil
	}
	if !db.IsNotFound(err) {
		return nil, fmt.Errorf("repo: look up %s: %w", id.Path(), err)
	}
	var target uuid.UUID
	err = idb.NewSelect().Model((*db.RepoRedirect)(nil)).Column("repo_id").Where("type = ?", string(id.Type)).
		Where("lower(old_namespace) = lower(?)", id.Namespace).Where("lower(old_name) = lower(?)", id.Name).Limit(1).Scan(ctx, &target)
	if db.IsNotFound(err) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, id.Path())
	}
	if err != nil {
		return nil, fmt.Errorf("repo: look up redirect for %s: %w", id.Path(), err)
	}
	row = new(repoRow)
	err = s.selectRepo(idb).Where("repo.id = ?", target).Where("repo.deleted_at IS NULL").Scan(ctx, row)
	if db.IsNotFound(err) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, id.Path())
	}
	if err != nil {
		return nil, fmt.Errorf("repo: look up %s: %w", id.Path(), err)
	}
	return row, nil
}

func (s *Store) byUUID(ctx context.Context, idb bun.IDB, id uuid.UUID) (*repoRow, error) {
	row := new(repoRow)
	err := s.selectRepo(idb).Where("repo.id = ?", id).Scan(ctx, row)
	if db.IsNotFound(err) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if err != nil {
		return nil, fmt.Errorf("repo: look up %s: %w", id, err)
	}
	return row, nil
}

// Get implements Service.
func (s *Store) Get(ctx context.Context, id model.RepoID) (*Repo, error) {
	row, err := s.lookup(ctx, s.db, id)
	if err != nil {
		return nil, err
	}
	return toRepo(row), nil
}

// GetByUUID implements Service.
func (s *Store) GetByUUID(ctx context.Context, id uuid.UUID) (*Repo, error) {
	row, err := s.byUUID(ctx, s.db, id)
	if err != nil {
		return nil, err
	}
	return toRepo(row), nil
}

// userRef returns the users.id to record for an actor, or nil for
// anonymous and system actors.
func userRef(p model.Principal) *uuid.UUID {
	if p.Kind != model.PrincipalUser || p.UserID == uuid.Nil {
		return nil
	}
	id := p.UserID
	return &id
}

// signature is the git author and committer for an actor: the display
// name and email, with fallbacks so git never sees an empty identity.
func signature(p model.Principal, when time.Time) Signature {
	name := p.DisplayName
	if name == "" {
		name = p.Username
	}
	if name == "" {
		name = "Hangar"
	}
	email := p.Email
	if email == "" {
		user := p.Username
		if user == "" {
			user = model.SystemUsername
		}
		email = user + "@users.noreply.hangar"
	}
	return Signature{Name: name, Email: email, When: when.UTC().Truncate(time.Second)}
}

func (s *Store) record(ctx context.Context, action audit.Action, actor model.Principal, r *Repo, data map[string]any) {
	if s.opts.Audit == nil {
		return
	}
	e := audit.Event{Action: action, Actor: actor, TargetType: "repo", TargetID: string(r.ID.Type) + ":" + r.ID.String(), Data: data}
	if err := s.opts.Audit.Record(ctx, e); err != nil {
		s.log.WarnContext(ctx, "record audit event", "action", string(action), "error", err.Error())
	}
}

func (s *Store) emit(ctx context.Context, name webhook.EventName, actor model.Principal, r *Repo, data map[string]any) {
	if s.opts.Webhooks == nil {
		return
	}
	id := r.ID
	if err := s.opts.Webhooks.Emit(ctx, webhook.NewEvent(name, actor, &id, r.UUID, r.NamespaceID, data)); err != nil {
		s.log.WarnContext(ctx, "emit webhook event", "event", string(name), "error", err.Error())
	}
}

func (s *Store) enqueue(ctx context.Context, j jobs.NewJob) {
	if s.opts.Jobs == nil {
		return
	}
	if _, err := s.opts.Jobs.Enqueue(ctx, j); err != nil {
		s.log.WarnContext(ctx, "enqueue job", "kind", string(j.Kind), "error", err.Error())
	}
}

func lowerAll(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = strings.ToLower(s)
	}
	return out
}
