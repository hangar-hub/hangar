// Package index extracts metadata from a Commit's files: formats,
// architecture, parameter counts, quantisations, context length, chat
// templates, card data and licence (META-1, docs/spec/07-indexing-and-scanning.md).
// It reads headers with ranged reads and never a whole large file.
// indextest has an in-memory fake.
package index

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"sort"
	"strings"

	"github.com/google/uuid"

	"github.com/jamesagarside/hangar/internal/model"
)

// ErrNotIndexed means no results exist yet for the Commit. Readers never
// wait: the Hub API omits derived fields and the UI shows "Indexing".
var ErrNotIndexed = errors.New("commit is not indexed yet")

// ErrNoHeader means the file is not a safetensors or GGUF file.
var ErrNoHeader = errors.New("file has no header to show")

// Parameters counts tensor elements, in total and per dtype (the Hub's
// `safetensors.parameters` and `safetensors.total`).
type Parameters struct {
	Total   int64            `json:"total"`
	ByDType map[string]int64 `json:"by_dtype,omitempty"`
}

// Quantisation describes one GGUF file or group, or an MLX quantisation.
type Quantisation struct {
	Name     string   `json:"name"`
	FileType int      `json:"file_type,omitempty"`
	Bits     int      `json:"bits,omitempty"`
	Group    int      `json:"group_size,omitempty"`
	Files    []string `json:"files,omitempty"`
}

// Licence is a Repo's licence.
type Licence struct {
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
	Link string `json:"link,omitempty"`
}

// Size counts total bytes and bytes present (less in a Partial mirror).
type Size struct {
	Total   int64 `json:"total"`
	Present int64 `json:"present"`
}

// FileInfo is per-file index data for the file viewers.
type FileInfo struct {
	Path        string `json:"path"`
	Format      string `json:"format,omitempty"`
	DType       string `json:"dtype,omitempty"`
	Quant       string `json:"quant,omitempty"`
	TensorCount int    `json:"tensor_count,omitempty"`
	Parameters  int64  `json:"parameters,omitempty"`
	Error       string `json:"error,omitempty"`
}

// Tokens are the BOS and EOS tokens (the Hub's gguf.bos_token and eos_token).
type Tokens struct {
	BOS string `json:"bos,omitempty"`
	EOS string `json:"eos,omitempty"`
}

// GGUF is the Hub's `gguf` field in model info.
type GGUF struct {
	Total         int64  `json:"total"`
	Architecture  string `json:"architecture,omitempty"`
	ContextLength int64  `json:"context_length,omitempty"`
	ChatTemplate  string `json:"chat_template,omitempty"`
	BOSToken      string `json:"bos_token,omitempty"`
	EOSToken      string `json:"eos_token,omitempty"`
}

// DatasetSummary is the dataset part of the index (DATA-9).
type DatasetSummary struct {
	Configs      []string       `json:"configs,omitempty"`
	Splits       map[string]int `json:"splits,omitempty"`
	Rows         int64          `json:"rows,omitempty"`
	Modalities   []string       `json:"modalities,omitempty"`
	Formats      []string       `json:"formats,omitempty"`
	Tasks        []string       `json:"task_categories,omitempty"`
	Languages    []string       `json:"languages,omitempty"`
	SizeCategory string         `json:"size_category,omitempty"`
	PrettyName   string         `json:"pretty_name,omitempty"`
	TrainedBy    []string       `json:"models,omitempty"`
}

// Summary is the index of one Commit (the metadata.summary column).
type Summary struct {
	Formats       []string        `json:"formats"`
	Architecture  string          `json:"architecture,omitempty"`
	ModelType     string          `json:"model_type,omitempty"`
	Parameters    *Parameters     `json:"parameters,omitempty"`
	Quantisations []Quantisation  `json:"quantisations,omitempty"`
	ContextLength int64           `json:"context_length,omitempty"`
	ChatTemplate  string          `json:"chat_template,omitempty"`
	Tokens        *Tokens         `json:"tokens,omitempty"`
	CardData      map[string]any  `json:"card_data,omitempty"`
	Licence       Licence         `json:"licence"`
	PipelineTag   string          `json:"pipeline_tag,omitempty"`
	LibraryName   string          `json:"library_name,omitempty"`
	Tags          []string        `json:"tags,omitempty"`
	Size          Size            `json:"size"`
	Files         []FileInfo      `json:"files,omitempty"`
	GGUF          *GGUF           `json:"gguf,omitempty"`
	Config        json.RawMessage `json:"config,omitempty"`
	Dataset       *DatasetSummary `json:"dataset,omitempty"`
}

// QuantGroup is one entry in the GGUF quant picker (PULL-4): every file of a
// quantisation, including split files, plus vision projectors.
type QuantGroup struct {
	Name       string   `json:"name"`
	Files      []string `json:"files"`
	Projectors []string `json:"projectors,omitempty"`
	Size       int64    `json:"size"`
}

// Indexer builds and reads index results.
type Indexer interface {
	// Index extracts metadata from the files present at a Commit, stores it
	// keyed by (Repo, Commit, indexer version) and returns it. A malformed
	// file gives a per-file error, never a failed index.
	Index(ctx context.Context, repoUUID uuid.UUID, id model.RepoID, commit model.CommitSHA) (*Summary, error)
	// Get returns stored results, or ErrNotIndexed.
	Get(ctx context.Context, repoUUID uuid.UUID, commit model.CommitSHA) (*Summary, error)
	// FileHeader returns the safetensors or GGUF header of a file as JSON,
	// read with ranged reads, or ErrNoHeader.
	FileHeader(ctx context.Context, id model.RepoID, commit model.CommitSHA, path string) (json.RawMessage, error)
	// Quants groups a Commit's GGUF files by quantisation.
	Quants(ctx context.Context, id model.RepoID, commit model.CommitSHA) ([]QuantGroup, error)
}

// quantRe matches quantisation names in GGUF filenames, case-insensitively.
var quantRe = regexp.MustCompile(`(?i)(?:^|[-_.])((?:I?Q[1-8](?:_[0-9A-Z]+)*)|F16|F32|BF16|MXFP4|TQ[12]_0)(?:[-_.]|$)`)

var splitRe = regexp.MustCompile(`(?i)-\d{5}-of-\d{5}\.gguf$`)

// QuantName returns the quantisation named in a GGUF filename, in upper
// case, or "" if there is none. For example `Llama-3.2-3B-Instruct-Q4_K_M.gguf`
// gives `Q4_K_M`.
func QuantName(filename string) string {
	base := filename
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	base = splitRe.ReplaceAllString(base, ".gguf")
	base = strings.TrimSuffix(strings.TrimSuffix(base, ".gguf"), ".GGUF")
	var name string
	for _, m := range quantRe.FindAllStringSubmatch(base+".", -1) {
		name = m[1]
	}
	return strings.ToUpper(name)
}

// GroupQuants groups GGUF files by quantisation name, joining split files
// (`*-00001-of-00003.gguf`) into one group and attaching every
// `mmproj-*.gguf` projector to each group. sizes maps paths to sizes.
func GroupQuants(sizes map[string]int64) []QuantGroup {
	groups := map[string]*QuantGroup{}
	var projectors []string
	var projSize int64
	for p, size := range sizes {
		if !strings.HasSuffix(strings.ToLower(p), ".gguf") {
			continue
		}
		base := p[strings.LastIndex(p, "/")+1:]
		if strings.HasPrefix(strings.ToLower(base), "mmproj") {
			projectors = append(projectors, p)
			projSize += size
			continue
		}
		name := QuantName(p)
		if name == "" {
			name = strings.TrimSuffix(splitRe.ReplaceAllString(base, ".gguf"), ".gguf")
		}
		g, ok := groups[name]
		if !ok {
			g = &QuantGroup{Name: name}
			groups[name] = g
		}
		g.Files = append(g.Files, p)
		g.Size += size
	}
	sort.Strings(projectors)
	out := make([]QuantGroup, 0, len(groups))
	for _, g := range groups {
		sort.Strings(g.Files)
		if len(projectors) > 0 {
			g.Projectors = projectors
			g.Size += projSize
		}
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
