package convert

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/hangar-hub/hangar/internal/datasets/features"
)

// jsonChunkBytes is the JsonConfig chunksize (10 MB).
const jsonChunkBytes = 10 << 20

// maxJSONDocument caps a JSON file read whole (a JSON array or `field`).
const maxJSONDocument = 1 << 31

// decodeValue decodes one JSON value, keeping object key order.
func decodeValue(dec *json.Decoder, depth int) (any, error) {
	if depth > 512 {
		return nil, errors.New("JSON nested too deeply")
	}
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			o := &orderedObject{}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				k, ok := kt.(string)
				if !ok {
					return nil, errors.New("expected an object key")
				}
				v, err := decodeValue(dec, depth+1)
				if err != nil {
					return nil, err
				}
				replaced := false
				for i, existing := range o.keys {
					if existing == k {
						o.vals[i] = v
						replaced = true
					}
				}
				if !replaced {
					o.keys = append(o.keys, k)
					o.vals = append(o.vals, v)
				}
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return o, nil
		case '[':
			out := []any{}
			for dec.More() {
				v, err := decodeValue(dec, depth+1)
				if err != nil {
					return nil, err
				}
				out = append(out, v)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return out, nil
		}
		return nil, fmt.Errorf("unexpected %v", t)
	}
	return tok, nil
}

func decodeJSON(b []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	v, err := decodeValue(dec, 0)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("extra data after the JSON value")
	}
	return v, nil
}

// jsonExamples splits a chunk into JSON Lines examples.
func jsonExamples(chunk []byte) ([]any, error) {
	var out []any
	for i, line := range bytes.Split(chunk, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		v, err := decodeJSON(line)
		if err != nil {
			return nil, fmt.Errorf("JSON parse error in line %d of the chunk: %w", i+1, err)
		}
		out = append(out, v)
	}
	return out, nil
}

type jsonBuilder struct{}

func (jsonBuilder) name() string { return "json" }

// jsonChunks yields the examples of a member in the chunks `datasets`
// reads: a JSON Lines file by 10 MB chunks, a JSON array or a `field` of
// an object whole.
func jsonChunks(m member, field string, fn func(examples []any) error) error {
	rc, err := m.open()
	if err != nil {
		return err
	}
	defer rc.Close()
	if field != "" {
		raw, err := readAllLimit(rc, maxJSONDocument)
		if err != nil {
			return err
		}
		doc, err := decodeJSON(bytes.TrimPrefix(raw, []byte{0xef, 0xbb, 0xbf}))
		if err != nil {
			return err
		}
		obj, ok := doc.(*orderedObject)
		if !ok {
			return fmt.Errorf("the JSON file is not an object, so it has no field %q", field)
		}
		v, ok := obj.get(field)
		if !ok {
			return fmt.Errorf("the JSON file has no field %q", field)
		}
		list, ok := v.([]any)
		if !ok {
			return fmt.Errorf("the JSON field %q is not a list", field)
		}
		return fn(wrapStrings(list))
	}
	cr := newChunkReader(rc, jsonChunkBytes)
	first := true
	for {
		chunk, err := cr.next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if first {
			chunk = bytes.TrimPrefix(chunk, []byte{0xef, 0xbb, 0xbf})
			first = false
			if bytes.HasPrefix(chunk, []byte("[")) {
				rest, err := readAllLimit(cr.r, maxJSONDocument)
				if err != nil {
					return err
				}
				doc, err := decodeJSON(append(chunk, rest...))
				if err != nil {
					return err
				}
				return fn(wrapStrings(doc.([]any)))
			}
		}
		ex, err := jsonExamples(chunk)
		if err != nil {
			return err
		}
		if len(ex) > 0 {
			if err := fn(ex); err != nil {
				return err
			}
		}
	}
}

// wrapStrings turns a list of strings into {"text": ...} examples, as the
// JSON builder does for a JSON list of strings.
func wrapStrings(list []any) []any {
	out := make([]any, len(list))
	for i, v := range list {
		if _, ok := v.(*orderedObject); ok {
			out[i] = v
			continue
		}
		out[i] = &orderedObject{keys: []string{"text"}, vals: []any{v}}
	}
	return out
}

func (b jsonBuilder) infer(e *env, splits []splitFiles) (features.Features, error) {
	field, _ := optString(e.opts, "field")
	if s, ok := optString(e.opts, "encoding"); ok && !strings.EqualFold(strings.ReplaceAll(s, "_", "-"), "utf-8") && !strings.EqualFold(s, "utf8") {
		return nil, fmt.Errorf("%w: encoding %q", ErrUnsupported, s)
	}
	var found features.Features
	stop := errors.New("stop")
	if len(splits) == 0 {
		return nil, errors.New("no JSON files")
	}
	for _, f := range splits[0].files {
		ms, err := members(e.ctx, f)
		if err != nil {
			return nil, err
		}
		for _, m := range ms {
			err := jsonChunks(m, field, func(ex []any) error {
				found = inferExamples(ex)
				return stop
			})
			if errors.Is(err, stop) {
				return found, nil
			}
			if err != nil {
				return nil, fmt.Errorf("%s: %w", f.Path, err)
			}
		}
	}
	return nil, errors.New("the JSON files are empty")
}

// inferExamples infers Features from examples, as `datasets` does from the
// first chunk: objects with differing keys below the top level, and values
// of conflicting types, become Json columns.
func inferExamples(ex []any) features.Features {
	t := &itype{id: features.Struct}
	for _, e := range ex {
		if _, ok := e.(*orderedObject); !ok {
			e = &orderedObject{keys: []string{"text"}, vals: []any{e}}
		}
		t = merge(t, inferJSON(e, true, 0))
	}
	for _, p := range mixedStructPaths(ex) {
		markJSON(t, p)
	}
	if t.json {
		return features.Features{{Name: "text", Feature: &features.Feature{Kind: features.KindJSON, Length: -1}}}
	}
	return features.Features(t.toFeature().Fields)
}

// jsonCast converts a decoded example field to a feature's storage,
// JSON-encoding Json features as `datasets` does (json_encode_field).
func jsonCast(v any, f *features.Feature) (any, error) {
	if v == nil {
		return nil, nil
	}
	switch f.Kind {
	case features.KindJSON:
		if s, ok := v.(string); ok {
			if _, err := decodeJSON([]byte(s)); err == nil {
				return s, nil
			}
		}
		return pandasJSON(v), nil
	case features.KindStruct:
		obj, ok := v.(*orderedObject)
		if !ok {
			break
		}
		out := make([]any, len(f.Fields))
		for i, fl := range f.Fields {
			fv, _ := obj.get(fl.Name)
			c, err := jsonCast(fv, fl.Feature)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", fl.Name, err)
			}
			out[i] = c
		}
		return out, nil
	case features.KindList, features.KindLargeList, features.KindLegacyList:
		list, ok := v.([]any)
		if !ok {
			break
		}
		out := make([]any, len(list))
		for i, e := range list {
			c, err := jsonCast(e, f.Elem)
			if err != nil {
				return nil, err
			}
			out[i] = c
		}
		st, err := f.Storage()
		if err != nil {
			return nil, err
		}
		if st.ID == features.FixedSizeList && len(out) != st.Size {
			return nil, fmt.Errorf("%w: a list of %d values for %s", ErrCast, len(out), st)
		}
		return out, nil
	}
	st, err := f.Storage()
	if err != nil {
		return nil, err
	}
	return CastFeature(v, f, st)
}

func (b jsonBuilder) read(e *env, split splitFiles, fs features.Features, emit emitFunc) error {
	field, _ := optString(e.opts, "field")
	// JSON Lines and JSON arrays go through pyarrow's JSON reader; a
	// `field` is extracted in Python and loaded from objects.
	e.nullStructChildren = field == ""
	defer func() { e.nullStructChildren = false }()
	names := map[string]bool{}
	for _, f := range fs {
		names[f.Name] = true
	}
	for _, f := range split.files {
		ms, err := members(e.ctx, f)
		if err != nil {
			return err
		}
		for _, m := range ms {
			err := jsonChunks(m, field, func(ex []any) error {
				rows := make([][]any, len(ex))
				for i, e := range ex {
					obj, ok := e.(*orderedObject)
					if !ok {
						obj = &orderedObject{keys: []string{"text"}, vals: []any{e}}
					}
					for _, k := range obj.keys {
						if !names[k] {
							return fmt.Errorf("couldn't cast the JSON data to the features: column %q is not in %v", k, fs.Names())
						}
					}
					row := make([]any, len(fs))
					for j, fl := range fs {
						v, _ := obj.get(fl.Name)
						c, err := jsonCast(v, fl.Feature)
						if err != nil {
							return fmt.Errorf("column %q: %w", fl.Name, err)
						}
						row[j] = c
					}
					rows[i] = row
				}
				return emit(rows)
			})
			if err != nil {
				return fmt.Errorf("%s: %w", f.Path, err)
			}
		}
	}
	return nil
}
