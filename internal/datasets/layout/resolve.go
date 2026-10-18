// Package layout resolves the Configs and Splits of a dataset Repo the way
// the `datasets` library does (DATA-2): from the card's `configs:` YAML
// when present, otherwise by matching file and directory names against the
// default Split patterns in datasets/data_files.py, and picks the packaged
// builder (csv, json, parquet and the rest) from the file extensions, as
// datasets/load.py does.
package layout

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/hangar-hub/hangar/internal/datasets"
	"github.com/hangar-hub/hangar/internal/datasets/card"
)

// module is a `datasets` packaged builder and its default options.
type module struct {
	name    string
	options map[string]any
}

// File is a file in a dataset Repo.
type File struct {
	Path string
	Size int64
	// Entries calls fn with the name of each entry of a zip archive, as
	// ZipEntries does. Resolve lists archives to find their builder, as
	// `datasets` does; it is nil for files that are not zip archives.
	Entries func(fn func(name string) error) error
}

// Errors.
var (
	// ErrEmptyDataset means no data files were found.
	ErrEmptyDataset = errors.New("the dataset has no data files")
	// ErrMixedFormats means the Splits have data files of different formats.
	ErrMixedFormats = errors.New("couldn't infer the same data file format for all splits")
	// ErrMissingFile means a data_files entry names a file that does not exist.
	ErrMissingFile = errors.New("a data file listed in the card does not exist")
	// ErrInvalidSplitName means a sharded file name gives a Split name
	// `datasets` refuses.
	ErrInvalidSplitName = errors.New("invalid split name")
	// ErrCardTooLarge means README.md or .huggingface.yaml is larger than
	// MaxCardBytes.
	ErrCardTooLarge = errors.New("the dataset card is too large")
	// ErrInvalidArchive means a zip archive among the data files cannot be
	// read.
	ErrInvalidArchive = errors.New("invalid zip archive")
)

// IsLayoutError reports whether err, from Resolve or ReadCard, is about
// the dataset's files or card, which only a new Commit changes, rather
// than a failure to read them.
func IsLayoutError(err error) bool {
	for _, e := range []error{ErrEmptyDataset, ErrMixedFormats, ErrMissingFile, ErrInvalidSplitName, ErrCardTooLarge,
		ErrInvalidArchive, ErrInvalidPattern, card.ErrInvalid} {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
}

// Split pattern constants from datasets/data_files.py.
const (
	splitPatternSharded = "data/{split}-[0-9][0-9][0-9][0-9][0-9]-of-[0-9][0-9][0-9][0-9][0-9]*.*"
	nonWordsChars       = "-._ 0-9"
)

var (
	splitKeywords = [][2]any{
		{"train", []string{"train", "training"}},
		{"validation", []string{"validation", "valid", "dev", "val"}},
		{"test", []string{"test", "testing", "eval", "evaluation"}},
	}
	keywordsInFilename = []string{"**/{keyword}[{sep}]*", "**/*[{sep}]{keyword}[{sep}]*"}
	keywordsInDirName  = []string{"**/{keyword}/**", "**/{keyword}[{sep}]*/**", "**/*[{sep}]{keyword}/**", "**/*[{sep}]{keyword}[{sep}]*/**"}
	defaultSplits      = []string{"train", "validation", "test"}
)

// patternSet is an ordered list of Splits and their patterns.
type patternSet []card.SplitPatterns

func keywordPatterns(bases []string) patternSet {
	var out patternSet
	for _, sk := range splitKeywords {
		split := sk[0].(string)
		var ps []string
		for _, kw := range sk[1].([]string) {
			for _, b := range bases {
				ps = append(ps, strings.NewReplacer("{keyword}", kw, "{sep}", nonWordsChars).Replace(b))
			}
		}
		out = append(out, card.SplitPatterns{Split: split, Patterns: ps})
	}
	return out
}

// allDefaultPatterns is ALL_DEFAULT_PATTERNS, tried in order.
var allDefaultPatterns = []patternSet{
	{{Split: "logs", Patterns: []string{"**/*.eval"}}},
	{{Split: "test", Patterns: []string{"**/task.toml", "**/instruction.md"}}},
	keywordPatterns(keywordsInDirName),
	keywordPatterns(keywordsInFilename),
	{{Split: "train", Patterns: []string{"**"}}},
}

// resolver matches patterns against a Repo's files.
type resolver struct {
	files []File
	// members lists the entries of the zip archives by path.
	members map[string]func(fn func(name string) error) error
}

func newResolver(files []File) *resolver {
	r := &resolver{files: files, members: map[string]func(fn func(name string) error) error{}}
	for _, f := range files {
		if f.Entries != nil {
			r.members[f.Path] = f.Entries
		}
	}
	return r
}

// resolve is datasets.data_files.resolve_pattern for files in a Repo.
// base is a directory relative to the Repo root, or "". allowed lists
// extensions to keep, or nil for every file.
func (r *resolver) resolve(pattern, base string, allowed []string) ([]string, error) {
	full := strings.TrimPrefix(pattern, "./")
	if strings.Contains(pattern, "://") || strings.HasPrefix(pattern, "/") {
		return nil, fmt.Errorf("%w: %q is not a path inside the Repo", ErrMissingFile, pattern)
	}
	if base != "" {
		full = base + "/" + full
	}
	full = cleanPattern(full)
	re, err := globRegexp(full)
	if err != nil {
		return nil, err
	}
	ignore := map[string]bool{}
	for _, f := range filesToIgnore {
		ignore[f] = true
	}
	delete(ignore, path.Base(full))
	var out []string
	for _, f := range r.files {
		if !re.MatchString(f.Path) || ignore[path.Base(f.Path)] {
			continue
		}
		if insideUnrequestedSpecialDir(f.Path, full) || unrequestedHidden(f.Path, full) {
			continue
		}
		if allowed != nil && !hasAllowedExtension(f.Path, allowed) {
			continue
		}
		out = append(out, f.Path)
	}
	sort.Strings(out)
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: unable to find %q", ErrMissingFile, full)
	}
	return out, nil
}

// cleanPattern removes "." and ".." components the way fsspec's path
// normalisation does for relative patterns.
func cleanPattern(p string) string {
	parts := strings.Split(p, "/")
	var out []string
	for _, s := range parts {
		switch s {
		case "", ".":
			continue
		case "..":
			if len(out) > 0 {
				out = out[:len(out)-1]
			}
			continue
		}
		out = append(out, s)
	}
	return strings.Join(out, "/")
}

func hasAllowedExtension(p string, allowed []string) bool {
	parts := strings.Split(path.Base(p), ".")
	for _, suffix := range parts[1:] {
		if slices.Contains(allowed, "."+suffix) {
			return true
		}
	}
	return false
}

func parentParts(p string) []string {
	dir := path.Dir(p)
	if dir == "." || dir == "" {
		return nil
	}
	return strings.Split(dir, "/")
}

// insideUnrequestedSpecialDir is _is_inside_unrequested_special_dir.
func insideUnrequestedSpecialDir(matched, pattern string) bool {
	count := func(parts []string) int {
		n := 0
		for _, p := range parts {
			if strings.HasPrefix(p, "__") {
				n++
			}
		}
		return n
	}
	return count(parentParts(matched)) != count(parentParts(pattern))
}

// unrequestedHidden is _is_unrequested_hidden_file_or_is_inside_unrequested_hidden_dir.
func unrequestedHidden(matched, pattern string) bool {
	count := func(p string) int {
		n := 0
		for _, part := range strings.Split(p, "/") {
			if strings.HasPrefix(part, ".") && strings.Trim(part, ".") != "" {
				n++
			}
		}
		return n
	}
	return count(matched) != count(pattern)
}

// splitNameFromSharded is string_to_dict(basename, basename(SPLIT_PATTERN_SHARDED)).
var shardedBasenameRe = regexp.MustCompile(`^(?P<_split>.+)-[0-9][0-9][0-9][0-9][0-9]-of-[0-9][0-9][0-9][0-9][0-9]*.*`)

// dataPatterns is datasets.data_files.get_data_patterns.
func (r *resolver) dataPatterns(base string) (patternSet, error) {
	shardedPattern := strings.ReplaceAll(splitPatternSharded, "{split}", "*")
	if files, err := r.resolve(shardedPattern, base, nil); err == nil && len(files) > 0 {
		seen := map[string]bool{}
		for _, f := range files {
			m := shardedBasenameRe.FindStringSubmatch(path.Base(f))
			if m == nil {
				continue
			}
			seen[m[1]] = true
		}
		var splits []string
		for _, s := range defaultSplits {
			if seen[s] {
				splits = append(splits, s)
			}
		}
		var others []string
		for s := range seen {
			if !slices.Contains(defaultSplits, s) {
				if !card.ValidSplitName(s) {
					return nil, fmt.Errorf("%w: split name should match %q but got %q", ErrInvalidSplitName, `^\w+(\.\w+)*$`, s)
				}
				others = append(others, s)
			}
		}
		sort.Strings(others)
		var out patternSet
		for _, s := range append(splits, others...) {
			out = append(out, card.SplitPatterns{Split: s, Patterns: []string{strings.ReplaceAll(splitPatternSharded, "{split}", s)}})
		}
		return out, nil
	}
	for _, set := range allDefaultPatterns {
		var out patternSet
		for _, sp := range set {
			for _, p := range sp.Patterns {
				if files, err := r.resolve(p, base, nil); err == nil && len(files) > 0 {
					out = append(out, sp)
					break
				}
			}
		}
		if len(out) > 0 {
			return out, nil
		}
	}
	return nil, ErrEmptyDataset
}

// resolveSplits is DataFilesDict.from_patterns: every pattern of a Split
// is resolved in order; a missing literal path is an error, a glob that
// matches nothing is skipped.
func (r *resolver) resolveSplits(ps patternSet, base string, allowed []string) ([]card.SplitPatterns, error) {
	out := make([]card.SplitPatterns, 0, len(ps))
	for _, sp := range ps {
		var files []string
		for _, p := range sp.Patterns {
			got, err := r.resolve(p, base, allowed)
			if err != nil {
				if hasMagic(p) && errors.Is(err, ErrMissingFile) {
					continue
				}
				return nil, err
			}
			files = append(files, got...)
		}
		out = append(out, card.SplitPatterns{Split: sp.Split, Patterns: files})
	}
	return out, nil
}

func lookupModule(ext string) (module, bool) {
	m, ok := extensionModules[ext]
	return m, ok
}

func isMetadataFile(p string) bool {
	base := path.Base(p)
	for _, names := range metadataFileNames {
		if slices.Contains(names, base) {
			return true
		}
	}
	return false
}

// inferModule is infer_module_for_data_files_list for one Split.
func (r *resolver) inferModule(files []string) (module, error) {
	type key struct {
		ext      string
		metadata bool
	}
	counts := map[key]int{}
	var order []key
	for _, f := range files {
		base := path.Base(f)
		parts := strings.Split(base, ".")
		for _, s := range parts[1:] {
			k := key{ext: "." + strings.ToLower(s), metadata: isMetadataFile(f)}
			if counts[k] == 0 {
				order = append(order, k)
			}
			counts[k]++
		}
	}
	prefs := []string{".parquet", ".lance", ".vortex", ".arrow", ".jsonl", ".json", ".csv"}
	sort.SliceStable(order, func(i, j int) bool {
		a, b := order[i], order[j]
		if a.metadata != b.metadata {
			return !a.metadata
		}
		if counts[a] != counts[b] {
			return counts[a] > counts[b]
		}
		for _, p := range prefs {
			if (a.ext == p) != (b.ext == p) {
				return a.ext == p
			}
		}
		return a.ext > b.ext
	})
	for _, k := range order {
		if m, ok := lookupModule(k.ext); ok {
			return m, nil
		}
		if k.ext == ".zip" {
			return r.inferArchives(files)
		}
	}
	return module{}, ErrEmptyDataset
}

func moduleExtensions(name string) []string {
	var out []string
	for ext, m := range extensionModules {
		if m.name == name {
			out = append(out, ext)
		}
	}
	out = append(out, ".zip")
	out = append(out, metadataExtensions[name]...)
	sort.Strings(out)
	return out
}

func allAllowedExtensions() []string {
	out := make([]string, 0, len(extensionModules)+4)
	for ext := range extensionModules {
		out = append(out, ext)
	}
	out = append(out, ".zip")
	for _, exts := range metadataExtensions {
		out = append(out, exts...)
	}
	sort.Strings(out)
	return out
}

// filterFiles is DataFilesList.filter(extensions, file_names).
func filterFiles(files []string, exts, names []string) []string {
	var pats []*regexp.Regexp
	if len(exts) > 0 {
		q := make([]string, len(exts))
		for i, e := range exts {
			q[i] = regexp.QuoteMeta(e)
		}
		pats = append(pats, regexp.MustCompile(`^.*(`+strings.Join(q, "|")+`)(\..+)?$`))
	}
	if len(names) > 0 {
		q := make([]string, len(names))
		for i, n := range names {
			q[i] = regexp.QuoteMeta(n)
		}
		pats = append(pats, regexp.MustCompile(`^.*[\/]?(`+strings.Join(q, "|")+`)$`))
	}
	if len(pats) == 0 {
		return files
	}
	var out []string
	for _, f := range files {
		for _, p := range pats {
			if p.MatchString(f) {
				out = append(out, f)
				break
			}
		}
	}
	return out
}

// Resolve resolves Configs and Splits from a Repo's files and its card.
// c may be nil when the Repo has no README.md.
func Resolve(files []File, c *card.Card) (*datasets.Resolved, error) {
	if c == nil {
		c = &card.Card{}
	}
	r := newResolver(files)
	allowed := allAllowedExtensions()

	// Find the builder from the first config's data_files, or from the
	// default patterns over the whole Repo, as HubDatasetModuleFactory does.
	var patterns patternSet
	var err error
	if len(c.Configs) > 0 && c.Configs[0].DataFiles != nil {
		patterns = c.Configs[0].DataFiles
	} else if patterns, err = r.dataPatterns(""); err != nil {
		return nil, err
	}
	splitFiles, err := r.resolveSplits(patterns, "", allowed)
	if err != nil {
		return nil, err
	}
	var mod module
	for i, sf := range splitFiles {
		m, err := r.inferModule(sf.Patterns)
		if errors.Is(err, ErrEmptyDataset) {
			return nil, fmt.Errorf("%w: no supported data files in split %q", ErrEmptyDataset, sf.Split)
		}
		if err != nil {
			return nil, err
		}
		if i == 0 {
			mod = m
		} else if m.name != mod.name || fmt.Sprint(m.options) != fmt.Sprint(mod.options) {
			return nil, fmt.Errorf("%w: %q and %q", ErrMixedFormats, mod.name, m.name)
		}
	}
	if mod.name == "" {
		return nil, ErrEmptyDataset
	}

	res := &datasets.Resolved{}
	if len(c.Configs) == 0 {
		cfg := datasets.Config{Name: datasets.DefaultConfig, Default: true, Builder: mod.name, DataFiles: map[string][]string{}, Files: map[string][]string{}}
		for _, sp := range patterns {
			cfg.DataFiles[sp.Split] = sp.Patterns
			cfg.SplitOrder = append(cfg.SplitOrder, sp.Split)
		}
		for _, sf := range splitFiles {
			cfg.Files[sf.Split] = filterFiles(sf.Patterns, moduleExtensions(mod.name), metadataFileNames[mod.name])
		}
		cfg.BuilderOptions = cloneOptions(mod.options)
		if in, ok := c.Info(cfg.Name); ok {
			cfg.Features = in.Features
		}
		res.Configs = []datasets.Config{cfg}
		return res, nil
	}

	def, err := c.DefaultConfig()
	if err != nil {
		return nil, err
	}
	res.FromCard = true
	for _, cs := range c.Configs {
		cfg := datasets.Config{Name: cs.Name, Default: cs.Name == def, Builder: mod.name, DataDir: cs.DataDir,
			DataFiles: map[string][]string{}, Files: map[string][]string{}}
		ps := patternSet(cs.DataFiles)
		if cs.DataFiles == nil {
			if ps, err = r.dataPatterns(cs.DataDir); err != nil {
				return nil, fmt.Errorf("config %q: %w", cs.Name, err)
			}
		}
		sf, err := r.resolveSplits(ps, cs.DataDir, allowed)
		if err != nil {
			return nil, fmt.Errorf("config %q: %w", cs.Name, err)
		}
		for i, sp := range ps {
			cfg.DataFiles[sp.Split] = sp.Patterns
			cfg.SplitOrder = append(cfg.SplitOrder, sp.Split)
			cfg.Files[sp.Split] = sf[i].Patterns
		}
		cfg.BuilderOptions = cloneOptions(mod.options)
		for k, v := range cs.Options {
			cfg.BuilderOptions[k] = v
		}
		cfg.Features = cs.Features
		if cfg.Features == nil {
			if in, ok := c.Info(cs.Name); ok {
				cfg.Features = in.Features
			}
		}
		res.Configs = append(res.Configs, cfg)
	}
	return res, nil
}

func cloneOptions(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
