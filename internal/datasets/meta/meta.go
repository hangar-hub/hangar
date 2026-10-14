// Package meta summarises a dataset for the index and the Arsenal facets
// (DATA-9, DATA-11): Configs, Splits, rows and bytes, features,
// modalities, formats, task categories, languages, the size category, the
// licence and the pretty name, from the card and the converted Configs.
package meta

import (
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/jamesagarside/hangar/internal/datasets/card"
	"github.com/jamesagarside/hangar/internal/datasets/features"
	"github.com/jamesagarside/hangar/internal/datasets/pipeline"
)

// SplitSummary is a Split's size.
type SplitSummary struct {
	Name         string `json:"name"`
	NumRows      int64  `json:"num_rows"`
	NumBytes     int64  `json:"num_bytes"`
	ParquetBytes int64  `json:"num_bytes_parquet"`
}

// ConfigSummary is a Config's Splits and features.
type ConfigSummary struct {
	Name     string            `json:"name"`
	Splits   []SplitSummary    `json:"splits"`
	Features features.Features `json:"features"`
	Format   string            `json:"format"`
}

// Summary is the dataset part of a Commit's index (DATA-9).
type Summary struct {
	Configs        []ConfigSummary `json:"configs"`
	NumRows        int64           `json:"num_rows"`
	NumBytes       int64           `json:"num_bytes"`
	Modalities     []string        `json:"modalities"`
	Formats        []string        `json:"formats"`
	TaskCategories []string        `json:"task_categories"`
	Languages      []string        `json:"languages"`
	SizeCategory   string          `json:"size_category"`
	Licences       []string        `json:"licences"`
	PrettyName     string          `json:"pretty_name,omitempty"`
}

// SizeCategory returns the Hub's size_categories bucket for a row count.
func SizeCategory(n int64) string {
	buckets := []struct {
		max  int64
		name string
	}{
		{1_000, "n<1K"}, {10_000, "1K<n<10K"}, {100_000, "10K<n<100K"}, {1_000_000, "100K<n<1M"}, {10_000_000, "1M<n<10M"},
		{100_000_000, "10M<n<100M"}, {1_000_000_000, "100M<n<1B"}, {10_000_000_000, "1B<n<10B"},
		{100_000_000_000, "10B<n<100B"}, {1_000_000_000_000, "100B<n<1T"},
	}
	for _, b := range buckets {
		if n < b.max {
			return b.name
		}
	}
	return "n>1T"
}

// Modalities is detect_features_modalities from the Hub's dataset-viewer.
func Modalities(fs features.Features) []string {
	set := map[string]bool{}
	for _, f := range fs {
		f.Feature.Contains(func(x *features.Feature) bool {
			switch {
			case x.Kind == features.KindAudio:
				set["audio"] = true
			case x.Kind == features.KindImage:
				set["image"] = true
			case x.Kind == features.KindVideo:
				set["video"] = true
			case x.Kind == features.KindValue && (x.Dtype == "string" || x.Dtype == "large_string"),
				x.Kind == features.KindTranslation, x.Kind == features.KindTranslationVariableLanguages:
				set["text"] = true
			case x.Kind == features.KindPdf:
				set["document"] = true
			}
			return false
		})
	}
	numeric := 0
	for _, f := range fs {
		if f.Feature.Kind == features.KindValue && (strings.Contains(f.Feature.Dtype, "int") || strings.Contains(f.Feature.Dtype, "float")) {
			numeric++
		}
	}
	if !set["audio"] && !set["image"] && numeric >= 2 {
		set["tabular"] = true
	}
	for _, f := range fs {
		if !strings.Contains(f.Name, "emb") && f.Feature.IsListKind() && f.Feature.Elem.Kind == features.KindValue && f.Feature.Elem.Dtype == "float32" {
			set["timeseries"] = true
		}
	}
	out := make([]string, 0, len(set))
	for m := range set {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

// Summarise builds a Summary from a card (which may be nil) and the
// converted Configs.
func Summarise(c *card.Card, infos []*pipeline.ConfigInfo) Summary {
	s := Summary{}
	mods := map[string]bool{}
	for _, info := range infos {
		cs := ConfigSummary{Name: info.Config, Features: info.Features, Format: info.Builder}
		for _, sp := range info.Splits {
			var pq int64
			for _, sh := range sp.Shards {
				pq += sh.Size
			}
			cs.Splits = append(cs.Splits, SplitSummary{Name: sp.Name, NumRows: sp.NumExamples, NumBytes: sp.NumBytes, ParquetBytes: pq})
			s.NumRows += sp.NumExamples
			s.NumBytes += sp.NumBytes
		}
		s.Configs = append(s.Configs, cs)
		if !slices.Contains(s.Formats, info.Builder) {
			s.Formats = append(s.Formats, info.Builder)
		}
		for _, m := range Modalities(info.Features) {
			mods[m] = true
		}
	}
	for m := range mods {
		s.Modalities = append(s.Modalities, m)
	}
	sort.Strings(s.Modalities)
	sort.Strings(s.Formats)
	s.SizeCategory = SizeCategory(s.NumRows)
	if c != nil {
		s.TaskCategories = c.TaskCategories
		s.Languages = c.Languages
		s.Licences = c.Licenses
		s.PrettyName = c.PrettyName
		if len(infos) == 0 && len(c.SizeCategories) > 0 {
			s.SizeCategory = c.SizeCategories[0]
		}
	}
	return s
}

// Keywords lists the Summary's facets as the Hub's Croissant keywords do.
func (s Summary) Keywords() []string {
	var out []string
	out = append(out, s.TaskCategories...)
	out = append(out, s.Languages...)
	if s.SizeCategory != "" {
		out = append(out, sizeLabel(s.SizeCategory))
	}
	out = append(out, s.Formats...)
	for _, m := range s.Modalities {
		out = append(out, strings.ToUpper(m[:1])+m[1:])
	}
	return out
}

// sizeLabel is the Hub's display form of a size category, such as "< 1K".
func sizeLabel(cat string) string {
	switch {
	case strings.HasPrefix(cat, "n<"):
		return "< " + strings.TrimPrefix(cat, "n<")
	case strings.HasPrefix(cat, "n>"):
		return "> " + strings.TrimPrefix(cat, "n>")
	}
	return strings.NewReplacer("<n<", " - ").Replace(cat)
}

var (
	mdLinkRe  = regexp.MustCompile(`!?\[([^\]]*)\]\([^)]*\)`)
	mdHTMLRe  = regexp.MustCompile(`<[^>]+>`)
	mdEmphRe  = regexp.MustCompile("[*_`]{1,3}")
	mdHeadRe  = regexp.MustCompile(`(?m)^\s{0,3}#{1,6}\s*`)
	mdFenceRe = regexp.MustCompile("(?s)```.*?```")
	blankRe   = regexp.MustCompile(`\n{3,}`)
)

// PlainText turns a card's Markdown into plain text for descriptions.
func PlainText(md string) string {
	s := mdFenceRe.ReplaceAllString(md, "")
	s = mdLinkRe.ReplaceAllString(s, "$1")
	s = mdHTMLRe.ReplaceAllString(s, "")
	s = mdHeadRe.ReplaceAllString(s, "")
	s = mdEmphRe.ReplaceAllString(s, "")
	s = blankRe.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}

// ModelCardDatasets returns the dataset Repo IDs a model card lists in its
// `datasets` metadata, for the index's dataset Dependencies and the
// "models trained on this dataset" links (DATA-9).
func ModelCardDatasets(c *card.Card) []string {
	if c == nil || c.Data == nil {
		return nil
	}
	var out []string
	switch v := c.Data.Get("datasets").(type) {
	case string:
		out = append(out, v)
	case []any:
		for _, x := range v {
			if s, ok := x.(string); ok && s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}
