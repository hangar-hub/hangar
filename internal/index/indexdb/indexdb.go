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
	"reflect"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/hangar-hub/hangar/internal/blob"
	"github.com/hangar-hub/hangar/internal/datasets/meta"
	"github.com/hangar-hub/hangar/internal/db"
	"github.com/hangar-hub/hangar/internal/index"
	"github.com/hangar-hub/hangar/internal/index/gguf"
	"github.com/hangar-hub/hangar/internal/index/rangeio"
	"github.com/hangar-hub/hangar/internal/index/safetensors"
	"github.com/hangar-hub/hangar/internal/jobs"
	"github.com/hangar-hub/hangar/internal/model"
	"github.com/hangar-hub/hangar/internal/repo"
	"github.com/hangar-hub/hangar/internal/telemetry"
	"github.com/hangar-hub/hangar/internal/webhook"
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
	// DatasetSplits returns the converted Splits of a dataset Commit, from
	// its Parquet export, for rows, bytes and features per Split (DATA-9);
	// optional. The datasets package provides it.
	DatasetSplits func(ctx context.Context, repoUUID uuid.UUID, commit model.CommitSHA) ([]DatasetSplit, error)
	// DatasetSplitsOf returns the converted Splits of many dataset
	// Commits at once, as DatasetSplits does for one, so GetMany reads
	// them with one call; optional. Without it GetMany calls DatasetSplits
	// for each dataset.
	DatasetSplitsOf func(ctx context.Context, commits map[uuid.UUID]model.CommitSHA) (map[uuid.UUID][]DatasetSplit, error)
	// AfterIndex runs after a Job stores results, for example to enqueue
	// dataset.convert; optional. Its error fails the Job so it is retried.
	AfterIndex func(ctx context.Context, job index.CommitJob, s *index.Summary) error
	// Window is the ranged read size; zero means 1 MiB.
	Window int64
}

// DatasetSplit is one Split of a dataset Commit as the Parquet export
// records it.
type DatasetSplit struct {
	Config string
	Split  string
	// Converted is true when the Split has a Parquet export; Partial when
	// the export stopped at the size limit. Splits that are not converted
	// have no sizes yet.
	Converted    bool
	Partial      bool
	Rows         int64
	Bytes        int64
	ParquetBytes int64
	// Features are the Split's columns, in order, or nil.
	Features []index.FeatureColumn
}

// Service is the database-backed Indexer.
type Service struct {
	o Options
}

var (
	_ index.Indexer     = (*Service)(nil)
	_ index.BatchGetter = (*Service)(nil)
)

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
	if _, err := s.applySplits(ctx, repoUUID, commit, sum); err != nil {
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
// from an older indexer stay visible while re-indexing runs. A dataset's
// Split sizes come from its Parquet export, which usually finishes after
// the index Job, so Get adds sizes that are newer than the stored results
// and stores them, because the export keeps rows only for the Commit it
// last converted.
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
	return s.decode(ctx, &row)
}

// getManyBatch is how many Commits one GetMany query reads.
const getManyBatch = 500

// GetMany implements index.BatchGetter: Get for many Commits, reading the
// stored results with one query per 500 Commits.
func (s *Service) GetMany(ctx context.Context, commits map[uuid.UUID]model.CommitSHA) (map[uuid.UUID]*index.Summary, error) {
	out := make(map[uuid.UUID]*index.Summary, len(commits))
	ids := make([]uuid.UUID, 0, len(commits))
	for id := range commits {
		ids = append(ids, id)
	}
	for start := 0; start < len(ids); start += getManyBatch {
		part := ids[start:min(start+getManyBatch, len(ids))]
		shas := make([]string, 0, len(part))
		for _, id := range part {
			shas = append(shas, string(commits[id]))
		}
		var rows []db.Metadata
		if err := s.o.DB.NewSelect().Model(&rows).Where("repo_id IN (?)", bun.List(part)).Where("commit_sha IN (?)", bun.List(shas)).
			Scan(ctx); err != nil {
			return nil, err
		}
		// The newest indexer version of each Repo's own Commit, as Get
		// picks it.
		newest := map[uuid.UUID]*db.Metadata{}
		for i := range rows {
			r := &rows[i]
			if string(commits[r.RepoID]) != r.Commit {
				continue
			}
			if cur := newest[r.RepoID]; cur == nil || r.IndexerVersion > cur.IndexerVersion {
				newest[r.RepoID] = r
			}
		}
		sums := map[uuid.UUID]*index.Summary{}
		datasets := map[uuid.UUID]model.CommitSHA{}
		for id, row := range newest {
			var sum index.Summary
			if err := json.Unmarshal(row.Summary, &sum); err != nil {
				return nil, fmt.Errorf("decode index results: %w", err)
			}
			sums[id] = &sum
			if sum.Dataset != nil {
				datasets[id] = commits[id]
			}
		}
		var splits map[uuid.UUID][]DatasetSplit
		if len(datasets) > 0 && s.o.DatasetSplits != nil && s.o.DatasetSplitsOf != nil {
			var err error
			if splits, err = s.o.DatasetSplitsOf(ctx, datasets); err != nil {
				return nil, fmt.Errorf("dataset splits: %w", err)
			}
		}
		for id, sum := range sums {
			var err error
			if splits != nil {
				err = s.storeSplits(ctx, newest[id], sum, splits[id])
			} else {
				err = s.addSplits(ctx, newest[id], sum)
			}
			if err != nil {
				return nil, err
			}
			out[id] = sum
		}
	}
	return out, nil
}

// decode reads a stored row's results and adds newer dataset sizes, storing
// them when they changed.
func (s *Service) decode(ctx context.Context, row *db.Metadata) (*index.Summary, error) {
	var sum index.Summary
	if err := json.Unmarshal(row.Summary, &sum); err != nil {
		return nil, fmt.Errorf("decode index results: %w", err)
	}
	if err := s.addSplits(ctx, row, &sum); err != nil {
		return nil, err
	}
	return &sum, nil
}

// addSplits adds a stored row's newer dataset sizes from DatasetSplits,
// storing them when they changed.
func (s *Service) addSplits(ctx context.Context, row *db.Metadata, sum *index.Summary) error {
	if sum.Dataset == nil || s.o.DatasetSplits == nil {
		return nil
	}
	splits, err := s.o.DatasetSplits(ctx, row.RepoID, model.CommitSHA(row.Commit))
	if err != nil {
		return fmt.Errorf("dataset splits: %w", err)
	}
	return s.storeSplits(ctx, row, sum, splits)
}

// storeSplits applies splits to a stored row's results and stores them
// when they changed.
func (s *Service) storeSplits(ctx context.Context, row *db.Metadata, sum *index.Summary, splits []DatasetSplit) error {
	if sum.Dataset == nil {
		return nil
	}
	changed, err := applySplitSizes(sum, splits)
	if err != nil || !changed {
		return err
	}
	raw, err := json.Marshal(sum)
	if err != nil {
		return err
	}
	row.Summary, row.UpdatedAt = raw, db.Now()
	if _, err := s.o.DB.NewUpdate().Model(row).Column("summary", "updated_at").WherePK().Exec(ctx); err != nil {
		return fmt.Errorf("store dataset sizes: %w", err)
	}
	return nil
}

// applySplits fills a dataset Summary's rows, bytes, features and size
// category from the Parquet export, and reports whether they changed.
// Commits the export has no Splits for keep what they have: the export
// drops the rows of a Commit when it converts a newer one.
func (s *Service) applySplits(ctx context.Context, repoUUID uuid.UUID, commit model.CommitSHA, sum *index.Summary) (bool, error) {
	if sum.Dataset == nil || s.o.DatasetSplits == nil {
		return false, nil
	}
	splits, err := s.o.DatasetSplits(ctx, repoUUID, commit)
	if err != nil {
		return false, fmt.Errorf("dataset splits: %w", err)
	}
	return applySplitSizes(sum, splits)
}

// applySplitSizes fills a dataset Summary's rows, bytes, features and size
// category from its Splits, as applySplits describes.
func applySplitSizes(sum *index.Summary, splits []DatasetSplit) (bool, error) {
	d := sum.Dataset
	var sizes []index.SplitSize
	var rows, bytes int64
	features := map[string][]index.FeatureColumn{}
	complete := len(splits) > 0
	for _, sp := range splits {
		if !sp.Converted {
			complete = false
			continue
		}
		sizes = append(sizes, index.SplitSize{Config: sp.Config, Split: sp.Split, Rows: sp.Rows, Bytes: sp.Bytes,
			ParquetBytes: sp.ParquetBytes, Partial: sp.Partial})
		rows += sp.Rows
		bytes += sp.Bytes
		if sp.Partial {
			complete = false
		}
		if _, ok := features[sp.Config]; !ok && len(sp.Features) > 0 {
			features[sp.Config] = sp.Features
		}
	}
	if len(sizes) == 0 {
		return false, nil
	}
	if len(features) == 0 {
		features = nil
	}
	before, err := json.Marshal(d)
	if err != nil {
		return false, err
	}
	d.SplitSizes, d.Rows, d.Bytes, d.Features = sizes, rows, bytes, features
	if complete {
		d.SizeCategory = meta.SizeCategory(rows)
	}
	syncSizeTag(sum)
	after, err := json.Marshal(d)
	if err != nil {
		return false, err
	}
	return !sameJSON(before, after), nil
}

// sameJSON compares two JSON documents by value: Postgres jsonb returns
// objects with their keys reordered and spaces added.
func sameJSON(a, b []byte) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	return reflect.DeepEqual(x, y)
}

// syncSizeTag keeps the size_categories tag equal to the size category.
func syncSizeTag(sum *index.Summary) {
	const prefix = "size_categories:"
	tags := sum.Tags[:0:0]
	at := -1
	for _, t := range sum.Tags {
		if strings.HasPrefix(t, prefix) {
			if at < 0 {
				at = len(tags)
			}
			continue
		}
		tags = append(tags, t)
	}
	if c := sum.Dataset.SizeCategory; c != "" {
		if at < 0 {
			at = len(tags)
		}
		tags = append(tags[:at], append([]string{prefix + c}, tags[at:]...)...)
	}
	sum.Tags = tags
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
	Version     uint32 `json:"version"`
	TensorCount uint64 `json:"tensor_count"`
	Alignment   uint64 `json:"alignment"`
	DataOffset  int64  `json:"data_offset"`
	// Metadata maps each key to its plain value. Arrays are listed whole
	// when the header keeps every item, and otherwise summarised as
	// {"type": "array", "length": N}, the shape the UI reads.
	Metadata map[string]any `json:"metadata"`
	Tensors  []HeaderTensor `json:"tensors"`
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
	view := HeaderView{Version: f.Version, TensorCount: f.TensorCount, Alignment: f.Alignment, DataOffset: f.DataOffset,
		Metadata: make(map[string]any, len(f.Metadata))}
	for k, v := range f.Metadata {
		view.Metadata[k] = plainValue(v)
	}
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
	if err != nil && s.deletedMeanwhile(ctx, r) {
		return nil
	}
	if errors.Is(err, repo.ErrRevisionNotFound) || errors.Is(err, repo.ErrNotFound) {
		return jobs.Permanent(err)
	}
	if err != nil {
		return err
	}
	// A file the index could not read is an entry in the results, not a
	// failure, so a Repo deleted meanwhile is checked here: it gets no
	// index.completed event and no Dependency resolution.
	if s.deletedMeanwhile(ctx, r) {
		return nil
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
			if s.deletedMeanwhile(ctx, r) {
				return nil
			}
			return err
		}
	}
	return nil
}

// deletedMeanwhile reports whether the Repo was deleted or purged while
// its Job ran, so a failure caused by the deletion ends the Job quietly
// instead of failing it.
func (s *Service) deletedMeanwhile(ctx context.Context, r *repo.Repo) bool {
	now, err := s.o.Repos.GetByUUID(ctx, r.UUID)
	if errors.Is(err, repo.ErrNotFound) || (err == nil && now.DeletedAt != nil) {
		s.o.Log.DebugContext(ctx, "index stopped: repo deleted", "repo.id", r.ID.String())
		return true
	}
	return false
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
		if _, err := q.Enqueue(ctx, jobs.NewJob{Kind: jobs.KindIndex, DedupeKey: job.DedupeKey(), Payload: job, Priority: jobs.PriorityLow, Repo: r.UUID}); err != nil {
			return queued, err
		}
		queued++
	}
	return queued, nil
}

// plainValue turns a GGUF metadata value into what the header viewer shows:
// the scalar itself, an array's items when all of them were kept, or a
// summary of a long array.
func plainValue(v gguf.Value) any {
	if v.Type != gguf.TypeArray {
		return v.Scalar
	}
	if int64(len(v.Items)) == v.Len {
		if v.Items == nil {
			return []any{}
		}
		return v.Items
	}
	return map[string]any{"type": "array", "length": v.Len}
}
