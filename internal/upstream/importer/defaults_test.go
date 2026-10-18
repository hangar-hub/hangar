package importer_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/model"
	"github.com/hangar-hub/hangar/internal/upstream/importer"
)

func TestDefaultFolders(t *testing.T) {
	t.Setenv("HF_HUB_CACHE", "/x/hub")
	require.Equal(t, "/x/hub", importer.DefaultHFCache())
	t.Setenv("HF_HUB_CACHE", "")
	t.Setenv("HF_HOME", "/y")
	require.Equal(t, filepath.Join("/y", "hub"), importer.DefaultHFCache())
	t.Setenv("HF_HOME", "")
	home := t.TempDir()
	t.Setenv("HOME", home)
	require.Equal(t, filepath.Join(home, ".cache", "huggingface", "hub"), importer.DefaultHFCache())
	require.Equal(t, filepath.Join(home, ".lmstudio", "models"), importer.DefaultLMStudio())
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".cache", "lm-studio", "models"), 0o755))
	require.Equal(t, filepath.Join(home, ".cache", "lm-studio", "models"), importer.DefaultLMStudio(), "the older folder when only it exists")
	t.Setenv("OLLAMA_MODELS", "/z")
	require.Equal(t, "/z", importer.DefaultOllama())
	t.Setenv("OLLAMA_MODELS", "")
	require.Equal(t, filepath.Join(home, ".ollama", "models"), importer.DefaultOllama())
}

// failingTarget fails every call.
type failingTarget struct{ err error }

func (f failingTarget) PullHistory(context.Context, string, model.RepoID, string) error { return f.err }
func (f failingTarget) PullFiles(context.Context, model.RepoID, []string) error         { return f.err }
func (f failingTarget) Tree(context.Context, model.RepoID, string) ([]model.Entry, error) {
	return nil, f.err
}
func (f failingTarget) Store(context.Context, model.RepoID, importer.File) error { return f.err }
func (f failingTarget) CreateRepo(context.Context, model.RepoID) error           { return f.err }
func (f failingTarget) Commit(context.Context, model.RepoID, string, []importer.File) error {
	return f.err
}

func TestImportErrors(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("boom")
	im := importer.New(importer.Options{Target: failingTarget{err: boom}})
	_, err := im.FromHFCache(ctx, filepath.Join(t.TempDir(), "missing"))
	require.Error(t, err)
	_, err = im.FromLMStudio(ctx, filepath.Join(t.TempDir(), "missing"))
	require.Error(t, err)
	rep, err := im.FromOllama(ctx, t.TempDir())
	require.Error(t, err, "no manifests folder")
	require.Nil(t, rep)

	root := t.TempDir()
	write(t, filepath.Join(root, "models--acme--bert", "refs", "main"), []byte("not a sha"))
	write(t, filepath.Join(root, "models--acme--bert", "snapshots", "0123456789012345678901234567890123456789", "f.txt"), []byte("x"))
	write(t, filepath.Join(root, "models--bad", "refs", "main"), []byte("x"))
	write(t, filepath.Join(root, "notes.txt"), []byte("x"))
	rep, err = im.FromHFCache(ctx, root)
	require.NoError(t, err)
	require.Len(t, rep.Results, 1, "the newest snapshot is used when refs/main is unreadable")
	require.ErrorIs(t, rep.Results[0].Err, boom)
	require.Equal(t, 1, rep.Failed())

	res := im.FromDir(ctx, root, model.RepoID{Type: model.RepoTypeModel, Namespace: "acme", Name: "bert"}, "s")
	require.ErrorIs(t, res.Err, boom)
	lms := t.TempDir()
	write(t, filepath.Join(lms, "pub", "repo", "f.gguf"), []byte("x"))
	write(t, filepath.Join(lms, "pub", "bad--name", "f.gguf"), []byte("x"))
	rep, err = im.FromLMStudio(ctx, lms)
	require.NoError(t, err)
	require.Len(t, rep.Results, 2)
	require.Equal(t, 2, rep.Failed())

	// An LM Studio folder the Upstream does not have falls back to a
	// local Repo, which also fails here.
	im = importer.New(importer.Options{Target: failingTarget{err: importer.ErrUpstreamMissing}, Namespace: "me"})
	rep, err = im.FromLMStudio(ctx, lms)
	require.NoError(t, err)
	for _, r := range rep.Results {
		if r.Repo.Name == "repo" {
			require.Equal(t, "me", r.Repo.Namespace)
			require.Equal(t, "local", r.Kind)
		}
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	_, err = im.FromLMStudio(cctx, lms)
	require.ErrorIs(t, err, context.Canceled)
	_, err = im.FromHFCache(cctx, root)
	require.ErrorIs(t, err, context.Canceled)
}
