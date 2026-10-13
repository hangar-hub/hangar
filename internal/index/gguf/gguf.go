// Package gguf reads GGUF headers: the magic, version, metadata key-value
// pairs and tensor infos, stopping before the tensor data
// (https://github.com/ggml-org/ggml/blob/master/docs/gguf.md). It reads
// through an io.ReaderAt, so a ranged reader fetches only the windows the
// header occupies. Type sizes and file type names come from
// github.com/gpustack/gguf-parser-go.
package gguf

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"

	parser "github.com/gpustack/gguf-parser-go"

	"github.com/jamesagarside/hangar/internal/index/rangeio"
)

// ErrInvalid means the file is not a valid GGUF file.
var ErrInvalid = errors.New("invalid GGUF file")

// Value types (gguf.md "GGUF metadata value types").
const (
	TypeUint8   = 0
	TypeInt8    = 1
	TypeUint16  = 2
	TypeInt16   = 3
	TypeUint32  = 4
	TypeInt32   = 5
	TypeFloat32 = 6
	TypeBool    = 7
	TypeString  = 8
	TypeArray   = 9
	TypeUint64  = 10
	TypeInt64   = 11
	TypeFloat64 = 12
)

// Limits that stop a malformed or hostile header from exhausting memory.
const (
	// maxStringLength bounds one metadata string, such as a chat template.
	maxStringLength = 64 << 20
	// maxDims is the most dimensions a tensor may have. ggml uses 4.
	maxDims = 8
	// KeepArray is how many array elements a Value keeps for display.
	KeepArray = 64
	// maxKeptTokens bounds the tokenizer vocabulary kept to look up the BOS
	// and EOS tokens.
	maxKeptTokens    = 1 << 21
	defaultAlignment = 32
)

// Value is a metadata value. Arrays keep their length and only their first
// KeepArray elements, except tokenizer.ggml.tokens, which File keeps
// separately.
type Value struct {
	Type int `json:"type"`
	// Scalar holds numbers, booleans and strings.
	Scalar any `json:"value,omitempty"`
	// ArrayType and Len describe arrays.
	ArrayType int   `json:"array_type,omitempty"`
	Len       int64 `json:"len,omitempty"`
	Items     []any `json:"items,omitempty"`
}

// String returns a string value, or "".
func (v Value) String() string {
	s, _ := v.Scalar.(string)
	return s
}

// Int returns an integer value as int64, and whether it is one.
func (v Value) Int() (int64, bool) {
	switch n := v.Scalar.(type) {
	case uint8:
		return int64(n), true
	case int8:
		return int64(n), true
	case uint16:
		return int64(n), true
	case int16:
		return int64(n), true
	case uint32:
		return int64(n), true
	case int32:
		return int64(n), true
	case uint64:
		if n > math.MaxInt64 {
			return 0, false
		}
		return int64(n), true
	case int64:
		return n, true
	}
	return 0, false
}

// Tensor is one tensor info.
type Tensor struct {
	Name   string   `json:"name"`
	Dims   []uint64 `json:"shape"`
	Type   uint32   `json:"type"`
	Offset uint64   `json:"offset"`
}

// TypeName returns the ggml type name, such as Q4_K.
func (t Tensor) TypeName() string { return parser.GGMLType(t.Type).String() }

// Elements returns the element count, and false on overflow.
func (t Tensor) Elements() (uint64, bool) {
	n := uint64(1)
	for _, d := range t.Dims {
		if d != 0 && n > math.MaxUint64/d {
			return 0, false
		}
		n *= d
	}
	return n, true
}

// Bytes returns the tensor's size in the data section, and false when the
// type is unknown or the shape does not fit the type's block size.
func (t Tensor) Bytes() (uint64, bool) {
	tt, ok := parser.GGMLType(t.Type).Trait()
	if !ok || tt.BlockSize == 0 || len(t.Dims) == 0 {
		return 0, false
	}
	n, ok := t.Elements()
	if !ok || n%tt.BlockSize != 0 {
		return 0, false
	}
	blocks := n / tt.BlockSize
	if tt.TypeSize != 0 && blocks > math.MaxUint64/tt.TypeSize {
		return 0, false
	}
	return blocks * tt.TypeSize, true
}

// File is a parsed GGUF header.
type File struct {
	Version     uint32 `json:"version"`
	BigEndian   bool   `json:"big_endian,omitempty"`
	TensorCount uint64 `json:"tensor_count"`
	// Keys lists metadata keys in file order.
	Keys     []string         `json:"-"`
	Metadata map[string]Value `json:"metadata"`
	Tensors  []Tensor         `json:"tensors"`
	// Alignment is general.alignment, or 32.
	Alignment uint64 `json:"alignment"`
	// DataOffset is where the tensor data starts.
	DataOffset int64 `json:"data_offset"`
	// Tokens is tokenizer.ggml.tokens, kept to name the BOS and EOS tokens.
	Tokens []string `json:"-"`
}

// Get returns a metadata value.
func (f *File) Get(key string) (Value, bool) {
	v, ok := f.Metadata[key]
	return v, ok
}

// StringValue returns a string metadata value, or "".
func (f *File) StringValue(key string) string {
	v, _ := f.Metadata[key]
	return v.String()
}

// Int returns an integer metadata value.
func (f *File) Int(key string) (int64, bool) {
	v, ok := f.Metadata[key]
	if !ok {
		return 0, false
	}
	return v.Int()
}

// Architecture returns general.architecture.
func (f *File) Architecture() string { return f.StringValue("general.architecture") }

// ContextLength returns <arch>.context_length.
func (f *File) ContextLength() int64 {
	n, _ := f.Int(f.Architecture() + ".context_length")
	return n
}

// ChatTemplate returns tokenizer.chat_template.
func (f *File) ChatTemplate() string { return f.StringValue("tokenizer.chat_template") }

func (f *File) token(key string) string {
	id, ok := f.Int(key)
	if !ok || id < 0 || id >= int64(len(f.Tokens)) {
		return ""
	}
	return f.Tokens[id]
}

// BOSToken returns the BOS token text.
func (f *File) BOSToken() string { return f.token("tokenizer.ggml.bos_token_id") }

// EOSToken returns the EOS token text.
func (f *File) EOSToken() string { return f.token("tokenizer.ggml.eos_token_id") }

// FileType returns general.file_type and its name without the MOSTLY_
// prefix, such as Q4_K_M.
func (f *File) FileType() (int, string) {
	n, ok := f.Int("general.file_type")
	if !ok || n < 0 || n > math.MaxInt32 {
		return -1, ""
	}
	name := parser.GGUFFileType(uint32(n)).String()
	if strings.HasPrefix(name, "GGUFFileType(") {
		return int(n), ""
	}
	return int(n), strings.TrimPrefix(name, "MOSTLY_")
}

// Parameters returns the total element count of every tensor.
func (f *File) Parameters() int64 {
	var total uint64
	for _, t := range f.Tensors {
		n, ok := t.Elements()
		if !ok || total > math.MaxInt64-n {
			return math.MaxInt64
		}
		total += n
	}
	return int64(total)
}

// ParametersByType returns element counts per ggml type name.
func (f *File) ParametersByType() map[string]int64 {
	out := map[string]int64{}
	for _, t := range f.Tensors {
		n, ok := t.Elements()
		if ok && n <= math.MaxInt64 {
			out[t.TypeName()] += int64(n)
		}
	}
	return out
}

// Split returns split.no and split.count for split files (0 and 0 when
// the file is not split).
func (f *File) Split() (no, count int64) {
	no, _ = f.Int("split.no")
	count, _ = f.Int("split.count")
	return no, count
}

type reader struct {
	c   rangeio.Cursor
	bo  binary.ByteOrder
	v   uint32
	end int64
	buf [8]byte
}

func (r *reader) read(n int) ([]byte, error) {
	if _, err := io.ReadFull(&r.c, r.buf[:n]); err != nil {
		return nil, fmt.Errorf("%w: unexpected end of header at byte %d", ErrInvalid, r.c.Pos)
	}
	return r.buf[:n], nil
}

func (r *reader) u8() (uint8, error) {
	b, err := r.read(1)
	if err != nil {
		return 0, err
	}
	return b[0], nil
}

func (r *reader) u16() (uint16, error) {
	b, err := r.read(2)
	if err != nil {
		return 0, err
	}
	return r.bo.Uint16(b), nil
}

func (r *reader) u32() (uint32, error) {
	b, err := r.read(4)
	if err != nil {
		return 0, err
	}
	return r.bo.Uint32(b), nil
}

func (r *reader) u64() (uint64, error) {
	b, err := r.read(8)
	if err != nil {
		return 0, err
	}
	return r.bo.Uint64(b), nil
}

// count reads a length or count: 32 bits in version 1, 64 bits after.
func (r *reader) count() (uint64, error) {
	if r.v == 1 {
		n, err := r.u32()
		return uint64(n), err
	}
	return r.u64()
}

func (r *reader) remaining() int64 { return r.end - r.c.Pos }

func (r *reader) str() (string, error) {
	n, err := r.count()
	if err != nil {
		return "", err
	}
	if n > maxStringLength || int64(n) > r.remaining() {
		return "", fmt.Errorf("%w: string of %d bytes at byte %d is too long", ErrInvalid, n, r.c.Pos)
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(&r.c, b); err != nil {
		return "", fmt.Errorf("%w: unexpected end of header in a string", ErrInvalid)
	}
	return string(b), nil
}

func scalarSize(t uint32) int64 {
	switch t {
	case TypeUint8, TypeInt8, TypeBool:
		return 1
	case TypeUint16, TypeInt16:
		return 2
	case TypeUint32, TypeInt32, TypeFloat32:
		return 4
	case TypeUint64, TypeInt64, TypeFloat64:
		return 8
	}
	return 0
}

func (r *reader) scalar(t uint32) (any, error) {
	switch t {
	case TypeUint8:
		return r.u8()
	case TypeInt8:
		v, err := r.u8()
		return int8(v), err
	case TypeUint16:
		return r.u16()
	case TypeInt16:
		v, err := r.u16()
		return int16(v), err
	case TypeUint32:
		return r.u32()
	case TypeInt32:
		v, err := r.u32()
		return int32(v), err
	case TypeFloat32:
		v, err := r.u32()
		return math.Float32frombits(v), err
	case TypeBool:
		v, err := r.u8()
		return v != 0, err
	case TypeString:
		return r.str()
	case TypeUint64:
		return r.u64()
	case TypeInt64:
		v, err := r.u64()
		return int64(v), err
	case TypeFloat64:
		v, err := r.u64()
		return math.Float64frombits(v), err
	}
	return nil, fmt.Errorf("%w: unknown value type %d at byte %d", ErrInvalid, t, r.c.Pos)
}

func (r *reader) value(t uint32, key string, f *File) (Value, error) {
	if t != TypeArray {
		s, err := r.scalar(t)
		return Value{Type: int(t), Scalar: s}, err
	}
	at, err := r.u32()
	if err != nil {
		return Value{}, err
	}
	if at == TypeArray {
		return Value{}, fmt.Errorf("%w: nested arrays are not supported (key %q)", ErrInvalid, key)
	}
	n, err := r.count()
	if err != nil {
		return Value{}, err
	}
	minSize := scalarSize(at)
	if at == TypeString {
		minSize = 4
		if r.v > 1 {
			minSize = 8
		}
	}
	if minSize == 0 {
		return Value{}, fmt.Errorf("%w: unknown array type %d (key %q)", ErrInvalid, at, key)
	}
	if n > uint64(r.remaining()/minSize) {
		return Value{}, fmt.Errorf("%w: array %q of %d items does not fit in the file", ErrInvalid, key, n)
	}
	v := Value{Type: TypeArray, ArrayType: int(at), Len: int64(n)}
	keepTokens := key == "tokenizer.ggml.tokens" && at == TypeString && n <= maxKeptTokens
	if keepTokens {
		f.Tokens = make([]string, 0, n)
	}
	if at != TypeString && !keepTokens {
		keep := min(n, KeepArray)
		for range keep {
			s, err := r.scalar(at)
			if err != nil {
				return Value{}, err
			}
			v.Items = append(v.Items, s)
		}
		r.c.Skip(int64(n-keep) * minSize)
		if r.c.Pos > r.end {
			return Value{}, fmt.Errorf("%w: array %q runs past the end of the file", ErrInvalid, key)
		}
		return v, nil
	}
	for i := range n {
		s, err := r.str()
		if err != nil {
			return Value{}, err
		}
		if i < KeepArray {
			v.Items = append(v.Items, s)
		}
		if keepTokens {
			f.Tokens = append(f.Tokens, s)
		}
	}
	return v, nil
}

// Read parses a GGUF header from r, a file of size bytes. It reads the
// metadata and tensor infos and never the tensor data.
func Read(r io.ReaderAt, size int64) (*File, error) {
	var magic [4]byte
	if _, err := r.ReadAt(magic[:], 0); err != nil {
		return nil, fmt.Errorf("%w: cannot read the magic", ErrInvalid)
	}
	f := &File{Metadata: map[string]Value{}, Alignment: defaultAlignment}
	rd := &reader{c: rangeio.Cursor{R: r, Pos: 4}, end: size}
	switch string(magic[:]) {
	case "GGUF":
		rd.bo = binary.LittleEndian
	case "FUGG":
		rd.bo = binary.BigEndian
		f.BigEndian = true
	default:
		return nil, fmt.Errorf("%w: bad magic %q", ErrInvalid, magic[:])
	}
	v, err := rd.u32()
	if err != nil {
		return nil, err
	}
	if v < 1 || v > 3 {
		return nil, fmt.Errorf("%w: unsupported version %d", ErrInvalid, v)
	}
	rd.v, f.Version = v, v
	tensors, err := rd.count()
	if err != nil {
		return nil, err
	}
	kvs, err := rd.count()
	if err != nil {
		return nil, err
	}
	// Each key-value pair and tensor info takes at least 12 bytes.
	if tensors > uint64(size/12) || kvs > uint64(size/12) {
		return nil, fmt.Errorf("%w: %d tensors and %d metadata keys do not fit in %d bytes", ErrInvalid, tensors, kvs, size)
	}
	f.TensorCount = tensors
	for range kvs {
		key, err := rd.str()
		if err != nil {
			return nil, err
		}
		t, err := rd.u32()
		if err != nil {
			return nil, err
		}
		val, err := rd.value(t, key, f)
		if err != nil {
			return nil, err
		}
		if _, dup := f.Metadata[key]; dup {
			return nil, fmt.Errorf("%w: duplicate metadata key %q", ErrInvalid, key)
		}
		f.Keys = append(f.Keys, key)
		f.Metadata[key] = val
	}
	if a, ok := f.Int("general.alignment"); ok {
		if a <= 0 || a&(a-1) != 0 || a > 1<<20 {
			return nil, fmt.Errorf("%w: general.alignment %d is not a power of two", ErrInvalid, a)
		}
		f.Alignment = uint64(a)
	}
	f.Tensors = make([]Tensor, 0, tensors)
	for range tensors {
		name, err := rd.str()
		if err != nil {
			return nil, err
		}
		nd, err := rd.u32()
		if err != nil {
			return nil, err
		}
		if nd == 0 || nd > maxDims {
			return nil, fmt.Errorf("%w: tensor %q has %d dimensions", ErrInvalid, name, nd)
		}
		t := Tensor{Name: name, Dims: make([]uint64, nd)}
		for i := range t.Dims {
			if t.Dims[i], err = rd.count(); err != nil {
				return nil, err
			}
		}
		if t.Type, err = rd.u32(); err != nil {
			return nil, err
		}
		if t.Offset, err = rd.u64(); err != nil {
			return nil, err
		}
		f.Tensors = append(f.Tensors, t)
	}
	pos := uint64(rd.c.Pos)
	if rem := pos % f.Alignment; rem != 0 {
		pos += f.Alignment - rem
	}
	f.DataOffset = int64(pos)
	return f, nil
}

// Validate checks the integrity rules in 07-indexing-and-scanning.md: every
// tensor has a known type, an aligned offset, and lies inside the file.
func (f *File) Validate(fileSize int64) error {
	if f.DataOffset > fileSize {
		return fmt.Errorf("%w: the header runs past the end of the file", ErrInvalid)
	}
	data := uint64(fileSize - f.DataOffset)
	for _, t := range f.Tensors {
		n, ok := t.Bytes()
		if !ok {
			return fmt.Errorf("%w: tensor %q has unknown type %d or a shape that does not fit it", ErrInvalid, t.Name, t.Type)
		}
		if t.Offset%f.Alignment != 0 {
			return fmt.Errorf("%w: tensor %q offset %d is not aligned to %d", ErrInvalid, t.Name, t.Offset, f.Alignment)
		}
		if t.Offset > data || n > data-t.Offset {
			return fmt.Errorf("%w: tensor %q at %d (%d bytes) runs past the end of the file", ErrInvalid, t.Name, t.Offset, n)
		}
	}
	return nil
}
