package fs

import (
	"errors"
	iofs "io/fs"
	"os"
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

func TestRelative(t *testing.T) {
	s := &Store{root: "/srv/blobs"}
	require.Equal(t, "lfs/sha256/ab/ab/x", s.relative("/srv/blobs/lfs/sha256/ab/ab/x"))
	require.Equal(t, "uploads/u/part-00001", s.relative("/srv/blobs/uploads/u/part-00001"))
	require.Equal(t, "x", s.relative("relative/x"), "a path that cannot be made relative keeps only its base name")
}
