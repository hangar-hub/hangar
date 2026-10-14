package card

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

// KV is one key and value of a Map.
type KV struct {
	Key   string
	Value any
}

// Map is a YAML or JSON mapping that keeps its key order. Values are nil,
// bool, int64, float64, string, []any or *Map.
type Map struct {
	Items []KV
}

// Get returns the value of a key, or nil.
func (m *Map) Get(key string) any {
	if m == nil {
		return nil
	}
	for _, kv := range m.Items {
		if kv.Key == key {
			return kv.Value
		}
	}
	return nil
}

// Has reports whether the key is present.
func (m *Map) Has(key string) bool {
	if m == nil {
		return false
	}
	for _, kv := range m.Items {
		if kv.Key == key {
			return true
		}
	}
	return false
}

// Set replaces a key's value, or appends the key.
func (m *Map) Set(key string, v any) {
	for i, kv := range m.Items {
		if kv.Key == key {
			m.Items[i].Value = v
			return
		}
	}
	m.Items = append(m.Items, KV{Key: key, Value: v})
}

// Clone returns a shallow copy.
func (m *Map) Clone() *Map {
	return &Map{Items: append([]KV(nil), m.Items...)}
}

// MarshalJSON writes the mapping in key order.
func (m *Map) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, kv := range m.Items {
		if i > 0 {
			b.WriteByte(',')
		}
		k, _ := json.Marshal(kv.Key)
		b.Write(k)
		b.WriteByte(':')
		v, err := json.Marshal(ToJSONValue(kv.Value))
		if err != nil {
			return nil, err
		}
		b.Write(v)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// ToJSONValue makes a value safe for encoding/json: non-finite floats
// become strings.
func ToJSONValue(v any) any {
	switch x := v.(type) {
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return fmt.Sprint(x)
		}
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = ToJSONValue(e)
		}
		return out
	case *Map:
		out := &Map{Items: make([]KV, len(x.Items))}
		for i, kv := range x.Items {
			out.Items[i] = KV{Key: kv.Key, Value: ToJSONValue(kv.Value)}
		}
		return out
	}
	return v
}

const maxNodeDepth = 256

func fromNode(n *yaml.Node, depth int) (any, error) {
	if depth > maxNodeDepth {
		return nil, errors.New("the YAML is nested too deeply")
	}
	switch n.Kind {
	case yaml.DocumentNode:
		if len(n.Content) == 0 {
			return nil, nil
		}
		return fromNode(n.Content[0], depth+1)
	case yaml.AliasNode:
		return fromNode(n.Alias, depth+1)
	case yaml.MappingNode:
		m := &Map{}
		for i := 0; i+1 < len(n.Content); i += 2 {
			k := n.Content[i]
			if k.Tag == "!!merge" {
				merged, err := fromNode(n.Content[i+1], depth+1)
				if err != nil {
					return nil, err
				}
				if mm, ok := merged.(*Map); ok {
					for _, kv := range mm.Items {
						if !m.Has(kv.Key) {
							m.Items = append(m.Items, kv)
						}
					}
				}
				continue
			}
			v, err := fromNode(n.Content[i+1], depth+1)
			if err != nil {
				return nil, err
			}
			m.Set(k.Value, v)
		}
		return m, nil
	case yaml.SequenceNode:
		out := make([]any, 0, len(n.Content))
		for _, c := range n.Content {
			v, err := fromNode(c, depth+1)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil
	case yaml.ScalarNode:
		return scalar(n)
	}
	return nil, fmt.Errorf("unexpected YAML node kind %d", n.Kind)
}

func scalar(n *yaml.Node) (any, error) {
	switch n.ShortTag() {
	case "!!null":
		return nil, nil
	case "!!bool":
		var b bool
		if err := n.Decode(&b); err != nil {
			return nil, err
		}
		return b, nil
	case "!!int":
		var i int64
		if err := n.Decode(&i); err == nil {
			return i, nil
		}
		var f float64
		if err := n.Decode(&f); err != nil {
			return nil, err
		}
		return f, nil
	case "!!float":
		var f float64
		if err := n.Decode(&f); err != nil {
			s := strings.ToLower(n.Value)
			switch s {
			case ".nan":
				return math.NaN(), nil
			case ".inf", "+.inf":
				return math.Inf(1), nil
			case "-.inf":
				return math.Inf(-1), nil
			}
			return strconv.ParseFloat(n.Value, 64)
		}
		return f, nil
	}
	return n.Value, nil
}
