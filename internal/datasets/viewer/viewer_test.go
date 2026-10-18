package viewer_test

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/blob"
	"github.com/jamesagarside/hangar/internal/datasets"
	"github.com/jamesagarside/hangar/internal/datasets/viewer"
	"github.com/jamesagarside/hangar/internal/datasets/viewertest"
	"github.com/jamesagarside/hangar/internal/jobs"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/repo"
	"github.com/jamesagarside/hangar/internal/repo/repotest"
)

type resp struct {
	status int
	header http.Header
	body   map[string]any
	raw    []byte
}

func get(t *testing.T, s *viewertest.Stack, path string, q url.Values, token string) resp {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, s.URL+path+"?"+q.Encode(), nil)
	require.NoError(t, err)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	out := resp{status: res.StatusCode, header: res.Header, raw: b}
	if strings.HasPrefix(res.Header.Get("Content-Type"), "application/json") {
		_ = json.Unmarshal(b, &out.body)
	}
	return out
}

func q(kv ...string) url.Values {
	v := url.Values{}
	for i := 0; i+1 < len(kv); i += 2 {
		v.Set(kv[i], kv[i+1])
	}
	return v
}

const reviewsCSV = "id,review,stars,ok\n1,Great app,5,True\n2,awful thing,1,False\n3,fine,3,True\n4,GREAT great,4,\n"

func TestViewerLifecycle(t *testing.T) {
	s := viewertest.New(t, viewertest.Options{})
	r, sha := s.Dataset(t, "reviews", map[string][]byte{
		"README.md":      []byte("---\nlicense: mit\npretty_name: Reviews\ntask_categories: [text-classification]\nlanguage: [en]\n---\n# Reviews\n\nApp **reviews**.\n"),
		"data/train.csv": []byte(reviewsCSV),
		"data/test.csv":  []byte("id,review,stars,ok\n9,meh,2,False\n"),
	})
	ds := r.ID.String()

	// Before conversion: the layout is known, previews come from the CSV.
	res := get(t, s, "/splits", q("dataset", ds), "")
	require.Equal(t, http.StatusOK, res.status, string(res.raw))
	require.Len(t, res.body["splits"], 2)
	require.Equal(t, string(sha), res.header.Get("X-Revision"))
	res = get(t, s, "/is-valid", q("dataset", ds), "")
	require.Equal(t, map[string]any{"preview": true, "viewer": false, "search": false, "filter": false, "statistics": false}, res.body)
	res = get(t, s, "/first-rows", q("dataset", ds, "config", "default", "split", "train"), "")
	require.Equal(t, http.StatusOK, res.status, string(res.raw))
	require.Len(t, res.body["rows"], 4)
	res = get(t, s, "/rows", q("dataset", ds, "config", "default", "split", "train"), "")
	require.Equal(t, http.StatusInternalServerError, res.status)
	require.Equal(t, "ResponseNotReady", res.header.Get("X-Error-Code"))
	res = get(t, s, "/info", q("dataset", ds), "")
	require.Equal(t, http.StatusOK, res.status)
	require.Equal(t, []any{map[string]any{"dataset": ds, "config": "default"}}, res.body["pending"])

	s.RunJobs(t) // the Viewer queued the conversion

	res = get(t, s, "/is-valid", q("dataset", ds), "")
	require.Equal(t, map[string]any{"preview": true, "viewer": true, "search": true, "filter": true, "statistics": true}, res.body)
	res = get(t, s, "/is-valid", q("dataset", ds, "config", "default", "split", "test"), "")
	require.Equal(t, true, res.body["viewer"])

	res = get(t, s, "/rows", q("dataset", ds, "config", "default", "split", "train", "offset", "1", "length", "2"), "")
	require.Equal(t, http.StatusOK, res.status, string(res.raw))
	require.Contains(t, string(res.raw), `"features":[{"feature_idx":0,"name":"id","type":{"dtype":"int64","_type":"Value"}}`)
	require.Contains(t, string(res.raw), `{"row_idx":1,"row":{"id":2,"review":"awful thing","stars":1,"ok":false},"truncated_cells":[]}`)
	require.EqualValues(t, 4, res.body["num_rows_total"])
	require.EqualValues(t, 100, res.body["num_rows_per_page"])
	require.Equal(t, false, res.body["partial"])
	res = get(t, s, "/rows", q("dataset", ds, "config", "default", "split", "train", "offset", "100"), "")
	require.Equal(t, []any{}, res.body["rows"])

	res = get(t, s, "/search", q("dataset", ds, "config", "default", "split", "train", "query", "great"), "")
	require.Equal(t, http.StatusOK, res.status, string(res.raw))
	require.EqualValues(t, 2, res.body["num_rows_total"])
	res = get(t, s, "/search", q("dataset", ds, "config", "default", "split", "train", "query", "great app"), "")
	require.EqualValues(t, 1, res.body["num_rows_total"])

	res = get(t, s, "/filter", q("dataset", ds, "config", "default", "split", "train", "where", `"stars" >= 3`, "orderby", `"stars" DESC`), "")
	require.Equal(t, http.StatusOK, res.status, string(res.raw))
	var idx []float64
	for _, row := range res.body["rows"].([]any) {
		idx = append(idx, row.(map[string]any)["row_idx"].(float64))
	}
	require.Equal(t, []float64{0, 3, 2}, idx)
	// Paging comes from the cached result.
	res = get(t, s, "/filter", q("dataset", ds, "config", "default", "split", "train", "where", `"stars" >= 3`, "orderby", `"stars" DESC`, "offset", "2"), "")
	require.Len(t, res.body["rows"], 1)
	res = get(t, s, "/filter", q("dataset", ds, "config", "default", "split", "train", "where", `"ok" IS NULL`), "")
	require.EqualValues(t, 1, res.body["num_rows_total"])
	res = get(t, s, "/filter", q("dataset", ds, "config", "default", "split", "train", "where", `stars > 3`), "")
	require.Equal(t, http.StatusUnprocessableEntity, res.status)
	require.Equal(t, "InvalidParameter", res.header.Get("X-Error-Code"))
	res = get(t, s, "/filter", q("dataset", ds, "config", "default", "split", "train", "where", `"nope" > 3`), "")
	require.Equal(t, http.StatusUnprocessableEntity, res.status)

	res = get(t, s, "/parquet", q("dataset", ds), "")
	require.Equal(t, http.StatusOK, res.status)
	files := res.body["parquet_files"].([]any)
	require.Len(t, files, 2)
	first := files[0].(map[string]any)
	require.Equal(t, "test", first["split"])
	require.Equal(t, s.Server.URL+"/datasets/ns/reviews/resolve/refs%2Fconvert%2Fparquet/default/test/0000.parquet", first["url"])
	res = get(t, s, "/parquet", q("dataset", ds, "config", "default"), "")
	require.Contains(t, res.body, "features")
	require.NotContains(t, res.body, "pending")

	res = get(t, s, "/info", q("dataset", ds), "")
	info := res.body["dataset_info"].(map[string]any)["default"].(map[string]any)
	require.Equal(t, "csv", info["builder_name"])
	require.Equal(t, "reviews", info["dataset_name"])
	require.Contains(t, info, "download_checksums")
	splits := info["splits"].(map[string]any)
	require.EqualValues(t, 4, splits["train"].(map[string]any)["num_examples"])
	res = get(t, s, "/info", q("dataset", ds, "config", "default"), "")
	require.Equal(t, "default", res.body["dataset_info"].(map[string]any)["config_name"])

	res = get(t, s, "/size", q("dataset", ds), "")
	size := res.body["size"].(map[string]any)
	require.EqualValues(t, 5, size["dataset"].(map[string]any)["num_rows"])
	require.Len(t, size["splits"], 2)
	res = get(t, s, "/size", q("dataset", ds, "config", "default"), "")
	require.Contains(t, res.body["size"], "config")

	res = get(t, s, "/statistics", q("dataset", ds, "config", "default", "split", "train"), "")
	require.Equal(t, http.StatusOK, res.status, string(res.raw))
	require.EqualValues(t, 4, res.body["num_examples"])
	require.Len(t, res.body["statistics"], 4)

	res = get(t, s, "/splits", q("dataset", ds, "config", "default"), "")
	require.Equal(t, []string{"splits"}, keys(res.body))
	res = get(t, s, "/opt-in-out-urls", q("dataset", ds), "")
	require.Equal(t, false, res.body["has_urls_columns"])
	res = get(t, s, "/presidio-entities", q("dataset", ds), "")
	require.Equal(t, true, res.body["full_scan"])

	cr, err := s.Viewer.Croissant(context.Background(), model.Anonymous(), r.ID)
	require.NoError(t, err)
	var doc map[string]any
	require.NoError(t, json.Unmarshal(cr, &doc))
	require.Equal(t, "Reviews", doc["name"])
	require.Contains(t, doc["description"], "App reviews.")
	require.Contains(t, doc["keywords"], "text-classification")
	require.Equal(t, "https://choosealicense.com/licenses/mit/", doc["license"])
}

func keys(m map[string]any) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestErrors(t *testing.T) {
	s := viewertest.New(t, viewertest.Options{})
	r, sha := s.Dataset(t, "e", map[string][]byte{"train.csv": []byte(reviewsCSV)})
	s.Convert(t, r, sha)
	ds := r.ID.String()
	cases := []struct {
		path   string
		q      url.Values
		status int
		code   string
	}{
		{"/rows", q(), 422, "MissingRequiredParameter"},
		{"/rows", q("dataset", ds), 422, "MissingRequiredParameter"},
		{"/rows", q("dataset", ds, "config", "default"), 422, "MissingRequiredParameter"},
		{"/rows", q("dataset", ds, "config", "default", "split", "train", "length", "101"), 422, "InvalidParameter"},
		{"/rows", q("dataset", ds, "config", "default", "split", "train", "offset", "-1"), 422, "InvalidParameter"},
		{"/rows", q("dataset", ds, "config", "default", "split", "train", "length", "x"), 422, "InvalidParameter"},
		{"/rows", q("dataset", ds, "config", "default", "split", "nope"), 404, "ResponseNotFound"},
		{"/statistics", q("dataset", ds, "config", "nope", "split", "train"), 404, "ResponseNotFound"},
		{"/info", q("dataset", ds, "config", "nope"), 404, "ResponseNotFound"},
		{"/splits", q("dataset", "ns/missing"), 404, "ExternalUnauthenticatedError"},
		{"/splits", q("dataset", "not-a-repo-id"), 404, "ExternalUnauthenticatedError"},
		{"/search", q("dataset", ds, "config", "default", "split", "train"), 422, "MissingRequiredParameter"},
		{"/nope", q("dataset", ds), 404, "ResponseNotFound"},
	}
	for _, c := range cases {
		res := get(t, s, c.path, c.q, "")
		require.Equal(t, c.status, res.status, "%s %v: %s", c.path, c.q, res.raw)
		require.Equal(t, c.code, res.header.Get("X-Error-Code"), "%s %v", c.path, c.q)
		require.Contains(t, res.body, "error")
	}
	res := get(t, s, "/healthcheck", q(), "")
	require.Equal(t, "ok", string(res.raw))
	req, _ := http.NewRequest(http.MethodPost, s.URL+"/rows", nil)
	post, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = post.Body.Close()
	require.Equal(t, http.StatusMethodNotAllowed, post.StatusCode)

	// A dataset without data files answers with the Hub's error.
	bad, badSHA := s.Dataset(t, "bad", map[string][]byte{"README.md": []byte("# no data\n")})
	s.Convert(t, bad, badSHA)
	res = get(t, s, "/splits", q("dataset", bad.ID.String()), "")
	require.Equal(t, http.StatusInternalServerError, res.status)
	require.Equal(t, "EmptyDatasetError", res.header.Get("X-Error-Code"))
	require.Equal(t, map[string]any{"error": "The dataset is empty."}, res.body)
	res = get(t, s, "/is-valid", q("dataset", bad.ID.String()), "")
	require.Equal(t, false, res.body["preview"])
}

// TestFailedConversion converts a dataset whose only Config cannot be
// converted, and checks every endpoint answers as the Hub does: /splits
// still lists the Split, with nothing failed, because the Hub's
// config-split-names step falls back to the data files when config-info
// failed; /info, /parquet and /size list the Config as failed; and the
// Split's endpoints answer with the Hub's DatasetGenerationError. None of
// them shows the conversion error's text, which can hold internal detail.
func TestFailedConversion(t *testing.T) {
	s := viewertest.New(t, viewertest.Options{})
	r, sha := s.Dataset(t, "xmlish", map[string][]byte{"train.xml": []byte("<rows><row a='1'/></rows>\n")})
	s.Convert(t, r, sha)
	ds := r.ID.String()
	generation := map[string]any{"error": "The dataset generation failed"}

	res := get(t, s, "/splits", q("dataset", ds), "")
	require.Equal(t, http.StatusOK, res.status, string(res.raw))
	require.Equal(t, []any{map[string]any{"dataset": ds, "config": "default", "split": "train"}}, res.body["splits"])
	require.Equal(t, []any{}, res.body["failed"])
	require.Equal(t, []any{}, res.body["pending"])
	res = get(t, s, "/splits", q("dataset", ds, "config", "default"), "")
	require.Equal(t, http.StatusOK, res.status, string(res.raw))

	failed := []any{map[string]any{"dataset": ds, "config": "default", "error": generation}}
	for _, path := range []string{"/info", "/parquet", "/size"} {
		res = get(t, s, path, q("dataset", ds), "")
		require.Equal(t, http.StatusOK, res.status, "%s: %s", path, res.raw)
		require.Equal(t, failed, res.body["failed"], path)
	}
	for _, c := range []struct {
		path string
		q    url.Values
	}{
		{"/first-rows", q("dataset", ds, "config", "default", "split", "train")},
		{"/rows", q("dataset", ds, "config", "default", "split", "train")},
		{"/search", q("dataset", ds, "config", "default", "split", "train", "query", "a")},
		{"/filter", q("dataset", ds, "config", "default", "split", "train", "where", `"a" = 1`)},
		{"/statistics", q("dataset", ds, "config", "default", "split", "train")},
		{"/info", q("dataset", ds, "config", "default")},
		{"/size", q("dataset", ds, "config", "default")},
	} {
		res = get(t, s, c.path, c.q, "")
		require.Equal(t, http.StatusInternalServerError, res.status, "%s: %s", c.path, res.raw)
		require.Equal(t, "DatasetGenerationError", res.header.Get("X-Error-Code"), c.path)
		require.Equal(t, generation, res.body, c.path)
	}
	res = get(t, s, "/is-valid", q("dataset", ds), "")
	require.Equal(t, map[string]any{"preview": false, "viewer": false, "search": false, "filter": false, "statistics": false}, res.body)
}

// TestReadErrorsAreNotFailures makes the Viewer's Repo service fail while
// it resolves a dataset's layout: the request fails with the Hub's
// UnexpectedApiError and no detail, and the dataset is not reported as
// failed once the Repo service recovers.
func TestReadErrorsAreNotFailures(t *testing.T) {
	broken := &flakyRepos{}
	s := viewertest.New(t, viewertest.Options{ViewerRepos: func(f *repotest.Fake) repo.Service {
		broken.Service = f
		return broken
	}})
	r, _ := s.Dataset(t, "flaky", map[string][]byte{"train.csv": []byte(reviewsCSV)})
	broken.fail.Store(true)
	res := get(t, s, "/splits", q("dataset", r.ID.String()), "")
	require.Equal(t, http.StatusInternalServerError, res.status)
	require.Equal(t, "UnexpectedApiError", res.header.Get("X-Error-Code"))
	require.Equal(t, map[string]any{"error": "Unexpected error."}, res.body)
	require.NotContains(t, string(res.raw), "10.0.0.7")
	broken.fail.Store(false)
	res = get(t, s, "/splits", q("dataset", r.ID.String()), "")
	require.Equal(t, http.StatusOK, res.status, string(res.raw))
	require.Len(t, res.body["splits"], 1)
}

// TestStalledConversionIsQueuedAgain checks that a conversion whose every
// attempt failed to read the data files leaves the Splits pending, and
// that the Viewer queues it again, once, when asked for them.
func TestStalledConversionIsQueuedAgain(t *testing.T) {
	broken := &unreadableData{}
	s := viewertest.New(t, viewertest.Options{PipelineRepos: func(f *repotest.Fake) repo.Service {
		broken.Service = f
		return broken
	}})
	now := time.Now()
	s.Jobs.SetClock(func() time.Time { return now })
	r, sha := s.Dataset(t, "stalled", map[string][]byte{"train.csv": []byte(reviewsCSV)})
	broken.fail.Store(true)
	ctx := context.Background()
	_, err := s.Pipeline.EnqueueConvert(ctx, r.UUID, sha)
	require.NoError(t, err)
	convertJobs := func() []*jobs.Job {
		list, _, err := s.Jobs.List(ctx, jobs.Filter{Kinds: []jobs.Kind{jobs.KindDatasetConvert}})
		require.NoError(t, err)
		return list
	}
	params := q("dataset", r.ID.String(), "config", "default", "split", "train", "offset", "0", "length", "10")
	for attempt := 1; attempt <= 3; attempt++ {
		s.RunJobs(t)
		now = now.Add(24 * time.Hour)
		if attempt == 3 {
			break
		}
		// A request while the Job waits to retry does not queue another.
		res := get(t, s, "/rows", params, "")
		require.Equal(t, http.StatusInternalServerError, res.status)
		require.Equal(t, "ResponseNotReady", res.header.Get("X-Error-Code"))
		require.Len(t, convertJobs(), 1)
	}
	list := convertJobs()
	require.Len(t, list, 1)
	require.Equal(t, jobs.StatusFailed, list[0].Status)
	require.Contains(t, list[0].Error, "read the data files of config default")

	// Every attempt failed; the Splits are still pending, not failed, so
	// the next request queues the conversion again.
	broken.fail.Store(false)
	res := get(t, s, "/rows", params, "")
	require.Equal(t, "ResponseNotReady", res.header.Get("X-Error-Code"))
	require.Len(t, convertJobs(), 2)
	s.RunJobs(t)
	res = get(t, s, "/rows", params, "")
	require.Equal(t, http.StatusOK, res.status, string(res.raw))
	require.Len(t, res.body["rows"], 4)
}

// unreadableData fails to read train.csv while fail is set.
type unreadableData struct {
	repo.Service
	fail atomic.Bool
}

func (u *unreadableData) OpenFile(ctx context.Context, id model.RepoID, commit model.CommitSHA, p string, rng *blob.Range) (io.ReadCloser, model.Entry, error) {
	if u.fail.Load() && p == "train.csv" {
		return nil, model.Entry{}, errors.New("blob: dial tcp 10.0.0.7:9000: connect: connection refused")
	}
	return u.Service.OpenFile(ctx, id, commit, p, rng)
}

// flakyRepos fails to list files while fail is set.
type flakyRepos struct {
	repo.Service
	fail atomic.Bool
}

func (f *flakyRepos) Tree(ctx context.Context, id model.RepoID, commit model.CommitSHA, p string, opts repo.TreeOpts) ([]model.Entry, error) {
	if f.fail.Load() {
		return nil, errors.New("gitstore: read 10.0.0.7: connection reset by peer")
	}
	return f.Service.Tree(ctx, id, commit, p, opts)
}

func TestPrivateDatasetsAreHidden(t *testing.T) {
	s := viewertest.New(t, viewertest.Options{})
	ctx := context.Background()
	owner := model.Principal{Kind: model.PrincipalUser, UserID: model.System().UserID, Username: "owner", NamespaceID: [16]byte{1}}
	id := model.RepoID{Type: model.RepoTypeDataset, Namespace: "owner", Name: "secret"}
	_, err := s.Repos.Create(ctx, repo.CreateParams{ID: id, Visibility: model.VisibilityPrivate, Actor: owner})
	require.NoError(t, err)
	res := get(t, s, "/splits", q("dataset", id.String()), "")
	require.Equal(t, http.StatusNotFound, res.status)
	require.Equal(t, "ExternalUnauthenticatedError", res.header.Get("X-Error-Code"))
	s.Authn.Add("tok", model.Principal{Kind: model.PrincipalUser, Username: "someone", Method: model.AuthToken})
	res = get(t, s, "/splits", q("dataset", id.String()), "tok")
	require.Equal(t, http.StatusNotFound, res.status)
	require.Equal(t, "ExternalAuthenticatedError", res.header.Get("X-Error-Code"))
	_, err = s.Viewer.Splits(ctx, model.Anonymous(), id)
	require.ErrorIs(t, err, datasets.ErrNotFound)
}

func pngBytes(t *testing.T, w, h int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	img.Set(0, 0, color.RGBA{255, 0, 0, 255})
	var b bytes.Buffer
	require.NoError(t, png.Encode(&b, img))
	return b.Bytes()
}

func TestImagesAndAssets(t *testing.T) {
	s := viewertest.New(t, viewertest.Options{})
	img := pngBytes(t, 7, 3)
	r, sha := s.Dataset(t, "pics", map[string][]byte{
		"train/a.png":        img,
		"train/metadata.csv": []byte("file_name,caption\na.png,red dot\n"),
	})
	s.Convert(t, r, sha)
	res := get(t, s, "/first-rows", q("dataset", r.ID.String(), "config", "default", "split", "train"), "")
	require.Equal(t, http.StatusOK, res.status, string(res.raw))
	require.Equal(t, false, res.body["truncated"])
	cell := res.body["rows"].([]any)[0].(map[string]any)["row"].(map[string]any)["image"].(map[string]any)
	require.EqualValues(t, 7, cell["width"])
	require.EqualValues(t, 3, cell["height"])
	src := cell["src"].(string)
	require.True(t, strings.HasPrefix(src, s.URL+"/assets/ns/pics/--/"+string(sha)+"/--/default/train/0/image/image.png?"), src)
	got, err := http.Get(src)
	require.NoError(t, err)
	body, _ := io.ReadAll(got.Body)
	_ = got.Body.Close()
	require.Equal(t, http.StatusOK, got.StatusCode)
	require.Equal(t, "image/png", got.Header.Get("Content-Type"))
	require.Equal(t, img, body)
	tampered := strings.Replace(src, "/0/image/", "/1/image/", 1)
	got, err = http.Get(tampered)
	require.NoError(t, err)
	_ = got.Body.Close()
	require.Equal(t, http.StatusForbidden, got.StatusCode)

	res = get(t, s, "/statistics", q("dataset", r.ID.String(), "config", "default", "split", "train"), "")
	require.Contains(t, string(res.raw), `"column_type":"image"`)
}

func TestScanLimitsMakeResultsPartial(t *testing.T) {
	s := viewertest.New(t, viewertest.Options{Viewer: viewer.Options{MaxScanRows: 3}})
	var b strings.Builder
	b.WriteString("n,s\n")
	for i := range 20 {
		fmt.Fprintf(&b, "%d,row %d\n", i, i)
	}
	r, sha := s.Dataset(t, "many", map[string][]byte{"train.csv": []byte(b.String())})
	s.Convert(t, r, sha)
	res := get(t, s, "/filter", q("dataset", r.ID.String(), "config", "default", "split", "train", "where", `"n" >= 0`), "")
	require.Equal(t, true, res.body["partial"])
	require.EqualValues(t, 3, res.body["num_rows_total"])
	res = get(t, s, "/search", q("dataset", r.ID.String(), "config", "default", "split", "train", "query", "row"), "")
	require.Equal(t, true, res.body["partial"])
}

// TestScanTimeLimitMakesResultsPartial checks MaxScanTime with a clock
// that moves a minute each time it is read, so the deadline has passed at
// every check. A scan reads scanTimeCheckRows (4,096) rows before its first
// check: it used to check before the first row too, and so returned no
// rows under heavy load, which made TestScanLimitsMakeResultsPartial flaky.
func TestScanTimeLimitMakesResultsPartial(t *testing.T) {
	var ticks atomic.Int64
	start := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	now := func() time.Time { return start.Add(time.Duration(ticks.Add(1)) * time.Minute) }
	s := viewertest.New(t, viewertest.Options{Now: now, Viewer: viewer.Options{MaxScanTime: 30 * time.Second}})
	var b strings.Builder
	b.WriteString("n,s\n")
	for i := range 5000 {
		fmt.Fprintf(&b, "%d,row %d\n", i, i)
	}
	r, sha := s.Dataset(t, "slow", map[string][]byte{"train.csv": []byte(b.String())})
	s.Convert(t, r, sha)
	res := get(t, s, "/filter", q("dataset", r.ID.String(), "config", "default", "split", "train", "where", `"n" >= 0`), "")
	require.Equal(t, true, res.body["partial"], string(res.raw))
	require.EqualValues(t, 4096, res.body["num_rows_total"], "the scan stops at its first check, after 4,096 rows")
	res = get(t, s, "/search", q("dataset", r.ID.String(), "config", "default", "split", "train", "query", "row"), "")
	require.Equal(t, true, res.body["partial"], string(res.raw))
	require.EqualValues(t, 4096, res.body["num_rows_total"])

	// With a clock that does not move, the same scan reads every row.
	fixed := viewertest.New(t, viewertest.Options{Now: func() time.Time { return start }, Viewer: viewer.Options{MaxScanTime: 30 * time.Second}})
	r, sha = fixed.Dataset(t, "fast", map[string][]byte{"train.csv": []byte(b.String())})
	fixed.Convert(t, r, sha)
	res = get(t, fixed, "/filter", q("dataset", r.ID.String(), "config", "default", "split", "train", "where", `"n" >= 0`), "")
	require.Equal(t, false, res.body["partial"], string(res.raw))
	require.EqualValues(t, 5000, res.body["num_rows_total"])
}

func TestLongCellsAreTruncated(t *testing.T) {
	s := viewertest.New(t, viewertest.Options{Viewer: viewer.Options{MaxCellBytes: 50}})
	long := strings.Repeat("é", 100)
	r, sha := s.Dataset(t, "long", map[string][]byte{"train.csv": []byte("t,x\n" + long + ",1\n")})
	s.Convert(t, r, sha)
	res := get(t, s, "/rows", q("dataset", r.ID.String(), "config", "default", "split", "train"), "")
	row := res.body["rows"].([]any)[0].(map[string]any)
	require.Equal(t, []any{"t"}, row["truncated_cells"])
	require.LessOrEqual(t, len(row["row"].(map[string]any)["t"].(string)), 50)
}

func zipBytes(t *testing.T, files map[string][]byte, order ...string) []byte {
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for _, name := range order {
		f, err := w.Create(name)
		require.NoError(t, err)
		_, err = f.Write(files[name])
		require.NoError(t, err)
	}
	require.NoError(t, w.Close())
	return buf.Bytes()
}

// TestZipArchives converts datasets of zip archives end to end: a CSV
// file in data/train.zip is a csv Config, and images with a metadata.csv
// at the top of train.zip are an imagefolder Config, as `datasets` reads
// them. Hangar used to call both a "zip" format and convert neither.
func TestZipArchives(t *testing.T) {
	s := viewertest.New(t, viewertest.Options{})
	csvZip := zipBytes(t, map[string][]byte{"a.csv": []byte(reviewsCSV)}, "a.csv")
	r, sha := s.Dataset(t, "zipped-csv", map[string][]byte{"data/train.zip": csvZip})
	res := get(t, s, "/first-rows", q("dataset", r.ID.String(), "config", "default", "split", "train"), "")
	require.Equal(t, http.StatusOK, res.status, "first rows from the archive while the conversion is pending: %s", res.raw)
	require.Len(t, res.body["rows"], 4)
	s.Convert(t, r, sha)
	res = get(t, s, "/info", q("dataset", r.ID.String(), "config", "default"), "")
	require.Equal(t, http.StatusOK, res.status, string(res.raw))
	require.Equal(t, "csv", res.body["dataset_info"].(map[string]any)["builder_name"])
	res = get(t, s, "/rows", q("dataset", r.ID.String(), "config", "default", "split", "train"), "")
	require.Equal(t, http.StatusOK, res.status, string(res.raw))
	require.EqualValues(t, 4, res.body["num_rows_total"])

	img := pngBytes(t, 4, 2)
	files := map[string][]byte{"a.png": img, "sub/b.png": pngBytes(t, 1, 1), "metadata.csv": []byte("file_name,caption\na.png,first\nsub/b.png,second\n")}
	r, sha = s.Dataset(t, "zipped-images", map[string][]byte{"train.zip": zipBytes(t, files, "metadata.csv", "a.png", "sub/b.png")})
	s.Convert(t, r, sha)
	res = get(t, s, "/info", q("dataset", r.ID.String(), "config", "default"), "")
	require.Equal(t, http.StatusOK, res.status, string(res.raw))
	require.Equal(t, "imagefolder", res.body["dataset_info"].(map[string]any)["builder_name"])
	res = get(t, s, "/first-rows", q("dataset", r.ID.String(), "config", "default", "split", "train"), "")
	require.Equal(t, http.StatusOK, res.status, string(res.raw))
	rows := res.body["rows"].([]any)
	require.Len(t, rows, 2)
	first := rows[0].(map[string]any)["row"].(map[string]any)
	require.Equal(t, "first", first["caption"])
	require.EqualValues(t, 4, first["image"].(map[string]any)["width"])
}
