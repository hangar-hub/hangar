package gguf

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"sort"

	parser "github.com/gpustack/gguf-parser-go"
)

// KV is one metadata pair for Encode. Value may be a string, bool, an
// integer or float type, or a slice of strings, int32 or float32 values.
type KV struct {
	Key   string
	Value any
}

// EncodeTensor is one tensor for Encode, by ggml type name (F32, Q4_0 ...).
type EncodeTensor struct {
	Name string
	Type string
	Dims []uint64
}

// GGMLTypeByName returns the ggml type number of a name such as Q4_K.
func GGMLTypeByName(name string) (uint32, bool) {
	for t := range uint32(64) {
		if parser.GGMLType(t).String() == name {
			if _, ok := parser.GGMLType(t).Trait(); ok {
				return t, true
			}
		}
	}
	return 0, false
}

// Encode builds a version 3 little-endian GGUF file with zero-filled tensor
// data. Tests and fixtures use it to make real, small GGUF files.
func Encode(kvs []KV, tensors []EncodeTensor) ([]byte, error) {
	var b bytes.Buffer
	le := binary.LittleEndian
	w := func(v any) { _ = binary.Write(&b, le, v) }
	str := func(s string) { w(uint64(len(s))); b.WriteString(s) }
	b.WriteString("GGUF")
	w(uint32(3))
	w(uint64(len(tensors)))
	w(uint64(len(kvs)))
	for _, kv := range kvs {
		str(kv.Key)
		switch v := kv.Value.(type) {
		case string:
			w(uint32(TypeString))
			str(v)
		case bool:
			w(uint32(TypeBool))
			if v {
				w(uint8(1))
			} else {
				w(uint8(0))
			}
		case uint8:
			w(uint32(TypeUint8))
			w(v)
		case int32:
			w(uint32(TypeInt32))
			w(v)
		case uint32:
			w(uint32(TypeUint32))
			w(v)
		case int:
			w(uint32(TypeUint32))
			w(uint32(v))
		case uint64:
			w(uint32(TypeUint64))
			w(v)
		case int64:
			w(uint32(TypeInt64))
			w(v)
		case float32:
			w(uint32(TypeFloat32))
			w(math.Float32bits(v))
		case []string:
			w(uint32(TypeArray))
			w(uint32(TypeString))
			w(uint64(len(v)))
			for _, s := range v {
				str(s)
			}
		case []int32:
			w(uint32(TypeArray))
			w(uint32(TypeInt32))
			w(uint64(len(v)))
			for _, n := range v {
				w(n)
			}
		case []float32:
			w(uint32(TypeArray))
			w(uint32(TypeFloat32))
			w(uint64(len(v)))
			for _, n := range v {
				w(math.Float32bits(n))
			}
		default:
			return nil, fmt.Errorf("gguf encode: unsupported value type %T for %q", kv.Value, kv.Key)
		}
	}
	align := uint64(defaultAlignment)
	for _, kv := range kvs {
		if kv.Key == "general.alignment" {
			if a, ok := kv.Value.(uint32); ok && a > 0 {
				align = uint64(a)
			}
		}
	}
	var offset uint64
	for _, t := range tensors {
		typ, ok := GGMLTypeByName(t.Type)
		if !ok {
			return nil, fmt.Errorf("gguf encode: unknown tensor type %q", t.Type)
		}
		str(t.Name)
		w(uint32(len(t.Dims)))
		for _, d := range t.Dims {
			w(d)
		}
		w(typ)
		w(offset)
		n, ok := Tensor{Dims: t.Dims, Type: typ}.Bytes()
		if !ok {
			return nil, fmt.Errorf("gguf encode: tensor %q shape does not fit type %s", t.Name, t.Type)
		}
		offset += n
		if rem := offset % align; rem != 0 {
			offset += align - rem
		}
	}
	for uint64(b.Len())%align != 0 {
		b.WriteByte(0)
	}
	b.Write(make([]byte, offset))
	return b.Bytes(), nil
}

// SortedKeys returns the metadata keys in order, for stable output.
func (f *File) SortedKeys() []string {
	keys := make([]string, 0, len(f.Metadata))
	for k := range f.Metadata {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
