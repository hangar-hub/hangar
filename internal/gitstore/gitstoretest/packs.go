// Package gitstoretest builds hostile git packs for tests of the limits in
// gitstore.LimitPack and the code paths that use it.
package gitstoretest

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1" //nolint:gosec // git packs are checksummed with SHA-1.
	"encoding/binary"
	"fmt"
)

// Object is one entry of a pack built by Pack.
type Object struct {
	// Type is 1 (commit), 2 (tree), 3 (blob), 4 (tag), 6 (OFS_DELTA) or
	// 7 (REF_DELTA).
	Type byte
	// Size is the size written in the object header. Data is used when
	// Size is negative.
	Size int64
	// Data is the object's content (a delta's instructions for deltas),
	// stored zlib-compressed.
	Data []byte
	// Base is, for an OFS_DELTA, the index of its base in the pack.
	Base int
	// BaseOID is, for a REF_DELTA, its base object ID.
	BaseOID [20]byte
}

// Pack returns a version 2 pack of objs with a valid trailer.
func Pack(objs ...Object) []byte {
	var b bytes.Buffer
	b.WriteString("PACK")
	_ = binary.Write(&b, binary.BigEndian, uint32(2))
	_ = binary.Write(&b, binary.BigEndian, uint32(len(objs)))
	offsets := make([]int, len(objs))
	for i, o := range objs {
		offsets[i] = b.Len()
		size := o.Size
		if size < 0 {
			size = int64(len(o.Data))
		}
		b.Write(objectHeader(o.Type, uint64(size)))
		switch o.Type {
		case 6:
			b.Write(ofsOffset(uint64(offsets[i] - offsets[o.Base])))
		case 7:
			b.Write(o.BaseOID[:])
		}
		b.Write(deflate(o.Data))
	}
	sum := sha1.Sum(b.Bytes()) //nolint:gosec // the pack trailer is SHA-1.
	b.Write(sum[:])
	return b.Bytes()
}

// Blob is a blob object of content.
func Blob(content []byte) Object { return Object{Type: 3, Size: -1, Data: content} }

// CopyDelta is an OFS_DELTA against the object at index base, of baseSize
// bytes, that copies the whole base copies times: a delta bomb whose
// result is baseSize*copies bytes from about 4 bytes per copy. baseSize
// must be at most 0xffffff.
func CopyDelta(base int, baseSize int64, copies int) Object {
	if baseSize <= 0 || baseSize > 0xffffff {
		panic(fmt.Sprintf("gitstoretest: base size %d out of range", baseSize))
	}
	d := append(Varint(uint64(baseSize)), Varint(uint64(baseSize)*uint64(copies))...)
	for range copies {
		// Copy from offset 0, with a three-byte size.
		d = append(d, 0xf0, byte(baseSize), byte(baseSize>>8), byte(baseSize>>16))
	}
	return Object{Type: 6, Size: -1, Data: d, Base: base}
}

// Varint is the little-endian base-128 encoding deltas use for sizes.
func Varint(n uint64) []byte {
	var b []byte
	for {
		c := byte(n & 0x7f)
		n >>= 7
		if n == 0 {
			return append(b, c)
		}
		b = append(b, c|0x80)
	}
}

// PktLine encodes s as a git pkt-line.
func PktLine(s string) string { return fmt.Sprintf("%04x%s", len(s)+4, s) }

// Flush is the flush packet.
const Flush = "0000"

func objectHeader(typ byte, size uint64) []byte {
	c := typ<<4 | byte(size&0x0f)
	size >>= 4
	var b []byte
	for size > 0 {
		b = append(b, c|0x80)
		c = byte(size & 0x7f)
		size >>= 7
	}
	return append(b, c)
}

func ofsOffset(off uint64) []byte {
	b := []byte{byte(off & 0x7f)}
	for off >>= 7; off > 0; off >>= 7 {
		off--
		b = append([]byte{byte(0x80 | off&0x7f)}, b...)
	}
	return b
}

func deflate(p []byte) []byte {
	var b bytes.Buffer
	w, _ := zlib.NewWriterLevel(&b, zlib.BestCompression)
	_, _ = w.Write(p)
	_ = w.Close()
	return b.Bytes()
}
