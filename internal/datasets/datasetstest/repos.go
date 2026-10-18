package datasetstest

import (
	"bytes"
	"context"
	"path"
	"sort"
	"testing"

	"github.com/hangar-hub/hangar/internal/model"
	"github.com/hangar-hub/hangar/internal/repo"
	"github.com/hangar-hub/hangar/internal/repo/repotest"
)

// lfsExtensions are stored as LFS objects by CommitFiles, as the Hub's
// default .gitattributes for datasets does.
var lfsExtensions = map[string]bool{
	".parquet": true, ".arrow": true, ".jpg": true, ".jpeg": true, ".png": true, ".wav": true, ".flac": true,
	".mp3": true, ".ogg": true, ".tar": true, ".gz": true, ".zst": true, ".bz2": true, ".zip": true, ".webp": true,
	".gif": true, ".bmp": true, ".tif": true, ".tiff": true, ".mp4": true,
}

// CreateDataset creates a public dataset Repo in a repotest.Fake, owned by
// the system user, and commits files to main. It returns the new Commit.
func CreateDataset(t testing.TB, repos *repotest.Fake, id model.RepoID, files map[string][]byte) model.CommitSHA {
	t.Helper()
	if _, err := repos.Create(context.Background(), repo.CreateParams{ID: id, Visibility: model.VisibilityPublic, Actor: model.System()}); err != nil {
		t.Fatalf("create %s: %v", id, err)
	}
	if len(files) == 0 {
		sha, err := repos.Resolve(context.Background(), id, "main")
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		return sha
	}
	return CommitFiles(t, repos, id, "main", files)
}

// CommitFiles commits files to a branch of a Repo in a repotest.Fake.
// Binary formats go through the fake LFS store; the rest are git blobs.
// A nil value deletes the file.
func CommitFiles(t testing.TB, repos *repotest.Fake, id model.RepoID, branch string, files map[string][]byte) model.CommitSHA {
	t.Helper()
	ctx := context.Background()
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	var ops []repo.Operation
	for _, n := range names {
		content := files[n]
		switch {
		case content == nil:
			ops = append(ops, repo.Operation{Kind: repo.OpDelete, Path: n})
		case lfsExtensions[path.Ext(n)]:
			obj, err := repos.LFS.Put(ctx, content)
			if err != nil {
				t.Fatalf("store %s: %v", n, err)
			}
			ops = append(ops, repo.Operation{Kind: repo.OpAddLFS, Path: n, OID: obj.OID, Size: obj.Size})
		default:
			ops = append(ops, repo.Operation{Kind: repo.OpAdd, Path: n, Content: bytes.NewReader(content)})
		}
	}
	res, err := repos.Commit(ctx, repo.CommitParams{Repo: id, Revision: branch, Summary: "add files", Operations: ops, Actor: model.System()})
	if err != nil {
		t.Fatalf("commit to %s: %v", id, err)
	}
	return res.SHA
}
