package scantest

import (
	"archive/zip"
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"strconv"
)

// Pickle writes pickle opcodes, for building benign and malicious test
// fixtures without Python. Nothing here is ever executed.
type Pickle struct {
	b    bytes.Buffer
	memo int
}

// NewPickle starts a pickle. Protocols 2 and later begin with PROTO.
func NewPickle(protocol int) *Pickle {
	p := &Pickle{}
	if protocol >= 2 {
		p.b.Write([]byte{0x80, byte(protocol)})
	}
	return p
}

// Raw appends bytes as they are.
func (p *Pickle) Raw(b ...byte) *Pickle { p.b.Write(b); return p }

// Global writes GLOBAL module name.
func (p *Pickle) Global(module, name string) *Pickle {
	p.b.WriteString("c" + module + "\n" + name + "\n")
	return p
}

// Inst writes INST module name, which pops to the last mark.
func (p *Pickle) Inst(module, name string) *Pickle {
	p.b.WriteString("i" + module + "\n" + name + "\n")
	return p
}

// Unicode writes SHORT_BINUNICODE (protocol 4) or BINUNICODE.
func (p *Pickle) Unicode(s string) *Pickle {
	if len(s) < 256 {
		p.b.WriteByte(0x8c)
		p.b.WriteByte(byte(len(s)))
	} else {
		p.b.WriteByte('X')
		_ = binary.Write(&p.b, binary.LittleEndian, uint32(len(s)))
	}
	p.b.WriteString(s)
	return p
}

// BinUnicode writes BINUNICODE (protocol 1 and later).
func (p *Pickle) BinUnicode(s string) *Pickle {
	p.b.WriteByte('X')
	_ = binary.Write(&p.b, binary.LittleEndian, uint32(len(s)))
	p.b.WriteString(s)
	return p
}

// String writes the protocol 0 STRING opcode.
func (p *Pickle) String(s string) *Pickle {
	p.b.WriteString("S'" + s + "'\n")
	return p
}

// Int writes BININT1 for small numbers, otherwise BININT.
func (p *Pickle) Int(n int) *Pickle {
	if n >= 0 && n < 256 {
		p.b.Write([]byte{'K', byte(n)})
		return p
	}
	p.b.WriteByte('J')
	_ = binary.Write(&p.b, binary.LittleEndian, int32(n))
	return p
}

// BinPut memoizes the top of the stack with BINPUT and returns its index.
func (p *Pickle) BinPut() int {
	idx := p.memo
	p.memo++
	p.b.Write([]byte{'q', byte(idx)})
	return idx
}

// Memoize writes MEMOIZE (protocol 4) and returns the index.
func (p *Pickle) Memoize() int {
	idx := p.memo
	p.memo++
	p.b.WriteByte(0x94)
	return idx
}

// BinGet pushes a memoized value.
func (p *Pickle) BinGet(idx int) *Pickle { p.b.Write([]byte{'h', byte(idx)}); return p }

// Op writes single-byte opcodes such as '(' (MARK), 't' (TUPLE), 'R'
// (REDUCE), 'b' (BUILD), '.' (STOP).
func (p *Pickle) Op(ops string) *Pickle { p.b.WriteString(ops); return p }

// StackGlobal pushes module and name strings, memoizing each, then writes
// STACK_GLOBAL, as protocol 4 pickles do.
func (p *Pickle) StackGlobal(module, name string) *Pickle {
	p.Unicode(module).Memoize()
	p.Unicode(name).Memoize()
	p.b.WriteByte(0x93)
	p.Memoize()
	return p
}

// Bytes returns the pickle.
func (p *Pickle) Bytes() []byte { return append([]byte(nil), p.b.Bytes()...) }

// TorchStateDict returns a protocol 2 pickle shaped like torch.save of a
// state dict with one 2x2 float tensor: only safe imports.
func TorchStateDict() []byte {
	p := NewPickle(2)
	p.Op("}")
	p.BinPut()
	p.Op("(")
	p.BinUnicode("weight")
	p.BinPut()
	p.Global("torch._utils", "_rebuild_tensor_v2")
	p.BinPut()
	p.Op("((")
	p.BinUnicode("storage")
	p.Global("torch", "FloatStorage")
	p.BinUnicode("0").BinUnicode("cpu").Int(4)
	p.Op("tQ")
	p.Int(0)
	p.Int(2).Int(2).Raw(0x86)
	p.Int(2).Int(1).Raw(0x86)
	p.Raw(0x89)
	p.Global("collections", "OrderedDict")
	p.Op(")R")
	p.Op("tRu.")
	return p.Bytes()
}

// Protocol4Benign returns a protocol 4 pickle with framing, STACK_GLOBAL
// and memo references to safe globals.
func Protocol4Benign() []byte {
	p := NewPickle(4)
	p.Raw(0x95, 0, 0, 0, 0, 0, 0, 0, 0)
	p.StackGlobal("collections", "OrderedDict")
	p.Op(")R")
	p.Memoize()
	p.StackGlobal("numpy.core.multiarray", "_reconstruct")
	p.StackGlobal("numpy", "ndarray")
	p.Int(0).Raw(0x85)
	p.Raw('C', 1, 'b')
	p.Raw(0x87)
	p.Op("R")
	p.Op("0.")
	return p.Bytes()
}

// OSSystemProtocol0 is the classic protocol 0 payload:
// os.system("echo pwned").
func OSSystemProtocol0() []byte {
	return NewPickle(0).Global("os", "system").Op("(").String("echo pwned").Op("tR.").Bytes()
}

// PosixSystemStackGlobal hides posix.system behind STACK_GLOBAL and memo
// references.
func PosixSystemStackGlobal() []byte {
	p := NewPickle(4)
	p.Unicode("posix")
	mod := p.Memoize()
	p.Unicode("system")
	name := p.Memoize()
	p.Op("0")
	p.Op("0")
	p.BinGet(mod).BinGet(name)
	p.Raw(0x93)
	p.Unicode("id").Raw(0x85).Op("R.")
	return p.Bytes()
}

// SubprocessInst uses INST to call subprocess.Popen.
func SubprocessInst() []byte {
	return NewPickle(0).Op("(").String("touch /tmp/pwned").Inst("subprocess", "Popen").Op(".").Bytes()
}

// BuiltinsEval calls __builtin__.eval, as a Python 2 pickle names it.
func BuiltinsEval() []byte {
	return NewPickle(1).Global("__builtin__", "eval").Op("(").String("1+1").Op("tR.").Bytes()
}

// PartialOSSystem wraps os.system in functools.partial.
func PartialOSSystem() []byte {
	p := NewPickle(2).Global("functools", "partial")
	p.Global("os", "system").String("echo pwned").Raw(0x86).Op("R)R.")
	return p.Bytes()
}

// PartialSafe wraps a safe callable in functools.partial.
func PartialSafe() []byte {
	return NewPickle(2).Global("functools", "partial").Global("collections", "OrderedDict").Raw(0x85).Op("R.").Bytes()
}

// ComputedStackGlobal builds the STACK_GLOBAL arguments with a call, so
// the import cannot be named.
func ComputedStackGlobal() []byte {
	p := NewPickle(4)
	p.StackGlobal("builtins", "str")
	p.Unicode("o").Raw(0x85).Op("R")
	p.Unicode("system").Raw(0x93).Op(")R.")
	return p.Bytes()
}

// SklearnSuspicious imports a class that is neither allowed nor denied.
func SklearnSuspicious() []byte {
	return NewPickle(2).Global("sklearn.linear_model._base", "LinearRegression").Op(")\x81}b.").Bytes()
}

// LegacyTorch returns a legacy (non-zip) torch.save file: a magic number,
// the protocol, system info, then the object pickle given, then raw
// storage bytes that are not a pickle.
func LegacyTorch(object []byte) []byte {
	var b bytes.Buffer
	magic := NewPickle(2)
	magic.Raw(0x8a, 10, 0x6c, 0xfc, 0x9c, 0x46, 0xf9, 0x20, 0x6a, 0xa8, 0x50, 0x19).Op(".")
	b.Write(magic.Bytes())
	b.Write(NewPickle(2).Int(1001).Op(".").Bytes())
	b.Write(NewPickle(2).Op("}(").BinUnicode("little_endian").Raw(0x88).Op("u.").Bytes())
	b.Write(object)
	b.Write(NewPickle(2).Op("]").BinUnicode("0").Op("a.").Bytes())
	b.Write(bytes.Repeat([]byte{0x00, 0xff}, 64))
	return b.Bytes()
}

// NumpyObjectArray returns a .npy file of an object array whose data is
// the given pickle.
func NumpyObjectArray(payload []byte) []byte {
	return npy("{'descr': '|O', 'fortran_order': False, 'shape': (1,), }", payload)
}

// NumpyFloatArray returns a .npy file of float32 numbers.
func NumpyFloatArray() []byte {
	return npy("{'descr': '<f4', 'fortran_order': False, 'shape': (4,), }", make([]byte, 16))
}

func npy(header string, data []byte) []byte {
	for (10+len(header)+1)%64 != 0 {
		header += " "
	}
	header += "\n"
	var b bytes.Buffer
	b.WriteString("\x93NUMPY\x01\x00")
	_ = binary.Write(&b, binary.LittleEndian, uint16(len(header)))
	b.WriteString(header)
	b.Write(data)
	return b.Bytes()
}

// Joblib returns a joblib file: the pickle, compressed with zlib when
// compress is set.
func Joblib(payload []byte, compress bool) []byte {
	if !compress {
		return payload
	}
	var b bytes.Buffer
	w := zlib.NewWriter(&b)
	_, _ = w.Write(payload)
	_ = w.Close()
	return b.Bytes()
}

// TorchZip returns a zip-based PyTorch archive as torch.save writes it:
// stored members with sizes in the local headers, a data.pkl, a version
// file and a tensor data member of dataSize bytes.
func TorchZip(dataPkl []byte, dataSize int) []byte {
	var b bytes.Buffer
	w := zip.NewWriter(&b)
	add := func(name string, content []byte) {
		h := &zip.FileHeader{Name: name, Method: zip.Store, CRC32: crc32.ChecksumIEEE(content),
			CompressedSize64: uint64(len(content)), UncompressedSize64: uint64(len(content))}
		fw, err := w.CreateRaw(h)
		if err != nil {
			panic(fmt.Sprintf("scantest: zip member %s: %v", name, err))
		}
		_, _ = fw.Write(content)
	}
	add("archive/data.pkl", dataPkl)
	add("archive/byteorder", []byte("little"))
	add("archive/data/0", bytes.Repeat([]byte{0x80, 0x02, 'c'}, dataSize/3+1)[:dataSize])
	add("archive/version", []byte(strconv.Itoa(3)+"\n"))
	if err := w.Close(); err != nil {
		panic(fmt.Sprintf("scantest: zip: %v", err))
	}
	return b.Bytes()
}

// ZipWithDescriptors returns a zip whose pickle member uses a data
// descriptor, as zip.Writer.Create writes them.
func ZipWithDescriptors(dataPkl []byte) []byte {
	var b bytes.Buffer
	w := zip.NewWriter(&b)
	fw, _ := w.CreateHeader(&zip.FileHeader{Name: "archive/data.pkl", Method: zip.Store})
	_, _ = fw.Write(dataPkl)
	_ = w.Close()
	return b.Bytes()
}

// JoblibArray describes one array a joblib pickle stores with
// joblib.numpy_pickle.NumpyArrayWrapper: its NumPy type string, shape and
// the bytes joblib writes to the file after the wrapper is built.
type JoblibArray struct {
	TypeString string
	Shape      []int
	// Align writes numpy_array_alignment_bytes as joblib 1.2 and later
	// do; the data then starts with a padding length byte.
	Align bool
	// Object marks an object array, whose data is a pickle.
	Object bool
	// MmapObject writes allow_mmap True for an object array, which joblib
	// never does.
	MmapObject bool
	Data       []byte
}

// wrapper writes the opcodes that build a NumpyArrayWrapper for a, up to
// and including BUILD, in protocol 2.
func (p *Pickle) wrapper(a JoblibArray) *Pickle {
	p.Global("joblib.numpy_pickle", "NumpyArrayWrapper").Op(")\x81")
	p.Op("}(")
	p.BinUnicode("subclass").Global("numpy", "ndarray")
	p.BinUnicode("shape").Op("(")
	for _, d := range a.Shape {
		p.Int(d)
	}
	p.Op("t")
	p.BinUnicode("order").BinUnicode("C")
	p.BinUnicode("dtype").Global("numpy", "dtype").BinUnicode(a.TypeString).Raw(0x89, 0x88, 0x87).Op("R")
	flags, size := 0, -1
	if a.Object {
		flags = 63
	}
	p.Op("(").Int(3).BinUnicode("|").Op("NNN").Int(size).Int(size).Int(flags).Op("tb")
	if a.Object && !a.MmapObject {
		p.BinUnicode("allow_mmap").Raw(0x89)
	} else {
		p.BinUnicode("allow_mmap").Raw(0x88)
	}
	if a.Align {
		p.BinUnicode("numpy_array_alignment_bytes").Int(16)
	}
	return p.Op("ub")
}

// JoblibPickle returns a protocol 2 pickle shaped like joblib.dump of a
// dict {"a": array}: the wrapper opcodes, the array data joblib writes
// after them, then rest, which must finish the dict and STOP.
func JoblibPickle(a JoblibArray, rest []byte) []byte {
	p := NewPickle(2).Op("}(").BinUnicode("a").wrapper(a)
	p.Raw(a.Data...)
	p.Raw(rest...)
	return p.Bytes()
}

// JoblibHiddenOSSystem hides os.system from a reader that does not know
// joblib's array data: the five bytes of a uint8 array are a BINBYTES
// opcode and length that swallow the call, so pickle.load sees bytes,
// while joblib.load skips the five bytes and runs the call.
func JoblibHiddenOSSystem() []byte {
	hidden := NewPickle(0).Global("os", "system").Op("(").String("echo pwned").Op("tR0u.").Bytes()
	data := append([]byte{'B'}, binary.LittleEndian.AppendUint32(nil, uint32(len(hidden)))...) //nolint:gosec // a short test pickle.
	p := NewPickle(2).Op("}(").BinUnicode("a").wrapper(JoblibArray{TypeString: "u1", Shape: []int{5}})
	p.Raw(data...).Raw(hidden...)
	// After joblib's STOP: what pickle.load reads once the bytes end.
	p.Op("0u.")
	return p.Bytes()
}

// JoblibFramedOSSystem puts a wrapper and os.system in one protocol 4
// frame, with the array data after the frame. joblib's unpickler reads
// opcodes from the frame and array data from the file after it, so it
// runs os.system; a reader that took the array data from the frame would
// skip over the call.
func JoblibFramedOSSystem() []byte {
	inner := NewPickle(0).Op("}(").BinUnicode("a").wrapper(JoblibArray{TypeString: "u1", Shape: []int{4}})
	inner.Global("os", "system").Op("(").String("echo pwned").Op("tR0u.")
	frame := inner.Bytes()
	var b bytes.Buffer
	b.Write([]byte{0x80, 4, 0x95})
	_ = binary.Write(&b, binary.LittleEndian, uint64(len(frame)))
	b.Write(frame)
	b.Write([]byte{0, 0, 0, 0})
	return b.Bytes()
}
