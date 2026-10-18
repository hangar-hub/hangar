// Package pipeline runs the dataset Jobs (DATA-3, DATA-5): `dataset.convert`
// writes the Parquet export of a dataset Repo's default-branch head to
// refs/convert/parquet as hangar-bot, and `dataset.stats` computes column
// statistics from it. It owns the dataset_splits and dataset_stats tables
// and the per-Config info documents in the Blob store, which the Dataset
// Viewer reads.
package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"

	"github.com/hangar-hub/hangar/internal/blob"
	"github.com/hangar-hub/hangar/internal/datasets/features"
	"github.com/hangar-hub/hangar/internal/db"
	"github.com/hangar-hub/hangar/internal/model"
)

// Split statuses, as the dataset_splits table stores them.
const (
	StatusPending = "pending"
	StatusReady   = "ready"
	StatusPartial = "partial"
	StatusFailed  = "failed"
)

// DatasetLevel is the Config and Split name of a row that records an
// error affecting the whole dataset, such as "no data files".
const DatasetLevel = ""

// SplitRow is one row of dataset_splits.
type SplitRow struct {
	Config          string
	Split           string
	Status          string
	NumRows         int64
	NumBytesSource  int64
	NumBytesParquet int64
	Features        features.Features
	Error           string
	ConvertedCommit model.CommitSHA
}

// Ready reports whether the Split has a Parquet export.
func (r SplitRow) Ready() bool { return r.Status == StatusReady || r.Status == StatusPartial }

// SplitInfo is a Split in a Config's info document.
type SplitInfo struct {
	Name        string `json:"name"`
	NumBytes    int64  `json:"num_bytes"`
	NumExamples int64  `json:"num_examples"`
	// Shards are the Split's Parquet files on refs/convert/parquet.
	Shards []ShardInfo `json:"shards"`
}

// ShardInfo is one Parquet file of the export.
type ShardInfo struct {
	Path    string `json:"path"`
	Size    int64  `json:"size"`
	OID     string `json:"oid"`
	NumRows int64  `json:"num_rows"`
}

// SourceFile is a data file the conversion read.
type SourceFile struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}

// ConfigInfo is what the Dataset Viewer's /info, /size and /parquet need
// about a converted Config.
type ConfigInfo struct {
	Config      string            `json:"config"`
	Builder     string            `json:"builder"`
	DatasetName string            `json:"dataset_name"`
	Features    features.Features `json:"features"`
	Splits      []SplitInfo       `json:"splits"`
	SourceFiles []SourceFile      `json:"source_files"`
	// Copied is true when source Parquet files were copied as they are.
	Copied  bool `json:"copied"`
	Partial bool `json:"partial"`
	// ExportCommit is the refs/convert/parquet Commit holding the files.
	ExportCommit model.CommitSHA `json:"export_commit"`
	// Commit is the source Commit that was converted.
	Commit model.CommitSHA `json:"commit"`
}

// StatsDoc is a Split's /statistics response, or the error that stopped
// the computation.
type StatsDoc struct {
	NumExamples int64             `json:"num_examples"`
	Statistics  []json.RawMessage `json:"statistics"`
	Partial     bool              `json:"partial"`
	Error       string            `json:"-"`
}

type bunTx = bun.Tx

// wrapJSON stores a JSON document as a JSON string, because Postgres jsonb
// reorders object keys, and Features and statistics keep their order.
func wrapJSON(doc []byte) []byte {
	b, _ := json.Marshal(string(doc))
	return b
}

// unwrapJSON reverses wrapJSON, and passes other documents through.
func unwrapJSON(v []byte) []byte {
	var s string
	if err := json.Unmarshal(v, &s); err == nil {
		return []byte(s)
	}
	return v
}

func timeStep(i int) time.Duration { return time.Duration(i) * time.Microsecond }

// Store reads and writes the dataset tables and info documents.
type Store struct {
	DB    *db.DB
	Blobs blob.Store
}

// ErrNoInfo means a Config has no info document yet.
var ErrNoInfo = errors.New("no converted info for this config")

func infoKey(repoID uuid.UUID, commit model.CommitSHA, config string) string {
	return fmt.Sprintf("datasets/%s/%s/info/%s.json", repoID, commit, hexName(config))
}

// hexName makes a Config name safe in a Blob store key.
func hexName(s string) string { return fmt.Sprintf("%x", s) }

// Splits returns the rows of a Commit, in insertion order of Configs and
// Splits as the conversion listed them.
func (s *Store) Splits(ctx context.Context, repoID uuid.UUID, commit model.CommitSHA) ([]SplitRow, error) {
	var rows []db.DatasetSplit
	if err := s.DB.NewSelect().Model(&rows).Where("repo_id = ?", repoID).Where("commit_sha = ?", string(commit)).
		Scan(ctx); err != nil {
		return nil, err
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if !rows[i].CreatedAt.Equal(rows[j].CreatedAt) {
			return rows[i].CreatedAt.Before(rows[j].CreatedAt)
		}
		if rows[i].Config != rows[j].Config {
			return rows[i].Config < rows[j].Config
		}
		return rows[i].Split < rows[j].Split
	})
	out := make([]SplitRow, 0, len(rows))
	for _, r := range rows {
		sr := SplitRow{Config: r.Config, Split: r.Split, Status: r.Status, Error: r.Error}
		if r.NumRows != nil {
			sr.NumRows = *r.NumRows
		}
		if r.NumBytesSource != nil {
			sr.NumBytesSource = *r.NumBytesSource
		}
		if r.NumBytesParquet != nil {
			sr.NumBytesParquet = *r.NumBytesParquet
		}
		if r.ConvertedCommit != nil {
			sr.ConvertedCommit = model.CommitSHA(*r.ConvertedCommit)
		}
		if doc := unwrapJSON(r.Features); len(doc) > 0 && string(doc) != "{}" && string(doc) != "null" {
			fs, err := features.ParseFeatures(doc)
			if err == nil {
				sr.Features = fs
			}
		}
		out = append(out, sr)
	}
	return out, nil
}

// Upsert writes rows for a Commit.
func (s *Store) Upsert(ctx context.Context, repoID uuid.UUID, commit model.CommitSHA, rows []SplitRow) error {
	if len(rows) == 0 {
		return nil
	}
	now := db.Now()
	models := make([]db.DatasetSplit, 0, len(rows))
	for i, r := range rows {
		feat := wrapJSON([]byte("{}"))
		if r.Features != nil {
			b, err := json.Marshal(r.Features)
			if err != nil {
				return err
			}
			feat = wrapJSON(b)
		}
		m := db.DatasetSplit{RepoID: repoID, Commit: string(commit), Config: r.Config, Split: r.Split, Status: r.Status,
			Error: r.Error, Features: feat}
		if r.Status != StatusPending && r.Status != StatusFailed {
			nr, ns, np := r.NumRows, r.NumBytesSource, r.NumBytesParquet
			m.NumRows, m.NumBytesSource, m.NumBytesParquet = &nr, &ns, &np
		}
		if r.ConvertedCommit != "" {
			c := string(r.ConvertedCommit)
			m.ConvertedCommit = &c
		}
		// Keep the conversion's order in created_at, a microsecond apart.
		m.CreatedAt = now.Add(timeStep(i))
		m.UpdatedAt = now
		models = append(models, m)
	}
	_, err := s.DB.NewInsert().Model(&models).
		On("CONFLICT (repo_id, commit_sha, config, split) DO UPDATE").
		Set("num_rows = EXCLUDED.num_rows").Set("num_bytes_source = EXCLUDED.num_bytes_source").
		Set("num_bytes_parquet = EXCLUDED.num_bytes_parquet").Set("features = EXCLUDED.features").
		Set("status = EXCLUDED.status").Set("error = EXCLUDED.error").Set("converted_commit = EXCLUDED.converted_commit").
		Set("created_at = EXCLUDED.created_at").Set("updated_at = EXCLUDED.updated_at").
		Exec(ctx)
	return err
}

// ReplaceSplits replaces every row of a Commit.
func (s *Store) ReplaceSplits(ctx context.Context, repoID uuid.UUID, commit model.CommitSHA, rows []SplitRow) error {
	if _, err := s.DB.NewDelete().Model((*db.DatasetSplit)(nil)).Where("repo_id = ?", repoID).
		Where("commit_sha = ?", string(commit)).Exec(ctx); err != nil {
		return err
	}
	return s.Upsert(ctx, repoID, commit, rows)
}

// DeleteOtherCommits removes the rows and statistics of every other Commit
// of a Repo, which the newer export replaced.
func (s *Store) DeleteOtherCommits(ctx context.Context, repoID uuid.UUID, keep model.CommitSHA) error {
	if _, err := s.DB.NewDelete().Model((*db.DatasetSplit)(nil)).Where("repo_id = ?", repoID).
		Where("commit_sha <> ?", string(keep)).Exec(ctx); err != nil {
		return err
	}
	_, err := s.DB.NewDelete().Model((*db.DatasetStat)(nil)).Where("repo_id = ?", repoID).
		Where("commit_sha <> ?", string(keep)).Exec(ctx)
	return err
}

// SaveInfo stores a Config's info document.
func (s *Store) SaveInfo(ctx context.Context, repoID uuid.UUID, info *ConfigInfo) error {
	b, err := json.Marshal(info)
	if err != nil {
		return err
	}
	return s.Blobs.Put(ctx, infoKey(repoID, info.Commit, info.Config), bytes.NewReader(b), int64(len(b)))
}

// MaxInfoBytes caps an info document read back.
const MaxInfoBytes = 64 << 20

// Info reads a Config's info document.
func (s *Store) Info(ctx context.Context, repoID uuid.UUID, commit model.CommitSHA, config string) (*ConfigInfo, error) {
	rc, _, err := s.Blobs.Get(ctx, infoKey(repoID, commit, config), nil)
	if errors.Is(err, blob.ErrNotFound) {
		return nil, ErrNoInfo
	}
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, MaxInfoBytes))
	if err != nil {
		return nil, err
	}
	var info ConfigInfo
	if err := json.Unmarshal(b, &info); err != nil {
		return nil, fmt.Errorf("info document of %s: %w", config, err)
	}
	return &info, nil
}

// statsColumn is the dataset_stats column_name of a Split's summary row,
// which holds num_examples and partial; real columns are never empty.
const statsColumn = ""

// SaveStats stores a Split's statistics, one row per column.
func (s *Store) SaveStats(ctx context.Context, repoID uuid.UUID, commit model.CommitSHA, config, split string, doc *StatsDoc) error {
	now := db.Now()
	summary, err := json.Marshal(map[string]any{"num_examples": doc.NumExamples, "partial": doc.Partial, "error": doc.Error})
	if err != nil {
		return err
	}
	rows := []db.DatasetStat{{RepoID: repoID, Commit: string(commit), Config: config, Split: split, ColumnName: statsColumn, Stats: wrapJSON(summary)}}
	for _, item := range doc.Statistics {
		var head struct {
			Name string `json:"column_name"`
		}
		if err := json.Unmarshal(item, &head); err != nil || head.Name == "" {
			return fmt.Errorf("a statistics item has no column name")
		}
		rows = append(rows, db.DatasetStat{RepoID: repoID, Commit: string(commit), Config: config, Split: split, ColumnName: head.Name, Stats: wrapJSON(item)})
	}
	for i := range rows {
		rows[i].CreatedAt, rows[i].UpdatedAt = now, now
	}
	return s.DB.InTx(ctx, func(ctx context.Context, tx bunTx) error {
		if _, err := tx.NewDelete().Model((*db.DatasetStat)(nil)).Where("repo_id = ?", repoID).Where("commit_sha = ?", string(commit)).
			Where("config = ?", config).Where("split = ?", split).Exec(ctx); err != nil {
			return err
		}
		_, err := tx.NewInsert().Model(&rows).Exec(ctx)
		return err
	})
}

// ErrNoStats means a Split's statistics are not computed yet.
var ErrNoStats = errors.New("statistics not computed yet")

// Stats reads a Split's statistics, sorted by column name.
func (s *Store) Stats(ctx context.Context, repoID uuid.UUID, commit model.CommitSHA, config, split string) (*StatsDoc, error) {
	var rows []db.DatasetStat
	if err := s.DB.NewSelect().Model(&rows).Where("repo_id = ?", repoID).Where("commit_sha = ?", string(commit)).
		Where("config = ?", config).Where("split = ?", split).Scan(ctx); err != nil {
		return nil, err
	}
	// Sort in Go: Postgres collations do not sort by code point.
	sort.Slice(rows, func(i, j int) bool { return rows[i].ColumnName < rows[j].ColumnName })
	doc := &StatsDoc{Statistics: []json.RawMessage{}}
	found := false
	for _, r := range rows {
		if r.ColumnName == statsColumn {
			var sum struct {
				NumExamples int64  `json:"num_examples"`
				Partial     bool   `json:"partial"`
				Error       string `json:"error"`
			}
			if err := json.Unmarshal(unwrapJSON(r.Stats), &sum); err != nil {
				return nil, err
			}
			doc.NumExamples, doc.Partial, doc.Error, found = sum.NumExamples, sum.Partial, sum.Error, true
			continue
		}
		doc.Statistics = append(doc.Statistics, json.RawMessage(unwrapJSON(r.Stats)))
	}
	if !found {
		return nil, ErrNoStats
	}
	return doc, nil
}
