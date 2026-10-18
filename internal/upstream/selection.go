package upstream

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/hangar-hub/hangar/internal/datasets"
	"github.com/hangar-hub/hangar/internal/index"
	"github.com/hangar-hub/hangar/internal/model"
)

// Selection picks the files of a Pull (PULL-2, PULL-4, DATA-8). With no
// paths, globs, quants, Configs or Splits it means every file; Exclude
// always applies. None selects no files, for history-only Pulls such as
// Pull-through. Quants, Configs and Splits are stored by name, so a Mirror
// that tracks its Upstream follows renamed files of the same quant or Split.
type Selection struct {
	Paths   []string `json:"paths,omitempty"`
	Include []string `json:"include,omitempty"`
	Exclude []string `json:"exclude,omitempty"`
	// Quants are GGUF quantisation names such as Q4_K_M (PULL-4). Vision
	// projectors (mmproj-*.gguf) come with any quant.
	Quants []string `json:"quants,omitempty"`
	// Configs and Splits select dataset files by Config and Split name.
	Configs []string `json:"configs,omitempty"`
	Splits  []string `json:"splits,omitempty"`
	// ParquetOnly pulls the Parquet export of the selected Configs and
	// Splits from refs/convert/parquet instead of the source files.
	ParquetOnly bool `json:"parquet_only,omitempty"`
	// None selects no files: the Pull fetches history only.
	None bool `json:"none,omitempty"`
}

// ErrInvalidSelection wraps selection validation errors.
var ErrInvalidSelection = errors.New("invalid selection")

// All reports whether the Selection takes every file (before Exclude).
func (s Selection) All() bool {
	return !s.None && len(s.Paths) == 0 && len(s.Include) == 0 && len(s.Quants) == 0 && !s.dataset()
}

func (s Selection) dataset() bool {
	return len(s.Configs) > 0 || len(s.Splits) > 0 || s.ParquetOnly
}

// Normalise trims, deduplicates and sorts every list, upper-cases quant
// names and validates paths and globs.
func (s Selection) Normalise() (Selection, error) {
	clean := func(in []string, upper bool) []string {
		var out []string
		for _, v := range in {
			v = strings.TrimSpace(v)
			if upper {
				v = strings.ToUpper(v)
			}
			if v != "" && !slices.Contains(out, v) {
				out = append(out, v)
			}
		}
		sort.Strings(out)
		return out
	}
	out := Selection{
		Paths: clean(s.Paths, false), Include: clean(s.Include, false), Exclude: clean(s.Exclude, false),
		Quants: clean(s.Quants, true), Configs: clean(s.Configs, false), Splits: clean(s.Splits, false),
		ParquetOnly: s.ParquetOnly, None: s.None,
	}
	for _, p := range out.Paths {
		if c, err := model.CleanRepoPath(p); err != nil || c == "" || c != p {
			return Selection{}, fmt.Errorf("%w: path %q is not a clean path inside the Repo", ErrInvalidSelection, p)
		}
	}
	for _, g := range append(slices.Clone(out.Include), out.Exclude...) {
		if _, err := globRegexp(g); err != nil {
			return Selection{}, fmt.Errorf("%w: %w", ErrInvalidSelection, err)
		}
	}
	if out.None && (len(out.Paths) > 0 || len(out.Include) > 0 || len(out.Quants) > 0 || out.dataset()) {
		return Selection{}, fmt.Errorf("%w: a history-only selection cannot also name files", ErrInvalidSelection)
	}
	return out, nil
}

// Union returns a Selection with every file either selection takes. A
// Mirror keeps the union of every Pull into it, so tracking pulls new
// versions of everything it holds.
func (s Selection) Union(o Selection) Selection {
	switch {
	case s.None:
		return o
	case o.None:
		return s
	case s.All() || o.All():
		// Keep only the excludes both sides share.
		var ex []string
		for _, e := range s.Exclude {
			if s.All() && o.All() && slices.Contains(o.Exclude, e) {
				ex = append(ex, e)
			}
		}
		return Selection{Exclude: ex}
	}
	merge := func(a, b []string) []string {
		out := slices.Clone(a)
		for _, v := range b {
			if !slices.Contains(out, v) {
				out = append(out, v)
			}
		}
		sort.Strings(out)
		return out
	}
	var ex []string
	for _, e := range s.Exclude {
		if slices.Contains(o.Exclude, e) {
			ex = append(ex, e)
		}
	}
	return Selection{
		Paths: merge(s.Paths, o.Paths), Include: merge(s.Include, o.Include), Exclude: ex,
		Quants: merge(s.Quants, o.Quants), Configs: merge(s.Configs, o.Configs), Splits: merge(s.Splits, o.Splits),
		ParquetOnly: s.ParquetOnly && o.ParquetOnly,
	}
}

// Matcher decides which files of a Commit a Selection takes.
type Matcher struct {
	sel      Selection
	include  []*regexp.Regexp
	exclude  []*regexp.Regexp
	paths    map[string]bool
	quant    map[string]bool
	datasets []*regexp.Regexp
}

// NewMatcher prepares a Selection for the files of one Commit. files maps
// every file path to its size; GGUF quant groups are computed from it.
// datasetPatterns are the file patterns a dataset Selection resolved to
// (see datasets.Layout.Files); they are ignored for other Selections.
func NewMatcher(s Selection, files map[string]int64, datasetPatterns []string) (*Matcher, error) {
	m := &Matcher{sel: s, paths: map[string]bool{}, quant: map[string]bool{}}
	compile := func(globs []string) ([]*regexp.Regexp, error) {
		out := make([]*regexp.Regexp, 0, len(globs))
		for _, g := range globs {
			re, err := globRegexp(g)
			if err != nil {
				return nil, err
			}
			out = append(out, re)
		}
		return out, nil
	}
	var err error
	if m.include, err = compile(s.Include); err != nil {
		return nil, err
	}
	if m.exclude, err = compile(s.Exclude); err != nil {
		return nil, err
	}
	if m.datasets, err = compile(datasetPatterns); err != nil {
		return nil, err
	}
	for _, p := range s.Paths {
		m.paths[p] = true
	}
	if len(s.Quants) > 0 {
		for _, g := range index.GroupQuants(files) {
			if !slices.Contains(s.Quants, strings.ToUpper(g.Name)) {
				continue
			}
			for _, f := range g.Files {
				m.quant[f] = true
			}
			for _, f := range g.Projectors {
				m.quant[f] = true
			}
		}
	}
	return m, nil
}

// Match reports whether the Selection takes path.
func (m *Matcher) Match(path string) bool {
	if m.sel.None {
		return false
	}
	for _, re := range m.exclude {
		if re.MatchString(path) {
			return false
		}
	}
	if m.sel.All() {
		return true
	}
	if m.paths[path] || m.quant[path] {
		return true
	}
	for _, re := range m.include {
		if re.MatchString(path) {
			return true
		}
	}
	for _, re := range m.datasets {
		if re.MatchString(path) {
			return true
		}
	}
	return false
}

// DatasetPatterns resolves a dataset Selection to file patterns with
// layout. It returns nil for Selections without Configs, Splits or
// ParquetOnly.
func DatasetPatterns(ctx context.Context, layout datasets.Layout, id model.RepoID, commit model.CommitSHA, s Selection) ([]string, error) {
	if !s.dataset() {
		return nil, nil
	}
	if id.Type != model.RepoTypeDataset {
		return nil, fmt.Errorf("%w: Configs, Splits and Parquet-only selections apply to datasets only", ErrInvalidSelection)
	}
	resolved, err := layout.Resolve(ctx, id, commit)
	if err != nil {
		return nil, fmt.Errorf("resolve the dataset's Configs and Splits: %w", err)
	}
	for _, c := range s.Configs {
		if _, ok := resolved.Config(c); !ok {
			return nil, fmt.Errorf("%w: the dataset has no Config %q", ErrInvalidSelection, c)
		}
	}
	pats, err := layout.Files(ctx, resolved, datasets.Selection{Configs: s.Configs, Splits: s.Splits, ParquetOnly: s.ParquetOnly})
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidSelection, err)
	}
	return pats, nil
}

var globCache sync.Map // string -> *regexp.Regexp

// globRegexp turns an fnmatch-style pattern, as huggingface_hub's
// allow_patterns uses it, into a regular expression: `*` matches any run of
// characters including `/`, `?` one character, and `[...]` a class. A
// pattern ending in `/` matches everything under that folder.
func globRegexp(glob string) (*regexp.Regexp, error) {
	if re, ok := globCache.Load(glob); ok {
		return re.(*regexp.Regexp), nil
	}
	if glob == "" || len(glob) > 1024 {
		return nil, fmt.Errorf("glob %q must be 1 to 1024 characters", glob)
	}
	g := glob
	if strings.HasSuffix(g, "/") {
		g += "*"
	}
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(g); i++ {
		c := g[i]
		switch c {
		case '*':
			b.WriteString(".*")
			for i+1 < len(g) && g[i+1] == '*' {
				i++
			}
		case '?':
			b.WriteString(".")
		case '[':
			j := i + 1
			if j < len(g) && (g[j] == '!' || g[j] == '^') {
				j++
			}
			if j < len(g) && g[j] == ']' {
				j++
			}
			for j < len(g) && g[j] != ']' {
				j++
			}
			if j >= len(g) {
				b.WriteString(`\[`)
				continue
			}
			class := g[i+1 : j]
			if strings.HasPrefix(class, "!") {
				class = "^" + class[1:]
			}
			b.WriteString("[" + strings.ReplaceAll(class, `\`, `\\`) + "]")
			i = j
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	if err != nil {
		return nil, fmt.Errorf("glob %q is not valid: %w", glob, err)
	}
	globCache.Store(glob, re)
	return re, nil
}

// MatchGlob reports whether name matches an fnmatch-style glob, as used by
// pull.pull_through.allow.
func MatchGlob(glob, name string) bool {
	re, err := globRegexp(glob)
	return err == nil && re.MatchString(name)
}
