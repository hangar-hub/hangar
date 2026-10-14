// Package indexdb is the database-backed index.Indexer: it runs the index
// Job for each new branch head, stores results in the metadata table, and
// serves file headers and GGUF quant groups (META-1, META-3). It lives
// apart from package index because the Repo service imports index for its
// interface, and this implementation reads files through the Repo service.
package indexdb

import (
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

// Options configure the Service.
type Options struct {
	DB    *db.DB
	Repos repo.Service
	// Webhooks receives index.completed events; optional.
	Webhooks webhook.Emitter
	// Instruments records hangar.index.duration; optional.
	Instruments *telemetry.Instruments
	Log         *slog.Logger
	// Layout returns a dataset Commit's Configs and Split counts (DATA-9);
	// optional. The datasets package provides it.
	Layout func(ctx context.Context, id model.RepoID, commit model.CommitSHA) ([]string, map[string]int, error)
	// AfterIndex runs after a Job stores results, for example to enqueue
	// dataset.convert; optional. Its error fails the Job so it is retried.
	AfterIndex func(ctx context.Context, job index.CommitJob, s *index.Summary) error
	// Window is the ranged read size; zero means 1 MiB.
	Window int64
}

// Service is the database-backed Indexer.
type Service struct {
	o Options
}

var _ index.Indexer = (*Service)(nil)

// New returns a Service.
func New(o Options) *Service {
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	return &Service{o: o}
}

// repoSource reads files through the Repo service.
type repoSource struct {
	repos  repo.Service
	id     model.RepoID
	commit model.CommitSHA
	window int64
}

func (s repoSource) ReadAll(ctx context.Context, p string, limit int64) ([]byte, error) {
	rc, _, err := s.repos.OpenFile(ctx, s.id, s.commit, p, nil)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%s is larger than %d bytes", p, limit)
	}
	return b, nil
}

func (s repoSource) ReaderAt(ctx context.Context, p string, size int64) io.ReaderAt {
	return rangeio.New(ctx, size, s.window, func(ctx context.Context, start, end int64) (io.ReadCloser, error) {
		rc, _, err := s.repos.OpenFile(ctx, s.id, s.commit, p, &blob.Range{Start: start, End: end})
		return rc, err
	})
}

// Index implements Indexer.
func (s *Service) Index(ctx context.Context, repoUUID uuid.UUID, id model.RepoID, commit model.CommitSHA) (*index.Summary, error) {
	entries, err := s.o.Repos.Tree(ctx, id, commit, "", repo.TreeOpts{Recursive: true})
	if err != nil {
		return nil, err
	}
	files := make([]index.File, 0, len(entries))
	for _, e := range entries {
		if e.Type == model.EntryFile {
			files = append(files, index.File{Path: e.Path, Size: e.Size, Present: e.Present})
		}
	}
	var layout index.DatasetLayout
	if id.Type == model.RepoTypeDataset && s.o.Layout != nil {
		layout = func(ctx context.Context) ([]string, map[string]int, error) { return s.o.Layout(ctx, id, commit) }
	}
	sum, err := index.Extract(ctx, id.Type, files, repoSource{repos: s.o.Repos, id: id, commit: commit, window: s.o.Window}, layout)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(sum)
	if err != nil {
		return nil, err
	}
	now := db.Now()
	row := &db.Metadata{RepoID: repoUUID, Commit: string(commit), IndexerVersion: index.Version, Summary: raw, IndexedAt: now}
	_, err = s.o.DB.NewInsert().Model(row).
		On("CONFLICT (repo_id, commit_sha, indexer_version) DO UPDATE").
		Set("summary = EXCLUDED.summary").Set("indexed_at = EXCLUDED.indexed_at").Set("updated_at = EXCLUDED.updated_at").
		Exec(ctx)
	if err != nil {
		return nil, fmt.Errorf("store index results: %w", err)
	}
	return sum, nil
}

// Get implements Indexer. It returns the newest stored version, so results
// from an older indexer stay visible while re-indexing runs.
func (s *Service) Get(ctx context.Context, repoUUID uuid.UUID, commit model.CommitSHA) (*index.Summary, error) {
	var row db.Metadata
	err := s.o.DB.NewSelect().Model(&row).Where("repo_id = ?", repoUUID).Where("commit_sha = ?", string(commit)).
		Order("indexer_version DESC").Limit(1).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", index.ErrNotIndexed, commit)
	}
	if err != nil {
		return nil, err
	}
	var sum index.Summary
	if err := json.Unmarshal(row.Summary, &sum); err != nil {
		return nil, fmt.Errorf("decode index results: %w", err)
	}
	return &sum, nil
}

// IndexedAt returns when a Commit was indexed, or index.ErrNotIndexed.
func (s *Service) IndexedAt(ctx context.Context, repoUUID uuid.UUID, commit model.CommitSHA) (time.Time, error) {
	var row db.Metadata
	err := s.o.DB.NewSelect().Model(&row).Column("indexed_at").Where("repo_id = ?", repoUUID).Where("commit_sha = ?", string(commit)).
		Order("indexer_version DESC").Limit(1).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, index.ErrNotIndexed
	}
	return row.IndexedAt, err
}

// HeaderView is the GGUF header as FileHeader returns it.
type HeaderView struct {
	Version     uint32                `json:"version"`
	TensorCount uint64                `json:"tensor_count"`
	Alignment   uint64                `json:"alignment"`
	DataOffset  int64                 `json:"data_offset"`
	Metadata    map[string]gguf.Value `json:"metadata"`
	Tensors     []HeaderTensor        `json:"tensors"`
}

// HeaderTensor is one tensor in a GGUF HeaderView.
type HeaderTensor struct {
	Name   string   `json:"name"`
	Shape  []uint64 `json:"shape"`
	Type   string   `json:"type"`
	Offset uint64   `json:"offset"`
}

// FileHeader implements Indexer: the safetensors header JSON as stored, or
// a GGUF header with long arrays shortened.
func (s *Service) FileHeader(ctx context.Context, id model.RepoID, commit model.CommitSHA, p string) (json.RawMessage, error) {
	ext := strings.ToLower(path.Ext(p))
	if ext != ".safetensors" && ext != ".gguf" {
		return nil, fmt.Errorf("%w: %s", index.ErrNoHeader, p)
	}
	e, err := s.o.Repos.Entry(ctx, id, commit, p)
	if err != nil {
		return nil, err
	}
	if e.Type != model.EntryFile {
		return nil, fmt.Errorf("%w: %s is a directory", index.ErrNoHeader, p)
	}
	if !e.Present {
		return nil, fmt.Errorf("%w: %s", repo.ErrNotPresent, p)
	}
	ra := repoSource{repos: s.o.Repos, id: id, commit: commit, window: s.o.Window}.ReaderAt(ctx, p, e.Size)
	if ext == ".safetensors" {
		h, err := safetensors.Read(ra, e.Size)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", index.ErrNoHeader, err)
		}
		return h.Raw, nil
	}
	f, err := gguf.Read(ra, e.Size)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", index.ErrNoHeader, err)
	}
	view := HeaderView{Version: f.Version, TensorCount: f.TensorCount, Alignment: f.Alignment, DataOffset: f.DataOffset, Metadata: f.Metadata}
	for _, t := range f.Tensors {
		view.Tensors = append(view.Tensors, HeaderTensor{Name: t.Name, Shape: t.Dims, Type: t.TypeName(), Offset: t.Offset})
	}
	return json.Marshal(view)
}

// Quants implements Indexer.
func (s *Service) Quants(ctx context.Context, id model.RepoID, commit model.CommitSHA) ([]index.QuantGroup, error) {
	entries, err := s.o.Repos.Tree(ctx, id, commit, "", repo.TreeOpts{Recursive: true})
	if err != nil {
		return nil, err
	}
	sizes := map[string]int64{}
	for _, e := range entries {
		if e.Type == model.EntryFile {
			sizes[e.Path] = e.Size
		}
	}
	return index.GroupQuants(sizes), nil
}

// Register adds the index Job handler.
func (s *Service) Register(reg *jobs.Registry) error {
	return reg.Register(jobs.KindIndex, s.HandleJob, jobs.HandlerOptions{MaxAttempts: 5, Timeout: time.Hour})
}

// HandleJob runs an index Job: it indexes the Commit, stores the results
// and emits index.completed. Deleted Repos are skipped.
func (s *Service) HandleJob(ctx context.Context, j *jobs.Job, _ jobs.Progress) error {
	var p index.CommitJob
	if err := j.DecodePayload(&p); err != nil {
		return jobs.Permanent(err)
	}
	if !model.IsFullSHA(string(p.Commit)) {
		return jobs.Permanent(fmt.Errorf("index job: invalid commit %q", p.Commit))
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
	start := time.Now()
	sum, err := s.Index(ctx, r.UUID, r.ID, p.Commit)
	outcome := "success"
	if err != nil {
		outcome = "error"
	}
	if s.o.Instruments != nil && s.o.Instruments.IndexDuration != nil {
		s.o.Instruments.IndexDuration.Record(ctx, time.Since(start).Seconds(), metric.WithAttributes(attribute.String(telemetry.MetricOutcome, outcome)))
	}
	if errors.Is(err, repo.ErrRevisionNotFound) || errors.Is(err, repo.ErrNotFound) {
		return jobs.Permanent(err)
	}
	if err != nil {
		return err
	}
	if s.o.Webhooks != nil {
		data := map[string]any{"commit": string(p.Commit), "formats": sum.Formats}
		if sum.Architecture != "" {
			data["architecture"] = sum.Architecture
		}
		if err := s.o.Webhooks.Emit(ctx, webhook.NewEvent(webhook.IndexCompleted, model.System(), &r.ID, r.UUID, r.NamespaceID, data)); err != nil {
			s.o.Log.WarnContext(ctx, "emit index.completed", "repo.id", r.ID.String(), "error", err.Error())
		}
	}
	if s.o.AfterIndex != nil {
		p.Repo, p.Type = r.ID.String(), r.ID.Type
		if err := s.o.AfterIndex(ctx, p, sum); err != nil {
			return err
		}
	}
	return nil
}

// ReindexStale enqueues low-priority index Jobs for branch heads whose
// results come from an older indexer version, and returns how many it
// queued. Run it when a worker starts after Version changes.
func (s *Service) ReindexStale(ctx context.Context, q jobs.Queue) (int, error) {
	var rows []struct {
		RepoID uuid.UUID `bun:"repo_id"`
		Commit string    `bun:"commit_sha"`
	}
	err := s.o.DB.NewSelect().TableExpr("metadata").ColumnExpr("repo_id, commit_sha").
		GroupExpr("repo_id, commit_sha").Having("MAX(indexer_version) < ?", index.Version).Scan(ctx, &rows)
	if err != nil {
		return 0, err
	}
	heads := map[uuid.UUID]map[model.CommitSHA]bool{}
	ids := map[uuid.UUID]*repo.Repo{}
	queued := 0
	for _, row := range rows {
		r, ok := ids[row.RepoID]
		if !ok {
			got, err := s.o.Repos.GetByUUID(ctx, row.RepoID)
			if errors.Is(err, repo.ErrNotFound) {
				ids[row.RepoID] = nil
				continue
			}
			if err != nil {
				return queued, err
			}
			r = got
			ids[row.RepoID] = r
			refs, err := s.o.Repos.Refs(ctx, r.ID)
			if err != nil {
				return queued, err
			}
			heads[row.RepoID] = map[model.CommitSHA]bool{}
			for _, b := range refs.Branches {
				heads[row.RepoID][b.Target] = true
			}
		}
		if r == nil || r.DeletedAt != nil || !heads[row.RepoID][model.CommitSHA(row.Commit)] {
			continue
		}
		job := index.CommitJob{RepoUUID: r.UUID, Repo: r.ID.String(), Type: r.ID.Type, Commit: model.CommitSHA(row.Commit)}
		if _, err := q.Enqueue(ctx, jobs.NewJob{Kind: jobs.KindIndex, DedupeKey: job.DedupeKey(), Payload: job, Priority: jobs.PriorityLow}); err != nil {
			return queued, err
		}
		queued++
	}
	return queued, nil
}
