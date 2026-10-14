// Package arrowipc reads Apache Arrow IPC data: the streaming and file
// formats that `datasets` writes as `.arrow` files, and the serialised
// schema pyarrow stores in Parquet files as `ARROW:schema`. It decodes the
// flatbuffer metadata directly, so Hangar needs no Arrow library, and
// returns values in the representation of the features package.
package arrowipc

import (
	"encoding/binary"
	"errors"
	"math"
)

// errCorrupt means the flatbuffer metadata is malformed.
var errCorrupt = errors.New("arrow: malformed flatbuffer metadata")

// table is a flatbuffer table: the buffer and the table's position.
type table struct {
	buf []byte
	pos int
}

func (t table) ok(off, n int) bool { return off >= 0 && n >= 0 && off <= len(t.buf)-n }

func (t table) u32(off int) (uint32, bool) {
	if !t.ok(off, 4) {
		return 0, false
	}
	return binary.LittleEndian.Uint32(t.buf[off:]), true
}

// rootTable returns the table a buffer's root offset points at.
func rootTable(buf []byte) (table, error) {
	t := table{buf: buf}
	off, ok := t.u32(0)
	if !ok || int(off) >= len(buf) {
		return table{}, errCorrupt
	}
	t.pos = int(off)
	if _, ok := t.vtable(); !ok {
		return table{}, errCorrupt
	}
	return t, nil
}

// vtable returns the position of the table's vtable.
func (t table) vtable() (int, bool) {
	if !t.ok(t.pos, 4) {
		return 0, false
	}
	soff := s32(binary.LittleEndian.Uint32(t.buf[t.pos:]))
	vt := t.pos - int(soff)
	if !t.ok(vt, 4) {
		return 0, false
	}
	vsize := int(binary.LittleEndian.Uint16(t.buf[vt:]))
	if vsize < 4 || !t.ok(vt, vsize) {
		return 0, false
	}
	return vt, true
}

// field returns the absolute position of field i, or 0 when absent.
func (t table) field(i int) int {
	vt, ok := t.vtable()
	if !ok {
		return 0
	}
	vsize := int(binary.LittleEndian.Uint16(t.buf[vt:]))
	entry := 4 + 2*i
	if entry+2 > vsize {
		return 0
	}
	off := int(binary.LittleEndian.Uint16(t.buf[vt+entry:]))
	if off == 0 {
		return 0
	}
	return t.pos + off
}

func (t table) i8(i int, def int8) int8 {
	p := t.field(i)
	if p == 0 || !t.ok(p, 1) {
		return def
	}
	return s8(t.buf[p])
}

func (t table) u8(i int, def uint8) uint8 {
	p := t.field(i)
	if p == 0 || !t.ok(p, 1) {
		return def
	}
	return t.buf[p]
}

func (t table) bool(i int, def bool) bool {
	p := t.field(i)
	if p == 0 || !t.ok(p, 1) {
		return def
	}
	return t.buf[p] != 0
}

func (t table) i16(i int, def int16) int16 {
	p := t.field(i)
	if p == 0 || !t.ok(p, 2) {
		return def
	}
	return s16(binary.LittleEndian.Uint16(t.buf[p:]))
}

func (t table) i32(i int, def int32) int32 {
	p := t.field(i)
	if p == 0 || !t.ok(p, 4) {
		return def
	}
	return s32(binary.LittleEndian.Uint32(t.buf[p:]))
}

func (t table) i64(i int, def int64) int64 {
	p := t.field(i)
	if p == 0 || !t.ok(p, 8) {
		return def
	}
	return s64(binary.LittleEndian.Uint64(t.buf[p:]))
}

// indirect follows a uoffset at position p.
func (t table) indirect(p int) (int, bool) {
	off, ok := t.u32(p)
	if !ok {
		return 0, false
	}
	target := p + int(off)
	if off > math.MaxInt32 || target >= len(t.buf) {
		return 0, false
	}
	return target, true
}

// sub returns the table in field i.
func (t table) sub(i int) (table, bool) {
	p := t.field(i)
	if p == 0 {
		return table{}, false
	}
	target, ok := t.indirect(p)
	if !ok {
		return table{}, false
	}
	s := table{buf: t.buf, pos: target}
	if _, ok := s.vtable(); !ok {
		return table{}, false
	}
	return s, true
}

// str returns the string in field i.
func (t table) str(i int) string {
	p := t.field(i)
	if p == 0 {
		return ""
	}
	target, ok := t.indirect(p)
	if !ok {
		return ""
	}
	n, ok := t.u32(target)
	if !ok || !t.ok(target+4, int(n)) {
		return ""
	}
	return string(t.buf[target+4 : target+4+int(n)])
}

// vector returns the start and length of the vector in field i.
func (t table) vector(i int) (start, n int, ok bool) {
	p := t.field(i)
	if p == 0 {
		return 0, 0, true
	}
	target, ok := t.indirect(p)
	if !ok {
		return 0, 0, false
	}
	l, ok := t.u32(target)
	if !ok || int64(l) > int64(len(t.buf)) {
		return 0, 0, false
	}
	return target + 4, int(l), true
}

// tables returns the tables of a vector of tables in field i.
func (t table) tables(i int) ([]table, error) {
	start, n, ok := t.vector(i)
	if !ok || !t.ok(start, 4*n) {
		return nil, errCorrupt
	}
	out := make([]table, 0, n)
	for k := range n {
		target, ok := t.indirect(start + 4*k)
		if !ok {
			return nil, errCorrupt
		}
		s := table{buf: t.buf, pos: target}
		if _, ok := s.vtable(); !ok {
			return nil, errCorrupt
		}
		out = append(out, s)
	}
	return out, nil
}

// structs returns a vector of fixed-size structs in field i as raw bytes.
func (t table) structs(i, size int) ([]byte, int, error) {
	start, n, ok := t.vector(i)
	if !ok || !t.ok(start, size*n) {
		return nil, 0, errCorrupt
	}
	return t.buf[start : start+size*n], n, nil
}

// The helpers below reinterpret little-endian bits as the signed values
// the Arrow and flatbuffer formats define.

func s8(b byte) int8 { return int8(b) } //nolint:gosec // bit reinterpretation

func s16(u uint16) int16 { return int16(u) } //nolint:gosec // bit reinterpretation

func s32(u uint32) int32 { return int32(u) } //nolint:gosec // bit reinterpretation

func s64(u uint64) int64 { return int64(u) } //nolint:gosec // bit reinterpretation
