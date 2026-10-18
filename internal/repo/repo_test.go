package repo_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/model"
	"github.com/hangar-hub/hangar/internal/repo"
)

func TestValidateCommitParams(t *testing.T) {
	ok := repo.CommitParams{Summary: "s", Operations: []repo.Operation{
		{Kind: repo.OpAdd, Path: "a.txt", Content: strings.NewReader("a")},
		{Kind: repo.OpAddLFS, Path: "w.bin", OID: strings.Repeat("a", 64)},
		{Kind: repo.OpDelete, Path: "old.bin"},
		{Kind: repo.OpDeleteFolder, Path: "logs/"},
		{Kind: repo.OpCopy, Path: "b.txt", SrcPath: "a.txt"},
	}}
	require.NoError(t, repo.ValidateCommitParams(ok))

	for name, p := range map[string]repo.CommitParams{
		"no summary": {Operations: ok.Operations},
		"no ops":     {Summary: "s"},
		"bad path":   {Summary: "s", Operations: []repo.Operation{{Kind: repo.OpDelete, Path: "../x"}}},
		"git path":   {Summary: "s", Operations: []repo.Operation{{Kind: repo.OpDelete, Path: ".git/config"}}},
		"empty path": {Summary: "s", Operations: []repo.Operation{{Kind: repo.OpDelete, Path: ""}}},
		"no content": {Summary: "s", Operations: []repo.Operation{{Kind: repo.OpAdd, Path: "a"}}},
		"bad oid":    {Summary: "s", Operations: []repo.Operation{{Kind: repo.OpAddLFS, Path: "a", OID: "xyz"}}},
		"bad copy":   {Summary: "s", Operations: []repo.Operation{{Kind: repo.OpCopy, Path: "a", SrcPath: "/etc/passwd"}}},
		"unknown":    {Summary: "s", Operations: []repo.Operation{{Kind: "rename", Path: "a"}}},
	} {
		require.ErrorIs(t, repo.ValidateCommitParams(p), repo.ErrInvalid, name)
	}
}

func TestDefaultGitAttributes(t *testing.T) {
	m := string(repo.DefaultGitAttributes(model.RepoTypeModel))
	d := string(repo.DefaultGitAttributes(model.RepoTypeDataset))
	require.Equal(t, 35, strings.Count(m, "\n"))
	require.Contains(t, m, "*.safetensors filter=lfs diff=lfs merge=lfs -text\n")
	require.NotContains(t, m, "*.mp3")
	require.Contains(t, d, "*.parquet filter=lfs diff=lfs merge=lfs -text\n")
	require.Contains(t, d, "*.mp3 filter=lfs diff=lfs merge=lfs -text\n")
	// Callers may modify the result without changing the default.
	b := repo.DefaultGitAttributes(model.RepoTypeModel)
	b[0] = 'X'
	require.Equal(t, m, string(repo.DefaultGitAttributes(model.RepoTypeModel)))
}

func TestSmallTypes(t *testing.T) {
	closed := false
	v := repo.NewGitView("/tmp/x", func() error { closed = true; return nil })
	require.NoError(t, v.Close())
	require.True(t, closed)
	var nilView *repo.GitView
	require.NoError(t, nilView.Close())

	require.Equal(t, "refs/heads/main: no", (&repo.PolicyError{Ref: "refs/heads/main", Message: "no"}).Error())
	require.Equal(t, "no", (&repo.PolicyError{Message: "no"}).Error())
	require.False(t, repo.Gating{}.Enabled())
	require.True(t, repo.Gating{Mode: "auto"}.Enabled())
}
