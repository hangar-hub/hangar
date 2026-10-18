package repo_test

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/blob"
	"github.com/hangar-hub/hangar/internal/db/dbtest"
	"github.com/hangar-hub/hangar/internal/model"
	"github.com/hangar-hub/hangar/internal/repo"
)

// gitCounter puts a git wrapper first on PATH that logs every invocation,
// so a test can count the git processes an operation starts. Every git
// process is a fork and exec, which is what makes requests slow on a busy
// host, so these counts must not grow with the number of files.
type gitCounter struct {
	t   *testing.T
	log string
}

// countGit installs the wrapper. It changes PATH for the whole process, so
// tests that use it must not run in parallel.
func countGit(t *testing.T) *gitCounter {
	t.Helper()
	real, err := exec.LookPath("git")
	require.NoError(t, err)
	dir := t.TempDir()
	log := filepath.Join(dir, "calls.log")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> '" + log + "'\nexec '" + real + "' \"$@\"\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o700)) //nolint:gosec // an executable test wrapper.
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return &gitCounter{t: t, log: log}
}

// calls returns the git invocations since the last call and resets the log.
func (c *gitCounter) calls() []string {
	c.t.Helper()
	b, err := os.ReadFile(c.log)
	if os.IsNotExist(err) {
		return nil
	}
	require.NoError(c.t, err)
	require.NoError(c.t, os.Remove(c.log))
	return strings.Split(strings.TrimRight(string(b), "\n"), "\n")
}

// TestGitProcessesPerOperation bounds the git processes behind the hot
// paths of the web UI. Every git process is a fork and exec, and on a
// loaded host each one costs hundreds of milliseconds: e2e flows timed out
// when Commit detail started four processes per changed file and a Commit
// one or more per file. The counts must not grow with the number of files.
func TestGitProcessesPerOperation(t *testing.T) {
	gc := countGit(t)
	e := newEnv(t, dbtest.SQLite(t))
	alice := e.addUser("alice")
	id := model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "procs"}
	check := func(name string, limit int) {
		t.Helper()
		calls := gc.calls()
		require.LessOrEqual(t, len(calls), limit, "%s started %d git processes:\n%s", name, len(calls), strings.Join(calls, "\n"))
	}

	e.create(repo.CreateParams{ID: id, Actor: alice, InitialReadme: true})
	check("create", 7)

	one := e.commit(id, alice, add("config.json", `{"a":1}`))
	check("commit of one file", 8)

	var ops []repo.Operation
	for i := range 40 {
		ops = append(ops, add(fmt.Sprintf("dir/file-%02d.txt", i), fmt.Sprintf("content %d\n", i)))
	}
	ops = append(ops, add("weights.bin", "tracked by LFS"), repo.Operation{Kind: repo.OpDelete, Path: "config.json"})
	many := e.commit(id, alice, ops...)
	// The delete adds an ls-files and an update-index.
	check("commit of 42 operations", 10)

	changes, err := e.svc.Diff(e.ctx, id, many.SHA)
	require.NoError(t, err)
	require.Len(t, changes, 42)
	check("diff of 42 changes", 3)
	byPath := map[string]repo.FileChange{}
	for _, c := range changes {
		byPath[c.Path] = c
	}
	require.True(t, byPath["weights.bin"].LFS)
	require.Equal(t, int64(len("tracked by LFS")), byPath["weights.bin"].NewSize)
	require.Equal(t, repo.ChangeDeleted, byPath["config.json"].Status)
	require.Equal(t, int64(len(`{"a":1}`)), byPath["config.json"].OldSize)

	fd, err := e.svc.FileDiff(e.ctx, id, many.SHA, "dir/file-03.txt")
	require.NoError(t, err)
	require.Contains(t, fd.Patch, "+content 3")
	check("file diff", 4)

	// A fresh Store has an empty blob cache: the first read of a small file
	// starts one process, later reads none.
	fresh, err := repo.NewStore(e.opts)
	require.NoError(t, err)
	readFile := func(rng *blob.Range) string {
		rc, _, err := fresh.OpenFile(e.ctx, id, many.SHA, "README.md", rng)
		require.NoError(t, err)
		defer rc.Close()
		b, err := io.ReadAll(rc)
		require.NoError(t, err)
		return string(b)
	}
	readme := readFile(nil)
	require.Contains(t, readme, "---")
	check("first read of a small file", 1)
	require.Equal(t, readme, readFile(nil))
	require.Equal(t, readme[2:5], readFile(&blob.Range{Start: 2, End: 4}))
	check("later reads of a small file", 0)

	_, err = e.svc.Tree(e.ctx, id, one.SHA, "", repo.TreeOpts{})
	require.NoError(t, err)
	check("tree of a cached Commit", 0)
}
