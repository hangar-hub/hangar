package api

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/jamesagarside/hangar/internal/api/gen"
	"github.com/jamesagarside/hangar/internal/auth"
	"github.com/jamesagarside/hangar/internal/datasets"
	"github.com/jamesagarside/hangar/internal/datasets/pipeline"
	"github.com/jamesagarside/hangar/internal/httpx"
	"github.com/jamesagarside/hangar/internal/jobs"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/repo"
)

// DatasetsDeps are the services behind the dataset endpoints.
type DatasetsDeps struct {
	Repos  repo.Service
	Authz  auth.Authz
	Layout datasets.Layout
	Store  *pipeline.Store
	Jobs   jobs.Queue
	Log    *slog.Logger
}

// DatasetsAPI serves /repos/datasets/{ns}/{name}/configs: the Configs and
// Splits of a dataset with their Parquet export status, for the Viewer
// tab (DATA-6) and the CLI (DATA-12).
type DatasetsAPI struct{ d DatasetsDeps }

// NewDatasetsAPI returns the dataset handlers.
func NewDatasetsAPI(d DatasetsDeps) (*DatasetsAPI, error) {
	if d.Repos == nil || d.Authz == nil || d.Layout == nil || d.Store == nil || d.Jobs == nil {
		return nil, errors.New("api: Repos, Authz, Layout, Store and Jobs are required for datasets")
	}
	if d.Log == nil {
		d.Log = slog.New(slog.DiscardHandler)
	}
	return &DatasetsAPI{d: d}, nil
}

func (a *DatasetsAPI) problem(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case clientGone(r, err):
		writeClientGone(w, r, a.d.Log, err)
	case errors.Is(err, repo.ErrNotFound), errors.Is(err, auth.ErrNotFound), errors.Is(err, auth.ErrForbidden), errors.Is(err, auth.ErrGated):
		status, detail := http.StatusNotFound, "Dataset not found. If it is private, make sure you are signed in and have access to it."
		if httpx.Principal(r.Context()).IsAnonymous() {
			status, detail = http.StatusUnauthorized, "Dataset not found. If it is private, sign in to see it."
		}
		httpx.WriteProblem(w, r, &httpx.Problem{Status: status, Detail: detail})
	case errors.Is(err, repo.ErrRevisionNotFound):
		httpx.WriteProblem(w, r, &httpx.Problem{Status: http.StatusNotFound, Detail: "Revision not found.",
			Errors: []httpx.ProblemError{{Parameter: "rev", Detail: "Revision not found."}}})
	case errors.Is(err, model.ErrInvalidRepoID):
		httpx.WriteProblem(w, r, &httpx.Problem{Status: http.StatusBadRequest, Detail: err.Error()})
	default:
		a.d.Log.ErrorContext(r.Context(), "dataset configs request failed", "error", err.Error())
		httpx.WriteProblem(w, r, &httpx.Problem{Status: http.StatusInternalServerError,
			Detail: httpx.InternalErrorMessage(r.Context())})
	}
}

// ListDatasetConfigs implements gen.ServerInterface.
func (a *DatasetsAPI) ListDatasetConfigs(w http.ResponseWriter, r *http.Request, ns gen.Ns, name gen.Name, params gen.ListDatasetConfigsParams) {
	ctx := r.Context()
	id := model.RepoID{Type: model.RepoTypeDataset, Namespace: ns, Name: name}
	if err := id.Validate(); err != nil {
		a.problem(w, r, err)
		return
	}
	rp, err := a.d.Repos.Get(ctx, id)
	if err != nil {
		a.problem(w, r, err)
		return
	}
	p := httpx.Principal(ctx)
	if err := a.d.Authz.Can(ctx, p, auth.ActionRead, auth.Target{Repo: &auth.RepoTarget{ID: rp.ID, UUID: rp.UUID,
		NamespaceID: rp.NamespaceID, Visibility: rp.Visibility, Gated: rp.Gating.Enabled(), Mirror: rp.Mirror}}); err != nil {
		a.problem(w, r, err)
		return
	}
	httpx.SetRepo(ctx, rp.ID)
	rev := rp.DefaultBranch
	if params.Rev != nil && *params.Rev != "" {
		rev = *params.Rev
	}
	commit, err := a.d.Repos.Resolve(ctx, rp.ID, rev)
	if err != nil {
		a.problem(w, r, err)
		return
	}
	out := gen.DatasetConfigs{Commit: string(commit), Configs: []gen.DatasetConfig{}}
	rows, err := a.d.Store.Splits(ctx, rp.UUID, commit)
	if err != nil {
		a.problem(w, r, err)
		return
	}
	type key struct{ c, s string }
	byKey := map[key]pipeline.SplitRow{}
	var datasetErr string
	for _, row := range rows {
		if row.Config == pipeline.DatasetLevel {
			datasetErr = pipeline.FailureOf(row.Error).Message
			continue
		}
		byKey[key{row.Config, row.Split}] = row
	}
	layout, lerr := a.d.Layout.Resolve(ctx, rp.ID, commit)
	if lerr != nil || datasetErr != "" {
		// The dataset has no Configs: say why, as a problem the UI shows.
		detail := datasetErr
		if detail == "" {
			if errors.Is(lerr, datasets.ErrNotFound) {
				a.problem(w, r, repo.ErrNotFound)
				return
			}
			code, ok := pipeline.LayoutFailure(lerr)
			if !ok {
				a.problem(w, r, lerr)
				return
			}
			detail = pipeline.FailureOf(code).Message
		}
		httpx.WriteProblem(w, r, &httpx.Problem{Status: http.StatusUnprocessableEntity, Title: "The dataset has no Configs",
			Detail: "Hangar cannot read this dataset's Configs and Splits: " + detail})
		return
	}
	out.FromCard = layout.FromCard
	var jobID *jobs.JobID
	isHead := rev == rp.DefaultBranch || string(commit) == rev
	for _, c := range layout.Configs {
		gc := gen.DatasetConfig{Config: c.Name, Default: c.Default, Splits: []gen.DatasetSplit{}}
		for _, s := range c.Splits() {
			gs := gen.DatasetSplit{Split: s, Status: gen.DatasetSplitStatusPending}
			if row, ok := byKey[key{c.Name, s}]; ok {
				gs.Status = gen.DatasetSplitStatus(row.Status)
				if row.Ready() {
					nr, ns, np := row.NumRows, row.NumBytesSource, row.NumBytesParquet
					gs.NumRows, gs.NumBytesSource, gs.NumBytesParquet = &nr, &ns, &np
				}
				if row.Error != "" {
					e := pipeline.FailureOf(row.Error).Message
					gs.Error = &e
				}
			}
			if gs.Status == gen.DatasetSplitStatusPending && isHead {
				if jobID == nil {
					id, err := pipeline.EnqueueConvert(ctx, a.d.Jobs, rp.UUID, commit)
					if err == nil {
						jobID = &id
					} else {
						a.d.Log.WarnContext(ctx, "could not queue the dataset conversion", "error", err.Error())
					}
				}
				gs.JobId = jobID
			}
			gc.Splits = append(gc.Splits, gs)
		}
		out.Configs = append(out.Configs, gc)
	}
	httpx.WriteJSON(w, r, http.StatusOK, out)
}
