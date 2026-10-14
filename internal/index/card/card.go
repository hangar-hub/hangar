// Package card reads the YAML front matter of a Model card or dataset card
// (`cardData` on the Hub). Parsing is safe for untrusted input: the front
// matter is limited to 1 MiB, anchors and aliases are refused, and values
// become plain JSON types.
package card

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"strconv"

	yaml "go.yaml.in/yaml/v3"
)

// MaxFrontMatter is the largest front matter Hangar parses.
const MaxFrontMatter = 1 << 20

// maxDepth bounds nesting.
const maxDepth = 64

// ErrInvalid means the front matter is not valid YAML that Hangar accepts.
var ErrInvalid = errors.New("invalid card metadata")

// Split returns the YAML front matter of a card and the body after it.
// ok is false when the card has no front matter.
func Split(readme []byte) (front, body []byte, ok bool) {
	b := bytes.TrimPrefix(readme, []byte("\xef\xbb\xbf"))
	if !bytes.HasPrefix(b, []byte("---\n")) && !bytes.HasPrefix(b, []byte("---\r\n")) {
		return nil, readme, false
	}
	rest := b[bytes.IndexByte(b, '\n')+1:]
	pos := 0
	for pos <= len(rest) {
		end := bytes.IndexByte(rest[pos:], '\n')
		line := rest[pos:]
		next := len(rest)
		if end >= 0 {
			line = rest[pos : pos+end]
			next = pos + end + 1
		}
		if string(bytes.TrimRight(line, "\r \t")) == "---" {
			return rest[:pos], rest[next:], true
		}
		if end < 0 {
			break
		}
		pos = next
	}
	return nil, readme, false
}

// Parse parses card front matter into JSON-compatible values. An empty
// front matter gives an empty map.
func Parse(front []byte) (map[string]any, error) {
	if len(front) > MaxFrontMatter {
		return nil, fmt.Errorf("%w: the metadata is larger than %d bytes", ErrInvalid, MaxFrontMatter)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(front, &doc); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if doc.Kind == 0 || len(doc.Content) == 0 {
		return map[string]any{}, nil
	}
	v, err := convert(doc.Content[0], 0)
	if err != nil {
		return nil, err
	}
	switch m := v.(type) {
	case map[string]any:
		return m, nil
	case nil:
		return map[string]any{}, nil
	default:
		return nil, fmt.Errorf("%w: the metadata must be a mapping", ErrInvalid)
	}
}

// FromReadme parses the front matter of a README.md, or returns nil when
// it has none.
func FromReadme(readme []byte) (map[string]any, error) {
	front, _, ok := Split(readme)
	if !ok {
		return nil, nil
	}
	return Parse(front)
}

func convert(n *yaml.Node, depth int) (any, error) {
	if depth > maxDepth {
		return nil, fmt.Errorf("%w: nesting is deeper than %d levels", ErrInvalid, maxDepth)
	}
	if n.Anchor != "" || n.Kind == yaml.AliasNode {
		return nil, fmt.Errorf("%w: anchors and aliases are not allowed (line %d)", ErrInvalid, n.Line)
	}
	switch n.Kind {
	case yaml.MappingNode:
		out := make(map[string]any, len(n.Content)/2)
		for i := 0; i+1 < len(n.Content); i += 2 {
			k := n.Content[i]
			if k.Kind != yaml.ScalarNode {
				return nil, fmt.Errorf("%w: keys must be scalars (line %d)", ErrInvalid, k.Line)
			}
			if k.Tag == "!!merge" {
				return nil, fmt.Errorf("%w: merge keys are not allowed (line %d)", ErrInvalid, k.Line)
			}
			v, err := convert(n.Content[i+1], depth+1)
			if err != nil {
				return nil, err
			}
			out[k.Value] = v
		}
		return out, nil
	case yaml.SequenceNode:
		out := make([]any, 0, len(n.Content))
		for _, c := range n.Content {
			v, err := convert(c, depth+1)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil
	case yaml.ScalarNode:
		return scalar(n)
	case yaml.DocumentNode:
		if len(n.Content) == 0 {
			return nil, nil
		}
		return convert(n.Content[0], depth+1)
	}
	return nil, fmt.Errorf("%w: unsupported node (line %d)", ErrInvalid, n.Line)
}

func scalar(n *yaml.Node) (any, error) {
	switch n.ShortTag() {
	case "!!null":
		return nil, nil
	case "!!bool":
		if b, err := strconv.ParseBool(n.Value); err == nil {
			return b, nil
		}
		var v bool
		if n.Decode(&v) == nil {
			return v, nil
		}
		return n.Value, nil
	case "!!int":
		var v int64
		if n.Decode(&v) == nil {
			return v, nil
		}
		return n.Value, nil
	case "!!float":
		var v float64
		if n.Decode(&v) == nil && !math.IsInf(v, 0) && !math.IsNaN(v) {
			return v, nil
		}
		return n.Value, nil
	case "!!str", "!!binary", "!!timestamp":
		return n.Value, nil
	}
	return nil, fmt.Errorf("%w: tag %q is not allowed (line %d)", ErrInvalid, n.Tag, n.Line)
}

// Strings returns a card field that may be a string or a list of strings.
func Strings(data map[string]any, key string) []string {
	switch v := data[key].(type) {
	case string:
		if v == "" {
			return nil
		}
		return []string{v}
	case []any:
		var out []string
		for _, x := range v {
			if s, ok := x.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// String returns a string card field, or the first of a list.
func String(data map[string]any, key string) string {
	if s := Strings(data, key); len(s) > 0 {
		return s[0]
	}
	return ""
}
