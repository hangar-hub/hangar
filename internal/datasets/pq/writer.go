// Package pq reads and writes the Parquet files of dataset Repos with
// github.com/parquet-go/parquet-go: it maps Arrow storage types (package
// features) to Parquet schemas the way pyarrow does, shreds and assembles
// nested values with repetition and definition levels, keeps column order,
// and stores the `datasets` Features in the `huggingface` key-value
// metadata, as Parquet exports on the Hub do.
package pq

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"reflect"
	"strings"
	"time"

	"github.com/parquet-go/parquet-go"
	"github.com/parquet-go/parquet-go/compress"
	"github.com/parquet-go/parquet-go/encoding"

	"github.com/hangar-hub/hangar/internal/datasets/features"
)

// ErrUnsupported means a type cannot be stored in Parquet by Hangar.
var ErrUnsupported = errors.New("parquet: unsupported type")

// MetadataKey is the key-value metadata key `datasets` uses for Features.
const MetadataKey = "huggingface"

// orderedGroup is a Parquet group node that keeps its fields in the order
// given, unlike parquet.Group, which sorts them by name. Column order must
// follow the dataset's features.
type orderedGroup struct {
	fields []parquet.Field
}

type orderedField struct {
	parquet.Node
	name string
}

func (f orderedField) Name() string                           { return f.name }
func (f orderedField) Value(base reflect.Value) reflect.Value { return base }

func newGroup(names []string, nodes []parquet.Node) parquet.Node {
	g := &orderedGroup{}
	for i, n := range nodes {
		g.fields = append(g.fields, orderedField{Node: n, name: names[i]})
	}
	return g
}

func (g *orderedGroup) ID() int                     { return 0 }
func (g *orderedGroup) String() string              { return "group" }
func (g *orderedGroup) Type() parquet.Type          { return parquet.Group{}.Type() }
func (g *orderedGroup) Optional() bool              { return false }
func (g *orderedGroup) Repeated() bool              { return false }
func (g *orderedGroup) Required() bool              { return true }
func (g *orderedGroup) Leaf() bool                  { return false }
func (g *orderedGroup) Fields() []parquet.Field     { return g.fields }
func (g *orderedGroup) Encoding() encoding.Encoding { return nil }
func (g *orderedGroup) Compression() compress.Codec { return nil }
func (g *orderedGroup) GoType() reflect.Type        { return reflect.TypeFor[map[string]any]() }

// writeType returns the Arrow type values are written as: Parquet has no
// second-precision timestamps or times, so those become milliseconds, as
// pyarrow writes them.
func writeType(a *features.Arrow) *features.Arrow {
	c := *a
	switch a.ID {
	case features.Timestamp, features.Time32:
		if a.Unit == "s" {
			c.Unit = "ms"
		}
	case features.Date64:
		c.ID = features.Date32
	case features.Float16:
		c.ID = features.Float32
	case features.LargeString:
		c.ID = features.String
	case features.LargeBinary:
		c.ID = features.Binary
	case features.LargeList, features.FixedSizeList:
		c.ID = features.List
		c.Size = 0
	}
	if a.Elem != nil {
		c.Elem = writeType(a.Elem)
	}
	if a.Fields != nil {
		c.Fields = make([]features.ArrowField, len(a.Fields))
		for i, f := range a.Fields {
			c.Fields[i] = features.ArrowField{Name: f.Name, Type: writeType(f.Type)}
		}
	}
	return &c
}

func timeUnit(u string) parquet.TimeUnit {
	switch u {
	case "us":
		return parquet.Microsecond
	case "ns":
		return parquet.Nanosecond
	}
	return parquet.Millisecond
}

// node returns the Parquet node of an Arrow type, made optional.
func node(a *features.Arrow) (parquet.Node, error) {
	var n parquet.Node
	switch a.ID {
	case features.Null:
		n = parquet.Leaf(parquet.NullType)
	case features.Bool:
		n = parquet.Leaf(parquet.BooleanType)
	case features.Int8:
		n = parquet.Int(8)
	case features.Int16:
		n = parquet.Int(16)
	case features.Int32:
		n = parquet.Int(32)
	case features.Int64:
		n = parquet.Int(64)
	case features.Uint8:
		n = parquet.Uint(8)
	case features.Uint16:
		n = parquet.Uint(16)
	case features.Uint32:
		n = parquet.Uint(32)
	case features.Uint64:
		n = parquet.Uint(64)
	case features.Float32:
		n = parquet.Leaf(parquet.FloatType)
	case features.Float64:
		n = parquet.Leaf(parquet.DoubleType)
	case features.String:
		n = parquet.String()
	case features.Binary:
		n = parquet.Leaf(parquet.ByteArrayType)
	case features.Date32:
		n = parquet.Date()
	case features.Timestamp:
		n = parquet.TimestampAdjusted(timeUnit(a.Unit), a.TZ != "")
	case features.Time32, features.Time64:
		n = parquet.Time(timeUnit(a.Unit))
	case features.Duration:
		n = parquet.Int(64)
	case features.Decimal128:
		n = parquet.Decimal(a.Scale, a.Precision, parquet.FixedLenByteArrayType(16))
	case features.List:
		elem, err := node(a.Elem)
		if err != nil {
			return nil, err
		}
		n = parquet.List(elem)
	case features.Struct:
		if len(a.Fields) == 0 {
			return nil, fmt.Errorf("%w: a struct with no fields", ErrUnsupported)
		}
		names := make([]string, len(a.Fields))
		nodes := make([]parquet.Node, len(a.Fields))
		for i, f := range a.Fields {
			c, err := node(f.Type)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", f.Name, err)
			}
			names[i], nodes[i] = f.Name, c
		}
		n = newGroup(names, nodes)
	default:
		return nil, fmt.Errorf("%w: %s", ErrUnsupported, a)
	}
	return parquet.Optional(n), nil
}

// WriterOptions configure a Writer.
type WriterOptions struct {
	// Features are stored in the `huggingface` metadata.
	Features features.Features
	// RowGroupRows is the most rows in one row group; zero means 1,000.
	RowGroupRows int64
}

// Writer writes rows to a Parquet file.
type Writer struct {
	w      *parquet.GenericWriter[any]
	types  []*features.Arrow
	leaves []int
	buf    [][]parquet.Value
}

// NewWriter starts a Parquet file with one column per field of schema, a
// Struct type.
func NewWriter(out io.Writer, schema *features.Arrow, opts WriterOptions) (*Writer, error) {
	if schema.ID != features.Struct {
		return nil, errors.New("parquet: the schema must be a struct")
	}
	wt := writeType(schema)
	root, err := rootNode(wt)
	if err != nil {
		return nil, err
	}
	rows := opts.RowGroupRows
	if rows <= 0 {
		rows = 1000
	}
	options := []parquet.WriterOption{
		parquet.NewSchema("schema", root),
		parquet.Compression(&parquet.Snappy),
		parquet.MaxRowsPerRowGroup(rows),
		parquet.CreatedBy("hangar", "", ""),
	}
	if opts.Features != nil {
		meta, err := HFMetadata(opts.Features)
		if err != nil {
			return nil, err
		}
		options = append(options, parquet.KeyValueMetadata(MetadataKey, meta))
	}
	w := &Writer{w: parquet.NewGenericWriter[any](out, options...)}
	for _, f := range wt.Fields {
		w.types = append(w.types, f.Type)
		w.leaves = append(w.leaves, countLeaves(f.Type))
	}
	return w, nil
}

func rootNode(st *features.Arrow) (parquet.Node, error) {
	names := make([]string, len(st.Fields))
	nodes := make([]parquet.Node, len(st.Fields))
	for i, f := range st.Fields {
		n, err := node(f.Type)
		if err != nil {
			return nil, fmt.Errorf("column %q: %w", f.Name, err)
		}
		names[i], nodes[i] = f.Name, n
	}
	return newGroup(names, nodes), nil
}

// HFMetadata returns the `huggingface` metadata JSON for Features.
func HFMetadata(fs features.Features) (string, error) {
	b, err := json.Marshal(map[string]any{"info": map[string]any{"features": fs}})
	return string(b), err
}

func countLeaves(a *features.Arrow) int {
	switch a.ID {
	case features.List:
		return countLeaves(a.Elem)
	case features.Struct:
		n := 0
		for _, f := range a.Fields {
			n += countLeaves(f.Type)
		}
		return n
	}
	return 1
}

// Write adds rows, each with a value per column.
func (w *Writer) Write(rows [][]any) error {
	out := make([]parquet.Row, 0, len(rows))
	for _, row := range rows {
		if len(row) != len(w.types) {
			return fmt.Errorf("parquet: a row has %d values, the schema %d columns", len(row), len(w.types))
		}
		total := 0
		for _, n := range w.leaves {
			total += n
		}
		if cap(w.buf) < total {
			w.buf = make([][]parquet.Value, total)
		}
		cols := w.buf[:total]
		for i := range cols {
			cols[i] = cols[i][:0]
		}
		leaf := 0
		for i, v := range row {
			s := &shredder{cols: cols[leaf : leaf+w.leaves[i]], base: leaf}
			if err := s.shred(v, w.types[i], 0, 0, 0, 0); err != nil {
				return err
			}
			leaf += w.leaves[i]
		}
		var r parquet.Row
		for _, c := range cols {
			r = append(r, c...)
		}
		out = append(out, r)
	}
	_, err := w.w.WriteRows(out)
	return err
}

// Flush ends the current row group.
func (w *Writer) Flush() error { return w.w.Flush() }

// Close writes the footer.
func (w *Writer) Close() error { return w.w.Close() }

type shredder struct {
	cols [][]parquet.Value
	base int
}

// shred appends the leaf values of v, whose optional node is at
// definition level def when absent, starting at leaf index leaf. rep is the
// repetition level of the first value and depth the number of repeated
// ancestors.
func (s *shredder) shred(v any, a *features.Arrow, leaf, rep, def, depth int) error {
	if v == nil {
		s.nulls(a, leaf, rep, def)
		return nil
	}
	def++
	switch a.ID {
	case features.Struct:
		vals, ok := v.([]any)
		if !ok || len(vals) != len(a.Fields) {
			return fmt.Errorf("parquet: a %s value must have one value per field, got %T", a, v)
		}
		for i, f := range a.Fields {
			if err := s.shred(vals[i], f.Type, leaf, rep, def, depth); err != nil {
				return err
			}
			leaf += countLeaves(f.Type)
		}
		return nil
	case features.List:
		vals, ok := v.([]any)
		if !ok {
			return fmt.Errorf("parquet: a %s value must be a list, got %T", a, v)
		}
		if len(vals) == 0 {
			s.nulls(a.Elem, leaf, rep, def)
			return nil
		}
		for i, e := range vals {
			r := rep
			if i > 0 {
				r = depth + 1
			}
			if err := s.shred(e, a.Elem, leaf, r, def+1, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	pv, err := leafValue(v, a)
	if err != nil {
		return err
	}
	s.cols[leaf] = append(s.cols[leaf], pv.Level(rep, def, s.base+leaf))
	return nil
}

func (s *shredder) nulls(a *features.Arrow, leaf, rep, def int) {
	n := countLeaves(a)
	for i := range n {
		s.cols[leaf+i] = append(s.cols[leaf+i], parquet.NullValue().Level(rep, def, s.base+leaf+i))
	}
}

func leafValue(v any, a *features.Arrow) (parquet.Value, error) {
	bad := func() (parquet.Value, error) {
		return parquet.Value{}, fmt.Errorf("parquet: value %T does not fit %s", v, a)
	}
	switch a.ID {
	case features.Null:
		return parquet.NullValue(), nil
	case features.Bool:
		b, ok := v.(bool)
		if !ok {
			return bad()
		}
		return parquet.BooleanValue(b), nil
	case features.Int8, features.Int16, features.Int32, features.Uint8, features.Uint16, features.Uint32:
		i, ok := v.(int64)
		if !ok {
			return bad()
		}
		return parquet.Int32Value(int32(i)), nil //nolint:gosec // Cast checked the value fits the column type
	case features.Int64, features.Duration, features.Time64:
		i, ok := v.(int64)
		if !ok {
			return bad()
		}
		return parquet.Int64Value(i), nil
	case features.Time32:
		i, ok := v.(int64)
		if !ok {
			return bad()
		}
		return parquet.Int32Value(int32(i)), nil //nolint:gosec // times of day fit 32 bits
	case features.Uint64:
		switch x := v.(type) {
		case uint64:
			return parquet.Int64Value(int64(x)), nil //nolint:gosec // uint64 values are stored as int64 bits, as pyarrow does
		case int64:
			return parquet.Int64Value(x), nil
		}
		return bad()
	case features.Float32:
		f, ok := v.(float64)
		if !ok {
			return bad()
		}
		return parquet.FloatValue(float32(f)), nil
	case features.Float64:
		f, ok := v.(float64)
		if !ok {
			return bad()
		}
		return parquet.DoubleValue(f), nil
	case features.String:
		s, ok := v.(string)
		if !ok {
			return bad()
		}
		return parquet.ByteArrayValue([]byte(s)), nil
	case features.Binary:
		b, ok := v.([]byte)
		if !ok {
			return bad()
		}
		return parquet.ByteArrayValue(b), nil
	case features.Date32:
		t, ok := v.(time.Time)
		if !ok {
			return bad()
		}
		return parquet.Int32Value(int32(features.TimeToDays(t))), nil //nolint:gosec // dates within ±5.8 million years fit 32 bits
	case features.Timestamp:
		t, ok := v.(time.Time)
		if !ok {
			return bad()
		}
		return parquet.Int64Value(features.TimeToUnits(t, a.Unit)), nil
	case features.Decimal128:
		s, ok := v.(string)
		if !ok {
			return bad()
		}
		b, err := decimalBytes(s, a.Scale)
		if err != nil {
			return parquet.Value{}, err
		}
		return parquet.FixedLenByteArrayValue(b), nil
	}
	return parquet.Value{}, fmt.Errorf("%w: %s", ErrUnsupported, a)
}

// decimalBytes encodes a decimal string as 16 big-endian bytes.
func decimalBytes(s string, scale int) ([]byte, error) {
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(strings.TrimPrefix(s, "-"), "+")
	whole, frac, _ := strings.Cut(s, ".")
	if len(frac) > scale {
		frac = frac[:scale]
	}
	frac += strings.Repeat("0", scale-len(frac))
	v, ok := new(big.Int).SetString(whole+frac, 10)
	if !ok {
		return nil, fmt.Errorf("parquet: invalid decimal %q", s)
	}
	if neg {
		v.Neg(v)
	}
	if v.BitLen() > 127 {
		return nil, fmt.Errorf("parquet: decimal %q is too large", s)
	}
	if v.Sign() < 0 {
		v.Add(v, new(big.Int).Lsh(big.NewInt(1), 128))
	}
	out := make([]byte, 16)
	v.FillBytes(out)
	return out, nil
}
