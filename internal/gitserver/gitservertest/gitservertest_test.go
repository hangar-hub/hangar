package gitservertest_test

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/auth/authtest"
	"github.com/jamesagarside/hangar/internal/gitserver/gitservertest"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/repo"
	"github.com/jamesagarside/hangar/internal/repo/repotest"
)

func TestStartServesCloneAndPush(t *testing.T) {
	ctx := context.Background()
	owner := model.Principal{Kind: model.PrincipalUser, UserID: uuid.New(), NamespaceID: uuid.New(), Username: "owner"}
	repos := repotest.NewFake(t)
	id := model.RepoID{Type: model.RepoTypeModel, Namespace: "owner", Name: "m"}
	_, err := repos.Create(ctx, repo.CreateParams{ID: id, Visibility: model.VisibilityPublic, Actor: owner})
	require.NoError(t, err)
	authn := authtest.NewAuthenticator()
	authn.Add("secret", owner)
	authz := authtest.NewAuthz()
	authz.AnonymousRead = true
	inst := gitservertest.Start(t, gitservertest.Options{Repos: repos, LFS: repos.LFS, Authn: authn, Authz: authz})

	g := gitservertest.NewGit(t)
	dir := filepath.Join(t.TempDir(), "c")
	g.Must(t, "", "clone", "-q", inst.RemoteURL(id, "secret", false), dir)
	g.Must(t, dir, "commit", "-q", "--allow-empty", "-m", "empty")
	g.Must(t, dir, "push", "-q", "origin", "main")
	sha, err := repos.Resolve(ctx, id, "main")
	require.NoError(t, err)
	require.Equal(t, string(sha), g.Must(t, dir, "rev-parse", "HEAD"))

	_, err = g.RunEnv(ctx, dir, []string{"GIT_TRACE=0"}, "push", inst.RemoteURL(id, "wrong", true), "main")
	require.Error(t, err)
	require.Equal(t, "http://user:secret@"+inst.HTTP.Listener.Addr().String()+"/owner/m.git", inst.RemoteURL(id, "secret", true))

	resp, err := http.Get(inst.URL + "/owner/m.git/info/refs?service=git-receive-pack")
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}
