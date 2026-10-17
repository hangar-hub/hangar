// Package pickle finds the imports in a Python pickle without executing
// it (META-2, docs/spec/07-indexing-and-scanning.md "Pickle scanner"). It
// walks the opcode stream of protocols 0 to 5 the way CPython's unpickler
// reads it, simulating just enough of the stack and memo to resolve
// STACK_GLOBAL arguments, and classifies every imported global against the
// lists in lists.go.
//
// Two readings are supported. Scan reads a stream as pickle.load and
// torch.load do. ScanJoblib reads it as joblib.load does: joblib's
// unpickler reads NumPy array data from the file in the middle of the
// opcode stream (joblib.numpy_pickle.NumpyArrayWrapper), so the same bytes
// mean different opcodes to the two loaders, and a file must be checked
// under both.
package pickle

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Classification of an import, from best to worst.
type Classification string

// Classifications.
const (
	Safe       Classification = "safe"
	Suspicious Classification = "suspicious"
	Unsafe     Classification = "unsafe"
)

// ErrInvalid means the stream is not a valid pickle at the point the scan
// stopped. Python's unpickler stops with an error at the same point, so
// nothing after it runs.
var ErrInvalid = errors.New("invalid pickle")

// ErrUnsupported means the scan cannot follow the stream past the point it
// stopped, although an unpickler may: what follows is not checked. A
// caller must not treat the file as checked.
var ErrUnsupported = errors.New("the pickle cannot be scanned in full")

// Limits that bound memory and work on hostile input. Exceeding one stops
// the scan with ErrUnsupported, never with a guess.
const (
	// maxStack bounds the stack. Picklers batch containers in groups of
	// 1000, so real pickles stay far below it.
	maxStack = 1 << 20
	// maxLine is how much of a protocol 0 line is kept; the rest is read
	// and discarded.
	maxLine = 1 << 16
	// maxStrKept is how much of a string argument is kept.
	maxStrKept = 1 << 12
	// maxStackStrings bounds the bytes of strings kept on the stack.
	maxStackStrings = 64 << 20
	// maxMemoStrings bounds the bytes of strings kept in the memo; later
	// strings are remembered as set but not kept.
	maxMemoStrings = 64 << 20
	// maxMemoValues bounds the other values ScanJoblib keeps in the memo.
	maxMemoValues = 1 << 20
	// maxMemoBits bounds memo indexes tracked in the bitset.
	maxMemoBits = 1 << 28
	// maxSparseMemo bounds memo indexes above maxMemoBits.
	maxSparseMemo = 1 << 16
	// maxTracked bounds the items of a tuple, list or dict whose values
	// ScanJoblib tracks.
	maxTracked = 64
	// maxFrame bounds a pickle.py frame, which ScanJoblib holds in memory
	// as joblib's unpickler does. Picklers write frames of about 64 KiB.
	maxFrame = 64 << 20
	// maxNested bounds pickles nested in joblib object arrays.
	maxNested = 1 << 12
)

// Import is one global a pickle imports.
type Import struct {
	Module string
	Name   string
	// Opcode is GLOBAL, STACK_GLOBAL or INST.
	Opcode         string
	Classification Classification
}

// Global returns `module.name`.
func (i Import) Global() string { return i.Module + "." + i.Name }

// Result is what Scan found.
type Result struct {
	Imports []Import
	// Pickles is how many pickles the stream held back to back.
	Pickles int
	// NotPickle is set when the data failed at its first opcode: it is not
	// a pickle, and an unpickler runs nothing.
	NotPickle bool
	// Joblib is set when the stream imports a global that joblib's
	// unpickler treats specially, so the stream must also be read with
	// ScanJoblib.
	Joblib bool
}

// Worst returns the worst classification among the imports, or Safe.
func (r Result) Worst() Classification {
	worst := Safe
	for _, i := range r.Imports {
		if rank(i.Classification) > rank(worst) {
			worst = i.Classification
		}
	}
	return worst
}

func rank(c Classification) int {
	switch c {
	case Suspicious:
		return 1
	case Unsafe:
		return 2
	}
	return 0
}

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
}

func unsupported(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrUnsupported, fmt.Sprintf(format, a...))
}

// joblibMarkers name the globals and strings that make joblib's unpickler
// read array data from the file: any import or string that mentions one
// may lead to a NumpyArrayWrapper.
var joblibMarkers = []string{"joblib", "NumpyArrayWrapper", "NDArrayWrapper"}

func mentionsJoblib(s string) bool {
	for _, m := range joblibMarkers {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// Values on the simulated stack.

type valueKind uint8

const (
	kindOther valueKind = iota
	kindMark
	kindString
	kindInt
	kindNone
	kindTuple
	kindList
	kindDict
	kindGlobal
	kindObject
)

type value struct {
	kind valueKind
	// taint is set on a value that may be, or hold, a NumpyArrayWrapper
	// (ScanJoblib only).
	taint bool
	// trunc is set on a string that was cut to maxStrKept, or on an
	// unknown string (kindOther) whose content was not kept.
	trunc bool
	// big is set on an int that does not fit in int64.
	big bool
	// s is a string, or a global's module.
	s string
	// name is a global's name.
	name  string
	n     int64
	items []value
	ref   *ref
}

// ref is a mutable Python object: a list, a dict or an instance, shared
// by every stack slot and memo entry that holds it.
type ref struct {
	taint bool
	// items holds a list's items; partial is set when some were not kept.
	items []value
	// keys holds a dict's string-keyed items, or an instance's attributes.
	keys    map[string]value
	partial bool
	// class is the callable that made an instance.
	class value
	// called is set when the instance was made by calling its class with
	// args, which runs __init__; otherwise only __new__ ran.
	called bool
	args   []value
	// state is the last BUILD state of an instance with __setstate__
	// (numpy.dtype).
	state *value
	built bool
}

func (v value) tainted() bool { return v.taint || (v.ref != nil && v.ref.taint) }

func other(taint bool) value { return value{kind: kindOther, taint: taint} }

// Memo.

// memo tracks which indexes are set, exactly, so MEMOIZE numbers entries as
// the unpickler does, and keeps string values (and, for ScanJoblib, other
// tracked values) within a budget.
type memo struct {
	bits    []uint64
	sparse  map[int64]bool
	count   int64
	values  map[int64]value
	strings int64
	// others counts kept values that are not strings (ScanJoblib).
	others int64
}

func (m *memo) set(idx int64) (bool, error) {
	if idx < maxMemoBits {
		w := idx / 64
		if int(w) >= len(m.bits) {
			m.bits = slices.Grow(m.bits, int(w)+1-len(m.bits))[:w+1]
		}
		bit := uint64(1) << (idx % 64)
		was := m.bits[w]&bit != 0
		m.bits[w] |= bit
		return was, nil
	}
	if m.sparse == nil {
		m.sparse = map[int64]bool{}
	}
	if m.sparse[idx] {
		return true, nil
	}
	if len(m.sparse) >= maxSparseMemo {
		return false, unsupported("too many memo entries above index %d", maxMemoBits)
	}
	m.sparse[idx] = true
	return false, nil
}

func (m *memo) has(idx int64) bool {
	if idx < maxMemoBits {
		w := idx / 64
		return int(w) < len(m.bits) && m.bits[w]&(uint64(1)<<(idx%64)) != 0
	}
	return m.sparse[idx]
}

// Machine.

type machine struct {
	in *input
	// joblib selects joblib's reading: pickle.py frames, rich values and
	// NumpyArrayWrapper array data.
	joblib bool
	stack  []value
	// stackStrings is the bytes of strings on the stack.
	stackStrings int64
	memo         memo
	imports      []Import
	// ops counts opcodes completed in the current pickle.
	ops    int64
	nested int
	sawJob bool
}

func newMachine(in *input, joblib bool) *machine {
	return &machine{in: in, joblib: joblib}
}

func (m *machine) reset() {
	m.stack = m.stack[:0]
	m.stackStrings = 0
	m.memo = memo{values: map[int64]value{}}
	m.ops = 0
}

func (m *machine) push(v value) error {
	if len(m.stack) >= maxStack {
		return unsupported("the stack is deeper than %d", maxStack)
	}
	if v.kind == kindString {
		if m.stackStrings+int64(len(v.s)) > maxStackStrings {
			v = value{kind: kindOther, trunc: true, taint: v.taint}
		} else {
			m.stackStrings += int64(len(v.s))
		}
	}
	m.stack = append(m.stack, v)
	return nil
}

func (m *machine) pop() (value, error) {
	if len(m.stack) == 0 {
		return value{}, invalid("stack underflow")
	}
	v := m.stack[len(m.stack)-1]
	m.stack = m.stack[:len(m.stack)-1]
	if v.kind == kindString {
		m.stackStrings -= int64(len(v.s))
	}
	return v, nil
}

func (m *machine) popN(n int) ([]value, error) {
	if len(m.stack) < n {
		return nil, invalid("stack underflow")
	}
	out := make([]value, n)
	for i := n - 1; i >= 0; i-- {
		v, _ := m.pop()
		out[i] = v
	}
	return out, nil
}

// popMark pops everything down to and including the topmost mark and
// returns what was above it.
func (m *machine) popMark() ([]value, error) {
	for i := len(m.stack) - 1; i >= 0; i-- {
		if m.stack[i].kind == kindMark {
			items := slices.Clone(m.stack[i+1:])
			for len(m.stack) > i {
				_, _ = m.pop()
			}
			return items, nil
		}
	}
	return nil, invalid("no mark on the stack")
}

func (m *machine) top() (value, error) {
	if len(m.stack) == 0 {
		return value{}, invalid("stack underflow")
	}
	return m.stack[len(m.stack)-1], nil
}

func (m *machine) setTop(v value) {
	old := m.stack[len(m.stack)-1]
	if old.kind == kindString {
		m.stackStrings -= int64(len(old.s))
	}
	m.stack = m.stack[:len(m.stack)-1]
	_ = m.push(v)
}

func (m *machine) str(s string, trunc bool) value {
	v := value{kind: kindString, s: s, trunc: trunc}
	if m.joblib && mentionsJoblib(s) {
		v.taint = true
	}
	return v
}

func (m *machine) memoPut(idx int64) error {
	if idx < 0 {
		return invalid("negative memo index")
	}
	v, err := m.top()
	if err != nil {
		return err
	}
	was, err := m.memo.set(idx)
	if err != nil {
		return err
	}
	if !was {
		m.memo.count++
	}
	keep := v.kind == kindString || (m.joblib && v.kind != kindOther)
	switch {
	case keep && v.kind == kindString:
		if m.memo.strings+int64(len(v.s)) > maxMemoStrings {
			keep = false
		} else {
			m.memo.strings += int64(len(v.s))
		}
	case keep:
		if m.memo.others >= maxMemoValues {
			keep = false
		} else {
			m.memo.others++
		}
	}
	switch {
	case keep:
		m.memo.values[idx] = v
	case v.kind == kindOther && !v.trunc && (!m.joblib || !v.tainted()):
		delete(m.memo.values, idx)
	default:
		// A string or value that could not be kept: it reads back as an
		// unknown value, which ScanJoblib treats as tainted.
		m.memo.values[idx] = value{kind: kindOther, trunc: true, taint: m.joblib}
	}
	return nil
}

func (m *machine) memoGet(idx int64) error {
	if !m.memo.has(idx) {
		return invalid("memo index %d is not set", idx)
	}
	v, ok := m.memo.values[idx]
	if !ok {
		v = other(false)
	}
	return m.push(v)
}

func (m *machine) addImport(module, name, opcode string, trunc bool) value {
	c := Classify(module, name)
	if trunc {
		// Only the start of the name was kept, so it cannot be told apart
		// from a dangerous one.
		c = Unsafe
	}
	m.imports = append(m.imports, Import{Module: module, Name: name, Opcode: opcode, Classification: c})
	g := value{kind: kindGlobal, s: module, name: name}
	if mentionsJoblib(module) || mentionsJoblib(name) {
		m.sawJob = true
		g.taint = m.joblib
	}
	return g
}

// pythonInt parses a protocol 0 integer argument as Python's int() does:
// surrounding whitespace, a sign, digits with single underscores between
// them, and leading zeros.
func pythonInt(s string) (int64, bool, error) {
	t := strings.TrimSuffix(strings.TrimSpace(s), "L")
	neg := false
	switch {
	case strings.HasPrefix(t, "-"):
		neg, t = true, t[1:]
	case strings.HasPrefix(t, "+"):
		t = t[1:]
	}
	if t == "" || t[0] == '_' || t[len(t)-1] == '_' || strings.Contains(t, "__") {
		return 0, false, invalid("bad integer %q", s)
	}
	digits := strings.ReplaceAll(t, "_", "")
	for _, c := range digits {
		if c < '0' || c > '9' {
			return 0, false, invalid("bad integer %q", s)
		}
	}
	var b big.Int
	b.SetString(digits, 10)
	if neg {
		b.Neg(&b)
	}
	if !b.IsInt64() {
		return 0, true, nil
	}
	return b.Int64(), false, nil
}

// lineInt reads a protocol 0 line holding an integer.
func (m *machine) lineInt() (int64, bool, error) {
	s, trunc, err := m.in.line()
	if err != nil {
		return 0, false, err
	}
	if trunc {
		return 0, true, nil
	}
	return pythonInt(s)
}

// longValue decodes a LONG1 or LONG4 argument: little-endian two's
// complement.
func longValue(b string, trunc bool) value {
	if trunc || len(b) > 8 {
		return value{kind: kindInt, big: true}
	}
	if len(b) == 0 {
		return value{kind: kindInt}
	}
	var u uint64
	for i := len(b) - 1; i >= 0; i-- {
		u = u<<8 | uint64(b[i])
	}
	shift := uint(64 - 8*len(b))
	return value{kind: kindInt, n: int64(u<<shift) >> shift} //nolint:gosec // two's complement sign extension.
}

// rawUnicodeEscape decodes a protocol 0 UNICODE argument as Python's
// raw-unicode-escape codec does: only \uXXXX and \UXXXXXXXX are escapes.
func rawUnicodeEscape(s string) (string, error) {
	if !strings.Contains(s, `\`) {
		return s, nil
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] != '\\' {
			b.WriteByte(s[i])
			i++
			continue
		}
		// An odd run of backslashes before u or U starts an escape.
		j := i
		for j < len(s) && s[j] == '\\' {
			j++
		}
		run := j - i
		if j < len(s) && (s[j] == 'u' || s[j] == 'U') && run%2 == 1 {
			b.WriteString(strings.Repeat(`\`, run-1))
			width := 4
			if s[j] == 'U' {
				width = 8
			}
			if j+1+width > len(s) {
				return "", invalid("truncated \\%c escape", s[j])
			}
			r, err := strconv.ParseUint(s[j+1:j+1+width], 16, 32)
			if err != nil || r > utf8.MaxRune {
				return "", invalid("bad \\%c escape", s[j])
			}
			b.WriteRune(rune(r))
			i = j + 1 + width
			continue
		}
		b.WriteString(s[i:j])
		i = j
	}
	return b.String(), nil
}

// escapeDecode decodes a protocol 0 STRING argument as Python's
// codecs.escape_decode does, after the quotes are stripped.
func escapeDecode(s string) (string, error) {
	if !strings.Contains(s, `\`) {
		return s, nil
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '\\' {
			b.WriteByte(c)
			continue
		}
		i++
		if i >= len(s) {
			return "", invalid("trailing \\ in string")
		}
		switch c = s[i]; c {
		case '\n':
		case '\\', '\'', '"':
			b.WriteByte(c)
		case 'a':
			b.WriteByte('\a')
		case 'b':
			b.WriteByte('\b')
		case 'f':
			b.WriteByte('\f')
		case 'n':
			b.WriteByte('\n')
		case 'r':
			b.WriteByte('\r')
		case 't':
			b.WriteByte('\t')
		case 'v':
			b.WriteByte('\v')
		case '0', '1', '2', '3', '4', '5', '6', '7':
			n := int(c - '0')
			for k := 0; k < 2 && i+1 < len(s) && s[i+1] >= '0' && s[i+1] <= '7'; k++ {
				i++
				n = n*8 + int(s[i]-'0')
			}
			b.WriteByte(byte(n & 0xff))
		case 'x':
			if i+2 >= len(s) {
				return "", invalid("truncated \\x escape")
			}
			n, err := strconv.ParseUint(s[i+1:i+3], 16, 8)
			if err != nil {
				return "", invalid("bad \\x escape")
			}
			b.WriteByte(byte(n))
			i += 2
		default:
			b.WriteByte('\\')
			b.WriteByte(c)
		}
	}
	return b.String(), nil
}

// stringArg decodes a protocol 0 STRING line: quoted with matching quotes,
// as pickle requires, then escape-decoded.
func stringArg(s string) (string, error) {
	if len(s) < 2 || (s[0] != '\'' && s[0] != '"') || s[len(s)-1] != s[0] {
		return "", invalid("the STRING opcode argument must be quoted")
	}
	return escapeDecode(s[1 : len(s)-1])
}

// tuple returns a tuple value of items.
func (m *machine) tuple(items []value) value {
	if !m.joblib {
		return value{kind: kindOther}
	}
	v := value{kind: kindTuple}
	for _, it := range items {
		v.taint = v.taint || it.tainted()
	}
	if len(items) > maxTracked {
		return other(v.taint)
	}
	v.items = items
	return v
}

func (m *machine) newList() value {
	if !m.joblib {
		return value{kind: kindOther}
	}
	return value{kind: kindList, ref: &ref{}}
}

func (m *machine) newDict() value {
	if !m.joblib {
		return value{kind: kindOther}
	}
	return value{kind: kindDict, ref: &ref{keys: map[string]value{}}}
}

// appendItems adds items to the list (or other target) under them.
func (m *machine) appendItems(target value, items []value) {
	if !m.joblib {
		return
	}
	r := target.ref
	if r == nil {
		return
	}
	for _, it := range items {
		r.taint = r.taint || it.tainted()
		if target.kind == kindList && len(r.items) < maxTracked {
			r.items = append(r.items, it)
		} else {
			r.partial = true
		}
	}
}

// setItems sets key and value pairs on the dict (or other target) under
// them.
func (m *machine) setItems(target value, kv []value) {
	if !m.joblib {
		return
	}
	r := target.ref
	if r == nil {
		return
	}
	for i := 0; i+1 < len(kv); i += 2 {
		k, v := kv[i], kv[i+1]
		r.taint = r.taint || k.tainted() || v.tainted()
		if target.kind == kindDict && k.kind == kindString && !k.trunc && len(r.keys) < maxTracked {
			r.keys[k.s] = v
		} else {
			r.partial = true
		}
	}
}

// call returns the result of calling callable with args (REDUCE, NEWOBJ,
// INST, OBJ). called is false for NEWOBJ, which runs only __new__.
func (m *machine) call(callable, args value, called bool) value {
	if !m.joblib {
		return value{kind: kindOther}
	}
	r := &ref{taint: callable.tainted() || args.tainted(), class: callable, called: called, keys: map[string]value{}}
	if args.kind == kindTuple {
		r.args = args.items
	} else {
		r.partial = true
	}
	// copyreg.__newobj__(cls, *args), copyreg.__newobj_ex__(cls, args,
	// kwargs) and copyreg._reconstructor(cls, base, state) make an
	// instance of cls without running cls.__init__.
	if callable.kind == kindGlobal && isCopyregNew(callable) && len(r.args) > 0 && r.args[0].kind == kindGlobal {
		r.class, r.called, r.args = r.args[0], false, nil
	}
	return value{kind: kindObject, ref: r}
}

func isCopyregNew(g value) bool {
	mod := normaliseModule(g.s)
	return mod == "copyreg" && (g.name == "__newobj__" || g.name == "__newobj_ex__" || g.name == "_reconstructor")
}

// isWrapper reports whether a global names joblib's NumpyArrayWrapper,
// which reads array data from the file when it is built.
func isWrapper(g value) bool {
	if g.kind != kindGlobal {
		return false
	}
	last := g.name[strings.LastIndex(g.name, ".")+1:]
	return last == "NumpyArrayWrapper"
}

// isCompatWrapper reports whether a global names one of joblib's old
// NDArrayWrapper classes, which read a separate file and nothing from the
// stream.
func isCompatWrapper(g value) bool {
	if g.kind != kindGlobal {
		return false
	}
	last := g.name[strings.LastIndex(g.name, ".")+1:]
	return last == "NDArrayWrapper" || last == "ZNDArrayWrapper"
}

// build applies a BUILD state to inst, as pickle.py's load_build does, and
// for ScanJoblib reads the array data joblib's load_build reads.
func (m *machine) build(state value) error {
	inst, err := m.top()
	if err != nil {
		return err
	}
	if !m.joblib {
		return nil
	}
	r := inst.ref
	if r == nil {
		if inst.tainted() {
			return unsupported("BUILD on a value that may be a NumpyArrayWrapper")
		}
		return nil
	}
	if state.tainted() {
		r.taint = true
	}
	if inst.kind != kindObject {
		// A list, dict or set is never a NumpyArrayWrapper.
		return nil
	}
	if _, ok := r.keys["__setstate__"]; ok {
		return unsupported("BUILD on an instance with a __setstate__ attribute")
	}
	if isDType(r.class) {
		s := state
		r.state, r.built = &s, true
		return nil
	}
	// No __setstate__: the state updates the instance's attributes.
	merge := func(s value) error {
		switch s.kind {
		case kindNone:
			return nil
		case kindDict:
			if s.ref.partial {
				r.partial = true
			}
			for k, v := range s.ref.keys {
				if strings.HasPrefix(k, "__") {
					return unsupported("BUILD sets the attribute %s", k)
				}
				r.keys[k] = v
			}
			return nil
		}
		r.partial = true
		return nil
	}
	if state.kind == kindTuple && len(state.items) == 2 {
		if err := merge(state.items[0]); err != nil {
			return err
		}
		if err := merge(state.items[1]); err != nil {
			return err
		}
	} else if err := merge(state); err != nil {
		return err
	}
	r.built = true
	switch {
	case isWrapper(r.class):
		arr, err := m.readArray(r)
		if err != nil {
			return err
		}
		m.setTop(arr)
	case isCompatWrapper(r.class):
		m.setTop(other(false))
	case r.taint:
		return unsupported("BUILD on a value that may be a NumpyArrayWrapper")
	}
	return nil
}

func isDType(g value) bool {
	if g.kind != kindGlobal || g.name != "dtype" {
		return false
	}
	switch g.s {
	case "numpy", "numpy.core.multiarray", "numpy._core.multiarray":
		return true
	}
	return false
}

// dtypeInfo returns the itemsize and hasobject of a numpy.dtype instance,
// as NumPy computes them after __setstate__.
func dtypeInfo(v value) (int64, bool, error) {
	if v.kind != kindObject || v.ref == nil || !isDType(v.ref.class) || !v.ref.called || len(v.ref.args) == 0 || v.ref.args[0].kind != kindString {
		return 0, false, unsupported("a NumpyArrayWrapper dtype that is not a numpy.dtype made from a type string")
	}
	kind, size, err := parseTypeString(v.ref.args[0].s)
	if err != nil {
		return 0, false, err
	}
	hasObject := kind == 'O'
	if v.ref.state != nil {
		st := *v.ref.state
		if st.kind != kindTuple || (len(st.items) != 8 && len(st.items) != 9) || st.items[0].kind != kindInt ||
			(st.items[0].n != 3 && st.items[0].n != 4) || st.items[5].kind != kindInt || st.items[7].kind != kindInt ||
			st.items[5].big || st.items[7].big {
			return 0, false, unsupported("a numpy.dtype state the scanner does not read")
		}
		hasObject = st.items[7].n&1 != 0
		if kind == 'S' || kind == 'U' || kind == 'V' {
			if st.items[5].n < 0 {
				return 0, false, unsupported("a numpy.dtype with a negative size")
			}
			size = st.items[5].n
		}
	}
	return size, hasObject, nil
}

// parseTypeString returns the kind and itemsize of a NumPy type string as
// numpy.dtype's pickles write it, such as f8, <i4, U2 or V12. Sizes that
// depend on the platform are refused.
func parseTypeString(s string) (byte, int64, error) {
	t := strings.TrimLeft(s, "<>|=")
	if i := strings.IndexByte(t, '['); i >= 0 && strings.HasSuffix(t, "]") {
		t = t[:i]
	}
	bad := unsupported("the NumPy type %q", s)
	if t == "" {
		return 0, 0, bad
	}
	kind, digits := t[0], t[1:]
	n := int64(-1)
	if digits != "" {
		v, err := strconv.ParseInt(digits, 10, 64)
		if err != nil || v < 0 || v > 1<<40 {
			return 0, 0, bad
		}
		n = v
	}
	sizes := map[byte][]int64{'b': {1}, 'i': {1, 2, 4, 8}, 'u': {1, 2, 4, 8}, 'f': {2, 4, 8}, 'c': {8, 16}, 'm': {8}, 'M': {8}, 'O': {8}}
	switch kind {
	case 'S', 'V':
		if n < 0 {
			n = 0
		}
		return kind, n, nil
	case 'U':
		if n < 0 {
			n = 0
		}
		return kind, 4 * n, nil
	case 'O':
		if n < 0 {
			n = 8
		}
	}
	if !slices.Contains(sizes[kind], n) {
		return 0, 0, bad
	}
	return kind, n, nil
}

// readArray reads what NumpyArrayWrapper.read reads from the file after
// BUILD: a nested pickle for object arrays, otherwise a padding byte,
// padding and the array bytes. It returns the array value.
func (m *machine) readArray(r *ref) (value, error) {
	if r.called {
		return value{}, unsupported("a NumpyArrayWrapper made by calling it")
	}
	attr := func(k string) (value, bool, error) {
		v, ok := r.keys[k]
		if !ok && r.partial {
			return value{}, false, unsupported("NumpyArrayWrapper attributes the scanner did not track")
		}
		return v, ok, nil
	}
	shape, ok, err := attr("shape")
	if err != nil {
		return value{}, err
	}
	if !ok {
		return value{}, invalid("NumpyArrayWrapper has no shape")
	}
	dt, ok, err := attr("dtype")
	if err != nil {
		return value{}, err
	}
	if !ok {
		return value{}, invalid("NumpyArrayWrapper has no dtype")
	}
	var dims []value
	switch {
	case shape.kind == kindTuple:
		dims = shape.items
	case shape.kind == kindList && !shape.ref.partial:
		dims = shape.ref.items
	default:
		return value{}, unsupported("a NumpyArrayWrapper shape the scanner does not read")
	}
	count := int64(1)
	overflow := false
	for _, d := range dims {
		if d.kind != kindInt || d.big {
			return value{}, unsupported("a NumpyArrayWrapper shape the scanner does not read")
		}
		hi, lo := mulInt64(count, d.n)
		if hi {
			overflow = true
		}
		count = lo
	}
	itemsize, hasObject, err := dtypeInfo(dt)
	if err != nil {
		return value{}, err
	}
	if hasObject {
		mm, ok := r.keys["allow_mmap"]
		if ok && !falsy(mm) {
			// With mmap_mode, joblib maps the bytes instead of unpickling
			// them, so the two readings part ways.
			return value{}, unsupported("an object array that joblib may memory-map")
		}
		return m.nestedPickle()
	}
	if count < 0 {
		return value{}, invalid("negative array size")
	}
	if itemsize == 0 {
		return value{}, invalid("array of zero-size items")
	}
	if align, ok := r.keys["numpy_array_alignment_bytes"]; ok && align.kind != kindNone {
		var pad [1]byte
		n, err := m.in.fileRead(pad[:])
		if err != nil {
			return value{}, err
		}
		if n == 1 && pad[0] != 0 {
			if err := m.in.fileSkip(uint64(pad[0]), false); err != nil {
				return value{}, err
			}
		}
	}
	total := new(big.Int).Mul(big.NewInt(count), big.NewInt(itemsize))
	if !total.IsUint64() || total.Uint64() > math.MaxInt64 {
		return value{}, invalid("unexpected end of data in array data")
	}
	if err := m.in.fileSkip(total.Uint64(), true); err != nil {
		return value{}, err
	}
	if overflow {
		return value{}, invalid("the array shape overflows")
	}
	return other(false), nil
}

func falsy(v value) bool {
	switch v.kind {
	case kindNone:
		return true
	case kindInt:
		return !v.big && v.n == 0
	}
	return false
}

// mulInt64 multiplies as NumPy's int64 does, wrapping, and reports
// whether it overflowed.
func mulInt64(a, b int64) (bool, int64) {
	p := new(big.Int).Mul(big.NewInt(a), big.NewInt(b))
	return !p.IsInt64(), a * b
}

// nestedPickle reads the pickle.load(file) that joblib runs for an object
// array: CPython's unpickler reading the file from where it is.
func (m *machine) nestedPickle() (value, error) {
	if m.nested >= maxNested {
		return value{}, unsupported("more than %d nested pickles", maxNested)
	}
	m.nested++
	sub := newMachine(&input{file: m.in.file}, false)
	sub.reset()
	err := sub.run()
	m.imports = append(m.imports, sub.imports...)
	m.sawJob = m.sawJob || sub.sawJob
	if err != nil {
		return value{}, err
	}
	// The objects of the array come from another unpickler; if they may
	// hold a NumpyArrayWrapper, so may the array.
	return other(sub.sawJob), nil
}

// run parses one pickle, up to and including its STOP opcode.
func (m *machine) run() error {
	for {
		op, err := m.in.readByte()
		if err != nil {
			return invalid("unexpected end of data before STOP")
		}
		if op == '.' { // STOP
			return nil
		}
		if err := m.step(op); err != nil {
			return err
		}
		m.ops++
	}
}

//nolint:gocyclo // one case per opcode, as in pickle.py.
func (m *machine) step(op byte) error {
	in := m.in
	switch op {
	case '(': // MARK
		return m.push(value{kind: kindMark})
	case '0': // POP
		_, err := m.pop()
		return err
	case '1': // POP_MARK
		_, err := m.popMark()
		return err
	case '2': // DUP
		v, err := m.top()
		if err != nil {
			return err
		}
		return m.push(v)
	case 'I', 'L': // INT, LONG
		// pickle reads these with int(x, 0), which takes forms such as 0x10
		// that the scan does not decode; those push an unknown value.
		s, trunc, err := in.line()
		if err != nil {
			return err
		}
		n, big, err := pythonInt(s)
		if err != nil || trunc {
			return m.push(other(false))
		}
		return m.push(value{kind: kindInt, n: n, big: big})
	case 'F': // FLOAT
		if _, _, err := in.line(); err != nil {
			return err
		}
		return m.push(other(false))
	case 'S': // STRING
		s, trunc, err := in.line()
		if err != nil {
			return err
		}
		if trunc {
			return m.push(m.str(s, true))
		}
		d, err := stringArg(s)
		if err != nil {
			return err
		}
		return m.push(m.str(d, false))
	case 'V': // UNICODE
		s, trunc, err := in.line()
		if err != nil {
			return err
		}
		if trunc {
			return m.push(m.str(s, true))
		}
		d, err := rawUnicodeEscape(s)
		if err != nil {
			return err
		}
		return m.push(m.str(d, false))
	case 'P': // PERSID
		if _, _, err := in.line(); err != nil {
			return err
		}
		return m.push(other(false))
	case 'Q': // BINPERSID
		if _, err := m.pop(); err != nil {
			return err
		}
		return m.push(other(false))
	case 'J': // BININT
		b, err := in.read(4)
		if err != nil {
			return err
		}
		return m.push(value{kind: kindInt, n: int64(int32(binary.LittleEndian.Uint32(b)))}) //nolint:gosec // a signed 32-bit argument.
	case 'K': // BININT1
		b, err := in.read(1)
		if err != nil {
			return err
		}
		return m.push(value{kind: kindInt, n: int64(b[0])})
	case 'M': // BININT2
		b, err := in.read(2)
		if err != nil {
			return err
		}
		return m.push(value{kind: kindInt, n: int64(binary.LittleEndian.Uint16(b))})
	case 'G': // BINFLOAT
		if _, err := in.read(8); err != nil {
			return err
		}
		return m.push(other(false))
	case 'N': // NONE
		return m.push(value{kind: kindNone})
	case 0x88: // NEWTRUE
		return m.push(value{kind: kindInt, n: 1})
	case 0x89: // NEWFALSE
		return m.push(value{kind: kindInt, n: 0})
	case 'T', 'X': // BINSTRING, BINUNICODE
		s, trunc, err := in.lenData(4)
		if err != nil {
			return err
		}
		return m.push(m.str(s, trunc))
	case 'U', 0x8c: // SHORT_BINSTRING, SHORT_BINUNICODE
		s, trunc, err := in.lenData(1)
		if err != nil {
			return err
		}
		return m.push(m.str(s, trunc))
	case 0x8d: // BINUNICODE8
		s, trunc, err := in.lenData(8)
		if err != nil {
			return err
		}
		return m.push(m.str(s, trunc))
	case 'B': // BINBYTES
		if _, _, err := in.lenData(4); err != nil {
			return err
		}
		return m.push(other(false))
	case 'C': // SHORT_BINBYTES
		if _, _, err := in.lenData(1); err != nil {
			return err
		}
		return m.push(other(false))
	case 0x8e, 0x96: // BINBYTES8, BYTEARRAY8
		if _, _, err := in.lenData(8); err != nil {
			return err
		}
		return m.push(other(false))
	case 0x8a, 0x8b: // LONG1, LONG4
		width := 1
		if op == 0x8b {
			width = 4
		}
		s, trunc, err := in.lenData(width)
		if err != nil {
			return err
		}
		return m.push(longValue(s, trunc))
	case 'a': // APPEND
		v, err := m.pop()
		if err != nil {
			return err
		}
		t, err := m.top()
		if err != nil {
			return err
		}
		m.appendItems(t, []value{v})
		return nil
	case 'e': // APPENDS
		items, err := m.popMark()
		if err != nil {
			return err
		}
		t, err := m.top()
		if err != nil {
			return err
		}
		m.appendItems(t, items)
		return nil
	case 0x90: // ADDITEMS
		items, err := m.popMark()
		if err != nil {
			return err
		}
		t, err := m.top()
		if err != nil {
			return err
		}
		m.appendItems(t, items)
		return nil
	case 's': // SETITEM
		kv, err := m.popN(2)
		if err != nil {
			return err
		}
		t, err := m.top()
		if err != nil {
			return err
		}
		m.setItems(t, kv)
		return nil
	case 'u': // SETITEMS
		kv, err := m.popMark()
		if err != nil {
			return err
		}
		t, err := m.top()
		if err != nil {
			return err
		}
		m.setItems(t, kv)
		return nil
	case 'b': // BUILD
		state, err := m.pop()
		if err != nil {
			return err
		}
		return m.build(state)
	case 'd': // DICT
		items, err := m.popMark()
		if err != nil {
			return err
		}
		d := m.newDict()
		m.setItems(d, items)
		if d.kind == kindOther {
			for _, it := range items {
				d.taint = d.taint || it.tainted()
			}
		}
		return m.push(d)
	case 'l': // LIST
		items, err := m.popMark()
		if err != nil {
			return err
		}
		l := m.newList()
		m.appendItems(l, items)
		return m.push(l)
	case 't': // TUPLE
		items, err := m.popMark()
		if err != nil {
			return err
		}
		return m.push(m.tuple(items))
	case 0x91: // FROZENSET
		items, err := m.popMark()
		if err != nil {
			return err
		}
		v := m.newList()
		m.appendItems(v, items)
		v.kind = kindOther
		return m.push(v)
	case '}': // EMPTY_DICT
		return m.push(m.newDict())
	case ']': // EMPTY_LIST
		return m.push(m.newList())
	case ')': // EMPTY_TUPLE
		return m.push(m.tuple(nil))
	case 0x8f: // EMPTY_SET
		v := m.newList()
		v.kind = kindOther
		return m.push(v)
	case 0x85, 0x86, 0x87: // TUPLE1, TUPLE2, TUPLE3
		items, err := m.popN(int(op-0x85) + 1)
		if err != nil {
			return err
		}
		return m.push(m.tuple(items))
	case 'R': // REDUCE
		vs, err := m.popN(2)
		if err != nil {
			return err
		}
		return m.push(m.call(vs[0], vs[1], true))
	case 0x81: // NEWOBJ
		vs, err := m.popN(2)
		if err != nil {
			return err
		}
		return m.push(m.call(vs[0], vs[1], false))
	case 0x92: // NEWOBJ_EX
		vs, err := m.popN(3)
		if err != nil {
			return err
		}
		res := m.call(vs[0], vs[1], false)
		if vs[2].tainted() && res.ref != nil {
			res.ref.taint = true
		}
		return m.push(res)
	case 'c': // GLOBAL
		mod, t1, err := in.line()
		if err != nil {
			return err
		}
		name, t2, err := in.line()
		if err != nil {
			return err
		}
		return m.push(m.addImport(mod, name, "GLOBAL", t1 || t2))
	case 'i': // INST
		mod, t1, err := in.line()
		if err != nil {
			return err
		}
		name, t2, err := in.line()
		if err != nil {
			return err
		}
		g := m.addImport(mod, name, "INST", t1 || t2)
		args, err := m.popMark()
		if err != nil {
			return err
		}
		return m.push(m.call(g, m.tuple(args), true))
	case 'o': // OBJ
		items, err := m.popMark()
		if err != nil {
			return err
		}
		if len(items) == 0 {
			return invalid("OBJ without a class")
		}
		return m.push(m.call(items[0], m.tuple(items[1:]), true))
	case 0x93: // STACK_GLOBAL
		vs, err := m.popN(2)
		if err != nil {
			return err
		}
		mod, name := vs[0], vs[1]
		if mod.kind == kindString && name.kind == kindString {
			return m.push(m.addImport(mod.s, name.s, "STACK_GLOBAL", mod.trunc || name.trunc))
		}
		// The arguments were computed, or not kept, so the import cannot
		// be named; treat it as unsafe, as picklescan does.
		m.imports = append(m.imports, Import{Module: "?", Name: "?", Opcode: "STACK_GLOBAL", Classification: Unsafe})
		return m.push(other(m.joblib))
	case 'g': // GET
		idx, big, err := m.lineInt()
		if err != nil {
			return err
		}
		if big {
			return invalid("memo index out of range")
		}
		return m.memoGet(idx)
	case 'h': // BINGET
		b, err := in.read(1)
		if err != nil {
			return err
		}
		return m.memoGet(int64(b[0]))
	case 'j': // LONG_BINGET
		b, err := in.read(4)
		if err != nil {
			return err
		}
		return m.memoGet(int64(binary.LittleEndian.Uint32(b)))
	case 'p': // PUT
		idx, big, err := m.lineInt()
		if err != nil {
			return err
		}
		if big {
			return unsupported("memo index out of range")
		}
		return m.memoPut(idx)
	case 'q': // BINPUT
		b, err := in.read(1)
		if err != nil {
			return err
		}
		return m.memoPut(int64(b[0]))
	case 'r': // LONG_BINPUT
		b, err := in.read(4)
		if err != nil {
			return err
		}
		return m.memoPut(int64(binary.LittleEndian.Uint32(b)))
	case 0x94: // MEMOIZE
		return m.memoPut(m.memo.count)
	case 0x80: // PROTO
		b, err := in.read(1)
		if err != nil {
			return err
		}
		if b[0] > 5 {
			return invalid("unsupported protocol %d", b[0])
		}
		return nil
	case 0x95: // FRAME
		b, err := in.read(8)
		if err != nil {
			return err
		}
		if m.joblib {
			return in.loadFrame(binary.LittleEndian.Uint64(b))
		}
		return nil
	case 0x82, 0x83, 0x84: // EXT1, EXT2, EXT4
		width := map[byte]int{0x82: 1, 0x83: 2, 0x84: 4}[op]
		if _, err := in.read(width); err != nil {
			return err
		}
		// The extension registry is empty unless code registers entries,
		// so the unpickler normally fails here; when it does not, the
		// value is a registered global the scan cannot name.
		m.imports = append(m.imports, Import{Module: "?", Name: "?", Opcode: "EXT", Classification: Unsafe})
		return m.push(other(m.joblib))
	case 0x97: // NEXT_BUFFER
		return m.push(other(false))
	case 0x98: // READONLY_BUFFER
		_, err := m.top()
		return err
	}
	return invalid("unknown opcode 0x%02x", op)
}

// Input.

// input reads a pickle stream. In the plain reading it is the file,
// linearly, as CPython's unpickler reads it. ScanJoblib also keeps the
// current pickle.py frame: opcodes come from the frame, while array data
// comes from the file after it.
type input struct {
	file  *bufio.Reader
	frame *bytes.Reader
	buf   [8]byte
}

func newInput(r io.Reader) *input {
	return &input{file: bufio.NewReaderSize(r, 64<<10)}
}

// loadFrame starts a frame of n bytes, as pickle.py's _Unframer does.
func (in *input) loadFrame(n uint64) error {
	if n > math.MaxInt64 {
		return invalid("frame size %d is too large", n)
	}
	if in.frame != nil && in.frame.Len() > 0 {
		return invalid("beginning of a new frame before end of current frame")
	}
	if n > maxFrame {
		return unsupported("a frame of %d bytes", n)
	}
	b := make([]byte, n)
	k, err := io.ReadFull(in.file, b)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return err
	}
	in.frame = bytes.NewReader(b[:k])
	return nil
}

// fromFrame reports whether the next opcode read comes from the frame,
// ending a frame that is used up.
func (in *input) fromFrame() bool {
	if in.frame == nil {
		return false
	}
	if in.frame.Len() == 0 {
		in.frame = nil
		return false
	}
	return true
}

func (in *input) readByte() (byte, error) {
	if in.fromFrame() {
		return in.frame.ReadByte()
	}
	return in.file.ReadByte()
}

func (in *input) read(n int) ([]byte, error) {
	b := in.buf[:n]
	if in.fromFrame() {
		if k, _ := io.ReadFull(in.frame, b); k < n {
			return nil, invalid("pickle exhausted before end of frame")
		}
		return b, nil
	}
	if _, err := io.ReadFull(in.file, b); err != nil {
		return nil, invalid("unexpected end of data")
	}
	return b, nil
}

// line reads a protocol 0 argument line without its newline, keeping the
// first maxLine bytes; trunc reports whether it was longer.
func (in *input) line() (string, bool, error) {
	var src io.ByteReader = in.file
	framed := in.fromFrame()
	if framed {
		src = in.frame
	}
	var sb strings.Builder
	trunc := false
	for {
		b, err := src.ReadByte()
		if err != nil {
			if framed {
				return "", false, invalid("pickle exhausted before end of frame")
			}
			return "", false, invalid("unexpected end of data in a line")
		}
		if b == '\n' {
			return sb.String(), trunc, nil
		}
		if sb.Len() >= maxLine {
			trunc = true
			continue
		}
		sb.WriteByte(b)
	}
}

// lenData reads a length of width bytes and then that many bytes, keeping
// the first maxStrKept of them.
func (in *input) lenData(width int) (string, bool, error) {
	b, err := in.read(width)
	if err != nil {
		return "", false, err
	}
	var n uint64
	switch width {
	case 1:
		n = uint64(b[0])
	case 4:
		n = uint64(binary.LittleEndian.Uint32(b))
	case 8:
		n = binary.LittleEndian.Uint64(b)
	}
	if n > math.MaxInt64 {
		return "", false, invalid("length %d is too large", n)
	}
	keep := min(n, maxStrKept)
	kb := make([]byte, keep)
	if in.fromFrame() {
		k, _ := io.ReadFull(in.frame, kb)
		if k < len(kb) || int64(in.frame.Len()) < int64(n-keep) { //nolint:gosec // n is at most MaxInt64.
			return "", false, invalid("pickle exhausted before end of frame")
		}
		_, _ = in.frame.Seek(int64(n-keep), io.SeekCurrent) //nolint:gosec // n is at most MaxInt64.
		return string(kb), n > keep, nil
	}
	if _, err := io.ReadFull(in.file, kb); err != nil {
		return "", false, invalid("unexpected end of data")
	}
	if err := discard(in.file, n-keep); err != nil {
		return "", false, err
	}
	return string(kb), n > keep, nil
}

// fileRead reads from the file directly, past any frame, as joblib's array
// reads do; a short read at the end is not an error.
func (in *input) fileRead(b []byte) (int, error) {
	n, err := io.ReadFull(in.file, b)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return n, err
	}
	return n, nil
}

// fileSkip discards n bytes of the file directly; exact makes a short file
// an error, as joblib's _read_bytes does.
func (in *input) fileSkip(n uint64, exact bool) error {
	err := discard(in.file, n)
	if err != nil && !exact && errors.Is(err, ErrInvalid) {
		return nil
	}
	return err
}

func discard(r *bufio.Reader, n uint64) error {
	for n > 0 {
		step := min(n, 1<<30)
		k, err := r.Discard(int(step)) //nolint:gosec // step is at most 1 GiB.
		n -= uint64(k)                 //nolint:gosec // k is not negative.
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return invalid("unexpected end of data")
			}
			return err
		}
	}
	return nil
}

// Scanning.

// legacyPickles is how many pickles a legacy (non-zip) torch.save file
// holds before its raw storage bytes: a magic number, the protocol
// version, system information, the object and the storage keys.
const legacyPickles = 5

// Scan reads one pickle and the pickles that follow it back to back, as
// pickle.load and torch.load do: up to five in any protocol (a legacy
// torch.save file), and more while they start with PROTO. It stops at the
// first data that is not a pickle, but keeps the imports seen before that
// point, because an unpickler runs them before it reaches the bad data.
//
// It returns ErrInvalid when the first pickle is invalid, and
// ErrUnsupported when it could not follow any of the pickles to its end;
// the imports found before the error are returned with it.
func Scan(r io.Reader) (Result, error) {
	m := newMachine(newInput(r), false)
	var res Result
	for {
		m.reset()
		err := m.run()
		res.Imports = append(res.Imports, m.imports...)
		m.imports = nil
		res.Joblib = res.Joblib || m.sawJob
		if err != nil {
			if errors.Is(err, ErrUnsupported) {
				return finish(res), err
			}
			if res.Pickles == 0 {
				res.NotPickle = m.ops == 0 && len(res.Imports) == 0
				return finish(res), err
			}
			// Data after the pickles that is not a pickle, where an
			// unpickler stops too.
			return finish(res), nil
		}
		res.Pickles++
		head, _ := m.in.file.Peek(2)
		if len(head) < 2 {
			return finish(res), nil
		}
		if more := res.Pickles < legacyPickles || (head[0] == 0x80 && head[1] <= 5); !more {
			return finish(res), nil
		}
	}
}

// ScanJoblib reads one pickle as joblib.load does: through pickle.py's
// unpickler, reading NumPy array data from the file after every
// NumpyArrayWrapper is built, and scanning the pickle joblib loads for an
// object array. It returns ErrInvalid when joblib would fail, and
// ErrUnsupported when the scan cannot tell where joblib reads next, for
// example when an instance that may be a NumpyArrayWrapper is built from
// values the scan did not track.
func ScanJoblib(r io.Reader) (Result, error) {
	m := newMachine(newInput(r), true)
	m.reset()
	err := m.run()
	res := Result{Imports: m.imports, Joblib: true}
	if err == nil {
		res.Pickles = 1
	} else if !errors.Is(err, ErrUnsupported) {
		res.NotPickle = m.ops == 0 && len(res.Imports) == 0
	}
	return finish(res), err
}

// finish raises partial wrappers to Unsafe when an unsafe target is
// imported in the same stream.
func finish(res Result) Result {
	anyUnsafe := false
	for _, i := range res.Imports {
		if i.Classification == Unsafe {
			anyUnsafe = true
		}
	}
	if anyUnsafe {
		for k, i := range res.Imports {
			if isPartial(i.Module, i.Name) {
				res.Imports[k].Classification = Unsafe
			}
		}
	}
	return res
}
