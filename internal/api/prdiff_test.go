package api_test

import (
	"bytes"
	"context"
	"net/http"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/api/gen"
	"github.com/jamesagarside/hangar/internal/repo"
)

// TestPullRequestFileDiff checks GET .../discussions/{num}/diff: a file's
// diff over every Commit of a pull request, from where its ref left the
// base branch, while the base moves on.
func TestPullRequestFileDiff(t *testing.T) {
	e := newMetaEnv(t)
	ctx := context.Background()
	base := "/repos/models/alice/tiny/discussions"
	yes := true
	var pr gen.Discussion
	e.json("POST", base, "bob-token", gen.DiscussionCreate{Title: "Notes", PullRequest: &yes}, http.StatusCreated, &pr)
	var d gen.Discussion
	e.json("POST", base, "bob-token", gen.DiscussionCreate{Title: "Not a pull request"}, http.StatusCreated, &d)
	diffPath := base + "/" + strconv.Itoa(pr.Num) + "/diff?path=notes.txt"

	status, _ := e.do("GET", diffPath, "bob-token", nil)
	require.Equal(t, http.StatusNotFound, status, "no Commit on the ref yet")

	tip, err := e.repos.Resolve(ctx, e.model.ID, "main")
	require.NoError(t, err)
	commit := func(rev, content string, from bool) {
		t.Helper()
		p := repo.CommitParams{Repo: e.model.ID, Revision: rev, Summary: "edit", Actor: e.bob,
			Operations: []repo.Operation{{Kind: repo.OpAdd, Path: "notes.txt", Content: bytes.NewReader([]byte(content))}}}
		if from {
			p.CreateFrom = tip
		}
		_, err := e.repos.Commit(ctx, p)
		require.NoError(t, err)
	}
	commit(*pr.PrRef, "hello\nfirst\n", true)
	commit(*pr.PrRef, "hello\nfirst\nsecond\n", false)
	// main moves on with another file; the pull request's diff ignores it.
	_, err = e.repos.Commit(ctx, repo.CommitParams{Repo: e.model.ID, Revision: "main", Summary: "other", Actor: e.alice,
		Operations: []repo.Operation{{Kind: repo.OpAdd, Path: "other.txt", Content: bytes.NewReader([]byte("x"))}}})
	require.NoError(t, err)

	var fd gen.FileDiff
	e.json("GET", diffPath, "bob-token", nil, http.StatusOK, &fd)
	require.Equal(t, "notes.txt", fd.Path)
	require.NotNil(t, fd.Patch)
	require.Contains(t, *fd.Patch, "-hello")
	require.Contains(t, *fd.Patch, "+first")
	require.Contains(t, *fd.Patch, "+second", "both Commits of the pull request are in the diff")

	status, _ = e.do("GET", base+"/"+strconv.Itoa(pr.Num)+"/diff?path=other.txt", "bob-token", nil)
	require.Equal(t, http.StatusNotFound, status, "a change on the base is not part of the pull request")
	status, _ = e.do("GET", base+"/"+strconv.Itoa(d.Num)+"/diff?path=notes.txt", "bob-token", nil)
	require.Equal(t, http.StatusUnprocessableEntity, status)
	status, _ = e.do("GET", base+"/99/diff?path=notes.txt", "bob-token", nil)
	require.Equal(t, http.StatusNotFound, status)
	status, _ = e.do("GET", "/repos/models/alice/missing/discussions/1/diff?path=x", "", nil)
	require.Equal(t, http.StatusUnauthorized, status)
}
