package upstream_test

import (
	"context"
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
	"github.com/jamesagarside/hangar/internal/db/dbtest"
	"github.com/jamesagarside/hangar/internal/httpx"
	"github.com/jamesagarside/hangar/internal/jobs"
	"github.com/jamesagarside/hangar/internal/jobs/jobstest"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/org/orgtest"
	"github.com/jamesagarside/hangar/internal/repo"
	"github.com/jamesagarside/hangar/internal/repo/repotest"
	"github.com/jamesagarside/hangar/internal/secrets"
	"github.com/jamesagarside/hangar/internal/upstream"
	"github.com/jamesagarside/hangar/internal/upstream/upstreamtest"
	"github.com/jamesagarside/hangar/internal/webhook/webhooktest"
)

// dbRepos is the repotest fake plus the repos and namespaces rows the
// mirrors and dependencies tables refer to, as the real repo.Service
// writes them.
type dbRepos struct {
	*repotest.Fake
	d *db.DB
}

func (r *dbRepos) Create(ctx context.Context, p repo.CreateParams) (*repo.Repo, error) {
	out, err := r.Fake.Create(ctx, p)
	if err != nil {
		return nil, err
	}
	now := db.Now()
	ns := db.Namespace{ID: out.NamespaceID, Name: p.ID.Namespace, Kind: "org", Timestamps: db.Timestamps{CreatedAt: now, UpdatedAt: now}}
	if _, err := r.d.NewInsert().Model(&ns).On("CONFLICT (id) DO NOTHING").Exec(ctx); err != nil {
		return nil, err
	}
	row := db.Repo{ID: out.UUID, NamespaceID: out.NamespaceID, Name: p.ID.Name, Type: string(p.ID.Type), Visibility: string(out.Visibility),
		DefaultBranch: "main", HeadRef: "refs/heads/main", Tags: []string{}, Settings: map[string]any{}, Gating: map[string]any{},
		Timestamps: db.Timestamps{CreatedAt: now, UpdatedAt: now}}
	if _, err := r.d.NewInsert().Model(&row).Exec(ctx); err != nil {
		return nil, err
	}
	return out, nil
}

// SetMirrorState implements upstream.MirrorStateSetter over the fake.
func (r *dbRepos) SetMirrorState(_ context.Context, id model.RepoID, mirror bool) error {
	return r.Fake.SetMirror(id, mirror)
}

// windows is a test upstream.Windows: open between two hours of the day (UTC).
type windows struct {
	from, to int
	spec     string
}

func (w windows) Open(t time.Time) bool { h := t.UTC().Hour(); return h >= w.from && h < w.to }
func (w windows) NextOpen(t time.Time) time.Time {
	if w.Open(t) {
		return t
	}
	n := time.Date(t.Year(), t.Month(), t.Day(), w.from, 0, 0, 0, time.UTC)
	if !n.After(t) {
		n = n.Add(24 * time.Hour)
	}
	return n
}
func (w windows) WaitMessage() string { return "Waiting for pull window (" + w.spec + ")" }

func parseWindows(specs []string) (upstream.Windows, error) {
	if len(specs) != 1 {
		return nil, errors.New("the test parser takes one window")
	}
	var a, b int
	if _, err := fmt.Sscanf(specs[0], "%02d:00-%02d:00", &a, &b); err != nil {
		return nil, err
	}
	return windows{from: a, to: b, spec: specs[0]}, nil
}

// snoozed is what the test SnoozeFunc returns.
type snoozed struct {
	until  time.Time
	reason string
}

func (s *snoozed) Error() string { return "snoozed until " + s.until.Format(time.RFC3339) + ": " + s.reason }

type env struct {
	t        *testing.T
	ctx      context.Context
	d        *db.DB
	remote   *upstreamtest.Fake
	srv      *upstreamtest.Server
	repos    *dbRepos
	lfs      *repotest.FakeLFS
	orgs     *orgtest.Fake
	reg      *jobs.Registry
	queue    *jobstest.Fake
	limiter  *hookLimiter
	authz    *authtest.Authz
	layout   *datasetstest.Layout
	hooks    *webhooktest.Fake
	audit    *audittest.Fake
	registry *upstream.Registry
	svc      *upstream.Service
	signer   *httpx.Signer

	mu       sync.Mutex
	settings upstream.Settings
	now      time.Time
	tokens   map[uuid.UUID]string
}

type envOpts struct {
	partSize int64
	cfg      map[string]config.UpstreamConfig
	deps     config.DependencyConfig
	client   upstream.Client
}

func (e *env) Token(_ context.Context, userID, _ uuid.UUID) (string, bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	t, ok := e.tokens[userID]
	return t, ok, nil
}

func (e *env) setSettings(s upstream.Settings) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.settings = s
}

func (e *env) clock() time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.now
}

func (e *env) advance(d time.Duration) {
	e.mu.Lock()
	e.now = e.now.Add(d)
	e.mu.Unlock()
}

func newEnv(t *testing.T, d *db.DB, o envOpts) *env {
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
	e := &env{t: t, ctx: ctx, d: d, remote: upstreamtest.NewFake(t), orgs: orgtest.NewFake(), reg: jobs.NewRegistry(), limiter: &hookLimiter{},
		authz: authtest.NewAuthz(), layout: datasetstest.NewLayout(), hooks: &webhooktest.Fake{}, audit: audittest.NewFake(),
		now: time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC), tokens: map[uuid.UUID]string{}, signer: signer}
	e.settings = upstream.Settings{AllowOverrideWindows: true, PullThroughAllow: []string{"*"}, PullThroughMinRole: "user"}
	e.srv = upstreamtest.NewServer(t, e.remote)
	local := repotest.NewFake(t)
	e.lfs = repotest.NewFakeLFS(blobtest.NewFake())
	local.LFS = e.lfs
	e.repos = &dbRepos{Fake: local, d: d}
	e.queue = jobstest.NewFake(e.reg)
	e.queue.SetClock(e.clock)
	e.registry = upstream.NewRegistry(d, sealer, e.audit)
	cfg := o.cfg
	if cfg == nil {
		cfg = map[string]config.UpstreamConfig{"huggingface": {BaseURL: e.srv.URL, ViewerURL: e.srv.URL + "/viewer", Enabled: true}}
	}
	require.NoError(t, e.registry.SyncConfig(ctx, cfg))
	client := o.client
	if client == nil {
		client = newClient()
	}
	dc := o.deps
	if dc.MaxDepth == 0 {
		dc = config.DependencyConfig{AutoPullCode: true, MaxDepth: 5}
	}
	e.svc, err = upstream.New(upstream.Options{DB: d, Registry: e.registry, Client: client, Repos: e.repos, LFS: e.lfs, Orgs: e.orgs,
		Queue: e.queue, Limiter: e.limiter, Authz: e.authz, Layout: e.layout, Webhooks: e.hooks, MirrorState: e.repos,
		Settings: func(context.Context) (upstream.Settings, error) {
			e.mu.Lock()
			defer e.mu.Unlock()
			return e.settings, nil
		},
		ParseWindows: parseWindows,
		Snooze:       func(until time.Time, reason string) error { return &snoozed{until: until, reason: reason} },
		Signer:       signer, PublicURL: "http://hangar.test", Dependencies: dc, UserTokens: e, Audit: e.audit,
		PartSize: o.partSize, PullAttempts: 100, PollInterval: time.Second, PullThroughWait: 5 * time.Second, SpoolDir: t.TempDir(), Now: e.clock})
	require.NoError(t, err)
	require.NoError(t, e.svc.RegisterJobs(e.reg))
	return e
}

// user returns a signed-in user with a write session.
func (e *env) user(name string) model.Principal {
	p, err := e.orgs.AddUser(name)
	require.NoError(e.t, err)
	return p
}

// run runs Jobs until none is due, moving the clock past snoozes and
// backoff, at most rounds times.
func (e *env) run(rounds int) {
	for range rounds {
		e.queue.RunPending(e.ctx)
		pending, _, err := e.queue.List(e.ctx, jobs.Filter{Statuses: []jobs.Status{jobs.StatusPending, jobs.StatusRunning}})
		require.NoError(e.t, err)
		if len(pending) == 0 {
			return
		}
		e.advance(10 * time.Minute)
	}
}

func (e *env) job(id jobs.JobID) *jobs.Job {
	j, err := e.queue.Get(e.ctx, id)
	require.NoError(e.t, err)
	return j
}

func forEachDB(t *testing.T, fn func(t *testing.T, d *db.DB)) {
	dbtest.ForEachDriver(t, fn)
}

func lfsIDs(es []model.Entry) map[string]bool {
	out := map[string]bool{}
	for _, e := range es {
		if e.LFS != nil {
			out[e.Path] = e.Present
		}
	}
	return out
}

func joined(names ...string) string { return strings.Join(names, ",") }

// hookLimiter is the jobstest Limiter with a hook that runs once when the
// bytes spent reach a threshold, to change the world mid-transfer.
type hookLimiter struct {
	jobstest.Limiter
	mu    sync.Mutex
	at    int64
	fn    func()
	spent int64
}

func (l *hookLimiter) after(n int64, fn func()) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.at, l.fn = n, fn
}

func (l *hookLimiter) WaitN(ctx context.Context, name string, n int64) error {
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
