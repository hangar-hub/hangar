package gitserver

import (
	"crypto/sha1" //nolint:gosec // git pack indexes are SHA-1.
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/format/idxfile"
	"github.com/stretchr/testify/require"
)

// TestPackObjectsReturnsComparableHashes covers a push whose pack holds
// two objects with the same first byte. go-git's index iterator copies
// the next name's bytes into the unused tail of a SHA-1 ObjectID, so the
// first hash printed the same as the tree's entry but compared unequal,
// and a refusal for an oversized file named "a file (blob ...)" instead of
// its path.
func TestPackObjectsReturnsComparableHashes(t *testing.T) {
	ids := []plumbing.Hash{
		plumbing.NewHash("ab00000000000000000000000000000000000001"),
		plumbing.NewHash("ab00000000000000000000000000000000000002"),
		plumbing.NewHash("cd00000000000000000000000000000000000003"),
	}
	w := new(idxfile.Writer)
	require.NoError(t, w.OnHeader(uint32(len(ids))))
	for i, id := range ids {
		require.NoError(t, w.OnInflatedObjectContent(id, int64(12+i*100), 0, nil))
	}
	require.NoError(t, w.OnFooter(plumbing.NewHash("ffffffffffffffffffffffffffffffffffffffff")))
	idx, err := w.Index()
	require.NoError(t, err)

	pack := filepath.Join(t.TempDir(), "pack-test.pack")
	f, err := os.Create(strings.TrimSuffix(pack, ".pack") + ".idx")
	require.NoError(t, err)
	require.NoError(t, idxfile.Encode(f, sha1.New(), idx)) //nolint:gosec // git pack indexes are SHA-1.
	require.NoError(t, f.Close())

	got, err := packObjects(pack)
	require.NoError(t, err)
	require.Len(t, got, len(ids))
	want := map[plumbing.Hash]bool{}
	for _, id := range ids {
		want[id] = true
	}
	for _, h := range got {
		require.True(t, want[h], "packObjects returned %s, which does not compare equal to the same hash parsed from hex", h)
	}
}
