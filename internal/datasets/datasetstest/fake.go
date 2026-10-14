// Package datasetstest provides fakes of datasets.Layout and
// datasets.Viewer over in-memory tables. The Viewer fake pages rows, runs
// substring search and supports a where clause of the form `"col" = 'value'`
// and a single-column orderby; anything else is ErrInvalidQuery, so tests
// never rely on query behaviour the fake does not have.
package datasetstest

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/jamesagarside/hangar/internal/datasets"
	"github.com/jamesagarside/hangar/internal/model"
)

// Layout is a fake datasets.Layout returning layouts set with Set.
type Layout struct {
	mu      sync.Mutex
	layouts map[string]*datasets.Resolved
}

var _ datasets.Layout = (*Layout)(nil)

// NewLayout returns an empty Layout.
func NewLayout() *Layout { return &Layout{layouts: map[string]*datasets.Resolved{}} }

// Set sets the layout of a dataset at a Commit.
func (l *Layout) Set(id model.RepoID, commit model.CommitSHA, r *datasets.Resolved) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.layouts[id.Path()+"@"+string(commit)] = r
}

// Resolve implements datasets.Layout.
func (l *Layout) Resolve(_ context.Context, id model.RepoID, commit model.CommitSHA) (*datasets.Resolved, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	r, ok := l.layouts[id.Path()+"@"+string(commit)]
	if !ok {
		return nil, fmt.Errorf("%w: %s@%s", datasets.ErrNotFound, id.Path(), commit)
	}
	return r, nil
}

// Files implements datasets.Layout like the real one: source patterns of
// the selected Splits, or `{config}/{split}/*.parquet` for Parquet-only.
func (l *Layout) Files(_ context.Context, layout *datasets.Resolved, sel datasets.Selection) ([]string, error) {
	var out []string
	for _, c := range layout.Configs {
		if len(sel.Configs) > 0 && !slices.Contains(sel.Configs, c.Name) {
			continue
		}
		for _, split := range c.Splits() {
			if len(sel.Splits) > 0 && !slices.Contains(sel.Splits, split) {
				continue
			}
			if sel.ParquetOnly {
				out = append(out, path.Join(c.Name, split, "*.parquet"), path.Join(c.Name, "partial-"+split, "*.parquet"))
				continue
			}
			out = append(out, c.DataFiles[split]...)
		}
	}
	for _, want := range sel.Configs {
		if _, ok := layout.Config(want); !ok {
			return nil, fmt.Errorf("%w: config %s", datasets.ErrNotFound, want)
		}
	}
	sort.Strings(out)
	return slices.Compact(out), nil
}

// Table is a Split's rows for the Viewer fake.
type Table struct {
	// Features gives each column's Hub Features JSON, in column order.
	Columns []string
	Types   []string
	Rows    []map[string]any
	// Ready false makes the Split "pending" (not converted yet).
	Ready bool
}

// Viewer is a fake datasets.Viewer. Readable decides access; nil allows all.
type Viewer struct {
	Readable func(p model.Principal, id model.RepoID) bool

	mu     sync.Mutex
	tables map[string]map[string]map[string]*Table // dataset -> config -> split
	files  map[string][]datasets.ParquetFile
}

var _ datasets.Viewer = (*Viewer)(nil)

// NewViewer returns an empty Viewer.
func NewViewer() *Viewer {
	return &Viewer{tables: map[string]map[string]map[string]*Table{}, files: map[string][]datasets.ParquetFile{}}
}

// SetTable sets a Split's rows.
func (v *Viewer) SetTable(id model.RepoID, config, split string, t *Table) {
	v.mu.Lock()
	defer v.mu.Unlock()
	ds := id.String()
	if v.tables[ds] == nil {
		v.tables[ds] = map[string]map[string]*Table{}
	}
	if v.tables[ds][config] == nil {
		v.tables[ds][config] = map[string]*Table{}
	}
	v.tables[ds][config][split] = t
}

// SetParquetFiles sets the Parquet export listing of a dataset.
func (v *Viewer) SetParquetFiles(id model.RepoID, files ...datasets.ParquetFile) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.files[id.String()] = files
}

func (v *Viewer) check(p model.Principal, id model.RepoID) error {
	if v.Readable != nil && !v.Readable(p, id) {
		return fmt.Errorf("%w: %s", datasets.ErrNotFound, id.String())
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if _, ok := v.tables[id.String()]; !ok {
		return fmt.Errorf("%w: %s", datasets.ErrNotFound, id.String())
	}
	return nil
}

func (v *Viewer) table(id model.RepoID, config, split string) (*Table, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	t, ok := v.tables[id.String()][config][split]
	if !ok {
		return nil, fmt.Errorf("%w: %s/%s", datasets.ErrNotFound, config, split)
	}
	if !t.Ready {
		return nil, datasets.ErrNotReady
	}
	return t, nil
}

func (v *Viewer) refs(id model.RepoID) (ready, pending []datasets.SplitRef) {
	v.mu.Lock()
	defer v.mu.Unlock()
	configs := v.tables[id.String()]
	names := make([]string, 0, len(configs))
	for c := range configs {
		names = append(names, c)
	}
	sort.Strings(names)
	for _, c := range names {
		splits := make([]string, 0, len(configs[c]))
		for s := range configs[c] {
			splits = append(splits, s)
		}
		sort.Strings(splits)
		for _, s := range splits {
			ref := datasets.SplitRef{Dataset: id.String(), Config: c, Split: s}
			if configs[c][s].Ready {
				ready = append(ready, ref)
			} else {
				pending = append(pending, ref)
			}
		}
	}
	return ready, pending
}

// IsValid implements datasets.Viewer.
func (v *Viewer) IsValid(_ context.Context, p model.Principal, id model.RepoID) (*datasets.IsValidResponse, error) {
	if err := v.check(p, id); err != nil {
		return nil, err
	}
	ready, _ := v.refs(id)
	ok := len(ready) > 0
	return &datasets.IsValidResponse{Preview: ok, Viewer: ok, Search: ok, Filter: ok, Statistics: ok}, nil
}

// Splits implements datasets.Viewer.
func (v *Viewer) Splits(_ context.Context, p model.Principal, id model.RepoID) (*datasets.SplitsResponse, error) {
	if err := v.check(p, id); err != nil {
		return nil, err
	}
	ready, pending := v.refs(id)
	resp := &datasets.SplitsResponse{Splits: append(ready, pending...), Pending: []datasets.ConfigRef{}, Failed: []datasets.FailedConfig{}}
	seen := map[string]bool{}
	for _, r := range pending {
		if !seen[r.Config] {
			seen[r.Config] = true
			resp.Pending = append(resp.Pending, datasets.ConfigRef{Dataset: r.Dataset, Config: r.Config})
		}
	}
	return resp, nil
}

func cells(t *Table, row map[string]any) datasets.Cells {
	out := make(datasets.Cells, 0, len(t.Columns))
	for _, c := range t.Columns {
		out = append(out, datasets.Cell{Name: c, Value: row[c]})
	}
	return out
}

func page(t *Table, idx []int, offset int64, length int) *datasets.RowsResponse {
	if length <= 0 || length > datasets.MaxRowsPerPage {
		length = datasets.MaxRowsPerPage
	}
	resp := &datasets.RowsResponse{NumRowsTotal: int64(len(idx)), NumRowsPerPage: length, Rows: []datasets.Row{}}
	for i, c := range t.Columns {
		typ := json.RawMessage(`{"dtype":"string","_type":"Value"}`)
		if i < len(t.Types) && t.Types[i] != "" {
			typ = json.RawMessage(t.Types[i])
		}
		resp.Features = append(resp.Features, datasets.Feature{FeatureIdx: i, Name: c, Type: typ})
	}
	for i := offset; i < int64(len(idx)) && i < offset+int64(length); i++ {
		resp.Rows = append(resp.Rows, datasets.Row{RowIdx: int64(idx[i]), Row: cells(t, t.Rows[idx[i]]), TruncatedCells: []string{}})
	}
	return resp
}

func all(t *Table) []int {
	idx := make([]int, len(t.Rows))
	for i := range idx {
		idx[i] = i
	}
	return idx
}

// FirstRows implements datasets.Viewer.
func (v *Viewer) FirstRows(ctx context.Context, p model.Principal, id model.RepoID, config, split string) (*datasets.FirstRowsResponse, error) {
	r, err := v.Rows(ctx, p, datasets.RowsRequest{Repo: id, Config: config, Split: split, Length: datasets.MaxRowsPerPage})
	if err != nil {
		return nil, err
	}
	return &datasets.FirstRowsResponse{Dataset: id.String(), Config: config, Split: split, Features: r.Features, Rows: r.Rows,
		Truncated: r.NumRowsTotal > int64(len(r.Rows))}, nil
}

// Rows implements datasets.Viewer.
func (v *Viewer) Rows(_ context.Context, p model.Principal, req datasets.RowsRequest) (*datasets.RowsResponse, error) {
	if err := v.check(p, req.Repo); err != nil {
		return nil, err
	}
	t, err := v.table(req.Repo, req.Config, req.Split)
	if err != nil {
		return nil, err
	}
	return page(t, all(t), req.Offset, req.Length), nil
}

// Search implements datasets.Viewer.
func (v *Viewer) Search(_ context.Context, p model.Principal, req datasets.SearchRequest) (*datasets.RowsResponse, error) {
	if err := v.check(p, req.Repo); err != nil {
		return nil, err
	}
	t, err := v.table(req.Repo, req.Config, req.Split)
	if err != nil {
		return nil, err
	}
	terms := strings.Fields(strings.ToLower(req.Query))
	var idx []int
	for i, row := range t.Rows {
		matched := true
		for _, term := range terms {
			found := false
			for _, val := range row {
				if s, ok := val.(string); ok && strings.Contains(strings.ToLower(s), term) {
					found = true
					break
				}
			}
			matched = matched && found
		}
		if matched {
			idx = append(idx, i)
		}
	}
	return page(t, idx, req.Offset, req.Length), nil
}

var (
	whereRe   = regexp.MustCompile(`^\s*"([^"]+)"\s*=\s*'([^']*)'\s*$`)
	orderByRe = regexp.MustCompile(`^\s*"([^"]+)"(?:\s+(ASC|DESC))?\s*$`)
)

// Filter implements datasets.Viewer.
func (v *Viewer) Filter(_ context.Context, p model.Principal, req datasets.FilterRequest) (*datasets.RowsResponse, error) {
	if err := v.check(p, req.Repo); err != nil {
		return nil, err
	}
	t, err := v.table(req.Repo, req.Config, req.Split)
	if err != nil {
		return nil, err
	}
	idx := all(t)
	if strings.TrimSpace(req.Where) != "" {
		m := whereRe.FindStringSubmatch(req.Where)
		if m == nil {
			return nil, fmt.Errorf("%w: the fake supports only \"col\" = 'value'", datasets.ErrInvalidQuery)
		}
		var kept []int
		for _, i := range idx {
			if fmt.Sprint(t.Rows[i][m[1]]) == m[2] {
				kept = append(kept, i)
			}
		}
		idx = kept
	}
	if strings.TrimSpace(req.OrderBy) != "" {
		m := orderByRe.FindStringSubmatch(req.OrderBy)
		if m == nil {
			return nil, fmt.Errorf("%w: the fake supports one \"col\" [ASC|DESC]", datasets.ErrInvalidQuery)
		}
		sort.SliceStable(idx, func(a, b int) bool {
			x, y := fmt.Sprint(t.Rows[idx[a]][m[1]]), fmt.Sprint(t.Rows[idx[b]][m[1]])
			if m[2] == "DESC" {
				return x > y
			}
			return x < y
		})
	}
	return page(t, idx, req.Offset, req.Length), nil
}

// Parquet implements datasets.Viewer.
func (v *Viewer) Parquet(_ context.Context, p model.Principal, id model.RepoID, config string) (*datasets.ParquetResponse, error) {
	if err := v.check(p, id); err != nil {
		return nil, err
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	resp := &datasets.ParquetResponse{ParquetFiles: []datasets.ParquetFile{}, Pending: []datasets.ConfigRef{}, Failed: []datasets.FailedConfig{}}
	for _, f := range v.files[id.String()] {
		if config == "" || f.Config == config {
			resp.ParquetFiles = append(resp.ParquetFiles, f)
		}
	}
	return resp, nil
}

func (v *Viewer) rowCounts(id model.RepoID, config string) map[string]any {
	v.mu.Lock()
	defer v.mu.Unlock()
	out := map[string]any{}
	for c, splits := range v.tables[id.String()] {
		if config != "" && c != config {
			continue
		}
		counts := map[string]any{}
		for s, t := range splits {
			counts[s] = map[string]any{"num_examples": len(t.Rows)}
		}
		out[c] = counts
	}
	return out
}

// Info implements datasets.Viewer.
func (v *Viewer) Info(_ context.Context, p model.Principal, id model.RepoID, config string) (json.RawMessage, error) {
	if err := v.check(p, id); err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{"dataset_info": v.rowCounts(id, config), "pending": []any{}, "failed": []any{}, "partial": false})
}

// Size implements datasets.Viewer.
func (v *Viewer) Size(_ context.Context, p model.Principal, id model.RepoID, config string) (json.RawMessage, error) {
	if err := v.check(p, id); err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{"size": map[string]any{"configs": v.rowCounts(id, config)}, "pending": []any{}, "failed": []any{}, "partial": false})
}

// Statistics implements datasets.Viewer with counts only.
func (v *Viewer) Statistics(_ context.Context, p model.Principal, id model.RepoID, config, split string) (json.RawMessage, error) {
	if err := v.check(p, id); err != nil {
		return nil, err
	}
	t, err := v.table(id, config, split)
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{"num_examples": len(t.Rows), "statistics": []any{}, "partial": false})
}

// Croissant implements datasets.Viewer with a minimal Croissant document.
func (v *Viewer) Croissant(_ context.Context, p model.Principal, id model.RepoID) (json.RawMessage, error) {
	if err := v.check(p, id); err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{"@context": map[string]any{"@vocab": "https://schema.org/", "cr": "http://mlcommons.org/croissant/"}, "@type": "sc:Dataset", "name": id.Name})
}
