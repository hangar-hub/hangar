package viewer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/jamesagarside/hangar/internal/auth"
	"github.com/jamesagarside/hangar/internal/datasets"
	"github.com/jamesagarside/hangar/internal/datasets/convert"
	"github.com/jamesagarside/hangar/internal/datasets/features"
	"github.com/jamesagarside/hangar/internal/datasets/pipeline"
	"github.com/jamesagarside/hangar/internal/datasets/pq"
	"github.com/jamesagarside/hangar/internal/model"
)

// First-rows limits from the Hub's worker configuration.
const (
	firstRowsMaxBytes     = 1_000_000
	firstRowsMinNumber    = 10
	firstRowsMinCellBytes = 100
)

// IsValid implements datasets.Viewer.
func (s *Service) IsValid(ctx context.Context, p model.Principal, id model.RepoID) (*datasets.IsValidResponse, error) {
	resp, _, err := s.IsValidFor(ctx, p, id, "", "")
	return resp, err
}

// IsValidFor answers /is-valid for a dataset, a Config or a Split.
func (s *Service) IsValidFor(ctx context.Context, p model.Principal, id model.RepoID, config, split string) (*datasets.IsValidResponse, *state, error) {
	st, err := s.head(ctx, p, id, auth.ActionReadFiles)
	if err != nil {
		return nil, nil, err
	}
	out := &datasets.IsValidResponse{}
	if st.failed != "" {
		return out, st, nil
	}
	if config != "" && !st.hasConfig(config) {
		return nil, st, fmt.Errorf("%w: config %s", datasets.ErrNotFound, config)
	}
	if split != "" {
		if _, ok := st.split(config, split); !ok {
			return nil, st, fmt.Errorf("%w: split %s", datasets.ErrNotFound, split)
		}
	}
	for _, r := range st.rows {
		if (config != "" && r.Config != config) || (split != "" && r.Split != split) {
			continue
		}
		switch {
		case r.Ready():
			out.Preview, out.Viewer, out.Filter = true, true, true
			if searchable(r.Features) {
				out.Search = true
			}
			if doc, err := s.d.Store.Stats(ctx, st.repo.UUID, st.commit, r.Config, r.Split); err == nil && doc.Error == "" {
				out.Statistics = true
			}
		case r.Status == pipeline.StatusPending:
			if b := s.builderOf(ctx, st, r.Config); convert.HeadBuilders[b] {
				out.Preview = true
			}
		}
	}
	return out, st, nil
}

// builderOf returns a Config's builder from the layout.
func (s *Service) builderOf(ctx context.Context, st *state, config string) string {
	lay := st.layout
	if lay == nil {
		l, err := s.resolveLayout(ctx, st.repo, st.commit)
		if err != nil {
			return ""
		}
		lay = l
	}
	if c, ok := lay.Config(config); ok {
		return c.Builder
	}
	return ""
}

// searchable is get_indexable_columns: a column with a string anywhere.
func searchable(fs features.Features) bool { return len(searchColumns(fs)) > 0 }

func searchColumns(fs features.Features) []int {
	var out []int
	for i, f := range fs {
		if f.Feature.Contains(func(x *features.Feature) bool {
			return (x.Kind == features.KindValue && (x.Dtype == "string" || x.Dtype == "large_string")) ||
				x.Kind == features.KindJSON || x.Kind == features.KindTranslation || x.Kind == features.KindTranslationVariableLanguages
		}) {
			out = append(out, i)
		}
	}
	return out
}

// Splits implements datasets.Viewer.
func (s *Service) Splits(ctx context.Context, p model.Principal, id model.RepoID) (*datasets.SplitsResponse, error) {
	resp, _, err := s.SplitsFor(ctx, p, id, "")
	return resp, err
}

// SplitsFor answers /splits for a dataset or a Config.
func (s *Service) SplitsFor(ctx context.Context, p model.Principal, id model.RepoID, config string) (*datasets.SplitsResponse, *state, error) {
	st, err := s.head(ctx, p, id, auth.ActionRead)
	if err != nil {
		return nil, nil, err
	}
	if st.failed != "" {
		return nil, st, failure(st.failed)
	}
	if config != "" && !st.hasConfig(config) {
		return nil, st, fmt.Errorf("%w: config %s", datasets.ErrNotFound, config)
	}
	out := &datasets.SplitsResponse{Splits: []datasets.SplitRef{}, Pending: []datasets.ConfigRef{}, Failed: []datasets.FailedConfig{}}
	for _, r := range st.rows {
		if config != "" && r.Config != config {
			continue
		}
		out.Splits = append(out.Splits, datasets.SplitRef{Dataset: st.dataset(), Config: r.Config, Split: r.Split})
	}
	return out, st, nil
}

// FirstRows implements datasets.Viewer.
func (s *Service) FirstRows(ctx context.Context, p model.Principal, id model.RepoID, config, split string) (*datasets.FirstRowsResponse, error) {
	resp, _, err := s.FirstRowsFor(ctx, p, id, config, split)
	return resp, err
}

// FirstRowsFor answers /first-rows: the first 100 rows, truncated to
// about 1 MB as the Hub does, read from the Parquet export or, while the
// conversion runs, from source files that can be read from the start.
func (s *Service) FirstRowsFor(ctx context.Context, p model.Principal, id model.RepoID, config, split string) (*datasets.FirstRowsResponse, *state, error) {
	st, err := s.head(ctx, p, id, auth.ActionReadFiles)
	if err != nil {
		return nil, nil, err
	}
	var fs features.Features
	var rows [][]any
	// more is the Hub's "not all_fetched": the Split has more rows than
	// the response can hold.
	more := false
	_, info, err := s.readySplit(ctx, st, config, split)
	switch {
	case err == nil:
		t, err := s.table(st, info, split)
		if err != nil {
			return nil, st, err
		}
		rows, err = t.read(ctx, nil, 0, datasets.MaxRowsPerPage)
		if err != nil {
			return nil, st, tooBig(err, http.StatusNotImplemented)
		}
		fs = info.Features
		for _, sp := range info.Splits {
			if sp.Name == split {
				more = sp.NumExamples > datasets.MaxRowsPerPage
			}
		}
	case errors.Is(err, datasets.ErrNotReady):
		fs, rows, err = s.headRows(ctx, st, config, split)
		if err != nil {
			return nil, st, err
		}
		if len(rows) > datasets.MaxRowsPerPage {
			rows, more = rows[:datasets.MaxRowsPerPage], true
		}
	default:
		return nil, st, err
	}
	storage, err := fs.StorageSchema()
	if err != nil {
		return nil, st, err
	}
	feats, err := featureList(fs)
	if err != nil {
		return nil, st, err
	}
	idx := make([]int64, len(rows))
	for i := range idx {
		idx[i] = int64(i)
	}
	items := s.rowItems(st, config, split, fs, storage, rows, idx, 0)
	var keep []string
	for _, f := range fs {
		if f.Feature.IsMedia() || (f.Feature.IsListKind() && f.Feature.Elem.IsMedia()) {
			keep = append(keep, f.Name)
		}
	}
	items, truncated := truncateRows(items, keep)
	return &datasets.FirstRowsResponse{Dataset: st.dataset(), Config: config, Split: split, Features: feats, Rows: items,
		Truncated: more || truncated}, st, nil
}

// headRows reads up to one more than the first page of rows from source
// files while a Split converts, so the caller knows whether there are more.
func (s *Service) headRows(ctx context.Context, st *state, config, split string) (features.Features, [][]any, error) {
	lay, err := s.resolveLayout(ctx, st.repo, st.commit)
	if err != nil {
		return nil, nil, datasets.ErrNotReady
	}
	c, ok := lay.Config(config)
	if !ok || !convert.HeadBuilders[c.Builder] {
		return nil, nil, datasets.ErrNotReady
	}
	files, err := pipeline.SourceFiles(ctx, s.d.Repos, st.repo.ID, st.commit)
	if err != nil {
		return nil, nil, err
	}
	fs, rows, err := convert.HeadWithin(ctx, pq.NewBudget(s.o.MaxRequestBytes), c, split, files, datasets.MaxRowsPerPage+1)
	if errors.Is(err, pq.ErrTooBig) {
		return nil, nil, tooBig(err, http.StatusNotImplemented)
	}
	if err != nil {
		s.d.Log.DebugContext(ctx, "first rows from source files failed", "error", err.Error())
		return nil, nil, datasets.ErrNotReady
	}
	return fs, rows, nil
}

// truncateRows is create_truncated_row_items: keep at least 10 rows, then
// rows while under 1 MB; if the first rows are already over, cut their
// cells to 100 bytes from the last row backwards.
func truncateRows(rows []datasets.Row, keep []string) ([]datasets.Row, bool) {
	size := 2
	var out []datasets.Row
	for i, r := range rows {
		n := jsonSize(r) + 1
		if i >= firstRowsMinNumber && size+n >= firstRowsMaxBytes {
			return out, true
		}
		size += n
		out = append(out, r)
		if i == firstRowsMinNumber-1 && size >= firstRowsMaxBytes {
			break
		}
	}
	if size < firstRowsMaxBytes {
		return out, len(out) < len(rows)
	}
	for i := len(out) - 1; i >= 0 && size >= firstRowsMaxBytes; i-- {
		before := jsonSize(out[i])
		for j, c := range out[i].Row {
			skip := false
			for _, k := range keep {
				if k == c.Name {
					skip = true
				}
			}
			if skip {
				continue
			}
			if cut, ok := truncateCell(c.Value, firstRowsMinCellBytes); ok {
				out[i].Row[j].Value = cut
				out[i].TruncatedCells = append(out[i].TruncatedCells, c.Name)
			}
		}
		size += jsonSize(out[i]) - before
	}
	return out, true
}

// Rows implements datasets.Viewer.
func (s *Service) Rows(ctx context.Context, p model.Principal, req datasets.RowsRequest) (*datasets.RowsResponse, error) {
	resp, _, err := s.RowsFor(ctx, p, req)
	return resp, err
}

func checkPage(offset int64, length int) error {
	switch {
	case offset < 0:
		return fmt.Errorf("%w: Parameter 'offset' must be positive", ErrInvalidParameter)
	case length < 0:
		return fmt.Errorf("%w: Parameter 'length' must be positive", ErrInvalidParameter)
	case length > datasets.MaxRowsPerPage:
		return fmt.Errorf("%w: Parameter 'length' must not be greater than %d", ErrInvalidParameter, datasets.MaxRowsPerPage)
	}
	return nil
}

// RowsFor answers /rows.
func (s *Service) RowsFor(ctx context.Context, p model.Principal, req datasets.RowsRequest) (*datasets.RowsResponse, *state, error) {
	if err := checkPage(req.Offset, req.Length); err != nil {
		return nil, nil, err
	}
	st, err := s.head(ctx, p, req.Repo, auth.ActionReadFiles)
	if err != nil {
		return nil, nil, err
	}
	_, info, err := s.readySplit(ctx, st, req.Config, req.Split)
	if err != nil {
		return nil, st, err
	}
	t, err := s.table(st, info, req.Split)
	if err != nil {
		return nil, st, err
	}
	rows, err := t.read(ctx, nil, req.Offset, int64(req.Length))
	if err != nil {
		return nil, st, err
	}
	idx := make([]int64, len(rows))
	for i := range idx {
		idx[i] = req.Offset + int64(i)
	}
	return s.page(st, info, req.Config, req.Split, t, rows, idx, t.numRows(), info.Partial), st, nil
}

func (s *Service) page(st *state, info *pipeline.ConfigInfo, config, split string, t *table, rows [][]any, idx []int64, total int64, partial bool) *datasets.RowsResponse {
	feats, _ := featureList(info.Features)
	return &datasets.RowsResponse{Features: feats, Rows: s.rowItems(st, config, split, t.features, t.storage, rows, idx, s.o.MaxCellBytes),
		NumRowsTotal: total, NumRowsPerPage: datasets.MaxRowsPerPage, Partial: partial}
}

// Parquet implements datasets.Viewer.
func (s *Service) Parquet(ctx context.Context, p model.Principal, id model.RepoID, config string) (*datasets.ParquetResponse, error) {
	resp, _, err := s.ParquetFor(ctx, p, id, config)
	return resp, err
}

// exportURL is a Parquet file's resolve URL on the export ref.
func (s *Service) exportURL(id model.RepoID, p string) string {
	parts := strings.Split(p, "/")
	for i, x := range parts {
		parts[i] = url.PathEscape(x)
	}
	return strings.TrimSuffix(s.o.PublicURL, "/") + "/" + id.Path() + "/resolve/" + url.PathEscape(pipeline.ExportRef) + "/" + strings.Join(parts, "/")
}

// pendingFailed lists Configs still converting and Configs that failed.
func pendingFailed(st *state, only string) ([]datasets.ConfigRef, []datasets.FailedConfig) {
	pending := []datasets.ConfigRef{}
	failed := []datasets.FailedConfig{}
	for _, c := range st.configs() {
		if only != "" && c != only {
			continue
		}
		var errMsg string
		isPending := false
		for _, r := range st.rows {
			if r.Config != c {
				continue
			}
			switch r.Status {
			case pipeline.StatusPending:
				isPending = true
			case pipeline.StatusFailed:
				errMsg = r.Error
			}
		}
		switch {
		case isPending:
			pending = append(pending, datasets.ConfigRef{Dataset: st.dataset(), Config: c})
		case errMsg != "":
			failed = append(failed, datasets.FailedConfig{Dataset: st.dataset(), Config: c,
				Error: map[string]any{"error": pipeline.FailureOf(errMsg).Message}})
		}
	}
	return pending, failed
}

// ParquetFor answers /parquet for a dataset or a Config.
func (s *Service) ParquetFor(ctx context.Context, p model.Principal, id model.RepoID, config string) (*datasets.ParquetResponse, *state, error) {
	st, err := s.head(ctx, p, id, auth.ActionReadFiles)
	if err != nil {
		return nil, nil, err
	}
	if st.failed != "" {
		return nil, st, failure(st.failed)
	}
	if config != "" && !st.hasConfig(config) {
		return nil, st, fmt.Errorf("%w: config %s", datasets.ErrNotFound, config)
	}
	out := &datasets.ParquetResponse{ParquetFiles: []datasets.ParquetFile{}}
	for _, c := range st.configs() {
		if config != "" && c != config {
			continue
		}
		info, err := s.info(ctx, st, c)
		if errors.Is(err, pipeline.ErrNoInfo) {
			if config != "" {
				return nil, st, datasets.ErrNotReady
			}
			continue
		}
		if err != nil {
			return nil, st, err
		}
		out.Partial = out.Partial || info.Partial
		var files []pipeline.ShardInfo
		split := map[string]string{}
		for _, sp := range info.Splits {
			for _, sh := range sp.Shards {
				files = append(files, sh)
				split[sh.Path] = sp.Name
			}
		}
		// The Hub lists a Config's files by path.
		sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
		for _, sh := range files {
			out.ParquetFiles = append(out.ParquetFiles, datasets.ParquetFile{Dataset: st.dataset(), Config: c, Split: split[sh.Path],
				URL: s.exportURL(st.repo.ID, sh.Path), Filename: sh.Path[strings.LastIndex(sh.Path, "/")+1:], Size: sh.Size})
		}
		if config != "" {
			b, err := json.Marshal(info.Features)
			if err != nil {
				return nil, st, err
			}
			out.Features = b
		}
	}
	if config == "" {
		out.Pending, out.Failed = pendingFailed(st, "")
	}
	return out, st, nil
}

// infoDoc is a Config's /info entry, with the Hub's key order.
func (s *Service) infoDoc(st *state, info *pipeline.ConfigInfo) (object, error) {
	featJSON, err := json.Marshal(info.Features)
	if err != nil {
		return nil, err
	}
	compat, err := backwardCompat(featJSON)
	if err != nil {
		return nil, err
	}
	splits := object{}
	var datasetSize, downloadSize int64
	for _, sp := range info.Splits {
		var dn any = info.DatasetName
		if info.Copied {
			dn = nil
		}
		splits = append(splits, datasets.Cell{Name: sp.Name, Value: object{{Name: "name", Value: sp.Name},
			{Name: "num_bytes", Value: sp.NumBytes}, {Name: "num_examples", Value: sp.NumExamples}, {Name: "dataset_name", Value: dn}}})
		datasetSize += sp.NumBytes
	}
	for _, f := range info.SourceFiles {
		downloadSize += f.Size
	}
	doc := object{{Name: "description", Value: ""}, {Name: "citation", Value: ""}, {Name: "homepage", Value: ""}, {Name: "license", Value: ""},
		{Name: "features", Value: rawJSON(compat)}, {Name: "builder_name", Value: info.Builder}, {Name: "dataset_name", Value: info.DatasetName},
		{Name: "config_name", Value: info.Config},
		{Name: "version", Value: object{{Name: "version_str", Value: "0.0.0"}, {Name: "major", Value: 0}, {Name: "minor", Value: 0}, {Name: "patch", Value: 0}}},
		{Name: "splits", Value: splits}}
	if info.Copied {
		doc = append(doc, datasets.Cell{Name: "download_size", Value: downloadSize}, datasets.Cell{Name: "dataset_size", Value: datasetSize})
		return doc, nil
	}
	checksums := object{}
	for _, f := range info.SourceFiles {
		checksums = append(checksums, datasets.Cell{Name: convert.DownloadChecksumKey(fmt.Sprintf("hf://datasets/%s@%s", st.dataset(), info.Commit), f.Path),
			Value: object{{Name: "num_bytes", Value: f.Size}, {Name: "checksum", Value: nil}}})
	}
	doc = append(doc, datasets.Cell{Name: "download_checksums", Value: checksums}, datasets.Cell{Name: "download_size", Value: downloadSize},
		datasets.Cell{Name: "dataset_size", Value: datasetSize}, datasets.Cell{Name: "size_in_bytes", Value: downloadSize + datasetSize})
	return doc, nil
}

// backwardCompat is backward_compat_features: /info writes variable
// length Lists in the old one-element list form, for datasets < 4.
func backwardCompat(featJSON []byte) ([]byte, error) {
	fs, err := features.ParseFeatures(featJSON)
	if err != nil {
		return nil, err
	}
	var conv func(f *features.Feature) *features.Feature
	conv = func(f *features.Feature) *features.Feature {
		c := *f
		if f.Elem != nil {
			c.Elem = conv(f.Elem)
		}
		if f.Fields != nil {
			c.Fields = make([]features.Field, len(f.Fields))
			for i, fl := range f.Fields {
				c.Fields[i] = features.Field{Name: fl.Name, Feature: conv(fl.Feature)}
			}
		}
		if c.Kind == features.KindList && c.Length < 0 {
			c.Kind = features.KindLegacyList
		}
		return &c
	}
	out := make(features.Features, len(fs))
	for i, f := range fs {
		out[i] = features.Field{Name: f.Name, Feature: conv(f.Feature)}
	}
	return json.Marshal(out)
}

// Info implements datasets.Viewer: the Hub's /info JSON.
func (s *Service) Info(ctx context.Context, p model.Principal, id model.RepoID, config string) (json.RawMessage, error) {
	b, _, err := s.InfoFor(ctx, p, id, config)
	return b, err
}

// InfoFor answers /info for a dataset or a Config.
func (s *Service) InfoFor(ctx context.Context, p model.Principal, id model.RepoID, config string) (json.RawMessage, *state, error) {
	st, err := s.head(ctx, p, id, auth.ActionRead)
	if err != nil {
		return nil, nil, err
	}
	if st.failed != "" {
		return nil, st, failure(st.failed)
	}
	if config != "" {
		if !st.hasConfig(config) {
			return nil, st, fmt.Errorf("%w: config %s", datasets.ErrNotFound, config)
		}
		info, err := s.info(ctx, st, config)
		if errors.Is(err, pipeline.ErrNoInfo) {
			return nil, st, s.configNotReady(st, config)
		}
		if err != nil {
			return nil, st, err
		}
		doc, err := s.infoDoc(st, info)
		if err != nil {
			return nil, st, err
		}
		b, err := json.Marshal(object{{Name: "dataset_info", Value: doc}, {Name: "partial", Value: info.Partial}})
		return b, st, err
	}
	all := object{}
	partial := false
	for _, c := range st.configs() {
		info, err := s.info(ctx, st, c)
		if errors.Is(err, pipeline.ErrNoInfo) {
			continue
		}
		if err != nil {
			return nil, st, err
		}
		doc, err := s.infoDoc(st, info)
		if err != nil {
			return nil, st, err
		}
		all = append(all, datasets.Cell{Name: c, Value: doc})
		partial = partial || info.Partial
	}
	pending, failed := pendingFailed(st, "")
	b, err := json.Marshal(object{{Name: "dataset_info", Value: all}, {Name: "pending", Value: pending}, {Name: "failed", Value: failed},
		{Name: "partial", Value: partial}})
	return b, st, err
}

// configNotReady is the error for a Config without converted info.
func (s *Service) configNotReady(st *state, config string) error {
	for _, r := range st.rows {
		if r.Config == config && r.Status == pipeline.StatusFailed {
			return failure(r.Error)
		}
	}
	return datasets.ErrNotReady
}

// Size implements datasets.Viewer: the Hub's /size JSON.
func (s *Service) Size(ctx context.Context, p model.Principal, id model.RepoID, config string) (json.RawMessage, error) {
	b, _, err := s.SizeFor(ctx, p, id, config)
	return b, err
}

type sizeTotals struct {
	original, parquet, memory, rows int64
}

func (s *Service) configSize(st *state, info *pipeline.ConfigInfo) (object, []any, sizeTotals) {
	var t sizeTotals
	for _, f := range info.SourceFiles {
		t.original += f.Size
	}
	var splits []any
	cols := len(info.Features)
	for _, sp := range info.Splits {
		var pq int64
		for _, sh := range sp.Shards {
			pq += sh.Size
		}
		t.parquet += pq
		t.memory += sp.NumBytes
		t.rows += sp.NumExamples
		splits = append(splits, object{{Name: "dataset", Value: st.dataset()}, {Name: "config", Value: info.Config}, {Name: "split", Value: sp.Name},
			{Name: "num_bytes_parquet_files", Value: pq}, {Name: "num_bytes_memory", Value: sp.NumBytes}, {Name: "num_rows", Value: sp.NumExamples},
			{Name: "num_columns", Value: cols}, {Name: "estimated_num_rows", Value: nil}})
	}
	cfg := object{{Name: "dataset", Value: st.dataset()}, {Name: "config", Value: info.Config}, {Name: "num_bytes_original_files", Value: t.original},
		{Name: "num_bytes_parquet_files", Value: t.parquet}, {Name: "num_bytes_memory", Value: t.memory}, {Name: "num_rows", Value: t.rows},
		{Name: "num_columns", Value: cols}, {Name: "estimated_num_rows", Value: nil}}
	return cfg, splits, t
}

// SizeFor answers /size for a dataset or a Config.
func (s *Service) SizeFor(ctx context.Context, p model.Principal, id model.RepoID, config string) (json.RawMessage, *state, error) {
	st, err := s.head(ctx, p, id, auth.ActionRead)
	if err != nil {
		return nil, nil, err
	}
	if st.failed != "" {
		return nil, st, failure(st.failed)
	}
	if config != "" {
		if !st.hasConfig(config) {
			return nil, st, fmt.Errorf("%w: config %s", datasets.ErrNotFound, config)
		}
		info, err := s.info(ctx, st, config)
		if errors.Is(err, pipeline.ErrNoInfo) {
			return nil, st, s.configNotReady(st, config)
		}
		if err != nil {
			return nil, st, err
		}
		cfg, splits, _ := s.configSize(st, info)
		b, err := json.Marshal(object{{Name: "size", Value: object{{Name: "config", Value: cfg}, {Name: "splits", Value: orEmpty(splits)}}},
			{Name: "partial", Value: info.Partial}})
		return b, st, err
	}
	var configs, splits []any
	var total sizeTotals
	partial := false
	for _, c := range st.configs() {
		info, err := s.info(ctx, st, c)
		if errors.Is(err, pipeline.ErrNoInfo) {
			continue
		}
		if err != nil {
			return nil, st, err
		}
		cfg, sp, t := s.configSize(st, info)
		configs = append(configs, cfg)
		splits = append(splits, sp...)
		total.original += t.original
		total.parquet += t.parquet
		total.memory += t.memory
		total.rows += t.rows
		partial = partial || info.Partial
	}
	ds := object{{Name: "dataset", Value: st.dataset()}, {Name: "num_bytes_original_files", Value: total.original},
		{Name: "num_bytes_parquet_files", Value: total.parquet}, {Name: "num_bytes_memory", Value: total.memory},
		{Name: "num_rows", Value: total.rows}, {Name: "estimated_num_rows", Value: nil}}
	pending, failed := pendingFailed(st, "")
	b, err := json.Marshal(object{{Name: "size", Value: object{{Name: "dataset", Value: ds}, {Name: "configs", Value: orEmpty(configs)},
		{Name: "splits", Value: orEmpty(splits)}}}, {Name: "pending", Value: pending}, {Name: "failed", Value: failed}, {Name: "partial", Value: partial}})
	return b, st, err
}

func orEmpty(v []any) []any {
	if v == nil {
		return []any{}
	}
	return v
}

// Statistics implements datasets.Viewer: the Hub's /statistics JSON.
func (s *Service) Statistics(ctx context.Context, p model.Principal, id model.RepoID, config, split string) (json.RawMessage, error) {
	b, _, err := s.StatisticsFor(ctx, p, id, config, split)
	return b, err
}

// StatisticsFor answers /statistics.
func (s *Service) StatisticsFor(ctx context.Context, p model.Principal, id model.RepoID, config, split string) (json.RawMessage, *state, error) {
	st, err := s.head(ctx, p, id, auth.ActionReadFiles)
	if err != nil {
		return nil, nil, err
	}
	if _, _, err := s.readySplit(ctx, st, config, split); err != nil {
		return nil, st, err
	}
	doc, err := s.d.Store.Stats(ctx, st.repo.UUID, st.commit, config, split)
	if errors.Is(err, pipeline.ErrNoStats) {
		if _, err := s.d.Jobs.Enqueue(ctx, pipeline.StatsJob(st.repo.UUID, st.commit)); err != nil {
			s.d.Log.WarnContext(ctx, "could not queue dataset statistics", "error", err.Error())
		}
		return nil, st, datasets.ErrNotReady
	}
	if err != nil {
		return nil, st, err
	}
	if doc.Error != "" {
		return nil, st, failure(doc.Error)
	}
	b, err := json.Marshal(object{{Name: "num_examples", Value: doc.NumExamples}, {Name: "statistics", Value: doc.Statistics},
		{Name: "partial", Value: doc.Partial}})
	return b, st, err
}
