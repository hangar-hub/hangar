package upstreamtest_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/blob"
	"github.com/jamesagarside/hangar/internal/db"
	"github.com/jamesagarside/hangar/internal/db/dbtest"
	"github.com/jamesagarside/hangar/internal/jobs"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/repo"
	"github.com/jamesagarside/hangar/internal/upstream"
	"github.com/jamesagarside/hangar/internal/upstream/upstreamtest"
)

func client() *upstream.HTTPClient {
	return upstream.NewHTTPClient(upstream.HTTPOptions{Version: "test", BaseBackoff: time.Millisecond, MaxAttempts: 2,
		Sleep: func(context.Context, time.Duration) error { return nil }})
}

func TestServerServesTheHubAPI(t *testing.T) {
	ctx := context.Background()
	f := upstreamtest.NewFake(t)
	w := bytes.Repeat([]byte("abc"), 400)
	commit := upstreamtest.Seed(t, f, jina, map[string][]byte{"config.json": []byte(`{}`), "dir/a.txt": []byte("a")}, map[string][]byte{"w.bin": w})
	ds := model.RepoID{Type: model.RepoTypeDataset, Namespace: "acme", Name: "data"}
	upstreamtest.Seed(t, f, ds, map[string][]byte{"d.csv": []byte("x\n1\n")}, nil)
	s := upstreamtest.NewServer(t, f)
	require.Contains(t, s.String(), s.URL)
	up := s.Upstream("test")
	c := client()

	info, err := c.RepoInfo(ctx, up, anon, jina, "")
	require.NoError(t, err)
	require.Equal(t, commit, info.SHA)
	require.Equal(t, 1, s.Requests("info"))
	_, err = c.RepoInfo(ctx, up, anon, ds, "main")
	require.NoError(t, err)
	page, err := c.Search(ctx, up, anon, upstream.SearchQuery{Type: model.RepoTypeDataset})
	require.NoError(t, err)
	require.Len(t, page.Repos, 1)
	entries, err := c.Tree(ctx, up, anon, jina, "main", "dir", false)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	_, err = c.Tree(ctx, up, anon, jina, "main", "nope", false)
	require.ErrorIs(t, err, upstream.ErrNotFound)

	// resolve: LFS files redirect to the CDN, regular files are served.
	rc, meta, err := c.Download(ctx, up, anon, jina, commit, "w.bin", nil)
	require.NoError(t, err)
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	require.Equal(t, w, got)
	require.True(t, meta.LFS)
	rc, _, err = c.Download(ctx, up, anon, ds, commit, "d.csv", nil)
	if err == nil {
		_ = rc.Close()
	}
	require.ErrorIs(t, err, upstream.ErrNotFound, "the dataset has a different Commit")
	_, _, err = c.Download(ctx, up, anon, jina, commit, "dir", nil)
	require.ErrorIs(t, err, upstream.ErrNotFound, "a folder is not a file")

	// The CDN refuses requests without the signature.
	resp, err := http.Get(s.CDN.URL + "/blob/x")
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	resp, err = http.Get(s.URL + "/acme")
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
	resp, err = http.Get(s.URL + "/acme/x/blob/main/a")
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
	resp, err = http.Get(s.URL + "/api/models/acme/x/unknown/main")
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusNotFound, resp.StatusCode)

	// Failure injection.
	s.Fail("/api/models/jinaai", http.StatusInternalServerError, 2, "")
	_, err = c.RepoInfo(ctx, up, anon, jina, "")
	require.ErrorIs(t, err, upstream.ErrUnavailable)
	oid := info.Siblings[len(info.Siblings)-1].LFS.OID
	s.CorruptNext(oid, 1)
	rc, _, err = c.Download(ctx, up, anon, jina, commit, "w.bin", nil)
	require.NoError(t, err)
	got, _ = io.ReadAll(rc)
	_ = rc.Close()
	require.NotEqual(t, w, got)
	s.BreakAfter(100)
	rc, _, err = c.Download(ctx, up, anon, jina, commit, "w.bin", nil)
	require.NoError(t, err)
	_, err = io.ReadAll(rc)
	_ = rc.Close()
	require.ErrorIs(t, err, upstream.ErrUnavailable)
	s.BreakAfter(0)
	s.Hold()
	done := make(chan []byte)
	go func() {
		rc, _, err := c.Download(ctx, up, anon, jina, commit, "w.bin", nil)
		if err != nil {
			done <- nil
			return
		}
		b, _ := io.ReadAll(rc)
		_ = rc.Close()
		done <- b
	}()
	time.Sleep(50 * time.Millisecond)
	s.Release()
	require.Equal(t, w, <-done)
	rc, _, err = c.Download(ctx, up, anon, jina, commit, "w.bin", &blobRange)
	require.NoError(t, err)
	_ = rc.Close()
	require.Contains(t, s.Ranges(), "bytes=10-")
	require.Zero(t, s.AuthOnCDN())

	// git, with and without credentials for private Repos.
	priv := model.RepoID{Type: model.RepoTypeModel, Namespace: "acme", Name: "private"}
	_, err = f.Repos.Create(ctx, repo.CreateParams{ID: priv, Visibility: model.VisibilityPrivate})
	require.NoError(t, err)
	f.RequireToken = "tok"
	_, err = c.FetchPack(ctx, up, anon, priv, nil)
	require.Error(t, err)
	res, err := c.FetchPack(ctx, up, upstream.Credentials{Token: "tok"}, priv, nil)
	require.NoError(t, err)
	require.Contains(t, res.Refs, "refs/heads/main")
	require.NotNil(t, res.Pack)
	require.NoError(t, res.Pack.Close())

	// The Viewer.
	f.SetViewerResponse("/is-valid", url.Values{"dataset": {"acme/data"}}, `{"viewer":true}`)
	raw, err := c.Viewer(ctx, up, anon, "/is-valid", url.Values{"dataset": {"acme/data"}})
	require.NoError(t, err)
	require.JSONEq(t, `{"viewer":true}`, string(raw))
	_, err = c.Viewer(ctx, up, anon, "/rows", nil)
	require.ErrorIs(t, err, upstream.ErrNotFound)
}

var blobRange = blobRangeFrom(10)

func TestHarnessRunsAPull(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		h := upstreamtest.NewHarness(t, d, upstreamtest.HarnessOptions{PartSize: 1 << 10})
		alice := h.User("alice")
		h.SetToken(alice.UserID, "tok")
		big := bytes.Repeat([]byte("x"), 3000)
		commit := h.Seed(jina, nil, map[string][]byte{"w.bin": big})
		require.NoError(t, h.AddUpstream("second", "s-"))
		h.SetSettings(upstream.Settings{Windows: []string{"13:00-14:00"}})
		var hit bool
		h.Limiter.After(1, func() { hit = true })
		id, _, err := h.Service.StartPull(h.Ctx, upstream.PullRequest{Repo: jina, Actor: alice})
		require.NoError(t, err)
		h.Run(20)
		require.Equal(t, jobs.StatusSucceeded, h.Job(id).Status, h.Job(id).Error)
		require.True(t, hit)
		require.True(t, h.Now().After(time.Date(2026, 10, 13, 13, 0, 0, 0, time.UTC)), "the transfer waited for the window")
		e, err := h.Repos.Entry(h.Ctx, jina, commit, "w.bin")
		require.NoError(t, err)
		require.True(t, e.Present)
		_, err = upstreamtest.ParseHourWindows([]string{"a", "b"})
		require.Error(t, err)
		_, err = upstreamtest.ParseHourWindows([]string{"bad"})
		require.Error(t, err)
		w, err := upstreamtest.ParseHourWindows([]string{"01:00-02:00"})
		require.NoError(t, err)
		require.Equal(t, time.Date(2026, 10, 14, 1, 0, 0, 0, time.UTC), w.NextOpen(time.Date(2026, 10, 13, 5, 0, 0, 0, time.UTC)))
		require.Contains(t, (&upstreamtest.Snoozed{Reason: "x"}).Error(), "x")
	})
}

func blobRangeFrom(start int64) blob.Range { return blob.Range{Start: start, End: -1} }
