// Package viewertest runs the dataset pipeline and the Dataset Viewer over
// fakes for tests: a repotest.Fake Repo service, an in-memory Blob store,
// a test database, the fake Job queue and Authz, and an HTTP server that
// routes /datasets-server/... the way cmd/hangar does.
package viewertest

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hangar-hub/hangar/internal/auth/authtest"
	"github.com/hangar-hub/hangar/internal/blob/blobtest"
	"github.com/hangar-hub/hangar/internal/datasets/datasetstest"
	"github.com/hangar-hub/hangar/internal/datasets/pipeline"
	"github.com/hangar-hub/hangar/internal/datasets/viewer"
	"github.com/hangar-hub/hangar/internal/db"
	"github.com/hangar-hub/hangar/internal/db/dbtest"
	"github.com/hangar-hub/hangar/internal/httpx"
	"github.com/hangar-hub/hangar/internal/jobs"
	"github.com/hangar-hub/hangar/internal/jobs/jobstest"
	"github.com/hangar-hub/hangar/internal/model"
	"github.com/hangar-hub/hangar/internal/repo"
	"github.com/hangar-hub/hangar/internal/repo/repotest"
)

// Options configure a Stack.
type Options struct {
	// DB is the database; nil uses a fresh SQLite database.
	DB *db.DB
	// MaxConvertBytes is viewer.max_convert_bytes; zero means no limit.
	MaxConvertBytes int64
	// Viewer options; PublicURL is set to the test server's URL.
	Viewer viewer.Options
	// Now is the Viewer's clock; nil uses time.Now.
	Now func() time.Time
	// ViewerRepos, when set, wraps the Repo service the Viewer uses (the
	// pipeline keeps the fake). Load tests use it to give the Viewer a Repo
	// service whose reads cost what the real one's do.
	ViewerRepos func(*repotest.Fake) repo.Service
	// PipelineRepos, when set, wraps the Repo service the pipeline uses.
	// Tests use it to make the Jobs' reads fail.
	PipelineRepos func(*repotest.Fake) repo.Service
}

// Stack is a running pipeline and Viewer.
type Stack struct {
	Repos    *repotest.Fake
	DB       *db.DB
	Blobs    *blobtest.Fake
	Jobs     *jobstest.Fake
	Pipeline *pipeline.Pipeline
	Viewer   *viewer.Service
	Authz    *authtest.Authz
	Authn    *authtest.Authenticator
	Server   *httptest.Server
	// URL is the Viewer's root, ending in /datasets-server.
	URL string
}

// TestKey is the Instance key the Stack signs asset URLs with.
var TestKey = []byte("0123456789abcdef0123456789abcdef")

// New starts a Stack, stopped when the test ends.
func New(t testing.TB, o Options) *Stack {
	t.Helper()
	d := o.DB
	if d == nil {
		d = dbtest.SQLite(t)
	}
	reg := jobs.NewRegistry()
	q := jobstest.NewFake(reg)
	repos := repotest.NewFake(t)
	blobs := blobtest.NewFake()
	var pipelineRepos repo.Service = repos
	if o.PipelineRepos != nil {
		pipelineRepos = o.PipelineRepos(repos)
	}
	p, err := pipeline.New(pipeline.Deps{DB: d, Blobs: blobs, Repos: pipelineRepos, LFS: repos.LFS, Jobs: q, MaxConvertBytes: o.MaxConvertBytes, TempDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Register(reg); err != nil {
		t.Fatal(err)
	}
	signer, err := httpx.NewSigner(TestKey)
	if err != nil {
		t.Fatal(err)
	}
	s := &Stack{Repos: repos, DB: d, Blobs: blobs, Jobs: q, Pipeline: p, Authz: authtest.NewAuthz(), Authn: authtest.NewAuthenticator()}
	s.Authz.AnonymousRead = true
	var handler http.Handler
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handler.ServeHTTP(w, r) }))
	t.Cleanup(s.Server.Close)
	vo := o.Viewer
	vo.PublicURL = s.Server.URL
	var viewerRepos repo.Service = repos
	if o.ViewerRepos != nil {
		viewerRepos = o.ViewerRepos(repos)
	}
	v, err := viewer.New(viewer.Deps{Repos: viewerRepos, Authz: s.Authz, Store: p.Store(), Jobs: q, Blobs: blobs, Signer: signer, Now: o.Now}, vo)
	if err != nil {
		t.Fatal(err)
	}
	s.Viewer = v
	h := httpx.Dispatch(httpx.Protocols{Viewer: v.Handler()}, s.Server.URL)
	h = httpx.Authenticate(s.Authn, nil)(h)
	handler = httpx.RequestIDs(h)
	s.URL = s.Server.URL + httpx.ViewerPrefix
	return s
}

// Dataset creates a public dataset Repo with files and records it in the
// database. It returns the Repo and the new Commit.
func (s *Stack) Dataset(t testing.TB, name string, files map[string][]byte) (*repo.Repo, model.CommitSHA) {
	t.Helper()
	id := model.RepoID{Type: model.RepoTypeDataset, Namespace: "ns", Name: name}
	sha := datasetstest.CreateDataset(t, s.Repos, id, files)
	r, err := s.Repos.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	datasetstest.InsertRepoRow(t, s.DB, r)
	return r, sha
}

// RunJobs runs queued Jobs until none are left.
func (s *Stack) RunJobs(t testing.TB) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	for s.Jobs.RunPending(ctx) > 0 {
		if ctx.Err() != nil {
			t.Fatal(ctx.Err())
		}
	}
}

// Convert converts a dataset Commit and computes its statistics.
func (s *Stack) Convert(t testing.TB, r *repo.Repo, commit model.CommitSHA) {
	t.Helper()
	if _, err := s.Pipeline.EnqueueConvert(context.Background(), r.UUID, commit); err != nil {
		t.Fatal(err)
	}
	s.RunJobs(t)
}
