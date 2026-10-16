package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/api"
	"github.com/jamesagarside/hangar/internal/api/gen"
	"github.com/jamesagarside/hangar/internal/datasets/layout"
	"github.com/jamesagarside/hangar/internal/datasets/viewertest"
	"github.com/jamesagarside/hangar/internal/db"
	"github.com/jamesagarside/hangar/internal/db/dbtest"
	"github.com/jamesagarside/hangar/internal/httpx"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/repo"
)

func configsRequest(t *testing.T, a *api.DatasetsAPI, ns, name string, rev *string, p model.Principal) (*httptest.ResponseRecorder, gen.DatasetConfigs) {
	t.Helper()
	ctx := httpx.WithInfo(context.Background(), "test")
	httpx.SetPrincipal(ctx, p)
	req := httptest.NewRequest(http.MethodGet, "/api/hangar/v1/repos/datasets/"+ns+"/"+name+"/configs", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	a.ListDatasetConfigs(rec, req, ns, name, gen.ListDatasetConfigsParams{Rev: rev})
	var out gen.DatasetConfigs
	if rec.Code == http.StatusOK {
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	}
	return rec, out
}

// TestListDatasetConfigs runs on SQLite and, with HANGAR_TEST_POSTGRES=1,
// on Postgres.
func TestListDatasetConfigs(t *testing.T) {
	t.Parallel()
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		s := viewertest.New(t, viewertest.Options{DB: d})
		a, err := api.NewDatasetsAPI(api.DatasetsDeps{Repos: s.Repos, Authz: s.Authz, Layout: &layout.Service{Repos: s.Repos},
			Store: s.Pipeline.Store(), Jobs: s.Jobs})
		require.NoError(t, err)
		_, err = api.NewDatasetsAPI(api.DatasetsDeps{})
		require.Error(t, err)

		r, sha := s.Dataset(t, "cfg", map[string][]byte{
			"README.md":   []byte("---\nconfigs:\n- config_name: main\n  data_files:\n  - split: train\n    path: a.csv\n---\n"),
			"a.csv":       []byte("x\n1\n2\n"),
			"broken.json": []byte("{"),
		})
		anon := model.Anonymous()

		// Before conversion every Split is pending, with the Job that converts it.
		rec, out := configsRequest(t, a, "ns", "cfg", nil, anon)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.Equal(t, string(sha), out.Commit)
		require.True(t, out.FromCard)
		require.Len(t, out.Configs, 1)
		require.Equal(t, "main", out.Configs[0].Config)
		require.True(t, out.Configs[0].Default)
		require.Equal(t, gen.DatasetSplitStatusPending, out.Configs[0].Splits[0].Status)
		require.NotNil(t, out.Configs[0].Splits[0].JobId)

		s.RunJobs(t)
		rec, out = configsRequest(t, a, "ns", "cfg", nil, anon)
		require.Equal(t, http.StatusOK, rec.Code)
		split := out.Configs[0].Splits[0]
		require.Equal(t, gen.DatasetSplitStatusReady, split.Status)
		require.EqualValues(t, 2, *split.NumRows)
		require.Nil(t, split.JobId)

		rev := "main"
		rec, _ = configsRequest(t, a, "ns", "cfg", &rev, anon)
		require.Equal(t, http.StatusOK, rec.Code)
		bad := "nope"
		rec, _ = configsRequest(t, a, "ns", "cfg", &bad, anon)
		require.Equal(t, http.StatusNotFound, rec.Code)
		rec, _ = configsRequest(t, a, "ns", "missing", nil, anon)
		require.Equal(t, http.StatusUnauthorized, rec.Code)
		rec, _ = configsRequest(t, a, "ns", "missing", nil, model.Principal{Kind: model.PrincipalUser, Username: "u"})
		require.Equal(t, http.StatusNotFound, rec.Code)
		rec, _ = configsRequest(t, a, "ns", "--bad--", nil, anon)
		require.Equal(t, http.StatusBadRequest, rec.Code)

		// A dataset without data files reports why.
		empty, emptySHA := s.Dataset(t, "empty", map[string][]byte{"README.md": []byte("# nothing\n")})
		rec, _ = configsRequest(t, a, "ns", "empty", nil, anon)
		require.Equal(t, http.StatusUnprocessableEntity, rec.Code)
		require.Contains(t, rec.Body.String(), "Hangar cannot read this dataset's Configs and Splits: The dataset is empty.")
		s.Convert(t, empty, emptySHA)
		rec, _ = configsRequest(t, a, "ns", "empty", nil, anon)
		require.Equal(t, http.StatusUnprocessableEntity, rec.Code)
		require.Contains(t, rec.Body.String(), "Hangar cannot read this dataset's Configs and Splits: The dataset is empty.")

		// A failed Split gives the Hub's message, not the conversion error.
		xml, xmlSHA := s.Dataset(t, "xml", map[string][]byte{"train.xml": []byte("<rows/>\n")})
		s.Convert(t, xml, xmlSHA)
		rec, out = configsRequest(t, a, "ns", "xml", nil, anon)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		split = out.Configs[0].Splits[0]
		require.Equal(t, gen.DatasetSplitStatusFailed, split.Status)
		require.Equal(t, "The dataset generation failed", *split.Error)

		// Private datasets are hidden.
		id := model.RepoID{Type: model.RepoTypeDataset, Namespace: "other", Name: "secret"}
		_, err = s.Repos.Create(context.Background(), repo.CreateParams{ID: id, Visibility: model.VisibilityPrivate, Actor: model.System()})
		require.NoError(t, err)
		rec, _ = configsRequest(t, a, "other", "secret", nil, anon)
		require.Equal(t, http.StatusUnauthorized, rec.Code)
		_ = r
	})
}
