package scan

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/jamesagarside/hangar/internal/blob"
	"github.com/jamesagarside/hangar/internal/db"
	"github.com/jamesagarside/hangar/internal/index"
	"github.com/jamesagarside/hangar/internal/index/gguf"
	"github.com/jamesagarside/hangar/internal/index/rangeio"
	"github.com/jamesagarside/hangar/internal/index/safetensors"
	"github.com/jamesagarside/hangar/internal/jobs"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/repo"
	"github.com/jamesagarside/hangar/internal/telemetry"
	"github.com/jamesagarside/hangar/internal/webhook"
)

// Version is the scanner version; results are keyed by it.
const Version = 1

// commitRow is the path of the row that records a Commit's overall
// status, so a Commit with nothing to scan still has results.
const commitRow = ""

// Options configure the Service.
type Options struct {
	DB    *db.DB
	Repos repo.Service
	// LFS checks that each LFS pointer's size matches its stored object;
	// optional.
	LFS repo.LFSStore
	// Webhooks receives scan.completed events; optional.
	Webhooks webhook.Emitter
	// Instruments records hangar.scan.files; optional.
	Instruments *telemetry.Instruments
	Log         *slog.Logger
	// Window is the ranged read size; zero means 1 MiB.
	Window int64
}

// Service is the database-backed Scanner.
type Service struct {
	o Options
}

var _ Scanner = (*Service)(nil)

// New returns a Service.
func New(o Options) *Service {
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	return &Service{o: o}
}

// ScanFile implements Scanner. It reads r as a stream; the path's
// extension decides the checks.
func (s *Service) ScanFile(ctx context.Context, p string, r io.Reader) (FileResult, error) {
	fr := FileResult{Path: p, Status: StatusSafe}
	switch KindOf(p) {
	case KindPickle:
		scanPickleStream(&fr, r)
	case KindJoblib:
		scanJoblib(&fr, r)
	case KindNumpy:
		scanNumpy(&fr, r)
	case KindSafetensors, KindGGUF:
		b, err := io.ReadAll(r)
		if err != nil {
			return FileResult{}, err
		}
		checkIntegrity(&fr, KindOf(p), bytes.NewReader(b), int64(len(b)))
	}
	sortFindings(&fr)
	return fr, ctx.Err()
}

// checkIntegrity applies the safetensors and GGUF rules in 07.
func checkIntegrity(fr *FileResult, k Kind, ra io.ReaderAt, size int64) {
	var err error
	switch k {
	case KindSafetensors:
		var h *safetensors.Header
		if h, err = safetensors.Read(ra, size); err == nil {
			err = h.Validate(size)
		}
	case KindGGUF:
		var f *gguf.File
		if f, err = gguf.Read(ra, size); err == nil {
			err = f.Validate(size)
		}
	}
	if err != nil {
		fr.Findings = append(fr.Findings, Finding{Status: StatusError, Detail: err.Error()})
		fail(fr, err)
	}
}

func (s *Service) open(ctx context.Context, id model.RepoID, commit model.CommitSHA, p string) rangeio.OpenFunc {
	return func(ctx context.Context, start, end int64) (io.ReadCloser, error) {
		rc, _, err := s.o.Repos.OpenFile(ctx, id, commit, p, &blob.Range{Start: start, End: end})
		return rc, err
	}
}

// scanEntry scans one file of a Commit, using ranged reads where they
// avoid reading the whole file.
func (s *Service) scanEntry(ctx context.Context, id model.RepoID, commit model.CommitSHA, e model.Entry, siblings map[string]bool) (FileResult, bool, error) {
	k := KindOf(e.Path)
	if k == KindPickle && strings.EqualFold(path.Ext(e.Path), ".bin") && siblings[strings.TrimSuffix(e.Path, path.Ext(e.Path))+".xml"] {
		// OpenVINO weights: raw numbers beside an .xml graph.
		k = KindNone
	}
	if k == KindNone && e.LFS == nil {
		return FileResult{}, false, nil
	}
	fr := FileResult{Path: e.Path, Status: StatusSafe}
	if e.LFS != nil && s.o.LFS != nil {
		obj, err := s.o.LFS.Stat(ctx, e.LFS.OID)
		switch {
		case errors.Is(err, repo.ErrNotFound) || errors.Is(err, blob.ErrNotFound):
			msg := "the LFS object is missing from the Blob store"
			fr.Findings = append(fr.Findings, Finding{Status: StatusError, Detail: msg})
			fail(&fr, errors.New(msg))
			return fr, true, nil
		case err != nil:
			return FileResult{}, false, err
		case obj.Size != e.LFS.Size:
			msg := fmt.Sprintf("the LFS pointer says %d bytes but the stored object has %d", e.LFS.Size, obj.Size)
			fr.Findings = append(fr.Findings, Finding{Status: StatusError, Detail: msg})
			fail(&fr, errors.New(msg))
		}
	}
	if k == KindNone {
		return fr, fr.Status != StatusSafe, nil
	}
	ra := rangeio.New(ctx, e.Size, s.o.Window, s.open(ctx, id, commit, e.Path))
	switch k {
	case KindSafetensors, KindGGUF:
		checkIntegrity(&fr, k, ra, e.Size)
	case KindPickle:
		head := make([]byte, 4)
		n, _ := ra.ReadAt(head, 0)
		if n == 4 && (bytes.Equal(head, zipMagic) || bytes.Equal(head, emptyZipMagic)) {
			scanZipAt(&fr, ra, e.Size)
			break
		}
		fallthrough
	default:
		rc, _, err := s.o.Repos.OpenFile(ctx, id, commit, e.Path, nil)
		if err != nil {
			return FileResult{}, false, err
		}
		r := bufio.NewReaderSize(rc, 256<<10)
		switch k {
		case KindJoblib:
			scanJoblib(&fr, r)
		case KindNumpy:
			scanNumpy(&fr, r)
		default:
			scanPickleStream(&fr, r)
		}
		_ = rc.Close()
	}
	if err := ctx.Err(); err != nil {
		return FileResult{}, false, err
	}
	sortFindings(&fr)
	return fr, true, nil
}

// ScanCommit implements Scanner.
func (s *Service) ScanCommit(ctx context.Context, repoUUID uuid.UUID, id model.RepoID, commit model.CommitSHA) (*CommitResult, error) {
	entries, err := s.o.Repos.Tree(ctx, id, commit, "", repo.TreeOpts{Recursive: true})
	if err != nil {
		return nil, err
	}
	siblings := map[string]bool{}
	for _, e := range entries {
		siblings[e.Path] = true
	}
	res := &CommitResult{Status: StatusSafe, Files: []FileResult{}}
	for _, e := range entries {
		if e.Type != model.EntryFile || !e.Present {
			continue
		}
		fr, ok, err := s.scanEntry(ctx, id, commit, e, siblings)
		if err != nil {
			return nil, fmt.Errorf("scan %s: %w", e.Path, err)
		}
		if !ok {
			continue
		}
		res.Files = append(res.Files, fr)
		res.Status = Worst(res.Status, fr.Status)
		if s.o.Instruments != nil && s.o.Instruments.ScanFiles != nil {
			s.o.Instruments.ScanFiles.Add(ctx, 1, metric.WithAttributes(attribute.String(telemetry.MetricStatus, string(fr.Status))))
		}
	}
	if err := s.store(ctx, repoUUID, commit, res); err != nil {
		return nil, err
	}
	return res, nil
}

type storedFile struct {
	Findings []Finding `json:"findings"`
	Error    string    `json:"error,omitempty"`
}

func (s *Service) store(ctx context.Context, repoUUID uuid.UUID, commit model.CommitSHA, res *CommitResult) error {
	return s.o.DB.InTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.NewDelete().Model((*db.ScanResult)(nil)).Where("repo_id = ?", repoUUID).
			Where("commit_sha = ?", string(commit)).Where("scanner_version = ?", Version).Exec(ctx); err != nil {
			return err
		}
		rows := []db.ScanResult{{RepoID: repoUUID, Commit: string(commit), Path: commitRow, ScannerVersion: Version, Status: string(res.Status), Findings: db.JSON("[]")}}
		for _, f := range res.Files {
			raw, err := json.Marshal(storedFile{Findings: nonNil(f.Findings), Error: f.Error})
			if err != nil {
				return err
			}
			rows = append(rows, db.ScanResult{RepoID: repoUUID, Commit: string(commit), Path: f.Path, ScannerVersion: Version, Status: string(f.Status), Findings: raw})
		}
		for start := 0; start < len(rows); start += 500 {
			batch := rows[start:min(start+500, len(rows))]
			if _, err := tx.NewInsert().Model(&batch).Exec(ctx); err != nil {
				return err
			}
		}
		return nil
	})
}

func nonNil(f []Finding) []Finding {
	if f == nil {
		return []Finding{}
	}
	return f
}

// Results implements Scanner. It returns the newest stored version.
func (s *Service) Results(ctx context.Context, repoUUID uuid.UUID, commit model.CommitSHA) (*CommitResult, error) {
	var version sql.NullInt64
	err := s.o.DB.NewSelect().Model((*db.ScanResult)(nil)).ColumnExpr("MAX(scanner_version)").
		Where("repo_id = ?", repoUUID).Where("commit_sha = ?", string(commit)).Where("path = ?", commitRow).Scan(ctx, &version)
	if err != nil && !db.IsNotFound(err) {
		return nil, err
	}
	if !version.Valid {
		return nil, fmt.Errorf("%w: %s", ErrNotScanned, commit)
	}
	var rows []db.ScanResult
	if err := s.o.DB.NewSelect().Model(&rows).Where("repo_id = ?", repoUUID).Where("commit_sha = ?", string(commit)).
		Where("scanner_version = ?", version.Int64).Order("path ASC").Scan(ctx); err != nil {
		return nil, err
	}
	res := &CommitResult{Status: StatusSafe, Files: []FileResult{}}
	for _, row := range rows {
		if row.Path == commitRow {
			res.Status = Status(row.Status)
			continue
		}
		var sf storedFile
		if err := json.Unmarshal(row.Findings, &sf); err != nil {
			return nil, fmt.Errorf("decode scan results for %s: %w", row.Path, err)
		}
		res.Files = append(res.Files, FileResult{Path: row.Path, Status: Status(row.Status), Findings: sf.Findings, Error: sf.Error})
	}
	return res, nil
}

// FileResult returns the stored result of one file, or ErrNotScanned.
func (s *Service) FileResult(ctx context.Context, repoUUID uuid.UUID, commit model.CommitSHA, p string) (*FileResult, error) {
	res, err := s.Results(ctx, repoUUID, commit)
	if err != nil {
		return nil, err
	}
	for i := range res.Files {
		if res.Files[i].Path == p {
			return &res.Files[i], nil
		}
	}
	return &FileResult{Path: p, Status: StatusSafe}, nil
}

// Register adds the scan Job handler.
func (s *Service) Register(reg *jobs.Registry) error {
	return reg.Register(jobs.KindScan, s.HandleJob, jobs.HandlerOptions{MaxAttempts: 5, Timeout: 6 * time.Hour})
}

// HandleJob runs a scan Job for a Commit and emits scan.completed.
func (s *Service) HandleJob(ctx context.Context, j *jobs.Job, _ jobs.Progress) error {
	var p index.CommitJob
	if err := j.DecodePayload(&p); err != nil {
		return jobs.Permanent(err)
	}
	if !model.IsFullSHA(string(p.Commit)) {
		return jobs.Permanent(fmt.Errorf("scan job: invalid commit %q", p.Commit))
	}
	r, err := s.o.Repos.GetByUUID(ctx, p.RepoUUID)
	if errors.Is(err, repo.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if r.DeletedAt != nil {
		return nil
	}
	res, err := s.ScanCommit(ctx, r.UUID, r.ID, p.Commit)
	if errors.Is(err, repo.ErrRevisionNotFound) || errors.Is(err, repo.ErrNotFound) {
		return jobs.Permanent(err)
	}
	if err != nil {
		return err
	}
	if s.o.Webhooks != nil {
		flagged := []map[string]any{}
		for _, f := range res.Files {
			if f.Status != StatusSafe {
				flagged = append(flagged, map[string]any{"path": f.Path, "status": string(f.Status)})
			}
		}
		data := map[string]any{"commit": string(p.Commit), "status": string(res.Status), "files": flagged}
		if err := s.o.Webhooks.Emit(ctx, webhook.NewEvent(webhook.ScanCompleted, model.System(), &r.ID, r.UUID, r.NamespaceID, data)); err != nil {
			s.o.Log.WarnContext(ctx, "emit scan.completed", "repo.id", r.ID.String(), "error", err.Error())
		}
	}
	return nil
}

// RescanStale enqueues low-priority scan Jobs for branch heads whose
// results come from an older scanner version, and returns how many it
// queued. Run it when a worker starts after Version changes.
func (s *Service) RescanStale(ctx context.Context, q jobs.Queue) (int, error) {
	var rows []struct {
		RepoID uuid.UUID `bun:"repo_id"`
		Commit string    `bun:"commit_sha"`
	}
	err := s.o.DB.NewSelect().TableExpr("scan_results").ColumnExpr("repo_id, commit_sha").Where("path = ?", commitRow).
		GroupExpr("repo_id, commit_sha").Having("MAX(scanner_version) < ?", Version).Scan(ctx, &rows)
	if err != nil {
		return 0, err
	}
	heads := map[uuid.UUID]map[model.CommitSHA]bool{}
	repos := map[uuid.UUID]*repo.Repo{}
	queued := 0
	for _, row := range rows {
		r, seen := repos[row.RepoID]
		if !seen {
			got, err := s.o.Repos.GetByUUID(ctx, row.RepoID)
			if err != nil && !errors.Is(err, repo.ErrNotFound) {
				return queued, err
			}
			r, repos[row.RepoID] = got, got
			if got != nil && got.DeletedAt == nil {
				refs, err := s.o.Repos.Refs(ctx, got.ID)
				if err != nil {
					return queued, err
				}
				heads[row.RepoID] = map[model.CommitSHA]bool{}
				for _, b := range refs.Branches {
					heads[row.RepoID][b.Target] = true
				}
			}
		}
		if r == nil || r.DeletedAt != nil || !heads[row.RepoID][model.CommitSHA(row.Commit)] {
			continue
		}
		job := index.CommitJob{RepoUUID: r.UUID, Repo: r.ID.String(), Type: r.ID.Type, Commit: model.CommitSHA(row.Commit)}
		if _, err := q.Enqueue(ctx, jobs.NewJob{Kind: jobs.KindScan, DedupeKey: job.DedupeKey(), Payload: job, Priority: jobs.PriorityLow}); err != nil {
			return queued, err
		}
		queued++
	}
	return queued, nil
}
