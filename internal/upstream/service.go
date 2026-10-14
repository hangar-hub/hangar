package upstream

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/google/uuid"

	"github.com/jamesagarside/hangar/internal/audit"
	"github.com/jamesagarside/hangar/internal/auth"
	"github.com/jamesagarside/hangar/internal/config"
	"github.com/jamesagarside/hangar/internal/datasets"
	"github.com/jamesagarside/hangar/internal/db"
	"github.com/jamesagarside/hangar/internal/httpx"
	"github.com/jamesagarside/hangar/internal/jobs"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/org"
	"github.com/jamesagarside/hangar/internal/repo"
	"github.com/jamesagarside/hangar/internal/telemetry"
	"github.com/jamesagarside/hangar/internal/upstream/deps"
	"github.com/jamesagarside/hangar/internal/webhook"
)

// Settings are the Instance settings Pulls follow (W5's auth.SettingsStore
// holds them; they can change at run time in the admin UI).
type Settings struct {
	// Windows are pull.windows, such as "01:00-07:00"; empty means any time.
	Windows              []string
	AllowOverrideWindows bool
	PullThroughEnabled   bool
	PullThroughAllow     []string
	// PullThroughMinRole is "anonymous" or "user" (any signed-in user).
	PullThroughMinRole string
}

// SettingsFunc returns the current Settings.
type SettingsFunc func(ctx context.Context) (Settings, error)

// Windows says when Pull transfers may run (PULL-7). jobs.Windows
// implements it.
type Windows interface {
	Open(t time.Time) bool
	NextOpen(t time.Time) time.Time
	WaitMessage() string
}

// WindowParser parses pull.windows; jobs.ParseWindows with the Instance
// time zone implements it.
type WindowParser func(specs []string) (Windows, error)

// SnoozeFunc returns the error that puts a running Job back in the queue
// until a time without using an attempt (jobs.Snooze).
type SnoozeFunc func(until time.Time, reason string) error

// UserTokens returns a user's saved token for an Upstream (PULL-8). W5's
// auth.UpstreamTokenStore implements it.
type UserTokens interface {
	Token(ctx context.Context, userID, upstreamID uuid.UUID) (token string, ok bool, err error)
}

// MirrorStateSetter marks a Repo as a Mirror or detaches it. W2's
// repo.Service implements it.
type MirrorStateSetter interface {
	SetMirrorState(ctx context.Context, id model.RepoID, mirror bool) error
}

// Options configure a Service. Every field except UserTokens, Quota,
// Audit, Instruments, Log, Now and the tuning values is required.
type Options struct {
	DB           *db.DB
	Registry     *Registry
	Client       Client
	Repos        repo.Service
	LFS          repo.LFSStore
	Orgs         org.Service
	Queue        jobs.Queue
	Limiter      jobs.Limiter
	Authz        auth.Authz
	Layout       datasets.Layout
	Webhooks     webhook.Emitter
	Settings     SettingsFunc
	ParseWindows WindowParser
	Snooze       SnoozeFunc
	MirrorState  MirrorStateSetter
	// Signer signs Pull-through URLs for git-lfs downloads.
	Signer    *httpx.Signer
	PublicURL string
	// Dependencies are pull.dependencies.
	Dependencies config.DependencyConfig
	UserTokens   UserTokens
	// Quota, when set, enforces Namespace storage quotas on Pulls (OPS-4):
	// a Pull that would take its Mirror's Namespace over the quota stores
	// nothing more and fails with reason quota_exceeded.
	Quota       QuotaChecker
	Audit       audit.Recorder
	Instruments *telemetry.Instruments
	Log         *slog.Logger
	// PartSize is the multipart part size for transfers. Zero means 64 MiB.
	PartSize int64
	// PollInterval is how often a Pull checks its file transfers. Zero
	// means 3 seconds.
	PollInterval time.Duration
	// PullThroughWait bounds how long a Pull-through request waits for
	// planning. Zero means 30 seconds.
	PullThroughWait time.Duration
	// SpoolDir holds Pull-through transfers that clients read while they
	// arrive. Empty means the system temporary directory.
	SpoolDir string
	// PullAttempts and FileAttempts cap retries of failed pull and
	// pull.file runs. Snoozed runs (waiting for transfers or a pull
	// window) do not count. Zero means 5 and 8.
	PullAttempts int
	FileAttempts int
	// Now is the clock. Nil means time.Now.
	Now func() time.Time
}

// Service plans and runs Pulls, keeps Mirrors in step, searches Upstreams
// for Discover, resolves Dependencies and serves Pull-through.
type Service struct {
	opts    Options
	mirrors mirrorStore
	deps    *deps.Store
	cache   *searchCache
	hub     *hub
	log     *slog.Logger
}

// New returns a Service.
func New(opts Options) (*Service, error) {
	var missing []string
	check := func(name string, ok bool) {
		if !ok {
			missing = append(missing, name)
		}
	}
	check("DB", opts.DB != nil)
	check("Registry", opts.Registry != nil)
	check("Client", opts.Client != nil)
	check("Repos", opts.Repos != nil)
	check("LFS", opts.LFS != nil)
	check("Orgs", opts.Orgs != nil)
	check("Queue", opts.Queue != nil)
	check("Limiter", opts.Limiter != nil)
	check("Authz", opts.Authz != nil)
	check("Layout", opts.Layout != nil)
	check("Webhooks", opts.Webhooks != nil)
	check("Settings", opts.Settings != nil)
	check("ParseWindows", opts.ParseWindows != nil)
	check("Snooze", opts.Snooze != nil)
	check("MirrorState", opts.MirrorState != nil)
	if len(missing) > 0 {
		return nil, fmt.Errorf("upstream service: missing %v", missing)
	}
	if opts.PartSize <= 0 {
		opts.PartSize = 64 << 20
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = 3 * time.Second
	}
	if opts.PullThroughWait <= 0 {
		opts.PullThroughWait = 30 * time.Second
	}
	if opts.SpoolDir == "" {
		opts.SpoolDir = os.TempDir()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.PullAttempts <= 0 {
		opts.PullAttempts = 5
	}
	if opts.FileAttempts <= 0 {
		opts.FileAttempts = 8
	}
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	if opts.Dependencies.MaxDepth <= 0 {
		opts.Dependencies.MaxDepth = 5
	}
	return &Service{opts: opts, mirrors: mirrorStore{db: opts.DB}, deps: deps.NewStore(opts.DB), cache: newSearchCache(5*time.Minute, 1000),
		hub: newHub(opts.SpoolDir), log: opts.Log}, nil
}

// Registry returns the Upstream registry.
func (s *Service) Registry() *Registry { return s.opts.Registry }

// Dependencies returns the Dependency store.
func (s *Service) Dependencies() *deps.Store { return s.deps }

// RegisterJobs registers the pull, pull.file and mirror.sync handlers.
func (s *Service) RegisterJobs(reg *jobs.Registry) error {
	if err := reg.Register(jobs.KindPull, s.runPull, jobs.HandlerOptions{MaxAttempts: s.opts.PullAttempts}); err != nil {
		return err
	}
	if err := reg.Register(jobs.KindPullFile, s.runFile, jobs.HandlerOptions{MaxAttempts: s.opts.FileAttempts}); err != nil {
		return err
	}
	return reg.Register(jobs.KindMirrorSync, s.runSync, jobs.HandlerOptions{MaxAttempts: 3})
}

// credentials picks the requesting user's token for the Upstream, then
// the Instance token, then none (06 "Upstreams").
func (s *Service) credentials(ctx context.Context, up *Upstream, user uuid.UUID) (Credentials, error) {
	if user != uuid.Nil && s.opts.UserTokens != nil {
		tok, ok, err := s.opts.UserTokens.Token(ctx, user, up.ID)
		if err != nil {
			return Credentials{}, fmt.Errorf("read the user's upstream token: %w", err)
		}
		if ok && tok != "" {
			return Credentials{Token: tok}, nil
		}
	}
	tok, err := s.opts.Registry.InstanceToken(ctx, up.ID)
	if err != nil {
		return Credentials{}, fmt.Errorf("read the Instance upstream token: %w", err)
	}
	return Credentials{Token: tok}, nil
}

// Pull request errors.
var (
	// ErrConflict means the Repo ID is taken: by a local Repo or
	// Namespace, or by a Mirror of another Upstream.
	ErrConflict = errors.New("repo id is taken")
	// ErrOverrideNotAllowed means "Pull now" is turned off on this Instance.
	ErrOverrideNotAllowed = errors.New("pulling outside the pull windows is turned off on this instance")
)

// PullRequest asks for a Pull.
type PullRequest struct {
	// Upstream is the Upstream's name; "" means the default Upstream.
	Upstream string
	// Repo is the upstream Repo ID.
	Repo model.RepoID
	// Revision defaults to the upstream default branch.
	Revision  string
	Selection Selection
	// Track makes the Mirror follow the Revision's branch (PULL-9).
	Track bool
	// Now ignores pull windows ("Pull now"), not the bandwidth cap.
	Now   bool
	Actor model.Principal
}

// pullPayload is the payload of a pull Job.
type pullPayload struct {
	UpstreamID   uuid.UUID      `json:"upstream_id"`
	Upstream     string         `json:"upstream"`
	UpstreamRepo string         `json:"upstream_repo"`
	Type         model.RepoType `json:"type"`
	Repo         string         `json:"repo"`
	Revision     string         `json:"revision,omitempty"`
	Selection    Selection      `json:"selection"`
	Track        bool           `json:"track,omitempty"`
	Now          bool           `json:"now,omitempty"`
	RequestedBy  uuid.UUID      `json:"requested_by,omitempty"`
	Sync         bool           `json:"sync,omitempty"`
	PullThrough  bool           `json:"pull_through,omitempty"`
	Depth        int            `json:"depth,omitempty"`
	Chain        []string       `json:"chain,omitempty"`
}

func (p pullPayload) upstreamID() (model.RepoID, error) {
	return model.ParseRepoID(p.Type, p.UpstreamRepo)
}
func (p pullPayload) localID() (model.RepoID, error) { return model.ParseRepoID(p.Type, p.Repo) }

// dedupeKey joins Pulls of the same files of the same Revision.
func (p pullPayload) dedupeKey() string {
	if p.Sync {
		return "sync:" + string(p.Type) + ":" + p.Repo
	}
	b, _ := json.Marshal(p.Selection)
	sum := sha256.Sum256(b)
	return string(p.Type) + ":" + p.Repo + "@" + p.Revision + "#" + hex.EncodeToString(sum[:8])
}

// StartPull checks a Pull request and queues it. It returns the pull Job
// and the Mirror's ID in the Arsenal.
//
// Who may pull: into an existing Mirror, callers with pull_mirror on it
// (write on its Namespace), or any signed-in caller with a write-scope
// Token while the Namespace is still an Instance-owned reserved Mirror
// Namespace; a new Mirror needs a signed-in caller with write scope.
func (s *Service) StartPull(ctx context.Context, req PullRequest) (jobs.JobID, model.RepoID, error) {
	sel, err := req.Selection.Normalise()
	if err != nil {
		return uuid.Nil, model.RepoID{}, err
	}
	if err := req.Repo.Validate(); err != nil {
		return uuid.Nil, model.RepoID{}, fmt.Errorf("%w: %w", ErrInvalidSelection, err)
	}
	up, err := s.opts.Registry.Enabled(ctx, req.Upstream)
	if err != nil {
		return uuid.Nil, model.RepoID{}, err
	}
	local := up.LocalID(req.Repo)
	if err := local.Validate(); err != nil {
		return uuid.Nil, model.RepoID{}, fmt.Errorf("%w: %w", ErrInvalidSelection, err)
	}
	if err := s.authorisePull(ctx, req.Actor, up, local, req.Repo); err != nil {
		return uuid.Nil, local, err
	}
	if req.Now {
		st, err := s.opts.Settings(ctx)
		if err != nil {
			return uuid.Nil, local, err
		}
		if !st.AllowOverrideWindows {
			return uuid.Nil, local, ErrOverrideNotAllowed
		}
	}
	p := pullPayload{UpstreamID: up.ID, Upstream: up.Name, UpstreamRepo: req.Repo.String(), Type: req.Repo.Type, Repo: local.String(),
		Revision: req.Revision, Selection: sel, Track: req.Track, Now: req.Now, RequestedBy: req.Actor.UserID}
	id, err := s.enqueuePull(ctx, p, uuid.Nil, jobs.PriorityNormal)
	return id, local, err
}

func (s *Service) enqueuePull(ctx context.Context, p pullPayload, parent uuid.UUID, priority int) (jobs.JobID, error) {
	return s.opts.Queue.Enqueue(ctx, jobs.NewJob{Kind: jobs.KindPull, DedupeKey: p.dedupeKey(), Payload: p, Priority: priority,
		CreatedBy: p.RequestedBy, ParentID: parent})
}

// repoTarget describes a Repo for auth.Authz.
func repoTarget(r *repo.Repo) auth.Target {
	return auth.Target{Repo: &auth.RepoTarget{ID: r.ID, UUID: r.UUID, NamespaceID: r.NamespaceID, Visibility: r.Visibility,
		Gated: r.Gating.Enabled(), Mirror: r.Mirror}}
}

func (s *Service) authorisePull(ctx context.Context, p model.Principal, up *Upstream, local, upstreamRepo model.RepoID) error {
	if p.Kind == model.PrincipalSystem {
		return nil
	}
	if p.IsAnonymous() {
		return fmt.Errorf("%w: sign in to pull into the arsenal", auth.ErrForbidden)
	}
	if !p.MaxScope().Includes(model.ScopeWrite) {
		return fmt.Errorf("%w: pulling needs a token with write scope", auth.ErrForbidden)
	}
	r, err := s.opts.Repos.Get(ctx, local)
	switch {
	case errors.Is(err, repo.ErrNotFound):
		ns, err := s.opts.Orgs.Get(ctx, local.Namespace)
		if err == nil && !ns.ReservedMirror {
			return fmt.Errorf("%w: the Namespace %s is a local Namespace, so %s cannot be mirrored with its upstream ID; an admin can set a namespace_prefix for the %s Upstream",
				ErrConflict, local.Namespace, upstreamRepo.String(), up.Name)
		}
		if err != nil && !errors.Is(err, org.ErrNotFound) {
			return err
		}
		return nil
	case err != nil:
		return err
	}
	if !r.ID.Equal(local) {
		return fmt.Errorf("%w: %s was renamed to %s and is no longer a Mirror of %s", ErrConflict, local, r.ID, upstreamRepo)
	}
	m, err := s.mirrors.active(ctx, r.UUID, r.ID.Type)
	if errors.Is(err, ErrNotMirror) {
		if err := s.opts.Authz.Can(ctx, p, auth.ActionRead, repoTarget(r)); err != nil {
			return err
		}
		return fmt.Errorf("%w: %s is a local Repo, not a Mirror", ErrConflict, r.ID.Path())
	}
	if err != nil {
		return err
	}
	if m.UpstreamID != up.ID || !m.UpstreamRepo.Equal(upstreamRepo) {
		return fmt.Errorf("%w: %s is a Mirror of %s from another Upstream", ErrConflict, r.ID.Path(), m.UpstreamRepo)
	}
	if ns, err := s.opts.Orgs.GetByID(ctx, r.NamespaceID); err == nil && ns.ReservedMirror {
		return s.opts.Authz.Can(ctx, p, auth.ActionRead, repoTarget(r))
	}
	return s.opts.Authz.Can(ctx, p, auth.ActionPullMirror, repoTarget(r))
}

// Mirror returns a Repo's Mirror settings and state, or ErrNotMirror.
func (s *Service) Mirror(ctx context.Context, r *repo.Repo) (*Mirror, *Upstream, error) {
	m, err := s.mirrors.get(ctx, r.UUID, r.ID.Type)
	if err != nil {
		return nil, nil, err
	}
	up, err := s.opts.Registry.Get(ctx, m.UpstreamID)
	if err != nil {
		return nil, nil, err
	}
	return m, up, nil
}

func (s *Service) emit(ctx context.Context, name webhook.EventName, actor model.Principal, r *repo.Repo, data map[string]any) {
	var id *model.RepoID
	var repoUUID, nsID uuid.UUID
	if r != nil {
		rid := r.ID
		id, repoUUID, nsID = &rid, r.UUID, r.NamespaceID
	}
	if err := s.opts.Webhooks.Emit(ctx, webhook.NewEvent(name, actor, id, repoUUID, nsID, data)); err != nil {
		s.log.WarnContext(ctx, "emit webhook event", "event", string(name), "error", err.Error())
	}
}

// commitJob is the payload of index and scan Jobs (repo.CommitJob on W2's branch).
type commitJob struct {
	RepoUUID uuid.UUID       `json:"repo_uuid"`
	Repo     string          `json:"repo"`
	Type     model.RepoType  `json:"type"`
	Commit   model.CommitSHA `json:"commit"`
}
