package hfapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/hangar-hub/hangar/internal/collections"
	"github.com/hangar-hub/hangar/internal/community"
	"github.com/hangar-hub/hangar/internal/datasets"
	"github.com/hangar-hub/hangar/internal/httpx"
	"github.com/hangar-hub/hangar/internal/model"
	"github.com/hangar-hub/hangar/internal/repo"
	"github.com/hangar-hub/hangar/internal/scan"
	"github.com/hangar-hub/hangar/internal/telemetry"
)

func testHandler() *Handler {
	return &Handler{d: Deps{Settings: Settings{CommitLineMax: 1 << 20}}, log: slog.New(slog.DiscardHandler), now: time.Now}
}

func hubRequest(method string) *http.Request {
	r := httptest.NewRequest(method, "/api/models/a/b", nil)
	ctx := httpx.WithInfo(r.Context(), "req-1")
	httpx.SetProtocol(ctx, telemetry.ProtocolHub)
	return r.WithContext(ctx)
}

func TestServiceErrorMapping(t *testing.T) {
	h := testHandler()
	id := model.RepoID{Type: model.RepoTypeModel, Namespace: "a", Name: "b"}
	cases := []struct {
		err    error
		status int
		code   string
	}{
		{repo.ErrNotFound, http.StatusUnauthorized, httpx.CodeRepoNotFound},
		{fmt.Errorf("x: %w", repo.ErrRevisionNotFound), http.StatusNotFound, httpx.CodeRevisionNotFound},
		{repo.ErrEntryNotFound, http.StatusNotFound, httpx.CodeEntryNotFound},
		{repo.ErrExists, http.StatusConflict, ""},
		{repo.ErrRefExists, http.StatusConflict, ""},
		{repo.ErrParentMismatch, http.StatusPreconditionFailed, ""},
		{repo.ErrRefConflict, http.StatusConflict, ""},
		{repo.ErrReadOnly, http.StatusForbidden, ""},
		{repo.ErrProtectedRef, http.StatusForbidden, ""},
		{&repo.PolicyError{Ref: "refs/heads/main", Message: "no"}, http.StatusForbidden, ""},
		{repo.ErrLFSObjectMissing, http.StatusBadRequest, ""},
		{repo.ErrInvalid, http.StatusBadRequest, ""},
		{model.ErrInvalidPath, http.StatusBadRequest, ""},
		{repo.ErrChecksumMismatch, http.StatusBadRequest, ""},
		{context.Canceled, 499, ""},
		{errors.New("boom"), http.StatusInternalServerError, ""},
	}
	for _, c := range cases {
		w := httptest.NewRecorder()
		h.serviceError(w, hubRequest(http.MethodGet), id, c.err)
		require.Equal(t, c.status, w.Code, c.err.Error())
		require.Equal(t, c.code, w.Header().Get(httpx.HeaderErrorCode), c.err.Error())
		require.NotEmpty(t, w.Header().Get(httpx.HeaderErrorMessage))
	}
	require.Equal(t, "Request failed.", errMessage(errors.New("")))
	require.Equal(t, "Done.", errMessage(errors.New("done.")))
}

func TestOtherErrorMappings(t *testing.T) {
	h := testHandler()
	rec := func(f func(w http.ResponseWriter, r *http.Request)) int {
		w := httptest.NewRecorder()
		f(w, hubRequest(http.MethodGet))
		return w.Code
	}
	for err, want := range map[error]int{datasets.ErrNotFound: 404, datasets.ErrNotReady: 404, errors.New("x"): 500} {
		require.Equal(t, want, rec(func(w http.ResponseWriter, r *http.Request) { h.viewerError(w, r, err) }))
	}
	for err, want := range map[error]int{collections.ErrNotFound: 404, collections.ErrExists: 409, collections.ErrInvalid: 400, collections.ErrForbidden: 401, errors.New("x"): 500} {
		require.Equal(t, want, rec(func(w http.ResponseWriter, r *http.Request) { h.collectionError(w, r, err) }))
	}
	rp := &repo.Repo{ID: model.RepoID{Type: model.RepoTypeModel, Namespace: "a", Name: "b"}}
	for err, want := range map[error]int{community.ErrNotFound: 404, community.ErrNotOpen: 409, community.ErrNotPullRequest: 400, community.ErrInvalid: 400, errors.New("x"): 500} {
		require.Equal(t, want, rec(func(w http.ResponseWriter, r *http.Request) { h.communityError(w, r, rp, err) }))
	}
	require.Equal(t, 500, rec(func(w http.ResponseWriter, r *http.Request) { h.denied(w, r, rp.ID, errors.New("x")) }))
}

func TestSpool(t *testing.T) {
	c := &commitRequest{}
	inMemory := int64(spoolMemory - 2)
	r, err := c.spool([]byte("ab"), &inMemory)
	require.NoError(t, err)
	b, _ := io.ReadAll(r)
	require.Equal(t, "ab", string(b))
	r, err = c.spool([]byte("spilled"), &inMemory)
	require.NoError(t, err)
	require.Len(t, c.temp, 1)
	b, _ = io.ReadAll(r)
	require.Equal(t, "spilled", string(b))
	name := c.temp[0].Name()
	c.close()
	_, err = os.Stat(name)
	require.True(t, os.IsNotExist(err), "spooled content is removed")
}

func TestReadCommitErrors(t *testing.T) {
	h := testHandler()
	_, err := h.readCommit(errReader{})
	require.Error(t, err)
	var ce *commitError
	require.False(t, errors.As(err, &ce))
	c, err := h.readCommit(strings.NewReader(`{"key":"header","value":{"summary":"s"}}`))
	require.NoError(t, err)
	require.Empty(t, c.ops)
	require.Equal(t, "boom", (&commitError{msg: "boom"}).Error())
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("read failed") }

func TestRecordDownload(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	in, err := telemetry.NewInstruments(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("t"))
	require.NoError(t, err)
	h := testHandler()
	h.d.Instruments = in
	h.recordDownload(context.Background(), model.RepoTypeModel, "proxy", 10, true)
	h.recordDownload(context.Background(), model.RepoTypeModel, "redirect", 5, false)
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	sums := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if s, ok := m.Data.(metricdata.Sum[int64]); ok {
				for _, dp := range s.DataPoints {
					sums[m.Name] += dp.Value
				}
			}
		}
	}
	require.EqualValues(t, 15, sums["hangar.download.bytes"])
	require.EqualValues(t, 1, sums["hangar.download.files"])
	testHandler().recordDownload(context.Background(), model.RepoTypeModel, "proxy", 1, true)
}

func TestSmallHelpers(t *testing.T) {
	require.Equal(t, "alice", authorName(repo.Signature{Name: "Alice A", Email: "alice@users.noreply.hangar"}))
	require.Equal(t, "Alice A", authorName(repo.Signature{Name: "Alice A", Email: "a@example.com"}))
	require.Equal(t, "a@example.com", authorName(repo.Signature{Email: "a@example.com"}))
	for s, want := range map[scan.Status]string{scan.StatusUnsafe: "danger", scan.StatusSuspicious: "caution", scan.StatusError: "error", scan.StatusSafe: ""} {
		require.Equal(t, want, issueLevel(s))
	}
	for s, want := range map[scan.Status]string{scan.StatusUnsafe: "unsafe", scan.StatusSuspicious: "caution", scan.StatusError: "error", scan.StatusSafe: "safe"} {
		require.Equal(t, want, hubScanStatus(s))
	}
	for s, want := range map[scan.Status]string{scan.StatusUnsafe: "dangerous", scan.StatusSuspicious: "suspicious", scan.StatusSafe: "innocuous"} {
		require.Equal(t, want, importSafety(s))
	}
	require.True(t, anyBool(true))
	require.True(t, anyBool("True"))
	require.True(t, anyBool(float64(1)))
	require.False(t, anyBool(nil))
	st := securityFileStatus(scan.StatusSafe, []scan.Finding{{Detail: "integrity only"}, {Module: "torch._utils", Name: "_rebuild_tensor_v2", Status: scan.StatusSafe}})
	require.Len(t, st["pickleImportScan"].(map[string]any)["pickleImports"], 1)
	p := reqPath{segs: []string{"a"}, esc: []string{"a"}}
	require.Equal(t, 0, p.shift(5).len())
	require.Equal(t, "", p.at(3))
	require.Equal(t, "", p.joined(4))
	rev, rest := takeRevision(reqPath{})
	require.Equal(t, "", rev)
	require.Equal(t, 0, rest.len())
	require.Contains(t, contentDisposition("Ü.txt"), "filename*=UTF-8''%C3%9C.txt")
	require.Contains(t, contentDisposition(`a"b.txt`), `filename="a_b.txt"`)
	require.True(t, strings.HasPrefix(contentDisposition("weights.gguf"), "attachment;"))
	g := gatedValue(repo.Gating{Mode: "auto"})
	require.Equal(t, "auto", g)
}

func TestInternalErrorHidesDetails(t *testing.T) {
	h := testHandler()
	w := httptest.NewRecorder()
	r := hubRequest(http.MethodGet)
	httpx.SetRoute(r.Context(), "/api/models/{ns}/{name}")
	h.internalError(w, r, errors.New("database password is hunter2"))
	require.Equal(t, http.StatusInternalServerError, w.Code)
	require.NotContains(t, w.Body.String(), "hunter2")
	require.Contains(t, w.Body.String(), "req-1")
	require.Equal(t, "/api/models/{ns}/{name}", routeOf(r))
	require.Equal(t, "", routeOf(httptest.NewRequest(http.MethodGet, "/", nil)))
}

// TestCommitWalkNext checks the cursor of the Commit listing: after a page
// that skipped nothing, the next page starts at what git's walk had queued
// but not shown (start Commits and parents of shown Commits, including the
// second parents of merges); otherwise, or when that queue is longer than
// a cursor holds, it keeps the start and skips what was shown.
func TestCommitWalkNext(t *testing.T) {
	sha := func(c byte) model.CommitSHA { return model.CommitSHA(strings.Repeat(string(c), 40)) }
	commit := func(c byte, parents ...byte) repo.CommitInfo {
		ci := repo.CommitInfo{SHA: sha(c)}
		for _, p := range parents {
			ci.Parents = append(ci.Parents, sha(p))
		}
		return ci
	}
	// f merges a and b; the parent of a and of b is c.
	w := commitWalk{from: []model.CommitSHA{sha('f')}}
	next := w.next([]repo.CommitInfo{commit('f', 'a', 'b'), commit('a', 'c')})
	require.Equal(t, commitWalk{from: []model.CommitSHA{sha('b'), sha('c')}}, next)

	// A start Commit the page did not reach stays in the cursor.
	w = commitWalk{from: []model.CommitSHA{sha('b'), sha('c')}}
	require.Equal(t, commitWalk{from: []model.CommitSHA{sha('c'), sha('d')}}, w.next([]repo.CommitInfo{commit('b', 'd')}))

	// After a skipping page, the walk keeps its start.
	w = commitWalk{from: []model.CommitSHA{sha('f')}, skip: 3}
	require.Equal(t, commitWalk{from: []model.CommitSHA{sha('f')}, skip: 5}, w.next([]repo.CommitInfo{commit('a', 'c'), commit('c', 'd')}))

	// A queue longer than a cursor holds falls back to skipping.
	octopus := repo.CommitInfo{SHA: sha('f')}
	for i := range maxCursorCommits + 1 {
		octopus.Parents = append(octopus.Parents, model.CommitSHA(fmt.Sprintf("%040x", i+1)))
	}
	w = commitWalk{from: []model.CommitSHA{sha('f')}}
	require.Equal(t, commitWalk{from: []model.CommitSHA{sha('f')}, skip: 1}, w.next([]repo.CommitInfo{octopus}))

	q := url.Values{"limit": {"3"}}
	commitWalk{from: []model.CommitSHA{sha('b'), sha('c')}}.set(q)
	require.Equal(t, sha('b')+","+sha('c'), model.CommitSHA(q.Get("cursor")))
	require.Empty(t, q.Get("skip"))
	got, ok := parseCommitCursor(q, sha('f'), 3)
	require.True(t, ok)
	require.Equal(t, commitWalk{from: []model.CommitSHA{sha('b'), sha('c')}}, got)
	got, ok = parseCommitCursor(url.Values{"p": {"2"}}, sha('f'), 3)
	require.True(t, ok)
	require.Equal(t, commitWalk{from: []model.CommitSHA{sha('f')}, skip: 6}, got)
}
