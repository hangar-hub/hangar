// Package deps finds a Repo's Dependencies from its files
// (docs/spec/06-upstreams-and-pulls.md "Dependencies", PULL-11): code
// Dependencies from `auto_map`, adapter and card base models, card
// datasets, sentence-transformers modules and diffusers components. Store
// keeps them in the dependencies table and answers "needs" and "needed by".
package deps

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/jamesagarside/hangar/internal/model"
)

// Kind is the kind of a Dependency.
type Kind string

// Dependency kinds (the dependencies table's kind column).
const (
	KindCode          Kind = "code"
	KindBaseModel     Kind = "base_model"
	KindAdapterBase   Kind = "adapter_base"
	KindCardBaseModel Kind = "card_base_model"
	KindDataset       Kind = "dataset"
)

// Dependency is a link from a Repo at a Commit to another Repo.
type Dependency struct {
	Target model.RepoID `json:"target"`
	// Revision is the Revision the source names, or "" for the default branch.
	Revision string `json:"revision,omitempty"`
	Kind     Kind   `json:"kind"`
	// Required is true for Dependencies the Repo cannot load without.
	Required bool `json:"required"`
	// SourcePath and SourceKey say where the Dependency was found, such as
	// config.json and auto_map.AutoModel.
	SourcePath string `json:"source_path"`
	SourceKey  string `json:"source_key"`
}

// Key identifies a Dependency for deduplication.
func (d Dependency) Key() string {
	return string(d.Kind) + "|" + string(d.Target.Type) + "|" + strings.ToLower(d.Target.String()) + "|" + d.Revision
}

// MaxFileSize is the largest file the parser reads; Dependency sources
// are small JSON and YAML files.
const MaxFileSize = 4 << 20

// Reader reads a file of the Repo at the Commit being parsed. It returns
// an error wrapping fs.ErrNotExist for a missing file.
type Reader func(ctx context.Context, path string) ([]byte, error)

// autoMapFiles hold `auto_map` objects.
var autoMapFiles = []string{"config.json", "tokenizer_config.json", "preprocessor_config.json", "processor_config.json", "generation_config.json"}

// SourceFiles are the files Parse reads, for callers that fetch them
// ahead of time (Discover reads them from the Upstream).
func SourceFiles() []string {
	return append(append([]string{}, autoMapFiles...), "adapter_config.json", "README.md", "modules.json", "model_index.json")
}

// Parse returns the Dependencies of the Repo self at one Commit. files
// lists every path in the Commit (to tell module folders from Repo IDs);
// read returns file contents. A malformed file is skipped, never fatal:
// the Repo may still be usable, and the index reports parse errors.
func Parse(ctx context.Context, self model.RepoID, files []string, read Reader) ([]Dependency, error) {
	present := map[string]bool{}
	dirs := map[string]bool{}
	for _, f := range files {
		present[f] = true
		for d := path.Dir(f); d != "." && d != "/" && !dirs[d]; d = path.Dir(d) {
			dirs[d] = true
		}
	}
	var out []Dependency
	seen := map[string]bool{}
	add := func(d Dependency) {
		if d.Target.Equal(self) && d.Target.Type == self.Type {
			return
		}
		if seen[d.Key()] {
			return
		}
		seen[d.Key()] = true
		out = append(out, d)
	}
	load := func(p string) ([]byte, bool, error) {
		if !present[p] {
			return nil, false, nil
		}
		b, err := read(ctx, p)
		if errors.Is(err, fs.ErrNotExist) {
			return nil, false, nil
		}
		if err != nil {
			return nil, false, fmt.Errorf("read %s: %w", p, err)
		}
		if len(b) > MaxFileSize {
			return nil, false, nil
		}
		return b, true, nil
	}

	for _, p := range autoMapFiles {
		b, ok, err := load(p)
		if err != nil {
			return nil, err
		}
		if ok {
			for _, d := range parseAutoMap(b, p) {
				add(d)
			}
		}
	}
	if b, ok, err := load("adapter_config.json"); err != nil {
		return nil, err
	} else if ok {
		var a struct {
			Base string `json:"base_model_name_or_path"`
		}
		if json.Unmarshal(b, &a) == nil {
			if id, rev, ok := repoRef(a.Base, model.RepoTypeModel); ok {
				add(Dependency{Target: id, Revision: rev, Kind: KindAdapterBase, SourcePath: "adapter_config.json", SourceKey: "base_model_name_or_path"})
			}
		}
	}
	if b, ok, err := load("README.md"); err != nil {
		return nil, err
	} else if ok {
		for _, d := range parseCard(b) {
			add(d)
		}
	}
	if b, ok, err := load("modules.json"); err != nil {
		return nil, err
	} else if ok {
		var mods []struct {
			Path string `json:"path"`
		}
		if json.Unmarshal(b, &mods) == nil {
			for i, m := range mods {
				p := strings.Trim(m.Path, "/")
				if p == "" || dirs[p] || present[p] {
					continue
				}
				if id, rev, ok := repoRef(p, model.RepoTypeModel); ok {
					add(Dependency{Target: id, Revision: rev, Kind: KindCode, Required: true, SourcePath: "modules.json", SourceKey: fmt.Sprintf("[%d].path", i)})
				}
			}
		}
	}
	if b, ok, err := load("model_index.json"); err != nil {
		return nil, err
	} else if ok {
		var idx map[string]json.RawMessage
		if json.Unmarshal(b, &idx) == nil {
			keys := make([]string, 0, len(idx))
			for k := range idx {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				var pair []any
				if json.Unmarshal(idx[k], &pair) != nil || len(pair) != 2 {
					continue
				}
				lib, ok := pair[0].(string)
				if !ok || !strings.Contains(lib, "/") || dirs[lib] {
					continue
				}
				if id, rev, ok := repoRef(lib, model.RepoTypeModel); ok {
					add(Dependency{Target: id, Revision: rev, Kind: KindCode, Required: true, SourcePath: "model_index.json", SourceKey: k})
				}
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].SourcePath != out[j].SourcePath {
			return out[i].SourcePath < out[j].SourcePath
		}
		return out[i].SourceKey < out[j].SourceKey
	})
	return out, nil
}

// parseAutoMap reads `auto_map` values of the form
// `<repo>--<module>.<Class>` or `<repo>@<revision>--<module>.<Class>`,
// including list values. A value with no `--` is in the same Repo.
func parseAutoMap(b []byte, source string) []Dependency {
	var doc struct {
		AutoMap map[string]json.RawMessage `json:"auto_map"`
	}
	if json.Unmarshal(b, &doc) != nil {
		return nil
	}
	keys := make([]string, 0, len(doc.AutoMap))
	for k := range doc.AutoMap {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []Dependency
	for _, k := range keys {
		var values []string
		var one string
		if json.Unmarshal(doc.AutoMap[k], &one) == nil {
			values = []string{one}
		} else {
			var list []*string
			if json.Unmarshal(doc.AutoMap[k], &list) != nil {
				continue
			}
			for _, v := range list {
				if v != nil {
					values = append(values, *v)
				}
			}
		}
		for _, v := range values {
			ref, _, ok := strings.Cut(v, "--")
			if !ok {
				continue
			}
			if id, rev, ok := repoRef(ref, model.RepoTypeModel); ok {
				out = append(out, Dependency{Target: id, Revision: rev, Kind: KindCode, Required: true, SourcePath: source, SourceKey: "auto_map." + k})
			}
		}
	}
	return out
}

// repoRef parses `ns/name` or `ns/name@revision`. Local paths and single
// names are not Repo IDs.
func repoRef(s string, t model.RepoType) (model.RepoID, string, bool) {
	s = strings.TrimSpace(s)
	if s == "" || strings.HasPrefix(s, "/") || strings.HasPrefix(s, ".") || strings.HasPrefix(s, "~") || strings.Contains(s, "\\") {
		return model.RepoID{}, "", false
	}
	ref, rev, _ := strings.Cut(s, "@")
	if strings.Count(ref, "/") != 1 {
		return model.RepoID{}, "", false
	}
	id, err := model.ParseRepoID(t, ref)
	if err != nil {
		return model.RepoID{}, "", false
	}
	if rev != "" && model.ValidateRefName("refs/heads/"+rev) != nil && !model.IsFullSHA(rev) {
		return model.RepoID{}, "", false
	}
	return id, rev, true
}

// FrontMatter returns the YAML front matter of a Model card, or nil.
func FrontMatter(card []byte) map[string]any {
	card = bytes.TrimPrefix(card, []byte("\ufeff"))
	text := strings.ReplaceAll(string(card), "\r\n", "\n")
	if !strings.HasPrefix(text, "---\n") {
		return nil
	}
	rest := text[4:]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return nil
	}
	var m map[string]any
	if yaml.Unmarshal([]byte(rest[:end]), &m) != nil {
		return nil
	}
	return m
}

// parseCard reads `base_model` (string or list) and `datasets` from the
// Model card's front matter.
func parseCard(b []byte) []Dependency {
	fm := FrontMatter(b)
	if fm == nil {
		return nil
	}
	strs := func(v any) []string {
		switch x := v.(type) {
		case string:
			return []string{x}
		case []any:
			var out []string
			for _, e := range x {
				if s, ok := e.(string); ok {
					out = append(out, s)
				}
			}
			return out
		}
		return nil
	}
	var out []Dependency
	for _, s := range strs(fm["base_model"]) {
		if id, rev, ok := repoRef(s, model.RepoTypeModel); ok {
			out = append(out, Dependency{Target: id, Revision: rev, Kind: KindCardBaseModel, SourcePath: "README.md", SourceKey: "base_model"})
		}
	}
	for _, s := range strs(fm["datasets"]) {
		if id, rev, ok := repoRef(s, model.RepoTypeDataset); ok {
			out = append(out, Dependency{Target: id, Revision: rev, Kind: KindDataset, SourcePath: "README.md", SourceKey: "datasets"})
		}
	}
	return out
}
