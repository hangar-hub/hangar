package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"

	"github.com/jamesagarside/hangar/internal/api"
	"github.com/jamesagarside/hangar/internal/api/gen"
	"github.com/jamesagarside/hangar/internal/audit"
	"github.com/jamesagarside/hangar/internal/auth"
	"github.com/jamesagarside/hangar/internal/blob"
	"github.com/jamesagarside/hangar/internal/blob/backends"
	"github.com/jamesagarside/hangar/internal/collections"
	"github.com/jamesagarside/hangar/internal/community"
	"github.com/jamesagarside/hangar/internal/community/communitydb"
	"github.com/jamesagarside/hangar/internal/config"
	"github.com/jamesagarside/hangar/internal/datasets"
	"github.com/jamesagarside/hangar/internal/datasets/layout"
	"github.com/jamesagarside/hangar/internal/datasets/meta"
	"github.com/jamesagarside/hangar/internal/datasets/pipeline"
	"github.com/jamesagarside/hangar/internal/datasets/viewer"
	"github.com/jamesagarside/hangar/internal/db"
	"github.com/jamesagarside/hangar/internal/gc"
	"github.com/jamesagarside/hangar/internal/gitserver"
	"github.com/jamesagarside/hangar/internal/gitstore"
	"github.com/jamesagarside/hangar/internal/hfapi"
	"github.com/jamesagarside/hangar/internal/httpx"
	"github.com/jamesagarside/hangar/internal/index"
	"github.com/jamesagarside/hangar/internal/index/indexdb"
	"github.com/jamesagarside/hangar/internal/jobs"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/ollama"
	"github.com/jamesagarside/hangar/internal/org"
	"github.com/jamesagarside/hangar/internal/repo"
	"github.com/jamesagarside/hangar/internal/scan"
	"github.com/jamesagarside/hangar/internal/secrets"
	"github.com/jamesagarside/hangar/internal/telemetry"
	"github.com/jamesagarside/hangar/internal/upstream"
	"github.com/jamesagarside/hangar/internal/webhook"
	"github.com/jamesagarside/hangar/internal/xet"
)

// instance holds every service of one Hangar process, built from the
// configuration: the real implementations of each workstream's interfaces,
// wired to each other as the handoffs in docs/build/handoffs describe.
type instance struct {
	cfg *config.Config
	log *slog.Logger
	tel *telemetry.Telemetry
	db  *db.DB

	blobs     blob.Store
	downloads blob.Downloads
	signer    *httpx.Signer
	sealer    *secrets.Sealer
	loc       *time.Location
	owner     string

	reg        *jobs.Registry
	queue      *jobs.DBQueue
	limiter    *jobs.DBLimiter
	accounting *gc.Accounting
	collector  *gc.Collector

	audit       *audit.DBRecorder
	orgs        *org.DBService
	auth        *auth.Service
	webhooks    *webhook.Service
	git         *gitstore.Store
	lfs         *repo.LFS
	repos       *repo.Store
	indexer     *indexdb.Service
	scanner     *scan.Service
	unsafeGate  *scan.Gate
	community   *communitydb.DB
	collections *collections.DB
	layout      *layout.Service
	datasets    *pipeline.Store
	pipeline    *pipeline.Pipeline
	viewer      *viewer.Service
	registry    *upstream.Registry
	client      *upstream.HTTPClient
	pulls       *upstream.Service
	xet         *xet.Service
	casURL      string
	gitServer   *gitserver.Server
	hub         *hfapi.Handler
	ollama      *ollama.Handler
}

// Compile-time checks that the real implementations satisfy the
// interfaces the other workstreams take.
var (
	_ repo.Service           = (*repo.Store)(nil)
	_ repo.LFSStore          = (*repo.LFS)(nil)
	_ gitserver.PullThrough  = (*upstream.PullThrough)(nil)
	_ ollama.PullThrough     = (*upstream.PullThrough)(nil)
	_ auth.UpstreamLookup    = (*upstream.Registry)(nil)
	_ upstream.UserTokens    = (*auth.UpstreamTokenStore)(nil)
	_ api.NamespaceSearcher  = (*org.DBService)(nil)
	_ xet.Ingestor           = (*xet.Service)(nil)
	_ webhook.Emitter        = (*webhook.Service)(nil)
	_ hfapi.PullThrough      = hubPullThrough{}
	_ repo.QuotaChecker      = repoQuota{}
	_ gitserver.QuotaChecker = repoQuota{}
	_ upstream.QuotaChecker  = pullQuota{}
)

// newInstance builds every service. It does not start the worker or serve
// HTTP; serve and the admin commands decide what runs.
func newInstance(ctx context.Context, cfg *config.Config, tel *telemetry.Telemetry, d *db.DB, blobs blob.Store) (*instance, error) {
	in := &instance{cfg: cfg, log: tel.Logger, tel: tel, db: d}
	inst := tel.Instruments
	var err error

	// Blob store (W1), opened by the caller.
	in.blobs = blobs
	in.downloads = backends.Downloads(cfg.Storage, in.blobs)

	key, err := cfg.InstanceKeyBytes()
	if err != nil {
		return nil, fmt.Errorf("security.instance_key: %w", err)
	}
	if in.signer, err = httpx.NewSigner(key); err != nil {
		return nil, err
	}
	if in.sealer, err = secrets.New(key); err != nil {
		return nil, err
	}
	if in.loc, err = time.LoadLocation(cfg.Server.Timezone); err != nil {
		return nil, fmt.Errorf("server.timezone: %w", err)
	}
	in.owner = processOwner()
	// Local scratch space: spooled xorbs and Parquet files, and Pull-through
	// transfers that clients read while they arrive.
	for _, dir := range []string{filepath.Join(cfg.Server.DataDir, "tmp"), filepath.Join(cfg.Server.DataDir, "pull-through")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
	}

	// Jobs, limiter and accounting (W1).
	in.reg = jobs.NewRegistry()
	in.queue = jobs.NewQueue(d, in.reg, jobs.QueueOptions{Location: in.loc})
	in.accounting = gc.NewAccounting(d)
	in.collector = gc.New(d, in.blobs, gc.Options{GracePeriod: cfg.GC.GracePeriod, PackGracePeriod: cfg.Git.PackGracePeriod,
		PendingPackAge: cfg.Git.Timeout + cfg.Git.PackGracePeriod, Logger: in.log, Instrument: inst})

	// Identity, Organisations and audit (W5), with the Upstream registry
	// (W6) that names Upstreams for upstream tokens.
	in.audit = audit.NewDB(d, tel.LoggerProvider)
	in.orgs = org.NewDB(d, in.audit)
	in.registry = upstream.NewRegistry(d, in.sealer, in.audit)
	in.auth, err = auth.New(auth.Options{DB: d, Config: cfg, Orgs: in.orgs, Audit: in.audit, Instruments: inst, Log: in.log,
		Upstreams: in.registry})
	if err != nil {
		return nil, err
	}
	in.limiter = jobs.NewLimiter(d, jobs.LimiterOptions{Owner: in.owner, Budget: func(ctx context.Context, _ string) (int64, error) {
		s, err := in.auth.Settings.Get(ctx)
		return s.PullBandwidthLimit, err
	}})

	// Webhooks (W7) before the Repo service, which emits Repo events.
	in.webhooks = webhook.New(webhook.Options{DB: d, Queue: in.queue, Sealer: in.sealer, Instruments: inst, Log: in.log,
		UserAgent: "Hangar/" + version, AllowPrivateTargets: func(ctx context.Context) (bool, error) {
			s, err := in.auth.Settings.Get(ctx)
			return s.WebhooksAllowPrivateTargets, err
		}})

	// git storage and the Repo service (W2). The index and community
	// services read Repos through the Repo service, and the Repo service
	// lists with their help, so they are bound once both exist.
	in.git, err = gitstore.New(gitstore.Options{DB: d, Blobs: in.blobs, CacheDir: cfg.Git.CacheDir, CacheSize: cfg.Git.CacheSize.Int64(),
		Timeout: cfg.Git.Timeout, Instruments: inst, Log: in.log})
	if err != nil {
		return nil, fmt.Errorf("git storage: %w", err)
	}
	in.lfs = repo.NewLFS(d, in.blobs)
	idx, comm := &boundIndexer{}, &boundCommunity{}
	in.repos, err = repo.NewStore(repo.Options{DB: d, Git: in.git, LFS: in.lfs, Namespaces: in.orgs, Jobs: in.queue,
		Webhooks: in.webhooks, Audit: in.audit, Quota: repoQuota{in.accounting}, Index: idx, Community: comm,
		AnonymousRead: cfg.Security.AnonymousRead, RestoreWindow: cfg.Repos.RestoreWindow, MaxBlobSize: cfg.Git.MaxBlobSize.Int64(),
		MaxPackSize: cfg.Git.MaxPackSize.Int64(), AllowForcePush: cfg.Git.AllowForcePush, CompactThreshold: cfg.Git.CompactThreshold,
		VerifyWait: cfg.LFS.VerifyWait, Log: in.log})
	if err != nil {
		return nil, err
	}
	in.auth.Ollama.Repos = api.AuthRepoResolver{Repos: in.repos}

	// Datasets (W10): Config and Split resolution, the Parquet export store
	// and the Dataset Viewer.
	in.layout = &layout.Service{Repos: in.repos}
	in.datasets = &pipeline.Store{DB: d, Blobs: in.blobs}

	// Upstreams and Pulls (W6).
	in.client = upstream.NewHTTPClient(upstream.HTTPOptions{Version: version, Tracer: tel.Tracer()})
	in.pulls, err = upstream.New(upstream.Options{
		DB: d, Registry: in.registry, Client: in.client, Repos: in.repos, LFS: in.lfs, Orgs: in.orgs, Queue: in.queue,
		Limiter: in.limiter, Authz: in.auth.Authz(), Layout: in.layout, Webhooks: in.webhooks, Audit: in.audit,
		Settings: pullSettings(in.auth),
		ParseWindows: func(specs []string) (upstream.Windows, error) {
			return jobs.ParseWindows(specs, in.loc)
		},
		Snooze: jobs.Snooze, MirrorState: in.repos, UserTokens: in.auth.UpstreamTokens, Signer: in.signer,
		PublicURL: cfg.Server.PublicURL, Dependencies: cfg.Pull.Dependencies, Quota: pullQuota{in.accounting},
		SpoolDir: filepath.Join(cfg.Server.DataDir, "pull-through"), Instruments: inst, Log: in.log,
	})
	if err != nil {
		return nil, err
	}

	// Metadata, community and Collections (W7).
	ixo := indexdb.Options{DB: d, Repos: in.repos, Webhooks: in.webhooks, Instruments: inst, Log: in.log,
		Layout: datasetLayout(in.layout), AfterIndex: in.afterIndex}
	if cfg.Viewer.Enabled {
		ixo.DatasetRows = datasetRows(in.layout, in.datasets)
	}
	in.indexer = indexdb.New(ixo)
	in.scanner = scan.New(scan.Options{DB: d, Repos: in.repos, LFS: in.lfs, Webhooks: in.webhooks, Instruments: inst, Log: in.log})
	// One gate enforces scan.block_unsafe_downloads for every protocol
	// that serves file bytes.
	in.unsafeGate = in.scanner.NewGate(func(ctx context.Context) (bool, error) {
		s, err := in.auth.Settings.Get(ctx)
		return s.ScanBlockUnsafeDownloads, err
	})
	in.community = communitydb.New(communitydb.Options{DB: d, Repos: in.repos, Webhooks: in.webhooks, Log: in.log})
	in.collections = collections.New(collections.Options{DB: d, Orgs: in.orgs, Repos: in.repos, Authz: in.auth.Authz()})
	idx.Indexer, comm.Service = in.indexer, in.community

	// Xet uploads (W11).
	xo, err := xet.OptionsFromConfig(cfg)
	if err != nil {
		return nil, err
	}
	xo.DB, xo.Blobs, xo.LFS = d, in.blobs, in.lfs
	xo.Instruments, xo.Log, xo.Tracer = inst, in.log, tel.Tracer()
	xo.TempDir = filepath.Join(cfg.Server.DataDir, "tmp")
	in.casURL = xo.CASURL
	if in.xet, err = xet.New(xo); err != nil {
		return nil, err
	}

	if cfg.Viewer.Enabled {
		in.pipeline, err = pipeline.New(pipeline.Deps{DB: d, Blobs: in.blobs, Repos: in.repos, LFS: in.lfs, Jobs: in.queue,
			MaxConvertBytes: cfg.Viewer.MaxConvertBytes.Int64(), TempDir: filepath.Join(cfg.Server.DataDir, "tmp"),
			ConvertConcurrency: cfg.Viewer.ConvertConcurrency, AfterConvert: in.reindexExported, Log: in.log})
		if err != nil {
			return nil, err
		}
		in.viewer, err = viewer.New(viewer.Deps{Repos: in.repos, Authz: in.auth.Authz(), Store: in.datasets, Jobs: in.queue,
			Blobs: in.blobs, Signer: in.signer, Log: in.log, Now: time.Now},
			viewer.Options{PublicURL: cfg.Server.PublicURL, MaxScanRows: int64(cfg.Viewer.MaxScanRows), MaxScanTime: cfg.Viewer.MaxScanTime,
				MaxCellBytes: int(cfg.Viewer.MaxCellBytes.Int64()), MaxRequestBytes: cfg.Viewer.MaxRequestBytes.Int64()})
		if err != nil {
			return nil, err
		}
	}

	// Protocol handlers: git and git-lfs (W3), the Hub API (W4) and the
	// Ollama registry (W7).
	gcfg := gitserver.ConfigFrom(cfg)
	gcfg.TempDir = filepath.Join(cfg.Git.CacheDir, "tmp")
	in.gitServer, err = gitserver.New(gcfg, gitserver.Deps{Repos: in.repos, LFS: in.lfs, Authz: in.auth.Authz(), Signer: in.signer,
		Quota: repoQuota{in.accounting}, PullThrough: in.pulls.PullThrough(), Downloads: in.unsafeGate, Log: in.log, Tracer: tel.Tracer(),
		Instruments: inst})
	if err != nil {
		return nil, err
	}
	hubDeps := hfapi.Deps{Settings: hfapi.FromConfig(cfg), Repos: in.repos, LFS: in.lfs, Authz: in.auth.Authz(), Orgs: in.orgs,
		Community: in.community, Collections: in.collections, Index: in.indexer, Scan: in.scanner, Xet: in.xet,
		Devices: in.auth.DeviceFlow(), PullThrough: hubPullThrough{in.pulls.PullThrough()}, Instruments: inst, Log: in.log,
		Downloads: in.unsafeGate}
	if in.viewer != nil {
		hubDeps.Viewer = in.viewer
	} else {
		hubDeps.Viewer = disabledViewer{}
	}
	if in.hub, err = hfapi.New(hubDeps); err != nil {
		return nil, err
	}
	in.ollama, err = ollama.New(ollama.Options{DB: d, Repos: in.repos, LFS: in.lfs, Authz: in.auth.Authz(), OllamaAuth: in.auth.OllamaAuth(),
		Signer: in.signer, PublicURL: cfg.Server.PublicURL, PullThrough: in.pulls.PullThrough(), Downloads: in.unsafeGate,
		Community: in.community, Instruments: inst, Log: in.log})
	if err != nil {
		return nil, err
	}

	if err := in.registerJobs(ctx); err != nil {
		return nil, err
	}
	return in, nil
}

// registerJobs registers every Job handler and schedule. Every process
// registers them, because the queue reads each kind's defaults from the
// registry when it enqueues; only processes with a worker run them.
func (in *instance) registerJobs(ctx context.Context) error {
	cfg := in.cfg
	steps := []func() error{
		// gc and upload.expire (W1), with their schedules.
		func() error { return gc.Register(ctx, in.reg, in.queue, in.collector, cfg.GC.Schedule) },
		// zip.export, repo.purge and git.compact (W2); upload.expire is W1's.
		func() error {
			return in.repos.RegisterJobs(in.reg, in.lfs, repo.JobOptions{Blobs: in.blobs, PackGracePeriod: cfg.Git.PackGracePeriod,
				ExportURL: api.ExportURL(cfg.Server.PublicURL, in.blobs, in.signer), UploadsExpiredElsewhere: true})
		},
		// auth.cleanup (W5).
		func() error { return in.auth.RegisterJobs(in.reg) },
		func() error { return in.queue.SetSchedule(ctx, auth.CleanupSchedule()) },
		// pull, pull.file and mirror.sync (W6).
		func() error { return in.pulls.RegisterJobs(in.reg) },
		// index, scan and webhook.deliver (W7).
		func() error { return in.indexer.Register(in.reg) },
		func() error { return in.scanner.Register(in.reg) },
		func() error { return in.webhooks.Register(in.reg) },
		// xet.gc (W11).
		func() error { return in.xet.RegisterJobs(in.reg) },
		func() error { return in.queue.SetSchedule(ctx, xet.GCSchedule(cfg.GC.Schedule)) },
	}
	if in.pipeline != nil {
		// dataset.convert and dataset.stats (W10).
		steps = append(steps, func() error { return in.pipeline.Register(in.reg) })
	}
	for _, step := range steps {
		if err := step(); err != nil {
			return fmt.Errorf("register jobs: %w", err)
		}
	}
	return nil
}

// afterIndex runs after each index Job (W7's hook): it resolves the
// Commit's Dependencies (W6) and queues the Parquet export of a dataset's
// default-branch head (W10).
func (in *instance) afterIndex(ctx context.Context, job index.CommitJob, _ *index.Summary) error {
	if warnings, err := in.pulls.ResolveDependencies(ctx, job.RepoUUID, job.Commit); err != nil {
		// GET .../dependencies resolves a head Commit with no rows on
		// first read, so a failure here is not worth retrying the index.
		in.log.WarnContext(ctx, "resolve dependencies", "repo.id", job.Repo, "commit", string(job.Commit), "error", err.Error())
	} else if len(warnings) > 0 {
		in.log.InfoContext(ctx, "dependencies not pulled", "repo.id", job.Repo, "commit", string(job.Commit), "warnings", warnings)
	}
	if job.Type != model.RepoTypeDataset || in.pipeline == nil {
		return nil
	}
	id, err := model.ParseRepoID(job.Type, job.Repo)
	if err != nil {
		return jobs.Permanent(err)
	}
	head, err := in.repos.Resolve(ctx, id, "")
	if errors.Is(err, repo.ErrNotFound) || errors.Is(err, repo.ErrRevisionNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if head != job.Commit {
		return nil
	}
	_, err = pipeline.EnqueueConvert(ctx, in.queue, job.RepoUUID, job.Commit)
	return err
}

// newWorker returns the Job worker for roles all and worker.
func (in *instance) newWorker() *jobs.Worker {
	return jobs.NewWorker(in.db, in.reg, jobs.WorkerOptions{
		Owner: in.owner, Concurrency: in.cfg.Jobs.Workers, Lease: in.cfg.Jobs.Lease, Location: in.loc,
		InstanceLimits: map[jobs.Kind]int{jobs.KindPullFile: in.cfg.Pull.Concurrency},
		LeaderTasks:    []func(context.Context) error{jobs.RecordQueueGauge(in.db, in.tel.Instruments)},
		Logger:         in.log, Tracer: in.tel.Tracer(), Instrument: in.tel.Instruments,
	})
}

// refreshStale queues index and scan Jobs for branch heads whose results
// come from an older indexer or scanner version. Workers run it on start.
func (in *instance) refreshStale(ctx context.Context) {
	if n, err := in.indexer.ReindexStale(ctx, in.queue); err != nil {
		in.log.WarnContext(ctx, "reindex stale results", "error", err.Error())
	} else if n > 0 {
		in.log.InfoContext(ctx, "queued index Jobs for a new indexer version", "count", n)
	}
	if n, err := in.scanner.RescanStale(ctx, in.queue); err != nil {
		in.log.WarnContext(ctx, "rescan stale results", "error", err.Error())
	} else if n > 0 {
		in.log.InfoContext(ctx, "queued scan Jobs for a new scanner version", "count", n)
	}
}

// close releases what the instance holds open.
func (in *instance) close(ctx context.Context) {
	if in.limiter != nil {
		_ = in.limiter.Close(ctx)
	}
	if in.gitServer != nil {
		_ = in.gitServer.Close()
	}
}

// presignOrigins returns the origin of the Blob store's presigned upload
// URLs, which the UI's Content Security Policy must allow for direct
// browser uploads, or nil when the store does not presign.
func (in *instance) presignOrigins(ctx context.Context) []string {
	req, err := in.blobs.PresignPut(ctx, "tmp/csp-origin", 1, nil, time.Minute)
	if err != nil {
		return nil
	}
	u, err := url.Parse(req.URL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil
	}
	return []string{u.Scheme + "://" + u.Host}
}

// processOwner names this process in Job leases and bandwidth slices.
func processOwner() string {
	host, err := os.Hostname()
	if err != nil {
		host = "hangar"
	}
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%s:%d:%s", host, os.Getpid(), hex.EncodeToString(b))
}

// boundIndexer and boundCommunity let the Repo service use the index and
// community services, which are built after it because they read Repos
// through it. Both are set before the process serves or runs Jobs.
type boundIndexer struct{ index.Indexer }

type boundCommunity struct{ community.Service }

// pullSettings reads the Pull settings from the Instance settings (W5), so
// an admin's change applies within the settings cache lifetime.
func pullSettings(a *auth.Service) upstream.SettingsFunc {
	return func(ctx context.Context) (upstream.Settings, error) {
		s, err := a.Settings.Get(ctx)
		return upstream.Settings{Windows: s.PullWindows, AllowOverrideWindows: s.PullAllowOverrideWindows,
			PullThroughEnabled: s.PullThroughEnabled, PullThroughAllow: s.PullThroughAllow, PullThroughMinRole: s.PullThroughMinRole}, err
	}
}

// repoQuota enforces Namespace quotas (W1) on Commits and pushes (W2) and
// on git-lfs upload batches (W3). A refusal is a *repo.PolicyError, which
// every protocol answers with its own refusal and the message.
type repoQuota struct{ a *gc.Accounting }

func (q repoQuota) CheckQuota(ctx context.Context, ns uuid.UUID, adding int64) error {
	err := q.a.CheckQuota(ctx, ns, adding)
	if errors.Is(err, gc.ErrQuotaExceeded) {
		return &repo.PolicyError{Message: err.Error()}
	}
	return err
}

// pullQuota enforces Namespace quotas on Pulls (W6), marking W1's refusals
// as upstream.ErrQuotaExceeded so a Pull can tell them from a failed check.
type pullQuota struct{ *gc.Accounting }

func (q pullQuota) CheckQuota(ctx context.Context, ns uuid.UUID, adding int64) error {
	err := q.Accounting.CheckQuota(ctx, ns, adding)
	if errors.Is(err, gc.ErrQuotaExceeded) {
		return upstream.QuotaExceeded(err)
	}
	return err
}

// hubPullThrough adapts W6's Pull-through to the Hub API: planning that
// takes too long is hfapi.ErrPullPending, and a quota refusal is a
// *repo.PolicyError, which the Hub API answers with 403 and the message.
type hubPullThrough struct{ pt *upstream.PullThrough }

func (h hubPullThrough) Repo(ctx context.Context, p model.Principal, id model.RepoID) (*repo.Repo, error) {
	r, err := h.pt.Repo(ctx, p, id)
	return r, hubPullError(err)
}

func (h hubPullThrough) File(ctx context.Context, p model.Principal, r *repo.Repo, commit model.CommitSHA, e model.Entry) (io.ReadCloser, error) {
	rc, err := h.pt.File(ctx, p, r, commit, e)
	return rc, hubPullError(err)
}

func hubPullError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, upstream.ErrPullPending):
		return hfapi.ErrPullPending
	case errors.Is(err, upstream.ErrQuotaExceeded):
		return &repo.PolicyError{Message: err.Error()}
	}
	return err
}

// datasetLayout gives the index (W7) each dataset Config and its number of
// Splits from W10's resolution (DATA-9).
func datasetLayout(l *layout.Service) func(ctx context.Context, id model.RepoID, commit model.CommitSHA) ([]string, map[string]int, error) {
	return func(ctx context.Context, id model.RepoID, commit model.CommitSHA) ([]string, map[string]int, error) {
		res, err := l.Resolve(ctx, id, commit)
		if err != nil {
			return nil, nil, err
		}
		names := make([]string, 0, len(res.Configs))
		splits := make(map[string]int, len(res.Configs))
		for _, c := range res.Configs {
			names = append(names, c.Name)
			n := len(c.SplitOrder)
			if n == 0 {
				n = len(c.DataFiles)
			}
			splits[c.Name] = n
		}
		return names, splits, nil
	}
}

// datasetRows gives the index (W7) a dataset Commit's rows and size
// category from its Parquet export (W10), once the export exists (DATA-9).
func datasetRows(l *layout.Service, store *pipeline.Store) func(ctx context.Context, repoUUID uuid.UUID, id model.RepoID,
	commit model.CommitSHA) (int64, string, bool, error) {
	return func(ctx context.Context, repoUUID uuid.UUID, id model.RepoID, commit model.CommitSHA) (int64, string, bool, error) {
		res, err := l.Resolve(ctx, id, commit)
		if errors.Is(err, datasets.ErrNotFound) {
			return 0, "", false, nil
		}
		if err != nil {
			return 0, "", false, err
		}
		infos := make([]*pipeline.ConfigInfo, 0, len(res.Configs))
		for _, c := range res.Configs {
			info, err := store.Info(ctx, repoUUID, commit, c.Name)
			if errors.Is(err, pipeline.ErrNoInfo) {
				return 0, "", false, nil
			}
			if err != nil {
				return 0, "", false, err
			}
			infos = append(infos, info)
		}
		if len(infos) == 0 {
			return 0, "", false, nil
		}
		s := meta.Summarise(nil, infos)
		return s.NumRows, s.SizeCategory, true, nil
	}
}

// reindexExported indexes a dataset Commit again once its Parquet export
// is stored (W10), so the index has its rows and size category (DATA-9).
func (in *instance) reindexExported(ctx context.Context, r *repo.Repo, commit model.CommitSHA) error {
	job := index.CommitJob{RepoUUID: r.UUID, Repo: r.ID.String(), Type: r.ID.Type, Commit: commit}
	_, err := in.queue.Enqueue(ctx, jobs.NewJob{Kind: jobs.KindIndex, DedupeKey: job.DedupeKey(), Payload: job, Repo: r.UUID})
	return err
}

// mirrorStatus fills mirror_status on native Repo responses (W2) from the
// Upstream service (W6).
func (in *instance) mirrorStatus(ctx context.Context, r *repo.Repo) (*gen.MirrorSummary, error) {
	return api.UpstreamMirrorSummary(ctx, in.pulls, r)
}

// disabledViewer answers the Hub API's Parquet and Croissant endpoints
// when viewer.enabled is false: no dataset has a Parquet export, so every
// request is datasets.ErrNotFound, as on a dataset the Hub cannot convert.
type disabledViewer struct{}

func (disabledViewer) IsValid(context.Context, model.Principal, model.RepoID) (*datasets.IsValidResponse, error) {
	return nil, datasets.ErrNotFound
}

func (disabledViewer) Splits(context.Context, model.Principal, model.RepoID) (*datasets.SplitsResponse, error) {
	return nil, datasets.ErrNotFound
}

func (disabledViewer) FirstRows(context.Context, model.Principal, model.RepoID, string, string) (*datasets.FirstRowsResponse, error) {
	return nil, datasets.ErrNotFound
}

func (disabledViewer) Rows(context.Context, model.Principal, datasets.RowsRequest) (*datasets.RowsResponse, error) {
	return nil, datasets.ErrNotFound
}

func (disabledViewer) Search(context.Context, model.Principal, datasets.SearchRequest) (*datasets.RowsResponse, error) {
	return nil, datasets.ErrNotFound
}

func (disabledViewer) Filter(context.Context, model.Principal, datasets.FilterRequest) (*datasets.RowsResponse, error) {
	return nil, datasets.ErrNotFound
}

func (disabledViewer) Parquet(context.Context, model.Principal, model.RepoID, string) (*datasets.ParquetResponse, error) {
	return nil, datasets.ErrNotFound
}

func (disabledViewer) Info(context.Context, model.Principal, model.RepoID, string) (json.RawMessage, error) {
	return nil, datasets.ErrNotFound
}

func (disabledViewer) Size(context.Context, model.Principal, model.RepoID, string) (json.RawMessage, error) {
	return nil, datasets.ErrNotFound
}

func (disabledViewer) Statistics(context.Context, model.Principal, model.RepoID, string, string) (json.RawMessage, error) {
	return nil, datasets.ErrNotFound
}

func (disabledViewer) Croissant(context.Context, model.Principal, model.RepoID) (json.RawMessage, error) {
	return nil, datasets.ErrNotFound
}
