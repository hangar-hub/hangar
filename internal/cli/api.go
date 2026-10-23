package cli

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/hangar-hub/hangar/internal/api/gen"
	"github.com/hangar-hub/hangar/internal/model"
)

// reach wraps a transport error from the generated client.
func (c *client) reach(err error) error {
	return fmt.Errorf("reach %s: %w", c.base.Host, err)
}

func (c *client) getRepo(ctx context.Context, id model.RepoID) (*gen.Repo, error) {
	r, err := c.api.GetRepoWithResponse(ctx, plural(id), id.Namespace, id.Name)
	if err != nil {
		return nil, c.reach(err)
	}
	if err := check(r.HTTPResponse, r.Body, http.StatusOK); err != nil {
		return nil, err
	}
	return r.JSON200, nil
}

func (c *client) createRepo(ctx context.Context, body gen.RepoCreate) (*gen.Repo, error) {
	r, err := c.api.CreateRepoWithResponse(ctx, body)
	if err != nil {
		return nil, c.reach(err)
	}
	if err := check(r.HTTPResponse, r.Body, http.StatusCreated); err != nil {
		return nil, err
	}
	return r.JSON201, nil
}

// tree lists every entry under the root at a Revision, following cursors.
func (c *client) tree(ctx context.Context, id model.RepoID, rev string) (commit string, entries []gen.TreeEntry, err error) {
	recursive := true
	limit := 500
	var cursor *string
	for {
		params := &gen.GetTreeRootParams{Recursive: &recursive, Limit: &limit, Cursor: cursor}
		r, err := c.api.GetTreeRootWithResponse(ctx, plural(id), id.Namespace, id.Name, rev, params)
		if err != nil {
			return "", nil, c.reach(err)
		}
		if err := check(r.HTTPResponse, r.Body, http.StatusOK); err != nil {
			return "", nil, err
		}
		commit = r.JSON200.Commit
		for _, e := range r.JSON200.Items {
			if e.Type == gen.File {
				entries = append(entries, e)
			}
		}
		if r.JSON200.NextCursor == nil || *r.JSON200.NextCursor == "" {
			return commit, entries, nil
		}
		cursor = r.JSON200.NextCursor
	}
}

func (c *client) quants(ctx context.Context, id model.RepoID, rev string) (*gen.QuantList, error) {
	params := &gen.ListQuantsParams{}
	if rev != "" {
		params.Rev = &rev
	}
	r, err := c.api.ListQuantsWithResponse(ctx, plural(id), id.Namespace, id.Name, params)
	if err != nil {
		return nil, c.reach(err)
	}
	if err := check(r.HTTPResponse, r.Body, http.StatusOK); err != nil {
		return nil, err
	}
	return r.JSON200, nil
}

func (c *client) dependencies(ctx context.Context, id model.RepoID) (*gen.Dependencies, error) {
	r, err := c.api.GetDependenciesWithResponse(ctx, plural(id), id.Namespace, id.Name)
	if err != nil {
		return nil, c.reach(err)
	}
	if err := check(r.HTTPResponse, r.Body, http.StatusOK); err != nil {
		return nil, err
	}
	return r.JSON200, nil
}

func (c *client) datasetConfigs(ctx context.Context, id model.RepoID, rev string) (*gen.DatasetConfigs, error) {
	params := &gen.ListDatasetConfigsParams{}
	if rev != "" {
		params.Rev = &rev
	}
	r, err := c.api.ListDatasetConfigsWithResponse(ctx, id.Namespace, id.Name, params)
	if err != nil {
		return nil, c.reach(err)
	}
	if err := check(r.HTTPResponse, r.Body, http.StatusOK); err != nil {
		return nil, err
	}
	return r.JSON200, nil
}

func (c *client) discoverDetail(ctx context.Context, id model.RepoID, upstream, rev string) (*gen.DiscoverDetail, error) {
	params := &gen.GetDiscoverDetailParams{}
	if upstream != "" {
		params.Upstream = &upstream
	}
	if rev != "" {
		params.Rev = &rev
	}
	r, err := c.api.GetDiscoverDetailWithResponse(ctx, plural(id), id.Namespace, id.Name, params)
	if err != nil {
		return nil, c.reach(err)
	}
	if err := check(r.HTTPResponse, r.Body, http.StatusOK); err != nil {
		return nil, err
	}
	return r.JSON200, nil
}

func (c *client) startPull(ctx context.Context, body gen.PullCreate) (*gen.Job, error) {
	r, err := c.api.StartPullWithResponse(ctx, body)
	if err != nil {
		return nil, c.reach(err)
	}
	if err := check(r.HTTPResponse, r.Body, http.StatusAccepted); err != nil {
		return nil, err
	}
	return r.JSON202, nil
}

// pullIntoMirror pulls files into a Mirror at a Revision of the upstream
// Repo, or at the Mirror's tracked branch when revision is empty. An empty
// Paths selection pulls history only.
func (c *client) pullIntoMirror(ctx context.Context, id model.RepoID, revision string, sel gen.Selection) (*gen.Job, error) {
	params := &gen.PullMoreIntoMirrorParams{}
	if revision != "" {
		params.Revision = &revision
	}
	r, err := c.api.PullMoreIntoMirrorWithResponse(ctx, plural(id), id.Namespace, id.Name, params, sel)
	if err != nil {
		return nil, c.reach(err)
	}
	if err := check(r.HTTPResponse, r.Body, http.StatusAccepted); err != nil {
		return nil, err
	}
	return r.JSON202, nil
}

func (c *client) job(ctx context.Context, id openapi_types.UUID) (*gen.Job, error) {
	r, err := c.api.GetJobWithResponse(ctx, id)
	if err != nil {
		return nil, c.reach(err)
	}
	if err := check(r.HTTPResponse, r.Body, http.StatusOK); err != nil {
		return nil, err
	}
	return r.JSON200, nil
}

func (c *client) planUploads(ctx context.Context, id model.RepoID, body gen.UploadPlanRequest) (*gen.UploadPlan, error) {
	r, err := c.api.PlanUploadsWithResponse(ctx, plural(id), id.Namespace, id.Name, body)
	if err != nil {
		return nil, c.reach(err)
	}
	if err := check(r.HTTPResponse, r.Body, http.StatusOK); err != nil {
		return nil, err
	}
	return r.JSON200, nil
}

func (c *client) uploadStatus(ctx context.Context, id model.RepoID, upload openapi_types.UUID) (*gen.UploadStatus, error) {
	r, err := c.api.GetUploadWithResponse(ctx, plural(id), id.Namespace, id.Name, upload)
	if err != nil {
		return nil, c.reach(err)
	}
	if err := check(r.HTTPResponse, r.Body, http.StatusOK); err != nil {
		return nil, err
	}
	return r.JSON200, nil
}

func (c *client) completeUpload(ctx context.Context, id model.RepoID, upload openapi_types.UUID, parts []gen.UploadedPart) (*gen.UploadStatus, error) {
	r, err := c.api.CompleteUploadWithResponse(ctx, plural(id), id.Namespace, id.Name, upload, gen.UploadComplete{Parts: parts})
	if err != nil {
		return nil, c.reach(err)
	}
	if err := check(r.HTTPResponse, r.Body, http.StatusOK); err != nil {
		return nil, err
	}
	return r.JSON200, nil
}

// ensureBranch creates a branch from the default branch through the Hub API,
// as `hf upload --revision` does. A branch that exists already (409) is
// left as it is.
func (c *client) ensureBranch(ctx context.Context, id model.RepoID, branch string) (created bool, err error) {
	resp, err := c.do(ctx, http.MethodPost, c.url(branchPath(id, branch)), strings.NewReader("{}"),
		http.Header{"Content-Type": {"application/json"}}, http.StatusOK, http.StatusConflict)
	if err != nil {
		return false, err
	}
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK, nil
}

func (c *client) createCommit(ctx context.Context, id model.RepoID, body gen.CommitRequest) (*gen.CommitResponse, error) {
	r, err := c.api.CreateCommitWithResponse(ctx, plural(id), id.Namespace, id.Name, body)
	if err != nil {
		return nil, c.reach(err)
	}
	if err := check(r.HTTPResponse, r.Body, http.StatusCreated); err != nil {
		return nil, err
	}
	return r.JSON201, nil
}

func (c *client) ollamaKeys(ctx context.Context) ([]gen.OllamaKey, error) {
	r, err := c.api.ListMyOllamaKeysWithResponse(ctx)
	if err != nil {
		return nil, c.reach(err)
	}
	if err := check(r.HTTPResponse, r.Body, http.StatusOK); err != nil {
		return nil, err
	}
	return r.JSON200.Items, nil
}

func (c *client) addOllamaKey(ctx context.Context, body gen.OllamaKeyCreate) (*gen.OllamaKey, error) {
	r, err := c.api.AddMyOllamaKeyWithResponse(ctx, body)
	if err != nil {
		return nil, c.reach(err)
	}
	if err := check(r.HTTPResponse, r.Body, http.StatusCreated); err != nil {
		return nil, err
	}
	return r.JSON201, nil
}

// ptr returns a pointer to v.
func ptr[T any](v T) *T { return &v }

// deref returns *p, or the zero value for nil.
func deref[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}
