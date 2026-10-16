package harness

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) { os.Exit(Main(m)) }

// runFunc adapts a function to the Run method of testing.M.
type runFunc func() int

func (f runFunc) Run() int { return f() }

// isolateTempDir points os.TempDir at a fresh directory and returns it.
func isolateTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	require.Equal(t, dir, os.TempDir())
	return dir
}

func leftovers(t *testing.T, dir string) []string {
	t.Helper()
	found, err := filepath.Glob(filepath.Join(dir, "hangar-compat-bin-*"))
	require.NoError(t, err)
	return found
}

func writeBinary(out string) error { return os.WriteFile(out, []byte("binary"), 0o755) }

func TestBuilderRemovesTheBinaryWhenTheTestsEnd(t *testing.T) {
	for name, want := range map[string]int{"passing tests": 0, "failing tests": 1} {
		t.Run(name, func(t *testing.T) {
			tmp := isolateTempDir(t)
			builds := 0
			b := &builder{build: func(out string) error { builds++; return writeBinary(out) }}
			var built string
			code := b.run(runFunc(func() int {
				first, err := b.binary()
				require.NoError(t, err)
				second, err := b.binary()
				require.NoError(t, err)
				require.Equal(t, first, second, "the binary is built once per test process")
				require.FileExists(t, first)
				require.Equal(t, tmp, filepath.Dir(filepath.Dir(first)))
				built = first
				return want
			}))
			require.Equal(t, want, code, "Main keeps the tests' exit code")
			require.Equal(t, 1, builds)
			require.NoDirExists(t, filepath.Dir(built))
			require.Empty(t, leftovers(t, tmp))
		})
	}
}

func TestBuilderRemovesTheDirectoryWhenTheBuildFails(t *testing.T) {
	tmp := isolateTempDir(t)
	b := &builder{build: func(string) error { return errors.New("compile error") }}
	code := b.run(runFunc(func() int {
		_, err := b.binary()
		require.ErrorContains(t, err, "compile error")
		require.Len(t, leftovers(t, tmp), 1)
		return 1
	}))
	require.Equal(t, 1, code)
	require.Empty(t, leftovers(t, tmp))
}

func TestBuilderRefusesToBuildOutsideMain(t *testing.T) {
	tmp := isolateTempDir(t)
	b := &builder{build: func(string) error {
		t.Fatal("built a binary that nothing would remove")
		return nil
	}}
	_, err := b.binary()
	require.ErrorContains(t, err, "harness.Main")
	require.Empty(t, leftovers(t, tmp))
}

func TestBuilderLeavesNothingWhenNothingIsBuilt(t *testing.T) {
	tmp := isolateTempDir(t)
	b := &builder{build: writeBinary}
	require.Equal(t, 0, b.run(runFunc(func() int { return 0 })))
	require.Empty(t, leftovers(t, tmp))
}
