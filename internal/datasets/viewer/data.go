package viewer

import (
	"context"
	"fmt"
	"sync"

	"github.com/google/uuid"

	"github.com/jamesagarside/hangar/internal/datasets"
	"github.com/jamesagarside/hangar/internal/datasets/features"
	"github.com/jamesagarside/hangar/internal/datasets/pipeline"
	"github.com/jamesagarside/hangar/internal/datasets/pq"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/repo"
)

// fileCache keeps open Parquet files, so their footers are read once.
// Exports are immutable: a key names a file at an export Commit.
type fileCache struct {
	mu    sync.Mutex
	max   int
	files map[string]*pq.File
	order []string
}

func newFileCache(n int) *fileCache { return &fileCache{max: n, files: map[string]*pq.File{}} }

func (c *fileCache) get(repos repo.Service, repoUUID uuid.UUID, id model.RepoID, commit model.CommitSHA, sh pipeline.ShardInfo) (*pq.File, error) {
	key := repoUUID.String() + "@" + string(commit) + "/" + sh.Path
	c.mu.Lock()
	if f, ok := c.files[key]; ok {
		c.mu.Unlock()
		return f, nil
	}
	c.mu.Unlock()
	// Cached files outlive requests, so they read with a background context.
	f, err := pq.Open(pipeline.OpenRepoFile(context.Background(), repos, id, commit, sh.Path, sh.Size), sh.Size)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", sh.Path, err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.files[key]; !ok {
		if len(c.order) >= c.max {
			delete(c.files, c.order[0])
			c.order = c.order[1:]
		}
		c.files[key] = f
		c.order = append(c.order, key)
	}
	return f, nil
}

// table gives row access to a converted Split for one request.
type table struct {
	s        *Service
	st       *state
	info     *pipeline.ConfigInfo
	split    *pipeline.SplitInfo
	features features.Features
	storage  *features.Arrow
	// budget limits the decoded Parquet data the request holds; reads
	// that would go over fail with pq.ErrTooBig.
	budget *pq.Budget
}

func (s *Service) table(st *state, info *pipeline.ConfigInfo, split string) (*table, error) {
	si, ok := splitInfo(info, split)
	if !ok {
		return nil, fmt.Errorf("%w: split %s", datasets.ErrNotFound, split)
	}
	storage, err := info.Features.StorageSchema()
	if err != nil {
		return nil, err
	}
	return &table{s: s, st: st, info: info, split: si, features: info.Features, storage: storage,
		budget: pq.NewBudget(s.o.MaxRequestBytes)}, nil
}

func (t *table) numRows() int64 { return t.split.NumExamples }

func (t *table) file(i int) (*pq.File, error) {
	return t.s.files.get(t.s.d.Repos, t.st.repo.UUID, t.st.repo.ID, t.info.ExportCommit, t.split.Shards[i])
}

// columns returns the indexes of named columns in a shard.
func columns(f *pq.File, names []string) ([]int, error) { return f.Columns(names) }

// read returns rows [offset, offset+n) of the given columns (nil means all).
func (t *table) read(ctx context.Context, names []string, offset, n int64) ([][]any, error) {
	var out [][]any
	var start int64
	for i, sh := range t.split.Shards {
		if n <= 0 {
			break
		}
		end := start + sh.NumRows
		if offset < end {
			f, err := t.file(i)
			if err != nil {
				return nil, err
			}
			cols, err := columns(f, names)
			if err != nil {
				return nil, err
			}
			local := offset - start
			k := min(n, sh.NumRows-local)
			rows, err := f.ReadRowsWithin(t.budget, cols, local, k)
			if err != nil {
				return nil, err
			}
			out = append(out, rows...)
			offset += int64(len(rows))
			n -= int64(len(rows))
		}
		start = end
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// featureList is to_features_list.
func featureList(fs features.Features) ([]datasets.Feature, error) {
	out := make([]datasets.Feature, len(fs))
	for i, f := range fs {
		b, err := f.Feature.MarshalJSON()
		if err != nil {
			return nil, err
		}
		out[i] = datasets.Feature{FeatureIdx: i, Name: f.Name, Type: b}
	}
	return out, nil
}

// rowItems encodes rows as Hub row items; maxCell truncates large cells,
// or 0 to keep them whole.
func (s *Service) rowItems(st *state, config, split string, fs features.Features, storage *features.Arrow, rows [][]any, idx []int64, maxCell int) []datasets.Row {
	enc := &cellEncoder{assetURL: s.assetURL(st, config, split)}
	out := make([]datasets.Row, len(rows))
	for i, r := range rows {
		cells := make(datasets.Cells, len(fs))
		var truncated []string
		for j, f := range fs {
			v := enc.encode(r[j], f.Feature, storage.Fields[j].Type, idx[i], f.Name, nil)
			if maxCell > 0 && !f.Feature.IsMedia() {
				if cut, ok := truncateCell(v, maxCell); ok {
					v = cut
					truncated = append(truncated, f.Name)
				}
			}
			cells[j] = datasets.Cell{Name: f.Name, Value: v}
		}
		if truncated == nil {
			truncated = []string{}
		}
		out[i] = datasets.Row{RowIdx: idx[i], Row: cells, TruncatedCells: truncated}
	}
	return out
}
