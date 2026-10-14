package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/jamesagarside/hangar/internal/datasets/pq"
	"github.com/jamesagarside/hangar/internal/datasets/stats"
	"github.com/jamesagarside/hangar/internal/jobs"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/repo"
)

// HandleStats is the dataset.stats Job handler: it computes the column
// statistics of every converted Split of a Commit.
func (p *Pipeline) HandleStats(ctx context.Context, j *jobs.Job, progress jobs.Progress) error {
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
	rows, err := p.store.Splits(ctx, r.UUID, pl.Commit)
	if err != nil {
		return err
	}
	for i, row := range rows {
		if !row.Ready() {
			continue
		}
		p.report(ctx, progress, i, len(rows), "Computing statistics of "+row.Config+"/"+row.Split)
		info, err := p.store.Info(ctx, r.UUID, pl.Commit, row.Config)
		if errors.Is(err, ErrNoInfo) {
			continue
		}
		if err != nil {
			return err
		}
		doc, err := p.ComputeStats(ctx, r.ID, info, row.Split)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			doc = &StatsDoc{Error: err.Error(), Statistics: []json.RawMessage{}}
		}
		if err := p.store.SaveStats(ctx, r.UUID, pl.Commit, row.Config, row.Split, doc); err != nil {
			return err
		}
	}
	return nil
}

// ComputeStats computes a Split's statistics from its Parquet export,
// reading at most MaxStatsBytes of uncompressed Parquet data as the Hub
// does (always at least the first file).
func (p *Pipeline) ComputeStats(ctx context.Context, id model.RepoID, info *ConfigInfo, split string) (*StatsDoc, error) {
	var si *SplitInfo
	for i := range info.Splits {
		if info.Splits[i].Name == split {
			si = &info.Splits[i]
		}
	}
	if si == nil {
		return nil, fmt.Errorf("split %s has no Parquet export", split)
	}
	var files []*pq.File
	var budget int64
	partial := info.Partial
	for i, sh := range si.Shards {
		pf, err := pq.Open(OpenRepoFile(ctx, p.d.Repos, id, info.ExportCommit, sh.Path, sh.Size), sh.Size)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", sh.Path, err)
		}
		if i > 0 && budget+pf.UncompressedBytes() > p.d.MaxStatsBytes {
			partial = true
			break
		}
		budget += pf.UncompressedBytes()
		files = append(files, pf)
	}
	doc := &StatsDoc{Partial: partial, Statistics: []json.RawMessage{}}
	for _, f := range files {
		doc.NumExamples += f.NumRows()
	}
	var items []stats.Item
	for _, fl := range info.Features {
		if !stats.Supported(fl.Feature) {
			continue
		}
		var values []any
		for _, f := range files {
			idx, err := f.Columns([]string{fl.Name})
			if err != nil {
				return nil, err
			}
			sc, err := f.Scan(idx, 0, -1)
			if err != nil {
				return nil, err
			}
			for {
				_, row, err := sc.Next()
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					sc.Close()
					return nil, err
				}
				values = append(values, row[0])
			}
			sc.Close()
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		item, err := stats.Column(fl.Name, fl.Feature, values)
		if err != nil {
			return nil, fmt.Errorf("column %s: %w", fl.Name, err)
		}
		if item != nil {
			items = append(items, *item)
		}
	}
	stats.Sort(items)
	for _, it := range items {
		b, err := json.Marshal(it)
		if err != nil {
			return nil, err
		}
		doc.Statistics = append(doc.Statistics, b)
	}
	if len(items) == 0 {
		return nil, errors.New("no columns for statistics computation found; supported feature types are numbers, strings, ClassLabel, Image, Audio, List, datetime and bool")
	}
	return doc, nil
}
