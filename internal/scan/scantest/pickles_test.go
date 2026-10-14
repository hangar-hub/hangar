package scantest_test

import (
	"archive/zip"
	"bytes"
	"compress/zlib"
	"io"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/scan/pickle"
	"github.com/jamesagarside/hangar/internal/scan/scantest"
)

func TestFixturesParseAsPickles(t *testing.T) {
	for name, data := range map[string][]byte{
		"state dict": scantest.TorchStateDict(), "protocol 4": scantest.Protocol4Benign(), "os.system": scantest.OSSystemProtocol0(),
		"posix": scantest.PosixSystemStackGlobal(), "inst": scantest.SubprocessInst(), "eval": scantest.BuiltinsEval(),
		"partial": scantest.PartialOSSystem(), "partial safe": scantest.PartialSafe(), "computed": scantest.ComputedStackGlobal(),
		"sklearn": scantest.SklearnSuspicious(), "legacy": scantest.LegacyTorch(scantest.TorchStateDict()),
		"joblib": scantest.Joblib(scantest.TorchStateDict(), false),
	} {
		_, err := pickle.Scan(bytes.NewReader(data))
		require.NoError(t, err, name)
	}
}

func TestBuilderOpcodes(t *testing.T) {
	p := scantest.NewPickle(4)
	p.Unicode(string(make([]byte, 300))).Memoize()
	p.BinPut()
	p.BinGet(0).Op("0")
	p.Int(-5).Int(70000).Op("0").Op("0")
	p.Op("0.")
	res, err := pickle.Scan(bytes.NewReader(p.Bytes()))
	require.NoError(t, err)
	require.Empty(t, res.Imports)
	require.Equal(t, []byte("S'x'\n"), scantest.NewPickle(0).String("x").Bytes())
}

func TestArchivesAndArrays(t *testing.T) {
	zr, err := zip.NewReader(bytes.NewReader(scantest.TorchZip(scantest.TorchStateDict(), 10)), int64(len(scantest.TorchZip(scantest.TorchStateDict(), 10))))
	require.NoError(t, err)
	names := []string{}
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	require.Equal(t, []string{"archive/data.pkl", "archive/byteorder", "archive/data/0", "archive/version"}, names)
	desc := scantest.ZipWithDescriptors(scantest.TorchStateDict())
	_, err = zip.NewReader(bytes.NewReader(desc), int64(len(desc)))
	require.NoError(t, err)

	z, err := zlib.NewReader(bytes.NewReader(scantest.Joblib(scantest.TorchStateDict(), true)))
	require.NoError(t, err)
	plain, err := io.ReadAll(z)
	require.NoError(t, err)
	require.Equal(t, scantest.TorchStateDict(), plain)

	npy := scantest.NumpyFloatArray()
	require.Equal(t, "\x93NUMPY", string(npy[:6]))
	require.Zero(t, (len(npy)-16)%64, "the header is padded to 64 bytes")
	require.Contains(t, string(scantest.NumpyObjectArray(scantest.OSSystemProtocol0())), "'|O'")
}
