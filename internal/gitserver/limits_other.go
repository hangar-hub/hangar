//go:build !linux

package gitserver

import "time"

// limitCPU is a no-op where the operating system cannot limit another
// process's CPU time; the wall-clock limit (git.timeout) still applies.
// Hangar's container images run on Linux, where the limit is enforced.
func limitCPU(int, time.Duration) error { return nil }
