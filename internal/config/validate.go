package config

import (
	"encoding/base64"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/jamesagarside/hangar/internal/model"
)

// InstanceKeyBytes is the decoded length of security.instance_key.
const InstanceKeyBytes = 32

var windowRe = regexp.MustCompile(`^([01]\d|2[0-3]):([0-5]\d)-([01]\d|2[0-3]):([0-5]\d)$`)

// CronParser parses the 5-field cron schedules used in configuration and
// the schedules table.
var CronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)

// Validate checks the whole configuration and returns Errors listing every
// problem, or nil.
func (c *Config) Validate() error { return c.validate(true) }

// ValidateBeforeSecrets checks the configuration as the server sees it before
// EnsureSecrets runs. It is Validate except that security.instance_key may be
// empty, because EnsureSecrets reads or generates it in server.data_dir. The
// server calls it before it creates anything, so a bad configuration reports
// every problem instead of a file system error.
func (c *Config) ValidateBeforeSecrets() error { return c.validate(false) }

func (c *Config) validate(requireInstanceKey bool) error {
	var e Errors

	// server
	if c.Server.PublicURL == "" {
		e.addf("server.public_url is required: set it to the URL clients use to reach Hangar, for example https://hangar.example.com")
	} else if u, err := url.Parse(c.Server.PublicURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
		e.addf("server.public_url %q must be an absolute http or https URL with no query", c.Server.PublicURL)
	}
	checkListen(&e, "server.listen", c.Server.Listen)
	switch c.Server.Role {
	case RoleAll, RoleWeb, RoleWorker:
	default:
		e.addf("server.role %q must be all, web or worker", c.Server.Role)
	}
	if c.Server.DataDir == "" {
		e.addf("server.data_dir is required")
	}
	for _, p := range c.Server.TrustedProxies {
		if _, err := ParsePrefix(p); err != nil {
			e.addf("server.trusted_proxies: %q is not a CIDR range or IP address", p)
		}
	}
	positiveSize(&e, "server.max_request_body", c.Server.MaxRequestBody)
	positiveDuration(&e, "server.shutdown_timeout", c.Server.ShutdownTimeout)
	if _, err := time.LoadLocation(c.Server.Timezone); err != nil {
		e.addf("server.timezone %q is not an IANA time zone", c.Server.Timezone)
	}

	// database
	switch c.Database.Driver {
	case DriverSQLite:
		if c.Server.Role != RoleAll && c.Server.Role != "" {
			e.addf("server.role %q needs database.driver postgres: SQLite supports one process with role all", c.Server.Role)
		}
		if c.Database.DSN == "" {
			e.addf("database.dsn is required: the SQLite database file path")
		}
	case DriverPostgres:
		if !strings.HasPrefix(c.Database.DSN, "postgres://") && !strings.HasPrefix(c.Database.DSN, "postgresql://") {
			e.addf("database.dsn must be a postgres:// URL when database.driver is postgres")
		}
	default:
		e.addf("database.driver %q must be sqlite or postgres", c.Database.Driver)
	}
	if c.Database.MaxOpenConns < 1 {
		e.addf("database.max_open_conns must be at least 1")
	}

	// storage
	switch c.Storage.Backend {
	case BackendFS:
		if c.Storage.FS.Path == "" {
			e.addf("storage.fs.path is required when storage.backend is fs")
		}
	case BackendS3:
		if c.Storage.S3.Endpoint == "" {
			e.addf("storage.s3.endpoint is required when storage.backend is s3")
		}
		if c.Storage.S3.Bucket == "" {
			e.addf("storage.s3.bucket is required when storage.backend is s3")
		}
		if (c.Storage.S3.AccessKeyID == "") != (c.Storage.S3.SecretAccessKey == "") {
			e.addf("storage.s3.access_key_id and storage.s3.secret_access_key must be set together")
		}
	default:
		e.addf("storage.backend %q must be fs or s3", c.Storage.Backend)
	}
	switch c.Storage.S3.DownloadMode {
	case DownloadRedir, DownloadProxy:
	default:
		e.addf("storage.s3.download_mode %q must be redirect or proxy", c.Storage.S3.DownloadMode)
	}
	switch c.Storage.S3.UploadMode {
	case UploadDirect, UploadProxy:
	default:
		e.addf("storage.s3.upload_mode %q must be direct or proxy", c.Storage.S3.UploadMode)
	}
	if c.Storage.S3.PresignTTL < time.Minute || c.Storage.S3.PresignTTL > 7*24*time.Hour {
		e.addf("storage.s3.presign_ttl must be between 1m and 168h (the S3 limit)")
	}
	if c.Storage.S3.Retries < 0 || c.Storage.S3.Retries > 20 {
		e.addf("storage.s3.retries must be between 0 and 20")
	}
	if c.Storage.S3.StallTimeout < 10*time.Second {
		e.addf("storage.s3.stall_timeout must be at least 10s")
	}
	if c.Storage.S3.StartupWait < 0 || c.Storage.S3.StartupWait > time.Hour {
		e.addf("storage.s3.startup_wait must be between 0 and 1h")
	}

	// security
	if c.Security.InstanceKey == "" {
		if requireInstanceKey {
			e.addf("security.instance_key is required: set a base64-encoded 32-byte key, or leave it empty on first start so Hangar generates one in server.data_dir")
		}
	} else if _, err := c.InstanceKeyBytes(); err != nil {
		e.addf("security.instance_key: %v", err)
	}
	switch c.Security.Registration {
	case RegistrationOpen, RegistrationInvite, RegistrationClosed:
	default:
		e.addf("security.registration %q must be open, invite or closed", c.Security.Registration)
	}
	positiveDuration(&e, "security.session_ttl", c.Security.SessionTTL)
	if c.Security.BootstrapAdmin.Username != "" {
		// The bootstrap admin gets a personal Namespace, so a reserved name
		// such as admin would be shadowed by Hangar's own routes.
		if err := model.ValidateNewNamespaceName(c.Security.BootstrapAdmin.Username); err != nil {
			e.addf("security.bootstrap_admin.username: %v", err)
		}
	}
	if c.Security.BootstrapAdmin.Password != "" && c.Security.BootstrapAdmin.PasswordFile != "" {
		e.addf("security.bootstrap_admin: set password or password_file, not both")
	}
	if !c.Security.LocalLogin && len(c.OIDC.Providers) == 0 {
		e.addf("security.local_login is false but no oidc.providers are configured, so nobody could sign in")
	}

	// oidc
	seen := map[string]bool{}
	for i, p := range c.OIDC.Providers {
		where := fmt.Sprintf("oidc.providers[%d]", i)
		switch {
		case p.ID == "":
			e.addf("%s.id is required", where)
		case !regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`).MatchString(p.ID):
			e.addf("%s.id %q may only contain lower-case letters, digits and '-'", where, p.ID)
		case seen[p.ID]:
			e.addf("%s.id %q is used twice", where, p.ID)
		}
		seen[p.ID] = true
		if p.Name == "" {
			e.addf("%s.name is required", where)
		}
		checkURL(&e, where+".issuer", p.Issuer, true)
		if p.ClientID == "" {
			e.addf("%s.client_id is required", where)
		}
		for j, m := range p.OrgMappings {
			mw := fmt.Sprintf("%s.org_mappings[%d]", where, j)
			if m.Group == "" {
				e.addf("%s.group is required", mw)
			}
			if err := model.ValidateNamespaceName(m.Org); err != nil {
				e.addf("%s.org: %v", mw, err)
			}
			if _, err := model.ParseRole(m.Role); err != nil {
				e.addf("%s.role: %v", mw, err)
			}
		}
	}

	// upstreams
	for id, u := range c.Upstreams {
		where := "upstreams." + id
		if !regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`).MatchString(id) {
			e.addf("%s: upstream IDs may only contain lower-case letters, digits and '-'", where)
		}
		checkURL(&e, where+".base_url", u.BaseURL, true)
		checkURL(&e, where+".viewer_url", u.ViewerURL, false)
		if u.NamespacePrefix != "" && !regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9\-_.]*$`).MatchString(u.NamespacePrefix) {
			e.addf("%s.namespace_prefix %q may only contain letters, digits, '-', '_' and '.'", where, u.NamespacePrefix)
		}
	}

	// pull
	if c.Pull.BandwidthLimit < 0 {
		e.addf("pull.bandwidth_limit cannot be negative")
	}
	if c.Pull.Concurrency < 1 {
		e.addf("pull.concurrency must be at least 1")
	}
	for _, w := range c.Pull.Windows {
		if !windowRe.MatchString(w) {
			e.addf("pull.windows: %q must have the form HH:MM-HH:MM", w)
		}
	}
	for _, g := range c.Pull.PullThrough.Allow {
		if _, err := path.Match(g, "x/y"); err != nil {
			e.addf("pull.pull_through.allow: %q is not a valid glob", g)
		}
	}
	switch c.Pull.PullThrough.MinRole {
	case "anonymous", "user":
	default:
		e.addf("pull.pull_through.min_role %q must be anonymous or user", c.Pull.PullThrough.MinRole)
	}
	if c.Pull.Dependencies.MaxDepth < 0 || c.Pull.Dependencies.MaxDepth > 50 {
		e.addf("pull.dependencies.max_depth must be between 0 and 50")
	}

	// git
	positiveSize(&e, "git.max_blob_size", c.Git.MaxBlobSize)
	positiveSize(&e, "git.max_pack_size", c.Git.MaxPackSize)
	if c.Git.CompactThreshold < 2 {
		e.addf("git.compact_threshold must be at least 2")
	}
	positiveDuration(&e, "git.pack_grace_period", c.Git.PackGracePeriod)
	if c.Git.CacheDir == "" {
		e.addf("git.cache_dir is required")
	}
	positiveSize(&e, "git.cache_size", c.Git.CacheSize)
	positiveDuration(&e, "git.timeout", c.Git.Timeout)

	// lfs
	if c.LFS.MultipartThreshold < 5*MiB {
		e.addf("lfs.multipart_threshold must be at least 5MiB, the smallest S3 part size")
	}
	positiveDuration(&e, "lfs.upload_url_ttl", c.LFS.UploadURLTTL)
	positiveDuration(&e, "lfs.verify_wait", c.LFS.VerifyWait)

	// hfapi
	positiveSize(&e, "hfapi.regular_upload_max", c.HFAPI.RegularUploadMax)
	switch c.HFAPI.Xet {
	case XetIngest, XetReject:
	default:
		e.addf("hfapi.xet %q must be ingest or reject", c.HFAPI.Xet)
	}
	if c.HFAPI.DefaultNamespace != "" {
		if err := model.ValidateNamespaceName(c.HFAPI.DefaultNamespace); err != nil {
			e.addf("hfapi.default_namespace: %v", err)
		}
	}

	// xet
	checkURL(&e, "xet.cas_url", c.Xet.CASURL, false)
	if c.Xet.TokenTTL < time.Minute {
		e.addf("xet.token_ttl must be at least 1m, because hf_xet refreshes 30 seconds before expiry")
	}
	if c.Xet.XorbRetention < 0 {
		e.addf("xet.xorb_retention cannot be negative")
	}
	positiveSize(&e, "xet.max_file_size", c.Xet.MaxFileSize)

	// jobs and gc
	if c.Jobs.Workers < 1 {
		e.addf("jobs.workers must be at least 1")
	}
	if c.Jobs.Lease < 5*time.Second {
		e.addf("jobs.lease must be at least 5s")
	}
	if _, err := CronParser.Parse(c.GC.Schedule); err != nil {
		e.addf("gc.schedule %q is not a valid 5-field cron schedule: %v", c.GC.Schedule, err)
	}
	if c.GC.GracePeriod < 0 {
		e.addf("gc.grace_period cannot be negative")
	}
	if c.Repos.RestoreWindow < 0 {
		e.addf("repos.restore_window cannot be negative")
	}

	// viewer
	positiveSize(&e, "viewer.max_convert_bytes", c.Viewer.MaxConvertBytes)
	if c.Viewer.MaxScanRows < 1 {
		e.addf("viewer.max_scan_rows must be at least 1")
	}
	positiveDuration(&e, "viewer.max_scan_seconds", c.Viewer.MaxScanTime)
	positiveSize(&e, "viewer.max_cell_bytes", c.Viewer.MaxCellBytes)
	positiveSize(&e, "viewer.max_request_bytes", c.Viewer.MaxRequestBytes)
	if c.Viewer.ConvertConcurrency < 1 {
		e.addf("viewer.convert_concurrency must be at least 1")
	}

	// telemetry
	if c.Telemetry.ServiceName == "" {
		e.addf("telemetry.service_name is required")
	}
	oneOf(&e, "telemetry.traces", c.Telemetry.Traces, "otlp", "console", "none")
	oneOf(&e, "telemetry.metrics", c.Telemetry.Metrics, "otlp", "prometheus", "console", "none")
	oneOf(&e, "telemetry.logs", c.Telemetry.Logs, "otlp", "console", "none")
	oneOf(&e, "telemetry.log_format", c.Telemetry.LogFormat, "json", "text")
	oneOf(&e, "telemetry.log_level", c.Telemetry.LogLevel, "debug", "info", "warn", "error")
	if c.Telemetry.Metrics == "prometheus" {
		checkListen(&e, "telemetry.prometheus_listen", c.Telemetry.PrometheusListen)
	}

	if len(e) == 0 {
		return nil
	}
	return e
}

// InstanceKeyBytes decodes security.instance_key.
func (c *Config) InstanceKeyBytes() ([]byte, error) {
	b, err := base64.StdEncoding.DecodeString(c.Security.InstanceKey)
	if err != nil {
		b, err = base64.RawStdEncoding.DecodeString(c.Security.InstanceKey)
	}
	if err != nil {
		return nil, fmt.Errorf("not valid base64")
	}
	if len(b) != InstanceKeyBytes {
		return nil, fmt.Errorf("decodes to %d bytes, want %d", len(b), InstanceKeyBytes)
	}
	return b, nil
}

// ParsePrefix parses a CIDR range or a single IP address.
func ParsePrefix(s string) (netip.Prefix, error) {
	if strings.Contains(s, "/") {
		p, err := netip.ParsePrefix(s)
		return p.Masked(), err
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Prefix{}, err
	}
	return netip.PrefixFrom(a, a.BitLen()), nil
}

func checkListen(e *Errors, key, addr string) {
	if _, _, err := net.SplitHostPort(addr); err != nil {
		e.addf("%s %q must be host:port or :port", key, addr)
	}
}

func checkURL(e *Errors, key, raw string, required bool) {
	if raw == "" {
		if required {
			e.addf("%s is required", key)
		}
		return
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		e.addf("%s %q must be an absolute http or https URL", key, raw)
	}
}

func positiveSize(e *Errors, key string, v ByteSize) {
	if v <= 0 {
		e.addf("%s must be greater than 0", key)
	}
}

func positiveDuration(e *Errors, key string, v time.Duration) {
	if v <= 0 {
		e.addf("%s must be greater than 0", key)
	}
}

func oneOf(e *Errors, key, v string, allowed ...string) {
	for _, a := range allowed {
		if v == a {
			return
		}
	}
	e.addf("%s %q must be one of %s", key, v, strings.Join(allowed, ", "))
}
