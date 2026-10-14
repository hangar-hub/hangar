package query

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jamesagarside/hangar/internal/datasets/features"
)

// ErrType means a clause compares a column with a value of another type,
// or orders by a column that has no order.
var ErrType = errors.New("type error")

// Column is a column of the table a clause runs on.
type Column struct {
	Name string
	Type *features.Arrow
}

// Plan is a compiled where clause and orderby list.
type Plan struct {
	// Columns are the top-level columns the plan reads, in the order the
	// values passed to Match and Key are given.
	Columns []string
	match   func(vals []any) tri
	keys    []keyFn
	// Bounds lets row groups be skipped: it reports whether any row with
	// a column between min and max can match.
	bounds func(stats map[string][2]any) bool
}

type tri int8

const (
	triFalse tri = iota
	triTrue
	triUnknown
)

func (t tri) not() tri {
	switch t {
	case triTrue:
		return triFalse
	case triFalse:
		return triTrue
	}
	return triUnknown
}

type keyFn struct {
	get  func(vals []any) any
	cmp  func(a, b any) int
	desc bool
}

// Match reports whether a row matches the where clause. vals holds the
// values of Plan.Columns.
func (p *Plan) Match(vals []any) bool {
	if p.match == nil {
		return true
	}
	return p.match(vals) == triTrue
}

// Ordered reports whether the plan has an orderby list.
func (p *Plan) Ordered() bool { return len(p.keys) > 0 }

// Key returns a row's sort key.
func (p *Plan) Key(vals []any) []any {
	out := make([]any, len(p.keys))
	for i, k := range p.keys {
		out[i] = k.get(vals)
	}
	return out
}

// Less orders two sort keys, nulls last in both directions as DuckDB does.
func (p *Plan) Less(a, b []any) bool {
	for i, k := range p.keys {
		x, y := a[i], b[i]
		switch {
		case x == nil && y == nil:
			continue
		case x == nil:
			return false
		case y == nil:
			return true
		}
		c := k.cmp(x, y)
		if k.desc {
			c = -c
		}
		if c != 0 {
			return c < 0
		}
	}
	return false
}

// MayMatch reports whether a row group whose columns span the given
// [min, max] ranges can hold matching rows. A false answer is certain.
func (p *Plan) MayMatch(stats map[string][2]any) bool {
	if p.bounds == nil {
		return true
	}
	return p.bounds(stats)
}

type compiler struct {
	cols   []Column
	used   []string
	usedIx map[string]int
}

func (c *compiler) resolve(path []string) (func(vals []any) any, *features.Arrow, string, error) {
	var top *Column
	for i := range c.cols {
		if c.cols[i].Name == path[0] {
			top = &c.cols[i]
		}
	}
	if top == nil {
		return nil, nil, "", fmt.Errorf("%w: no column %q", ErrType, path[0])
	}
	ix, ok := c.usedIx[top.Name]
	if !ok {
		ix = len(c.used)
		c.used = append(c.used, top.Name)
		c.usedIx[top.Name] = ix
	}
	t := top.Type
	var fieldIx []int
	for _, f := range path[1:] {
		if t.ID != features.Struct {
			return nil, nil, "", fmt.Errorf("%w: %s has no field %q", ErrType, quoteColumn(path), f)
		}
		k := t.Field(f)
		if k < 0 {
			return nil, nil, "", fmt.Errorf("%w: %s has no field %q", ErrType, quoteColumn(path), f)
		}
		fieldIx = append(fieldIx, k)
		t = t.Fields[k].Type
	}
	get := func(vals []any) any {
		v := vals[ix]
		for _, k := range fieldIx {
			s, ok := v.([]any)
			if !ok || k >= len(s) {
				return nil
			}
			v = s[k]
		}
		return v
	}
	return get, t, top.Name, nil
}

// Compile compiles a where clause and orderby list against columns.
func Compile(where Expr, order []Order, cols []Column) (*Plan, error) {
	c := &compiler{cols: cols, usedIx: map[string]int{}}
	p := &Plan{}
	if where != nil {
		m, err := c.expr(where, 0)
		if err != nil {
			return nil, err
		}
		p.match = m
		b, err := c.bounds(where, 0)
		if err != nil {
			return nil, err
		}
		p.bounds = b
	}
	for _, o := range order {
		get, t, _, err := c.resolve(o.Column)
		if err != nil {
			return nil, err
		}
		cmp, err := comparator(t)
		if err != nil {
			return nil, fmt.Errorf("%w: cannot order by %s, a %s", ErrType, quoteColumn(o.Column), t)
		}
		p.keys = append(p.keys, keyFn{get: get, cmp: cmp, desc: o.Desc})
	}
	p.Columns = c.used
	return p, nil
}

func (c *compiler) expr(e Expr, depth int) (func([]any) tri, error) {
	if depth > maxDepth*4 {
		return nil, fmt.Errorf("%w: the clause is nested too deeply", ErrSyntax)
	}
	switch x := e.(type) {
	case *And:
		l, err := c.expr(x.Left, depth+1)
		if err != nil {
			return nil, err
		}
		r, err := c.expr(x.Right, depth+1)
		if err != nil {
			return nil, err
		}
		return func(v []any) tri {
			a := l(v)
			if a == triFalse {
				return triFalse
			}
			b := r(v)
			switch {
			case b == triFalse:
				return triFalse
			case a == triTrue && b == triTrue:
				return triTrue
			}
			return triUnknown
		}, nil
	case *Or:
		l, err := c.expr(x.Left, depth+1)
		if err != nil {
			return nil, err
		}
		r, err := c.expr(x.Right, depth+1)
		if err != nil {
			return nil, err
		}
		return func(v []any) tri {
			a := l(v)
			if a == triTrue {
				return triTrue
			}
			b := r(v)
			switch {
			case b == triTrue:
				return triTrue
			case a == triFalse && b == triFalse:
				return triFalse
			}
			return triUnknown
		}, nil
	case *Not:
		f, err := c.expr(x.X, depth+1)
		if err != nil {
			return nil, err
		}
		return func(v []any) tri { return f(v).not() }, nil
	case *Pred:
		return c.pred(x)
	}
	return nil, fmt.Errorf("%w: unknown expression", ErrSyntax)
}

func (c *compiler) pred(p *Pred) (func([]any) tri, error) {
	get, t, _, err := c.resolve(p.Column)
	if err != nil {
		return nil, err
	}
	if p.Op == OpNull {
		return func(v []any) tri {
			isNull := get(v) == nil
			if isNull != p.Negate {
				return triTrue
			}
			return triFalse
		}, nil
	}
	if p.Op == OpLike || p.Op == OpILike {
		re, err := likeRegexp(p.Values[0].Text, p.Op == OpILike)
		if err != nil {
			return nil, err
		}
		toStr, err := stringer(t)
		if err != nil {
			return nil, fmt.Errorf("%w: %s on %s, a %s", ErrType, p.Op, quoteColumn(p.Column), t)
		}
		return func(v []any) tri {
			x := get(v)
			if x == nil {
				return triUnknown
			}
			if re.MatchString(toStr(x)) != p.Negate {
				return triTrue
			}
			return triFalse
		}, nil
	}
	cmp, err := comparator(t)
	if err != nil {
		return nil, fmt.Errorf("%w: cannot compare %s, a %s", ErrType, quoteColumn(p.Column), t)
	}
	lits := make([]any, len(p.Values))
	hasNull := false
	for i, l := range p.Values {
		if l.Kind == "null" {
			hasNull = true
			continue
		}
		v, err := literalAs(l, t)
		if err != nil {
			return nil, fmt.Errorf("%w: %s: %w", ErrType, quoteColumn(p.Column), err)
		}
		lits[i] = v
	}
	if p.Op == OpIn {
		return func(v []any) tri {
			x := get(v)
			if x == nil {
				return triUnknown
			}
			for _, l := range lits {
				if l != nil && cmp(x, l) == 0 {
					if p.Negate {
						return triFalse
					}
					return triTrue
				}
			}
			if hasNull {
				return triUnknown
			}
			if p.Negate {
				return triTrue
			}
			return triFalse
		}, nil
	}
	lit := lits[0]
	if lit == nil {
		return func([]any) tri { return triUnknown }, nil
	}
	test := opTest(p.Op)
	return func(v []any) tri {
		x := get(v)
		if x == nil {
			return triUnknown
		}
		if test(cmp(x, lit)) {
			return triTrue
		}
		return triFalse
	}, nil
}

func opTest(op Op) func(int) bool {
	switch op {
	case OpEq:
		return func(c int) bool { return c == 0 }
	case OpNe:
		return func(c int) bool { return c != 0 }
	case OpLt:
		return func(c int) bool { return c < 0 }
	case OpLe:
		return func(c int) bool { return c <= 0 }
	case OpGt:
		return func(c int) bool { return c > 0 }
	}
	return func(c int) bool { return c >= 0 }
}

// bounds builds the row group test of a clause: only conjunctions of
// comparisons with literals prune; anything else may match.
func (c *compiler) bounds(e Expr, depth int) (func(map[string][2]any) bool, error) {
	if depth > maxDepth*4 {
		return nil, nil
	}
	switch x := e.(type) {
	case *And:
		l, err := c.bounds(x.Left, depth+1)
		if err != nil {
			return nil, err
		}
		r, err := c.bounds(x.Right, depth+1)
		if err != nil {
			return nil, err
		}
		return func(s map[string][2]any) bool { return (l == nil || l(s)) && (r == nil || r(s)) }, nil
	case *Or:
		l, err := c.bounds(x.Left, depth+1)
		if err != nil {
			return nil, err
		}
		r, err := c.bounds(x.Right, depth+1)
		if err != nil {
			return nil, err
		}
		if l == nil || r == nil {
			return nil, nil
		}
		return func(s map[string][2]any) bool { return l(s) || r(s) }, nil
	case *Pred:
		if len(x.Column) != 1 || x.Negate || len(x.Values) != 1 || x.Values[0].Kind == "null" {
			return nil, nil
		}
		switch x.Op {
		case OpEq, OpLt, OpLe, OpGt, OpGe:
		default:
			return nil, nil
		}
		_, t, name, err := c.resolve(x.Column)
		if err != nil {
			return nil, err
		}
		if !t.IsNumeric() && !t.IsString() {
			return nil, nil
		}
		cmp, _ := comparator(t)
		lit, err := literalAs(x.Values[0], t)
		if err != nil {
			return nil, nil //nolint:nilerr // pred reports the error; bounds only prunes
		}
		op := x.Op
		return func(s map[string][2]any) bool {
			mm, ok := s[name]
			if !ok || mm[0] == nil || mm[1] == nil {
				return true
			}
			lo, hi := mm[0], mm[1]
			switch op {
			case OpEq:
				return cmp(lo, lit) <= 0 && cmp(hi, lit) >= 0
			case OpLt:
				return cmp(lo, lit) < 0
			case OpLe:
				return cmp(lo, lit) <= 0
			case OpGt:
				return cmp(hi, lit) > 0
			}
			return cmp(hi, lit) >= 0
		}, nil
	}
	return nil, nil
}

func likeRegexp(pattern string, fold bool) (*regexp.Regexp, error) {
	var b strings.Builder
	if fold {
		b.WriteString("(?i)")
	}
	b.WriteString("^(?s:")
	for _, r := range pattern {
		switch r {
		case '%':
			b.WriteString(".*")
		case '_':
			b.WriteString(".")
		default:
			b.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	b.WriteString(")$")
	return regexp.Compile(b.String())
}

func numberOf(v any) (float64, bool) {
	switch x := v.(type) {
	case int64:
		return float64(x), true
	case uint64:
		return float64(x), true
	case float64:
		return x, true
	}
	return 0, false
}

func cmpFloat(a, b float64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	case a == b:
		return 0
	}
	// NaN sorts after every number, as in DuckDB.
	switch {
	case math.IsNaN(a) && math.IsNaN(b):
		return 0
	case math.IsNaN(a):
		return 1
	}
	return -1
}

// comparator returns how values of a type compare.
func comparator(t *features.Arrow) (func(a, b any) int, error) {
	switch {
	case t.IsInteger():
		return func(a, b any) int {
			x, xu := a.(uint64)
			y, yu := b.(uint64)
			if xu && yu {
				switch {
				case x < y:
					return -1
				case x > y:
					return 1
				}
				return 0
			}
			if ai, ok := a.(int64); ok {
				if bi, ok := b.(int64); ok {
					switch {
					case ai < bi:
						return -1
					case ai > bi:
						return 1
					}
					return 0
				}
			}
			fa, _ := numberOf(a)
			fb, _ := numberOf(b)
			return cmpFloat(fa, fb)
		}, nil
	case t.IsFloat(), t.ID == features.Decimal128:
		return func(a, b any) int {
			fa, ok := numberOf(a)
			if !ok {
				fa, _ = strconv.ParseFloat(fmt.Sprint(a), 64)
			}
			fb, ok := numberOf(b)
			if !ok {
				fb, _ = strconv.ParseFloat(fmt.Sprint(b), 64)
			}
			return cmpFloat(fa, fb)
		}, nil
	case t.IsString():
		return func(a, b any) int { return strings.Compare(a.(string), b.(string)) }, nil
	case t.IsBinary():
		return func(a, b any) int { return bytes.Compare(a.([]byte), b.([]byte)) }, nil
	case t.ID == features.Bool:
		return func(a, b any) int {
			x, y := a.(bool), b.(bool)
			switch {
			case x == y:
				return 0
			case !x:
				return -1
			}
			return 1
		}, nil
	case t.ID == features.Timestamp, t.ID == features.Date32, t.ID == features.Date64:
		return func(a, b any) int { return a.(time.Time).Compare(b.(time.Time)) }, nil
	case t.ID == features.Time32, t.ID == features.Time64, t.ID == features.Duration:
		return func(a, b any) int {
			x, y := a.(int64), b.(int64)
			switch {
			case x < y:
				return -1
			case x > y:
				return 1
			}
			return 0
		}, nil
	}
	return nil, ErrType
}

func stringer(t *features.Arrow) (func(any) string, error) {
	switch {
	case t.IsString():
		return func(v any) string { return v.(string) }, nil
	case t.IsBinary():
		return func(v any) string { return string(v.([]byte)) }, nil
	case t.IsNumeric(), t.ID == features.Bool, t.ID == features.Decimal128:
		return func(v any) string { return fmt.Sprint(v) }, nil
	case t.ID == features.Timestamp:
		return func(v any) string { return v.(time.Time).Format("2006-01-02 15:04:05") }, nil
	case t.ID == features.Date32, t.ID == features.Date64:
		return func(v any) string { return v.(time.Time).Format("2006-01-02") }, nil
	}
	return nil, ErrType
}

var timeLayouts = []string{"2006-01-02 15:04:05.999999999", "2006-01-02T15:04:05.999999999", time.RFC3339Nano, "2006-01-02 15:04:05Z07:00", "2006-01-02"}

// literalAs converts a literal to a value of a column type.
func literalAs(l Literal, t *features.Arrow) (any, error) {
	switch {
	case t.IsInteger() || t.IsFloat() || t.ID == features.Decimal128:
		var s string
		switch l.Kind {
		case "number", "string":
			s = strings.TrimSpace(l.Text)
		case "bool":
			if l.Bool {
				s = "1"
			} else {
				s = "0"
			}
		}
		if t.IsInteger() {
			if i, err := strconv.ParseInt(s, 10, 64); err == nil {
				return i, nil
			}
			if u, err := strconv.ParseUint(s, 10, 64); err == nil {
				return u, nil
			}
		}
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return nil, fmt.Errorf("%s is not a number", l)
		}
		return f, nil
	case t.IsString():
		switch l.Kind {
		case "bool":
			return strconv.FormatBool(l.Bool), nil
		}
		return l.Text, nil
	case t.IsBinary():
		return []byte(l.Text), nil
	case t.ID == features.Bool:
		switch l.Kind {
		case "bool":
			return l.Bool, nil
		case "string", "number":
			switch strings.ToLower(strings.TrimSpace(l.Text)) {
			case "true", "t", "1", "yes", "y":
				return true, nil
			case "false", "f", "0", "no", "n":
				return false, nil
			}
		}
		return nil, fmt.Errorf("%s is not a boolean", l)
	case t.ID == features.Timestamp || t.ID == features.Date32 || t.ID == features.Date64:
		if l.Kind != "string" {
			return nil, fmt.Errorf("%s is not a date or time; quote it, as '2024-01-31'", l)
		}
		for _, layout := range timeLayouts {
			if tm, err := time.Parse(layout, strings.TrimSpace(l.Text)); err == nil {
				return tm.UTC(), nil
			}
		}
		return nil, fmt.Errorf("%s is not a date or time", l)
	case t.ID == features.Time32 || t.ID == features.Time64 || t.ID == features.Duration:
		i, err := strconv.ParseInt(strings.TrimSpace(l.Text), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%s is not an integer", l)
		}
		return i, nil
	}
	return nil, fmt.Errorf("cannot compare a %s", t)
}
