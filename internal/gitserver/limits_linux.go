//go:build linux

package gitserver

import (
	"time"

	"golang.org/x/sys/unix"
)

// limitCPU sets the CPU time limit of a running process to d (git.timeout),
// so a pathological request cannot use more CPU than wall-clock time.
func limitCPU(pid int, d time.Duration) error {
	secs := uint64(d.Seconds())
	if secs == 0 {
		secs = 1
	}
	return unix.Prlimit(pid, unix.RLIMIT_CPU, &unix.Rlimit{Cur: secs, Max: secs + 5}, nil)
}
