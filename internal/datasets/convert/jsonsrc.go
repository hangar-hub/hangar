package convert

import (
	"bufio"
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

// utf8BOM is the UTF-8 byte order mark, which `datasets` ignores.
var utf8BOM = []byte{0xef, 0xbb, 0xbf}

// valueLimit fails a read once more than max bytes have been read since
// the last reset, so json.Decoder cannot buffer one huge value without
// bound. Reset it before each array item or object member.
type valueLimit struct {
	r      io.Reader
	n, max int64
}

func newValueLimit(r io.Reader) *valueLimit { return &valueLimit{r: r, max: maxValueBytes} }

func (l *valueLimit) reset() { l.n = 0 }

func (l *valueLimit) Read(p []byte) (int, error) {
	if l.n >= l.max {
		return 0, fmt.Errorf("%w: a JSON value longer than %d bytes", ErrUnsupported, l.max)
	}
	if int64(len(p)) > l.max-l.n {
		p = p[:l.max-l.n]
	}
	n, err := l.r.Read(p)
	l.n += int64(n)
	return n, err
}

func newJSONDecoder(r io.Reader) *json.Decoder {
	dec := json.NewDecoder(r)
	dec.UseNumber()
	return dec
}

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
	dec := newJSONDecoder(bytes.NewReader(b))
	v, err := decodeValue(dec, 0)
	if err != nil {
		return nil, err
	}
	if err := jsonEnd(dec); err != nil {
		return nil, err
	}
	return v, nil
}

// jsonEnd checks that nothing but white space follows the JSON value.
func jsonEnd(dec *json.Decoder) error {
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("extra data after the JSON value")
	}
	return nil
}

// skipValue reads past one JSON value without keeping it.
func skipValue(dec *json.Decoder, lim *valueLimit, depth int) error {
	if depth > 512 {
		return errors.New("JSON nested too deeply")
	}
	lim.reset()
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	d, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	if d != '{' && d != '[' {
		return fmt.Errorf("unexpected %v", d)
	}
	for dec.More() {
		if d == '{' {
			lim.reset()
			if _, err := dec.Token(); err != nil {
				return err
			}
		}
		if err := skipValue(dec, lim, depth+1); err != nil {
			return err
		}
	}
	_, err = dec.Token()
	return err
}

// jsonList passes the items of the JSON array dec has just opened to fn
// in batches of about jsonChunkBytes of input, then reads the closing
// bracket. fn sees at least one batch, empty for an empty array.
func jsonList(dec *json.Decoder, lim *valueLimit, depth int, fn func(examples []any, whole bool) error) error {
	batch := []any{}
	start := dec.InputOffset()
	sent := false
	for {
		lim.reset()
		if !dec.More() {
			break
		}
		v, err := decodeValue(dec, depth)
		if err != nil {
			return err
		}
		batch = append(batch, v)
		if dec.InputOffset()-start >= jsonChunkBytes {
			if err := fn(wrapStrings(batch), true); err != nil {
				return err
			}
			batch, start, sent = []any{}, dec.InputOffset(), true
		}
	}
	if _, err := dec.Token(); err != nil {
		return err
	}
	if len(batch) > 0 || !sent {
		return fn(wrapStrings(batch), true)
	}
	return nil
}

// jsonArray streams a file that holds one JSON array.
func jsonArray(r io.Reader, fn func(examples []any, whole bool) error) error {
	lim := newValueLimit(r)
	dec := newJSONDecoder(lim)
	if _, err := dec.Token(); err != nil {
		return err
	}
	if err := jsonList(dec, lim, 1, fn); err != nil {
		return err
	}
	return jsonEnd(dec)
}

// jsonField streams the list in the `field` member of a file that holds
// one JSON object, skipping the other members.
func jsonField(r io.Reader, field string, fn func(examples []any, whole bool) error) error {
	lim := newValueLimit(r)
	dec := newJSONDecoder(lim)
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return fmt.Errorf("the JSON file is not an object, so it has no field %q", field)
	}
	found := false
	for {
		lim.reset()
		if !dec.More() {
			break
		}
		kt, err := dec.Token()
		if err != nil {
			return err
		}
		if k, _ := kt.(string); k != field {
			if err := skipValue(dec, lim, 1); err != nil {
				return err
			}
			continue
		}
		if found {
			// Python keeps the last of repeated keys, but the first has
			// been read by now.
			return fmt.Errorf("%w: the JSON file has the field %q more than once", ErrUnsupported, field)
		}
		found = true
		lim.reset()
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		if d, ok := tok.(json.Delim); !ok || d != '[' {
			return fmt.Errorf("the JSON field %q is not a list", field)
		}
		if err := jsonList(dec, lim, 2, fn); err != nil {
			return err
		}
	}
	if _, err := dec.Token(); err != nil {
		return err
	}
	if err := jsonEnd(dec); err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("the JSON file has no field %q", field)
	}
	return nil
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
// reads: a JSON Lines file by 10 MB chunks, each a table of its own (whole
// is false), and a JSON array or a `field` of an object as one table
// (whole is true), streamed in batches so that a large file is never held
// in memory.
func jsonChunks(m member, field string, fn func(examples []any, whole bool) error) error {
	rc, err := m.open()
	if err != nil {
		return err
	}
	defer rc.Close()
	br := bufio.NewReaderSize(rc, 1<<16)
	if head, _ := br.Peek(len(utf8BOM)); bytes.Equal(head, utf8BOM) {
		_, _ = br.Discard(len(utf8BOM))
	}
	if field != "" {
		return jsonField(br, field, fn)
	}
	if head, _ := br.Peek(1); bytes.Equal(head, []byte("[")) {
		return jsonArray(br, fn)
	}
	cr := newChunkReader(br, jsonChunkBytes)
	for {
		chunk, err := cr.next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		ex, err := jsonExamples(chunk)
		if err != nil {
			return err
		}
		if len(ex) > 0 {
			if err := fn(ex, false); err != nil {
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
			// A JSON Lines file infers from its first chunk; a JSON
			// array or field from all its items, read in batches.
			var whole *inference
			err := jsonChunks(m, field, func(ex []any, oneTable bool) error {
				if oneTable {
					if whole == nil {
						whole = newInference()
					}
					whole.add(ex)
					return nil
				}
				found = inferExamples(ex)
				return stop
			})
			if errors.Is(err, stop) {
				return found, nil
			}
			if err != nil {
				return nil, fmt.Errorf("%s: %w", f.Path, err)
			}
			if whole != nil {
				return whole.features(), nil
			}
		}
	}
	return nil, errors.New("the JSON files are empty")
}

// inference folds examples into Features, so that a list read in batches
// infers what the whole list would.
type inference struct {
	t     *itype
	shape *shapeNode
}

func newInference() *inference {
	return &inference{t: &itype{id: features.Struct}, shape: newShape()}
}

func (in *inference) add(ex []any) {
	for _, e := range ex {
		in.shape.add(e)
		if _, ok := e.(*orderedObject); !ok {
			e = &orderedObject{keys: []string{"text"}, vals: []any{e}}
		}
		in.t = merge(in.t, inferJSON(e, true, 0))
	}
}

func (in *inference) features() features.Features {
	t := in.t
	for _, p := range in.shape.mixedPaths() {
		markJSON(t, p)
	}
	if t.json {
		return features.Features{{Name: "text", Feature: &features.Feature{Kind: features.KindJSON, Length: -1}}}
	}
	return features.Features(t.toFeature().Fields)
}

// inferExamples infers Features from examples, as `datasets` does from the
// first chunk: objects with differing keys below the top level, and values
// of conflicting types, become Json columns.
func inferExamples(ex []any) features.Features {
	in := newInference()
	in.add(ex)
	return in.features()
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
			err := jsonChunks(m, field, func(ex []any, _ bool) error {
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
