package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/hangar-hub/hangar/internal/blob"
	"github.com/hangar-hub/hangar/internal/jobs"
	"github.com/hangar-hub/hangar/internal/model"
	"github.com/hangar-hub/hangar/internal/repo"
	"github.com/hangar-hub/hangar/internal/telemetry"
)

// BandwidthBudget is the jobs.Limiter budget name Pulls spend.
const BandwidthBudget = "pull"

// filePayload is the payload of a pull.file Job: one LFS object.
type filePayload struct {
	UpstreamID   uuid.UUID       `json:"upstream_id"`
	Upstream     string          `json:"upstream"`
	UpstreamRepo string          `json:"upstream_repo"`
	Type         model.RepoType  `json:"type"`
	Repo         string          `json:"repo"`
	RepoUUID     uuid.UUID       `json:"repo_uuid"`
	Commit       model.CommitSHA `json:"commit"`
	Path         string          `json:"path"`
	OID          string          `json:"oid"`
	Size         int64           `json:"size"`
	RequestedBy  uuid.UUID       `json:"requested_by,omitempty"`
	Now          bool            `json:"now,omitempty"`
}

// fileResult is the pull.file Job's result: the multipart upload to
// resume, and why the transfer failed when the Pull must know
// (reasonQuotaExceeded).
type fileResult struct {
	UploadID uuid.UUID `json:"upload_id,omitempty"`
	Reason   string    `json:"reason,omitempty"`
}

func upstreamAttrs(name, outcome string) metric.MeasurementOption {
	attrs := []attribute.KeyValue{attribute.String(telemetry.MetricUpstream, name)}
	if outcome != "" {
		attrs = append(attrs, attribute.String("outcome", outcome))
	}
	return metric.WithAttributes(attrs...)
}

// runFile is the pull.file Job handler (06 "Transfer"): stream one object
// from the Upstream into the Blob store, hashing as it goes, resuming
// from the parts already uploaded, inside pull windows and under the
// shared bandwidth cap.
func (s *Service) runFile(ctx context.Context, j *jobs.Job, prog jobs.Progress) error {
	var p filePayload
	if err := j.DecodePayload(&p); err != nil {
		return jobs.Permanent(err)
	}
	id, err := model.ParseRepoID(p.Type, p.Repo)
	if err != nil {
		return jobs.Permanent(err)
	}
	if !model.IsSHA256(p.OID) || p.Size < 0 {
		return jobs.Permanent(fmt.Errorf("invalid object %q", p.OID))
	}
	ns, err := s.repoNamespace(ctx, p.RepoUUID)
	if err != nil {
		return err
	}
	if done, err := s.markIfStored(ctx, id, ns, p.OID, p.Size); err != nil || done {
		if done {
			s.countFile(ctx, p.Upstream, "deduplicated")
		}
		return s.fileFailure(ctx, j, prog, p, err)
	}
	if !p.Now {
		if err := s.waitForWindow(ctx, prog); err != nil {
			return err
		}
	}
	// A Pull-through transfer of the same object in this process finishes first.
	if t := s.hub.lookup(p.OID); t != nil {
		_ = prog.Report(ctx, jobs.ProgressUpdate{BytesTotal: p.Size, FilesTotal: 1, Message: "Waiting for a transfer of the same file"})
		if err := t.wait(ctx); err == nil {
			if done, err := s.markIfStored(ctx, id, ns, p.OID, p.Size); err != nil || done {
				return s.fileFailure(ctx, j, prog, p, err)
			}
		}
	}
	up, err := s.opts.Registry.Get(ctx, p.UpstreamID)
	if err != nil {
		return err
	}
	if !up.Enabled {
		return jobs.Permanent(fmt.Errorf("%w: %s", ErrUpstreamDisabled, up.Name))
	}
	cred, err := s.credentials(ctx, up, p.RequestedBy)
	if err != nil {
		return err
	}
	upID, err := model.ParseRepoID(p.Type, p.UpstreamRepo)
	if err != nil {
		return jobs.Permanent(err)
	}
	// The object is not stored anywhere in the Instance, so all of it
	// counts towards the Namespace's usage.
	if err := s.checkQuota(ctx, ns, map[string]int64{p.OID: p.Size}, 0); err != nil {
		return s.fileFailure(ctx, j, prog, p, err)
	}
	pr := newProgressReporter(prog, p.Path, p.Size, s.opts.Now)
	src := fileSource{up: up, cred: cred, id: upID, commit: p.Commit, path: p.Path, oid: p.OID, size: p.Size}
	if p.Size <= s.opts.PartSize {
		err = s.ingestWhole(ctx, src, pr)
	} else {
		err = s.ingestMultipart(ctx, j, prog, src, pr, p.Now)
	}
	if err != nil {
		switch {
		case s.isSnooze(err):
		case errors.Is(err, ErrNotFound), errors.Is(err, ErrGated), errors.Is(err, ErrUnauthorized):
			s.countFile(ctx, p.Upstream, "failed")
			return jobs.Permanent(err)
		case j.Attempts >= j.MaxAttempts:
			s.countFile(ctx, p.Upstream, "failed")
		default:
			s.countFile(ctx, p.Upstream, "retry")
		}
		return err
	}
	if err := s.opts.Repos.SetLFSPresence(ctx, id, []repo.LFSRef{{OID: p.OID, Size: p.Size, Present: true}}); err != nil {
		return err
	}
	pr.flush(ctx, true)
	s.countFile(ctx, p.Upstream, "ok")
	return nil
}

// repoNamespace returns the Namespace of the Repo a transfer stores into,
// for quota checks. It returns uuid.Nil when no QuotaChecker is set.
func (s *Service) repoNamespace(ctx context.Context, repoUUID uuid.UUID) (uuid.UUID, error) {
	if s.opts.Quota == nil {
		return uuid.Nil, nil
	}
	r, err := s.opts.Repos.GetByUUID(ctx, repoUUID)
	if errors.Is(err, repo.ErrNotFound) {
		return uuid.Nil, jobs.Permanent(err)
	}
	if err != nil {
		return uuid.Nil, err
	}
	return r.NamespaceID, nil
}

// fileFailure ends a pull.file Job that the Namespace quota refused,
// recording the reason for its Pull; other errors pass through.
func (s *Service) fileFailure(ctx context.Context, j *jobs.Job, prog jobs.Progress, p filePayload, err error) error {
	if !errors.Is(err, ErrQuotaExceeded) {
		return err
	}
	var fr fileResult
	if len(j.Result) > 0 {
		_ = json.Unmarshal(j.Result, &fr)
	}
	fr.Reason = reasonQuotaExceeded
	if serr := prog.SetResult(ctx, fr); serr != nil {
		s.log.WarnContext(ctx, "record a refused transfer", "error", serr.Error())
	}
	_ = prog.Report(ctx, jobs.ProgressUpdate{BytesTotal: p.Size, FilesTotal: 1, Message: quotaMessage,
		Files: []jobs.FileProgress{{Path: p.Path, BytesTotal: p.Size, Status: fileFailed, Error: err.Error()}}})
	s.countFile(ctx, p.Upstream, "failed")
	return jobs.Permanent(err)
}

// isSnooze reports whether err is the error SnoozeFunc returns.
func (s *Service) isSnooze(err error) bool {
	var marker *snoozeMarker
	return errors.As(err, &marker)
}

// snoozeMarker wraps a SnoozeFunc error so handlers can tell it apart.
type snoozeMarker struct{ err error }

func (m *snoozeMarker) Error() string { return m.err.Error() }
func (m *snoozeMarker) Unwrap() error { return m.err }

func (s *Service) snooze(until time.Time, reason string) error {
	return &snoozeMarker{err: s.opts.Snooze(until, reason)}
}

// markIfStored marks the object present in the Repo if the Instance
// already holds it, once the Namespace ns has room for it: holding an
// object another Namespace stored still counts towards ns's quota.
func (s *Service) markIfStored(ctx context.Context, id model.RepoID, ns uuid.UUID, oid string, size int64) (bool, error) {
	o, err := s.opts.LFS.Stat(ctx, oid)
	if errors.Is(err, repo.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !o.Verified || o.Size != size {
		return false, nil
	}
	if err := s.checkQuota(ctx, ns, map[string]int64{oid: size}, 0); err != nil {
		return false, err
	}
	return true, s.opts.Repos.SetLFSPresence(ctx, id, []repo.LFSRef{{OID: oid, Size: size, Present: true}})
}

// waitForWindow snoozes the Job until the next pull window opens.
func (s *Service) waitForWindow(ctx context.Context, prog jobs.Progress) error {
	st, err := s.opts.Settings(ctx)
	if err != nil {
		return err
	}
	if len(st.Windows) == 0 {
		return nil
	}
	w, err := s.opts.ParseWindows(st.Windows)
	if err != nil {
		return jobs.Permanent(fmt.Errorf("pull.windows: %w", err))
	}
	now := s.opts.Now()
	if w.Open(now) {
		return nil
	}
	msg := w.WaitMessage()
	_ = prog.Report(ctx, jobs.ProgressUpdate{Message: msg})
	return s.snooze(w.NextOpen(now), msg)
}

type fileSource struct {
	up     *Upstream
	cred   Credentials
	id     model.RepoID
	commit model.CommitSHA
	path   string
	oid    string
	size   int64
}

// open starts a download of src from offset and checks its metadata.
func (s *Service) open(ctx context.Context, src fileSource, offset int64) (io.ReadCloser, error) {
	var rng *blob.Range
	if offset > 0 {
		rng = &blob.Range{Start: offset, End: -1}
	}
	rc, meta, err := s.opts.Client.Download(ctx, *src.up, src.cred, src.id, src.commit, src.path, rng)
	if err != nil {
		return nil, err
	}
	if meta.Size != 0 && meta.Size != src.size {
		_ = rc.Close()
		return nil, jobs.Permanent(fmt.Errorf("the file %s on the Upstream is %d bytes, not the %d its LFS pointer names", src.path, meta.Size, src.size))
	}
	if model.IsSHA256(meta.ETag) && meta.ETag != src.oid {
		_ = rc.Close()
		return nil, jobs.Permanent(fmt.Errorf("the file %s on the Upstream has sha256 %s, not the %s its LFS pointer names", src.path, meta.ETag, src.oid))
	}
	return rc, nil
}

// meter wraps a download: it spends the shared bandwidth budget, counts
// bytes for metrics and reports progress.
func (s *Service) meter(ctx context.Context, r io.Reader, upName string, onBytes func(int)) io.Reader {
	return &meteredReader{ctx: ctx, r: r, s: s, up: upName, onBytes: onBytes}
}

type meteredReader struct {
	ctx     context.Context
	r       io.Reader
	s       *Service
	up      string
	onBytes func(int)
}

// meterChunk bounds each read, so the limiter is asked often.
const meterChunk = 64 << 10

func (m *meteredReader) Read(p []byte) (int, error) {
	if len(p) > meterChunk {
		p = p[:meterChunk]
	}
	n, err := m.r.Read(p)
	if n > 0 {
		start := time.Now()
		if werr := m.s.opts.Limiter.WaitN(m.ctx, BandwidthBudget, int64(n)); werr != nil {
			return n, werr
		}
		in := m.s.opts.Instruments
		if in != nil {
			if waited := time.Since(start); waited > time.Millisecond && in.PullRateLimitedSeconds != nil {
				in.PullRateLimitedSeconds.Add(m.ctx, waited.Seconds(), upstreamAttrs(m.up, ""))
			}
			if in.PullBytes != nil {
				in.PullBytes.Add(m.ctx, int64(n), upstreamAttrs(m.up, ""))
			}
		}
		if m.onBytes != nil {
			m.onBytes(n)
		}
	}
	return n, err
}

// ingestWhole streams a small object into the LFS store in one go.
func (s *Service) ingestWhole(ctx context.Context, src fileSource, pr *progressReporter) error {
	rc, err := s.open(ctx, src, 0)
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()
	pr.set(0)
	_, err = s.opts.LFS.Ingest(ctx, src.oid, src.size, s.meter(ctx, rc, src.up.Name, func(n int) { pr.add(ctx, int64(n)) }))
	if errors.Is(err, repo.ErrChecksumMismatch) {
		return fmt.Errorf("the downloaded %s did not match its sha256 or size; it will be downloaded again: %w", src.path, err)
	}
	return err
}

// ingestMultipart streams a large object as a multipart upload of
// PartSize parts. The upload ID is kept in the Job's result, so a
// restarted Job (lease expiry, worker killed, pull window) resumes with an
// HTTP Range from the first missing part.
func (s *Service) ingestMultipart(ctx context.Context, j *jobs.Job, prog jobs.Progress, src fileSource, pr *progressReporter, now bool) error {
	var fr fileResult
	if len(j.Result) > 0 {
		_ = json.Unmarshal(j.Result, &fr)
	}
	var up *repo.Upload
	if fr.UploadID != uuid.Nil {
		u, err := s.opts.LFS.GetUpload(ctx, fr.UploadID)
		switch {
		case err == nil && u.OID == src.oid && u.Size == src.size:
			up = u
		case err != nil && !errors.Is(err, repo.ErrNotFound):
			return err
		}
	}
	if up == nil {
		u, err := s.opts.LFS.CreateUpload(ctx, repo.UploadParams{OID: src.oid, Size: src.size, Multipart: true, PartSize: s.opts.PartSize, TTL: 7 * 24 * time.Hour})
		if err != nil {
			return err
		}
		up = u
		if err := prog.SetResult(ctx, fileResult{UploadID: up.ID}); err != nil {
			return err
		}
	}
	done := append([]blob.Part(nil), up.Done...)
	sort.Slice(done, func(a, b int) bool { return done[a].Number < done[b].Number })
	var parts []blob.Part
	for _, d := range done {
		if d.Number != len(parts)+1 {
			break
		}
		parts = append(parts, d)
	}
	next := len(parts) + 1
	offset := int64(next-1) * up.PartSize
	pr.set(offset)
	if next <= up.PartCount {
		rc, err := s.open(ctx, src, offset)
		if err != nil {
			return err
		}
		defer func() { _ = rc.Close() }()
		body := s.meter(ctx, rc, src.up.Name, func(n int) { pr.add(ctx, int64(n)) })
		for part := next; part <= up.PartCount; part++ {
			if part > next && !now {
				pr.flush(ctx, true)
				if err := s.waitForWindow(ctx, prog); err != nil {
					return err
				}
			}
			size := min(up.PartSize, src.size-offset)
			etag, err := s.opts.LFS.PutPart(ctx, up.ID, part, io.LimitReader(body, size), size)
			if err != nil {
				return fmt.Errorf("upload part %d of %s: %w", part, src.path, err)
			}
			parts = append(parts, blob.Part{Number: part, ETag: etag, Size: size})
			offset += size
		}
	}
	if _, err := s.opts.LFS.CompleteUpload(ctx, up.ID, parts); err != nil {
		if errors.Is(err, repo.ErrChecksumMismatch) || errors.Is(err, repo.ErrNotFound) {
			_ = prog.SetResult(ctx, fileResult{})
			return fmt.Errorf("the downloaded %s did not match its sha256 or size; it will be downloaded again: %w", src.path, err)
		}
		return err
	}
	return nil
}

// progressReporter throttles progress reports to one a second and keeps
// a rate over the last few seconds.
type progressReporter struct {
	prog  jobs.Progress
	path  string
	total int64
	now   func() time.Time

	mu       sync.Mutex
	done     int64
	last     time.Time
	lastDone int64
	rate     float64
}

func newProgressReporter(prog jobs.Progress, path string, total int64, now func() time.Time) *progressReporter {
	return &progressReporter{prog: prog, path: path, total: total, now: now, last: now()}
}

func (p *progressReporter) set(done int64) {
	p.mu.Lock()
	p.done, p.lastDone = done, done
	p.mu.Unlock()
}

func (p *progressReporter) add(ctx context.Context, n int64) {
	p.mu.Lock()
	p.done += n
	p.mu.Unlock()
	p.flush(ctx, false)
}

func (p *progressReporter) flush(ctx context.Context, force bool) {
	p.mu.Lock()
	now := p.now()
	el := now.Sub(p.last)
	if !force && el < time.Second {
		p.mu.Unlock()
		return
	}
	if el > 0 {
		inst := float64(p.done-p.lastDone) / el.Seconds()
		if p.rate == 0 {
			p.rate = inst
		} else {
			p.rate = 0.5*p.rate + 0.5*inst
		}
	}
	p.last, p.lastDone = now, p.done
	u := jobs.ProgressUpdate{BytesDone: p.done, BytesTotal: p.total, FilesTotal: 1, Rate: p.rate,
		Files: []jobs.FileProgress{{Path: p.path, BytesDone: p.done, BytesTotal: p.total, Status: fileRunning}}}
	if p.done >= p.total {
		u.FilesDone = 1
		u.Files[0].Status = fileDone
	}
	p.mu.Unlock()
	_ = p.prog.Report(ctx, u)
}
