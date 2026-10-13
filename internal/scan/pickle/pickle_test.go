package pickle_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/scan/pickle"
	"github.com/jamesagarside/hangar/internal/scan/scantest"
)

func globals(r pickle.Result) []string {
	var out []string
	for _, i := range r.Imports {
		out = append(out, i.Global()+":"+i.Opcode+":"+string(i.Classification))
	}
	return out
}

func TestScanClassifiesImports(t *testing.T) {
	cases := []struct {
		name  string
		data  []byte
		worst pickle.Classification
		want  []string
	}{
		{"torch state dict", scantest.TorchStateDict(), pickle.Safe, []string{
			"torch._utils._rebuild_tensor_v2:GLOBAL:safe", "torch.FloatStorage:GLOBAL:safe", "collections.OrderedDict:GLOBAL:safe"}},
		{"protocol 4", scantest.Protocol4Benign(), pickle.Safe, []string{
			"collections.OrderedDict:STACK_GLOBAL:safe", "numpy.core.multiarray._reconstruct:STACK_GLOBAL:safe", "numpy.ndarray:STACK_GLOBAL:safe"}},
		{"os.system", scantest.OSSystemProtocol0(), pickle.Unsafe, []string{"os.system:GLOBAL:unsafe"}},
		{"posix.system through the memo", scantest.PosixSystemStackGlobal(), pickle.Unsafe, []string{"posix.system:STACK_GLOBAL:unsafe"}},
		{"INST", scantest.SubprocessInst(), pickle.Unsafe, []string{"subprocess.Popen:INST:unsafe"}},
		{"Python 2 builtins", scantest.BuiltinsEval(), pickle.Unsafe, []string{"__builtin__.eval:GLOBAL:unsafe"}},
		{"partial of os.system", scantest.PartialOSSystem(), pickle.Unsafe, []string{"functools.partial:GLOBAL:unsafe", "os.system:GLOBAL:unsafe"}},
		{"partial of a safe callable", scantest.PartialSafe(), pickle.Suspicious, []string{"functools.partial:GLOBAL:suspicious", "collections.OrderedDict:GLOBAL:safe"}},
		{"computed STACK_GLOBAL", scantest.ComputedStackGlobal(), pickle.Unsafe, []string{"builtins.str:STACK_GLOBAL:suspicious", "?.?:STACK_GLOBAL:unsafe"}},
		{"unknown class", scantest.SklearnSuspicious(), pickle.Suspicious, []string{"sklearn.linear_model._base.LinearRegression:GLOBAL:suspicious"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res, err := pickle.Scan(bytes.NewReader(c.data))
			require.NoError(t, err)
			require.Equal(t, c.want, globals(res))
			require.Equal(t, c.worst, res.Worst())
			require.Equal(t, 1, res.Pickles)
		})
	}
}

func TestScanFollowsBackToBackPickles(t *testing.T) {
	res, err := pickle.Scan(bytes.NewReader(scantest.LegacyTorch(scantest.OSSystemProtocol0())))
	require.NoError(t, err, "raw storage bytes after the pickles end the scan without an error")
	require.Equal(t, pickle.Unsafe, res.Worst())
	require.Equal(t, 5, res.Pickles)

	res, err = pickle.Scan(bytes.NewReader(scantest.LegacyTorch(scantest.TorchStateDict())))
	require.NoError(t, err)
	require.Equal(t, pickle.Safe, res.Worst())
}

func TestClassify(t *testing.T) {
	cases := map[string]pickle.Classification{
		"os.system": pickle.Unsafe, "os.path.join": pickle.Unsafe, "nt.system": pickle.Unsafe,
		"builtins.getattr": pickle.Unsafe, "builtins.__import__": pickle.Unsafe, "__builtins__.exec": pickle.Unsafe,
		"torch.load": pickle.Unsafe, "torch.hub.load": pickle.Unsafe, "operator.attrgetter": pickle.Unsafe,
		"importlib.import_module": pickle.Unsafe, "urllib.request.urlopen": pickle.Unsafe, "webbrowser.open": pickle.Unsafe,
		"code.InteractiveInterpreter.runcode": pickle.Unsafe,
		"torch.FloatStorage":                  pickle.Safe, "torch.BFloat16Storage": pickle.Safe, "numpy.dtypes.Float32DType": pickle.Safe,
		"builtins.set": pickle.Safe, "_codecs.encode": pickle.Safe, "torch._utils._rebuild_parameter": pickle.Safe,
		"torch.nn.Linear": pickle.Suspicious, "builtins.print": pickle.Suspicious, "torchvision.models.resnet50": pickle.Suspicious,
		"osx.thing": pickle.Suspicious, "torch.Storage.evil": pickle.Suspicious,
	}
	for g, want := range cases {
		i := strings.LastIndex(g, ".")
		if strings.HasPrefix(g, "code.") {
			i = len("code")
		}
		if strings.HasPrefix(g, "torch.Storage.") {
			i = len("torch")
		}
		require.Equal(t, want, pickle.Classify(g[:i], g[i+1:]), g)
	}
}

func TestScanRejectsMalformedPickles(t *testing.T) {
	cases := map[string][]byte{
		"empty":          nil,
		"no STOP":        []byte("\x80\x02}"),
		"unknown opcode": []byte("\x80\x02\xff"),
		"underflow":      []byte("\x80\x02R."),
		"no mark":        []byte("\x80\x02t."),
		"bad memo":       []byte("\x80\x02h\x05."),
		"bad protocol":   []byte("\x80\x09."),
		"long line":      append([]byte("c"), bytes.Repeat([]byte("a"), 70000)...),
		"short string":   []byte("\x80\x04\x8c\x10ab"),
		"bad GET":        []byte("gx\n."),
		"bad PUT":        []byte("N" + "px\n."),
		"truncated int":  []byte("\x80\x02J\x01"),
		"huge bytes":     []byte("\x80\x04\x8e\xff\xff\xff\xff\xff\xff\xff\xff."),
		"empty stack":    []byte("2."),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := pickle.Scan(bytes.NewReader(data))
			require.ErrorIs(t, err, pickle.ErrInvalid)
		})
	}
	res, err := pickle.Scan(bytes.NewReader([]byte("cos\nsystem\n\xff")))
	require.Error(t, err)
	require.Equal(t, pickle.Unsafe, res.Worst(), "imports found before an error are returned")
}

func TestScanCoversEveryOpcode(t *testing.T) {
	// A protocol 0 to 5 mix that pickletools accepts; every value-pushing
	// opcode appears once.
	p := scantest.NewPickle(5)
	p.Op("(")
	p.Raw('I').Op("1\n").Raw('L').Op("2L\n").Raw('F').Op("1.5\n").Raw('G', 0, 0, 0, 0, 0, 0, 0, 0)
	p.Op("N").Raw(0x88, 0x89).Raw('M', 1, 0).Raw(0x8a, 1, 1).Raw(0x8b, 1, 0, 0, 0, 1)
	p.Raw('T', 1, 0, 0, 0, 'a').Raw('U', 1, 'b').Raw('V').Op("c\n").Raw(0x8d, 1, 0, 0, 0, 0, 0, 0, 0, 'd')
	p.Raw('B', 1, 0, 0, 0, 'e').Raw('C', 1, 'f').Raw(0x8e, 1, 0, 0, 0, 0, 0, 0, 0, 'g').Raw(0x96, 1, 0, 0, 0, 0, 0, 0, 0, 'h')
	p.Op("P").Op("pid\n").Op("N").Op("Q")
	p.Raw(0x82, 1).Raw(0x83, 1, 0).Raw(0x84, 1, 0, 0, 0)
	p.Raw(0x97).Raw(0x98)
	p.Op("l")
	p.Op("p7\n").Op("0").Op("g7\n").Raw('r', 8, 0, 0, 0).Raw('j', 8, 0, 0, 0).Op("2").Op("0")
	p.Op("(").Op("1")
	p.Op("]").Op("(N").Op("e").Op("N").Op("a")
	p.Op("}(NNu").Op("NN").Op("s")
	p.Raw(0x8f).Op("(N").Raw(0x90).Op("(N").Raw(0x91)
	p.Op("(t").Op("(d").Raw(0x85).Raw(0x86).Op("N").Raw(0x87)
	p.Global("collections", "OrderedDict").Op(")").Op("}").Raw(0x92)
	p.Op("(").Global("collections", "OrderedDict").Op("o")
	p.Op("0000.")
	res, err := pickle.Scan(bytes.NewReader(p.Bytes()))
	require.NoError(t, err)
	require.Equal(t, pickle.Safe, res.Worst())
}

func FuzzScan(f *testing.F) {
	for _, seed := range [][]byte{
		scantest.TorchStateDict(), scantest.Protocol4Benign(), scantest.OSSystemProtocol0(), scantest.PosixSystemStackGlobal(),
		scantest.SubprocessInst(), scantest.PartialOSSystem(), scantest.ComputedStackGlobal(), scantest.LegacyTorch(scantest.TorchStateDict()),
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		res, err := pickle.Scan(bytes.NewReader(data))
		if err != nil {
			require.ErrorIs(t, err, pickle.ErrInvalid)
		}
		w := res.Worst()
		for _, i := range res.Imports {
			if i.Classification == pickle.Unsafe {
				require.Equal(t, pickle.Unsafe, w)
			}
		}
	})
}
