package telemetry

import (
	"errors"

	"go.opentelemetry.io/otel/metric"
)

// Instruments are Hangar's own metrics, from docs/spec/11-observability.md.
// Repo IDs are never metric attributes, because their cardinality is
// unbounded; put them on spans and logs instead.
type Instruments struct {
	DownloadBytes          metric.Int64Counter     // hangar.download.bytes {protocol, mode, repo.type}
	DownloadFiles          metric.Int64Counter     // hangar.download.files {protocol, repo.type}
	UploadBytes            metric.Int64Counter     // hangar.upload.bytes {protocol}
	PullBytes              metric.Int64Counter     // hangar.pull.bytes {upstream}
	PullRateLimitedSeconds metric.Float64Counter   // hangar.pull.rate_limited.duration {upstream}
	PullFiles              metric.Int64Counter     // hangar.pull.files {upstream, outcome}
	JobsQueued             metric.Int64Gauge       // hangar.jobs.queued {kind, status}
	JobsDuration           metric.Float64Histogram // hangar.jobs.duration {kind, outcome}
	JobsLeaseExpired       metric.Int64Counter     // hangar.jobs.lease.expired {kind}
	GitPush                metric.Int64Counter     // hangar.git.push {outcome}
	GitPackDuration        metric.Float64Histogram // hangar.git.pack.duration {service}
	GitCacheRequests       metric.Int64Counter     // hangar.git.cache.requests {outcome}
	GitCacheBytes          metric.Int64Gauge       // hangar.git.cache.bytes
	GitPacks               metric.Int64Gauge       // hangar.git.packs {state}
	XetXorbs               metric.Int64Counter     // hangar.xet.xorbs {outcome}
	XetReassemblyDuration  metric.Float64Histogram // hangar.xet.reassembly.duration {outcome}
	XetReassemblyBytes     metric.Int64Counter     // hangar.xet.reassembly.bytes
	BlobOperationDuration  metric.Float64Histogram // hangar.blob.operation.duration {backend, operation, outcome}
	StorageBytes           metric.Int64Gauge       // hangar.storage.bytes {kind}
	Repos                  metric.Int64Gauge       // hangar.repos {type, visibility, mirror}
	Users                  metric.Int64Gauge       // hangar.users
	AuthAttempts           metric.Int64Counter     // hangar.auth.attempts {method, outcome}
	WebhookDeliveries      metric.Int64Counter     // hangar.webhook.deliveries {outcome}
	IndexDuration          metric.Float64Histogram // hangar.index.duration {outcome}
	ScanFiles              metric.Int64Counter     // hangar.scan.files {status}
}

// Metric attribute keys used with the instruments.
const (
	MetricProtocol   = "protocol"
	MetricMode       = "mode"
	MetricRepoType   = "repo.type"
	MetricUpstream   = "upstream"
	MetricOutcome    = "outcome"
	MetricKind       = "kind"
	MetricStatus     = "status"
	MetricService    = "service"
	MetricState      = "state"
	MetricBackend    = "backend"
	MetricOperation  = "operation"
	MetricMethod     = "method"
	MetricType       = "type"
	MetricVisibility = "visibility"
	MetricMirror     = "mirror"
)

// durationBuckets suit operations from a millisecond to an hour.
var durationBuckets = []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 300, 900, 3600}

// NewInstruments creates every instrument on the meter.
func NewInstruments(m metric.Meter) (*Instruments, error) {
	var (
		i    Instruments
		errs []error
	)
	counter := func(name, unit, desc string) metric.Int64Counter {
		c, err := m.Int64Counter(name, metric.WithUnit(unit), metric.WithDescription(desc))
		errs = append(errs, err)
		return c
	}
	gauge := func(name, unit, desc string) metric.Int64Gauge {
		g, err := m.Int64Gauge(name, metric.WithUnit(unit), metric.WithDescription(desc))
		errs = append(errs, err)
		return g
	}
	hist := func(name, desc string) metric.Float64Histogram {
		h, err := m.Float64Histogram(name, metric.WithUnit("s"), metric.WithDescription(desc), metric.WithExplicitBucketBoundaries(durationBuckets...))
		errs = append(errs, err)
		return h
	}

	i.DownloadBytes = counter("hangar.download.bytes", "By", "Bytes downloaded by clients.")
	i.DownloadFiles = counter("hangar.download.files", "{file}", "Files downloaded by clients.")
	i.UploadBytes = counter("hangar.upload.bytes", "By", "Bytes uploaded by clients.")
	i.PullBytes = counter("hangar.pull.bytes", "By", "Bytes transferred from Upstreams by Pulls.")
	rl, err := m.Float64Counter("hangar.pull.rate_limited.duration", metric.WithUnit("s"), metric.WithDescription("Time Pulls spent waiting for the bandwidth limit."))
	errs = append(errs, err)
	i.PullRateLimitedSeconds = rl
	i.PullFiles = counter("hangar.pull.files", "{file}", "Files transferred by Pulls, by outcome.")
	i.JobsQueued = gauge("hangar.jobs.queued", "{job}", "Jobs by kind and status.")
	i.JobsDuration = hist("hangar.jobs.duration", "Job run time, by kind and outcome.")
	i.JobsLeaseExpired = counter("hangar.jobs.lease.expired", "{job}", "Jobs whose lease expired and went back to the queue.")
	i.GitPush = counter("hangar.git.push", "{push}", "git pushes, by outcome.")
	i.GitPackDuration = hist("hangar.git.pack.duration", "git pack generation and receive time.")
	i.GitCacheRequests = counter("hangar.git.cache.requests", "{request}", "Local pack cache lookups, by outcome.")
	i.GitCacheBytes = gauge("hangar.git.cache.bytes", "By", "Size of the local pack cache.")
	i.GitPacks = gauge("hangar.git.packs", "{pack}", "git packs by state, reported by compaction runs.")
	i.XetXorbs = counter("hangar.xet.xorbs", "{xorb}", "Xet xorb uploads, by outcome.")
	i.XetReassemblyDuration = hist("hangar.xet.reassembly.duration", "Time to reassemble Xet uploads into LFS objects.")
	i.XetReassemblyBytes = counter("hangar.xet.reassembly.bytes", "By", "Bytes reassembled from Xet uploads.")
	i.BlobOperationDuration = hist("hangar.blob.operation.duration", "Blob store operation time.")
	i.StorageBytes = gauge("hangar.storage.bytes", "By", "Stored bytes by kind, reported by GC runs.")
	i.Repos = gauge("hangar.repos", "{repo}", "Repos by type, visibility and Mirror state.")
	i.Users = gauge("hangar.users", "{user}", "Users.")
	i.AuthAttempts = counter("hangar.auth.attempts", "{attempt}", "Authentication attempts, by method and outcome.")
	i.WebhookDeliveries = counter("hangar.webhook.deliveries", "{delivery}", "Webhook deliveries, by outcome.")
	i.IndexDuration = hist("hangar.index.duration", "Index run time, by outcome.")
	i.ScanFiles = counter("hangar.scan.files", "{file}", "Scanned files, by status.")

	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return &i, nil
}
