//go:build compat

// The scenarios of this suite on the real pieces: internal/repo (W2) on
// the filesystem Blob store (W1), the database Job queue, worker and
// shared bandwidth limiter (W1), Organisations from the database (W5),
// and a second Instance built from W2's Repo service, W3's git server and
// W4's Hub API: go test -tags compat ./compat/upstream/
package upstreamcompat

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/api"
	"github.com/jamesagarside/hangar/internal/api/gen"
	"github.com/jamesagarside/hangar/internal/audit/audittest"
	"github.com/jamesagarside/hangar/internal/auth"
	"github.com/jamesagarside/hangar/internal/auth/authtest"
	"github.com/jamesagarside/hangar/internal/blob/fs"
	"github.com/jamesagarside/hangar/internal/collections/collectionstest"
	"github.com/jamesagarside/hangar/internal/community/communitytest"
	"github.com/jamesagarside/hangar/internal/config"
	"github.com/jamesagarside/hangar/internal/datasets/datasetstest"
	"github.com/jamesagarside/hangar/internal/db"
	"github.com/jamesagarside/hangar/internal/db/dbtest"
	"github.com/jamesagarside/hangar/internal/gc"
	"github.com/jamesagarside/hangar/internal/gitserver"
	"github.com/jamesagarside/hangar/internal/hfapi"
	"github.com/jamesagarside/hangar/internal/httpx"
	"github.com/jamesagarside/hangar/internal/index/indextest"
	"github.com/jamesagarside/hangar/internal/jobs"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/org"
	"github.com/jamesagarside/hangar/internal/repo"
	"github.com/jamesagarside/hangar/internal/repo/repotest"
	"github.com/jamesagarside/hangar/internal/scan/scantest"
	"github.com/jamesagarside/hangar/internal/secrets"
	"github.com/jamesagarside/hangar/internal/upstream"
	"github.com/jamesagarside/hangar/internal/upstream/importer"
	"github.com/jamesagarside/hangar/internal/webhook/webhooktest"
	"github.com/jamesagarside/hangar/internal/xet/xettest"
)

var instanceKey = bytes.Repeat([]byte{7}, 32)

// listen reserves a local port so a server can know its URL before it starts.
func listen(t *testing.T) (net.Listener, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	return ln, "http://" + ln.Addr().String()
}

// classify records the protocol before authentication, as cmd/hangar must.
func classify(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpx.SetProtocol(r.Context(), httpx.Classify(r, "").Protocol())
		next.ServeHTTP(w, r)
	})
}

// hubDeps are hfapi.Deps over a real Repo service and fakes for the rest.
func hubDeps(base string, real *repotest.Real, orgs org.Service, authz *authtest.Authz, authn *authtest.Authenticator) hfapi.Deps {
	return hfapi.Deps{Settings: hfapi.Settings{PublicURL: base}, Repos: real.Store, LFS: real.LFS, Authz: authz, Orgs: orgs,
		Community: communitytest.NewFake(), Collections: collectionstest.NewFake(), Index: indextest.NewFake(), Scan: scantest.NewFake(real.Store),
		Xet: xettest.NewFake(base+"/xet-cas", time.Hour), Viewer: datasetstest.NewViewer(), Devices: authtest.NewDeviceFlow(authtest.NewTokens(authn), base),
		Log: slog.New(slog.DiscardHandler)}
}

// secondInstance is a Hangar built from W2, W3 and W4 that plays the
// Upstream: the Hub API and git smart HTTP on one origin.
type secondInstance struct {
	URL   string
	Real  *repotest.Real
	Owner model.Principal

	mu     sync.Mutex
	ranges []string
}

func newSecondInstance(t *testing.T) *secondInstance {
	t.Helper()
	d := dbtest.SQLite(t)
	blobs, err := fs.New(t.TempDir())
	require.NoError(t, err)
	real := repotest.NewReal(t, d, repotest.RealOptions{Blobs: blobs})
	ln, base := listen(t)
	authn := authtest.NewAuthenticator()
	authz := authtest.NewAuthz()
	authz.AnonymousRead = true
	orgs := org.NewDB(d, audittest.NewFake())
	hub, err := hfapi.New(hubDeps(base, real, orgs, authz, authn))
	require.NoError(t, err)
	signer, err := httpx.NewSigner(instanceKey)
	require.NoError(t, err)
	gs, err := gitserver.New(gitserver.Config{PublicURL: base, TempDir: t.TempDir()}, gitserver.Deps{Repos: real.Store, LFS: real.LFS, Authz: authz, Signer: signer})
	require.NoError(t, err)
	s := &secondInstance{URL: base, Real: real, Owner: real.AddUser(t, "lab")}
	h := httpx.Dispatch(httpx.Protocols{Hub: hub, Git: gs.Git(), LFS: gs.LFS()}, base)
	h = httpx.RequestIDs(classify(httpx.Authenticate(authn, nil)(h)))
	record := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rg := r.Header.Get("Range"); rg != "" && strings.Contains(r.URL.Path, "/resolve/") {
			s.mu.Lock()
			s.ranges = append(s.ranges, rg)
			s.mu.Unlock()
		}
		h.ServeHTTP(w, r)
	})
	srv := &httptest.Server{Listener: ln, Config: &http.Server{Handler: record, ReadHeaderTimeout: 30 * time.Second}}
	srv.Start()
	t.Cleanup(func() { srv.Close(); _ = gs.Close() })
	return s
}

func (s *secondInstance) Ranges() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.ranges...)
}

// seed commits files to a Repo of the second Instance, creating it.
func (s *secondInstance) seed(t *testing.T, id model.RepoID, files map[string][]byte) model.CommitSHA {
	t.Helper()
	ctx := context.Background()
	if _, err := s.Real.Get(ctx, id); errors.Is(err, repo.ErrNotFound) {
		_, err := s.Real.Create(ctx, repo.CreateParams{ID: id, Actor: s.Owner, Visibility: model.VisibilityPublic})
		require.NoError(t, err)
	}
	var ops []repo.Operation
	for p, c := range files {
		if strings.HasSuffix(p, ".bin") || strings.HasSuffix(p, ".gguf") || strings.HasSuffix(p, ".safetensors") {
			sum := sha256.Sum256(c)
			obj, err := s.Real.LFS.Ingest(ctx, hex.EncodeToString(sum[:]), int64(len(c)), bytes.NewReader(c))
			require.NoError(t, err)
			ops = append(ops, repo.Operation{Kind: repo.OpAddLFS, Path: p, OID: obj.OID, Size: obj.Size})
			continue
		}
		ops = append(ops, repo.Operation{Kind: repo.OpAdd, Path: p, Content: bytes.NewReader(c)})
	}
	res, err := s.Real.Commit(ctx, repo.CommitParams{Repo: id, Summary: "add files", Operations: ops, Actor: s.Owner})
	require.NoError(t, err)
	return res.SHA
}

// countingLimiter wraps a limiter and records what it granted, and when.
type countingLimiter struct {
	inner       jobs.Limiter
	spent       *atomic.Int64
	mu          *sync.Mutex
	first, last *time.Time
}

func newCounting(inner jobs.Limiter, shared *countingLimiter) *countingLimiter {
	if shared != nil {
		return &countingLimiter{inner: inner, spent: shared.spent, mu: shared.mu, first: shared.first, last: shared.last}
	}
	return &countingLimiter{inner: inner, spent: &atomic.Int64{}, mu: &sync.Mutex{}, first: &time.Time{}, last: &time.Time{}}
}

func (c *countingLimiter) WaitN(ctx context.Context, name string, n int64) error {
	if err := c.inner.WaitN(ctx, name, n); err != nil {
		return err
	}
	c.spent.Add(n)
	c.mu.Lock()
	if c.first.IsZero() {
		*c.first = time.Now()
	}
	*c.last = time.Now()
	c.mu.Unlock()
	return nil
}

// localInstance is the Instance under test: the real Upstream service on
// the real Repo service, with the database queue and a worker.
type localInstance struct {
	t        *testing.T
	ctx      context.Context
	d        *db.DB
	blobs    *fs.Store
	real     *repotest.Real
	orgs     *org.DBService
	reg      *jobs.Registry
	queue    *jobs.DBQueue
	registry *upstream.Registry
	quota    *gc.Accounting
	svc      *upstream.Service
	authz    *authtest.Authz
	user     model.Principal
	worker   *jobs.Worker
	limiter  *jobs.DBLimiter
	settings atomic.Pointer[upstream.Settings]
	stop     context.CancelFunc
	done     chan struct{}
}

type localOptions struct {
	owner    string
	rate     int64
	partSize int64
	blobDir  string
	// shared counts bytes across workers.
	shared *countingLimiter
	// noWorker leaves the worker stopped.
	noWorker bool
	// existing reuses another Instance's Repo rows and Blob store (a
	// second worker of one Instance).
	existing *localInstance
	// beforeQuotaCheck runs before each quota check a Pull makes.
	beforeQuotaCheck func(ctx context.Context)
	// logger receives the worker's log; nil discards it.
	logger *slog.Logger
}

func newLocal(t *testing.T, d *db.DB, upstreamURL string, o localOptions) *localInstance {
	t.Helper()
	l := &localInstance{t: t, ctx: context.Background(), d: d, reg: jobs.NewRegistry(), authz: authtest.NewAuthz()}
	l.queue = jobs.NewQueue(d, l.reg, jobs.QueueOptions{})
	if o.blobDir == "" {
		o.blobDir = t.TempDir()
	}
	blobs, err := fs.New(o.blobDir)
	require.NoError(t, err)
	l.blobs = blobs
	l.real = repotest.NewReal(t, d, repotest.RealOptions{Blobs: blobs, Tweak: func(ro *repo.Options) { ro.Jobs = l.queue }})
	l.orgs = org.NewDB(d, audittest.NewFake())
	l.quota = gc.NewAccounting(d)
	sealer, err := secrets.New(instanceKey)
	require.NoError(t, err)
	l.registry = upstream.NewRegistry(d, sealer, nil)
	require.NoError(t, l.registry.SyncConfig(l.ctx, map[string]config.UpstreamConfig{upstream.DefaultUpstream: {BaseURL: upstreamURL, Enabled: true}}))
	if o.existing != nil {
		l.user = o.existing.user
	} else {
		l.user = l.real.AddUser(t, "alice")
		l.user.IsAdmin = true
	}
	owner := o.owner
	if owner == "" {
		owner = "worker-" + uuid.NewString()[:8]
	}
	rate := o.rate
	l.limiter = jobs.NewLimiter(d, jobs.LimiterOptions{Owner: owner, Budget: jobs.StaticBudget(map[string]int64{upstream.BandwidthBudget: rate})})
	var lim jobs.Limiter = l.limiter
	if o.shared != nil {
		lim = newCounting(l.limiter, o.shared)
	}
	l.settings.Store(&upstream.Settings{AllowOverrideWindows: true, PullThroughAllow: []string{"*"}, PullThroughMinRole: "user"})
	signer, err := httpx.NewSigner(instanceKey)
	require.NoError(t, err)
	l.svc, err = upstream.New(upstream.Options{DB: d, Registry: l.registry, Client: upstream.NewHTTPClient(upstream.HTTPOptions{Version: "compat"}),
		Repos: l.real.Store, LFS: l.real.LFS, Orgs: l.orgs, Queue: l.queue, Limiter: lim, Authz: l.authz, Layout: datasetstest.NewLayout(),
		Webhooks: &webhooktest.Fake{}, MirrorState: l.real.Store, Signer: signer, PublicURL: "http://unused.invalid",
		Dependencies: config.DependencyConfig{AutoPullCode: true, MaxDepth: 5},
		Settings:     func(context.Context) (upstream.Settings, error) { return *l.settings.Load(), nil },
		ParseWindows: func(specs []string) (upstream.Windows, error) { return jobs.ParseWindows(specs, time.UTC) },
		Snooze:       jobs.Snooze, PartSize: o.partSize, PollInterval: 200 * time.Millisecond, Quota: pullQuota{Accounting: l.quota, before: o.beforeQuotaCheck}})
	require.NoError(t, err)
	require.NoError(t, l.svc.RegisterJobs(l.reg))
	l.worker = jobs.NewWorker(d, l.reg, jobs.WorkerOptions{Owner: owner, Concurrency: 4, Lease: 3 * time.Second, PollInterval: 50 * time.Millisecond,
		InstanceLimits: map[jobs.Kind]int{jobs.KindPullFile: 4}, Logger: o.logger})
	if !o.noWorker {
		l.start()
	}
	t.Cleanup(l.halt)
	return l
}

func (l *localInstance) start() {
	ctx, cancel := context.WithCancel(context.Background())
	l.stop, l.done = cancel, make(chan struct{})
	go func() { _ = l.worker.Run(ctx); close(l.done) }()
}

func (l *localInstance) halt() {
	if l.stop != nil {
		l.stop()
		<-l.done
		l.stop = nil
	}
	_ = l.limiter.Close(context.Background())
}

// wait polls a Job until it ends.
func (l *localInstance) wait(id jobs.JobID, timeout time.Duration) *jobs.Job {
	l.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		j, err := l.queue.Get(l.ctx, id)
		require.NoError(l.t, err)
		if !j.Status.Active() {
			return j
		}
		require.True(l.t, time.Now().Before(deadline), "Job %s %s did not finish: %s %+v", j.Kind, id, j.Status, j.Progress)
		time.Sleep(100 * time.Millisecond)
	}
}

func (l *localInstance) read(id model.RepoID, path string) []byte {
	l.t.Helper()
	head, err := l.real.Resolve(l.ctx, id, "main")
	require.NoError(l.t, err)
	rc, _, err := l.real.OpenFile(l.ctx, id, head, path, nil)
	require.NoError(l.t, err, path)
	defer rc.Close()
	b, err := io.ReadAll(rc)
	require.NoError(l.t, err)
	return b
}

// TestRealPullAndTrackFromASecondInstance pulls from W3 and W4 into W2
// through W1's queue, then follows a new Commit and a force-push.
func TestRealPullAndTrackFromASecondInstance(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		up := newSecondInstance(t)
		l := newLocal(t, d, up.URL, localOptions{partSize: 1 << 20})
		id := model.RepoID{Type: model.RepoTypeModel, Namespace: "lab", Name: "tracked"}
		w1 := bytes.Repeat([]byte("v1 weights "), 300_000) // 3.3 MB, four parts
		first := up.seed(t, id, map[string][]byte{"config.json": []byte(`{"model_type":"bert"}`), "model.safetensors": w1, "README.md": []byte("# tracked")})

		jid, local, err := l.svc.StartPull(l.ctx, upstream.PullRequest{Repo: id, Actor: l.user, Track: true})
		require.NoError(t, err)
		require.Equal(t, id, local)
		j := l.wait(jid, 2*time.Minute)
		require.Equal(t, jobs.StatusSucceeded, j.Status, j.Error)
		require.Equal(t, int64(len(w1)), j.Progress.BytesDone)
		head, err := l.real.Resolve(l.ctx, id, "main")
		require.NoError(t, err)
		require.Equal(t, first, head, "the Mirror keeps the upstream Commit SHA")
		require.Equal(t, w1, l.read(id, "model.safetensors"))
		r, err := l.real.Get(l.ctx, id)
		require.NoError(t, err)
		require.True(t, r.Mirror, "W2 marks the Repo as a Mirror")
		ns, err := l.orgs.Get(l.ctx, "lab")
		require.NoError(t, err)
		require.True(t, ns.ReservedMirror)
		// A Mirror refuses git pushes (W2's ApplyPack).
		err = l.real.ApplyPack(l.ctx, repo.ApplyPackParams{Repo: id, Source: repo.SourcePush, Actor: l.user,
			Updates: []repo.RefUpdate{{Name: "refs/heads/x", New: first}}})
		require.ErrorIs(t, err, repo.ErrReadOnly)
		scheds, err := l.queue.Schedules(l.ctx)
		require.NoError(t, err)
		require.Len(t, scheds, 1)

		// A new upstream Commit.
		w2 := bytes.Repeat([]byte("v2 weights "), 200_000)
		second := up.seed(t, id, map[string][]byte{"model.safetensors": w2})
		sync, err := l.queue.Enqueue(l.ctx, jobs.NewJob{Kind: jobs.KindMirrorSync, Payload: map[string]string{"repo_uuid": r.UUID.String()}})
		require.NoError(t, err)
		require.Equal(t, jobs.StatusSucceeded, l.wait(sync, time.Minute).Status)
		syncPull := l.latestPull()
		require.Equal(t, jobs.StatusSucceeded, l.wait(syncPull, 2*time.Minute).Status)
		head, _ = l.real.Resolve(l.ctx, id, "main")
		require.Equal(t, second, head)
		require.Equal(t, w2, l.read(id, "model.safetensors"))

		// An upstream force-push back to the first Commit.
		require.NoError(t, up.Real.UpdateRefs(l.ctx, id, []repo.RefUpdate{{Name: "refs/heads/main", Old: second, New: first}}))
		sync, err = l.queue.Enqueue(l.ctx, jobs.NewJob{Kind: jobs.KindMirrorSync, Payload: map[string]string{"repo_uuid": r.UUID.String()}})
		require.NoError(t, err)
		l.wait(sync, time.Minute)
		require.Equal(t, jobs.StatusSucceeded, l.wait(l.latestPull(), 2*time.Minute).Status)
		head, _ = l.real.Resolve(l.ctx, id, "main")
		require.Equal(t, first, head)
		refs, err := l.real.Refs(l.ctx, id)
		require.NoError(t, err)
		kept := false
		for _, o := range refs.Other {
			kept = kept || (strings.HasPrefix(o.Name, "refs/hangar/previous/") && o.Target == second)
		}
		require.True(t, kept, "the replaced tip stays reachable")
		view, err := l.real.OpenView(l.ctx, id)
		require.NoError(t, err)
		defer view.Close()
		gitIn(t, view.Dir, "fsck", "--no-dangling")
		require.Equal(t, string(second), gitIn(t, view.Dir, "rev-parse", mustPrevious(t, refs)))
	})
}

func mustPrevious(t *testing.T, refs *repo.Refs) string {
	for _, o := range refs.Other {
		if strings.HasPrefix(o.Name, "refs/hangar/previous/") {
			return o.Name
		}
	}
	t.Fatal("no previous ref")
	return ""
}

func (l *localInstance) latestPull() jobs.JobID {
	l.t.Helper()
	list, _, err := l.queue.List(l.ctx, jobs.Filter{Kinds: []jobs.Kind{jobs.KindPull}, Limit: 1})
	require.NoError(l.t, err)
	require.NotEmpty(l.t, list)
	return list[0].ID
}

// TestRealPullWindowsAndPullNow checks that W1's worker snoozes transfers
// outside a pull window without using an attempt, and that Pull now
// ignores the window.
func TestRealPullWindowsAndPullNow(t *testing.T) {
	up := newSecondInstance(t)
	l := newLocal(t, dbtest.SQLite(t), up.URL, localOptions{})
	now := time.Now().UTC()
	window := fmt.Sprintf("%02d:00-%02d:00", (now.Hour()+3)%24, (now.Hour()+4)%24)
	l.settings.Store(&upstream.Settings{Windows: []string{window}, AllowOverrideWindows: true})
	waiting := model.RepoID{Type: model.RepoTypeModel, Namespace: "lab", Name: "later"}
	up.seed(t, waiting, map[string][]byte{"w.bin": []byte("later weights")})
	jid, _, err := l.svc.StartPull(l.ctx, upstream.PullRequest{Repo: waiting, Actor: l.user})
	require.NoError(t, err)
	var file *jobs.Job
	require.Eventually(t, func() bool {
		list, _, err := l.queue.List(l.ctx, jobs.Filter{Kinds: []jobs.Kind{jobs.KindPullFile}})
		if err != nil || len(list) == 0 || list[0].Progress.Message == "" {
			return false
		}
		file = list[0]
		return file.Status == jobs.StatusPending
	}, time.Minute, 100*time.Millisecond)
	require.Equal(t, "Waiting for pull window ("+window+")", file.Progress.Message)
	require.Equal(t, (now.Hour()+3)%24, file.RunAfter.UTC().Hour(), "the transfer runs when the window opens")
	require.Zero(t, file.Attempts, "waiting for a window uses no attempt")
	j, err := l.queue.Get(l.ctx, jid)
	require.NoError(t, err)
	require.True(t, j.Status.Active(), "the Pull waits too")

	now2 := model.RepoID{Type: model.RepoTypeModel, Namespace: "lab", Name: "now"}
	up.seed(t, now2, map[string][]byte{"w.bin": []byte("pull now weights")})
	jid2, _, err := l.svc.StartPull(l.ctx, upstream.PullRequest{Repo: now2, Actor: l.user, Now: true})
	require.NoError(t, err)
	require.Equal(t, jobs.StatusSucceeded, l.wait(jid2, time.Minute).Status)
	require.Equal(t, []byte("pull now weights"), l.read(now2, "w.bin"))
}

// TestRealBandwidthCapAcrossTwoWorkers runs two workers of one Instance
// with W1's shared limiter and checks the cap holds within 10 % over the
// window.
func TestRealBandwidthCapAcrossTwoWorkers(t *testing.T) {
	seconds := 60
	if v := os.Getenv("HANGAR_UPSTREAM_CAP_SECONDS"); v != "" {
		_, err := fmt.Sscanf(v, "%d", &seconds)
		require.NoError(t, err)
	}
	const rate = 256 << 10
	up := newSecondInstance(t)
	d := dbtest.SQLite(t)
	shared := newCounting(nil, nil)
	blobDir := t.TempDir()
	a := newLocal(t, d, up.URL, localOptions{owner: "worker-a", rate: rate, shared: shared, blobDir: blobDir, partSize: 1 << 20})
	b := newLocal(t, d, up.URL, localOptions{owner: "worker-b", rate: rate, shared: shared, blobDir: blobDir, partSize: 1 << 20, existing: a})
	size := rate * seconds / 2
	var ids []jobs.JobID
	for i, l := range []*localInstance{a, b} {
		id := model.RepoID{Type: model.RepoTypeModel, Namespace: "lab", Name: fmt.Sprintf("repo-%d", i)}
		up.seed(t, id, map[string][]byte{"w.bin": bytes.Repeat([]byte{byte('a' + i)}, size)})
		jid, _, err := l.svc.StartPull(l.ctx, upstream.PullRequest{Repo: id, Actor: a.user})
		require.NoError(t, err)
		ids = append(ids, jid)
	}
	for _, jid := range ids {
		require.Equal(t, jobs.StatusSucceeded, a.wait(jid, time.Duration(3*seconds)*time.Second).Status)
	}
	elapsed := shared.last.Sub(*shared.first).Seconds()
	got := float64(shared.spent.Load())
	want := rate * elapsed
	t.Logf("two workers moved %.0f bytes in %.1f s under a %d B/s cap: %.1f %% of the cap", got, elapsed, rate, 100*got/want)
	require.GreaterOrEqual(t, elapsed, 0.9*float64(seconds))
	require.Less(t, math.Abs(got-want)/want, 0.10)
	// Both workers transferred.
	files, _, err := a.queue.List(a.ctx, jobs.Filter{Kinds: []jobs.Kind{jobs.KindPullFile}})
	require.NoError(t, err)
	require.Len(t, files, 2)
}

// The worker process for TestRealResumeAfterKillingTheWorkerProcess. It
// does nothing unless the test started it.
func TestRealWorkerProcess(t *testing.T) {
	dsn := os.Getenv("HANGAR_W6_WORKER_DSN")
	if dsn == "" {
		return
	}
	d, err := db.Open(context.Background(), db.Options{Driver: db.Postgres, DSN: dsn})
	require.NoError(t, err)
	l := newLocal(t, d, os.Getenv("HANGAR_W6_WORKER_UPSTREAM"), localOptions{owner: "doomed", rate: 1 << 20, partSize: 1 << 20,
		blobDir: os.Getenv("HANGAR_W6_WORKER_BLOBS"), existing: &localInstance{user: model.System()}})
	_ = l
	fmt.Println("worker ready")
	select {} // until killed
}

// TestRealResumeAfterKillingTheWorkerProcess kills a worker process with
// SIGKILL in the middle of a multipart transfer. W1's lease expiry hands
// the Job to a second worker, which resumes from the uploaded parts.
func TestRealResumeAfterKillingTheWorkerProcess(t *testing.T) {
	up := newSecondInstance(t)
	dsn := dbtest.PostgresDSN(t)
	d, err := db.Open(context.Background(), db.Options{Driver: db.Postgres, DSN: dsn})
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	require.NoError(t, d.Migrate(context.Background()))
	blobDir := t.TempDir()
	l := newLocal(t, d, up.URL, localOptions{owner: "survivor", partSize: 1 << 20, blobDir: blobDir, noWorker: true})
	id := model.RepoID{Type: model.RepoTypeModel, Namespace: "lab", Name: "killed"}
	content := bytes.Repeat([]byte("kill me mid-file "), 6<<20/17)
	up.seed(t, id, map[string][]byte{"w.bin": content})
	jid, _, err := l.svc.StartPull(l.ctx, upstream.PullRequest{Repo: id, Actor: l.user})
	require.NoError(t, err)

	cmd := exec.Command(os.Args[0], "-test.run=^TestRealWorkerProcess$", "-test.v")
	cmd.Env = append(os.Environ(), "HANGAR_W6_WORKER_DSN="+dsn, "HANGAR_W6_WORKER_UPSTREAM="+up.URL, "HANGAR_W6_WORKER_BLOBS="+blobDir)
	out := &bytes.Buffer{}
	cmd.Stdout, cmd.Stderr = out, out
	require.NoError(t, cmd.Start())
	var file *jobs.Job
	require.Eventually(t, func() bool {
		list, _, err := l.queue.List(l.ctx, jobs.Filter{Kinds: []jobs.Kind{jobs.KindPullFile}})
		if err != nil || len(list) == 0 {
			return false
		}
		file = list[0]
		return file.Status == jobs.StatusRunning && file.Progress.BytesDone >= 5<<19 // 2.5 MiB at 1 MiB/s
	}, 2*time.Minute, 50*time.Millisecond, "the doomed worker started the transfer: %s", out)
	require.NoError(t, cmd.Process.Signal(syscall.SIGKILL))
	_ = cmd.Wait()
	var res map[string]string
	got, err := l.queue.Get(l.ctx, file.ID)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(got.Result, &res))
	require.NotEmpty(t, res["upload_id"], "the killed worker recorded its upload")
	require.Equal(t, jobs.StatusRunning, got.Status, "the Job is still leased to the dead worker")

	// The surviving worker recovers the expired lease and resumes.
	l.start()
	j := l.wait(jid, 2*time.Minute)
	require.Equal(t, jobs.StatusSucceeded, j.Status, j.Error)
	require.Equal(t, content, l.read(id, "w.bin"))
	var resumed []string
	for _, r := range up.Ranges() {
		if r != "bytes=0-" {
			resumed = append(resumed, r)
		}
	}
	require.Len(t, resumed, 1, "one ranged request: %v", up.Ranges())
	var start int
	_, err = fmt.Sscanf(resumed[0], "bytes=%d-", &start)
	require.NoError(t, err)
	require.GreaterOrEqual(t, start, 2<<20, "at least two parts were kept")
	require.Zero(t, start%(1<<20), "the resume starts at a part boundary")
	t.Logf("killed after %d bytes; resumed with %s", file.Progress.BytesDone, resumed[0])
}

// localHub serves the Instance under test: W4's Hub API with the
// Pull-through adapter the integration lead must write, W3's git server
// with Pull-through, the signed Pull-through handler, and the native API
// with W2's Repo and upload handlers and W6's.
type localHub struct {
	URL   string
	Token string
}

// pullThroughAdapter maps upstream.ErrPullPending to hfapi.ErrPullPending.
type pullThroughAdapter struct{ *upstream.PullThrough }

func (a pullThroughAdapter) Repo(ctx context.Context, p model.Principal, id model.RepoID) (*repo.Repo, error) {
	r, err := a.PullThrough.Repo(ctx, p, id)
	if errors.Is(err, upstream.ErrPullPending) {
		return nil, hfapi.ErrPullPending
	}
	return r, err
}

type nativeServer struct {
	*api.ReposAPI
	*api.UploadsAPI
	*api.Upstreams
	nativeUnimplemented
}

type nativeUnimplemented struct{ gen.Unimplemented }

func newLocalHub(t *testing.T, l *localInstance, publicURL string, ln net.Listener) *localHub {
	t.Helper()
	authn := authtest.NewAuthenticator()
	tok := "hgr_alice_compat"
	authn.Add(tok, l.user)
	l.authz.AnonymousRead = true
	hubDeps := hubDeps(publicURL, l.real, l.orgs, l.authz, authn)
	hubDeps.PullThrough = pullThroughAdapter{l.svc.PullThrough()}
	hub, err := hfapi.New(hubDeps)
	require.NoError(t, err)
	signer, err := httpx.NewSigner(instanceKey)
	require.NoError(t, err)
	gs, err := gitserver.New(gitserver.Config{PublicURL: publicURL, TempDir: t.TempDir()}, gitserver.Deps{Repos: l.real.Store, LFS: l.real.LFS,
		Authz: l.authz, Signer: signer, PullThrough: l.svc.PullThrough()})
	require.NoError(t, err)
	t.Cleanup(func() { _ = gs.Close() })
	rd := api.RepoDeps{Repos: l.real.Store, LFS: l.real.LFS, Authz: l.authz, Orgs: l.orgs, Jobs: l.queue, Collections: collectionstest.NewFake(),
		NamespaceSearch: namespaceSearch{}, Community: communitytest.NewFake(), Index: indextest.NewFake(), PublicURL: publicURL}
	repos, err := api.NewReposAPI(rd)
	require.NoError(t, err)
	uploads, err := api.NewUploadsAPI(rd)
	require.NoError(t, err)
	native := gen.HandlerWithOptions(nativeServer{ReposAPI: repos, UploadsAPI: uploads,
		Upstreams: &api.Upstreams{Service: l.svc, Repos: l.real.Store, Authz: l.authz, Queue: l.queue}}, gen.ChiServerOptions{BaseURL: "/api/hangar/v1"})
	h := httpx.Dispatch(httpx.Protocols{Hub: hub, Git: gs.Git(), LFS: gs.LFS(), Native: native}, publicURL)
	h = httpx.RequestIDs(classify(httpx.Authenticate(authn, nil)(h)))
	mux := http.NewServeMux()
	mux.Handle(upstream.PullThroughPath, l.svc.PullThrough().Handler())
	mux.Handle("/", h)
	srv := &httptest.Server{Listener: ln, Config: &http.Server{Handler: mux, ReadHeaderTimeout: 30 * time.Second}}
	srv.Start()
	t.Cleanup(srv.Close)
	return &localHub{URL: publicURL, Token: tok}
}

type namespaceSearch struct{}

func (namespaceSearch) SearchNamespaces(context.Context, string, int) ([]org.Namespace, error) {
	return nil, nil
}

// newWithSigner rebuilds the Instance's Service with the public URL its
// Pull-through links need.
func newLocalWithHub(t *testing.T, d *db.DB, upstreamURL string) (*localInstance, *localHub) {
	t.Helper()
	ln, base := listen(t)
	l := newLocal(t, d, upstreamURL, localOptions{partSize: 1 << 20})
	signer, err := httpx.NewSigner(instanceKey)
	require.NoError(t, err)
	l.halt()
	l.reg = jobs.NewRegistry()
	l.queue = jobs.NewQueue(d, l.reg, jobs.QueueOptions{})
	l.svc, err = upstream.New(upstream.Options{DB: d, Registry: l.registry, Client: upstream.NewHTTPClient(upstream.HTTPOptions{Version: "compat"}),
		Repos: l.real.Store, LFS: l.real.LFS, Orgs: l.orgs, Queue: l.queue, Limiter: l.limiter, Authz: l.authz, Layout: datasetstest.NewLayout(),
		Webhooks: &webhooktest.Fake{}, MirrorState: l.real.Store, Signer: signer, PublicURL: base,
		Settings:     func(context.Context) (upstream.Settings, error) { return *l.settings.Load(), nil },
		ParseWindows: func(specs []string) (upstream.Windows, error) { return jobs.ParseWindows(specs, time.UTC) },
		Snooze:       jobs.Snooze, PartSize: 1 << 20, PollInterval: 200 * time.Millisecond})
	require.NoError(t, err)
	require.NoError(t, l.svc.RegisterJobs(l.reg))
	l.limiter = jobs.NewLimiter(d, jobs.LimiterOptions{Owner: "hub-worker", Budget: jobs.StaticBudget(nil)})
	l.worker = jobs.NewWorker(d, l.reg, jobs.WorkerOptions{Owner: "hub-worker", Concurrency: 4, Lease: 3 * time.Second, PollInterval: 50 * time.Millisecond})
	l.start()
	return l, newLocalHub(t, l, base, ln)
}

// TestRealPullThroughWithHFHubDownloadAndGitLFS asks the Instance's Hub
// API for a Repo it does not have with huggingface_hub, and clones it
// with git and git-lfs, with Pull-through on.
func TestRealPullThroughWithHFHubDownloadAndGitLFS(t *testing.T) {
	if _, err := exec.LookPath("uv"); err != nil {
		t.Fatal("uv is required for the huggingface_hub client")
	}
	up := newSecondInstance(t)
	l, hub := newLocalWithHub(t, dbtest.SQLite(t), up.URL)
	l.settings.Store(&upstream.Settings{PullThroughEnabled: true, PullThroughAllow: []string{"lab/*"}, PullThroughMinRole: "user"})
	id := model.RepoID{Type: model.RepoTypeModel, Namespace: "lab", Name: "through"}
	weights := bytes.Repeat([]byte("pulled through "), 200_000)
	gitWeights := bytes.Repeat([]byte("git lfs pulled through "), 50_000)
	up.seed(t, id, map[string][]byte{"config.json": []byte(`{"model_type":"bert"}`), "model.safetensors": weights, "extra.bin": gitWeights})

	home := t.TempDir()
	script := `import hashlib, sys
from huggingface_hub import hf_hub_download
p = hf_hub_download("lab/through", "model.safetensors")
print(hashlib.sha256(open(p, "rb").read()).hexdigest())`
	cmd := exec.Command("uv", "run", "--project", filepath.Join(repoRoot(t), "compat"), "python", "-c", script)
	cmd.Env = append(os.Environ(), "HF_ENDPOINT="+hub.URL, "HF_TOKEN="+hub.Token, "HF_HOME="+home, "HF_HUB_DISABLE_XET=1", "HF_HUB_DISABLE_TELEMETRY=1")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", out)
	sum := sha256.Sum256(weights)
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	require.Equal(t, hex.EncodeToString(sum[:]), lines[len(lines)-1], "hf_hub_download got the upstream bytes through Pull-through")
	r, err := l.real.Get(l.ctx, id)
	require.NoError(t, err)
	require.True(t, r.Mirror)

	// git clone with git-lfs: extra.bin is not in the arsenal, and the
	// git-lfs batch answer points at the signed Pull-through link.
	dir := t.TempDir()
	u := strings.Replace(hub.URL, "http://", "http://user:"+hub.Token+"@", 1) + "/lab/through"
	gitEnv := append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0", "HOME="+t.TempDir())
	install := exec.Command("git", "lfs", "install", "--skip-repo")
	install.Env = gitEnv
	out, err = install.CombinedOutput()
	require.NoError(t, err, "%s", out)
	git := exec.Command("git", "clone", "-q", u, dir)
	git.Env = gitEnv
	out, err = git.CombinedOutput()
	require.NoError(t, err, "%s", out)
	got, err := os.ReadFile(filepath.Join(dir, "extra.bin"))
	require.NoError(t, err)
	gotSum, wantSum := sha256.Sum256(got), sha256.Sum256(gitWeights)
	require.Equal(t, hex.EncodeToString(wantSum[:]), hex.EncodeToString(gotSum[:]), "git-lfs fetched the object through Pull-through")
}

func repoRoot(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	require.NoError(t, err)
	return strings.TrimSpace(string(out))
}

// TestRealImportThroughTheNativeAPI imports a Hugging Face cache and an
// LM Studio folder through W2's upload endpoints and W6's Pull endpoints,
// with the HTTP Target `hangar import` uses.
func TestRealImportThroughTheNativeAPI(t *testing.T) {
	up := newSecondInstance(t)
	l, hub := newLocalWithHub(t, dbtest.SQLite(t), up.URL)
	id := model.RepoID{Type: model.RepoTypeModel, Namespace: "lab", Name: "cached"}
	w := bytes.Repeat([]byte("cached weights "), 100_000)
	q4, q8 := []byte("q4 quant bytes"), []byte("q8 quant bytes")
	commit := up.seed(t, id, map[string][]byte{"config.json": []byte(`{}`), "model.safetensors": w, "other.bin": []byte("not cached")})
	gguf := model.RepoID{Type: model.RepoTypeModel, Namespace: "lab", Name: "Tiny-GGUF"}
	// GGUF Repos track *.gguf with LFS, as on huggingface.co.
	attrs := append(repo.DefaultGitAttributes(model.RepoTypeModel), []byte("*.gguf filter=lfs diff=lfs merge=lfs -text\n")...)
	up.seed(t, gguf, map[string][]byte{".gitattributes": attrs, "Tiny-Q4_K_M.gguf": q4, "Tiny-Q8_0.gguf": q8})

	root := t.TempDir()
	// A Hugging Face cache as huggingface_hub lays it out.
	cdir := filepath.Join(root, "hub", "models--lab--cached")
	writeFile(t, filepath.Join(cdir, "refs", "main"), []byte(commit))
	sum := sha256.Sum256(w)
	writeFile(t, filepath.Join(cdir, "blobs", hex.EncodeToString(sum[:])), w)
	writeFile(t, filepath.Join(cdir, "snapshots", string(commit), "config.json"), []byte(`{}`))
	snap := filepath.Join(cdir, "snapshots", string(commit), "model.safetensors")
	require.NoError(t, os.Symlink(filepath.Join("..", "..", "blobs", hex.EncodeToString(sum[:])), snap))
	writeFile(t, filepath.Join(root, "lms", "lab", "Tiny-GGUF", "Tiny-Q4_K_M.gguf"), q4)

	target := &importer.HTTPTarget{BaseURL: hub.URL, Token: hub.Token, PollInterval: 100 * time.Millisecond, Version: "compat"}
	im := importer.New(importer.Options{Target: target})
	rep, err := im.FromHFCache(l.ctx, filepath.Join(root, "hub"))
	require.NoError(t, err)
	require.Len(t, rep.Results, 1)
	require.NoError(t, rep.Results[0].Err)
	require.Equal(t, 1, rep.Results[0].Files)
	require.Equal(t, w, l.read(id, "model.safetensors"))
	head, _ := l.real.Resolve(l.ctx, id, "main")
	require.Equal(t, commit, head)
	e, err := l.real.Entry(l.ctx, id, head, "other.bin")
	require.NoError(t, err)
	require.False(t, e.Present, "files not in the cache stay not in the arsenal")
	require.Empty(t, upstreamResolves(up, "model.safetensors"), "the cached file was not downloaded")

	rep, err = im.FromLMStudio(l.ctx, filepath.Join(root, "lms"))
	require.NoError(t, err)
	require.Len(t, rep.Results, 1)
	require.NoError(t, rep.Results[0].Err)
	require.Equal(t, q4, l.read(gguf, "Tiny-Q4_K_M.gguf"))
	ghead, _ := l.real.Resolve(l.ctx, gguf, "main")
	e, err = l.real.Entry(l.ctx, gguf, ghead, "Tiny-Q8_0.gguf")
	require.NoError(t, err)
	require.False(t, e.Present)

	dirRepo := model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "from-dir"}
	writeFile(t, filepath.Join(root, "dir", "README.md"), []byte("# from a folder"))
	writeFile(t, filepath.Join(root, "dir", "weights.bin"), w)
	res := im.FromDir(l.ctx, filepath.Join(root, "dir"), dirRepo, "")
	require.NoError(t, res.Err)
	require.Equal(t, []byte("# from a folder"), l.read(dirRepo, "README.md"))
	require.Equal(t, w, l.read(dirRepo, "weights.bin"))
}

// upstreamResolves is unused by the server; the second Instance serves
// files directly, so its Range log has no entry for a file unless it was
// downloaded with a range. The import never downloads, so it is empty.
func upstreamResolves(up *secondInstance, _ string) []string { return up.Ranges() }

func writeFile(t *testing.T, path string, b []byte) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, b, 0o644))
}

// pullQuota is the adapter the integration lead wires between W1's
// Namespace quotas and Pulls: W1's refusals become upstream.ErrQuotaExceeded
// with W1's message kept.
type pullQuota struct {
	*gc.Accounting
	// before is a test hook that runs before each check.
	before func(ctx context.Context)
}

func (q pullQuota) CheckQuota(ctx context.Context, ns uuid.UUID, adding int64) error {
	if q.before != nil {
		q.before(ctx)
	}
	err := q.Accounting.CheckQuota(ctx, ns, adding)
	if errors.Is(err, gc.ErrQuotaExceeded) {
		return upstream.QuotaExceeded(err)
	}
	return err
}

// TestRealPullRespectsNamespaceQuota checks Pulls against W1's quotas on
// W2's Repo service: a pack that does not fit is not applied, a selection
// that does not fit is not transferred, and both Pulls end with W1's
// message and reason quota_exceeded until an admin raises the quota.
func TestRealPullRespectsNamespaceQuota(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		up := newSecondInstance(t)
		l := newLocal(t, d, up.URL, localOptions{partSize: 1 << 20})
		// Upstream Repos in their own Namespaces, so each local Mirror
		// Namespace has its own quota.
		upstreamRepo := func(id model.RepoID) {
			t.Helper()
			owner := up.Real.AddUser(t, id.Namespace)
			_, err := up.Real.Create(l.ctx, repo.CreateParams{ID: id, Actor: owner, Visibility: model.VisibilityPublic})
			require.NoError(t, err)
		}
		requireRefused := func(jid jobs.JobID) {
			t.Helper()
			j := l.wait(jid, time.Minute)
			require.Equal(t, jobs.StatusFailed, j.Status)
			var st map[string]any
			require.NoError(t, json.Unmarshal(j.Result, &st))
			require.Equal(t, "quota_exceeded", st["reason"])
			require.Contains(t, j.Error, "storage quota. Delete files or ask an admin to raise the quota.")
			require.Equal(t, "Storage quota exceeded", j.Progress.Message)
		}

		// The history does not fit.
		hist := model.RepoID{Type: model.RepoTypeModel, Namespace: "tight", Name: "history"}
		upstreamRepo(hist)
		up.seed(t, hist, map[string][]byte{"config.json": []byte(`{"model_type":"bert"}`), "model.safetensors": []byte("weights")})
		tight, err := l.orgs.EnsureMirrorNamespace(l.ctx, "tight")
		require.NoError(t, err)
		require.NoError(t, l.quota.SetQuota(l.ctx, tight.ID, 1))
		jid, _, err := l.svc.StartPull(l.ctx, upstream.PullRequest{Repo: hist, Actor: l.user})
		require.NoError(t, err)
		requireRefused(jid)
		_, err = l.real.Resolve(l.ctx, hist, "main")
		require.Error(t, err, "W2 stored no history")

		// The history fits; the weights do not.
		big := bytes.Repeat([]byte("over quota "), 20_000) // 220 KB
		sel := model.RepoID{Type: model.RepoTypeModel, Namespace: "roomy", Name: "weights"}
		upstreamRepo(sel)
		up.seed(t, sel, map[string][]byte{"config.json": []byte(`{}`), "model.safetensors": big})
		roomy, err := l.orgs.EnsureMirrorNamespace(l.ctx, "roomy")
		require.NoError(t, err)
		require.NoError(t, l.quota.SetQuota(l.ctx, roomy.ID, 64<<10))
		jid, _, err = l.svc.StartPull(l.ctx, upstream.PullRequest{Repo: sel, Actor: l.user})
		require.NoError(t, err)
		requireRefused(jid)
		_, err = l.real.Resolve(l.ctx, sel, "main")
		require.NoError(t, err, "the history fitted")
		files, _, err := l.queue.List(l.ctx, jobs.Filter{Kinds: []jobs.Kind{jobs.KindPullFile}})
		require.NoError(t, err)
		require.Empty(t, files, "nothing was transferred")
		usage, err := l.quota.NamespaceUsage(l.ctx, roomy.ID)
		require.NoError(t, err)
		require.Zero(t, usage.LFSBytes)

		// An admin raises the quota; retrying the Pull stores the weights.
		require.NoError(t, l.quota.SetQuota(l.ctx, roomy.ID, 0))
		retry, err := l.queue.Retry(l.ctx, jid)
		require.NoError(t, err)
		j := l.wait(retry, time.Minute)
		require.Equal(t, jobs.StatusSucceeded, j.Status, j.Error)
		require.Equal(t, big, l.read(sel, "model.safetensors"))
		usage, err = l.quota.NamespaceUsage(l.ctx, roomy.ID)
		require.NoError(t, err)
		require.Equal(t, int64(len(big)), usage.LFSBytes)
	})
}

// TestRealPullEndsPromptlyAfterALostLease is the root cause of a
// TestRealPullRespectsNamespaceQuota failure on a loaded machine. The
// worker leases Jobs for 3 seconds and renews every second; a renewal
// that comes late lets the worker recover the pull Job and start a second
// run of it while the first is still going. The second run stores the
// Mirror's history first. W1 fences Job writes on the attempt number, so
// the first run loses the Job at its next write or lease renewal, its
// context is cancelled and the worker discards its result: whatever it
// makes of its pack (repo.ErrRefConflict from ApplyPack, or the cancelled
// context) cannot back the Job off or end it. This test makes the lease
// expire at that point on purpose. The Pull must end promptly with the
// second run's quota refusal.
func TestRealPullEndsPromptlyAfterALostLease(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		up := newSecondInstance(t)
		sel := model.RepoID{Type: model.RepoTypeModel, Namespace: "roomy", Name: "weights"}
		var (
			l     *localInstance
			jid   jobs.JobID
			calls atomic.Int32
			mu    sync.Mutex
			// stale is the context of the run whose lease the hook
			// expires, and staleAttempt its attempt number.
			stale        context.Context
			staleAttempt int
		)
		ready := make(chan struct{})
		outcomes := &jobOutcomes{}
		// The hook reads and writes with l.ctx: the run it is called
		// from can lose its lease, which cancels the run's context.
		get := func() *jobs.Job {
			j, err := l.queue.Get(l.ctx, jid)
			if err != nil {
				t.Errorf("read the pull Job: %v", err)
				return &jobs.Job{}
			}
			return j
		}
		waitFor := func(what string, cond func() bool) {
			deadline := time.Now().Add(time.Minute)
			for !cond() {
				if time.Now().After(deadline) {
					t.Errorf("timed out waiting until %s", what)
					return
				}
				time.Sleep(10 * time.Millisecond)
			}
		}
		historyStored := func() bool {
			_, err := l.real.Resolve(l.ctx, sel, "main")
			return err == nil
		}
		// Runs are told apart by their context, not by the order of the
		// calls: a run that loses its lease on its own under load never
		// reaches the hook, and the next run's first check takes its place.
		hook := func(ctx context.Context) {
			<-ready
			calls.Add(1)
			mu.Lock()
			first := stale == nil && ctx.Err() == nil
			if first {
				stale = ctx
				staleAttempt = get().Attempts
			}
			own, attempt := ctx == stale, staleAttempt
			mu.Unlock()
			switch {
			case first:
				// This run is about to store the history. Its lease
				// expires, and the worker starts another run, which
				// stores the history first.
				waitFor("another run of the Pull stored the history", func() bool {
					if get().Attempts <= attempt {
						// Only this run's lease: a later run must keep
						// its own.
						_, err := d.NewUpdate().Model((*db.Job)(nil)).Set("lease_expires_at = ?", time.Now().UTC().Add(-time.Minute)).
							Where("id = ? AND status = ? AND attempts = ?", jid, string(jobs.StatusRunning), attempt).Exec(l.ctx)
						if err != nil {
							t.Errorf("expire the lease: %v", err)
						}
						return false
					}
					return historyStored()
				})
			case own, ctx.Err() != nil, !historyStored():
				// The stale run, a run that already lost its lease, or a
				// later run's check of the pack it is about to store.
			default:
				// A later run's selection check waits until the stale run
				// has ended, so the stale run is done with its pack before
				// this run ends the Job.
				waitFor("the first run ended", func() bool { return outcomes.count(jid, "lost") > 0 })
			}
		}
		l = newLocal(t, d, up.URL, localOptions{partSize: 1 << 20, beforeQuotaCheck: hook, logger: slog.New(outcomes)})
		owner := up.Real.AddUser(t, sel.Namespace)
		_, err := up.Real.Create(l.ctx, repo.CreateParams{ID: sel, Actor: owner, Visibility: model.VisibilityPublic})
		require.NoError(t, err)
		up.seed(t, sel, map[string][]byte{"config.json": []byte(`{}`), "model.safetensors": bytes.Repeat([]byte("over quota "), 20_000)})
		roomy, err := l.orgs.EnsureMirrorNamespace(l.ctx, sel.Namespace)
		require.NoError(t, err)
		require.NoError(t, l.quota.SetQuota(l.ctx, roomy.ID, 64<<10))

		jid, _, err = l.svc.StartPull(l.ctx, upstream.PullRequest{Repo: sel, Actor: l.user})
		require.NoError(t, err)
		close(ready)
		// Well under the 30-second backoff a stale run's ref conflict
		// used to cause.
		j := l.wait(jid, 20*time.Second)
		require.GreaterOrEqual(t, calls.Load(), int32(3), "the lease expired mid-run")
		mu.Lock()
		attempt := staleAttempt
		mu.Unlock()
		require.Greater(t, j.Attempts, attempt, "another run took the Job over")
		require.Positive(t, outcomes.count(jid, "lost"), "the worker discarded the stale run's result")
		require.Equal(t, 1, outcomes.count(jid, "failed"), "only the run holding the Job ended it")
		require.Equal(t, jobs.StatusFailed, j.Status, j.Error)
		var st map[string]any
		require.NoError(t, json.Unmarshal(j.Result, &st))
		require.Equal(t, "quota_exceeded", st["reason"])
		require.NotContains(t, st, "ref_conflicts", "the stale run recorded nothing")
		require.Contains(t, j.Error, "storage quota. Delete files or ask an admin to raise the quota.")
		require.Equal(t, "Storage quota exceeded", j.Progress.Message)
	})
}

// jobOutcomes is a slog handler that keeps the Job ID and outcome of every
// "job finished" record a worker logs.
type jobOutcomes struct {
	mu   sync.Mutex
	seen []string
}

func (*jobOutcomes) Enabled(context.Context, slog.Level) bool { return true }
func (h *jobOutcomes) WithAttrs([]slog.Attr) slog.Handler     { return h }
func (h *jobOutcomes) WithGroup(string) slog.Handler          { return h }

func (h *jobOutcomes) Handle(_ context.Context, r slog.Record) error {
	if r.Message != "job finished" {
		return nil
	}
	var id, outcome string
	r.Attrs(func(a slog.Attr) bool {
		switch a.Key {
		case "job_id":
			id = a.Value.String()
		case "outcome":
			outcome = a.Value.String()
		}
		return true
	})
	h.mu.Lock()
	h.seen = append(h.seen, id+" "+outcome)
	h.mu.Unlock()
	return nil
}

// count returns how many runs of the Job ended with outcome.
func (h *jobOutcomes) count(id jobs.JobID, outcome string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, s := range h.seen {
		if s == id.String()+" "+outcome {
			n++
		}
	}
	return n
}

// The interfaces the integration lead wires W6 through, checked against
// the other workstreams' types.
var (
	_ upstream.QuotaChecker      = pullQuota{}
	_ gitserver.PullThrough      = (*upstream.PullThrough)(nil)
	_ hfapi.PullThrough          = pullThroughAdapter{}
	_ auth.UpstreamLookup        = (*upstream.Registry)(nil)
	_ upstream.UserTokens        = (*auth.UpstreamTokenStore)(nil)
	_ upstream.MirrorStateSetter = (*repo.Store)(nil)
	_ upstream.Windows           = jobs.Windows{}
	_ jobs.Limiter               = (*jobs.DBLimiter)(nil)
)
