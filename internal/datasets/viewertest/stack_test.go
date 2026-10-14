package viewertest_test

import (
	"context"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/datasets/viewertest"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/repo"
	"github.com/jamesagarside/hangar/internal/repo/repotest"
)

func TestStackServesAConvertedDataset(t *testing.T) {
	s := viewertest.New(t, viewertest.Options{MaxConvertBytes: 1 << 20})
	r, sha := s.Dataset(t, "demo", map[string][]byte{"train.csv": []byte("a,b\n1,x\n2,y\n")})
	require.Equal(t, "ns/demo", r.ID.String())
	s.Convert(t, r, sha)

	res, err := http.Get(s.URL + "/rows?dataset=ns/demo&config=default&split=train")
	require.NoError(t, err)
	defer res.Body.Close()
	require.Equal(t, http.StatusOK, res.StatusCode)
	require.Equal(t, string(sha), res.Header.Get("X-Revision"))
	var body struct {
		NumRowsTotal int `json:"num_rows_total"`
	}
	require.NoError(t, json.NewDecoder(res.Body).Decode(&body))
	require.Equal(t, 2, body.NumRowsTotal)
	s.RunJobs(t)
}

// countingRepos counts the Resolve calls the Viewer makes.
type countingRepos struct {
	repo.Service
	resolves atomic.Int64
}

func (c *countingRepos) Resolve(ctx context.Context, id model.RepoID, revision string) (model.CommitSHA, error) {
	c.resolves.Add(1)
	return c.Service.Resolve(ctx, id, revision)
}

func TestViewerReposWrapsOnlyTheViewer(t *testing.T) {
	counter := &countingRepos{}
	s := viewertest.New(t, viewertest.Options{ViewerRepos: func(r *repotest.Fake) repo.Service {
		counter.Service = r
		return counter
	}})
	r, sha := s.Dataset(t, "wrapped", map[string][]byte{"train.csv": []byte("a\n1\n")})
	s.Convert(t, r, sha)
	require.Zero(t, counter.resolves.Load(), "the pipeline uses the fake directly")

	res, err := http.Get(s.URL + "/is-valid?dataset=ns/wrapped")
	require.NoError(t, err)
	require.NoError(t, res.Body.Close())
	require.Equal(t, http.StatusOK, res.StatusCode)
	require.Positive(t, counter.resolves.Load())
}
