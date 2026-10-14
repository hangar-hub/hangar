package viewertest_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/datasets/viewertest"
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
