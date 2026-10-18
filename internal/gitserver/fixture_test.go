package gitserver_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/auth/authtest"
	"github.com/hangar-hub/hangar/internal/blob/blobtest"
	"github.com/hangar-hub/hangar/internal/gitserver"
	"github.com/hangar-hub/hangar/internal/gitserver/gitservertest"
	"github.com/hangar-hub/hangar/internal/model"
	"github.com/hangar-hub/hangar/internal/repo"
	"github.com/hangar-hub/hangar/internal/repo/repotest"
)

// Tokens used by the fixture.
const (
	tokAlice     = "alice-write"
	tokAliceRead = "alice-read"
	tokBob       = "bob-write"
	tokCarol     = "carol-write"
)

var (
	alice = model.Principal{Kind: model.PrincipalUser, UserID: uuid.New(), NamespaceID: uuid.New(), Username: "alice", DisplayName: "Alice", Email: "alice@example.com", Method: model.AuthToken}
	bob   = model.Principal{Kind: model.PrincipalUser, UserID: uuid.New(), NamespaceID: uuid.New(), Username: "bob", Method: model.AuthToken}
	carol = model.Principal{Kind: model.PrincipalUser, UserID: uuid.New(), NamespaceID: uuid.New(), Username: "carol", Method: model.AuthToken}

	publicID  = model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "tiny"}
	privateID = model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "secret"}
	datasetID = model.RepoID{Type: model.RepoTypeDataset, Namespace: "alice", Name: "data"}
	mirrorID  = model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "mirror"}
)

func withToken(p model.Principal, scope model.Scope) model.Principal {
	p.Token = &model.TokenInfo{ID: uuid.New(), Name: "test", Scope: scope}
	return p
}

// fixture is a git server over repotest fakes with three users: alice owns
// the alice Namespace, carol has write access to it, and bob has none.
type fixture struct {
	repos *repotest.Fake
	blobs *blobtest.Fake
	authn *authtest.Authenticator
	authz *authtest.Authz
	inst  *gitservertest.Instance
	git   *gitservertest.Git
}

type fixtureOption func(*gitservertest.Options, *fixture)

func withConfig(fn func(*gitserver.Config)) fixtureOption {
	return func(o *gitservertest.Options, _ *fixture) { fn(&o.Config) }
}

func newFixture(t *testing.T, opts ...fixtureOption) *fixture {
	t.Helper()
	ctx := context.Background()
	f := &fixture{repos: repotest.NewFake(t), authn: authtest.NewAuthenticator(), authz: authtest.NewAuthz(), git: gitservertest.NewGit(t)}
	f.blobs = f.repos.LFS.Store.(*blobtest.Fake)
	f.authn.Add(tokAlice, withToken(alice, model.ScopeWrite))
	f.authn.Add(tokAliceRead, withToken(alice, model.ScopeRead))
	f.authn.Add(tokBob, withToken(bob, model.ScopeWrite))
	f.authn.Add(tokCarol, withToken(carol, model.ScopeWrite))
	f.authz.AnonymousRead = true
	f.authz.SetRole(carol.UserID, alice.NamespaceID, model.RoleWrite)
	for _, p := range []repo.CreateParams{
		{ID: publicID, Visibility: model.VisibilityPublic, Actor: alice, InitialReadme: true},
		{ID: privateID, Visibility: model.VisibilityPrivate, Actor: alice, InitialReadme: true},
		{ID: datasetID, Visibility: model.VisibilityPublic, Actor: alice, InitialReadme: true},
		{ID: mirrorID, Visibility: model.VisibilityPublic, Actor: alice, InitialReadme: true},
	} {
		_, err := f.repos.Create(ctx, p)
		require.NoError(t, err)
	}
	require.NoError(t, f.repos.SetMirror(mirrorID, true))
	o := gitservertest.Options{Repos: f.repos, LFS: f.repos.LFS, Authn: f.authn, Authz: f.authz,
		Config: gitserver.Config{MaxBlobSize: 1 << 20, MaxPackSize: 64 << 20, VerifyWait: 0, MultipartThreshold: 1 << 20, PartSize: 256 << 10}}
	for _, opt := range opts {
		opt(&o, f)
	}
	f.inst = gitservertest.Start(t, o)
	return f
}

func (f *fixture) remote(id model.RepoID, token string) string {
	return f.inst.RemoteURL(id, token, true)
}

// clone clones a Repo into a new directory and returns it.
func (f *fixture) clone(t *testing.T, id model.RepoID, token string, extra ...string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "clone")
	args := append([]string{"clone", "-q"}, extra...)
	args = append(args, f.remote(id, token), dir)
	f.git.Must(t, "", args...)
	return dir
}

func writeFile(t *testing.T, dir, name string, content []byte) {
	t.Helper()
	path := filepath.Join(dir, name)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, content, 0o644))
}

func (f *fixture) commit(t *testing.T, dir, name, content, msg string) string {
	t.Helper()
	writeFile(t, dir, name, []byte(content))
	f.git.Must(t, dir, "add", "-A")
	f.git.Must(t, dir, "commit", "-q", "-m", msg)
	return f.git.Must(t, dir, "rev-parse", "HEAD")
}

func (f *fixture) resolve(t *testing.T, id model.RepoID, rev string) string {
	t.Helper()
	sha, err := f.repos.Resolve(context.Background(), id, rev)
	require.NoError(t, err)
	return string(sha)
}

type gitserverConfig = gitserver.Config
