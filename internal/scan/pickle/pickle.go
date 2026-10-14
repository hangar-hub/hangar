// Package pickle finds the imports in a Python pickle without executing
// it (META-2, docs/spec/07-indexing-and-scanning.md "Pickle scanner"). It
// walks the opcode stream of protocols 0 to 5, simulating just enough of
// the stack and memo to resolve STACK_GLOBAL arguments, and classifies
// every imported global against the lists in lists.go.
package pickle

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
)

// Classification of an import, from best to worst.
type Classification string

// Classifications.
const (
	Safe       Classification = "safe"
	Suspicious Classification = "suspicious"
	Unsafe     Classification = "unsafe"
)

// ErrInvalid means the stream is not a valid pickle.
var ErrInvalid = errors.New("invalid pickle")

// Limits that bound memory on hostile input.
const (
	maxStack   = 1 << 20
	maxMemo    = 1 << 20
	maxLine    = 1 << 16
	maxStrKept = 1 << 12
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

	firstErr error
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

// normaliseModule maps Python 2 module names to their Python 3 names, as
// pickle's fix_imports does.
func normaliseModule(m string) string {
	switch m {
	case "__builtin__", "__builtins__", "exceptions":
		return "builtins"
	case "copy_reg":
		return "copyreg"
	}
	return m
}

func matches(entry, module, name string) bool {
	if prefix, ok := strings.CutSuffix(entry, ".*"); ok {
		return module == prefix || strings.HasPrefix(module, prefix+".")
	}
	global := module + "." + name
	return global == entry || strings.HasPrefix(global, entry+".")
}

// Classify returns the classification of a global by the lists in
// lists.go. Partial wrappers are Suspicious here; Scan raises them to
// Unsafe when the same pickle imports an unsafe target.
func Classify(module, name string) Classification {
	module = normaliseModule(module)
	for _, e := range unsafeGlobals {
		if matches(e, module, name) {
			return Unsafe
		}
	}
	if slices.Contains(safeGlobals, module+"."+name) {
		return Safe
	}
	for _, s := range safeSuffixes {
		if module == s.module && strings.HasSuffix(name, s.suffix) && !strings.Contains(name, ".") {
			return Safe
		}
	}
	return Suspicious
}

func isPartial(module, name string) bool {
	return slices.Contains(partialGlobals, normaliseModule(module)+"."+name)
}

type valueKind uint8

const (
	kindOther valueKind = iota
	kindString
	kindMark
)

type value struct {
	kind valueKind
	s    string
}

type machine struct {
	r       *bufio.Reader
	stack   []value
	memo    map[int64]value
	imports []Import
	buf     [8]byte
}

func (m *machine) push(v value) error {
	if len(m.stack) >= maxStack {
		return fmt.Errorf("%w: the stack is too deep", ErrInvalid)
	}
	m.stack = append(m.stack, v)
	return nil
}

func (m *machine) pop() (value, error) {
	if len(m.stack) == 0 {
		return value{}, fmt.Errorf("%w: stack underflow", ErrInvalid)
	}
	v := m.stack[len(m.stack)-1]
	m.stack = m.stack[:len(m.stack)-1]
	return v, nil
}

func (m *machine) popN(n int) error {
	for range n {
		if _, err := m.pop(); err != nil {
			return err
		}
	}
	return nil
}

// popMark pops everything down to and including the topmost mark.
func (m *machine) popMark() error {
	for i := len(m.stack) - 1; i >= 0; i-- {
		if m.stack[i].kind == kindMark {
			m.stack = m.stack[:i]
			return nil
		}
	}
	return fmt.Errorf("%w: no mark on the stack", ErrInvalid)
}

func (m *machine) top() (value, error) {
	if len(m.stack) == 0 {
		return value{}, fmt.Errorf("%w: stack underflow", ErrInvalid)
	}
	return m.stack[len(m.stack)-1], nil
}

func (m *machine) read(n int) ([]byte, error) {
	if _, err := io.ReadFull(m.r, m.buf[:n]); err != nil {
		return nil, fmt.Errorf("%w: unexpected end of data", ErrInvalid)
	}
	return m.buf[:n], nil
}

func (m *machine) line() (string, error) {
	var sb strings.Builder
	for {
		b, err := m.r.ReadByte()
		if err != nil {
			return "", fmt.Errorf("%w: unexpected end of data in a line", ErrInvalid)
		}
		if b == '\n' {
			return sb.String(), nil
		}
		if sb.Len() >= maxLine {
			return "", fmt.Errorf("%w: line longer than %d bytes", ErrInvalid, maxLine)
		}
		sb.WriteByte(b)
	}
}

// data reads n bytes, keeping up to maxStrKept of them; the rest are
// discarded as they stream past.
func (m *machine) data(n uint64) (string, error) {
	keep := min(n, maxStrKept)
	b := make([]byte, keep)
	if _, err := io.ReadFull(m.r, b); err != nil {
		return "", fmt.Errorf("%w: unexpected end of data", ErrInvalid)
	}
	if rest := n - keep; rest > 0 {
		if rest > 1<<62 {
			return "", fmt.Errorf("%w: length %d is too large", ErrInvalid, n)
		}
		if _, err := io.CopyN(io.Discard, m.r, int64(rest)); err != nil {
			return "", fmt.Errorf("%w: unexpected end of data", ErrInvalid)
		}
	}
	return string(b), nil
}

func (m *machine) lenData(width int) (string, error) {
	b, err := m.read(width)
	if err != nil {
		return "", err
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
	return m.data(n)
}

func (m *machine) addImport(module, name, opcode string) {
	m.imports = append(m.imports, Import{Module: module, Name: name, Opcode: opcode, Classification: Classify(module, name)})
}

func (m *machine) memoPut(idx int64) error {
	if idx < 0 {
		return fmt.Errorf("%w: negative memo index", ErrInvalid)
	}
	v, err := m.top()
	if err != nil {
		return err
	}
	if _, ok := m.memo[idx]; !ok && len(m.memo) >= maxMemo {
		return fmt.Errorf("%w: the memo is too large", ErrInvalid)
	}
	m.memo[idx] = v
	return nil
}

func (m *machine) memoGet(idx int64) error {
	v, ok := m.memo[idx]
	if !ok {
		return fmt.Errorf("%w: memo index %d is not set", ErrInvalid, idx)
	}
	return m.push(v)
}

// stringOps push a string whose value Scan keeps, for STACK_GLOBAL.
func (m *machine) pushString(s string) error { return m.push(value{kind: kindString, s: s}) }

// run parses one pickle, up to and including its STOP opcode.
func (m *machine) run() error {
	m.stack = m.stack[:0]
	m.memo = map[int64]value{}
	other := value{}
	for {
		op, err := m.r.ReadByte()
		if err != nil {
			return fmt.Errorf("%w: unexpected end of data before STOP", ErrInvalid)
		}
		switch op {
		case '.': // STOP
			return nil
		case '(': // MARK
			err = m.push(value{kind: kindMark})
		case '0': // POP
			_, err = m.pop()
		case '1': // POP_MARK
			err = m.popMark()
		case '2': // DUP
			var v value
			if v, err = m.top(); err == nil {
				err = m.push(v)
			}
		case 'F', 'I', 'L': // FLOAT, INT, LONG
			if _, err = m.line(); err == nil {
				err = m.push(other)
			}
		case 'S': // STRING
			var s string
			if s, err = m.line(); err == nil {
				err = m.pushString(unquote(s))
			}
		case 'V': // UNICODE
			var s string
			if s, err = m.line(); err == nil {
				err = m.pushString(s)
			}
		case 'P': // PERSID
			if _, err = m.line(); err == nil {
				err = m.push(other)
			}
		case 'Q': // BINPERSID
			if _, err = m.pop(); err == nil {
				err = m.push(other)
			}
		case 'J': // BININT
			if _, err = m.read(4); err == nil {
				err = m.push(other)
			}
		case 'K': // BININT1
			if _, err = m.read(1); err == nil {
				err = m.push(other)
			}
		case 'M': // BININT2
			if _, err = m.read(2); err == nil {
				err = m.push(other)
			}
		case 'G': // BINFLOAT
			if _, err = m.read(8); err == nil {
				err = m.push(other)
			}
		case 'N', '\x88', '\x89': // NONE, NEWTRUE, NEWFALSE
			err = m.push(other)
		case 'T', 'X': // BINSTRING, BINUNICODE
			var s string
			if s, err = m.lenData(4); err == nil {
				err = m.pushString(s)
			}
		case 'U', '\x8c': // SHORT_BINSTRING, SHORT_BINUNICODE
			var s string
			if s, err = m.lenData(1); err == nil {
				err = m.pushString(s)
			}
		case '\x8d': // BINUNICODE8
			var s string
			if s, err = m.lenData(8); err == nil {
				err = m.pushString(s)
			}
		case 'B': // BINBYTES
			if _, err = m.lenData(4); err == nil {
				err = m.push(other)
			}
		case 'C': // SHORT_BINBYTES
			if _, err = m.lenData(1); err == nil {
				err = m.push(other)
			}
		case '\x8e', '\x96': // BINBYTES8, BYTEARRAY8
			if _, err = m.lenData(8); err == nil {
				err = m.push(other)
			}
		case '\x8a': // LONG1
			if _, err = m.lenData(1); err == nil {
				err = m.push(other)
			}
		case '\x8b': // LONG4
			if _, err = m.lenData(4); err == nil {
				err = m.push(other)
			}
		case 'a': // APPEND
			_, err = m.pop()
		case 'b': // BUILD
			_, err = m.pop()
		case 'd', 'l', 't', '\x91': // DICT, LIST, TUPLE, FROZENSET
			if err = m.popMark(); err == nil {
				err = m.push(other)
			}
		case '}', ']', ')', '\x8f': // EMPTY_DICT, EMPTY_LIST, EMPTY_TUPLE, EMPTY_SET
			err = m.push(other)
		case 'e', 'u', '\x90': // APPENDS, SETITEMS, ADDITEMS
			err = m.popMark()
		case 's': // SETITEM
			err = m.popN(2)
		case '\x85': // TUPLE1
			if err = m.popN(1); err == nil {
				err = m.push(other)
			}
		case '\x86': // TUPLE2
			if err = m.popN(2); err == nil {
				err = m.push(other)
			}
		case '\x87': // TUPLE3
			if err = m.popN(3); err == nil {
				err = m.push(other)
			}
		case 'R', '\x81': // REDUCE, NEWOBJ
			if err = m.popN(2); err == nil {
				err = m.push(other)
			}
		case '\x92': // NEWOBJ_EX
			if err = m.popN(3); err == nil {
				err = m.push(other)
			}
		case 'c': // GLOBAL
			var mod, name string
			if mod, err = m.line(); err == nil {
				if name, err = m.line(); err == nil {
					m.addImport(mod, name, "GLOBAL")
					err = m.push(other)
				}
			}
		case 'i': // INST
			var mod, name string
			if mod, err = m.line(); err == nil {
				if name, err = m.line(); err == nil {
					m.addImport(mod, name, "INST")
					if err = m.popMark(); err == nil {
						err = m.push(other)
					}
				}
			}
		case 'o': // OBJ
			if err = m.popMark(); err == nil {
				err = m.push(other)
			}
		case '\x93': // STACK_GLOBAL
			var name, mod value
			if name, err = m.pop(); err == nil {
				if mod, err = m.pop(); err == nil {
					if name.kind == kindString && mod.kind == kindString {
						m.addImport(mod.s, name.s, "STACK_GLOBAL")
					} else {
						// The arguments were computed, so the import cannot be
						// named; treat it as unsafe, as picklescan does.
						m.imports = append(m.imports, Import{Module: "?", Name: "?", Opcode: "STACK_GLOBAL", Classification: Unsafe})
					}
					err = m.push(other)
				}
			}
		case 'g': // GET
			var s string
			if s, err = m.line(); err == nil {
				var idx int64
				if idx, err = strconv.ParseInt(strings.TrimSpace(s), 10, 64); err != nil {
					err = fmt.Errorf("%w: bad GET index", ErrInvalid)
				} else {
					err = m.memoGet(idx)
				}
			}
		case 'h': // BINGET
			var b []byte
			if b, err = m.read(1); err == nil {
				err = m.memoGet(int64(b[0]))
			}
		case 'j': // LONG_BINGET
			var b []byte
			if b, err = m.read(4); err == nil {
				err = m.memoGet(int64(binary.LittleEndian.Uint32(b)))
			}
		case 'p': // PUT
			var s string
			if s, err = m.line(); err == nil {
				var idx int64
				if idx, err = strconv.ParseInt(strings.TrimSpace(s), 10, 64); err != nil {
					err = fmt.Errorf("%w: bad PUT index", ErrInvalid)
				} else {
					err = m.memoPut(idx)
				}
			}
		case 'q': // BINPUT
			var b []byte
			if b, err = m.read(1); err == nil {
				err = m.memoPut(int64(b[0]))
			}
		case 'r': // LONG_BINPUT
			var b []byte
			if b, err = m.read(4); err == nil {
				err = m.memoPut(int64(binary.LittleEndian.Uint32(b)))
			}
		case '\x94': // MEMOIZE
			err = m.memoPut(int64(len(m.memo)))
		case '\x80': // PROTO
			var b []byte
			if b, err = m.read(1); err == nil && b[0] > 5 {
				err = fmt.Errorf("%w: unsupported protocol %d", ErrInvalid, b[0])
			}
		case '\x95': // FRAME
			_, err = m.read(8)
		case '\x82': // EXT1
			if _, err = m.read(1); err == nil {
				err = m.push(other)
			}
		case '\x83': // EXT2
			if _, err = m.read(2); err == nil {
				err = m.push(other)
			}
		case '\x84': // EXT4
			if _, err = m.read(4); err == nil {
				err = m.push(other)
			}
		case '\x97': // NEXT_BUFFER
			err = m.push(other)
		case '\x98': // READONLY_BUFFER
			_, err = m.top()
		default:
			return fmt.Errorf("%w: unknown opcode 0x%02x", ErrInvalid, op)
		}
		if err != nil {
			return err
		}
	}
}

// next parses one more pickle into res and reports whether another may
// follow. When the first pickle fails, res.firstErr holds the error; a
// later failure means the data after the pickles is not a pickle, where an
// unpickler stops too.
func (m *machine) next(res *Result) bool {
	runErr := m.run()
	res.Imports = append(res.Imports, m.imports...)
	m.imports = nil
	if runErr != nil {
		if res.Pickles == 0 {
			res.firstErr = runErr
		}
		return false
	}
	res.Pickles++
	head, _ := m.r.Peek(2)
	if len(head) < 2 {
		return false
	}
	return res.Pickles < legacyPickles || (head[0] == '\x80' && head[1] <= 5)
}

// unquote strips the quotes of a protocol 0 STRING argument.
func unquote(s string) string {
	if len(s) >= 2 && (s[0] == '\'' || s[0] == '"') && s[len(s)-1] == s[0] {
		if u, err := strconv.Unquote(`"` + s[1:len(s)-1] + `"`); err == nil {
			return u
		}
		return s[1 : len(s)-1]
	}
	return s
}

// legacyPickles is how many pickles a legacy (non-zip) torch.save file
// holds before its raw storage bytes: a magic number, the protocol
// version, system information, the object and the storage keys.
const legacyPickles = 5

// Scan reads one pickle and the pickles that follow it back to back, as in
// legacy PyTorch files: up to five in any protocol, and more while they
// start with PROTO. It stops at the first data that is not a pickle, but
// keeps the imports seen before that point, because an unpickler runs them
// before it reaches the bad data. It fails only when the first pickle is
// invalid; the imports found before the error are returned with it.
func Scan(r io.Reader) (Result, error) {
	m := &machine{r: bufio.NewReaderSize(r, 64<<10)}
	var res Result
	for m.next(&res) {
	}
	if res.Pickles == 0 {
		return finish(res), res.firstErr
	}
	return finish(res), nil
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
