//go:build compat

// Package gitcompat is the git and git-lfs compatibility suite
// (docs/spec/13-testing.md "git"). It drives the real git, git-lfs and hf
// command line clients against Hangar's git server, wired to each
// registered Repo service backend, with two replicas sharing one backend.
//
// Run it with `go test -tags compat ./compat/git/`. It needs git (2.39 or
// later), git-lfs, and uv for the hf multipart transfer agent.
package gitcompat
