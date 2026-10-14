package pipeline

import (
	"context"
	"io"

	"github.com/jamesagarside/hangar/internal/datasets/convert"
	"github.com/jamesagarside/hangar/internal/datasets/layout"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/repo"
)

// OpenRepoFile returns random access to a file at a Commit; see
// layout.OpenRepoFile.
func OpenRepoFile(ctx context.Context, repos repo.Service, id model.RepoID, commit model.CommitSHA, path string, size int64) *layout.RepoFile {
	return layout.OpenRepoFile(ctx, repos, id, commit, path, size)
}

// SourceFiles lists a Commit's files as conversion sources.
func SourceFiles(ctx context.Context, repos repo.Service, id model.RepoID, commit model.CommitSHA) (map[string]convert.File, error) {
	entries, err := repos.Tree(ctx, id, commit, "", repo.TreeOpts{Recursive: true})
	if err != nil {
		return nil, err
	}
	out := make(map[string]convert.File, len(entries))
	for _, e := range entries {
		if e.Type != model.EntryFile {
			continue
		}
		out[e.Path] = sourceFile(repos, id, commit, e)
	}
	return out, nil
}

func sourceFile(repos repo.Service, id model.RepoID, commit model.CommitSHA, e model.Entry) convert.File {
	p, size := e.Path, e.Size
	f := convert.File{Path: p, Size: size,
		Open: func(ctx context.Context) (io.ReadCloser, error) {
			rc, _, err := repos.OpenFile(ctx, id, commit, p, nil)
			return rc, err
		},
		OpenAt: func(ctx context.Context) (io.ReaderAt, error) {
			return OpenRepoFile(ctx, repos, id, commit, p, size), nil
		}}
	if e.LFS != nil {
		f.OID = e.LFS.OID
	}
	return f
}
