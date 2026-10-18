// Package card reads the dataset card metadata that the Dataset Viewer,
// the Parquet export and Croissant need: the YAML front matter of a
// dataset Repo's README.md (and the optional .huggingface.yaml), with its
// `configs`, `dataset_info` and descriptive fields, as `datasets` and
// huggingface_hub read them.
package card

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/hangar-hub/hangar/internal/datasets/features"
)

// ErrInvalid means the card metadata is not valid.
var ErrInvalid = errors.New("invalid dataset card metadata")

// frontMatterRe is huggingface_hub's REGEX_YAML_BLOCK (repocard.py).
var frontMatterRe = regexp.MustCompile(`^(\s*---[\r\n]+)([\S\s]*?)([\r\n]+---(\r\n|\n|$))`)

// FrontMatter returns the YAML block at the top of a card and the rest of
// the text. ok is false when the card has no front matter.
func FrontMatter(readme string) (yamlText, body string, ok bool) {
	readme = strings.TrimPrefix(readme, "\ufeff")
	m := frontMatterRe.FindStringSubmatchIndex(readme)
	if m == nil {
		return "", readme, false
	}
	return readme[m[4]:m[5]], readme[m[1]:], true
}

// SplitPatterns is a Split and its data file patterns.
type SplitPatterns struct {
	Split    string
	Patterns []string
}

// ConfigSpec is one entry of the card's `configs` list.
type ConfigSpec struct {
	Name    string
	Default bool
	// DataFiles is nil when the entry has no data_files.
	DataFiles []SplitPatterns
	DataDir   string
	// Options are the other keys, such as a CSV `sep`, as JSON values.
	Options map[string]any
	// Features is set when the entry declares features.
	Features features.Features
}

// SplitInfo is a Split in `dataset_info`.
type SplitInfo struct {
	Name        string `json:"name"`
	NumBytes    int64  `json:"num_bytes"`
	NumExamples int64  `json:"num_examples"`
}

// Info is one `dataset_info` entry.
type Info struct {
	ConfigName   string
	Features     features.Features
	Splits       []SplitInfo
	DownloadSize int64
	DatasetSize  int64
}

// Card is the parsed metadata of a dataset card.
type Card struct {
	// Data is the whole front matter as an ordered value.
	Data *Map
	// Body is the Markdown after the front matter.
	Body           string
	Configs        []ConfigSpec
	Infos          []Info
	PrettyName     string
	Licenses       []string
	TaskCategories []string
	TaskIDs        []string
	Languages      []string
	SizeCategories []string
	Tags           []string
	Modalities     []string
	Citation       string
	Homepage       string
}

// Parse parses a README.md and, optionally, a .huggingface.yaml whose
// top-level keys override the card's, as `datasets` does.
func Parse(readme, standaloneYAML []byte) (*Card, error) {
	front, body, _ := FrontMatter(string(readme))
	c := &Card{Body: body, Data: &Map{}}
	if strings.TrimSpace(front) != "" {
		m, err := decodeMap([]byte(front))
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
		}
		c.Data = m
	}
	if len(standaloneYAML) > 0 {
		m, err := decodeMap(standaloneYAML)
		if err != nil {
			return nil, fmt.Errorf("%w: .huggingface.yaml: %w", ErrInvalid, err)
		}
		for _, kv := range m.Items {
			c.Data.Set(kv.Key, kv.Value)
		}
	}
	if err := c.fill(); err != nil {
		return nil, err
	}
	return c, nil
}

func decodeMap(text []byte) (*Map, error) {
	var n yaml.Node
	if err := yaml.Unmarshal(text, &n); err != nil {
		return nil, err
	}
	if n.Kind == 0 {
		return &Map{}, nil
	}
	v, err := fromNode(&n, 0)
	if err != nil {
		return nil, err
	}
	m, ok := v.(*Map)
	if !ok {
		if v == nil {
			return &Map{}, nil
		}
		return nil, errors.New("the metadata must be a YAML mapping")
	}
	return m, nil
}

func (c *Card) fill() error {
	c.PrettyName = asString(c.Data.Get("pretty_name"))
	c.Licenses = stringList(c.Data.Get("license"))
	c.TaskCategories = stringList(c.Data.Get("task_categories"))
	c.TaskIDs = stringList(c.Data.Get("task_ids"))
	c.Languages = stringList(c.Data.Get("language"))
	c.SizeCategories = stringList(c.Data.Get("size_categories"))
	c.Tags = stringList(c.Data.Get("tags"))
	c.Modalities = stringList(c.Data.Get("modalities"))
	c.Citation = asString(c.Data.Get("citation"))
	c.Homepage = asString(c.Data.Get("homepage"))
	if v := c.Data.Get("configs"); v != nil {
		list, ok := v.([]any)
		if !ok {
			return fmt.Errorf("%w: configs must be a list, got %s", ErrInvalid, describe(v))
		}
		seen := map[string]bool{}
		for i, item := range list {
			m, ok := item.(*Map)
			if !ok {
				return fmt.Errorf("%w: configs[%d] must be a mapping", ErrInvalid, i)
			}
			cs, err := parseConfig(m)
			if err != nil {
				return fmt.Errorf("%w: configs[%d]: %w", ErrInvalid, i, err)
			}
			if seen[cs.Name] {
				return fmt.Errorf("%w: config %q is declared twice", ErrInvalid, cs.Name)
			}
			seen[cs.Name] = true
			c.Configs = append(c.Configs, cs)
		}
		if _, err := c.DefaultConfig(); err != nil {
			return err
		}
	}
	if v := c.Data.Get("dataset_info"); v != nil {
		var items []any
		switch x := v.(type) {
		case []any:
			items = x
		case *Map:
			items = []any{x}
		default:
			return fmt.Errorf("%w: dataset_info must be a mapping or a list", ErrInvalid)
		}
		for i, item := range items {
			m, ok := item.(*Map)
			if !ok {
				return fmt.Errorf("%w: dataset_info[%d] must be a mapping", ErrInvalid, i)
			}
			info, err := parseInfo(m)
			if err != nil {
				return fmt.Errorf("%w: dataset_info[%d]: %w", ErrInvalid, i, err)
			}
			c.Infos = append(c.Infos, info)
		}
	}
	return nil
}

// splitNameRe is datasets.naming._split_re.
var splitNameRe = regexp.MustCompile(`^\w+(\.\w+)*$`)

// ValidSplitName reports whether a Split name is allowed.
func ValidSplitName(s string) bool { return splitNameRe.MatchString(s) }

func parseConfig(m *Map) (ConfigSpec, error) {
	cs := ConfigSpec{Options: map[string]any{}}
	name, ok := m.Get("config_name").(string)
	if !ok || name == "" {
		return cs, errors.New("each config needs a config_name")
	}
	cs.Name = name
	for _, kv := range m.Items {
		switch kv.Key {
		case "config_name":
		case "default":
			b, ok := kv.Value.(bool)
			if !ok {
				return cs, errors.New("default must be true or false")
			}
			cs.Default = b
		case "data_dir":
			s, ok := kv.Value.(string)
			if !ok {
				return cs, errors.New("data_dir must be a string")
			}
			cs.DataDir = strings.Trim(s, "/")
		case "data_files":
			df, err := SanitizePatterns(kv.Value)
			if err != nil {
				return cs, err
			}
			cs.DataFiles = df
		case "features":
			fs, err := FeaturesFromYAML(kv.Value)
			if err != nil {
				return cs, fmt.Errorf("features: %w", err)
			}
			cs.Features = fs
		default:
			cs.Options[kv.Key] = ToJSONValue(kv.Value)
		}
	}
	return cs, nil
}

// SanitizePatterns turns a data_files value into Splits and patterns, as
// datasets.data_files.sanitize_patterns does: a string or a list of
// strings is the train Split, a list of {split, path} mappings names
// Splits, and a mapping maps Split names to patterns.
func SanitizePatterns(v any) ([]SplitPatterns, error) {
	strs := func(x any) ([]string, error) {
		switch y := x.(type) {
		case string:
			return []string{y}, nil
		case []any:
			out := make([]string, 0, len(y))
			for _, e := range y {
				s, ok := e.(string)
				if !ok {
					return nil, errors.New("data_files paths must be strings")
				}
				out = append(out, s)
			}
			return out, nil
		}
		return nil, errors.New("data_files paths must be a string or a list of strings")
	}
	switch x := v.(type) {
	case string:
		return []SplitPatterns{{Split: "train", Patterns: []string{x}}}, nil
	case *Map:
		var out []SplitPatterns
		for _, kv := range x.Items {
			p, err := strs(kv.Value)
			if err != nil {
				return nil, err
			}
			out = append(out, SplitPatterns{Split: kv.Key, Patterns: p})
		}
		return out, nil
	case []any:
		anyMap := false
		for _, e := range x {
			if _, ok := e.(*Map); ok {
				anyMap = true
			}
		}
		if !anyMap {
			p, err := strs(x)
			if err != nil {
				return nil, err
			}
			return []SplitPatterns{{Split: "train", Patterns: p}}, nil
		}
		var out []SplitPatterns
		seen := map[string]bool{}
		for _, e := range x {
			m, ok := e.(*Map)
			if !ok || len(m.Items) != 2 || m.Get("split") == nil || m.Get("path") == nil {
				return nil, errors.New("each data_files entry must have exactly a split and a path")
			}
			split, ok := m.Get("split").(string)
			if !ok || !ValidSplitName(split) {
				return nil, fmt.Errorf("invalid split name %v: use letters, digits and underscores, with dots between words", m.Get("split"))
			}
			if seen[split] {
				return nil, fmt.Errorf("split %q appears twice in data_files", split)
			}
			seen[split] = true
			p, err := strs(m.Get("path"))
			if err != nil {
				return nil, err
			}
			out = append(out, SplitPatterns{Split: split, Patterns: p})
		}
		return out, nil
	}
	return nil, fmt.Errorf("data_files must be a string, a list or a mapping, got %s", describe(v))
}

// DefaultConfig returns the name of the default config, as
// MetadataConfigs.get_default_config_name does: the only config, the one
// named "default", or the one with `default: true`.
func (c *Card) DefaultConfig() (string, error) {
	def := ""
	for _, cs := range c.Configs {
		if len(c.Configs) == 1 || cs.Name == "default" || cs.Default {
			if def != "" {
				return "", fmt.Errorf("%w: several default configs: %q and %q", ErrInvalid, def, cs.Name)
			}
			def = cs.Name
		}
	}
	return def, nil
}

// Info returns the dataset_info entry of a config.
func (c *Card) Info(config string) (Info, bool) {
	for _, in := range c.Infos {
		name := in.ConfigName
		if name == "" {
			name = "default"
		}
		if name == config {
			return in, true
		}
	}
	return Info{}, false
}

func parseInfo(m *Map) (Info, error) {
	var in Info
	in.ConfigName = asString(m.Get("config_name"))
	if v := m.Get("features"); v != nil {
		fs, err := FeaturesFromYAML(v)
		if err != nil {
			return in, fmt.Errorf("features: %w", err)
		}
		in.Features = fs
	}
	in.DownloadSize = asInt(m.Get("download_size"))
	in.DatasetSize = asInt(m.Get("dataset_size"))
	if splits, ok := m.Get("splits").([]any); ok {
		for _, s := range splits {
			sm, ok := s.(*Map)
			if !ok {
				continue
			}
			in.Splits = append(in.Splits, SplitInfo{Name: asString(sm.Get("name")), NumBytes: asInt(sm.Get("num_bytes")), NumExamples: asInt(sm.Get("num_examples"))})
		}
	}
	return in, nil
}

// FeaturesFromYAML converts the YAML list form of features (`- name: x`
// with `dtype`, `class_label`, `list`, `struct` and the rest) to Features,
// as Features._from_yaml_list does.
func FeaturesFromYAML(v any) (features.Features, error) {
	if m, ok := v.(*Map); ok {
		// Some cards use the JSON dict form directly.
		b, err := json.Marshal(ToJSONValue(m))
		if err != nil {
			return nil, err
		}
		return features.ParseFeatures(b)
	}
	list, ok := v.([]any)
	if !ok {
		return nil, errors.New("features must be a list")
	}
	obj, err := fromYAMLInner(list, 0)
	if err != nil {
		return nil, err
	}
	b, err := json.Marshal(obj)
	if err != nil {
		return nil, err
	}
	return features.ParseFeatures(b)
}

func unsimplify(m *Map) (*Map, error) {
	m = m.Clone()
	for _, lt := range []string{"large_list", "list", "sequence"} {
		if s, ok := m.Get(lt).(string); ok {
			m.Set(lt, &Map{Items: []KV{{Key: "dtype", Value: s}}})
		}
	}
	if cl, ok := m.Get("class_label").(*Map); ok {
		if names, ok := cl.Get("names").(*Map); ok {
			ids := make([]int, 0, len(names.Items))
			byID := map[int]any{}
			for _, kv := range names.Items {
				id, err := strconv.Atoi(kv.Key)
				if err != nil {
					return nil, fmt.Errorf("class label id %q is not an integer", kv.Key)
				}
				ids = append(ids, id)
				byID[id] = kv.Value
			}
			list := make([]any, len(ids))
			for i := range ids {
				v, ok := byID[i]
				if !ok {
					return nil, fmt.Errorf("ClassLabel expected a value for all label ids [0:%d] but some ids are missing", len(ids))
				}
				list[i] = fmt.Sprint(v)
			}
			cl = cl.Clone()
			cl.Set("names", list)
			m.Set("class_label", cl)
		}
	}
	return m, nil
}

func snakeToCamel(s string) string {
	parts := strings.Split(s, "_")
	for i, p := range parts {
		if p != "" {
			parts[i] = strings.ToUpper(p[:1]) + p[1:]
		}
	}
	return strings.Join(parts, "")
}

// fromYAMLInner returns a JSON-encodable ordered value.
func fromYAMLInner(obj any, depth int) (any, error) {
	if depth > 64 {
		return nil, errors.New("features are nested too deeply")
	}
	switch x := obj.(type) {
	case *Map:
		if len(x.Items) == 0 {
			return &Map{}, nil
		}
		typ := x.Items[0].Key
		rest := func(m *Map, skip string) *Map {
			out := &Map{}
			for _, kv := range m.Items {
				if kv.Key != skip {
					out.Items = append(out.Items, KV{Key: kv.Key, Value: ToJSONValue(kv.Value)})
				}
			}
			return out
		}
		switch typ {
		case "large_list", "list", "sequence":
			u, err := unsimplify(x)
			if err != nil {
				return nil, err
			}
			inner := u.Get(typ)
			name := map[string]string{"large_list": "LargeList", "list": "List", "sequence": "List"}[typ]
			if typ == "sequence" {
				if l, ok := inner.([]any); ok {
					st, err := fromYAMLInner(l, depth+1)
					if err != nil {
						return nil, err
					}
					out := &Map{}
					for _, kv := range st.(*Map).Items {
						item := &Map{Items: []KV{{Key: "feature", Value: kv.Value}}}
						item.Items = append(item.Items, rest(u, typ).Items...)
						item.Items = append(item.Items, KV{Key: "_type", Value: "List"})
						out.Items = append(out.Items, KV{Key: kv.Key, Value: item})
					}
					return out, nil
				}
			}
			f, err := fromYAMLInner(inner, depth+1)
			if err != nil {
				return nil, err
			}
			out := &Map{Items: []KV{{Key: "feature", Value: f}}}
			out.Items = append(out.Items, rest(u, typ).Items...)
			out.Items = append(out.Items, KV{Key: "_type", Value: name})
			return out, nil
		case "struct":
			return fromYAMLInner(x.Get("struct"), depth+1)
		case "dtype":
			if s, ok := x.Get("dtype").(string); ok {
				if _, err := features.ParseDType(s); err == nil {
					out := ToJSONValue(x).(*Map)
					out.Items = append(out.Items, KV{Key: "_type", Value: "Value"})
					return out, nil
				}
				return &Map{Items: []KV{{Key: "_type", Value: snakeToCamel(s)}}}, nil
			}
			return fromYAMLInner(x.Get("dtype"), depth+1)
		default:
			u, err := unsimplify(x)
			if err != nil {
				return nil, err
			}
			out := &Map{Items: []KV{{Key: "_type", Value: snakeToCamel(typ)}}}
			if inner, ok := u.Get(typ).(*Map); ok {
				out.Items = append(out.Items, ToJSONValue(inner).(*Map).Items...)
			}
			return out, nil
		}
	case []any:
		out := &Map{}
		for _, item := range x {
			m, ok := item.(*Map)
			if !ok {
				return nil, errors.New("each feature must be a mapping with a name")
			}
			name, ok := m.Get("name").(string)
			if !ok {
				return nil, errors.New("each feature needs a name")
			}
			f, err := fromYAMLInner(rest(m, "name"), depth+1)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", name, err)
			}
			out.Items = append(out.Items, KV{Key: name, Value: f})
		}
		return out, nil
	}
	return nil, fmt.Errorf("expected a mapping or a list, got %s", describe(obj))
}

func rest(m *Map, skip string) *Map {
	out := &Map{}
	for _, kv := range m.Items {
		if kv.Key != skip {
			out.Items = append(out.Items, kv)
		}
	}
	return out
}

func asString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case int64, float64, bool:
		return fmt.Sprint(x)
	}
	return ""
}

func asInt(v any) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case float64:
		return int64(x)
	}
	return 0
}

func stringList(v any) []string {
	switch x := v.(type) {
	case string:
		if x == "" {
			return nil
		}
		return []string{x}
	case []any:
		var out []string
		for _, e := range x {
			if s := asString(e); s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func describe(v any) string {
	switch v.(type) {
	case *Map:
		return "a mapping"
	case []any:
		return "a list"
	case string:
		return "a string"
	case nil:
		return "nothing"
	}
	return fmt.Sprintf("%T", v)
}
