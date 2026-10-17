package cli

import (
	"net/url"
	"strings"

	"github.com/jamesagarside/hangar/internal/api/gen"
	"github.com/jamesagarside/hangar/internal/model"
)

// repoRef is a Repo ID as typed on the command line, with an optional quant.
type repoRef struct {
	ID    model.RepoID
	Quant string
}

// parseRepoRef parses `[models/|datasets/]ns/name[:quant]`. typeFlag, when
// set, gives the type for an ID without a prefix (DATA-12: a `datasets/`
// prefix works everywhere the CLI takes a Repo ID).
func parseRepoRef(arg, typeFlag string, allowQuant bool) (repoRef, error) {
	t := model.RepoTypeModel
	if typeFlag != "" {
		pt, err := model.ParseRepoType(typeFlag)
		if err != nil {
			return repoRef{}, usagef("--type must be model or dataset, not %q", typeFlag)
		}
		t = pt
	}
	s := strings.TrimSpace(arg)
	switch {
	case strings.HasPrefix(s, "datasets/"):
		t, s = model.RepoTypeDataset, strings.TrimPrefix(s, "datasets/")
	case strings.HasPrefix(s, "models/"):
		t, s = model.RepoTypeModel, strings.TrimPrefix(s, "models/")
	}
	var quant string
	if i := strings.LastIndexByte(s, ':'); i >= 0 {
		if !allowQuant {
			return repoRef{}, usagef("%q: this command does not take a :quant", arg)
		}
		s, quant = s[:i], s[i+1:]
		if quant == "" {
			return repoRef{}, usagef("%q: the quant after : is empty", arg)
		}
	}
	id, err := model.ParseRepoID(t, s)
	if err != nil {
		return repoRef{}, usagef("%q is not a Repo ID; use the form namespace/name", arg)
	}
	return repoRef{ID: id, Quant: quant}, nil
}

// plural returns the native API path type.
func plural(id model.RepoID) gen.RepoTypePlural {
	return gen.RepoTypePlural(id.Type.Plural())
}

// singular returns the native API body type.
func singular(id model.RepoID) gen.RepoTypeSingular {
	return gen.RepoTypeSingular(id.Type)
}

// display returns the ID as shown to users: `datasets/ns/name` for datasets.
func display(id model.RepoID) string {
	return id.Path()
}

// escapePath percent-encodes a Repo path segment by segment, keeping `/`.
func escapePath(p string) string {
	parts := strings.Split(p, "/")
	for i, s := range parts {
		parts[i] = url.PathEscape(s)
	}
	return strings.Join(parts, "/")
}

// resolvePath returns the Hub download path of a file at a Commit.
// branchPath returns the Hub API path that creates or deletes a branch.
func branchPath(id model.RepoID, branch string) string {
	return "/api/" + id.Type.Plural() + "/" + escapePath(id.Namespace) + "/" + escapePath(id.Name) +
		"/branch/" + url.PathEscape(branch)
}

func resolvePath(id model.RepoID, rev, file string) string {
	return "/" + id.Type.URLPrefix() + escapePath(id.Namespace) + "/" + escapePath(id.Name) +
		"/resolve/" + url.PathEscape(rev) + "/" + escapePath(file)
}
