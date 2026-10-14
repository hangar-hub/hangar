package pq

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"sync"

	"github.com/parquet-go/parquet-go"
	"github.com/parquet-go/parquet-go/deprecated"
	"github.com/parquet-go/parquet-go/encoding"
	"github.com/parquet-go/parquet-go/format"

	"github.com/jamesagarside/hangar/internal/datasets/arrowipc"
	"github.com/jamesagarside/hangar/internal/datasets/features"
)

// ErrInvalid means a file is not valid Parquet or has a schema Hangar
// cannot read.
var ErrInvalid = errors.New("parquet: invalid file")

type kind int

const (
	kindLeaf kind = iota
	kindStruct
	kindList
)

// pnode is a column of the file's schema with its levels.
type pnode struct {
	name string
	kind kind
	// def is the definition level a value reaches when this node is
	// present. For lists, elemDef is the level when the list has at least
	// one element, and rep is the repetition level of its elements.
	def, elemDef, rep    int
	children             []*pnode
	elem                 *pnode
	leaf                 int
	leafStart, leafCount int
	// maxDef and maxRep are a leaf's highest definition and repetition
	// levels.
	maxDef, maxRep int
	arrow          *features.Arrow
	el             format.SchemaElement
}

// File is an open Parquet file.
type File struct {
	f *parquet.File
	// Schema is the Struct of the file's top-level columns, in Arrow
	// storage types.
	Schema *features.Arrow
	// Features are from the `huggingface` metadata when present and
	// consistent with the columns, otherwise inferred from Schema.
	Features features.Features
	// HasFeatures is true when the `huggingface` metadata gave Features.
	HasFeatures bool
	cols        []*pnode
	leaves      []*pnode
	rowStarts   []int64
	r           io.ReaderAt
	size        int64
	mu          sync.Mutex
	// checked holds the result of checkChunk by row group and leaf column.
	checked map[[2]int]error
}

// OpenOptions configure Open.
type OpenOptions struct {
	// ReadBufferSize is the size of reads when no page index is used.
	ReadBufferSize int
}

// decode runs fn, which calls into parquet-go, and turns a panic into
// ErrInvalid. parquet-go trusts the lengths, offsets and indexes it reads
// from a file, so some corrupt files make it index past the end of a
// buffer. Every caller discards the parquet-go state fn used.
func decode(fn func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%w: %v", ErrInvalid, r)
		}
	}()
	return fn()
}

// Open opens a Parquet file and reads its footer.
func Open(r io.ReaderAt, size int64) (*File, error) {
	if err := checkFooter(r, size); err != nil {
		return nil, err
	}
	var f *parquet.File
	err := decode(func() error {
		var err error
		f, err = parquet.OpenFile(r, size, parquet.SkipPageIndex(true), parquet.SkipBloomFilters(true))
		return err
	})
	if err != nil {
		if errors.Is(err, ErrInvalid) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	file := &File{f: f, r: r, size: size, checked: map[[2]int]error{}}
	if err := file.buildSchema(); err != nil {
		return nil, err
	}
	var start int64
	for _, rg := range f.RowGroups() {
		if rg.NumRows() < 0 || start+rg.NumRows() < start {
			return nil, fmt.Errorf("%w: row group of %d rows", ErrInvalid, rg.NumRows())
		}
		file.rowStarts = append(file.rowStarts, start)
		start += rg.NumRows()
	}
	if start != f.NumRows() {
		return nil, fmt.Errorf("%w: the row groups hold %d rows but the footer says %d", ErrInvalid, start, f.NumRows())
	}
	return file, nil
}

// NumRows returns the number of rows.
func (f *File) NumRows() int64 { return f.f.NumRows() }

// NumRowGroups returns the number of row groups.
func (f *File) NumRowGroups() int { return len(f.f.RowGroups()) }

// RowGroupRows returns the rows of a row group.
func (f *File) RowGroupRows(i int) int64 { return f.f.RowGroups()[i].NumRows() }

// UncompressedBytes is the sum of the row groups' total_byte_size, which
// the Hub reports as `num_bytes` for copied Parquet files.
func (f *File) UncompressedBytes() int64 {
	var n int64
	for _, rg := range f.f.Metadata().RowGroups {
		n += rg.TotalByteSize
	}
	return n
}

// Metadata returns a key-value metadata entry.
func (f *File) Metadata(key string) (string, bool) { return f.f.Lookup(key) }

// MaxRowGroupBytes returns the largest uncompressed row group.
func (f *File) MaxRowGroupBytes() int64 {
	var n int64
	for _, rg := range f.f.Metadata().RowGroups {
		n = max(n, rg.TotalByteSize)
	}
	return n
}

// ColumnStats returns the min and max statistics of a leaf column in a row
// group when the writer recorded them. Statistics that cannot be decoded
// count as not recorded.
func (f *File) ColumnStats(rowGroup, column int) (minV, maxV parquet.Value, ok bool) {
	rg := f.f.RowGroups()[rowGroup]
	chunks := rg.ColumnChunks()
	if column >= len(chunks) {
		return parquet.Value{}, parquet.Value{}, false
	}
	type bounded interface {
		Bounds() (min, max parquet.Value, ok bool)
	}
	b, isBounded := chunks[column].(bounded)
	if !isBounded {
		return parquet.Value{}, parquet.Value{}, false
	}
	if err := decode(func() error {
		minV, maxV, ok = b.Bounds()
		return nil
	}); err != nil {
		return parquet.Value{}, parquet.Value{}, false
	}
	return minV, maxV, ok
}

// LeafRange returns the first leaf column index and count of a top-level column.
func (f *File) LeafRange(col int) (start, n int) { return f.cols[col].leafStart, f.cols[col].leafCount }

func (f *File) buildSchema() error {
	els := f.f.Metadata().Schema
	if len(els) == 0 {
		return fmt.Errorf("%w: no schema", ErrInvalid)
	}
	pos := 1
	leaf := 0
	root, _ := els[0].NumChildren.Get()
	if root < 0 {
		return fmt.Errorf("%w: schema", ErrInvalid)
	}
	f.Schema = &features.Arrow{ID: features.Struct}
	for range root {
		n, err := build(els, &pos, &leaf, 0, 0, 0)
		if err != nil {
			return err
		}
		f.cols = append(f.cols, n)
	}
	// pyarrow's ARROW:schema gives exact Arrow types (large strings, time
	// zones, durations); use it when it agrees with the Parquet columns.
	if b64, ok := f.f.Lookup("ARROW:schema"); ok {
		if raw, err := base64.StdEncoding.DecodeString(b64); err == nil {
			if s, err := arrowipc.ParseSchema(raw); err == nil && len(s.Type.Fields) == len(f.cols) {
				for i, c := range f.cols {
					if s.Type.Fields[i].Name == c.name && compatible(c.arrow, s.Type.Fields[i].Type) {
						c.arrow = s.Type.Fields[i].Type
					}
				}
			}
		}
	}
	for _, c := range f.cols {
		f.Schema.Fields = append(f.Schema.Fields, features.ArrowField{Name: c.name, Type: c.arrow})
		f.leaves = appendLeaves(f.leaves, c)
	}
	f.Features = features.FromArrowSchema(f.Schema)
	if meta, ok := f.f.Lookup(MetadataKey); ok {
		var doc struct {
			Info struct {
				Features json.RawMessage `json:"features"`
			} `json:"info"`
		}
		if json.Unmarshal([]byte(meta), &doc) == nil && len(doc.Info.Features) > 0 {
			if fs, err := features.ParseFeatures(doc.Info.Features); err == nil && len(fs) == len(f.cols) {
				okAll := true
				for i, fl := range fs {
					st, err := fl.Feature.Storage()
					if err != nil || fl.Name != f.cols[i].name || !compatible(f.cols[i].arrow, st) {
						okAll = false
						break
					}
				}
				if okAll {
					f.Features, f.HasFeatures = fs, true
				}
			}
		}
	}
	return nil
}

// appendLeaves appends n's leaf columns in file order.
func appendLeaves(out []*pnode, n *pnode) []*pnode {
	switch n.kind {
	case kindLeaf:
		return append(out, n)
	case kindList:
		return appendLeaves(out, n.elem)
	}
	for _, c := range n.children {
		out = appendLeaves(out, c)
	}
	return out
}

// compatible reports whether values read as a can be read as b: the same
// shape, with primitives that differ only in width, unit or encoding.
func compatible(a, b *features.Arrow) bool {
	if a.IsList() && b.IsList() {
		return compatible(a.Elem, b.Elem)
	}
	if a.ID == features.Struct || b.ID == features.Struct {
		if a.ID != b.ID || len(a.Fields) != len(b.Fields) {
			return false
		}
		for i := range a.Fields {
			if a.Fields[i].Name != b.Fields[i].Name || !compatible(a.Fields[i].Type, b.Fields[i].Type) {
				return false
			}
		}
		return true
	}
	if a.IsList() || b.IsList() {
		return false
	}
	group := func(x *features.Arrow) int {
		switch {
		case x.ID == features.Null:
			return 0
		case x.ID == features.Bool:
			return 1
		case x.IsInteger(), x.ID == features.Duration, x.ID == features.Time32, x.ID == features.Time64:
			return 2
		case x.IsFloat():
			return 3
		case x.IsString(), x.ID == features.Decimal128:
			return 4
		case x.IsBinary():
			return 5
		case x.ID == features.Timestamp, x.ID == features.Date32, x.ID == features.Date64:
			return 6
		}
		return 7
	}
	// A column of nulls can be read as anything.
	return group(a) == group(b) || group(a) == 0 || group(b) == 0
}

func repetition(el format.SchemaElement) format.FieldRepetitionType {
	r, _ := el.RepetitionType.Get()
	return r
}

const maxSchemaDepth = 64

func build(els []format.SchemaElement, pos, leaf *int, def, rep, depth int) (*pnode, error) {
	if *pos >= len(els) || depth > maxSchemaDepth {
		return nil, fmt.Errorf("%w: truncated schema", ErrInvalid)
	}
	el := els[*pos]
	*pos++
	switch repetition(el) {
	case format.Optional:
		def++
	case format.Repeated:
		// A repeated field outside a LIST group is a required list of its
		// elements.
		start := *leaf
		elem, err := buildBody(els, el, pos, leaf, def+1, rep+1, depth)
		if err != nil {
			return nil, err
		}
		elem.def = def + 1
		n := &pnode{name: el.Name, kind: kindList, def: def, elemDef: def + 1, rep: rep + 1, elem: elem,
			leafStart: start, leafCount: *leaf - start, arrow: features.ListOf(elem.arrow), el: el}
		return n, nil
	}
	n, err := buildBody(els, el, pos, leaf, def, rep, depth)
	if err != nil {
		return nil, err
	}
	n.def = def
	return n, nil
}

// buildBody builds a node whose own repetition is already accounted for.
func buildBody(els []format.SchemaElement, el format.SchemaElement, pos, leaf *int, def, rep, depth int) (*pnode, error) {
	nc, _ := el.NumChildren.Get()
	start := *leaf
	if nc <= 0 {
		if _, isGroup := el.NumChildren.Get(); isGroup && nc == 0 && !el.Type.Valid {
			return nil, fmt.Errorf("%w: empty group %q", ErrInvalid, el.Name)
		}
		a, err := leafArrow(el)
		if err != nil {
			return nil, err
		}
		n := &pnode{name: el.Name, kind: kindLeaf, leaf: *leaf, leafStart: *leaf, leafCount: 1, arrow: a, el: el, maxDef: def, maxRep: rep}
		*leaf++
		return n, nil
	}
	conv, _ := el.ConvertedType.Get()
	_, isList := el.LogicalType.Value.(*format.ListType)
	_, isMap := el.LogicalType.Value.(*format.MapType)
	if el.ConvertedType.Valid && conv == deprecated.List {
		isList = true
	}
	if el.ConvertedType.Valid && (conv == deprecated.Map || conv == deprecated.MapKeyValue) {
		isMap = true
	}
	if (isList || isMap) && nc == 1 && *pos < len(els) && repetition(els[*pos]) == format.Repeated {
		r := els[*pos]
		rnc, _ := r.NumChildren.Get()
		*pos++
		var elem *pnode
		var err error
		if isList && rnc == 1 && r.Name != "array" && r.Name != el.Name+"_tuple" {
			// Three-level list: the repeated group wraps the element.
			elem, err = build(els, pos, leaf, def+1, rep+1, depth+1)
		} else {
			// Two-level list, or a map's key_value group: the repeated
			// node is the element.
			elem, err = buildBody(els, r, pos, leaf, def+1, rep+1, depth+1)
			if err == nil {
				elem.def = def + 1
			}
		}
		if err != nil {
			return nil, err
		}
		return &pnode{name: el.Name, kind: kindList, elemDef: def + 1, rep: rep + 1, elem: elem,
			leafStart: start, leafCount: *leaf - start, arrow: features.ListOf(elem.arrow), el: el}, nil
	}
	n := &pnode{name: el.Name, kind: kindStruct, el: el, arrow: &features.Arrow{ID: features.Struct}}
	for range nc {
		c, err := build(els, pos, leaf, def, rep, depth+1)
		if err != nil {
			return nil, err
		}
		n.children = append(n.children, c)
		n.arrow.Fields = append(n.arrow.Fields, features.ArrowField{Name: c.name, Type: c.arrow})
	}
	n.leafStart, n.leafCount = start, *leaf-start
	return n, nil
}

func unitOf(u format.TimeUnit) string {
	switch u.Value.(type) {
	case *format.MicroSeconds:
		return "us"
	case *format.NanoSeconds:
		return "ns"
	}
	return "ms"
}

// maxDecimalPrecision is the most digits pyarrow reads a Parquet decimal
// with, as decimal256.
const maxDecimalPrecision = 76

// decimalArrow returns the type of a decimal column, checking its
// precision and scale as Arrow does: a corrupt scale of millions of digits
// would otherwise reach every value formatted.
func decimalArrow(name string, precision, scale int64) (*features.Arrow, error) {
	switch {
	case precision < 1 || scale < 0 || scale > precision:
		return nil, fmt.Errorf("%w: column %q has decimal precision %d and scale %d", ErrInvalid, name, precision, scale)
	case precision > maxDecimalPrecision:
		return nil, fmt.Errorf("%w: column %q has decimal precision %d, more than %d", ErrUnsupported, name, precision, maxDecimalPrecision)
	}
	return &features.Arrow{ID: features.Decimal128, Precision: int(precision), Scale: int(scale)}, nil
}

// leafArrow returns the Arrow type pyarrow reads a Parquet leaf as.
func leafArrow(el format.SchemaElement) (*features.Arrow, error) {
	phys, ok := el.Type.Get()
	if !ok {
		return nil, fmt.Errorf("%w: leaf %q has no type", ErrInvalid, el.Name)
	}
	conv, hasConv := el.ConvertedType.Get()
	p := features.Primitive
	switch lt := el.LogicalType.Value.(type) {
	case *format.StringType, *format.EnumType, *format.JsonType:
		return p(features.String), nil
	case *format.IntType:
		ids := map[[2]int]features.TypeID{{8, 1}: features.Int8, {16, 1}: features.Int16, {32, 1}: features.Int32, {64, 1}: features.Int64,
			{8, 0}: features.Uint8, {16, 0}: features.Uint16, {32, 0}: features.Uint32, {64, 0}: features.Uint64}
		s := 0
		if lt.IsSigned {
			s = 1
		}
		if id, ok := ids[[2]int{int(lt.BitWidth), s}]; ok {
			return p(id), nil
		}
	case *format.DateType:
		return p(features.Date32), nil
	case *format.TimestampType:
		a := &features.Arrow{ID: features.Timestamp, Unit: unitOf(lt.Unit)}
		if lt.IsAdjustedToUTC {
			a.TZ = "UTC"
		}
		return a, nil
	case *format.TimeType:
		u := unitOf(lt.Unit)
		if u == "ms" {
			return &features.Arrow{ID: features.Time32, Unit: u}, nil
		}
		return &features.Arrow{ID: features.Time64, Unit: u}, nil
	case *format.DecimalType:
		return decimalArrow(el.Name, int64(lt.Precision), int64(lt.Scale))
	case *format.Float16Type:
		return p(features.Float16), nil
	case *format.NullType:
		return p(features.Null), nil
	}
	if hasConv {
		switch conv {
		case deprecated.UTF8, deprecated.Enum, deprecated.Json:
			return p(features.String), nil
		case deprecated.Int8:
			return p(features.Int8), nil
		case deprecated.Int16:
			return p(features.Int16), nil
		case deprecated.Int32:
			return p(features.Int32), nil
		case deprecated.Int64:
			return p(features.Int64), nil
		case deprecated.Uint8:
			return p(features.Uint8), nil
		case deprecated.Uint16:
			return p(features.Uint16), nil
		case deprecated.Uint32:
			return p(features.Uint32), nil
		case deprecated.Uint64:
			return p(features.Uint64), nil
		case deprecated.Date:
			return p(features.Date32), nil
		case deprecated.TimestampMillis:
			return &features.Arrow{ID: features.Timestamp, Unit: "ms", TZ: "UTC"}, nil
		case deprecated.TimestampMicros:
			return &features.Arrow{ID: features.Timestamp, Unit: "us", TZ: "UTC"}, nil
		case deprecated.TimeMillis:
			return &features.Arrow{ID: features.Time32, Unit: "ms"}, nil
		case deprecated.TimeMicros:
			return &features.Arrow{ID: features.Time64, Unit: "us"}, nil
		case deprecated.Decimal:
			prec, _ := el.Precision.Get()
			sc, _ := el.Scale.Get()
			return decimalArrow(el.Name, int64(prec), int64(sc))
		}
	}
	switch phys {
	case format.Boolean:
		return p(features.Bool), nil
	case format.Int32:
		return p(features.Int32), nil
	case format.Int64:
		return p(features.Int64), nil
	case format.Int96:
		return &features.Arrow{ID: features.Timestamp, Unit: "ns"}, nil
	case format.Float:
		return p(features.Float32), nil
	case format.Double:
		return p(features.Float64), nil
	case format.ByteArray, format.FixedLenByteArray:
		return p(features.Binary), nil
	}
	return nil, fmt.Errorf("%w: column %q has physical type %d", ErrUnsupported, el.Name, phys)
}

// convert turns a Parquet leaf value into the representation of the
// features package for type a.
func convert(v parquet.Value, el format.SchemaElement, a *features.Arrow) any {
	if v.IsNull() {
		return nil
	}
	switch v.Kind() {
	case parquet.Boolean:
		return v.Boolean()
	case parquet.Int32:
		i := int64(v.Int32())
		switch a.ID {
		case features.Uint32, features.Uint8, features.Uint16:
			return int64(uint32(v.Int32())) //nolint:gosec // unsigned Parquet values are stored in signed physical types
		case features.Date32, features.Date64:
			return features.DaysToTime(i)
		case features.Time32:
			if a.Unit == "s" {
				return i / 1000
			}
		case features.Decimal128:
			b := make([]byte, 16)
			binary.LittleEndian.PutUint64(b, uint64(i)) //nolint:gosec // two's complement bits of the decimal
			if i < 0 {
				binary.LittleEndian.PutUint64(b[8:], math.MaxUint64)
			}
			return arrowipc.DecimalString(b, a.Scale)
		case features.Float64, features.Float32:
			return float64(i)
		}
		return i
	case parquet.Int64:
		i := v.Int64()
		switch a.ID {
		case features.Uint64:
			return uint64(i) //nolint:gosec // unsigned Parquet values are stored in signed physical types
		case features.Timestamp:
			unit := "ms"
			if lt, ok := el.LogicalType.Value.(*format.TimestampType); ok {
				unit = unitOf(lt.Unit)
			} else if c, ok := el.ConvertedType.Get(); ok && c == deprecated.TimestampMicros {
				unit = "us"
			}
			return features.TimeFromUnits(i, unit)
		case features.Date64:
			return features.TimeFromUnits(i, "ms")
		case features.Decimal128:
			b := make([]byte, 16)
			binary.LittleEndian.PutUint64(b, uint64(i)) //nolint:gosec // two's complement bits of the decimal
			if i < 0 {
				binary.LittleEndian.PutUint64(b[8:], math.MaxUint64)
			}
			return arrowipc.DecimalString(b, a.Scale)
		case features.Float64, features.Float32:
			return float64(i)
		}
		return i
	case parquet.Int96:
		return int96Time(v.Int96())
	case parquet.Float:
		return float64(v.Float())
	case parquet.Double:
		return v.Double()
	case parquet.ByteArray, parquet.FixedLenByteArray:
		b := v.ByteArray()
		switch {
		case a.ID == features.Decimal128:
			le := make([]byte, 16)
			fill := byte(0)
			if len(b) > 0 && b[0]&0x80 != 0 {
				fill = 0xff
			}
			for i := range le {
				le[i] = fill
			}
			for i := 0; i < len(b) && i < 16; i++ {
				le[i] = b[len(b)-1-i]
			}
			return arrowipc.DecimalString(le, a.Scale)
		case a.ID == features.Float16 && len(b) == 2:
			return arrowipc.HalfToFloat(binary.LittleEndian.Uint16(b))
		case a.IsString():
			return string(b)
		}
		return append([]byte(nil), b...)
	}
	return nil
}

// leafReader streams one leaf column's values of a row group, a batch at
// a time, so its memory does not grow with the size of a page.
type leafReader struct {
	pages parquet.Pages
	// page is the page being read and values its reader; both are nil
	// between pages.
	page   parquet.Page
	values parquet.ValueReader
	batch  []parquet.Value
	buf    []parquet.Value
	pos    int
	done   bool
}

// batchSize is the number of values read from a page at a time.
const batchSize = 1024

func (l *leafReader) release() {
	if l.page != nil {
		parquet.Release(l.page)
	}
	l.page, l.values = nil, nil
}

func (l *leafReader) close() {
	l.release()
	if l.pages != nil {
		_ = decode(l.pages.Close)
	}
}

// fill reads the next batch of values. After an error the reader is done.
func (l *leafReader) fill() error {
	err := decode(l.readBatch)
	if err != nil {
		l.release()
		l.done, l.buf, l.pos = true, nil, 0
	}
	return err
}

// readBatch reads values until it has some or the column ends.
func (l *leafReader) readBatch() error {
	if l.batch == nil {
		l.batch = make([]parquet.Value, batchSize)
	}
	for {
		if l.values == nil {
			page, err := l.pages.ReadPage()
			if errors.Is(err, io.EOF) {
				l.done = true
				l.buf, l.pos = nil, 0
				return nil
			}
			if err != nil {
				return err
			}
			l.page = page
			if err := checkDictionaryIndexes(page); err != nil {
				return err
			}
			l.values = page.Values()
		}
		k, err := l.values.ReadValues(l.batch)
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		// Values can point into the page, which is released when read.
		l.buf, l.pos = l.buf[:0], 0
		for _, v := range l.batch[:k] {
			l.buf = append(l.buf, v.Clone())
		}
		if errors.Is(err, io.EOF) || k == 0 {
			l.release()
		}
		if k > 0 {
			return nil
		}
	}
}

// checkDictionaryIndexes reports ErrInvalid when a dictionary-encoded page
// refers to a value past the end of its dictionary. parquet-go looks the
// indexes up without checking them, so a corrupt page would otherwise panic
// with an index out of range.
func checkDictionaryIndexes(page parquet.Page) error {
	dict := page.Dictionary()
	if dict == nil {
		return nil
	}
	data := page.Data()
	if data.Kind() != encoding.Int32 {
		return fmt.Errorf("%w: dictionary page without indexes", ErrInvalid)
	}
	size := dict.Len()
	for _, i := range data.Int32() {
		if i < 0 || int(i) >= size {
			return fmt.Errorf("%w: dictionary index %d is out of range for %d values", ErrInvalid, i, size)
		}
	}
	return nil
}

// row returns the values of the next row.
func (l *leafReader) row() ([]parquet.Value, error) {
	var out []parquet.Value
	for {
		if l.pos >= len(l.buf) {
			if l.done {
				break
			}
			if err := l.fill(); err != nil {
				return nil, err
			}
			continue
		}
		v := l.buf[l.pos]
		if v.RepetitionLevel() == 0 && len(out) > 0 {
			break
		}
		out = append(out, v)
		l.pos++
	}
	if len(out) == 0 {
		return nil, io.ErrUnexpectedEOF
	}
	return out, nil
}

// Scanner reads rows of chosen top-level columns.
type Scanner struct {
	f       *File
	cols    []int
	rg      int
	left    int64
	readers []*leafReader
	next    int64
}

// Columns returns the indexes of top-level columns by name; an empty list
// means every column.
func (f *File) Columns(names []string) ([]int, error) {
	if len(names) == 0 {
		out := make([]int, len(f.cols))
		for i := range out {
			out[i] = i
		}
		return out, nil
	}
	var out []int
	for _, n := range names {
		found := false
		for i, c := range f.cols {
			if c.name == n {
				out = append(out, i)
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("parquet: no column %q", n)
		}
	}
	return out, nil
}

// Scan returns a Scanner over rows [start, start+n) of the given columns,
// or to the end when n < 0.
func (f *File) Scan(cols []int, start, n int64) (*Scanner, error) {
	if start < 0 || start > f.NumRows() {
		return nil, fmt.Errorf("parquet: row %d is out of range", start)
	}
	if n < 0 || start+n > f.NumRows() {
		n = f.NumRows() - start
	}
	s := &Scanner{f: f, cols: cols, left: n, next: start}
	return s, nil
}

func (s *Scanner) closeReaders() {
	for _, r := range s.readers {
		r.close()
	}
	s.readers = nil
}

// Close releases the open pages.
func (s *Scanner) Close() { s.closeReaders() }

// open positions leaf readers at row s.next.
func (s *Scanner) open() error {
	s.closeReaders()
	starts := s.f.rowStarts
	rg := sort.Search(len(starts), func(i int) bool { return starts[i] > s.next }) - 1
	if rg < 0 {
		rg = 0
	}
	s.rg = rg
	group := s.f.f.RowGroups()[rg]
	chunks := group.ColumnChunks()
	local := s.next - starts[rg]
	for _, c := range s.cols {
		col := s.f.cols[c]
		for l := col.leafStart; l < col.leafStart+col.leafCount; l++ {
			if l >= len(chunks) {
				return fmt.Errorf("%w: missing column chunk", ErrInvalid)
			}
			if err := s.f.checkChunk(rg, l); err != nil {
				return err
			}
			var pages parquet.Pages
			err := decode(func() error {
				pages = chunks[l].Pages()
				return pages.SeekToRow(local)
			})
			if err != nil {
				if pages != nil {
					_ = decode(pages.Close)
				}
				return err
			}
			s.readers = append(s.readers, &leafReader{pages: pages})
		}
	}
	return nil
}

// Next returns the next row's values for the scanner's columns and the
// row's index, or io.EOF.
func (s *Scanner) Next() (int64, []any, error) {
	if s.left <= 0 {
		return 0, nil, io.EOF
	}
	if s.readers == nil || (s.rg+1 < len(s.f.rowStarts) && s.next >= s.f.rowStarts[s.rg+1]) {
		if err := s.open(); err != nil {
			return 0, nil, err
		}
	}
	out := make([]any, len(s.cols))
	ri := 0
	for i, c := range s.cols {
		col := s.f.cols[c]
		vals := make([][]parquet.Value, col.leafCount)
		for k := range vals {
			v, err := s.readers[ri].row()
			if err != nil {
				return 0, nil, fmt.Errorf("%w: column %q: %w", ErrInvalid, col.name, err)
			}
			vals[k] = v
			ri++
		}
		v, err := assemble(col, vals, 0)
		if err != nil {
			return 0, nil, err
		}
		out[i] = v
	}
	idx := s.next
	s.next++
	s.left--
	return idx, out, nil
}

// ReadRows reads rows [start, start+n) of the given columns.
func (f *File) ReadRows(cols []int, start, n int64) ([][]any, error) {
	s, err := f.Scan(cols, start, n)
	if err != nil {
		return nil, err
	}
	defer s.Close()
	var out [][]any
	for {
		_, row, err := s.Next()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
}

func assemble(n *pnode, vals [][]parquet.Value, depth int) (any, error) {
	if depth > maxSchemaDepth || len(vals) == 0 || len(vals[0]) == 0 {
		return nil, fmt.Errorf("%w: malformed levels", ErrInvalid)
	}
	first := vals[0][0]
	if first.DefinitionLevel() < n.def {
		return nil, nil
	}
	switch n.kind {
	case kindLeaf:
		return convert(first, n.el, n.arrow), nil
	case kindStruct:
		out := make([]any, len(n.children))
		off := 0
		for i, c := range n.children {
			if off+c.leafCount > len(vals) {
				return nil, fmt.Errorf("%w: struct levels", ErrInvalid)
			}
			v, err := assemble(c, vals[off:off+c.leafCount], depth+1)
			if err != nil {
				return nil, err
			}
			out[i] = v
			off += c.leafCount
		}
		return out, nil
	}
	if first.DefinitionLevel() < n.elemDef {
		return []any{}, nil
	}
	// Split every leaf's values into elements.
	var elems [][][]parquet.Value
	for li, lv := range vals {
		k := -1
		for i, v := range lv {
			if i == 0 || v.RepetitionLevel() == n.rep {
				k++
				if li == 0 {
					elems = append(elems, make([][]parquet.Value, len(vals)))
				}
				if k >= len(elems) {
					return nil, fmt.Errorf("%w: list levels", ErrInvalid)
				}
			}
			elems[k][li] = append(elems[k][li], v)
		}
		if k != len(elems)-1 {
			return nil, fmt.Errorf("%w: list levels differ between columns", ErrInvalid)
		}
	}
	out := make([]any, len(elems))
	for i, e := range elems {
		v, err := assemble(n.elem, e, depth+1)
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}

// int96Time converts a legacy Impala timestamp.
func int96Time(v deprecated.Int96) any {
	nanos := int64(v[1])<<32 | int64(v[0])
	julian := int64(v[2])
	days := julian - 2440588
	return features.TimeFromUnits(days*86400*1_000_000_000+nanos, "ns")
}
