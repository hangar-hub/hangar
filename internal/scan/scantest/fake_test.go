package scantest_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/repo"
	"github.com/jamesagarside/hangar/internal/repo/repotest"
	"github.com/jamesagarside/hangar/internal/scan"
	"github.com/jamesagarside/hangar/internal/scan/scantest"
)

func TestFakeScanner(t *testing.T) {
	ctx := context.Background()
	repos := repotest.NewFake(t)
	id := model.RepoID{Type: model.RepoTypeModel, Namespace: "ns", Name: "pickle"}
	r, err := repos.Create(ctx, repo.CreateParams{ID: id})
	require.NoError(t, err)
	res, err := repos.Commit(ctx, repo.CommitParams{Repo: id, Summary: "add", Operations: []repo.Operation{
		{Kind: repo.OpAdd, Path: "pytorch_model.bin", Content: strings.NewReader("pickle bytes")},
		{Kind: repo.OpAdd, Path: "config.json", Content: strings.NewReader("{}")},
	}})
	require.NoError(t, err)

	f := scantest.NewFake(repos)
	_, err = f.Results(ctx, r.UUID, res.SHA)
	require.ErrorIs(t, err, scan.ErrNotScanned)

	f.SetResult("pytorch_model.bin", scan.StatusUnsafe, scan.Finding{Module: "os", Name: "system", Opcode: "STACK_GLOBAL", Status: scan.StatusUnsafe})
	out, err := f.ScanCommit(ctx, r.UUID, id, res.SHA)
	require.NoError(t, err)
	require.Equal(t, scan.StatusUnsafe, out.Status)
	require.Len(t, out.Files, 3)
	stored, err := f.Results(ctx, r.UUID, res.SHA)
	require.NoError(t, err)
	require.Equal(t, out, stored)

	fr, err := f.ScanFile(ctx, "other.pkl", strings.NewReader("x"))
	require.NoError(t, err)
	require.Equal(t, scan.StatusSafe, fr.Status)
	_, err = f.ScanCommit(ctx, r.UUID, id, model.CommitSHA(strings.Repeat("f", 40)))
	require.Error(t, err)
}
