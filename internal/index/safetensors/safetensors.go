// Package safetensors reads and checks safetensors headers: an 8-byte
// little-endian header length, then that many bytes of JSON describing each
// tensor's dtype, shape and byte range in the data section that follows
// (https://github.com/huggingface/safetensors, "Format").
package safetensors

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"sort"
)

// MaxHeaderSize is the largest header Hangar reads: 100 MiB, as the
// safetensors library allows.
const MaxHeaderSize = 100 << 20

// ErrInvalid means the file is not a valid safetensors file.
var ErrInvalid = errors.New("invalid safetensors file")

// Tensor describes one tensor in the header.
type Tensor struct {
	Name  string  `json:"name"`
	DType string  `json:"dtype"`
	Shape []int64 `json:"shape"`
	// Begin and End are byte offsets into the data section.
	Begin int64 `json:"begin"`
	End   int64 `json:"end"`
}

// Elements returns the number of elements in the tensor.
func (t Tensor) Elements() int64 {
	n := int64(1)
	for _, d := range t.Shape {
		n *= d
	}
	return n
}

// Header is a parsed safetensors header.
type Header struct {
	// HeaderSize is the length of the JSON header in bytes.
	HeaderSize int64 `json:"header_size"`
	// Metadata is the optional `__metadata__` string map.
	Metadata map[string]string `json:"metadata,omitempty"`
	// Tensors are sorted by name.
	Tensors []Tensor `json:"tensors"`
	// Raw is the header JSON as stored.
	Raw json.RawMessage `json:"-"`
}

// DataStart is the offset of the data section in the file.
func (h *Header) DataStart() int64 { return 8 + h.HeaderSize }

// Parameters returns element counts per dtype and in total, as the Hub's
// `safetensors.parameters` and `safetensors.total`.
func (h *Header) Parameters() (byDType map[string]int64, total int64) {
	byDType = map[string]int64{}
	for _, t := range h.Tensors {
		n := t.Elements()
		byDType[t.DType] += n
		total += n
	}
	return byDType, total
}

// bitsPerElement gives the storage size of each dtype the safetensors
// library defines (safetensors/src/tensor.rs, Dtype::bitsize).
var bitsPerElement = map[string]int64{
	"BOOL": 8, "U8": 8, "I8": 8, "F8_E5M2": 8, "F8_E4M3": 8, "F8_E8M0": 8,
	"I16": 16, "U16": 16, "F16": 16, "BF16": 16,
	"I32": 32, "U32": 32, "F32": 32, "C64": 64,
	"F64": 64, "I64": 64, "U64": 64,
	"F4": 4, "F6_E2M3": 6, "F6_E3M2": 6,
}

// KnownDType reports whether dtype is one the safetensors format defines.
func KnownDType(dtype string) bool { _, ok := bitsPerElement[dtype]; return ok }

type rawTensor struct {
	DType       string    `json:"dtype"`
	Shape       []int64   `json:"shape"`
	DataOffsets [2]*int64 `json:"data_offsets"`
}

// Read parses the header of a safetensors file through r, which must hold
// at least the header bytes. size is the file size, or -1 if unknown.
func Read(r io.ReaderAt, size int64) (*Header, error) {
	var lenBuf [8]byte
	if _, err := r.ReadAt(lenBuf[:], 0); err != nil {
		return nil, fmt.Errorf("%w: cannot read the header length: %v", ErrInvalid, err)
	}
	n := binary.LittleEndian.Uint64(lenBuf[:])
	if n == 0 || n > MaxHeaderSize {
		return nil, fmt.Errorf("%w: header length %d is not between 1 and %d bytes", ErrInvalid, n, MaxHeaderSize)
	}
	if size >= 0 && int64(n) > size-8 {
		return nil, fmt.Errorf("%w: header length %d is larger than the file", ErrInvalid, n)
	}
	raw := make([]byte, n)
	if _, err := r.ReadAt(raw, 8); err != nil {
		return nil, fmt.Errorf("%w: cannot read the %d-byte header: %v", ErrInvalid, n, err)
	}
	return Parse(raw)
}

// Parse parses header JSON.
func Parse(raw []byte) (*Header, error) {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.TrimSpace(raw)[0] != '{' {
		return nil, fmt.Errorf("%w: the header is not a JSON object", ErrInvalid)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, fmt.Errorf("%w: the header is not valid JSON: %v", ErrInvalid, err)
	}
	h := &Header{HeaderSize: int64(len(raw)), Raw: json.RawMessage(raw)}
	for name, v := range fields {
		if name == "__metadata__" {
			var md map[string]string
			if err := json.Unmarshal(v, &md); err != nil {
				return nil, fmt.Errorf("%w: __metadata__ must map strings to strings", ErrInvalid)
			}
			h.Metadata = md
			continue
		}
		var rt rawTensor
		if err := json.Unmarshal(v, &rt); err != nil {
			return nil, fmt.Errorf("%w: tensor %q: %v", ErrInvalid, name, err)
		}
		if rt.DType == "" || rt.DataOffsets[0] == nil || rt.DataOffsets[1] == nil || rt.Shape == nil {
			return nil, fmt.Errorf("%w: tensor %q needs dtype, shape and data_offsets", ErrInvalid, name)
		}
		elements := big.NewInt(1)
		for _, d := range rt.Shape {
			if d < 0 {
				return nil, fmt.Errorf("%w: tensor %q has a negative dimension", ErrInvalid, name)
			}
			elements.Mul(elements, big.NewInt(d))
		}
		if !elements.IsInt64() || elements.Int64() > math.MaxInt64/64 {
			return nil, fmt.Errorf("%w: tensor %q is too large", ErrInvalid, name)
		}
		h.Tensors = append(h.Tensors, Tensor{Name: name, DType: rt.DType, Shape: rt.Shape, Begin: *rt.DataOffsets[0], End: *rt.DataOffsets[1]})
	}
	sort.Slice(h.Tensors, func(i, j int) bool { return h.Tensors[i].Name < h.Tensors[j].Name })
	return h, nil
}

// Validate checks the integrity rules in 07-indexing-and-scanning.md:
// every dtype is known, each tensor's byte range matches its dtype and
// shape, ranges are in bounds and do not overlap, together they cover the
// whole data section, and the file is exactly as long as the header says.
// fileSize is the stored object's size.
func (h *Header) Validate(fileSize int64) error {
	dataSize := fileSize - h.DataStart()
	if dataSize < 0 {
		return fmt.Errorf("%w: the file is shorter than its header", ErrInvalid)
	}
	ranges := make([]Tensor, len(h.Tensors))
	copy(ranges, h.Tensors)
	for _, t := range ranges {
		bits, ok := bitsPerElement[t.DType]
		if !ok {
			return fmt.Errorf("%w: tensor %q has unknown dtype %q", ErrInvalid, t.Name, t.DType)
		}
		if t.Begin < 0 || t.End < t.Begin {
			return fmt.Errorf("%w: tensor %q has data_offsets [%d, %d]", ErrInvalid, t.Name, t.Begin, t.End)
		}
		if t.End > dataSize {
			return fmt.Errorf("%w: tensor %q ends at %d, past the data section of %d bytes", ErrInvalid, t.Name, t.End, dataSize)
		}
		total := t.Elements() * bits
		if total%8 != 0 {
			return fmt.Errorf("%w: tensor %q does not fill whole bytes", ErrInvalid, t.Name)
		}
		if want := total / 8; t.End-t.Begin != want {
			return fmt.Errorf("%w: tensor %q has %d bytes but its dtype and shape need %d", ErrInvalid, t.Name, t.End-t.Begin, want)
		}
	}
	sort.Slice(ranges, func(i, j int) bool {
		if ranges[i].Begin != ranges[j].Begin {
			return ranges[i].Begin < ranges[j].Begin
		}
		return ranges[i].End < ranges[j].End
	})
	var pos int64
	for _, t := range ranges {
		if t.Begin < pos {
			return fmt.Errorf("%w: tensor %q overlaps the tensor before it", ErrInvalid, t.Name)
		}
		if t.Begin > pos {
			return fmt.Errorf("%w: bytes %d to %d of the data section belong to no tensor", ErrInvalid, pos, t.Begin)
		}
		pos = t.End
	}
	if pos != dataSize {
		return fmt.Errorf("%w: the tensors cover %d bytes but the data section has %d", ErrInvalid, pos, dataSize)
	}
	return nil
}

// DominantDType returns the dtype holding the most elements.
func (h *Header) DominantDType() string {
	by, _ := h.Parameters()
	best, bestN := "", int64(-1)
	for d, n := range by {
		if n > bestN || (n == bestN && d < best) {
			best, bestN = d, n
		}
	}
	return best
}

// Encode builds a safetensors file from tensors laid out in order, with
// zero-filled data. Tests and fixtures use it.
func Encode(metadata map[string]string, tensors []Tensor) ([]byte, error) {
	header := map[string]any{}
	if metadata != nil {
		header["__metadata__"] = metadata
	}
	var pos int64
	for _, t := range tensors {
		bits, ok := bitsPerElement[t.DType]
		if !ok {
			return nil, fmt.Errorf("unknown dtype %q", t.DType)
		}
		n := t.Elements() * bits / 8
		header[t.Name] = map[string]any{"dtype": t.DType, "shape": t.Shape, "data_offsets": []int64{pos, pos + n}}
		pos += n
	}
	raw, err := json.Marshal(header)
	if err != nil {
		return nil, err
	}
	for len(raw)%8 != 0 {
		raw = append(raw, ' ')
	}
	out := make([]byte, 8, 8+len(raw)+int(pos))
	binary.LittleEndian.PutUint64(out, uint64(len(raw)))
	out = append(out, raw...)
	return append(out, make([]byte, pos)...), nil
}
