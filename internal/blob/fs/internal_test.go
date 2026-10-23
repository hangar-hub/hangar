package fs

import (
	"context"
	"errors"
	iofs "io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLostDirectoryRetriesOnlyTransientErrors(t *testing.T) {
	link := func(errno syscall.Errno) error {
		return &os.LinkError{Op: "rename", Old: "/root/.staging/put-1", New: "/root/lfs/x", Err: errno}
	}
	require.True(t, lostDirectory(link(syscall.ENOENT)), "a parent directory removed before Rename")
	require.True(t, lostDirectory(link(syscall.EINVAL)), "macOS: a parent directory being removed")
	require.False(t, lostDirectory(link(syscall.EEXIST)), "os.Rename onto a directory is permanent")
	require.False(t, lostDirectory(link(syscall.EISDIR)))
	require.False(t, lostDirectory(link(syscall.EACCES)))
}

func TestInstallErrorHasNoPaths(t *testing.T) {
	err := installError("lfs/x", &os.LinkError{Op: "rename", Old: "/root/.staging/put-1", New: "/root/lfs/x", Err: syscall.EEXIST})
	require.ErrorIs(t, err, iofs.ErrExist)
	require.Equal(t, "blob fs: store lfs/x: a directory already has this key's path: file exists", err.Error())

	err = installError("lfs/x", &os.PathError{Op: "mkdir", Path: "/root/lfs", Err: syscall.ENOTDIR})
	require.ErrorIs(t, err, syscall.ENOTDIR)
	require.NotContains(t, err.Error(), "/root")

	other := errors.New("other")
	require.ErrorIs(t, installError("lfs/x", other), other)
}

// TestInstallSyncsTheDirectory: a rename into place is only durable once
// the directory that holds it is synced. A filesystem that cannot sync a
// directory, such as an NFS or SMB mount, does not fail the write; any other
// sync error does, without the absolute path.
func TestInstallSyncsTheDirectory(t *testing.T) {
	s, err := New(filepath.Join(t.TempDir(), "blobs"))
	require.NoError(t, err)
	ctx := context.Background()
	realSync := syncDir
	t.Cleanup(func() { syncDir = realSync })

	var synced []string
	var answer error
	syncDir = func(dir string) error {
		synced = append(synced, dir)
		return answer
	}
	require.NoError(t, s.Put(ctx, "lfs/sha256/ab/cd/x", strings.NewReader("x"), 1))
	require.Equal(t, []string{filepath.Join(s.Root(), "lfs", "sha256", "ab", "cd")}, synced)

	for _, errno := range []syscall.Errno{syscall.EINVAL, syscall.ENOTSUP, syscall.EOPNOTSUPP} {
		answer = &os.PathError{Op: "sync", Path: s.Root(), Err: errno}
		require.NoError(t, s.Put(ctx, "a", strings.NewReader("a"), 1), "%v", errno)
	}
	answer = &os.PathError{Op: "sync", Path: s.Root(), Err: syscall.EIO}
	err = s.Put(ctx, "b", strings.NewReader("b"), 1)
	require.ErrorIs(t, err, syscall.EIO)
	require.NotContains(t, err.Error(), s.Root())

	// The real sync works on the test's filesystem.
	syncDir = realSync
	require.NoError(t, s.Put(ctx, "c/d", strings.NewReader("d"), 1))
}

func TestRelative(t *testing.T) {
	s := &Store{root: "/srv/blobs"}
	require.Equal(t, "lfs/sha256/ab/ab/x", s.relative("/srv/blobs/lfs/sha256/ab/ab/x"))
	require.Equal(t, "uploads/u/part-00001", s.relative("/srv/blobs/uploads/u/part-00001"))
	require.Equal(t, "x", s.relative("relative/x"), "a path that cannot be made relative keeps only its base name")
}
