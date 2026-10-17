package pickle_test

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/scan/pickle"
	"github.com/jamesagarside/hangar/internal/scan/scantest"
)

// The tests in this file are regressions for ways a pickle could run code
// that the scan did not report: places where the scan read the opcode
// stream differently from CPython's unpickler or joblib's.

func scanBytes(t *testing.T, b []byte) (pickle.Result, error) {
	t.Helper()
	return pickle.Scan(bytes.NewReader(b))
}

// backslash is written out so that no tool rewrites the escape.
const backslash = "\x5c"

func requireUnsafe(t *testing.T, res pickle.Result, global string) {
	t.Helper()
	for _, i := range res.Imports {
		if i.Global() == global {
			require.Equal(t, pickle.Unsafe, i.Classification, global)
			require.Equal(t, pickle.Unsafe, res.Worst())
			return
		}
	}
	require.Failf(t, "import not found", "%s not in %v", global, globals(res))
}

// stackGlobal pushes module and name with the given string opcodes, then
// STACK_GLOBAL and a call with one argument.
func stackGlobalCall(module, name []byte) []byte {
	var b bytes.Buffer
	b.Write(module)
	b.Write(name)
	b.WriteString("\x93S'id'\n\x85R.")
	return b.Bytes()
}

func TestScanDecodesProtocol0StringsAsPython(t *testing.T) {
	// UNICODE is raw-unicode-escape: a backslash, u and 006f is "o".
	res, err := scanBytes(t, stackGlobalCall([]byte("V"+backslash+"u006fs\n"), []byte("Vsystem\n")))
	require.NoError(t, err)
	requireUnsafe(t, res, "os.system")

	// STRING is escape-decoded: a two-digit octal escape \56 is ".", so the
	// name is hub.load, which protocol 4 resolves to torch.hub.load.
	res, err = scanBytes(t, stackGlobalCall([]byte("S'torch'\n"), []byte(`S'hub\56load'`+"\n")))
	require.NoError(t, err)
	requireUnsafe(t, res, "torch.hub.load")

	// Hex escapes in a STRING.
	res, err = scanBytes(t, stackGlobalCall([]byte(`S'\x6fs'`+"\n"), []byte(`S'sys\x74em'`+"\n")))
	require.NoError(t, err)
	requireUnsafe(t, res, "os.system")

	_, err = scanBytes(t, []byte("S'unquoted\n."))
	require.ErrorIs(t, err, pickle.ErrInvalid, "pickle requires STRING arguments to be quoted")
}

func TestScanParsesMemoIndexesAsPython(t *testing.T) {
	// int() accepts underscores, spaces and leading zeros, so the
	// unpickler reads past these PUT and GET lines.
	for _, idx := range []string{"1_0", " 10 ", "010", "+10"} {
		p := "S'os'\np" + idx + "\n0cos\nsystem\n(S'id'\ntRg" + idx + "\n0."
		res, err := scanBytes(t, []byte(p))
		require.NoError(t, err, idx)
		requireUnsafe(t, res, "os.system")
	}
}

func TestScanReadsPastLongLines(t *testing.T) {
	// The unpickler reads lines of any length.
	p := "F" + strings.Repeat("1", 70000) + "\n0cos\nsystem\n(S'id'\ntR."
	res, err := scanBytes(t, []byte(p))
	require.NoError(t, err)
	requireUnsafe(t, res, "os.system")

	// A name longer than the scan keeps cannot be told apart from a
	// dangerous one.
	long := strings.Repeat("a", 5000)
	res, err = scanBytes(t, []byte("\x80\x04X"+string(binary.LittleEndian.AppendUint32(nil, 5000))+long+"\x8c\x01x\x93."))
	require.NoError(t, err)
	require.Equal(t, pickle.Unsafe, res.Worst())
}

func TestScanFollowsLargeMemos(t *testing.T) {
	// More memo entries than the old limit, as a large vocabulary pickle
	// has, then a call.
	var b bytes.Buffer
	b.WriteString("\x80\x04")
	for range (1 << 20) + 10 {
		b.WriteString("N\x940")
	}
	b.WriteString("\x8c\x02os\x94\x8c\x06system\x94\x93\x8c\x02id\x85R.")
	res, err := scanBytes(t, b.Bytes())
	require.NoError(t, err)
	requireUnsafe(t, res, "os.system")

	// MEMOIZE numbers entries by how many are set, so a value read back
	// through the memo is the one the unpickler reads.
	p := scantest.NewPickle(4)
	p.Raw('N', 'p').Op("5\n").Op("0") // index 5, the first entry
	p.Unicode("os").Memoize()         // index 1: one entry is set
	p.Op("0")
	p.Unicode("system").Raw('q', 2).Op("0")
	p.BinGet(1).BinGet(2).Raw(0x93).Op(")R.")
	res, err = scanBytes(t, p.Bytes())
	require.NoError(t, err)
	requireUnsafe(t, res, "os.system")
}

func TestScanReportsLimitsInLaterPickles(t *testing.T) {
	// A legacy torch.save file whose second pickle is deeper than the scan
	// follows: torch.load reads all five pickles, so the scan must not
	// treat the rest as raw storage bytes.
	var b bytes.Buffer
	b.WriteString("\x80\x02N.")
	b.WriteString("\x80\x02(")
	b.Write(bytes.Repeat([]byte("N"), (1<<20)+1))
	b.WriteString("1cos\nsystem\n(S'id'\ntR.")
	_, err := scanBytes(t, b.Bytes())
	require.ErrorIs(t, err, pickle.ErrUnsupported)
}

func TestClassifyMapsPython2Names(t *testing.T) {
	// fix_imports maps these to pickle.loads, urllib.request.urlopen,
	// sys.intern and subprocess.check_output.
	cases := map[[2]string]pickle.Classification{
		{"cPickle", "loads"}:           pickle.Unsafe,
		{"urllib2", "urlopen"}:         pickle.Unsafe,
		{"__builtin__", "intern"}:      pickle.Unsafe,
		{"commands", "check_output"}:   pickle.Unsafe,
		{"urllib", "urlretrieve"}:      pickle.Unsafe,
		{"__builtin__", "set"}:         pickle.Safe,
		{"copy_reg", "_reconstructor"}: pickle.Suspicious,
		{"__builtin__", "reduce"}:      pickle.Suspicious,
	}
	for g, want := range cases {
		require.Equal(t, want, pickle.Classify(g[0], g[1]), "%s.%s", g[0], g[1])
	}
}

func TestClassifyDottedNames(t *testing.T) {
	// Protocol 4 resolves dotted names attribute by attribute.
	cases := map[[2]string]pickle.Classification{
		{"collections", "abc.sys.modules"}:                 pickle.Unsafe,
		{"collections", "abc.sys"}:                         pickle.Unsafe,
		{"torch._utils", "_rebuild_tensor_v2.__globals__"}: pickle.Unsafe,
		{"builtins", "object.__subclasses__"}:              pickle.Unsafe,
		{"torch", "serialization.load"}:                    pickle.Unsafe,
		{"pathlib", "os.system"}:                           pickle.Unsafe,
		{"builtins", "str.lower"}:                          pickle.Suspicious,
		{"mypackage.models", "Outer.Inner"}:                pickle.Suspicious,
		{"torch", "FloatStorage.x"}:                        pickle.Suspicious,
		{"numpy.core.multiarray", "_reconstruct"}:          pickle.Safe,
		{"_posixsubprocess", "fork_exec"}:                  pickle.Unsafe,
		{"torch.utils.cpp_extension", "load_inline"}:       pickle.Unsafe,
		{"numpy", "load"}:                                  pickle.Unsafe,
		{"joblib", "load"}:                                 pickle.Unsafe,
		{"pandas", "read_pickle"}:                          pickle.Unsafe,
		{"builtins", "setattr"}:                            pickle.Unsafe,
		{"_frozen_importlib_external", "SourceFileLoader"}: pickle.Unsafe,
	}
	for g, want := range cases {
		require.Equal(t, want, pickle.Classify(g[0], g[1]), "%s.%s", g[0], g[1])
	}
}

func TestScanJoblibFindsCodeHiddenByArrayData(t *testing.T) {
	data := scantest.JoblibHiddenOSSystem()
	// pickle.load reads the array data as a bytes object and never reaches
	// the call...
	res, err := scanBytes(t, data)
	require.NoError(t, err)
	require.NotEqual(t, pickle.Unsafe, res.Worst())
	require.True(t, res.Joblib, "the stream names NumpyArrayWrapper, so it must also be read as joblib does")
	// ...but joblib.load skips the array data and runs os.system.
	res, err = pickle.ScanJoblib(bytes.NewReader(data))
	require.NoError(t, err)
	requireUnsafe(t, res, "os.system")
}

func TestScanJoblibReadsArrayDataAfterTheFrame(t *testing.T) {
	res, err := pickle.ScanJoblib(bytes.NewReader(scantest.JoblibFramedOSSystem()))
	require.NoError(t, err)
	requireUnsafe(t, res, "os.system")
}

func TestScanJoblibScansObjectArrayPickles(t *testing.T) {
	nested := scantest.OSSystemProtocol0()
	data := scantest.JoblibPickle(scantest.JoblibArray{TypeString: "O8", Shape: []int{1}, Object: true, Data: nested}, []byte("u."))
	res, err := pickle.ScanJoblib(bytes.NewReader(data))
	require.NoError(t, err)
	requireUnsafe(t, res, "os.system")
}

func TestScanJoblibRefusesWhatItCannotFollow(t *testing.T) {
	cases := map[string][]byte{
		// An object array joblib may memory-map instead of unpickling.
		"object array with allow_mmap": scantest.JoblibPickle(scantest.JoblibArray{TypeString: "O8", Shape: []int{1}, Object: true, MmapObject: true}, nil),
		// A type whose size depends on the platform.
		"long double": scantest.JoblibPickle(scantest.JoblibArray{TypeString: "f16", Shape: []int{1}}, nil),
		// An instance made by a call the scan cannot see through, from a
		// value that names the wrapper.
		"computed wrapper": scantest.NewPickle(2).Global("builtins", "next").
			Global("joblib.numpy_pickle", "NumpyArrayWrapper").Raw(0x85).Op("R}b.").Bytes(),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := pickle.ScanJoblib(bytes.NewReader(data))
			require.ErrorIs(t, err, pickle.ErrUnsupported)
		})
	}
}

// TestScanJoblibReadsRealFiles reads joblib.dump output (testdata/joblib,
// written by generate.py): both readings must end without an
// unsupported error, and the joblib reading without any error.
func TestScanJoblibReadsRealFiles(t *testing.T) {
	files, err := filepath.Glob("testdata/joblib/*.joblib")
	require.NoError(t, err)
	require.NotEmpty(t, files)
	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			raw, err := os.ReadFile(f)
			require.NoError(t, err)
			if strings.Contains(f, "zlib") {
				zr, err := zlib.NewReader(bytes.NewReader(raw))
				require.NoError(t, err)
				raw, err = io.ReadAll(zr)
				require.NoError(t, err)
			}
			plain, err := pickle.Scan(bytes.NewReader(raw))
			if err != nil {
				require.NotErrorIs(t, err, pickle.ErrUnsupported)
			}
			require.True(t, plain.Joblib)
			res, err := pickle.ScanJoblib(bytes.NewReader(raw))
			require.NoError(t, err)
			require.Equal(t, 1, res.Pickles)
			require.NotEqual(t, pickle.Unsafe, res.Worst(), "%v", globals(res))
		})
	}
}

func TestScanSpotsDataThatIsNotAPickle(t *testing.T) {
	for _, data := range [][]byte{[]byte("\x00\x01\x02"), []byte("raw weights"), []byte("ggjt\x01\x00\x00\x00")} {
		res, err := scanBytes(t, data)
		require.ErrorIs(t, err, pickle.ErrInvalid)
		require.True(t, res.NotPickle, "%q", data)
	}
	res, err := scanBytes(t, []byte("(\xff"))
	require.ErrorIs(t, err, pickle.ErrInvalid)
	require.False(t, res.NotPickle, "a MARK ran first")
}
