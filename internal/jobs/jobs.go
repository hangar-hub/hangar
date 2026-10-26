// Package jobs is the background Job system: a queue in the database
// (claim, leases, deduplication, retries), the handler registry, recurring
// schedules and the shared Pull rate limiter (docs/spec/02-architecture.md
// "Jobs"). jobstest has a fake Queue that runs handlers synchronously.
package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
)

// JobID identifies a Job.
type JobID = uuid.UUID

// Kind names what a Job does.
type Kind string

// Job kinds from 02-architecture.md.
const (
	KindPull           Kind = "pull"
	KindPullFile       Kind = "pull.file"
	KindMirrorSync     Kind = "mirror.sync"
	KindIndex          Kind = "index"
	KindScan           Kind = "scan"
	KindGC             Kind = "gc"
	KindGitCompact     Kind = "git.compact"
	KindLFSVerify      Kind = "lfs.verify"
	KindDatasetConvert Kind = "dataset.convert"
	KindDatasetStats   Kind = "dataset.stats"
	KindXetGC          Kind = "xet.gc"
	KindWebhookDeliver Kind = "webhook.deliver"
	KindZipExport      Kind = "zip.export"
	KindRepoPurge      Kind = "repo.purge"
	KindUploadExpire   Kind = "upload.expire"
	// KindAuthCleanup removes expired sessions, device codes, OIDC states,
	// Ollama nonces and sign-in throttle counters (internal/auth).
	KindAuthCleanup Kind = "auth.cleanup"
)

// AllKinds lists every Job kind.
func AllKinds() []Kind {
	return []Kind{KindPull, KindPullFile, KindMirrorSync, KindIndex, KindScan, KindGC, KindGitCompact,
		KindLFSVerify, KindDatasetConvert, KindDatasetStats, KindXetGC, KindWebhookDeliver, KindZipExport,
		KindRepoPurge, KindUploadExpire, KindAuthCleanup}
}

// Status is a Job's state.
type Status string

// Job statuses.
const (
	StatusPending   Status = "pending"
	StatusRunning   Status = "running"
	StatusSucceeded Status = "succeeded"
	StatusFailed    Status = "failed"
	StatusCancelled Status = "cancelled"
)

// Active reports whether the Job may still run.
func (s Status) Active() bool { return s == StatusPending || s == StatusRunning }

// Priorities. Higher runs first.
const (
	PriorityLow    = -10
	PriorityNormal = 0
	PriorityHigh   = 10
)

// NewJob describes a Job to enqueue.
type NewJob struct {
	Kind Kind
	// DedupeKey makes (Kind, DedupeKey) unique while a Job is pending or
	// running; enqueuing a duplicate returns the existing Job's ID.
	DedupeKey string
	// Payload is marshalled to JSON.
	Payload any
	// Priority orders claims; higher first.
	Priority int
	// RunAfter delays the Job; zero means now.
	RunAfter time.Time
	// MaxAttempts caps retries; zero uses the handler's default.
	MaxAttempts int
	// CreatedBy is the user who started the Job, for "my Jobs" listings.
	CreatedBy uuid.UUID
	// ParentID links child Jobs, such as pull.file Jobs of a pull.
	ParentID uuid.UUID
	// Repo is the UUID of the Repo a Repo-scoped Job works on: zip export,
	// Pull and pull.file, mirror.sync, index, scan, git.compact,
	// lfs.verify, dataset.convert and dataset.stats, and any other Job
	// whose progress or result reveals the Repo's content. The native API
	// shows such a Job only to callers who can still read that Repo, within
	// their Token's restrictions, and lets only callers who can write to it
	// cancel or retry it. uuid.Nil marks an Instance-wide Job (gc, xet.gc,
	// upload.expire), which only Instance admins with an unrestricted
	// credential see unless they started it.
	Repo uuid.UUID
}

// FileProgress is per-file progress for Pulls.
type FileProgress struct {
	Path       string `json:"path"`
	BytesDone  int64  `json:"bytes_done"`
	BytesTotal int64  `json:"bytes_total"`
	Status     string `json:"status"`
	Error      string `json:"error,omitempty"`
}

// ProgressUpdate is what a handler reports while it runs. The UI polls it
// through the native API.
type ProgressUpdate struct {
	BytesDone  int64 `json:"bytes_done"`
	BytesTotal int64 `json:"bytes_total"`
	FilesDone  int   `json:"files_done"`
	FilesTotal int   `json:"files_total"`
	// Rate is bytes per second over the last few seconds.
	Rate float64 `json:"rate"`
	// Message is a short status, such as "Waiting for pull window (01:00-07:00)".
	Message string         `json:"message,omitempty"`
	Files   []FileProgress `json:"files,omitempty"`
}

// Job is a queued, running or finished Job.
type Job struct {
	ID          JobID
	Kind        Kind
	DedupeKey   string
	Payload     json.RawMessage
	Status      Status
	Priority    int
	Attempts    int
	MaxAttempts int
	RunAfter    time.Time
	Progress    ProgressUpdate
	Result      json.RawMessage
	Error       string
	CreatedBy   uuid.UUID
	ParentID    uuid.UUID
	Repo        uuid.UUID
	CreatedAt   time.Time
	UpdatedAt   time.Time
	StartedAt   *time.Time
	FinishedAt  *time.Time
}

// DecodePayload unmarshals the Job's payload into v.
func (j *Job) DecodePayload(v any) error {
	if err := json.Unmarshal(j.Payload, v); err != nil {
		return fmt.Errorf("decode %s job payload: %w", j.Kind, err)
	}
	return nil
}

// Progress lets a running handler report progress and its result. Report
// also renews the Job's lease, so long-running handlers should call it at
// least every few seconds.
type Progress interface {
	Report(ctx context.Context, u ProgressUpdate) error
	// SetResult records the Job's JSON result, such as a zip download URL.
	SetResult(ctx context.Context, v any) error
}

// Handler runs one Job. Handlers must be idempotent: a Job whose lease
// expires runs again. A run that lost its Job that way keeps going until its
// next Progress call, which returns ErrLeaseLost and cancels its context; its
// final result is discarded. Return a Permanent error to fail without
// retrying.
type Handler func(ctx context.Context, j *Job, progress Progress) error

// Filter selects Jobs for List.
type Filter struct {
	Kinds     []Kind
	Statuses  []Status
	CreatedBy uuid.UUID
	ParentID  uuid.UUID
	Cursor    string
	Limit     int
}

// Schedule is a recurring Job (the schedules table).
type Schedule struct {
	Kind Kind
	// Key identifies the schedule within its kind, such as a Mirror's Repo UUID.
	Key string
	// Cron is a 5-field cron expression in server.timezone.
	Cron    string
	Payload any
	Enabled bool
}

// Errors.
var (
	ErrNotFound    = errors.New("job not found")
	ErrNotActive   = errors.New("job is not pending or running")
	ErrNotRetrying = errors.New("only failed or cancelled jobs can be retried")
	ErrNoHandler   = errors.New("no handler registered for job kind")
)

// Queue is the Job queue.
type Queue interface {
	// Enqueue adds a Job, or returns the ID of the pending or running Job
	// with the same kind and dedupe key.
	Enqueue(ctx context.Context, j NewJob) (JobID, error)
	// Cancel stops a pending Job, or asks a running one to stop.
	Cancel(ctx context.Context, id JobID) error
	// Get returns a Job.
	Get(ctx context.Context, id JobID) (*Job, error)
	// List returns Jobs, newest first, and the next cursor.
	List(ctx context.Context, f Filter) ([]*Job, string, error)
	// Retry enqueues a failed or cancelled Job again and returns the new ID.
	Retry(ctx context.Context, id JobID) (JobID, error)
	// Postpone moves a pending Job's earliest start to until, when it is
	// due before then. A Job that is running, has finished or is already
	// due later is left alone.
	Postpone(ctx context.Context, id JobID, until time.Time) error
	// SetSchedule creates or replaces a recurring Job.
	SetSchedule(ctx context.Context, s Schedule) error
	// DeleteSchedule removes a recurring Job.
	DeleteSchedule(ctx context.Context, kind Kind, key string) error
}

// Limiter shares a bandwidth budget across every worker of the Instance
// (PULL-7). WaitN blocks until n bytes may be transferred under the named
// budget, or ctx ends.
type Limiter interface {
	WaitN(ctx context.Context, name string, n int64) error
}

// permanentError marks errors that must not be retried.
type permanentError struct{ err error }

func (e *permanentError) Error() string { return e.err.Error() }
func (e *permanentError) Unwrap() error { return e.err }

// Permanent wraps err so the worker fails the Job without retrying.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return &permanentError{err: err}
}

// IsPermanent reports whether err was wrapped with Permanent.
func IsPermanent(err error) bool {
	var p *permanentError
	return errors.As(err, &p)
}

// HandlerOptions configure how a kind runs.
type HandlerOptions struct {
	// MaxAttempts is the default for Jobs of this kind. Zero means 5.
	MaxAttempts int
	// Concurrency limits Jobs of this kind per worker process. Zero means
	// no limit beyond jobs.workers.
	Concurrency int
	// Timeout bounds one run. Zero means no limit.
	Timeout time.Duration
	// Backoff returns the delay before attempt n+1. Nil means exponential
	// from 30 seconds, capped at one hour.
	Backoff func(attempt int) time.Duration
}

// DefaultBackoff doubles from 30 seconds, capped at one hour.
func DefaultBackoff(attempt int) time.Duration {
	d := 30 * time.Second
	for range max(0, attempt-1) {
		d *= 2
		if d >= time.Hour {
			return time.Hour
		}
	}
	return d
}

// Registration is a handler and its options.
type Registration struct {
	Kind    Kind
	Handler Handler
	Options HandlerOptions
}

// Registry maps Job kinds to handlers. Workers only claim kinds that have
// a handler. It is safe for concurrent use.
type Registry struct {
	mu       sync.RWMutex
	handlers map[Kind]Registration
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry { return &Registry{handlers: map[Kind]Registration{}} }

// Register adds a handler. It returns an error if the kind already has one.
func (r *Registry) Register(kind Kind, h Handler, opts HandlerOptions) error {
	if h == nil {
		return fmt.Errorf("register %s: nil handler", kind)
	}
	if opts.MaxAttempts == 0 {
		opts.MaxAttempts = 5
	}
	if opts.Backoff == nil {
		opts.Backoff = DefaultBackoff
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.handlers[kind]; ok {
		return fmt.Errorf("register %s: a handler is already registered", kind)
	}
	r.handlers[kind] = Registration{Kind: kind, Handler: h, Options: opts}
	return nil
}

// Lookup returns the registration of a kind.
func (r *Registry) Lookup(kind Kind) (Registration, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	reg, ok := r.handlers[kind]
	return reg, ok
}

// Kinds returns the registered kinds in order.
func (r *Registry) Kinds() []Kind {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Kind, 0, len(r.handlers))
	for k := range r.handlers {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
