package layout

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"slices"
	"sort"
	"strings"

	"github.com/jamesagarside/hangar/internal/datasets"
	"github.com/jamesagarside/hangar/internal/datasets/card"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/repo"
)

// MaxCardBytes is the largest README.md or .huggingface.yaml read.
const MaxCardBytes = 4 << 20

// Service is the datasets.Layout over the Repo service.
type Service struct {
	Repos repo.Service
}

var _ datasets.Layout = (*Service)(nil)

// ReadCard reads and parses the card of a dataset Repo at a Commit. It
// returns an empty card when the Repo has no README.md.
func ReadCard(ctx context.Context, repos repo.Service, id model.RepoID, commit model.CommitSHA) (*card.Card, error) {
	readme, err := readSmall(ctx, repos, id, commit, "README.md")
	if err != nil {
		return nil, err
	}
	standalone, err := readSmall(ctx, repos, id, commit, ".huggingface.yaml")
	if err != nil {
		return nil, err
	}
	c, err := card.Parse(readme, standalone)
	if err != nil && !errors.Is(err, card.ErrInvalid) {
		err = fmt.Errorf("%w: %w", card.ErrInvalid, err)
	}
	return c, err
}

func readSmall(ctx context.Context, repos repo.Service, id model.RepoID, commit model.CommitSHA, p string) ([]byte, error) {
	rc, _, err := repos.OpenFile(ctx, id, commit, p, nil)
	if errors.Is(err, repo.ErrEntryNotFound) || errors.Is(err, repo.ErrNotPresent) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, MaxCardBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > MaxCardBytes {
		return nil, fmt.Errorf("%w: %s is larger than %d bytes", ErrCardTooLarge, p, MaxCardBytes)
	}
	return b, nil
}

// ListFiles returns every file of a Commit. Zip archives list their
// entries by reading their central directory from the Repo when Resolve
// needs them.
func ListFiles(ctx context.Context, repos repo.Service, id model.RepoID, commit model.CommitSHA) ([]File, error) {
	entries, err := repos.Tree(ctx, id, commit, "", repo.TreeOpts{Recursive: true})
	if err != nil {
		return nil, err
	}
	out := make([]File, 0, len(entries))
	for _, e := range entries {
		if e.Type != model.EntryFile {
			continue
		}
		f := File{Path: e.Path, Size: e.Size}
		if strings.HasSuffix(e.Path, ".zip") {
			p, size := e.Path, e.Size
			f.Entries = func(fn func(name string) error) error {
				return ZipEntries(OpenRepoFile(ctx, repos, id, commit, p, size), size, fn)
			}
		}
		out = append(out, f)
	}
	return out, nil
}

// Resolve implements datasets.Layout.
func (s *Service) Resolve(ctx context.Context, id model.RepoID, commit model.CommitSHA) (*datasets.Resolved, error) {
	files, err := ListFiles(ctx, s.Repos, id, commit)
	if err != nil {
		if errors.Is(err, repo.ErrNotFound) || errors.Is(err, repo.ErrRevisionNotFound) {
			return nil, fmt.Errorf("%w: %w", datasets.ErrNotFound, err)
		}
		return nil, err
	}
	c, err := ReadCard(ctx, s.Repos, id, commit)
	if err != nil {
		return nil, err
	}
	return Resolve(files, c)
}

// Files implements datasets.Layout. For source selections it returns the
// data files of the selected Splits at the layout's Commit, with the card
// and the metadata files folder builders read; Selections are kept by name,
// so resolving again at a newer Commit follows new files. For Parquet-only
// selections it returns patterns in the Hub's refs/convert/parquet layout.
func (s *Service) Files(_ context.Context, layout *datasets.Resolved, sel datasets.Selection) ([]string, error) {
	for _, want := range sel.Configs {
		if _, ok := layout.Config(want); !ok {
			return nil, fmt.Errorf("%w: config %s", datasets.ErrNotFound, want)
		}
	}
	var out []string
	for _, c := range layout.Configs {
		if len(sel.Configs) > 0 && !slices.Contains(sel.Configs, c.Name) {
			continue
		}
		for _, split := range c.Splits() {
			if len(sel.Splits) > 0 && !slices.Contains(sel.Splits, split) {
				continue
			}
			if sel.ParquetOnly {
				out = append(out,
					path.Join(c.Name, split, "*.parquet"),
					path.Join(c.Name, "partial-"+split, "*.parquet"),
					path.Join(c.Name, split+"-part*", "*.parquet"))
				continue
			}
			if files, ok := c.Files[split]; ok {
				out = append(out, files...)
			} else {
				out = append(out, c.DataFiles[split]...)
			}
		}
	}
	if !sel.ParquetOnly && len(out) > 0 {
		out = append(out, "README.md")
	}
	sort.Strings(out)
	return slices.Compact(out), nil
}

// ModuleForExtension returns the `datasets` builder that reads files with
// an extension (with its leading dot), or "".
func ModuleForExtension(ext string) string {
	return extensionModules[ext].name
}
