//go:build compat

package gitcompat

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/compat/harness"
	"github.com/jamesagarside/hangar/internal/auth/authtest"
	"github.com/jamesagarside/hangar/internal/blob"
	"github.com/jamesagarside/hangar/internal/blob/blobtest"
	"github.com/jamesagarside/hangar/internal/gitserver"
	"github.com/jamesagarside/hangar/internal/gitserver/gitservertest"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/repo"
)

const (
	tokOwner  = "hgr_owner"
	tokWriter = "hgr_writer"
	tokReader = "hgr_reader"
	tokOther  = "hgr_other"
)

var (
	modelID   = model.RepoID{Type: model.RepoTypeModel, Namespace: "owner", Name: "tiny-model"}
	privateID = model.RepoID{Type: model.RepoTypeModel, Namespace: "owner", Name: "private-model"}
	datasetID = model.RepoID{Type: model.RepoTypeDataset, Namespace: "owner", Name: "tiny-data"}
	mirrorID  = model.RepoID{Type: model.RepoTypeModel, Namespace: "owner", Name: "mirror-model"}
)

func token(p model.Principal, scope model.Scope) model.Principal {
	p.Method = model.AuthToken
	p.Token = &model.TokenInfo{ID: uuid.New(), Name: "compat", Scope: scope}
	return p
}

// suite is one backend with two replicas in front of it.
type suite struct {
	t        *testing.T
	repos    repo.Service
	owner    model.Principal
	lfs      repo.LFSStore
	blobs    *blobtest.Fake
	replicas [2]*gitservertest.Instance
	git      *gitservertest.Git
}

type suiteOptions struct {
	// s3 makes the Blob store presign URLs, served by an S3 simulator.
	s3 bool
}

func newSuite(t *testing.T, factory BackendFactory, o suiteOptions) *suite {
	t.Helper()
	ctx := context.Background()
	blobs := blobtest.NewFake()
	if o.s3 {
		sim := httptest.NewServer(s3Simulator(t, blobs))
		t.Cleanup(sim.Close)
		blobs.PresignBaseURL = sim.URL + "/bucket"
		blobs.MaxPresignPut = 5 << 30
	}
	b := factory(t, blobs, []string{"owner", "writer", "other"})
	owner, writer, other := b.Users["owner"], b.Users["writer"], b.Users["other"]
	s := &suite{t: t, repos: b.Replica(t), owner: owner, lfs: b.LFS, blobs: blobs, git: gitservertest.NewGit(t)}

	authn := authtest.NewAuthenticator()
	authn.Add(tokOwner, token(owner, model.ScopeWrite))
	authn.Add(tokWriter, token(writer, model.ScopeWrite))
	authn.Add(tokReader, token(owner, model.ScopeRead))
	authn.Add(tokOther, token(other, model.ScopeWrite))
	authz := authtest.NewAuthz()
	authz.AnonymousRead = true
	for _, p := range []repo.CreateParams{
		{ID: modelID, Visibility: model.VisibilityPublic, Actor: owner, InitialReadme: true},
		{ID: privateID, Visibility: model.VisibilityPrivate, Actor: owner, InitialReadme: true},
		{ID: datasetID, Visibility: model.VisibilityPublic, Actor: owner, InitialReadme: true},
		{ID: mirrorID, Visibility: model.VisibilityPublic, Actor: owner, Mirror: true},
	} {
		r, err := s.repos.Create(ctx, p)
		require.NoError(t, err)
		authz.SetRole(writer.UserID, r.NamespaceID, model.RoleWrite)
	}
	cfg := gitserver.Config{MaxBlobSize: 1 << 20, MaxPackSize: 256 << 20, MultipartThreshold: 1 << 20, PartSize: 512 << 10}
	for i := range s.replicas {
		repos := s.repos
		if i > 0 {
			repos = b.Replica(t)
		}
		s.replicas[i] = gitservertest.Start(t, gitservertest.Options{Repos: repos, LFS: b.LFS, Authn: authn, Authz: authz, Config: cfg})
	}
	s.git.Must(t, "", "lfs", "install", "--skip-repo")
	return s
}

func (s *suite) remote(id model.RepoID, tok string) string {
	return s.replicas[0].RemoteURL(id, tok, true)
}

func (s *suite) clone(id model.RepoID, tok string, args ...string) string {
	s.t.Helper()
	return s.cloneEnv(id, tok, nil, args...)
}

func (s *suite) cloneEnv(id model.RepoID, tok string, env []string, args ...string) string {
	s.t.Helper()
	dir := filepath.Join(s.t.TempDir(), "clone")
	all := append([]string{"clone", "-q"}, args...)
	all = append(all, s.remote(id, tok), dir)
	out, err := s.git.RunEnv(context.Background(), "", env, all...)
	require.NoError(s.t, err, out)
	return dir
}

func (s *suite) must(dir string, args ...string) string {
	s.t.Helper()
	return s.git.Must(s.t, dir, args...)
}

func (s *suite) run(dir string, args ...string) (string, error) {
	return s.git.Run(context.Background(), dir, args...)
}

func (s *suite) write(dir, name string, content []byte) {
	s.t.Helper()
	p := filepath.Join(dir, name)
	require.NoError(s.t, os.MkdirAll(filepath.Dir(p), 0o755))
	require.NoError(s.t, os.WriteFile(p, content, 0o644))
}

func (s *suite) commit(dir, name, content, msg string) string {
	s.t.Helper()
	s.write(dir, name, []byte(content))
	s.must(dir, "add", "-A")
	s.must(dir, "commit", "-q", "-m", msg)
	return s.must(dir, "rev-parse", "HEAD")
}

func (s *suite) resolve(id model.RepoID, rev string) string {
	s.t.Helper()
	sha, err := s.repos.Resolve(context.Background(), id, rev)
	require.NoError(s.t, err)
	return string(sha)
}

// s3Simulator serves the presigned URLs of a blobtest.Fake as S3 would,
// and, like S3, refuses requests that carry an Authorization header as well
// as a presigned query.
func s3Simulator(t *testing.T, blobs *blobtest.Fake) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			http.Error(w, "Only one auth mechanism allowed", http.StatusBadRequest)
			return
		}
		key, ok := strings.CutPrefix(r.URL.Path, "/bucket/")
		if !ok || blob.ValidateKey(key) != nil {
			http.NotFound(w, r)
			return
		}
		ctx := r.Context()
		q := r.URL.Query()
		switch r.Method {
		case http.MethodGet:
			rc, info, err := blobs.Get(ctx, key, nil)
			if err != nil {
				http.NotFound(w, r)
				return
			}
			defer rc.Close()
			w.Header().Set("Content-Length", strconv.FormatInt(info.Size, 10))
			_, _ = io.Copy(w, rc)
		case http.MethodPut:
			if q.Has("partNumber") {
				n, _ := strconv.Atoi(q.Get("partNumber"))
				etag, err := blobs.PutPart(ctx, key, q.Get("uploadId"), n, r.Body, r.ContentLength)
				if err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				w.Header().Set("ETag", etag)
				return
			}
			if err := blobs.Put(ctx, key, r.Body, r.ContentLength); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
			}
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
}

var (
	hfOnce sync.Once
	hfPath string
	hfErr  error
)

// hfBinary returns the hf CLI from the compat Python project, which
// provides the multipart custom transfer agent.
func hfBinary(t *testing.T) string {
	t.Helper()
	hfOnce.Do(func() {
		cmd := exec.Command("uv", "run", "--project", filepath.Join(harness.RepoRoot(), "compat"), "--frozen",
			"python", "-c", "import shutil, sys; print(shutil.which('hf') or sys.exit('hf not found'))")
		out, err := cmd.Output()
		hfPath, hfErr = strings.TrimSpace(string(out)), err
	})
	if hfErr != nil {
		t.Fatalf("the hf CLI from compat/pyproject.toml is needed for the multipart transfer: %v", hfErr)
	}
	return hfPath
}
