package convert

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/jamesagarside/hangar/internal/datasets/features"
)

// csvChunkRows is the pandas chunksize the `datasets` CSV builder uses.
const csvChunkRows = 10_000

// pandasNA is pandas' default set of strings read as missing values.
var pandasNA = []string{"", "#N/A", "#N/A N/A", "#NA", "-1.#IND", "-1.#QNAN", "-NaN", "-nan", "1.#IND", "1.#QNAN",
	"<NA>", "N/A", "NA", "NULL", "NaN", "None", "n/a", "nan", "null"}

// csvOptions are the CsvConfig options Hangar supports.
type csvOptions struct {
	sep           rune
	quote         rune
	header        bool
	names         []string
	skipRows      int
	comment       rune
	na            map[string]bool
	trueValues    map[string]bool
	falseValues   map[string]bool
	encodingLatin bool
}

func optString(opts map[string]any, keys ...string) (string, bool) {
	for _, k := range keys {
		if v, ok := opts[k]; ok && v != nil {
			s, ok := v.(string)
			return s, ok
		}
	}
	return "", false
}

func optStrings(v any) []string {
	switch x := v.(type) {
	case string:
		return []string{x}
	case []any:
		var out []string
		for _, e := range x {
			out = append(out, fmt.Sprint(e))
		}
		return out
	}
	return nil
}

func parseCSVOptions(opts map[string]any) (*csvOptions, error) {
	o := &csvOptions{sep: ',', quote: '"', header: true, na: map[string]bool{}, trueValues: map[string]bool{"True": true, "TRUE": true, "true": true},
		falseValues: map[string]bool{"False": true, "FALSE": true, "false": true}}
	single := func(name, s string) (rune, error) {
		if s == `\t` {
			return '\t', nil
		}
		if utf8.RuneCountInString(s) != 1 {
			return 0, fmt.Errorf("%w: CSV %s %q must be one character", ErrUnsupported, name, s)
		}
		r, _ := utf8.DecodeRuneInString(s)
		return r, nil
	}
	if s, ok := optString(opts, "sep", "delimiter"); ok {
		r, err := single("separator", s)
		if err != nil {
			return nil, err
		}
		o.sep = r
	}
	if s, ok := optString(opts, "quotechar"); ok {
		r, err := single("quote character", s)
		if err != nil {
			return nil, err
		}
		o.quote = r
	}
	if o.quote != '"' {
		return nil, fmt.Errorf("%w: CSV quote characters other than \"", ErrUnsupported)
	}
	if s, ok := optString(opts, "comment"); ok {
		r, err := single("comment character", s)
		if err != nil {
			return nil, err
		}
		o.comment = r
	}
	if v, ok := opts["header"]; ok {
		switch h := v.(type) {
		case nil:
			o.header = false
		case int64:
			if h != 0 {
				return nil, fmt.Errorf("%w: CSV header row %d", ErrUnsupported, h)
			}
		case string:
			if h != "infer" {
				return nil, fmt.Errorf("%w: CSV header %q", ErrUnsupported, h)
			}
		}
	}
	for _, k := range []string{"names", "column_names"} {
		if v, ok := opts[k]; ok && v != nil {
			o.names = optStrings(v)
			if _, hasHeader := opts["header"]; !hasHeader {
				o.header = false
			}
		}
	}
	if v, ok := opts["skiprows"].(int64); ok && v > 0 {
		o.skipRows = int(v)
	}
	keepDefault := true
	if v, ok := opts["keep_default_na"].(bool); ok {
		keepDefault = v
	}
	if keepDefault {
		for _, s := range pandasNA {
			o.na[s] = true
		}
	}
	for _, s := range optStrings(opts["na_values"]) {
		o.na[s] = true
	}
	if v := optStrings(opts["true_values"]); v != nil {
		for _, s := range v {
			o.trueValues[s] = true
		}
	}
	if v := optStrings(opts["false_values"]); v != nil {
		for _, s := range v {
			o.falseValues[s] = true
		}
	}
	if s, ok := optString(opts, "encoding"); ok {
		switch strings.ToLower(strings.ReplaceAll(s, "_", "-")) {
		case "utf-8", "utf8", "utf-8-sig":
		case "latin-1", "latin1", "iso-8859-1":
			o.encodingLatin = true
		default:
			return nil, fmt.Errorf("%w: encoding %q", ErrUnsupported, s)
		}
	}
	return o, nil
}

// csvTable is a parsed CSV chunk: column names and raw cells.
type csvReader struct {
	r     *csvTokenizer
	o     *csvOptions
	names []string
}

func latin1ToUTF8(b []byte) []byte {
	var out bytes.Buffer
	for _, c := range b {
		out.WriteRune(rune(c))
	}
	return out.Bytes()
}

func newCSVReader(r io.Reader, o *csvOptions) (*csvReader, error) {
	if o.encodingLatin {
		raw, err := readAllLimit(r, 1<<34)
		if err != nil {
			return nil, err
		}
		r = bytes.NewReader(latin1ToUTF8(raw))
	}
	br := bytes.NewBuffer(nil)
	// Strip a UTF-8 byte order mark, as pandas does.
	head := make([]byte, 3)
	n, _ := io.ReadFull(r, head)
	if n != 3 || !bytes.Equal(head, []byte{0xef, 0xbb, 0xbf}) {
		br.Write(head[:n])
	}
	cr := newCSVTokenizer(io.MultiReader(br, r), o.sep, o.quote, o.comment)
	c := &csvReader{r: cr, o: o}
	for range o.skipRows {
		if _, err := cr.read(); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, err
		}
	}
	if o.header {
		rec, err := cr.read()
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("%w: no columns to parse from file", ErrUnsupported)
		}
		if err != nil {
			return nil, err
		}
		c.names = mangleNames(rec)
	}
	if o.names != nil {
		c.names = o.names
	}
	return c, nil
}

// mangleNames names unnamed columns and de-duplicates names as pandas does.
func mangleNames(rec []string) []string {
	out := make([]string, len(rec))
	seen := map[string]int{}
	for i, n := range rec {
		if n == "" {
			n = "Unnamed: " + strconv.Itoa(i)
		}
		base := n
		for seen[n] > 0 {
			n = base + "." + strconv.Itoa(seen[base])
			seen[base]++
		}
		seen[n]++
		out[i] = n
	}
	return out
}

// next reads up to csvChunkRows rows.
func (c *csvReader) next() ([][]string, error) {
	var rows [][]string
	for len(rows) < csvChunkRows {
		rec, err := c.r.read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if c.names == nil {
			c.names = make([]string, len(rec))
			for i := range rec {
				c.names[i] = strconv.Itoa(i)
			}
		}
		if len(rec) > len(c.names) {
			return nil, fmt.Errorf("error tokenizing data: expected %d fields in line %d, saw %d", len(c.names), c.r.line, len(rec))
		}
		rows = append(rows, rec)
	}
	if len(rows) == 0 {
		return nil, io.EOF
	}
	return rows, nil
}

var intRe = regexp.MustCompile(`^\s*[+-]?\d+\s*$`)

// csvColumnType is the dtype pandas infers for a column of a chunk.
func csvColumnType(cells []string, present []bool, o *csvOptions) *features.Arrow {
	allNA, anyNA := true, false
	for i := range cells {
		if present[i] && !o.na[cells[i]] {
			allNA = false
		} else {
			anyNA = true
		}
	}
	if allNA {
		return features.Primitive(features.Float64)
	}
	isInt, isFloat, isBool := true, true, true
	unsigned := false
	for i, s := range cells {
		if !present[i] || o.na[s] {
			continue
		}
		if isInt {
			if !intRe.MatchString(s) {
				isInt = false
			} else if _, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64); err != nil {
				if _, err := strconv.ParseUint(strings.TrimPrefix(strings.TrimSpace(s), "+"), 10, 64); err == nil {
					unsigned = true
				} else {
					isInt = false
				}
			}
		}
		if isFloat {
			if _, err := parsePyFloat(s); err != nil || strings.TrimSpace(s) == "" {
				isFloat = false
			}
		}
		if isBool && !o.trueValues[s] && !o.falseValues[s] {
			isBool = false
		}
	}
	switch {
	case isInt && !anyNA && unsigned:
		return features.Primitive(features.Uint64)
	case isInt && !anyNA:
		return features.Primitive(features.Int64)
	case isInt || isFloat:
		return features.Primitive(features.Float64)
	case isBool:
		return features.Primitive(features.Bool)
	}
	return features.Primitive(features.String)
}

// csvValue converts a cell to a value of type a, the way pandas parsed it.
func csvValue(s string, present bool, a *features.Arrow, o *csvOptions) (any, error) {
	if !present || o.na[s] {
		return nil, nil
	}
	switch a.ID {
	case features.Bool:
		if o.trueValues[s] {
			return true, nil
		}
		if o.falseValues[s] {
			return false, nil
		}
	case features.Int64:
		if i, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64); err == nil {
			return i, nil
		}
	case features.Uint64:
		if u, err := strconv.ParseUint(strings.TrimPrefix(strings.TrimSpace(s), "+"), 10, 64); err == nil {
			return u, nil
		}
	case features.Float64:
		if f, err := parsePyFloat(s); err == nil {
			return f, nil
		}
	case features.String:
		return s, nil
	}
	// The chunk's own type differs from the first chunk's: let the cast decide.
	return Cast(s, a)
}

type csvBuilder struct{}

func (csvBuilder) name() string { return "csv" }

// chunkTypes reads the types of a chunk's columns.
func chunkTypes(rows [][]string, names []string, o *csvOptions) []*features.Arrow {
	types := make([]*features.Arrow, len(names))
	cells := make([]string, len(rows))
	present := make([]bool, len(rows))
	for c := range names {
		for i, r := range rows {
			if c < len(r) {
				cells[i], present[i] = r[c], true
			} else {
				cells[i], present[i] = "", false
			}
		}
		types[c] = csvColumnType(cells, present, o)
	}
	return types
}

func (b csvBuilder) infer(e *env, splits []splitFiles) (features.Features, error) {
	o, err := parseCSVOptions(e.opts)
	if err != nil {
		return nil, err
	}
	if len(splits) == 0 {
		return nil, errors.New("no CSV files")
	}
	for _, f := range splits[0].files {
		ms, err := members(e.ctx, f)
		if err != nil {
			return nil, err
		}
		for _, m := range ms {
			rc, err := m.open()
			if err != nil {
				return nil, err
			}
			cr, err := newCSVReader(rc, o)
			if err != nil {
				_ = rc.Close()
				return nil, err
			}
			rows, err := cr.next()
			_ = rc.Close()
			if errors.Is(err, io.EOF) {
				if cr.names != nil {
					var fs features.Features
					for _, n := range cr.names {
						fs = append(fs, features.Field{Name: n, Feature: features.Value("float64")})
					}
					return fs, nil
				}
				continue
			}
			if err != nil {
				return nil, err
			}
			types := chunkTypes(rows, cr.names, o)
			var fs features.Features
			for i, n := range cr.names {
				fs = append(fs, features.Field{Name: n, Feature: features.FromArrow(types[i])})
			}
			return fs, nil
		}
	}
	return nil, errors.New("the CSV files are empty")
}

func (b csvBuilder) read(e *env, split splitFiles, fs features.Features, emit emitFunc) error {
	o, err := parseCSVOptions(e.opts)
	if err != nil {
		return err
	}
	st, err := fs.StorageSchema()
	if err != nil {
		return err
	}
	for _, f := range split.files {
		ms, err := members(e.ctx, f)
		if err != nil {
			return err
		}
		for _, m := range ms {
			if err := b.readMember(m, o, fs, st, emit); err != nil {
				return fmt.Errorf("%s: %w", f.Path, err)
			}
		}
	}
	return nil
}

func (b csvBuilder) readMember(m member, o *csvOptions, fs features.Features, st *features.Arrow, emit emitFunc) error {
	rc, err := m.open()
	if err != nil {
		return err
	}
	defer rc.Close()
	cr, err := newCSVReader(rc, o)
	if err != nil {
		return err
	}
	for {
		rows, err := cr.next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		idx := make([]int, len(fs))
		for i, fl := range fs {
			idx[i] = -1
			for j, n := range cr.names {
				if n == fl.Name {
					idx[i] = j
				}
			}
			if idx[i] < 0 {
				return fmt.Errorf("couldn't cast the CSV columns %v to the features: column %q is missing", cr.names, fl.Name)
			}
		}
		if len(cr.names) != len(fs) {
			return fmt.Errorf("couldn't cast the CSV columns %v to the features %v", cr.names, fs.Names())
		}
		// Parse the chunk with its own inferred types, as pandas does,
		// then cast to the features.
		types := chunkTypes(rows, cr.names, o)
		out := make([][]any, len(rows))
		for r, rec := range rows {
			row := make([]any, len(fs))
			for i := range fs {
				c := idx[i]
				cell, present := "", c < len(rec)
				if present {
					cell = rec[c]
				}
				v, err := csvValue(cell, present, types[c], o)
				if err != nil {
					return fmt.Errorf("column %q: %w", fs[i].Name, err)
				}
				v, err = Cast(v, st.Fields[i].Type)
				if err != nil {
					return fmt.Errorf("column %q: %w", fs[i].Name, err)
				}
				row[i] = v
			}
			out[r] = row
		}
		if err := emit(out); err != nil {
			return err
		}
	}
}
