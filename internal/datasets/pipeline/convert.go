package pipeline

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/jamesagarside/hangar/internal/blob"
	"github.com/jamesagarside/hangar/internal/datasets"
	"github.com/jamesagarside/hangar/internal/datasets/convert"
	"github.com/jamesagarside/hangar/internal/datasets/layout"
	"github.com/jamesagarside/hangar/internal/datasets/pq"
	"github.com/jamesagarside/hangar/internal/db"
	"github.com/jamesagarside/hangar/internal/jobs"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/repo"
)

// ExportRef is the ref that holds the Parquet export, as on the Hub.
const ExportRef = "refs/convert/parquet"

// CommitMessage is the summary of export Commits, as the Hub writes them.
const CommitMessage = "Update parquet files"

// Payload is the payload of dataset.convert and dataset.stats Jobs.
type Payload struct {
	Repo   uuid.UUID       `json:"repo"`
	Commit model.CommitSHA `json:"commit"`
}

// Deps are the services the Jobs need.
type Deps struct {
	DB    *db.DB
	Blobs blob.Store
	Repos repo.Service
	LFS   repo.LFSStore
	Jobs  jobs.Queue
	// MaxConvertBytes is viewer.max_convert_bytes.
	MaxConvertBytes int64
	// MaxStatsBytes caps the Parquet data statistics read per Split, as
	// the Hub's descriptive statistics do. Zero means 100 MB.
	MaxStatsBytes int64
	// TempDir holds Parquet files until they are stored; "" means the
	// system temporary directory.
	TempDir string
	// ConvertConcurrency is viewer.convert_concurrency.
	ConvertConcurrency int
	Log                *slog.Logger
}

// Pipeline runs the dataset Jobs.
type Pipeline struct {
	d     Deps
	store *Store
}

// New returns a Pipeline.
func New(d Deps) (*Pipeline, error) {
	if d.DB == nil || d.Blobs == nil || d.Repos == nil || d.LFS == nil || d.Jobs == nil {
		return nil, errors.New("pipeline: DB, Blobs, Repos, LFS and Jobs are required")
	}
	if d.MaxStatsBytes <= 0 {
		d.MaxStatsBytes = 100_000_000
	}
	if d.ConvertConcurrency <= 0 {
		d.ConvertConcurrency = 1
	}
	if d.Log == nil {
		d.Log = slog.New(slog.DiscardHandler)
	}
	return &Pipeline{d: d, store: &Store{DB: d.DB, Blobs: d.Blobs}}, nil
}

// Store returns the Pipeline's store.
func (p *Pipeline) Store() *Store { return p.store }

// Register adds the dataset.convert and dataset.stats handlers.
func (p *Pipeline) Register(reg *jobs.Registry) error {
	if err := reg.Register(jobs.KindDatasetConvert, p.HandleConvert, jobs.HandlerOptions{
		MaxAttempts: 3, Concurrency: p.d.ConvertConcurrency, Timeout: 6 * time.Hour,
	}); err != nil {
		return err
	}
	return reg.Register(jobs.KindDatasetStats, p.HandleStats, jobs.HandlerOptions{
		MaxAttempts: 3, Concurrency: p.d.ConvertConcurrency, Timeout: 2 * time.Hour,
	})
}

func dedupeKey(repoID uuid.UUID, commit model.CommitSHA) string {
	return repoID.String() + "@" + string(commit)
}

// EnqueueConvert queues the conversion of a dataset Commit, or returns the
// pending or running Job that already converts it. Call it after the index
// Job of a default-branch head; the Viewer also calls it for heads it has
// no rows for.
func (p *Pipeline) EnqueueConvert(ctx context.Context, repoID uuid.UUID, commit model.CommitSHA) (jobs.JobID, error) {
	return EnqueueConvert(ctx, p.d.Jobs, repoID, commit)
}

// EnqueueConvert queues a dataset.convert Job on a queue.
func EnqueueConvert(ctx context.Context, q jobs.Queue, repoID uuid.UUID, commit model.CommitSHA) (jobs.JobID, error) {
	return q.Enqueue(ctx, jobs.NewJob{Kind: jobs.KindDatasetConvert, DedupeKey: dedupeKey(repoID, commit),
		Payload: Payload{Repo: repoID, Commit: commit}, Priority: jobs.PriorityLow, Repo: repoID})
}

// StatsJob returns the dataset.stats Job of a Commit.
func StatsJob(repoID uuid.UUID, commit model.CommitSHA) jobs.NewJob {
	return jobs.NewJob{Kind: jobs.KindDatasetStats, DedupeKey: dedupeKey(repoID, commit), Repo: repoID,
		Payload: Payload{Repo: repoID, Commit: commit}, Priority: jobs.PriorityLow}
}

// HandleConvert is the dataset.convert Job handler.
func (p *Pipeline) HandleConvert(ctx context.Context, j *jobs.Job, progress jobs.Progress) error {
	var pl Payload
	if err := j.DecodePayload(&pl); err != nil {
		return jobs.Permanent(err)
	}
	r, err := p.d.Repos.GetByUUID(ctx, pl.Repo)
	if errors.Is(err, repo.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if r.DeletedAt != nil || r.ID.Type != model.RepoTypeDataset {
		return nil
	}
	// Skip work that is already done.
	if rows, err := p.store.Splits(ctx, r.UUID, pl.Commit); err == nil && len(rows) > 0 && !anyPending(rows) {
		return nil
	}
	if err := p.convert(ctx, r, pl.Commit, progress); err != nil {
		return err
	}
	_, err = p.d.Jobs.Enqueue(ctx, StatsJob(r.UUID, pl.Commit))
	return err
}

func anyPending(rows []SplitRow) bool {
	for _, r := range rows {
		if r.Status == StatusPending {
			return true
		}
	}
	return false
}

func (p *Pipeline) report(ctx context.Context, progress jobs.Progress, done, total int, msg string) {
	if progress != nil {
		_ = progress.Report(ctx, jobs.ProgressUpdate{FilesDone: done, FilesTotal: total, Message: msg})
	}
}

// convert converts every Config of a Commit and commits the export.
func (p *Pipeline) convert(ctx context.Context, r *repo.Repo, commit model.CommitSHA, progress jobs.Progress) error {
	files, err := SourceFiles(ctx, p.d.Repos, r.ID, commit)
	if errors.Is(err, repo.ErrRevisionNotFound) || errors.Is(err, repo.ErrNotFound) {
		return jobs.Permanent(err)
	}
	if err != nil {
		return err
	}
	card, cerr := layout.ReadCard(ctx, p.d.Repos, r.ID, commit)
	var res *datasets.Resolved
	if cerr == nil {
		list := make([]layout.File, 0, len(files))
		for _, f := range files {
			lf := layout.File{Path: f.Path, Size: f.Size}
			if strings.HasSuffix(f.Path, ".zip") {
				lf.Entries = func(fn func(name string) error) error {
					ra, err := f.OpenAt(ctx)
					if err != nil {
						return err
					}
					return layout.ZipEntries(ra, f.Size, fn)
				}
			}
			list = append(list, lf)
		}
		res, cerr = layout.Resolve(list, card)
	}
	if cerr != nil {
		code, ok := LayoutFailure(cerr)
		if !ok {
			// Reading the files or the card failed; the Job retries.
			return cerr
		}
		p.d.Log.WarnContext(ctx, "the dataset has no Configs", "repo", r.ID.Path(), "error", cerr.Error())
		return p.store.ReplaceSplits(ctx, r.UUID, commit, []SplitRow{{Config: DatasetLevel, Split: DatasetLevel, Status: StatusFailed, Error: code}})
	}
	// Show every Split as pending while the conversion runs.
	var pending []SplitRow
	for _, c := range res.Configs {
		for _, s := range c.Splits() {
			pending = append(pending, SplitRow{Config: c.Name, Split: s, Status: StatusPending})
		}
	}
	if err := p.store.ReplaceSplits(ctx, r.UUID, commit, pending); err != nil {
		return err
	}
	if r.Mirror {
		adopted, err := p.adoptExport(ctx, r, commit, res)
		if err != nil || adopted {
			return err
		}
	}
	tmp, err := os.MkdirTemp(p.d.TempDir, "hangar-convert-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	repoURL := fmt.Sprintf("hf://datasets/%s@%s", r.ID.String(), commit)
	opts := convert.Options{MaxSplitBytes: p.d.MaxConvertBytes, TempDir: tmp, DatasetName: convert.SnakeCase(r.ID.Name), RepoURL: repoURL,
		Open: func(_ context.Context, path string) (convert.File, error) {
			f, ok := files[path]
			if !ok {
				return convert.File{}, fmt.Errorf("%s does not exist at %s", path, commit)
			}
			return f, nil
		}}
	var results []*convert.ConfigResult
	var rows []SplitRow
	for i, c := range res.Configs {
		p.report(ctx, progress, i, len(res.Configs), "Converting config "+c.Name)
		cr, err := convert.Convert(ctx, c, files, opts)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			p.d.Log.WarnContext(ctx, "dataset conversion failed", "repo", r.ID.Path(), "config", c.Name, "error", err.Error())
			for _, s := range c.Splits() {
				rows = append(rows, SplitRow{Config: c.Name, Split: s, Status: StatusFailed, Error: FailureGeneration})
			}
			continue
		}
		results = append(results, cr)
	}
	defer func() {
		for _, cr := range results {
			cr.Cleanup()
		}
	}()
	export, err := p.commitExport(ctx, r, commit, results)
	if err != nil {
		return err
	}
	for _, cr := range results {
		info := &ConfigInfo{Config: cr.Name, Builder: cr.Builder, DatasetName: convert.SnakeCase(r.ID.Name), Features: cr.Features,
			Copied: cr.Copied, Partial: cr.Partial, ExportCommit: export, Commit: commit}
		for _, f := range cr.SourceFiles {
			info.SourceFiles = append(info.SourceFiles, SourceFile{Path: f.Path, Size: f.Size})
		}
		status := StatusReady
		if cr.Partial {
			status = StatusPartial
		}
		for _, sr := range cr.Splits {
			si := SplitInfo{Name: sr.Name, NumBytes: sr.NumBytes, NumExamples: sr.NumRows}
			var parquetBytes int64
			for _, sh := range sr.Shards {
				si.Shards = append(si.Shards, ShardInfo{Path: sh.ExportPath, Size: sh.Size, OID: sh.SHA256, NumRows: sh.NumRows})
				parquetBytes += sh.Size
			}
			info.Splits = append(info.Splits, si)
			var source int64
			if c, ok := res.Config(cr.Name); ok {
				for _, path := range c.Files[sr.Name] {
					source += files[path].Size
				}
			}
			rows = append(rows, SplitRow{Config: cr.Name, Split: sr.Name, Status: status, NumRows: sr.NumRows,
				NumBytesSource: source, NumBytesParquet: parquetBytes, Features: cr.Features, ConvertedCommit: export})
		}
		if err := p.store.SaveInfo(ctx, r.UUID, info); err != nil {
			return err
		}
	}
	rows = orderRows(rows, res)
	if err := p.store.ReplaceSplits(ctx, r.UUID, commit, rows); err != nil {
		return err
	}
	return p.store.DeleteOtherCommits(ctx, r.UUID, commit)
}

// orderRows sorts rows in the layout's Config and Split order.
func orderRows(rows []SplitRow, res *datasets.Resolved) []SplitRow {
	pos := map[string]int{}
	i := 0
	for _, c := range res.Configs {
		for _, s := range c.Splits() {
			pos[c.Name+"\x00"+s] = i
			i++
		}
	}
	sort.SliceStable(rows, func(a, b int) bool {
		return pos[rows[a].Config+"\x00"+rows[a].Split] < pos[rows[b].Config+"\x00"+rows[b].Split]
	})
	return rows
}

// commitExport stores the Parquet files and commits them to the export
// ref as hangar-bot, replacing the previous export.
func (p *Pipeline) commitExport(ctx context.Context, r *repo.Repo, commit model.CommitSHA, results []*convert.ConfigResult) (model.CommitSHA, error) {
	var ops []repo.Operation
	for _, cr := range results {
		for _, sr := range cr.Splits {
			for _, sh := range sr.Shards {
				if sh.LocalPath != "" {
					if err := p.ingest(ctx, sh); err != nil {
						return "", err
					}
				}
				ops = append(ops, repo.Operation{Kind: repo.OpAddLFS, Path: sh.ExportPath, OID: sh.SHA256, Size: sh.Size})
			}
		}
	}
	if len(ops) == 0 {
		return "", nil
	}
	params := repo.CommitParams{Repo: r.ID, Revision: ExportRef, Summary: CommitMessage, Actor: model.System(), AllowMirror: true}
	tip, err := p.d.Repos.Resolve(ctx, r.ID, ExportRef)
	switch {
	case errors.Is(err, repo.ErrRevisionNotFound):
		params.CreateFrom = commit
	case err != nil:
		return "", err
	default:
		params.ParentCommit = tip
	}
	// Start from an empty tree with the source's .gitattributes, as the
	// Hub keeps only .gitattributes and the Parquet files on the ref.
	base := commit
	if tip != "" {
		base = tip
	}
	var clear []repo.Operation
	if entries, err := p.d.Repos.Tree(ctx, r.ID, base, "", repo.TreeOpts{}); err == nil && len(entries) > 0 {
		clear = append(clear, repo.Operation{Kind: repo.OpDeleteFolder, Path: ""})
	}
	attrs, err := p.readSmall(ctx, r.ID, commit, ".gitattributes")
	if err != nil {
		return "", err
	}
	if attrs != nil {
		clear = append(clear, repo.Operation{Kind: repo.OpAdd, Path: ".gitattributes", Content: bytes.NewReader(attrs)})
	}
	params.Operations = append(clear, ops...)
	res, err := p.d.Repos.Commit(ctx, params)
	if err != nil {
		return "", fmt.Errorf("commit the Parquet export: %w", err)
	}
	return res.SHA, nil
}

func (p *Pipeline) readSmall(ctx context.Context, id model.RepoID, commit model.CommitSHA, name string) ([]byte, error) {
	rc, _, err := p.d.Repos.OpenFile(ctx, id, commit, name, nil)
	if errors.Is(err, repo.ErrEntryNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(io.LimitReader(rc, 1<<20))
}

func (p *Pipeline) ingest(ctx context.Context, sh convert.Shard) error {
	if _, err := p.d.LFS.Stat(ctx, sh.SHA256); err == nil {
		return nil
	}
	f, err := os.Open(sh.LocalPath)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = p.d.LFS.Ingest(ctx, sh.SHA256, sh.Size, f)
	return err
}

// adoptExport uses a Mirror's own refs/convert/parquet, which Pulls fetch
// from the Upstream, instead of converting. It returns false when the
// Mirror has no export.
func (p *Pipeline) adoptExport(ctx context.Context, r *repo.Repo, commit model.CommitSHA, res *datasets.Resolved) (bool, error) {
	tip, err := p.d.Repos.Resolve(ctx, r.ID, ExportRef)
	if errors.Is(err, repo.ErrRevisionNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	entries, err := p.d.Repos.Tree(ctx, r.ID, tip, "", repo.TreeOpts{Recursive: true})
	if err != nil {
		return false, err
	}
	type key struct{ config, split string }
	shards := map[key][]model.Entry{}
	partial := map[string]bool{}
	for _, e := range entries {
		if e.Type != model.EntryFile || !strings.HasSuffix(e.Path, ".parquet") {
			continue
		}
		parts := strings.Split(e.Path, "/")
		if len(parts) != 3 {
			continue
		}
		split := parts[1]
		if s, ok := strings.CutPrefix(split, "partial-"); ok {
			split = s
			partial[parts[0]] = true
		}
		if i := strings.LastIndex(split, "-part"); i > 0 {
			split = split[:i]
		}
		shards[key{parts[0], split}] = append(shards[key{parts[0], split}], e)
	}
	var rows []SplitRow
	for _, c := range res.Configs {
		info := &ConfigInfo{Config: c.Name, Builder: c.Builder, DatasetName: convert.SnakeCase(r.ID.Name), ExportCommit: tip, Commit: commit,
			Partial: partial[c.Name]}
		for _, s := range c.Splits() {
			files := shards[key{c.Name, s}]
			sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
			row := SplitRow{Config: c.Name, Split: s, ConvertedCommit: tip}
			if len(files) == 0 {
				p.d.Log.WarnContext(ctx, "the Upstream's Parquet export has no files for a split", "repo", r.ID.Path(), "config", c.Name, "split", s)
				row.Status, row.Error = StatusFailed, FailureGeneration
				rows = append(rows, row)
				continue
			}
			si := SplitInfo{Name: s}
			var ferr error
			for _, f := range files {
				if !f.Present {
					ferr = errNotPulled
					break
				}
				pf, err := pq.Open(OpenRepoFile(ctx, p.d.Repos, r.ID, tip, f.Path, f.Size), f.Size)
				if err != nil {
					ferr = err
					break
				}
				if info.Features == nil {
					info.Features = pf.Features
				}
				oid := ""
				if f.LFS != nil {
					oid = f.LFS.OID
				}
				si.Shards = append(si.Shards, ShardInfo{Path: f.Path, Size: f.Size, OID: oid, NumRows: pf.NumRows()})
				si.NumExamples += pf.NumRows()
				si.NumBytes += pf.UncompressedBytes()
				row.NumBytesParquet += f.Size
			}
			if ferr != nil {
				row.Status, row.Error = StatusFailed, FailureNotPulled
				if !errors.Is(ferr, errNotPulled) {
					p.d.Log.WarnContext(ctx, "could not read the Upstream's Parquet export", "repo", r.ID.Path(), "config", c.Name, "split", s,
						"error", ferr.Error())
					row.Error = FailureGeneration
				}
				rows = append(rows, row)
				continue
			}
			row.Status = StatusReady
			if info.Partial {
				row.Status = StatusPartial
			}
			row.NumRows, row.Features = si.NumExamples, info.Features
			info.Splits = append(info.Splits, si)
			rows = append(rows, row)
		}
		if len(info.Splits) > 0 {
			if err := p.store.SaveInfo(ctx, r.UUID, info); err != nil {
				return false, err
			}
		}
	}
	if err := p.store.ReplaceSplits(ctx, r.UUID, commit, rows); err != nil {
		return false, err
	}
	return true, p.store.DeleteOtherCommits(ctx, r.UUID, commit)
}
