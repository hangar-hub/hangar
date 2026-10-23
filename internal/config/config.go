// Package config defines Hangar's configuration: the full set of keys with
// defaults and descriptions, loading from a YAML file, `HANGAR_*`
// environment variables and flags (in that order, later sources win),
// validation that reports every error at once, first-start secret
// generation, and the generated configuration reference.
//
// Every key has a `koanf` tag with its name and a `desc` tag with the
// description printed by `hangar config reference`.
package config

import "time"

// Config is the whole Hangar configuration.
type Config struct {
	Server    ServerConfig              `koanf:"server" desc:"HTTP server and process settings."`
	Database  DatabaseConfig            `koanf:"database" desc:"Database connection."`
	Storage   StorageConfig             `koanf:"storage" desc:"Blob store for LFS objects, git data and uploads."`
	Security  SecurityConfig            `koanf:"security" desc:"Secrets, sign-in and access defaults."`
	OIDC      OIDCConfig                `koanf:"oidc" desc:"OpenID Connect sign-in providers."`
	Upstreams map[string]UpstreamConfig `koanf:"upstreams" desc:"Upstreams that Hangar can Pull from, keyed by ID. huggingface is built in."`
	Pull      PullConfig                `koanf:"pull" desc:"Pull scheduling and network controls."`
	Git       GitConfig                 `koanf:"git" desc:"git storage, push limits and the local pack cache."`
	LFS       LFSConfig                 `koanf:"lfs" desc:"git-lfs uploads."`
	HFAPI     HFAPIConfig               `koanf:"hfapi" desc:"Hugging Face Hub API behaviour."`
	Xet       XetConfig                 `koanf:"xet" desc:"Xet upload ingestion."`
	Jobs      JobsConfig                `koanf:"jobs" desc:"Background Job workers."`
	GC        GCConfig                  `koanf:"gc" desc:"Garbage collection of unreferenced LFS objects and git data."`
	Repos     ReposConfig               `koanf:"repos" desc:"Repo lifecycle."`
	Viewer    ViewerConfig              `koanf:"viewer" desc:"Dataset Viewer and Parquet export."`
	UI        UIConfig                  `koanf:"ui" desc:"Web UI."`
	Webhooks  WebhooksConfig            `koanf:"webhooks" desc:"Webhook deliveries."`
	Telemetry TelemetryConfig           `koanf:"telemetry" desc:"OpenTelemetry traces, metrics and logs. Standard OTEL_* variables also apply and win."`
}

// UIConfig configures the web UI.
type UIConfig struct {
	DocsURL string `koanf:"docs_url" desc:"Where the web UI's links to the documentation go, such as a docs mirror on an air-gapped network. The mirror must serve the same paths as https://hangar.page/. Empty removes the links."`
}

// WebhooksConfig configures webhook deliveries.
type WebhooksConfig struct {
	AllowPrivateTargets bool `koanf:"allow_private_targets" desc:"Let webhooks reach private addresses: loopback, RFC 1918, carrier-grade NAT and unique local. Off by default, so users cannot make Hangar send requests into your network. Link-local and cloud metadata addresses are refused whatever this says. This is the default for the Instance setting of the same name, which an Instance admin can change under Admin, Settings."`
}

// DefaultDocsURL is the public documentation site.
const DefaultDocsURL = "https://hangar.page/"

// Process roles.
const (
	RoleAll    = "all"
	RoleWeb    = "web"
	RoleWorker = "worker"
)

// ServerConfig configures the HTTP server and the process.
type ServerConfig struct {
	PublicURL       string        `koanf:"public_url" desc:"The URL clients use to reach Hangar. Required. Used in redirects, LFS links and git URLs."`
	Listen          string        `koanf:"listen" desc:"Address to listen on for HTTP."`
	Role            string        `koanf:"role" desc:"Process role: all, web or worker. SQLite needs all."`
	DataDir         string        `koanf:"data_dir" desc:"Directory for local state: the SQLite lock file and the generated secrets.yaml."`
	TrustedProxies  []string      `koanf:"trusted_proxies" desc:"CIDR ranges of reverse proxies whose X-Forwarded-For and X-Forwarded-Proto headers Hangar trusts."`
	MaxRequestBody  ByteSize      `koanf:"max_request_body" desc:"Body size limit for endpoints that buffer the request body. Streaming endpoints have no limit apart from quotas."`
	ShutdownTimeout time.Duration `koanf:"shutdown_timeout" desc:"How long to let in-flight requests finish on SIGTERM. Longer downloads are cut."`
	Timezone        string        `koanf:"timezone" desc:"IANA time zone for pull windows and schedules."`
}

// Database drivers.
const (
	DriverSQLite   = "sqlite"
	DriverPostgres = "postgres"
)

// DatabaseConfig configures the database connection.
type DatabaseConfig struct {
	Driver       string `koanf:"driver" desc:"sqlite or postgres."`
	DSN          string `koanf:"dsn" desc:"SQLite file path, or a postgres:// connection URL."`
	MaxOpenConns int    `koanf:"max_open_conns" desc:"Maximum open database connections per process."`
}

// Storage backends, download modes and upload modes.
const (
	BackendFS     = "fs"
	BackendS3     = "s3"
	DownloadRedir = "redirect"
	DownloadProxy = "proxy"
	UploadDirect  = "direct"
	UploadProxy   = "proxy"
)

// StorageConfig configures the Blob store.
type StorageConfig struct {
	Backend string   `koanf:"backend" desc:"fs or s3."`
	FS      FSConfig `koanf:"fs" desc:"Filesystem backend."`
	S3      S3Config `koanf:"s3" desc:"S3-compatible backend."`
}

// FSConfig configures the filesystem Blob store.
type FSConfig struct {
	Path string `koanf:"path" desc:"Root directory of the Blob store."`
}

// S3Config configures the S3-compatible Blob store.
type S3Config struct {
	Endpoint        string        `koanf:"endpoint" desc:"S3 endpoint host, with an optional port. Use https:// or http:// to choose the scheme; the default is HTTPS."`
	Region          string        `koanf:"region" desc:"Bucket region."`
	Bucket          string        `koanf:"bucket" desc:"Bucket name."`
	Prefix          string        `koanf:"prefix" desc:"Key prefix inside the bucket."`
	AccessKeyID     string        `koanf:"access_key_id" desc:"Access key ID. Leave empty to use the default credential chain, IRSA or Workload Identity." secret:"true"`
	SecretAccessKey string        `koanf:"secret_access_key" desc:"Secret access key." secret:"true"`
	UsePathStyle    bool          `koanf:"use_path_style" desc:"Use path-style bucket addressing, as MinIO and some other stores need."`
	DownloadMode    string        `koanf:"download_mode" desc:"redirect sends clients to presigned URLs; proxy streams bytes through Hangar."`
	UploadMode      string        `koanf:"upload_mode" desc:"direct gives clients presigned URLs to upload LFS objects and parts to the store; proxy has them upload through Hangar. Use proxy when clients cannot reach the endpoint."`
	PresignTTL      time.Duration `koanf:"presign_ttl" desc:"Lifetime of presigned download URLs."`
	Retries         int           `koanf:"retries" desc:"How many times Hangar sends a failed part or object upload again, waiting longer each time. Part bodies are spooled to the temporary directory while they are sent. 0 turns retries off."`
	StartupWait     time.Duration `koanf:"startup_wait" desc:"How long a server or worker keeps trying to reach the bucket at start, such as an object store whose Pod is still starting, before it exits. /readyz answers 503 while it waits. 0 exits at once."`
	StallTimeout    time.Duration `koanf:"stall_timeout" desc:"How long a request to the store may go without sending data or getting a response before Hangar cancels it and, for uploads, retries. Raise it on slow or congested links."`
}

// Registration modes.
const (
	RegistrationOpen   = "open"
	RegistrationInvite = "invite"
	RegistrationClosed = "closed"
)

// SecurityConfig configures secrets and sign-in.
type SecurityConfig struct {
	InstanceKey    string          `koanf:"instance_key" desc:"32-byte key, base64-encoded, that encrypts stored secrets and signs URLs. Generated on first start if empty." secret:"true"`
	AnonymousRead  bool            `koanf:"anonymous_read" desc:"Let anonymous users read public Repos and see user and Organisation profiles."`
	Registration   string          `koanf:"registration" desc:"Who can create an account: open, invite or closed."`
	LocalLogin     bool            `koanf:"local_login" desc:"Show the username and password form. The bootstrap admin can always sign in at /auth/login?local=1."`
	SessionTTL     time.Duration   `koanf:"session_ttl" desc:"Lifetime of a UI session."`
	BootstrapAdmin BootstrapConfig `koanf:"bootstrap_admin" desc:"The first admin, created on start if no admin exists."`
}

// BootstrapConfig describes the first admin.
type BootstrapConfig struct {
	Username     string `koanf:"username" desc:"Username of the bootstrap admin."`
	Password     string `koanf:"password" desc:"Password of the bootstrap admin. Generated on first start if empty and no password_file is set." secret:"true"`
	PasswordFile string `koanf:"password_file" desc:"File that holds the bootstrap admin password."`
	Email        string `koanf:"email" desc:"Email address of the bootstrap admin."`
}

// OIDCConfig lists OpenID Connect providers.
type OIDCConfig struct {
	Providers []OIDCProvider `koanf:"providers" desc:"Providers, shown as sign-in buttons. Set them in the YAML file, or as a JSON array in HANGAR_OIDC_PROVIDERS."`
}

// OIDCProvider configures one provider.
type OIDCProvider struct {
	ID              string            `koanf:"id" json:"id" desc:"Stable provider ID, used in callback URLs."`
	Name            string            `koanf:"name" json:"name" desc:"Name on the sign-in button."`
	Issuer          string            `koanf:"issuer" json:"issuer" desc:"Issuer URL, used for discovery."`
	ClientID        string            `koanf:"client_id" json:"client_id" desc:"OAuth client ID."`
	ClientSecret    string            `koanf:"client_secret" json:"client_secret" desc:"OAuth client secret. Leave empty for public clients." secret:"true"`
	Scopes          []string          `koanf:"scopes" json:"scopes" desc:"Scopes to request."`
	Claims          map[string]string `koanf:"claims" json:"claims" desc:"Claim names for username, email, name and groups."`
	AdminGroups     []string          `koanf:"admin_groups" json:"admin_groups" desc:"Members of these groups are Instance admins. Checked at every sign-in."`
	OrgMappings     []OrgMapping      `koanf:"org_mappings" json:"org_mappings" desc:"Groups that grant Organisation Roles. Checked at every sign-in."`
	AutoCreateUsers bool              `koanf:"auto_create_users" json:"auto_create_users" desc:"Create a User on first sign-in."`
}

// OrgMapping grants an Organisation Role to members of a group.
type OrgMapping struct {
	Group string `koanf:"group" json:"group" desc:"Group claim value."`
	Org   string `koanf:"org" json:"org" desc:"Organisation name."`
	Role  string `koanf:"role" json:"role" desc:"read, write or admin."`
}

// UpstreamConfig configures one Upstream.
type UpstreamConfig struct {
	BaseURL         string `koanf:"base_url" desc:"Base URL of the Upstream's Hub API."`
	Enabled         bool   `koanf:"enabled" desc:"Whether users can Discover and Pull from this Upstream."`
	Token           string `koanf:"token" desc:"Optional Instance token, used when the requesting user has not saved their own." secret:"true"`
	NamespacePrefix string `koanf:"namespace_prefix" desc:"Prefix for Mirror Namespaces from this Upstream, for example hf-. Prefixed Mirrors no longer work as drop-in HF_ENDPOINT replacements."`
	ViewerURL       string `koanf:"viewer_url" desc:"Dataset Viewer URL of the Upstream. Default: the base URL with datasets-server. added to the host."`
}

// PullConfig configures Pulls.
type PullConfig struct {
	BandwidthLimit ByteSize          `koanf:"bandwidth_limit" desc:"Bytes per second across the Instance. 0 means unlimited."`
	Concurrency    int               `koanf:"concurrency" desc:"Files transferred at once across the Instance."`
	Windows        []string          `koanf:"windows" desc:"Times when Pull transfers may run, such as 01:00-07:00, in server.timezone. Empty means any time."`
	StallTimeout   time.Duration     `koanf:"stall_timeout" desc:"How long an Upstream may take to answer a request, or go without sending data during a download, before Hangar cuts the connection and retries. Raise it on slow or congested links."`
	PullThrough    PullThroughConfig `koanf:"pull_through" desc:"Start Pulls automatically when clients ask for Repos that are not in the Arsenal."`
	Dependencies   DependencyConfig  `koanf:"dependencies" desc:"Dependency resolution."`
}

// PullThroughConfig configures Pull-through.
type PullThroughConfig struct {
	Enabled bool     `koanf:"enabled" desc:"Turn on Pull-through."`
	Allow   []string `koanf:"allow" desc:"Globs of upstream Repo IDs that may be pulled through."`
	MinRole string   `koanf:"min_role" desc:"Who may trigger Pull-through: anonymous or user (any signed-in user)."`
}

// DependencyConfig configures Dependency Pulls.
type DependencyConfig struct {
	AutoPullCode       bool `koanf:"auto_pull_code" desc:"Pull code Dependencies (from auto_map) automatically."`
	AutoPullBaseModels bool `koanf:"auto_pull_base_models" desc:"Pull the base model of adapters automatically."`
	MaxDepth           int  `koanf:"max_depth" desc:"Maximum depth of recursive Dependency Pulls."`
}

// GitConfig configures git storage and the git server.
type GitConfig struct {
	MaxBlobSize      ByteSize      `koanf:"max_blob_size" desc:"Largest non-LFS file accepted on push."`
	MaxPackSize      ByteSize      `koanf:"max_pack_size" desc:"Largest pack accepted on push."`
	AllowForcePush   bool          `koanf:"allow_force_push" desc:"Allow force-pushing default branches. Namespace admins always can."`
	CompactThreshold int           `koanf:"compact_threshold" desc:"Compact a Repo's packs when it has more live packs than this."`
	PackGracePeriod  time.Duration `koanf:"pack_grace_period" desc:"How long retired packs are kept before deletion. Must be longer than the longest request."`
	CacheDir         string        `koanf:"cache_dir" desc:"Local pack cache directory. Disposable."`
	CacheSize        ByteSize      `koanf:"cache_size" desc:"Size limit of the local pack cache."`
	Timeout          time.Duration `koanf:"timeout" desc:"Wall-clock limit for one git operation."`
}

// LFSConfig configures git-lfs uploads.
type LFSConfig struct {
	MultipartThreshold ByteSize      `koanf:"multipart_threshold" desc:"Objects larger than this use multipart uploads when the client supports them."`
	UploadURLTTL       time.Duration `koanf:"upload_url_ttl" desc:"Lifetime of signed upload URLs."`
	VerifyWait         time.Duration `koanf:"verify_wait" desc:"How long a Commit waits for an LFS object that is still being verified."`
}

// Xet modes.
const (
	XetIngest = "ingest"
	XetReject = "reject"
)

// HFAPIConfig configures the Hub API.
type HFAPIConfig struct {
	RegularUploadMax ByteSize `koanf:"regular_upload_max" desc:"Preupload answers regular for files below this size unless .gitattributes says LFS."`
	Xet              string   `koanf:"xet" desc:"ingest accepts Xet uploads; reject turns them off and asks clients to set HF_HUB_DISABLE_XET=1."`
	DefaultNamespace string   `koanf:"default_namespace" desc:"Namespace that single-segment Repo IDs such as gpt2 redirect to. Empty turns the redirect off."`
}

// XetConfig configures Xet ingestion.
type XetConfig struct {
	CASURL        string        `koanf:"cas_url" desc:"URL of the Xet CAS endpoints. Default: <public_url>/xet-cas."`
	TokenTTL      time.Duration `koanf:"token_ttl" desc:"Lifetime of Xet write tokens."`
	XorbRetention time.Duration `koanf:"xorb_retention" desc:"How long xorbs are kept after their last use, so clients can deduplicate against them. Keep it longer than hf_xet's 21-day shard cache."`
	MaxFileSize   ByteSize      `koanf:"max_file_size" desc:"Largest file a Xet upload may describe. One shard may rebuild at most four times this, counting only files not uploaded before; larger uploads get 413."`
}

// JobsConfig configures Job workers.
type JobsConfig struct {
	Workers int           `koanf:"workers" desc:"Concurrent Jobs per worker process."`
	Lease   time.Duration `koanf:"lease" desc:"Lease length of a claimed Job. Renewed while the Job runs."`
}

// GCConfig configures garbage collection.
type GCConfig struct {
	Schedule    string        `koanf:"schedule" desc:"Cron schedule (5 fields) of the gc Job."`
	GracePeriod time.Duration `koanf:"grace_period" desc:"Unreferenced LFS objects are deleted after this long."`
}

// ReposConfig configures the Repo lifecycle.
type ReposConfig struct {
	RestoreWindow time.Duration `koanf:"restore_window" desc:"How long a deleted Repo can be restored before it is purged."`
}

// ViewerConfig configures the Dataset Viewer and Parquet export.
type ViewerConfig struct {
	Enabled            bool          `koanf:"enabled" desc:"Turn on Parquet export and the Dataset Viewer."`
	MaxConvertBytes    ByteSize      `koanf:"max_convert_bytes" desc:"Splits larger than this get a partial Parquet export."`
	MaxScanRows        int           `koanf:"max_scan_rows" desc:"Rows scanned by one filter or search request before it returns partial results."`
	MaxScanTime        time.Duration `koanf:"max_scan_seconds" desc:"Time limit of one filter or search request before it returns partial results."`
	MaxCellBytes       ByteSize      `koanf:"max_cell_bytes" desc:"Cells larger than this are truncated in Viewer responses."`
	MaxRequestBytes    ByteSize      `koanf:"max_request_bytes" desc:"Decoded Parquet data one Dataset Viewer request may hold; requests that need more answer with an error, as the Hub's do."`
	ConvertConcurrency int           `koanf:"convert_concurrency" desc:"dataset.convert Jobs per worker."`
}

// TelemetryConfig configures OpenTelemetry. Standard OTEL_* variables win.
type TelemetryConfig struct {
	ServiceName      string `koanf:"service_name" desc:"service.name resource attribute."`
	Environment      string `koanf:"environment" desc:"deployment.environment.name resource attribute. Empty leaves it unset."`
	Traces           string `koanf:"traces" desc:"Trace exporter: otlp, console or none. otlp exports only when an OTLP endpoint is set with OTEL_EXPORTER_OTLP_ENDPOINT or OTEL_EXPORTER_OTLP_TRACES_ENDPOINT. Ignored when OTEL_TRACES_EXPORTER is set."`
	Metrics          string `koanf:"metrics" desc:"Metric exporter: otlp, prometheus, console or none. otlp exports only when an OTLP endpoint is set. Ignored when OTEL_METRICS_EXPORTER is set."`
	Logs             string `koanf:"logs" desc:"Log exporter: otlp, console or none. otlp exports only when an OTLP endpoint is set. Ignored when OTEL_LOGS_EXPORTER is set. Logs always go to stdout too."`
	PrometheusListen string `koanf:"prometheus_listen" desc:"Address of the Prometheus /metrics endpoint when metrics is prometheus."`
	LogFormat        string `koanf:"log_format" desc:"Stdout log format: json or text."`
	LogLevel         string `koanf:"log_level" desc:"Minimum log level: debug, info, warn or error."`
}

// Default returns the configuration with every default applied. It matches
// the YAML example in docs/spec/02-architecture.md.
func Default() Config {
	return Config{
		Server: ServerConfig{
			Listen:          ":8080",
			Role:            RoleAll,
			DataDir:         "/var/lib/hangar",
			TrustedProxies:  []string{},
			MaxRequestBody:  64 * MiB,
			ShutdownTimeout: 30 * time.Second,
			Timezone:        "UTC",
		},
		Database: DatabaseConfig{Driver: DriverSQLite, DSN: "/var/lib/hangar/hangar.db", MaxOpenConns: 20},
		Storage: StorageConfig{
			Backend: BackendFS,
			FS:      FSConfig{Path: "/var/lib/hangar/blobs"},
			S3: S3Config{
				Endpoint:     "s3.amazonaws.com",
				Region:       "eu-west-2",
				Bucket:       "hangar",
				DownloadMode: DownloadRedir,
				UploadMode:   UploadDirect,
				PresignTTL:   time.Hour,
				Retries:      5,
				StallTimeout: 2 * time.Minute,
				StartupWait:  5 * time.Minute,
			},
		},
		Security: SecurityConfig{
			Registration:   RegistrationClosed,
			LocalLogin:     true,
			SessionTTL:     720 * time.Hour,
			BootstrapAdmin: BootstrapConfig{Username: "hangar-admin"},
		},
		OIDC: OIDCConfig{Providers: []OIDCProvider{}},
		Upstreams: map[string]UpstreamConfig{
			"huggingface": {BaseURL: "https://huggingface.co", Enabled: true},
		},
		Pull: PullConfig{
			Concurrency:  4,
			Windows:      []string{},
			StallTimeout: 2 * time.Minute,
			PullThrough:  PullThroughConfig{Allow: []string{"*"}, MinRole: "user"},
			Dependencies: DependencyConfig{
				AutoPullCode: true,
				MaxDepth:     5,
			},
		},
		Git: GitConfig{
			MaxBlobSize:      10 * MiB,
			MaxPackSize:      2 * GiB,
			CompactThreshold: 20,
			PackGracePeriod:  time.Hour,
			CacheDir:         "/var/cache/hangar/git",
			CacheSize:        10 * GiB,
			Timeout:          time.Hour,
		},
		LFS: LFSConfig{
			MultipartThreshold: 64 * MiB,
			UploadURLTTL:       24 * time.Hour,
			VerifyWait:         10 * time.Minute,
		},
		HFAPI: HFAPIConfig{RegularUploadMax: 10 * MiB, Xet: XetIngest},
		Xet:   XetConfig{TokenTTL: time.Hour, XorbRetention: 672 * time.Hour, MaxFileSize: 50 * GiB},
		Jobs:  JobsConfig{Workers: 4, Lease: 60 * time.Second},
		GC:    GCConfig{Schedule: "0 3 * * *", GracePeriod: 168 * time.Hour},
		Repos: ReposConfig{RestoreWindow: 168 * time.Hour},
		Viewer: ViewerConfig{
			Enabled:            true,
			MaxConvertBytes:    5 * GiB,
			MaxScanRows:        5_000_000,
			MaxScanTime:        10 * time.Second,
			MaxCellBytes:       100 * KiB,
			MaxRequestBytes:    300_000_000,
			ConvertConcurrency: 1,
		},
		UI: UIConfig{DocsURL: DefaultDocsURL},
		Telemetry: TelemetryConfig{
			ServiceName:      "hangar",
			Traces:           "otlp",
			Metrics:          "otlp",
			Logs:             "otlp",
			PrometheusListen: ":9464",
			LogFormat:        "json",
			LogLevel:         "info",
		},
	}
}

// CASURL returns the Xet CAS URL: xet.cas_url, or <public_url>/xet-cas.
func (c *Config) CASURL() string {
	if c.Xet.CASURL != "" {
		return c.Xet.CASURL
	}
	return trimSlash(c.Server.PublicURL) + "/xet-cas"
}

func trimSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}
