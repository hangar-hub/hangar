package convert

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/hangar-hub/hangar/internal/datasets/features"
)

// ErrCast means a value cannot be stored as the column's type.
var ErrCast = errors.New("value does not fit the column type")

// itype is a type inferred from decoded JSON-like values, as pyarrow
// infers types when it builds a table from Python objects or JSON. json
// marks a path whose values have conflicting types, which `datasets` 4
// and later store as the Json feature.
type itype struct {
	id     features.TypeID
	elem   *itype
	fields []ifield
	json   bool
}

type ifield struct {
	name string
	typ  *itype
}

func (t *itype) field(name string) int {
	for i, f := range t.fields {
		if f.name == name {
			return i
		}
	}
	return -1
}

// orderedObject is a JSON object with its keys in document order.
type orderedObject struct {
	keys []string
	vals []any
}

func (o *orderedObject) get(k string) (any, bool) {
	for i, kk := range o.keys {
		if kk == k {
			return o.vals[i], true
		}
	}
	return nil, false
}

// isoTimestampRe is the subset of ISO 8601 that pyarrow's JSON reader
// infers as timestamp[s]: a date, optionally with hours, minutes and
// seconds (no fraction) and a Z or numeric offset.
var isoTimestampRe = regexp.MustCompile(`^(\d{4})-(\d{2})-(\d{2})(?:[T ](\d{2})(?::(\d{2})(?::(\d{2}))?)?(Z|[+-]\d{2}(?::?\d{2})?)?)?$`)

// parseISOTimestamp parses the strings pyarrow infers as timestamps,
// returning the time in UTC.
func parseISOTimestamp(s string) (time.Time, bool) {
	m := isoTimestampRe.FindStringSubmatch(s)
	if m == nil {
		return time.Time{}, false
	}
	num := func(x string) int {
		n, _ := strconv.Atoi(x)
		return n
	}
	y, mo, d := num(m[1]), num(m[2]), num(m[3])
	h, mi, se := num(m[4]), num(m[5]), num(m[6])
	if mo < 1 || mo > 12 || d < 1 || d > 31 || h > 23 || mi > 59 || se > 59 {
		return time.Time{}, false
	}
	t := time.Date(y, time.Month(mo), d, h, mi, se, 0, time.UTC)
	if t.Day() != d {
		return time.Time{}, false
	}
	if tz := m[7]; tz != "" && tz != "Z" {
		sign := 1
		if tz[0] == '-' {
			sign = -1
		}
		digits := strings.ReplaceAll(tz[1:], ":", "")
		oh := num(digits[:2])
		om := 0
		if len(digits) >= 4 {
			om = num(digits[2:4])
		}
		if oh > 23 || om > 59 {
			return time.Time{}, false
		}
		t = t.Add(-time.Duration(sign) * (time.Duration(oh)*time.Hour + time.Duration(om)*time.Minute))
	}
	return t, true
}

// inferJSON returns the type pyarrow's JSON reader gives a value.
func inferJSON(v any, timestamps bool, depth int) *itype {
	if depth > 128 {
		return &itype{json: true}
	}
	switch x := v.(type) {
	case nil:
		return &itype{id: features.Null}
	case bool:
		return &itype{id: features.Bool}
	case int64:
		return &itype{id: features.Int64}
	case float64:
		return &itype{id: features.Float64}
	case json.Number:
		if isIntLiteral(string(x)) {
			if _, err := strconv.ParseInt(string(x), 10, 64); err == nil {
				return &itype{id: features.Int64}
			}
		}
		return &itype{id: features.Float64}
	case string:
		if timestamps {
			if _, ok := parseISOTimestamp(x); ok {
				return &itype{id: features.Timestamp}
			}
		}
		return &itype{id: features.String}
	case []any:
		t := &itype{id: features.List, elem: &itype{id: features.Null}}
		for _, e := range x {
			t.elem = merge(t.elem, inferJSON(e, timestamps, depth+1))
		}
		return t
	case *orderedObject:
		t := &itype{id: features.Struct}
		for i, k := range x.keys {
			t.fields = append(t.fields, ifield{name: k, typ: inferJSON(x.vals[i], timestamps, depth+1)})
		}
		return t
	}
	return &itype{json: true}
}

func isIntLiteral(s string) bool {
	return !strings.ContainsAny(s, ".eE")
}

// merge combines the types of two values at the same path, promoting
// integers to floats and timestamps to strings as pyarrow does, and
// marking anything else that disagrees as JSON.
func merge(a, b *itype) *itype {
	switch {
	case a.json || b.json:
		return &itype{json: true}
	case a.id == features.Null:
		return b
	case b.id == features.Null:
		return a
	}
	if a.id == b.id {
		switch a.id {
		case features.List:
			return &itype{id: features.List, elem: merge(a.elem, b.elem)}
		case features.Struct:
			out := &itype{id: features.Struct}
			out.fields = append(out.fields, a.fields...)
			for _, f := range b.fields {
				if i := out.field(f.name); i >= 0 {
					out.fields[i] = ifield{name: f.name, typ: merge(out.fields[i].typ, f.typ)}
				} else {
					out.fields = append(out.fields, f)
				}
			}
			return out
		}
		return a
	}
	num := func(t *itype) bool { return t.id == features.Int64 || t.id == features.Float64 }
	if num(a) && num(b) {
		return &itype{id: features.Float64}
	}
	str := func(t *itype) bool { return t.id == features.String || t.id == features.Timestamp }
	if str(a) && str(b) {
		return &itype{id: features.String}
	}
	return &itype{json: true}
}

// mixedStructPaths is datasets.utils.json.find_mixed_struct_types_field_paths:
// paths below the root where objects have different keys, or are empty,
// or where objects or lists are mixed with other values. Path elements are
// field names, or "" for list items.
func mixedStructPaths(examples []any) [][]string {
	var out [][]string
	type work struct {
		path    []string
		content []any
	}
	var nonNil []any
	for _, e := range examples {
		if e != nil {
			nonNil = append(nonNil, e)
		}
	}
	if len(nonNil) == 0 {
		return nil
	}
	queue := []work{{path: nil, content: nonNil}}
	for steps := 0; len(queue) > 0 && steps < 100_000; steps++ {
		w := queue[0]
		queue = queue[1:]
		allObj, allList, anyNested := true, true, false
		for _, x := range w.content {
			_, isObj := x.(*orderedObject)
			_, isList := x.([]any)
			allObj = allObj && isObj
			allList = allList && isList
			anyNested = anyNested || isObj || isList
		}
		switch {
		case allObj:
			first := w.content[0].(*orderedObject)
			differ := len(first.keys) == 0
			for _, x := range w.content[1:] {
				if !sameKeys(first, x.(*orderedObject)) {
					differ = true
					break
				}
			}
			if len(w.path) > 0 && differ {
				out = append(out, w.path)
				continue
			}
			var names []string
			seen := map[string]bool{}
			for _, x := range w.content {
				for _, k := range x.(*orderedObject).keys {
					if !seen[k] {
						seen[k] = true
						names = append(names, k)
					}
				}
			}
			for _, k := range names {
				var sub []any
				for _, x := range w.content {
					if v, ok := x.(*orderedObject).get(k); ok && v != nil {
						sub = append(sub, v)
					}
				}
				if len(sub) > 0 {
					queue = append(queue, work{path: append(append([]string{}, w.path...), k), content: sub})
				}
			}
		case allList:
			var sub []any
			for _, x := range w.content {
				for _, e := range x.([]any) {
					if e != nil {
						sub = append(sub, e)
					}
				}
			}
			if len(sub) > 0 {
				queue = append(queue, work{path: append(append([]string{}, w.path...), ""), content: sub})
			}
		case anyNested:
			out = append(out, w.path)
		}
	}
	return out
}

func sameKeys(a, b *orderedObject) bool {
	if len(a.keys) != len(b.keys) {
		return false
	}
	set := make(map[string]bool, len(a.keys))
	for _, k := range a.keys {
		set[k] = true
	}
	for _, k := range b.keys {
		if !set[k] {
			return false
		}
	}
	return true
}

// markJSON sets the json flag on the type at each path.
func markJSON(t *itype, path []string) {
	for len(path) > 0 && t != nil {
		p := path[0]
		path = path[1:]
		if p == "" {
			if t.elem == nil {
				return
			}
			if len(path) == 0 {
				t.elem = &itype{json: true}
				return
			}
			t = t.elem
			continue
		}
		i := t.field(p)
		if i < 0 {
			return
		}
		if len(path) == 0 {
			t.fields[i].typ = &itype{json: true}
			return
		}
		t = t.fields[i].typ
	}
}

// toFeature turns an inferred type into the feature `datasets` reports.
func (t *itype) toFeature() *features.Feature {
	switch {
	case t.json:
		return &features.Feature{Kind: features.KindJSON, Length: -1}
	case t.id == features.List:
		return features.ListOfFeature(t.elem.toFeature())
	case t.id == features.Struct:
		st := &features.Feature{Kind: features.KindStruct}
		for _, f := range t.fields {
			st.Fields = append(st.Fields, features.Field{Name: f.name, Feature: f.typ.toFeature()})
		}
		return st
	case t.id == features.Timestamp:
		return features.Value("timestamp[s]")
	}
	return features.Value(features.Primitive(t.id).DType())
}

// pandasJSON encodes a value the way pandas' ujson_dumps does, which
// `datasets` uses for Json features: compact, ASCII only, with escaped
// forward slashes.
func pandasJSON(v any) string {
	var b strings.Builder
	writePandasJSON(&b, v, 0)
	return b.String()
}

func writePandasJSON(b *strings.Builder, v any, depth int) {
	if depth > 256 {
		b.WriteString("null")
		return
	}
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		b.WriteString(strconv.FormatBool(x))
	case json.Number:
		v := numberValue(x)
		if i, ok := v.(int64); ok {
			b.WriteString(strconv.FormatInt(i, 10))
		} else if isIntLiteral(string(x)) {
			b.WriteString(string(x))
		} else {
			b.WriteString(ujsonFloat(v.(float64)))
		}
	case int64:
		b.WriteString(strconv.FormatInt(x, 10))
	case float64:
		b.WriteString(ujsonFloat(x))
	case string:
		b.WriteByte('"')
		for _, r := range x {
			switch r {
			case '"':
				b.WriteString(`\"`)
			case '\\':
				b.WriteString(`\\`)
			case '/':
				b.WriteString(`\/`)
			case '\n':
				b.WriteString(`\n`)
			case '\r':
				b.WriteString(`\r`)
			case '\t':
				b.WriteString(`\t`)
			case '\b':
				b.WriteString(`\b`)
			case '\f':
				b.WriteString(`\f`)
			default:
				switch {
				case r < 0x20 || (r >= 0x80 && r < 0x10000):
					fmt.Fprintf(b, `\u%04x`, r)
				case r >= 0x10000:
					r1, r2 := utf16Pair(r)
					fmt.Fprintf(b, `\u%04x\u%04x`, r1, r2)
				default:
					b.WriteRune(r)
				}
			}
		}
		b.WriteByte('"')
	case []any:
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			writePandasJSON(b, e, depth+1)
		}
		b.WriteByte(']')
	case *orderedObject:
		b.WriteByte('{')
		for i, k := range x.keys {
			if i > 0 {
				b.WriteByte(',')
			}
			writePandasJSON(b, k, depth+1)
			b.WriteByte(':')
			writePandasJSON(b, x.vals[i], depth+1)
		}
		b.WriteByte('}')
	default:
		writePandasJSON(b, fmt.Sprint(x), depth+1)
	}
}

func utf16Pair(r rune) (rune, rune) {
	r -= 0x10000
	return 0xd800 + (r>>10)&0x3ff, 0xdc00 + r&0x3ff
}

// ujsonFloat formats a float as pandas' ujson_dumps does with its
// default precision of 10 decimal places.
func ujsonFloat(f float64) string {
	switch {
	case math.IsNaN(f) || math.IsInf(f, 0):
		return "null"
	case f == 0:
		return "0.0"
	case math.Abs(f) < 1e-10:
		if f < 0 {
			return "-0.0"
		}
		return "0.0"
	case math.Abs(f) > 1e16:
		s := strconv.FormatFloat(f, 'e', 9, 64)
		mant, exp, _ := strings.Cut(s, "e")
		if strings.Contains(mant, ".") {
			mant = strings.TrimRight(strings.TrimRight(mant, "0"), ".")
		}
		sign := exp[0]
		digits := strings.TrimLeft(exp[1:], "0")
		return mant + "e" + string(sign) + digits
	}
	s := strconv.FormatFloat(f, 'f', 10, 64)
	s = strings.TrimRight(s, "0")
	if strings.HasSuffix(s, ".") {
		s += "0"
	}
	return s
}

// numberValue converts a JSON number to int64 when it is an integer that
// fits, otherwise to float64.
func numberValue(n json.Number) any {
	s := string(n)
	if isIntLiteral(s) {
		if i, err := strconv.ParseInt(s, 10, 64); err == nil {
			return i
		}
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		// Too large for a float64: keep the closest value.
		bf, _, perr := big.ParseFloat(s, 10, 64, big.ToNearestEven)
		if perr != nil {
			return math.NaN()
		}
		f, _ = bf.Float64()
	}
	return f
}

// Cast converts a value to the storage type a, as pyarrow's safe casts do
// when `datasets` casts a table to its features.
func Cast(v any, a *features.Arrow) (any, error) {
	return cast(v, a, 0)
}

// CastFeature is Cast with the Features that decide more than the
// storage type: a ClassLabel takes its label names as strings and
// converts them to ids (ClassLabel.cast_storage), and refuses ids outside
// [-1, number of names).
func CastFeature(v any, f *features.Feature, a *features.Arrow) (any, error) {
	return castFeature(v, f, a, 0)
}

func castFeature(v any, f *features.Feature, a *features.Arrow, depth int) (any, error) {
	if v == nil || f == nil {
		return cast(v, a, depth)
	}
	if depth > 128 {
		return nil, fmt.Errorf("%w: nested too deeply", ErrCast)
	}
	switch {
	case f.Kind == features.KindClassLabel:
		if s, ok := v.(string); ok {
			for i, n := range f.Names {
				if n == s {
					return int64(i), nil
				}
			}
			return nil, fmt.Errorf("%w: invalid string class label %s", ErrCast, s)
		}
		c, err := cast(v, a, depth)
		if err != nil {
			return nil, err
		}
		if id, ok := c.(int64); ok && len(f.Names) > 0 && (id < -1 || id >= int64(len(f.Names))) {
			return nil, fmt.Errorf("%w: class label %d is outside the %d configured names", ErrCast, id, len(f.Names))
		}
		return c, nil
	case f.IsListKind() && f.Elem != nil && a.Elem != nil:
		l, ok := v.([]any)
		if !ok {
			break
		}
		if a.ID == features.FixedSizeList && len(l) != a.Size {
			return nil, fmt.Errorf("%w: a list of %d values for %s", ErrCast, len(l), a)
		}
		out := make([]any, len(l))
		for i, e := range l {
			c, err := castFeature(e, f.Elem, a.Elem, depth+1)
			if err != nil {
				return nil, err
			}
			out[i] = c
		}
		return out, nil
	case f.Kind == features.KindStruct && a.ID == features.Struct && len(f.Fields) == len(a.Fields):
		out := make([]any, len(a.Fields))
		switch x := v.(type) {
		case *orderedObject:
			for i, fl := range a.Fields {
				fv, _ := x.get(fl.Name)
				c, err := castFeature(fv, f.Fields[i].Feature, fl.Type, depth+1)
				if err != nil {
					return nil, fmt.Errorf("%s: %w", fl.Name, err)
				}
				out[i] = c
			}
			return out, nil
		case []any:
			if len(x) != len(a.Fields) {
				break
			}
			for i, fl := range a.Fields {
				c, err := castFeature(x[i], f.Fields[i].Feature, fl.Type, depth+1)
				if err != nil {
					return nil, fmt.Errorf("%s: %w", fl.Name, err)
				}
				out[i] = c
			}
			return out, nil
		}
	}
	return cast(v, a, depth)
}

func cast(v any, a *features.Arrow, depth int) (any, error) {
	if v == nil {
		return nil, nil
	}
	if depth > 128 {
		return nil, fmt.Errorf("%w: nested too deeply", ErrCast)
	}
	if n, ok := v.(json.Number); ok {
		v = numberValue(n)
	}
	bad := func() (any, error) {
		return nil, fmt.Errorf("%w: cannot store %s as %s", ErrCast, describe(v), a)
	}
	switch a.ID {
	case features.Null:
		return bad()
	case features.Bool:
		switch x := v.(type) {
		case bool:
			return x, nil
		case int64:
			return x != 0, nil
		case string:
			switch strings.ToLower(x) {
			case "true", "1":
				return true, nil
			case "false", "0":
				return false, nil
			}
		}
		return bad()
	case features.Int8, features.Int16, features.Int32, features.Int64, features.Uint8, features.Uint16, features.Uint32, features.Uint64:
		var i int64
		switch x := v.(type) {
		case int64:
			i = x
		case uint64:
			if a.ID == features.Uint64 {
				return x, nil
			}
			if x > math.MaxInt64 {
				return bad()
			}
			i = int64(x)
		case bool:
			if x {
				i = 1
			}
		case float64:
			if x != math.Trunc(x) || math.IsInf(x, 0) || math.IsNaN(x) || math.Abs(x) > 9.2e18 {
				return bad()
			}
			i = int64(x)
		case string:
			p, err := strconv.ParseInt(strings.TrimSpace(x), 10, 64)
			if err != nil {
				if a.ID == features.Uint64 {
					if u, err := strconv.ParseUint(strings.TrimSpace(x), 10, 64); err == nil {
						return u, nil
					}
				}
				return bad()
			}
			i = p
		default:
			return bad()
		}
		lo, hi := intRange(a.ID)
		if i < lo || (hi >= 0 && i > hi) {
			return bad()
		}
		if a.ID == features.Uint64 {
			if i < 0 {
				return bad()
			}
			return uint64(i), nil
		}
		return i, nil
	case features.Float16, features.Float32, features.Float64:
		var f float64
		switch x := v.(type) {
		case float64:
			f = x
		case int64:
			f = float64(x)
		case uint64:
			f = float64(x)
		case bool:
			if x {
				f = 1
			}
		case string:
			p, err := parsePyFloat(x)
			if err != nil {
				return bad()
			}
			f = p
		default:
			return bad()
		}
		if a.ID == features.Float32 || a.ID == features.Float16 {
			f = float64(float32(f))
		}
		return f, nil
	case features.String, features.LargeString:
		switch x := v.(type) {
		case string:
			return x, nil
		case int64:
			return strconv.FormatInt(x, 10), nil
		case uint64:
			return strconv.FormatUint(x, 10), nil
		case float64:
			return arrowFloatString(x), nil
		case bool:
			return strconv.FormatBool(x), nil
		case []byte:
			if utf8.Valid(x) {
				return string(x), nil
			}
		case time.Time:
			return x.Format("2006-01-02 15:04:05"), nil
		}
		return bad()
	case features.Binary, features.LargeBinary:
		switch x := v.(type) {
		case []byte:
			return x, nil
		case string:
			return []byte(x), nil
		}
		return bad()
	case features.Timestamp, features.Date32, features.Date64:
		var t time.Time
		switch x := v.(type) {
		case time.Time:
			t = x
		case string:
			p, ok := parseISOTimestamp(x)
			if !ok {
				pt, err := time.Parse(time.RFC3339Nano, x)
				if err != nil {
					return bad()
				}
				p = pt.UTC()
			}
			t = p
		case int64:
			switch a.ID {
			case features.Date32:
				t = features.DaysToTime(x)
			case features.Date64:
				t = features.TimeFromUnits(x, "ms")
			default:
				t = features.TimeFromUnits(x, a.Unit)
			}
		default:
			return bad()
		}
		if a.ID != features.Timestamp {
			t = features.DaysToTime(features.TimeToDays(t))
		} else {
			t = features.TimeFromUnits(features.TimeToUnits(t, a.Unit), a.Unit)
		}
		return t, nil
	case features.Time32, features.Time64, features.Duration:
		switch x := v.(type) {
		case int64:
			return x, nil
		case float64:
			if x == math.Trunc(x) {
				return int64(x), nil
			}
		}
		return bad()
	case features.Decimal128:
		switch x := v.(type) {
		case string:
			if _, ok := new(big.Float).SetString(x); ok {
				return x, nil
			}
		case int64:
			return strconv.FormatInt(x, 10), nil
		case float64:
			return strconv.FormatFloat(x, 'f', a.Scale, 64), nil
		}
		return bad()
	case features.List, features.LargeList, features.FixedSizeList:
		l, ok := v.([]any)
		if !ok {
			return bad()
		}
		if a.ID == features.FixedSizeList && len(l) != a.Size {
			return nil, fmt.Errorf("%w: a list of %d values for %s", ErrCast, len(l), a)
		}
		out := make([]any, len(l))
		for i, e := range l {
			c, err := cast(e, a.Elem, depth+1)
			if err != nil {
				return nil, err
			}
			out[i] = c
		}
		return out, nil
	case features.Struct:
		out := make([]any, len(a.Fields))
		switch x := v.(type) {
		case *orderedObject:
			for i, f := range a.Fields {
				fv, _ := x.get(f.Name)
				c, err := cast(fv, f.Type, depth+1)
				if err != nil {
					return nil, fmt.Errorf("%s: %w", f.Name, err)
				}
				out[i] = c
			}
			return out, nil
		case []any:
			if len(x) != len(a.Fields) {
				return bad()
			}
			for i, f := range a.Fields {
				c, err := cast(x[i], f.Type, depth+1)
				if err != nil {
					return nil, fmt.Errorf("%s: %w", f.Name, err)
				}
				out[i] = c
			}
			return out, nil
		}
		return bad()
	}
	return bad()
}

func intRange(id features.TypeID) (int64, int64) {
	switch id {
	case features.Int8:
		return math.MinInt8, math.MaxInt8
	case features.Int16:
		return math.MinInt16, math.MaxInt16
	case features.Int32:
		return math.MinInt32, math.MaxInt32
	case features.Uint8:
		return 0, math.MaxUint8
	case features.Uint16:
		return 0, math.MaxUint16
	case features.Uint32:
		return 0, math.MaxUint32
	case features.Uint64:
		return 0, -1
	}
	return math.MinInt64, math.MaxInt64
}

// arrowFloatString formats a float as pyarrow casts it to a string.
func arrowFloatString(f float64) string {
	switch {
	case math.IsNaN(f):
		return "nan"
	case math.IsInf(f, 1):
		return "inf"
	case math.IsInf(f, -1):
		return "-inf"
	case f == 0:
		if math.Signbit(f) {
			return "-0"
		}
		return "0"
	}
	// Arrow's shortest form: decimal notation for exponents from -6 to 9,
	// otherwise 1.5e-7 or 1e+10.
	sci := strconv.FormatFloat(f, 'e', -1, 64)
	mant, expText, _ := strings.Cut(sci, "e")
	exp, _ := strconv.Atoi(expText)
	if exp < -6 || exp > 9 {
		sign := "+"
		if exp < 0 {
			sign, exp = "-", -exp
		}
		return mant + "e" + sign + strconv.Itoa(exp)
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// parsePyFloat parses a float the way Python's float() does.
func parsePyFloat(s string) (float64, error) {
	t := strings.TrimSpace(strings.ReplaceAll(s, "_", ""))
	switch strings.ToLower(strings.TrimLeft(t, "+-")) {
	case "inf", "infinity", "nan":
		f, err := strconv.ParseFloat(t, 64)
		return f, err
	}
	return strconv.ParseFloat(t, 64)
}

func describe(v any) string {
	switch x := v.(type) {
	case string:
		if len(x) > 40 {
			x = x[:40] + "..."
		}
		return strconv.Quote(x)
	case *orderedObject:
		return "an object"
	case []any:
		return "a list"
	}
	return fmt.Sprintf("%v", v)
}
