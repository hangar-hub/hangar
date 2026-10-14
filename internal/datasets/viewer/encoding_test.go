package viewer_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/datasets"
	"github.com/jamesagarside/hangar/internal/datasets/media"
	"github.com/jamesagarside/hangar/internal/datasets/viewertest"
	"github.com/jamesagarside/hangar/internal/db"
	"github.com/jamesagarside/hangar/internal/db/dbtest"
	"github.com/jamesagarside/hangar/internal/model"
)

// TestCellsMatchTheHub reads testdata/cells.parquet through /rows and
// compares every row, byte for byte, with what the Hub's viewer writes
// for it (testdata/cells.json, from compat/viewer/oracle/cells_cases.py).
func TestCellsMatchTheHub(t *testing.T) {
	pq, err := os.ReadFile("testdata/cells.parquet")
	require.NoError(t, err)
	raw, err := os.ReadFile("testdata/cells.json")
	require.NoError(t, err)
	var want struct {
		Rows []string `json:"rows"`
	}
	require.NoError(t, json.Unmarshal(raw, &want))

	s := viewertest.New(t, viewertest.Options{})
	r, sha := s.Dataset(t, "cells", map[string][]byte{"train.parquet": pq})
	s.Convert(t, r, sha)
	for _, endpoint := range []string{"/rows", "/first-rows"} {
		res := get(t, s, endpoint, q("dataset", r.ID.String(), "config", "default", "split", "train"), "")
		require.Equal(t, http.StatusOK, res.status, string(res.raw))
		var body struct {
			Rows []struct {
				Row json.RawMessage `json:"row"`
			} `json:"rows"`
		}
		require.NoError(t, json.Unmarshal(res.raw, &body))
		require.Len(t, body.Rows, len(want.Rows))
		for i, row := range body.Rows {
			require.Equal(t, want.Rows[i], string(row.Row), "%s row %d", endpoint, i)
		}
	}
}

func TestNestedMedia(t *testing.T) {
	pq, err := os.ReadFile("testdata/media.parquet")
	require.NoError(t, err)
	s := viewertest.New(t, viewertest.Options{})
	r, sha := s.Dataset(t, "media", map[string][]byte{"train.parquet": pq})
	s.Convert(t, r, sha)
	res := get(t, s, "/rows", q("dataset", r.ID.String(), "config", "default", "split", "train"), "")
	require.Equal(t, http.StatusOK, res.status, string(res.raw))
	rows := res.body["rows"].([]any)
	row := rows[0].(map[string]any)["row"].(map[string]any)

	png, err := os.ReadFile("../media/testdata/i.png")
	require.NoError(t, err)
	gif, err := os.ReadFile("../media/testdata/i.gif")
	require.NoError(t, err)
	wav, err := os.ReadFile("../media/testdata/a.wav")
	require.NoError(t, err)
	pw, ph, _, err := media.ImageSize(png)
	require.NoError(t, err)
	prefix := s.URL + "/assets/ns/media/--/" + string(sha) + "/--/default/train/0/"

	pics := row["pics"].([]any)
	require.Len(t, pics, 2)
	first := pics[0].(map[string]any)
	require.EqualValues(t, pw, first["width"])
	require.EqualValues(t, ph, first["height"])
	// The JSON path [0] is hashed into the name as the Hub does.
	require.True(t, strings.HasPrefix(first["src"].(string), prefix+"pics/image-1d100e9.png?"), first["src"])
	second := pics[1].(map[string]any)
	require.True(t, strings.HasPrefix(second["src"].(string), prefix+"pics/image-1d300ea.gif?"), second["src"])
	img := row["meta"].(map[string]any)["img"].(map[string]any)
	require.True(t, strings.HasPrefix(img["src"].(string), prefix+"meta/image-8e7023a.png?"), img["src"])
	require.EqualValues(t, 1, row["meta"].(map[string]any)["n"])
	sound := row["sound"].([]any)[0].(map[string]any)
	require.Equal(t, "audio/wav", sound["type"])
	require.True(t, strings.HasPrefix(sound["src"].(string), prefix+"sound/audio.wav?"), sound["src"])

	for src, body := range map[string][]byte{second["src"].(string): gif, img["src"].(string): png, sound["src"].(string): wav} {
		res, err := http.Get(src)
		require.NoError(t, err)
		got, err := io.ReadAll(res.Body)
		require.NoError(t, err)
		require.NoError(t, res.Body.Close())
		require.Equal(t, http.StatusOK, res.StatusCode, src)
		require.Equal(t, body, got, src)
		require.Equal(t, "default-src 'none'; sandbox", res.Header.Get("Content-Security-Policy"))
	}

	// Empty and missing media cells are null.
	row = rows[1].(map[string]any)["row"].(map[string]any)
	require.Equal(t, []any{}, row["pics"])
	require.Nil(t, row["meta"].(map[string]any)["img"])
	require.Nil(t, row["sound"])

	// Asset URLs that point at nothing are not found.
	bad := strings.Replace(img["src"].(string), "/train/0/", "/train/9/", 1)
	res2, err := http.Get(bad)
	require.NoError(t, err)
	require.NoError(t, res2.Body.Close())
	require.Equal(t, http.StatusForbidden, res2.StatusCode, "the signature covers the row")
}

func TestViewerInterface(t *testing.T) {
	s := viewertest.New(t, viewertest.Options{})
	r, sha := s.Dataset(t, "iface", map[string][]byte{
		"train.csv": []byte(reviewsCSV),
		"test.csv":  []byte("id,review,stars,ok\n9,meh,2,False\n"),
	})
	ctx := context.Background()
	anon := model.Anonymous()
	var v datasets.Viewer = s.Viewer

	// Before conversion the dataset is pending.
	_, err := v.Rows(ctx, anon, datasets.RowsRequest{Repo: r.ID, Config: "default", Split: "train", Length: 10})
	require.ErrorIs(t, err, datasets.ErrNotReady)
	s.Convert(t, r, sha)

	valid, err := v.IsValid(ctx, anon, r.ID)
	require.NoError(t, err)
	require.Equal(t, datasets.IsValidResponse{Preview: true, Viewer: true, Search: true, Filter: true, Statistics: true}, *valid)
	splits, err := v.Splits(ctx, anon, r.ID)
	require.NoError(t, err)
	require.Len(t, splits.Splits, 2)
	first, err := v.FirstRows(ctx, anon, r.ID, "default", "train")
	require.NoError(t, err)
	require.Len(t, first.Rows, 4)
	require.False(t, first.Truncated)
	rows, err := v.Rows(ctx, anon, datasets.RowsRequest{Repo: r.ID, Config: "default", Split: "train", Offset: 1, Length: 2})
	require.NoError(t, err)
	require.EqualValues(t, 4, rows.NumRowsTotal)
	require.Len(t, rows.Rows, 2)
	require.EqualValues(t, 1, rows.Rows[0].RowIdx)
	found, err := v.Search(ctx, anon, datasets.SearchRequest{RowsRequest: datasets.RowsRequest{Repo: r.ID, Config: "default", Split: "train", Length: 10}, Query: "great"})
	require.NoError(t, err)
	require.EqualValues(t, 2, found.NumRowsTotal)
	filtered, err := v.Filter(ctx, anon, datasets.FilterRequest{RowsRequest: datasets.RowsRequest{Repo: r.ID, Config: "default", Split: "train", Length: 10},
		Where: `"stars" >= 3`, OrderBy: `"stars" DESC`})
	require.NoError(t, err)
	require.EqualValues(t, 3, filtered.NumRowsTotal)
	stars, _ := filtered.Rows[0].Row.Get("stars")
	require.EqualValues(t, 5, stars)
	_, err = v.Filter(ctx, anon, datasets.FilterRequest{RowsRequest: datasets.RowsRequest{Repo: r.ID, Config: "default", Split: "train", Length: 10}, Where: `"nope" = 1`})
	require.Error(t, err)

	all, err := v.Parquet(ctx, anon, r.ID, "")
	require.NoError(t, err)
	require.Len(t, all.ParquetFiles, 2)
	one, err := v.Parquet(ctx, anon, r.ID, "default")
	require.NoError(t, err)
	require.NotEmpty(t, one.Features)
	_, err = v.Parquet(ctx, anon, r.ID, "missing")
	require.ErrorIs(t, err, datasets.ErrNotFound)

	for name, call := range map[string]func(string) (json.RawMessage, error){
		"info": func(c string) (json.RawMessage, error) { return v.Info(ctx, anon, r.ID, c) },
		"size": func(c string) (json.RawMessage, error) { return v.Size(ctx, anon, r.ID, c) },
	} {
		b, err := call("")
		require.NoError(t, err, name)
		require.True(t, json.Valid(b), name)
		b, err = call("default")
		require.NoError(t, err, name)
		require.True(t, json.Valid(b), name)
		_, err = call("missing")
		require.ErrorIs(t, err, datasets.ErrNotFound, name)
	}
	st, err := v.Statistics(ctx, anon, r.ID, "default", "test")
	require.NoError(t, err)
	require.Contains(t, string(st), `"num_examples":1`)
	_, err = v.Statistics(ctx, anon, r.ID, "default", "missing")
	require.ErrorIs(t, err, datasets.ErrNotFound)
	cr, err := v.Croissant(ctx, anon, r.ID)
	require.NoError(t, err)
	require.Contains(t, string(cr), `"conformsTo":"http://mlcommons.org/croissant/1.0"`)

	// Unknown and private datasets are not found, whoever asks.
	missing := model.RepoID{Type: model.RepoTypeDataset, Namespace: "ns", Name: "missing"}
	_, err = v.IsValid(ctx, anon, missing)
	require.True(t, errors.Is(err, datasets.ErrNotFound), err)
}

// TestViewerOnEachDriver runs the Viewer over SQLite and, with
// HANGAR_TEST_POSTGRES=1, Postgres, whose jsonb would reorder keys if the
// store kept documents as jsonb.
func TestViewerOnEachDriver(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		s := viewertest.New(t, viewertest.Options{DB: d})
		r, sha := s.Dataset(t, "drivers", map[string][]byte{"train.csv": []byte(reviewsCSV)})
		s.Convert(t, r, sha)
		ds := r.ID.String()
		res := get(t, s, "/rows", q("dataset", ds, "config", "default", "split", "train"), "")
		require.Equal(t, http.StatusOK, res.status, string(res.raw))
		require.Contains(t, string(res.raw), `"row":{"id":1,"review":"Great app","stars":5,"ok":true}`)
		res = get(t, s, "/info", q("dataset", ds), "")
		require.Equal(t, http.StatusOK, res.status)
		require.Contains(t, string(res.raw), `"features":{"id":{"dtype":"int64","_type":"Value"},"review":{"dtype":"string","_type":"Value"}`)
		res = get(t, s, "/filter", q("dataset", ds, "config", "default", "split", "train", "where", `"stars" > 3`), "")
		require.EqualValues(t, 2, res.body["num_rows_total"])
		res = get(t, s, "/statistics", q("dataset", ds, "config", "default", "split", "train"), "")
		require.Equal(t, http.StatusOK, res.status)
		require.EqualValues(t, 4, res.body["num_examples"])
	})
}
