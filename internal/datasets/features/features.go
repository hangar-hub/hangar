package features

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Kind is the `_type` of a feature.
type Kind string

// Feature kinds, named as in `datasets`. KindStruct is a plain JSON object
// of sub-features, and KindLegacyList is the old one-element JSON array form.
const (
	KindValue                        Kind = "Value"
	KindClassLabel                   Kind = "ClassLabel"
	KindList                         Kind = "List"
	KindLargeList                    Kind = "LargeList"
	KindLegacyList                   Kind = "list"
	KindStruct                       Kind = "dict"
	KindImage                        Kind = "Image"
	KindAudio                        Kind = "Audio"
	KindVideo                        Kind = "Video"
	KindPdf                          Kind = "Pdf"
	KindJSON                         Kind = "Json"
	KindTranslation                  Kind = "Translation"
	KindTranslationVariableLanguages Kind = "TranslationVariableLanguages"
	KindArray2D                      Kind = "Array2D"
	KindArray3D                      Kind = "Array3D"
	KindArray4D                      Kind = "Array4D"
	KindArray5D                      Kind = "Array5D"
)

// ErrUnsupported means a feature type Hangar cannot store or convert.
var ErrUnsupported = errors.New("unsupported feature type")

// Feature is one `datasets` feature type.
type Feature struct {
	Kind Kind
	// Dtype is the dtype of a Value or an ArrayND feature.
	Dtype string
	// Names are the class names of a ClassLabel.
	Names []string
	// Elem is the element feature of lists.
	Elem *Feature
	// Length is the fixed length of a List, or -1.
	Length int
	// Fields are the sub-features of a struct, in order.
	Fields []Field
	// Params holds the other dataclass fields that differ from their
	// defaults, such as an Audio sampling_rate, in the order `datasets`
	// writes them.
	Params []Param
}

// Field is a named sub-feature.
type Field struct {
	Name    string
	Feature *Feature
}

// Param is a feature parameter and its JSON value.
type Param struct {
	Key   string
	Value json.RawMessage
}

// paramSpec lists the dataclass fields of each kind, in declaration order,
// with their defaults as JSON. Fields equal to their default are left out
// of the JSON, as datasets.utils.py_utils.asdict does.
var paramSpec = map[Kind][][2]string{
	KindValue:      {{"id", "null"}},
	KindClassLabel: {{"id", "null"}},
	KindImage:      {{"mode", "null"}, {"decode", "true"}, {"id", "null"}},
	KindAudio:      {{"sampling_rate", "null"}, {"decode", "true"}, {"num_channels", "null"}, {"stream_index", "null"}, {"id", "null"}},
	KindVideo: {{"decode", "true"}, {"stream_index", "null"}, {"dimension_order", `"NCHW"`}, {"num_ffmpeg_threads", "1"},
		{"device", `"cpu"`}, {"seek_mode", `"exact"`}, {"id", "null"}},
	KindPdf:                          {{"decode", "true"}, {"id", "null"}},
	KindJSON:                         {{"decode", "true"}, {"id", "null"}},
	KindList:                         {{"id", "null"}},
	KindLargeList:                    {{"id", "null"}},
	KindTranslation:                  {{"languages", "null"}, {"id", "null"}},
	KindTranslationVariableLanguages: {{"languages", "null"}, {"num_languages", "null"}, {"id", "null"}},
	KindArray2D:                      {{"shape", "null"}, {"id", "null"}},
	KindArray3D:                      {{"shape", "null"}, {"id", "null"}},
	KindArray4D:                      {{"shape", "null"}, {"id", "null"}},
	KindArray5D:                      {{"shape", "null"}, {"id", "null"}},
}

// Value returns a Value feature.
func Value(dtype string) *Feature { return &Feature{Kind: KindValue, Dtype: dtype} }

// ListOfFeature returns a variable-length List feature.
func ListOfFeature(elem *Feature) *Feature { return &Feature{Kind: KindList, Elem: elem, Length: -1} }

// Param returns the JSON value of a parameter, or nil.
func (f *Feature) Param(key string) json.RawMessage {
	for _, p := range f.Params {
		if p.Key == key {
			return p.Value
		}
	}
	return nil
}

// Field returns the index of a struct field, or -1.
func (f *Feature) Field(name string) int {
	for i, fl := range f.Fields {
		if fl.Name == name {
			return i
		}
	}
	return -1
}

// IsMedia reports whether the feature stores files as {bytes, path}.
func (f *Feature) IsMedia() bool {
	switch f.Kind {
	case KindImage, KindAudio, KindVideo, KindPdf:
		return true
	}
	return false
}

// IsListKind reports whether the feature is a list of any form.
func (f *Feature) IsListKind() bool {
	return f.Kind == KindList || f.Kind == KindLargeList || f.Kind == KindLegacyList
}

// Contains reports whether f or any nested feature satisfies pred.
func (f *Feature) Contains(pred func(*Feature) bool) bool {
	if pred(f) {
		return true
	}
	if f.Elem != nil && f.Elem.Contains(pred) {
		return true
	}
	for _, fl := range f.Fields {
		if fl.Feature.Contains(pred) {
			return true
		}
	}
	return false
}

// MarshalJSON writes the Features JSON form that `datasets` writes.
func (f *Feature) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	if err := f.write(&b); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func writeKey(b *bytes.Buffer, k string, first *bool) {
	if !*first {
		b.WriteByte(',')
	}
	*first = false
	kb, _ := json.Marshal(k)
	b.Write(kb)
	b.WriteByte(':')
}

func (f *Feature) write(b *bytes.Buffer) error {
	switch f.Kind {
	case KindStruct:
		b.WriteByte('{')
		first := true
		for _, fl := range f.Fields {
			writeKey(b, fl.Name, &first)
			if err := fl.Feature.write(b); err != nil {
				return err
			}
		}
		b.WriteByte('}')
		return nil
	case KindLegacyList:
		b.WriteByte('[')
		if err := f.Elem.write(b); err != nil {
			return err
		}
		b.WriteByte(']')
		return nil
	}
	b.WriteByte('{')
	first := true
	switch f.Kind {
	case KindValue:
		writeKey(b, "dtype", &first)
		d, _ := json.Marshal(f.Dtype)
		b.Write(d)
	case KindClassLabel:
		writeKey(b, "names", &first)
		names := f.Names
		if names == nil {
			names = []string{}
		}
		d, _ := json.Marshal(names)
		b.Write(d)
	case KindList, KindLargeList:
		writeKey(b, "feature", &first)
		if err := f.Elem.write(b); err != nil {
			return err
		}
		if f.Kind == KindList && f.Length >= 0 {
			writeKey(b, "length", &first)
			fmt.Fprintf(b, "%d", f.Length)
		}
	case KindArray2D, KindArray3D, KindArray4D, KindArray5D:
		if v := f.Param("shape"); v != nil {
			writeKey(b, "shape", &first)
			b.Write(v)
		}
		writeKey(b, "dtype", &first)
		d, _ := json.Marshal(f.Dtype)
		b.Write(d)
	}
	for _, spec := range paramSpec[f.Kind] {
		if spec[0] == "shape" && strings.HasPrefix(string(f.Kind), "Array") {
			continue
		}
		if v := f.Param(spec[0]); v != nil && !jsonEqual(v, []byte(spec[1])) {
			writeKey(b, spec[0], &first)
			b.Write(v)
		}
	}
	writeKey(b, "_type", &first)
	d, _ := json.Marshal(string(f.Kind))
	b.Write(d)
	b.WriteByte('}')
	return nil
}

func jsonEqual(a, b []byte) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return bytes.Equal(a, b)
	}
	xa, _ := json.Marshal(x)
	ya, _ := json.Marshal(y)
	return bytes.Equal(xa, ya)
}

// UnmarshalJSON reads any Features JSON form `datasets` accepts, including
// legacy Sequence features, which become a List or, for a struct, a struct
// of Lists, as `datasets` 4 and later load them.
func (f *Feature) UnmarshalJSON(data []byte) error {
	g, err := parseFeature(data, 0)
	if err != nil {
		return err
	}
	*f = *g
	return nil
}

const maxDepth = 64

// ParseFeature parses Features JSON for one column.
func ParseFeature(data []byte) (*Feature, error) { return parseFeature(data, 0) }

func parseFeature(data []byte, depth int) (*Feature, error) {
	if depth > maxDepth {
		return nil, errors.New("features are nested too deeply")
	}
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return nil, errors.New("empty feature")
	}
	if data[0] == '[' {
		var items []json.RawMessage
		if err := json.Unmarshal(data, &items); err != nil {
			return nil, err
		}
		if len(items) != 1 {
			return nil, fmt.Errorf("a list feature must have exactly one element, got %d", len(items))
		}
		elem, err := parseFeature(items[0], depth+1)
		if err != nil {
			return nil, err
		}
		return &Feature{Kind: KindLegacyList, Elem: elem, Length: -1}, nil
	}
	if data[0] != '{' {
		return nil, fmt.Errorf("a feature must be a JSON object or array, got %.40s", data)
	}
	keys, vals, err := orderedObject(data)
	if err != nil {
		return nil, err
	}
	typ := ""
	for i, k := range keys {
		if k == "_type" {
			if err := json.Unmarshal(vals[i], &typ); err != nil {
				// A struct with a field named _type whose value is a feature.
				typ = ""
			}
		}
	}
	if typ == "" {
		st := &Feature{Kind: KindStruct}
		for i, k := range keys {
			sub, err := parseFeature(vals[i], depth+1)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", k, err)
			}
			st.Fields = append(st.Fields, Field{Name: k, Feature: sub})
		}
		return st, nil
	}
	get := func(k string) json.RawMessage {
		for i, kk := range keys {
			if kk == k {
				return vals[i]
			}
		}
		return nil
	}
	f := &Feature{Kind: Kind(typ), Length: -1}
	switch f.Kind {
	case KindValue:
		if err := json.Unmarshal(get("dtype"), &f.Dtype); err != nil || f.Dtype == "" {
			return nil, errors.New("a Value feature needs a dtype")
		}
		if _, err := ParseDType(f.Dtype); err != nil {
			return nil, err
		}
		// datasets normalises aliases such as "float" to the pyarrow name.
		a, _ := ParseDType(f.Dtype)
		f.Dtype = a.DType()
	case KindClassLabel:
		if raw := get("names"); raw != nil && string(raw) != "null" {
			if err := json.Unmarshal(raw, &f.Names); err != nil {
				return nil, fmt.Errorf("ClassLabel names: %w", err)
			}
		} else if raw := get("num_classes"); raw != nil {
			var n int
			if err := json.Unmarshal(raw, &n); err != nil || n < 0 || n > 1<<20 {
				return nil, errors.New("ClassLabel num_classes must be a small non-negative integer")
			}
			for i := range n {
				f.Names = append(f.Names, fmt.Sprint(i))
			}
		}
	case KindList, KindLargeList, "Sequence":
		raw := get("feature")
		if raw == nil {
			return nil, fmt.Errorf("a %s feature needs a feature", typ)
		}
		elem, err := parseFeature(raw, depth+1)
		if err != nil {
			return nil, err
		}
		f.Elem = elem
		if l := get("length"); l != nil && string(l) != "null" {
			if err := json.Unmarshal(l, &f.Length); err != nil || f.Length < -1 {
				return nil, errors.New("list length must be an integer")
			}
		}
		if typ == "Sequence" {
			f.Kind = KindList
			// A Sequence of a struct is a struct of Lists, unless every
			// sub-feature is already a List.
			if elem.Kind == KindStruct && !allLists(elem) {
				st := &Feature{Kind: KindStruct}
				for _, fl := range elem.Fields {
					st.Fields = append(st.Fields, Field{Name: fl.Name, Feature: &Feature{Kind: KindList, Elem: fl.Feature, Length: f.Length}})
				}
				return st, nil
			}
		}
		if f.Kind == KindLargeList {
			f.Length = -1
		}
	case KindArray2D, KindArray3D, KindArray4D, KindArray5D:
		if err := json.Unmarshal(get("dtype"), &f.Dtype); err != nil {
			return nil, fmt.Errorf("%s needs a dtype", typ)
		}
	case KindImage, KindAudio, KindVideo, KindPdf, KindJSON, KindTranslation, KindTranslationVariableLanguages:
	default:
		return nil, fmt.Errorf("%w: %q", ErrUnsupported, typ)
	}
	for _, spec := range paramSpec[f.Kind] {
		if v := get(spec[0]); v != nil {
			f.Params = append(f.Params, Param{Key: spec[0], Value: slices.Clone(v)})
		}
	}
	return f, nil
}

func allLists(st *Feature) bool {
	for _, fl := range st.Fields {
		if fl.Feature.Kind != KindList {
			return false
		}
	}
	return true
}

// orderedObject returns the keys and raw values of a JSON object in order.
func orderedObject(data []byte) ([]string, []json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return nil, nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, nil, errors.New("expected a JSON object")
	}
	var keys []string
	var vals []json.RawMessage
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, nil, err
		}
		k, ok := tok.(string)
		if !ok {
			return nil, nil, errors.New("expected an object key")
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, nil, err
		}
		keys = append(keys, k)
		vals = append(vals, v)
	}
	if _, err := dec.Token(); err != nil {
		return nil, nil, err
	}
	return keys, vals, nil
}

// Features is an ordered set of named columns.
type Features []Field

// MarshalJSON writes the Features as a JSON object in column order.
func (fs Features) MarshalJSON() ([]byte, error) {
	return (&Feature{Kind: KindStruct, Fields: fs}).MarshalJSON()
}

// UnmarshalJSON reads a Features JSON object, keeping column order.
func (fs *Features) UnmarshalJSON(data []byte) error {
	f, err := ParseFeatures(data)
	if err != nil {
		return err
	}
	*fs = f
	return nil
}

// ParseFeatures parses a Features JSON object.
func ParseFeatures(data []byte) (Features, error) {
	f, err := parseFeature(data, 0)
	if err != nil {
		return nil, err
	}
	if f.Kind != KindStruct {
		return nil, errors.New("features must be a JSON object of columns")
	}
	return Features(f.Fields), nil
}

// Names returns the column names.
func (fs Features) Names() []string {
	out := make([]string, len(fs))
	for i, f := range fs {
		out[i] = f.Name
	}
	return out
}

// Index returns the position of a column, or -1.
func (fs Features) Index(name string) int {
	for i, f := range fs {
		if f.Name == name {
			return i
		}
	}
	return -1
}

// Storage returns the Arrow type that stores a feature's values.
func (f *Feature) Storage() (*Arrow, error) {
	switch f.Kind {
	case KindValue:
		return ParseDType(f.Dtype)
	case KindClassLabel:
		return Primitive(Int64), nil
	case KindList, KindLegacyList:
		e, err := f.Elem.Storage()
		if err != nil {
			return nil, err
		}
		if f.Kind == KindList && f.Length >= 0 {
			return &Arrow{ID: FixedSizeList, Elem: e, Size: f.Length}, nil
		}
		return ListOf(e), nil
	case KindLargeList:
		e, err := f.Elem.Storage()
		if err != nil {
			return nil, err
		}
		return &Arrow{ID: LargeList, Elem: e}, nil
	case KindStruct:
		st := &Arrow{ID: Struct}
		for _, fl := range f.Fields {
			t, err := fl.Feature.Storage()
			if err != nil {
				return nil, fmt.Errorf("%s: %w", fl.Name, err)
			}
			st.Fields = append(st.Fields, ArrowField{Name: fl.Name, Type: t})
		}
		return st, nil
	case KindImage, KindAudio, KindVideo, KindPdf:
		return MediaStorage(), nil
	case KindJSON:
		return Primitive(String), nil
	case KindTranslation:
		var langs []string
		if raw := f.Param("languages"); raw != nil {
			if err := json.Unmarshal(raw, &langs); err != nil {
				return nil, fmt.Errorf("translation languages: %w", err)
			}
		}
		sorted := slices.Clone(langs)
		slices.Sort(sorted)
		st := &Arrow{ID: Struct}
		for _, l := range sorted {
			st.Fields = append(st.Fields, ArrowField{Name: l, Type: Primitive(String)})
		}
		return st, nil
	case KindTranslationVariableLanguages:
		return StructOf(ArrowField{Name: "language", Type: ListOf(Primitive(String))},
			ArrowField{Name: "translation", Type: ListOf(Primitive(String))}), nil
	}
	return nil, fmt.Errorf("%w: %s", ErrUnsupported, f.Kind)
}

// FromArrow returns the feature `datasets` infers for an Arrow type, as
// Features.from_arrow_schema does when a file has no `datasets` metadata.
func FromArrow(a *Arrow) *Feature {
	switch a.ID {
	case Struct:
		st := &Feature{Kind: KindStruct}
		for _, fl := range a.Fields {
			st.Fields = append(st.Fields, Field{Name: fl.Name, Feature: FromArrow(fl.Type)})
		}
		return st
	case FixedSizeList:
		return &Feature{Kind: KindList, Elem: FromArrow(a.Elem), Length: a.Size}
	case List:
		return &Feature{Kind: KindList, Elem: FromArrow(a.Elem), Length: -1}
	case LargeList:
		return &Feature{Kind: KindLargeList, Elem: FromArrow(a.Elem), Length: -1}
	}
	return Value(a.DType())
}

// StorageSchema returns the Struct Arrow type of every column.
func (fs Features) StorageSchema() (*Arrow, error) {
	return (&Feature{Kind: KindStruct, Fields: fs}).Storage()
}

// FromArrowSchema returns the Features of every column of a Struct type.
func FromArrowSchema(a *Arrow) Features {
	return Features(FromArrow(a).Fields)
}
