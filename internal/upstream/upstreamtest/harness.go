package upstreamtest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/audit/audittest"
	"github.com/jamesagarside/hangar/internal/auth/authtest"
	"github.com/jamesagarside/hangar/internal/blob/blobtest"
	"github.com/jamesagarside/hangar/internal/config"
	"github.com/jamesagarside/hangar/internal/datasets/datasetstest"
	"github.com/jamesagarside/hangar/internal/db"
	"github.com/jamesagarside/hangar/internal/httpx"
	"github.com/jamesagarside/hangar/internal/jobs"
	"github.com/jamesagarside/hangar/internal/jobs/jobstest"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/org/orgtest"
	"github.com/jamesagarside/hangar/internal/repo"
	"github.com/jamesagarside/hangar/internal/repo/repotest"
	"github.com/jamesagarside/hangar/internal/secrets"
	"github.com/jamesagarside/hangar/internal/upstream"
	"github.com/jamesagarside/hangar/internal/webhook/webhooktest"
)

// DBRepos is a repotest.Fake that also writes the repos and namespaces
// rows that the mirrors and dependencies tables refer to, as the real
// repo.Service does, and implements upstream.MirrorStateSetter.
type DBRepos struct {
	*repotest.Fake
	DB *db.DB

	mu     sync.Mutex
	marked map[string][]string
	// beforeApply runs before ApplyPack; see BeforeApplyPack.
	beforeApply func(ctx context.Context, p repo.ApplyPackParams) error
}

// BeforeApplyPack sets a hook that runs before each ApplyPack. When it
// returns an error, ApplyPack returns that error and stores nothing.
func (r *DBRepos) BeforeApplyPack(fn func(ctx context.Context, p repo.ApplyPackParams) error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.beforeApply = fn
}

// ApplyPack implements repo.Service, running the BeforeApplyPack hook first.
func (r *DBRepos) ApplyPack(ctx context.Context, p repo.ApplyPackParams) error {
	r.mu.Lock()
	fn := r.beforeApply
	r.mu.Unlock()
	if fn != nil {
		if err := fn(ctx, p); err != nil {
			return err
		}
	}
	return r.Fake.ApplyPack(ctx, p)
}

// SetLFSPresence implements repo.Service and records the objects marked
// present in each Repo, which the fake's Entry cannot tell apart from
// objects stored for another Repo.
func (r *DBRepos) SetLFSPresence(ctx context.Context, id model.RepoID, refs []repo.LFSRef) error {
	if err := r.Fake.SetLFSPresence(ctx, id, refs); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.marked == nil {
		r.marked = map[string][]string{}
	}
	key := strings.ToLower(string(id.Type) + ":" + id.String())
	for _, ref := range refs {
		if ref.Present {
			r.marked[key] = append(r.marked[key], ref.OID)
		}
	}
	return nil
}

// MarkedPresent returns the OIDs marked present in a Repo so far.
func (r *DBRepos) MarkedPresent(id model.RepoID) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.marked[strings.ToLower(string(id.Type)+":"+id.String())]...)
}

// Create implements repo.Service.
func (r *DBRepos) Create(ctx context.Context, p repo.CreateParams) (*repo.Repo, error) {
	out, err := r.Fake.Create(ctx, p)
	if err != nil {
		return nil, err
	}
	now := db.Now()
	ns := db.Namespace{ID: out.NamespaceID, Name: p.ID.Namespace, Kind: "org", Timestamps: db.Timestamps{CreatedAt: now, UpdatedAt: now}}
	if _, err := r.DB.NewInsert().Model(&ns).On("CONFLICT (id) DO NOTHING").Exec(ctx); err != nil {
		return nil, err
	}
	row := db.Repo{ID: out.UUID, NamespaceID: out.NamespaceID, Name: p.ID.Name, Type: string(p.ID.Type), Visibility: string(out.Visibility),
		DefaultBranch: "main", HeadRef: "refs/heads/main", Tags: []string{}, Settings: map[string]any{}, Gating: map[string]any{},
		Timestamps: db.Timestamps{CreatedAt: now, UpdatedAt: now}}
	if _, err := r.DB.NewInsert().Model(&row).Exec(ctx); err != nil {
		return nil, err
	}
	return out, nil
}

// SetMirrorState implements upstream.MirrorStateSetter.
func (r *DBRepos) SetMirrorState(_ context.Context, id model.RepoID, mirror bool) error {
	return r.SetMirror(id, mirror)
}

// hourWindows is an upstream.Windows open between two whole hours (UTC).
type hourWindows struct {
	from, to int
	spec     string
}

func (w hourWindows) Open(t time.Time) bool { h := t.UTC().Hour(); return h >= w.from && h < w.to }

func (w hourWindows) NextOpen(t time.Time) time.Time {
	if w.Open(t) {
		return t
	}
	n := time.Date(t.Year(), t.Month(), t.Day(), w.from, 0, 0, 0, time.UTC)
	if !n.After(t) {
		n = n.Add(24 * time.Hour)
	}
	return n
}

func (w hourWindows) WaitMessage() string { return "Waiting for pull window (" + w.spec + ")" }

// ParseHourWindows parses one window of whole hours, such as
// "01:00-07:00", in UTC. jobs.ParseWindows replaces it in a real Instance.
func ParseHourWindows(specs []string) (upstream.Windows, error) {
	if len(specs) != 1 {
		return nil, errors.New("the test parser takes one window")
	}
	var a, b int
	if _, err := fmt.Sscanf(specs[0], "%02d:00-%02d:00", &a, &b); err != nil {
		return nil, err
	}
	return hourWindows{from: a, to: b, spec: specs[0]}, nil
}

// Snoozed is the error the Harness's SnoozeFunc returns. jobstest.Fake
// treats it as a retryable failure; jobs.Snooze does not use an attempt.
type Snoozed struct {
	Until  time.Time
	Reason string
}

func (s *Snoozed) Error() string {
	return "snoozed until " + s.Until.Format(time.RFC3339) + ": " + s.Reason
}

// HookLimiter is a jobstest.Limiter that runs a hook once when the bytes
// spent reach a threshold, to change the world in the middle of a transfer.
type HookLimiter struct {
	jobstest.Limiter
	mu    sync.Mutex
	at    int64
	fn    func()
	spent int64
}

// After runs fn once, when n bytes have been spent.
func (l *HookLimiter) After(n int64, fn func()) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.at, l.fn = n, fn
}

// WaitN implements jobs.Limiter.
func (l *HookLimiter) WaitN(ctx context.Context, name string, n int64) error {
	if err := l.Limiter.WaitN(ctx, name, n); err != nil {
		return err
	}
	l.mu.Lock()
	l.spent += n
	var fn func()
	if l.fn != nil && l.spent >= l.at {
		fn, l.fn = l.fn, nil
	}
	l.mu.Unlock()
	if fn != nil {
		fn()
	}
	return nil
}

// HarnessOptions configure a Harness.
type HarnessOptions struct {
	// PartSize is the multipart part size; zero means the default.
	PartSize int64
	// Upstreams replaces the default configuration: one huggingface
	// Upstream at the Server.
	Upstreams map[string]config.UpstreamConfig
	// Deps defaults to auto_pull_code with max_depth 5.
	Deps config.DependencyConfig
	// Client defaults to an upstream.HTTPClient with no backoff waits.
	Client upstream.Client
	// Limiter defaults to a HookLimiter.
	Limiter jobs.Limiter
	// PullThroughWait bounds how long a Pull-through waits for planning.
	// Zero means the Service's default (30 seconds), which a test that
	// expects planning to finish keeps: planning runs git and takes
	// seconds on a loaded machine.
	PullThroughWait time.Duration
}

// Harness is an upstream.Service over fakes: a Fake Upstream behind a
// Server, local Repos in a repotest.Fake with database rows, an in-memory
// Job queue that runs when asked, and a clock the test moves.
type Harness struct {
	T         testing.TB
	Ctx       context.Context
	DB        *db.DB
	Remote    *Fake
	Server    *Server
	Repos     *DBRepos
	LFS       *repotest.FakeLFS
	Orgs      *orgtest.Fake
	Jobs      *jobs.Registry
	Queue     *jobstest.Fake
	Limiter   *HookLimiter
	Authz     *authtest.Authz
	Layout    *datasetstest.Layout
	Hooks     *webhooktest.Fake
	Audit     *audittest.Fake
	Upstreams *upstream.Registry
	Service   *upstream.Service
	Signer    *httpx.Signer
	Client    upstream.Client
	// Quota enforces Namespace quotas; it has no limits until a test sets one.
	Quota *Quota

	mu       sync.Mutex
	partSize int64
	ptWait   time.Duration
	settings upstream.Settings
	now      time.Time
	tokens   map[uuid.UUID]string
}

// NewHarness builds a Harness on d.
func NewHarness(t testing.TB, d *db.DB, o HarnessOptions) *Harness {
	t.Helper()
	ctx := context.Background()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	sealer, err := secrets.New(key)
	require.NoError(t, err)
	signer, err := httpx.NewSigner(key)
	require.NoError(t, err)
	h := &Harness{T: t, Ctx: ctx, DB: d, Remote: NewFake(t), Orgs: orgtest.NewFake(), Jobs: jobs.NewRegistry(), Limiter: &HookLimiter{},
		Authz: authtest.NewAuthz(), Layout: datasetstest.NewLayout(), Hooks: &webhooktest.Fake{}, Audit: audittest.NewFake(), Quota: NewQuota(),
		now: time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC), tokens: map[uuid.UUID]string{}, Signer: signer, partSize: o.PartSize,
		ptWait: o.PullThroughWait}
	h.settings = upstream.Settings{AllowOverrideWindows: true, PullThroughAllow: []string{"*"}, PullThroughMinRole: "user"}
	h.Server = NewServer(t, h.Remote)
	local := repotest.NewFake(t)
	h.LFS = repotest.NewFakeLFS(blobtest.NewFake())
	local.LFS = h.LFS
	h.Repos = &DBRepos{Fake: local, DB: d}
	h.Queue = jobstest.NewFake(h.Jobs)
	h.Queue.SetClock(h.Now)
	h.Upstreams = upstream.NewRegistry(d, sealer, h.Audit)
	cfg := o.Upstreams
	if cfg == nil {
		cfg = map[string]config.UpstreamConfig{upstream.DefaultUpstream: {BaseURL: h.Server.URL, ViewerURL: h.Server.URL + "/viewer", Enabled: true}}
	}
	require.NoError(t, h.Upstreams.SyncConfig(ctx, cfg))
	h.Client = o.Client
	if h.Client == nil {
		h.Client = upstream.NewHTTPClient(upstream.HTTPOptions{Version: "test", BaseBackoff: time.Millisecond,
			Sleep: func(context.Context, time.Duration) error { return nil }})
	}
	dc := o.Deps
	if dc.MaxDepth == 0 {
		dc = config.DependencyConfig{AutoPullCode: true, MaxDepth: 5}
	}
	var limiter jobs.Limiter = h.Limiter
	if o.Limiter != nil {
		limiter = o.Limiter
	}
	h.Service, err = upstream.New(upstream.Options{DB: d, Registry: h.Upstreams, Client: h.Client, Repos: h.Repos, LFS: h.LFS, Orgs: h.Orgs,
		Queue: h.Queue, Limiter: limiter, Authz: h.Authz, Layout: h.Layout, Webhooks: h.Hooks, MirrorState: h.Repos,
		Settings: func(context.Context) (upstream.Settings, error) {
			h.mu.Lock()
			defer h.mu.Unlock()
			return h.settings, nil
		},
		ParseWindows: ParseHourWindows,
		Snooze:       func(until time.Time, reason string) error { return &Snoozed{Until: until, Reason: reason} },
		Signer:       signer, PublicURL: "http://hangar.test", Dependencies: dc, UserTokens: h, Quota: h.Quota, Audit: h.Audit,
		PartSize: o.PartSize, PullAttempts: 100, PollInterval: time.Second, PullThroughWait: o.PullThroughWait, SpoolDir: t.TempDir(), Now: h.Now})
	require.NoError(t, err)
	require.NoError(t, h.Service.RegisterJobs(h.Jobs))
	return h
}

// Token implements upstream.UserTokens with tokens set by SetToken.
func (h *Harness) Token(_ context.Context, userID, _ uuid.UUID) (string, bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	t, ok := h.tokens[userID]
	return t, ok, nil
}

// SetToken saves a user's upstream token.
func (h *Harness) SetToken(userID uuid.UUID, token string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.tokens[userID] = token
}

// SetSettings replaces the Instance settings.
func (h *Harness) SetSettings(s upstream.Settings) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.settings = s
}

// Now is the Harness clock.
func (h *Harness) Now() time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.now
}

// Advance moves the clock.
func (h *Harness) Advance(d time.Duration) {
	h.mu.Lock()
	h.now = h.now.Add(d)
	h.mu.Unlock()
}

// MirrorNamespace returns the ID of the reserved Mirror Namespace name,
// creating it as the first Pull into it would, so a test can set its quota
// before pulling.
func (h *Harness) MirrorNamespace(name string) uuid.UUID {
	ns, err := h.Orgs.EnsureMirrorNamespace(h.Ctx, name)
	require.NoError(h.T, err)
	return ns.ID
}

// User adds a signed-in user with a personal Namespace.
func (h *Harness) User(name string) model.Principal {
	p, err := h.Orgs.AddUser(name)
	require.NoError(h.T, err)
	return p
}

// Run runs Jobs until none is due, moving the clock ten minutes past
// snoozes and backoff each round, at most rounds times.
func (h *Harness) Run(rounds int) {
	for range rounds {
		h.Queue.RunPending(h.Ctx)
		pending, _, err := h.Queue.List(h.Ctx, jobs.Filter{Statuses: []jobs.Status{jobs.StatusPending, jobs.StatusRunning}})
		require.NoError(h.T, err)
		if len(pending) == 0 {
			return
		}
		h.Advance(10 * time.Minute)
	}
}

// Job returns a Job.
func (h *Harness) Job(id jobs.JobID) *jobs.Job {
	j, err := h.Queue.Get(h.Ctx, id)
	require.NoError(h.T, err)
	return j
}

// AddUpstream adds an admin Upstream at the Server.
func (h *Harness) AddUpstream(name, prefix string) error {
	_, err := h.Upstreams.Create(h.Ctx, upstream.UpstreamParams{Name: name, BaseURL: h.Server.URL, Enabled: true, NamespacePrefix: prefix}, model.System())
	return err
}

// Seed commits regular files and LFS files to a Repo on the Fake
// Upstream, creating it public if needed, and returns the Commit.
func (h *Harness) Seed(id model.RepoID, files, lfs map[string][]byte) model.CommitSHA {
	return Seed(h.T, h.Remote, id, files, lfs)
}

// NewService returns another Service over the same database, Repos, LFS
// store, queue and Upstream, as a second process would have: it shares
// nothing in memory with the first, so tests can stop one mid-transfer
// and carry on with the other.
func (h *Harness) NewService(limiter jobs.Limiter) *upstream.Service {
	h.T.Helper()
	if limiter == nil {
		limiter = h.Limiter
	}
	svc, err := upstream.New(upstream.Options{DB: h.DB, Registry: h.Upstreams, Client: h.Client, Repos: h.Repos, LFS: h.LFS, Orgs: h.Orgs,
		Queue: h.Queue, Limiter: limiter, Authz: h.Authz, Layout: h.Layout, Webhooks: h.Hooks, MirrorState: h.Repos,
		Settings: func(context.Context) (upstream.Settings, error) {
			h.mu.Lock()
			defer h.mu.Unlock()
			return h.settings, nil
		},
		ParseWindows: ParseHourWindows,
		Snooze:       func(until time.Time, reason string) error { return &Snoozed{Until: until, Reason: reason} },
		Signer:       h.Signer, PublicURL: "http://hangar.test", UserTokens: h, Quota: h.Quota, Audit: h.Audit, PartSize: h.partSize,
		PullAttempts: 100, PollInterval: time.Second, PullThroughWait: h.ptWait, SpoolDir: h.T.TempDir(), Now: h.Now})
	require.NoError(h.T, err)
	return svc
}

// Progress is a jobs.Progress that keeps the last report and result, for
// running a handler outside the queue.
type Progress struct {
	mu     sync.Mutex
	Last   jobs.ProgressUpdate
	Result []byte
}

// Report implements jobs.Progress.
func (p *Progress) Report(_ context.Context, u jobs.ProgressUpdate) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Last = u
	return nil
}

// SetResult implements jobs.Progress.
func (p *Progress) SetResult(_ context.Context, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Result = b
	return nil
}
