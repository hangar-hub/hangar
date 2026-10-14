// Package datasets makes datasets first-class (docs/spec/14-datasets.md):
// Config and Split resolution (Layout), the Parquet export, statistics, the
// where and orderby query parser, the Hub-compatible Dataset Viewer API
// (Viewer) and Croissant metadata. datasetstest has fakes of Layout and
// Viewer.
package datasets

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sort"

	"github.com/jamesagarside/hangar/internal/datasets/features"
	"github.com/jamesagarside/hangar/internal/model"
)

// Errors. The Viewer handlers answer with the Hub's viewer error shape.
var (
	// ErrNotFound means the dataset, Config or Split does not exist, or the
	// caller cannot read the dataset (404, so private names do not leak).
	ErrNotFound = errors.New("dataset, config or split not found")
	// ErrNotReady means the Split is not converted to Parquet yet.
	ErrNotReady = errors.New("the split is not ready: Hangar is converting this dataset to Parquet")
	// ErrInvalidQuery means a where or orderby clause did not parse.
	ErrInvalidQuery = errors.New("invalid query")
)

// MaxRowsPerPage is the largest page /rows, /search and /filter return.
const MaxRowsPerPage = 100

// DefaultConfig is the Config name used when a dataset declares none.
const DefaultConfig = "default"

// Config is a resolved Config: the data files of each Split and the
// builder options from the card's `configs:` YAML (DATA-2).
type Config struct {
	Name    string `json:"config_name"`
	Default bool   `json:"default,omitempty"`
	// DataFiles maps a Split name to path patterns relative to the Repo root.
	DataFiles map[string][]string `json:"data_files"`
	DataDir   string              `json:"data_dir,omitempty"`
	// Builder is the `datasets` packaged builder that reads the files:
	// parquet, csv, json, text, arrow, webdataset, imagefolder, audiofolder,
	// or another name Hangar lists but cannot convert.
	Builder        string         `json:"builder,omitempty"`
	BuilderOptions map[string]any `json:"builder_options,omitempty"`
	// SplitOrder is the order of the Splits, as `datasets` reports them.
	SplitOrder []string `json:"split_order,omitempty"`
	// Files maps a Split name to the data files its patterns matched at the
	// Commit, in the order `datasets` reads them.
	Files map[string][]string `json:"files,omitempty"`
	// Features are declared by the card (the config's `features` or its
	// `dataset_info`); nil means they are inferred from the data.
	Features features.Features `json:"features,omitempty"`
}

// Splits returns the Config's Split names: SplitOrder when set, otherwise
// train, validation and test first and the rest sorted.
func (c Config) Splits() []string {
	if len(c.SplitOrder) > 0 {
		return slices.Clone(c.SplitOrder)
	}
	out := make([]string, 0, len(c.DataFiles))
	for _, s := range []string{"train", "validation", "test"} {
		if _, ok := c.DataFiles[s]; ok {
			out = append(out, s)
		}
	}
	var extra []string
	for s := range c.DataFiles {
		if s != "train" && s != "validation" && s != "test" {
			extra = append(extra, s)
		}
	}
	sort.Strings(extra)
	return append(out, extra...)
}

// Resolved is the Config and Split layout of a dataset Commit.
type Resolved struct {
	Configs []Config `json:"configs"`
	// FromCard is true when the layout came from the card's `configs:`
	// YAML, and false when it was inferred from file names.
	FromCard bool `json:"from_card"`
}

// Config returns the named Config.
func (r *Resolved) Config(name string) (Config, bool) {
	for _, c := range r.Configs {
		if c.Name == name {
			return c, true
		}
	}
	return Config{}, false
}

// Selection picks Configs and Splits for a partial Pull (DATA-8). Empty
// lists mean all.
type Selection struct {
	Configs []string `json:"configs,omitempty"`
	Splits  []string `json:"splits,omitempty"`
	// ParquetOnly pulls the Parquet export from refs/convert/parquet
	// instead of the source files.
	ParquetOnly bool `json:"parquet_only,omitempty"`
}

// Layout resolves Configs and Splits.
type Layout interface {
	// Resolve returns the layout of a dataset Repo at a Commit.
	Resolve(ctx context.Context, id model.RepoID, commit model.CommitSHA) (*Resolved, error)
	// Files turns a Selection into the file patterns to Pull. For
	// ParquetOnly selections the patterns are for refs/convert/parquet, in
	// the Hub's `{config}/{split}/` layout.
	Files(ctx context.Context, layout *Resolved, sel Selection) ([]string, error)
}

// Feature is one column in a Viewer response. Type is the Hub's Features
// JSON for the column, such as {"dtype":"string","_type":"Value"}.
type Feature struct {
	FeatureIdx int             `json:"feature_idx"`
	Name       string          `json:"name"`
	Type       json.RawMessage `json:"type"`
}

// Cell is one column of a row in a Viewer response.
type Cell struct {
	Name  string
	Value any
}

// Cells are a row's columns, in feature order. They marshal to a JSON
// object that keeps that order, as the Hub's responses do.
type Cells []Cell

// Get returns a column's value.
func (c Cells) Get(name string) (any, bool) {
	for _, x := range c {
		if x.Name == name {
			return x.Value, true
		}
	}
	return nil, false
}

// MarshalJSON writes the cells as an ordered JSON object.
func (c Cells) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, x := range c {
		if i > 0 {
			b.WriteByte(',')
		}
		k, err := json.Marshal(x.Name)
		if err != nil {
			return nil, err
		}
		b.Write(k)
		b.WriteByte(':')
		v, err := json.Marshal(x.Value)
		if err != nil {
			return nil, err
		}
		b.Write(v)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// Row is one row in a Viewer response.
type Row struct {
	RowIdx         int64    `json:"row_idx"`
	Row            Cells    `json:"row"`
	TruncatedCells []string `json:"truncated_cells"`
}

// RowsResponse is the Hub's shape for /rows, /search and /filter.
type RowsResponse struct {
	Features       []Feature `json:"features"`
	Rows           []Row     `json:"rows"`
	NumRowsTotal   int64     `json:"num_rows_total"`
	NumRowsPerPage int       `json:"num_rows_per_page"`
	Partial        bool      `json:"partial"`
}

// FirstRowsResponse is the Hub's shape for /first-rows.
type FirstRowsResponse struct {
	Dataset   string    `json:"dataset"`
	Config    string    `json:"config"`
	Split     string    `json:"split"`
	Features  []Feature `json:"features"`
	Rows      []Row     `json:"rows"`
	Truncated bool      `json:"truncated"`
}

// SplitRef names a Split.
type SplitRef struct {
	Dataset string `json:"dataset"`
	Config  string `json:"config"`
	Split   string `json:"split"`
}

// ConfigRef names a Config.
type ConfigRef struct {
	Dataset string `json:"dataset"`
	Config  string `json:"config"`
}

// FailedConfig is a Config that could not be processed, with the error.
type FailedConfig struct {
	Dataset string         `json:"dataset"`
	Config  string         `json:"config"`
	Error   map[string]any `json:"error"`
}

// SplitsResponse is /splits: the Splits of every Config whose Splits are
// known, the Configs still being processed and the Configs that failed.
type SplitsResponse struct {
	Splits  []SplitRef     `json:"splits"`
	Pending []ConfigRef    `json:"pending"`
	Failed  []FailedConfig `json:"failed"`
}

// IsValidResponse is /is-valid.
type IsValidResponse struct {
	Preview    bool `json:"preview"`
	Viewer     bool `json:"viewer"`
	Search     bool `json:"search"`
	Filter     bool `json:"filter"`
	Statistics bool `json:"statistics"`
}

// ParquetFile is one shard of the Parquet export.
type ParquetFile struct {
	Dataset  string `json:"dataset"`
	Config   string `json:"config"`
	Split    string `json:"split"`
	URL      string `json:"url"`
	Filename string `json:"filename"`
	Size     int64  `json:"size"`
}

// ParquetResponse is /parquet. For one Config, Features holds the
// Config's Features JSON and Pending and Failed are not sent.
type ParquetResponse struct {
	ParquetFiles []ParquetFile   `json:"parquet_files"`
	Pending      []ConfigRef     `json:"pending,omitempty"`
	Failed       []FailedConfig  `json:"failed,omitempty"`
	Features     json.RawMessage `json:"features,omitempty"`
	Partial      bool            `json:"partial"`
}

// RowsRequest is a /rows request; Length is at most MaxRowsPerPage.
type RowsRequest struct {
	Repo   model.RepoID
	Config string
	Split  string
	Offset int64
	Length int
}

// SearchRequest is a /search request: every term must match some string
// column, case-insensitively.
type SearchRequest struct {
	RowsRequest
	Query string
}

// FilterRequest is a /filter request with a where clause and an orderby list.
type FilterRequest struct {
	RowsRequest
	Where   string
	OrderBy string
}

// Viewer is the Dataset Viewer API (DATA-4). Every method applies the
// Repo's read permissions for p.
type Viewer interface {
	IsValid(ctx context.Context, p model.Principal, id model.RepoID) (*IsValidResponse, error)
	Splits(ctx context.Context, p model.Principal, id model.RepoID) (*SplitsResponse, error)
	FirstRows(ctx context.Context, p model.Principal, id model.RepoID, config, split string) (*FirstRowsResponse, error)
	Rows(ctx context.Context, p model.Principal, req RowsRequest) (*RowsResponse, error)
	Search(ctx context.Context, p model.Principal, req SearchRequest) (*RowsResponse, error)
	Filter(ctx context.Context, p model.Principal, req FilterRequest) (*RowsResponse, error)
	// Parquet lists the Parquet export, optionally for one Config.
	Parquet(ctx context.Context, p model.Principal, id model.RepoID, config string) (*ParquetResponse, error)
	// Info, Size and Statistics return the Hub's JSON for those endpoints.
	Info(ctx context.Context, p model.Principal, id model.RepoID, config string) (json.RawMessage, error)
	Size(ctx context.Context, p model.Principal, id model.RepoID, config string) (json.RawMessage, error)
	Statistics(ctx context.Context, p model.Principal, id model.RepoID, config, split string) (json.RawMessage, error)
	// Croissant returns MLCommons Croissant JSON-LD (DATA-10).
	Croissant(ctx context.Context, p model.Principal, id model.RepoID) (json.RawMessage, error)
}
