package arrowipc

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"strings"

	"github.com/klauspost/compress/zstd"

	"github.com/jamesagarside/hangar/internal/datasets/features"
)

// Errors.
var (
	// ErrUnsupported means the data uses an Arrow feature Hangar does not read.
	ErrUnsupported = errors.New("arrow: unsupported")
	// ErrInvalid means the data is not valid Arrow IPC.
	ErrInvalid = errors.New("arrow: invalid IPC data")
)

// MaxMessageBytes caps one message's metadata or body.
const MaxMessageBytes = 1 << 31

// Message header types (Message.fbs).
const (
	headerSchema          = 1
	headerDictionaryBatch = 2
	headerRecordBatch     = 3
)

// node is a schema field with its dictionary encoding.
type node struct {
	name     string
	arrow    *features.Arrow
	dictID   int64
	dict     bool
	index    *features.Arrow
	children []*node
}

// Schema is a decoded Arrow schema.
type Schema struct {
	// Type is a Struct of the top-level fields.
	Type *features.Arrow
	// Metadata is the schema's custom metadata, such as `huggingface`.
	Metadata map[string]string
	fields   []*node
}

func keyValues(t table, i int) (map[string]string, error) {
	kvs, err := t.tables(i)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(kvs))
	for _, kv := range kvs {
		out[kv.str(0)] = kv.str(1)
	}
	return out, nil
}

var timeUnits = []string{"s", "ms", "us", "ns"}

func unitName(u int16) (string, error) {
	if u < 0 || int(u) >= len(timeUnits) {
		return "", fmt.Errorf("%w: time unit %d", ErrInvalid, u)
	}
	return timeUnits[u], nil
}

const maxFieldDepth = 64

// maxFields caps the fields of one schema, so corrupt metadata whose
// offsets point back at earlier tables cannot expand without end.
const maxFields = 100_000

func parseField(f table, depth int, count *int) (*node, error) {
	if depth > maxFieldDepth {
		return nil, fmt.Errorf("%w: fields nested too deeply", ErrInvalid)
	}
	if *count++; *count > maxFields {
		return nil, fmt.Errorf("%w: more than %d fields", ErrInvalid, maxFields)
	}
	n := &node{name: f.str(0)}
	kids, err := f.tables(5)
	if err != nil {
		return nil, err
	}
	for _, k := range kids {
		c, err := parseField(k, depth+1, count)
		if err != nil {
			return nil, err
		}
		n.children = append(n.children, c)
	}
	typeType := f.u8(2, 0)
	t, ok := f.sub(3)
	if !ok && typeType != 1 {
		return nil, fmt.Errorf("%w: field %q has no type", ErrInvalid, n.name)
	}
	a := &features.Arrow{}
	switch typeType {
	case 1:
		a.ID = features.Null
	case 2:
		bits, signed := t.i32(0, 0), t.bool(1, false)
		ids := map[int32][2]features.TypeID{8: {features.Uint8, features.Int8}, 16: {features.Uint16, features.Int16},
			32: {features.Uint32, features.Int32}, 64: {features.Uint64, features.Int64}}
		pair, ok := ids[bits]
		if !ok {
			return nil, fmt.Errorf("%w: integer width %d", ErrInvalid, bits)
		}
		a.ID = pair[0]
		if signed {
			a.ID = pair[1]
		}
	case 3:
		switch t.i16(0, 0) {
		case 0:
			a.ID = features.Float16
		case 1:
			a.ID = features.Float32
		case 2:
			a.ID = features.Float64
		default:
			return nil, fmt.Errorf("%w: float precision", ErrInvalid)
		}
	case 4, 15:
		a.ID = features.Binary
	case 5:
		a.ID = features.String
	case 6:
		a.ID = features.Bool
	case 7:
		if bw := t.i32(2, 128); bw != 128 {
			return nil, fmt.Errorf("%w: decimal%d", ErrUnsupported, bw)
		}
		a.ID, a.Precision, a.Scale = features.Decimal128, int(t.i32(0, 0)), int(t.i32(1, 0))
		if a.Precision < 1 || a.Precision > 38 || a.Scale < -38 || a.Scale > 38 {
			return nil, fmt.Errorf("%w: decimal precision %d and scale %d", ErrInvalid, a.Precision, a.Scale)
		}
	case 8:
		a.ID = features.Date64
		if t.i16(0, 1) == 0 {
			a.ID = features.Date32
		}
	case 9:
		u, err := unitName(t.i16(0, 1))
		if err != nil {
			return nil, err
		}
		a.ID, a.Unit = features.Time32, u
		if t.i32(1, 32) == 64 {
			a.ID = features.Time64
		}
	case 10:
		u, err := unitName(t.i16(0, 0))
		if err != nil {
			return nil, err
		}
		a.ID, a.Unit, a.TZ = features.Timestamp, u, t.str(1)
	case 12, 21, 17:
		// A Map is a list of key and value structs.
		if len(n.children) != 1 {
			return nil, fmt.Errorf("%w: list without one child", ErrInvalid)
		}
		a.ID, a.Elem = features.List, n.children[0].arrow
		if typeType == 21 {
			a.ID = features.LargeList
		}
	case 13:
		a.ID = features.Struct
		for _, c := range n.children {
			a.Fields = append(a.Fields, features.ArrowField{Name: c.name, Type: c.arrow})
		}
	case 16:
		if len(n.children) != 1 {
			return nil, fmt.Errorf("%w: fixed size list without one child", ErrInvalid)
		}
		a.ID, a.Elem, a.Size = features.FixedSizeList, n.children[0].arrow, int(t.i32(0, 0))
		if a.Size < 0 {
			return nil, fmt.Errorf("%w: negative list size", ErrInvalid)
		}
	case 18:
		u, err := unitName(t.i16(0, 1))
		if err != nil {
			return nil, err
		}
		a.ID, a.Unit = features.Duration, u
	case 19:
		a.ID = features.LargeBinary
	case 20:
		a.ID = features.LargeString
	default:
		return nil, fmt.Errorf("%w: Arrow type %d in field %q", ErrUnsupported, typeType, n.name)
	}
	n.arrow = a
	if d, ok := f.sub(4); ok {
		n.dict = true
		n.dictID = d.i64(0, 0)
		n.index = features.Primitive(features.Int32)
		if it, ok := d.sub(1); ok {
			ids := map[int32][2]features.TypeID{8: {features.Uint8, features.Int8}, 16: {features.Uint16, features.Int16},
				32: {features.Uint32, features.Int32}, 64: {features.Uint64, features.Int64}}
			pair, ok := ids[it.i32(0, 32)]
			if !ok {
				return nil, fmt.Errorf("%w: dictionary index width", ErrInvalid)
			}
			n.index = features.Primitive(pair[0])
			if it.bool(1, false) {
				n.index = features.Primitive(pair[1])
			}
		}
	}
	return n, nil
}

func parseSchemaTable(t table) (*Schema, error) {
	fields, err := t.tables(1)
	if err != nil {
		return nil, err
	}
	s := &Schema{Type: &features.Arrow{ID: features.Struct}}
	count := 0
	for _, f := range fields {
		n, err := parseField(f, 0, &count)
		if err != nil {
			return nil, err
		}
		s.fields = append(s.fields, n)
		s.Type.Fields = append(s.Type.Fields, features.ArrowField{Name: n.name, Type: n.arrow})
	}
	if s.Metadata, err = keyValues(t, 2); err != nil {
		return nil, err
	}
	return s, nil
}

// message is a decoded Message table.
type message struct {
	headerType uint8
	header     table
	bodyLength int64
}

func parseMessage(meta []byte) (*message, error) {
	root, err := rootTable(meta)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	m := &message{headerType: root.u8(1, 0), bodyLength: root.i64(3, 0)}
	h, ok := root.sub(2)
	if !ok {
		return nil, fmt.Errorf("%w: message without header", ErrInvalid)
	}
	m.header = h
	if m.bodyLength < 0 || m.bodyLength > MaxMessageBytes {
		return nil, fmt.Errorf("%w: body length %d", ErrInvalid, m.bodyLength)
	}
	return m, nil
}

// ParseSchema decodes a serialised Arrow schema, as pyarrow stores it
// (base64-decoded) in a Parquet file's `ARROW:schema` metadata: an IPC
// message, with or without the continuation marker.
func ParseSchema(b []byte) (*Schema, error) {
	r := &Reader{r: bytes.NewReader(b)}
	m, _, err := r.readMessage()
	if err != nil {
		return nil, err
	}
	if m == nil || m.headerType != headerSchema {
		return nil, fmt.Errorf("%w: expected a schema message", ErrInvalid)
	}
	return parseSchemaTable(m.header)
}

// Reader reads record batches from an Arrow IPC stream or file.
type Reader struct {
	r      io.Reader
	Schema *Schema
	dicts  map[int64][]any
	zstd   *zstd.Decoder
}

// fileMagic starts and ends the Arrow file format.
const fileMagic = "ARROW1"

// NewReader starts reading an Arrow IPC stream or file and reads its schema.
func NewReader(r io.Reader) (*Reader, error) {
	br := &peekReader{r: r}
	head, err := br.peek(8)
	if err != nil && len(head) < 4 {
		return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if strings.HasPrefix(string(head), fileMagic) {
		if _, err := io.ReadFull(br, make([]byte, 8)); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
		}
	}
	rd := &Reader{r: br, dicts: map[int64][]any{}}
	m, _, err := rd.readMessage()
	if err != nil {
		return nil, err
	}
	if m == nil {
		return nil, fmt.Errorf("%w: no schema", ErrInvalid)
	}
	if m.headerType != headerSchema {
		return nil, fmt.Errorf("%w: the stream does not start with a schema", ErrInvalid)
	}
	s, err := parseSchemaTable(m.header)
	if err != nil {
		return nil, err
	}
	rd.Schema = s
	return rd, nil
}

type peekReader struct {
	r   io.Reader
	buf []byte
}

func (p *peekReader) peek(n int) ([]byte, error) {
	for len(p.buf) < n {
		tmp := make([]byte, n-len(p.buf))
		k, err := p.r.Read(tmp)
		p.buf = append(p.buf, tmp[:k]...)
		if err != nil {
			return p.buf, err
		}
	}
	return p.buf[:n], nil
}

func (p *peekReader) Read(b []byte) (int, error) {
	if len(p.buf) > 0 {
		n := copy(b, p.buf)
		p.buf = p.buf[n:]
		return n, nil
	}
	return p.r.Read(b)
}

// readMessage reads one encapsulated message. It returns nil at the end
// of the stream.
func (r *Reader) readMessage() (*message, []byte, error) {
	var word [4]byte
	if _, err := io.ReadFull(r.r, word[:]); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	size := binary.LittleEndian.Uint32(word[:])
	if size == 0xFFFFFFFF {
		if _, err := io.ReadFull(r.r, word[:]); err != nil {
			return nil, nil, fmt.Errorf("%w: %w", ErrInvalid, err)
		}
		size = binary.LittleEndian.Uint32(word[:])
	}
	if size == 0 {
		return nil, nil, nil
	}
	if size > MaxMessageBytes {
		return nil, nil, fmt.Errorf("%w: metadata of %d bytes", ErrInvalid, size)
	}
	meta, err := readN(r.r, int64(size))
	if err != nil {
		return nil, nil, err
	}
	m, err := parseMessage(meta)
	if err != nil {
		return nil, nil, err
	}
	body, err := readN(r.r, m.bodyLength)
	if err != nil {
		return nil, nil, err
	}
	return m, body, nil
}

// readN reads exactly n bytes, growing the buffer as data arrives so a
// corrupt length cannot make it allocate more than the input holds.
func readN(r io.Reader, n int64) ([]byte, error) {
	var buf bytes.Buffer
	got, err := io.CopyN(&buf, r, n)
	if err != nil || got != n {
		return nil, fmt.Errorf("%w: truncated message (%d of %d bytes)", ErrInvalid, got, n)
	}
	return buf.Bytes(), nil
}

// Next returns the rows of the next record batch, each a value per
// top-level field, or io.EOF.
func (r *Reader) Next() ([][]any, error) {
	for {
		m, body, err := r.readMessage()
		if err != nil {
			return nil, err
		}
		if m == nil {
			return nil, io.EOF
		}
		switch m.headerType {
		case headerDictionaryBatch:
			if err := r.readDictionary(m.header, body); err != nil {
				return nil, err
			}
		case headerRecordBatch:
			cols, n, err := r.decodeBatch(m.header, body, r.Schema.fields)
			if err != nil {
				return nil, err
			}
			rows := make([][]any, n)
			for i := range rows {
				row := make([]any, len(cols))
				for j, c := range cols {
					row[j] = c[i]
				}
				rows[i] = row
			}
			return rows, nil
		case headerSchema:
			return nil, fmt.Errorf("%w: a second schema in the stream", ErrInvalid)
		default:
			return nil, fmt.Errorf("%w: message type %d", ErrUnsupported, m.headerType)
		}
	}
}

func findDictField(fields []*node, id int64) *node {
	for _, f := range fields {
		if f.dict && f.dictID == id {
			return f
		}
		if c := findDictField(f.children, id); c != nil {
			return c
		}
	}
	return nil
}

func (r *Reader) readDictionary(h table, body []byte) error {
	id := h.i64(0, 0)
	f := findDictField(r.Schema.fields, id)
	if f == nil {
		return fmt.Errorf("%w: dictionary %d has no field", ErrInvalid, id)
	}
	data, ok := h.sub(1)
	if !ok {
		return fmt.Errorf("%w: dictionary without data", ErrInvalid)
	}
	valueField := &node{name: f.name, arrow: f.arrow, children: f.children}
	cols, _, err := r.decodeBatch(data, body, []*node{valueField})
	if err != nil {
		return err
	}
	if h.bool(2, false) {
		r.dicts[id] = append(r.dicts[id], cols[0]...)
	} else {
		r.dicts[id] = cols[0]
	}
	return nil
}

// batch walks a record batch's nodes and buffers.
type batch struct {
	r       *Reader
	body    []byte
	nodes   []byte
	nNodes  int
	buffers []byte
	nBufs   int
	codec   int8
	compact bool
	ni, bi  int
}

func (b *batch) nextNode() (length, nulls int64, err error) {
	if b.ni >= b.nNodes {
		return 0, 0, fmt.Errorf("%w: too few field nodes", ErrInvalid)
	}
	p := b.nodes[16*b.ni:]
	b.ni++
	length = s64(binary.LittleEndian.Uint64(p))
	nulls = s64(binary.LittleEndian.Uint64(p[8:]))
	// Every value but a null takes at least a bit of the body.
	if length < 0 || nulls < 0 || nulls > length || length > 8*int64(len(b.body))+1<<16 {
		return 0, 0, fmt.Errorf("%w: field node length %d", ErrInvalid, length)
	}
	return length, nulls, nil
}

func (b *batch) nextBuffer() ([]byte, error) {
	if b.bi >= b.nBufs {
		return nil, fmt.Errorf("%w: too few buffers", ErrInvalid)
	}
	p := b.buffers[16*b.bi:]
	b.bi++
	off := s64(binary.LittleEndian.Uint64(p))
	n := s64(binary.LittleEndian.Uint64(p[8:]))
	if off < 0 || n < 0 || off > int64(len(b.body)) || n > int64(len(b.body))-off {
		return nil, fmt.Errorf("%w: buffer out of range", ErrInvalid)
	}
	buf := b.body[off : off+n]
	if !b.compact || n == 0 {
		return buf, nil
	}
	if n < 8 {
		return nil, fmt.Errorf("%w: compressed buffer", ErrInvalid)
	}
	raw := s64(binary.LittleEndian.Uint64(buf))
	if raw == -1 {
		return buf[8:], nil
	}
	if raw < 0 || raw > MaxMessageBytes {
		return nil, fmt.Errorf("%w: uncompressed size", ErrInvalid)
	}
	switch b.codec {
	case 1:
		if b.r.zstd == nil {
			d, err := zstd.NewReader(nil, zstd.WithDecoderMaxMemory(MaxMessageBytes))
			if err != nil {
				return nil, err
			}
			b.r.zstd = d
		}
		out, err := b.r.zstd.DecodeAll(buf[8:], make([]byte, 0, raw))
		if err != nil {
			return nil, fmt.Errorf("%w: zstd: %w", ErrInvalid, err)
		}
		return out, nil
	}
	return nil, fmt.Errorf("%w: LZ4 compressed Arrow buffers", ErrUnsupported)
}

func (r *Reader) decodeBatch(h table, body []byte, fields []*node) ([][]any, int64, error) {
	length := h.i64(0, 0)
	if length < 0 || length > 8*int64(len(body))+1<<16 {
		return nil, 0, fmt.Errorf("%w: batch length %d", ErrInvalid, length)
	}
	nodes, nNodes, err := h.structs(1, 16)
	if err != nil {
		return nil, 0, err
	}
	buffers, nBufs, err := h.structs(2, 16)
	if err != nil {
		return nil, 0, err
	}
	b := &batch{r: r, body: body, nodes: nodes, nNodes: nNodes, buffers: buffers, nBufs: nBufs}
	if c, ok := h.sub(3); ok {
		b.compact = true
		b.codec = c.i8(0, 0)
	}
	cols := make([][]any, len(fields))
	for i, f := range fields {
		col, err := b.decode(f, 0)
		if err != nil {
			return nil, 0, fmt.Errorf("field %q: %w", f.name, err)
		}
		if int64(len(col)) != length {
			return nil, 0, fmt.Errorf("%w: field %q has %d values, not %d", ErrInvalid, f.name, len(col), length)
		}
		cols[i] = col
	}
	return cols, length, nil
}

func valid(bitmap []byte, nulls int64, i int64) bool {
	if nulls == 0 || len(bitmap) == 0 {
		return true
	}
	if int(i/8) >= len(bitmap) {
		return false
	}
	return bitmap[i/8]&(1<<(i%8)) != 0
}

func need(buf []byte, n int64) error {
	if int64(len(buf)) < n {
		return fmt.Errorf("%w: buffer of %d bytes, need %d", ErrInvalid, len(buf), n)
	}
	return nil
}

func (b *batch) decode(f *node, depth int) ([]any, error) {
	if depth > maxFieldDepth {
		return nil, fmt.Errorf("%w: nesting", ErrInvalid)
	}
	n, nulls, err := b.nextNode()
	if err != nil {
		return nil, err
	}
	out := make([]any, n)
	a := f.arrow
	if f.dict {
		bitmap, err := b.nextBuffer()
		if err != nil {
			return nil, err
		}
		idx, err := b.nextBuffer()
		if err != nil {
			return nil, err
		}
		ids, err := fixed(f.index, idx, n)
		if err != nil {
			return nil, err
		}
		dict := b.r.dicts[f.dictID]
		for i := range n {
			if !valid(bitmap, nulls, i) {
				continue
			}
			var k int64
			switch v := ids[i].(type) {
			case int64:
				k = v
			case uint64:
				if v > math.MaxInt64 {
					return nil, fmt.Errorf("%w: dictionary index", ErrInvalid)
				}
				k = int64(v)
			}
			if k < 0 || k >= int64(len(dict)) {
				return nil, fmt.Errorf("%w: dictionary index %d out of range", ErrInvalid, k)
			}
			out[i] = dict[k]
		}
		return out, nil
	}
	if a.ID == features.Null {
		return out, nil
	}
	bitmap, err := b.nextBuffer()
	if err != nil {
		return nil, err
	}
	switch a.ID {
	case features.String, features.Binary, features.LargeString, features.LargeBinary:
		offs, err := b.nextBuffer()
		if err != nil {
			return nil, err
		}
		data, err := b.nextBuffer()
		if err != nil {
			return nil, err
		}
		large := a.ID == features.LargeString || a.ID == features.LargeBinary
		off, err := offsets(offs, n, large, int64(len(data)))
		if err != nil {
			return nil, err
		}
		for i := range n {
			if !valid(bitmap, nulls, i) {
				continue
			}
			s := data[off[i]:off[i+1]]
			if a.ID == features.String || a.ID == features.LargeString {
				out[i] = string(s)
			} else {
				out[i] = bytes.Clone(s)
			}
		}
		return out, nil
	case features.List, features.LargeList:
		offs, err := b.nextBuffer()
		if err != nil {
			return nil, err
		}
		child, err := b.decode(f.children[0], depth+1)
		if err != nil {
			return nil, err
		}
		off, err := offsets(offs, n, a.ID == features.LargeList, int64(len(child)))
		if err != nil {
			return nil, err
		}
		for i := range n {
			if valid(bitmap, nulls, i) {
				out[i] = append([]any{}, child[off[i]:off[i+1]]...)
			}
		}
		return out, nil
	case features.FixedSizeList:
		child, err := b.decode(f.children[0], depth+1)
		if err != nil {
			return nil, err
		}
		size := int64(a.Size)
		if int64(len(child)) < n*size {
			return nil, fmt.Errorf("%w: fixed size list child too short", ErrInvalid)
		}
		for i := range n {
			if valid(bitmap, nulls, i) {
				out[i] = append([]any{}, child[i*size:(i+1)*size]...)
			}
		}
		return out, nil
	case features.Struct:
		cols := make([][]any, len(f.children))
		for k, c := range f.children {
			col, err := b.decode(c, depth+1)
			if err != nil {
				return nil, err
			}
			if int64(len(col)) < n {
				return nil, fmt.Errorf("%w: struct child too short", ErrInvalid)
			}
			cols[k] = col
		}
		for i := range n {
			if !valid(bitmap, nulls, i) {
				continue
			}
			row := make([]any, len(cols))
			for k := range cols {
				row[k] = cols[k][i]
			}
			out[i] = row
		}
		return out, nil
	}
	data, err := b.nextBuffer()
	if err != nil {
		return nil, err
	}
	vals, err := fixed(a, data, n)
	if err != nil {
		return nil, err
	}
	for i := range n {
		if valid(bitmap, nulls, i) {
			out[i] = vals[i]
		}
	}
	return out, nil
}

func offsets(buf []byte, n int64, large bool, limit int64) ([]int64, error) {
	w := int64(4)
	if large {
		w = 8
	}
	if n == 0 {
		return []int64{0}, nil
	}
	if err := need(buf, (n+1)*w); err != nil {
		return nil, err
	}
	out := make([]int64, n+1)
	for i := range out {
		if large {
			out[i] = s64(binary.LittleEndian.Uint64(buf[int64(i)*8:]))
		} else {
			out[i] = int64(s32(binary.LittleEndian.Uint32(buf[int64(i)*4:])))
		}
		if out[i] < 0 || out[i] > limit || (i > 0 && out[i] < out[i-1]) {
			return nil, fmt.Errorf("%w: offsets out of range", ErrInvalid)
		}
	}
	return out, nil
}

// fixed decodes n fixed-width values.
func fixed(a *features.Arrow, buf []byte, n int64) ([]any, error) {
	out := make([]any, n)
	if a.ID == features.Bool {
		if err := need(buf, (n+7)/8); err != nil {
			return nil, err
		}
		for i := range n {
			out[i] = buf[i/8]&(1<<(i%8)) != 0
		}
		return out, nil
	}
	widths := map[features.TypeID]int64{
		features.Int8: 1, features.Uint8: 1, features.Int16: 2, features.Uint16: 2, features.Float16: 2,
		features.Int32: 4, features.Uint32: 4, features.Float32: 4, features.Date32: 4, features.Time32: 4,
		features.Int64: 8, features.Uint64: 8, features.Float64: 8, features.Date64: 8, features.Time64: 8,
		features.Timestamp: 8, features.Duration: 8, features.Decimal128: 16,
	}
	w, ok := widths[a.ID]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrUnsupported, a)
	}
	if err := need(buf, n*w); err != nil {
		return nil, err
	}
	for i := range n {
		p := buf[i*w:]
		switch a.ID {
		case features.Int8:
			out[i] = int64(s8(p[0]))
		case features.Uint8:
			out[i] = int64(p[0])
		case features.Int16:
			out[i] = int64(s16(binary.LittleEndian.Uint16(p)))
		case features.Uint16:
			out[i] = int64(binary.LittleEndian.Uint16(p))
		case features.Float16:
			out[i] = HalfToFloat(binary.LittleEndian.Uint16(p))
		case features.Int32, features.Time32:
			out[i] = int64(s32(binary.LittleEndian.Uint32(p)))
		case features.Uint32:
			out[i] = int64(binary.LittleEndian.Uint32(p))
		case features.Float32:
			out[i] = float64(math.Float32frombits(binary.LittleEndian.Uint32(p)))
		case features.Date32:
			out[i] = features.DaysToTime(int64(s32(binary.LittleEndian.Uint32(p))))
		case features.Int64, features.Time64, features.Duration:
			out[i] = s64(binary.LittleEndian.Uint64(p))
		case features.Uint64:
			out[i] = binary.LittleEndian.Uint64(p)
		case features.Float64:
			out[i] = math.Float64frombits(binary.LittleEndian.Uint64(p))
		case features.Date64:
			out[i] = features.TimeFromUnits(s64(binary.LittleEndian.Uint64(p)), "ms")
		case features.Timestamp:
			out[i] = features.TimeFromUnits(s64(binary.LittleEndian.Uint64(p)), a.Unit)
		case features.Decimal128:
			out[i] = DecimalString(p[:16], a.Scale)
		}
	}
	return out, nil
}

// HalfToFloat converts an IEEE 754 half-precision value.
func HalfToFloat(h uint16) float64 {
	sign := 1.0
	if h&0x8000 != 0 {
		sign = -1
	}
	exp := int(h>>10) & 0x1f
	frac := float64(h & 0x3ff)
	switch exp {
	case 0:
		return sign * math.Ldexp(frac, -24)
	case 0x1f:
		if frac == 0 {
			return math.Inf(int(sign))
		}
		return math.NaN()
	}
	return sign * math.Ldexp(1+frac/1024, exp-15)
}

// DecimalString formats a little-endian two's complement decimal.
func DecimalString(le []byte, scale int) string {
	be := make([]byte, len(le))
	for i := range le {
		be[len(le)-1-i] = le[i]
	}
	v := new(big.Int).SetBytes(be)
	if len(be) > 0 && be[0]&0x80 != 0 {
		v.Sub(v, new(big.Int).Lsh(big.NewInt(1), uint(8*len(be))))
	}
	s := v.String()
	if scale <= 0 {
		return s + strings.Repeat("0", -scale)
	}
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	if len(s) <= scale {
		s = strings.Repeat("0", scale-len(s)+1) + s
	}
	s = s[:len(s)-scale] + "." + s[len(s)-scale:]
	if neg {
		s = "-" + s
	}
	return s
}
