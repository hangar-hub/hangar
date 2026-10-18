package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/api/gen"
	"github.com/hangar-hub/hangar/internal/db"
	"github.com/hangar-hub/hangar/internal/jobs"
	"github.com/hangar-hub/hangar/internal/model"
	"github.com/hangar-hub/hangar/internal/repo"
)

// jobRepos is an api.JobRepos over a map, standing in for repo.Service.
type jobRepos map[uuid.UUID]*repo.Repo

func (m jobRepos) GetByUUID(_ context.Context, id uuid.UUID) (*repo.Repo, error) {
	if r, ok := m[id]; ok {
		c := *r
		return &c, nil
	}
	return nil, fmt.Errorf("%w: %s", repo.ErrNotFound, id)
}

func (e *platformEnv) addRepo(ns model.Principal, nsName, name string, vis model.Visibility) *repo.Repo {
	r := &repo.Repo{
		UUID: uuid.New(), ID: model.RepoID{Type: model.RepoTypeModel, Namespace: nsName, Name: name},
		NamespaceID: ns.NamespaceID, Visibility: vis,
	}
	e.repos[r.UUID] = r
	return r
}

// withToken returns p authenticating with a Token of the scope and
// restrictions.
func withToken(p model.Principal, scope model.Scope, r model.TokenRestrictions) model.Principal {
	p.Method = model.AuthToken
	p.Token = &model.TokenInfo{ID: uuid.New(), Scope: scope, Restrictions: r}
	return p
}

// jobAuthzFixture is a private Organisation Repo, a public Repo in alice's
// Namespace, a deleted Repo and a purged one, with a Job of alice's on each,
// an Instance-wide Job of alice's and a system Job.
type jobAuthzFixture struct {
	e                                                *platformEnv
	secret, public                                   *repo.Repo
	acme                                             model.Principal
	zipSecret, pullPublic, deleted, purged, instance uuid.UUID
	gcJob                                            uuid.UUID
}

func newJobAuthzFixture(t *testing.T) *jobAuthzFixture {
	e := newPlatformEnv(t, nil)
	f := &jobAuthzFixture{e: e}
	// acme stands in for an Organisation; alice is a write Member.
	f.acme = e.newUser("acme", false)
	alice := e.users["alice"]
	e.authz.SetRole(alice.UserID, f.acme.NamespaceID, model.RoleWrite)
	f.secret = e.addRepo(f.acme, "acme", "secret", model.VisibilityPrivate)
	f.public = e.addRepo(alice, "alice", "public", model.VisibilityPublic)
	old := e.addRepo(f.acme, "acme", "old", model.VisibilityPrivate)
	now := time.Now()
	old.DeletedAt = &now

	f.zipSecret = e.enqueue("alice", jobs.NewJob{Kind: jobs.KindZipExport, Repo: f.secret.UUID})
	_, err := e.db.NewUpdate().Model((*db.Job)(nil)).Set("result = ?", json.RawMessage(`{"url":"https://hangar.example/exports/secret.zip"}`)).
		Where("id = ?", f.zipSecret).Exec(context.Background())
	require.NoError(t, err)
	f.pullPublic = e.enqueue("alice", jobs.NewJob{Kind: jobs.KindPull, Repo: f.public.UUID})
	f.deleted = e.enqueue("alice", jobs.NewJob{Kind: jobs.KindIndex, Repo: old.UUID})
	f.purged = e.enqueue("alice", jobs.NewJob{Kind: jobs.KindScan, Repo: uuid.New()})
	f.instance = e.enqueue("alice", jobs.NewJob{Kind: jobs.KindUploadExpire})
	f.gcJob = e.enqueue("", jobs.NewJob{Kind: jobs.KindGC})

	write := func(name string, r model.TokenRestrictions) {
		base := e.users["alice"]
		if strings.HasPrefix(name, "root") {
			base = e.users["root"]
		}
		e.users[name] = withToken(base, model.ScopeWrite, r)
	}
	write("alice-only-public", model.TokenRestrictions{Repos: []string{"model:alice/public"}})
	write("alice-only-secret", model.TokenRestrictions{Repos: []string{"model:acme/secret"}})
	write("alice-ns-acme", model.TokenRestrictions{Namespaces: []string{"acme"}})
	write("root-only-public", model.TokenRestrictions{Repos: []string{"model:alice/public"}})
	write("root-write", model.TokenRestrictions{})
	return f
}

func TestJobsCheckRepoAccess(t *testing.T) {
	t.Parallel()
	f := newJobAuthzFixture(t)
	e := f.e
	jobs := map[string]uuid.UUID{
		"zipSecret": f.zipSecret, "pullPublic": f.pullPublic, "deleted": f.deleted,
		"purged": f.purged, "instance": f.instance, "gc": f.gcJob,
	}
	// retry is used to check write access without changing anything: the
	// Jobs are pending, so a caller who may retry gets 409, not 403 or 404.
	for _, tc := range []struct {
		user, job  string
		get, retry int
	}{
		{"alice", "zipSecret", 200, 409},
		{"alice", "pullPublic", 200, 409},
		{"alice", "instance", 200, 409},
		{"alice", "deleted", 404, 404},
		{"alice", "purged", 404, 404},
		{"alice", "gc", 404, 404},
		{"bob", "zipSecret", 404, 404},
		{"bob", "pullPublic", 404, 404},

		// A Token restricted to one Repo sees only that Repo's Jobs.
		{"alice-only-public", "pullPublic", 200, 409},
		{"alice-only-public", "zipSecret", 404, 404},
		{"alice-only-public", "instance", 404, 404},
		{"alice-only-secret", "zipSecret", 200, 409},
		{"alice-only-secret", "pullPublic", 404, 404},
		{"alice-ns-acme", "zipSecret", 200, 409},
		{"alice-ns-acme", "pullPublic", 404, 404},
		{"alice-read", "zipSecret", 200, 403},

		{"root", "zipSecret", 200, 409},
		{"root", "deleted", 200, 409},
		{"root", "purged", 200, 409},
		{"root", "gc", 200, 409},
		{"root-write", "zipSecret", 200, 409},
		// Restrictions narrow an Instance admin too.
		{"root-only-public", "pullPublic", 200, 409},
		{"root-only-public", "zipSecret", 404, 404},
		{"root-only-public", "purged", 404, 404},
		{"root-only-public", "gc", 404, 404},
	} {
		id := jobs[tc.job]
		code, body := e.do(tc.user, http.MethodGet, "/jobs/"+id.String(), nil)
		require.Equal(t, tc.get, code, "%s GET %s: %s", tc.user, tc.job, body)
		code, body = e.do(tc.user, http.MethodPost, "/jobs/"+id.String()+"/retry", nil)
		require.Equal(t, tc.retry, code, "%s retry %s: %s", tc.user, tc.job, body)
		code, body = e.do(tc.user, http.MethodGet, "/jobs?limit=500", nil)
		require.Equal(t, http.StatusOK, code)
		require.Equal(t, tc.get == 200, listed(t, body, id), "%s list has %s", tc.user, tc.job)
	}
}

func listed(t *testing.T, body []byte, id uuid.UUID) bool {
	t.Helper()
	for _, j := range decode[gen.JobList](t, body).Items {
		if j.Id == id {
			return true
		}
	}
	return false
}

func TestJobsFollowRevokedRepoAccess(t *testing.T) {
	t.Parallel()
	f := newJobAuthzFixture(t)
	e := f.e
	alice := e.users["alice"]
	path := "/jobs/" + f.zipSecret.String()

	code, body := e.do("alice", http.MethodGet, path, nil)
	require.Equal(t, http.StatusOK, code)
	require.Contains(t, string(body), "secret.zip")

	// Demoted to read: the Job is still visible but cannot be changed.
	e.authz.SetRole(alice.UserID, f.acme.NamespaceID, model.RoleRead)
	code, _ = e.do("alice", http.MethodGet, path, nil)
	require.Equal(t, http.StatusOK, code)
	for _, action := range []string{"/cancel", "/retry"} {
		code, body = e.do("alice", http.MethodPost, path+action, nil)
		require.Equal(t, http.StatusForbidden, code, action)
		require.Contains(t, string(body), "write access to this Job's Repo")
	}

	// Removed from the Organisation: the private Repo's Job and its zip
	// URL are gone, as if they never existed.
	e.authz.SetRole(alice.UserID, f.acme.NamespaceID, "")
	code, body = e.do("alice", http.MethodGet, path, nil)
	require.Equal(t, http.StatusNotFound, code)
	require.NotContains(t, string(body), "secret.zip")
	for _, action := range []string{"/cancel", "/retry"} {
		code, _ = e.do("alice", http.MethodPost, path+action, nil)
		require.Equal(t, http.StatusNotFound, code, action)
	}
	code, body = e.do("alice", http.MethodGet, "/jobs", nil)
	require.Equal(t, http.StatusOK, code)
	require.False(t, listed(t, body, f.zipSecret))
	require.True(t, listed(t, body, f.pullPublic), "Jobs on Repos alice can still read stay visible")
	require.NotContains(t, string(body), "secret.zip")

	// A collaborator Role on the Repo restores access.
	e.authz.SetCollaborator(alice.UserID, f.secret.UUID, model.RoleWrite)
	code, _ = e.do("alice", http.MethodGet, path, nil)
	require.Equal(t, http.StatusOK, code)
	code, body = e.do("alice", http.MethodPost, path+"/cancel", nil)
	require.Equal(t, http.StatusOK, code, string(body))
}

func TestJobsListPagesPastHiddenJobs(t *testing.T) {
	t.Parallel()
	f := newJobAuthzFixture(t)
	e := f.e
	// Twenty-five newer Jobs on the private Repo, which the restricted
	// Token cannot see, stand in front of the one it can.
	for range 25 {
		e.enqueue("alice", jobs.NewJob{Kind: jobs.KindIndex, Repo: f.secret.UUID})
	}
	var seen []uuid.UUID
	cursor := ""
	for range 10 {
		code, body := e.do("alice-only-public", http.MethodGet, "/jobs?limit=1"+cursor, nil)
		require.Equal(t, http.StatusOK, code)
		page := decode[gen.JobList](t, body)
		require.LessOrEqual(t, len(page.Items), 1)
		for _, j := range page.Items {
			seen = append(seen, j.Id)
		}
		if page.NextCursor == nil {
			break
		}
		cursor = "&cursor=" + *page.NextCursor
	}
	require.Equal(t, []uuid.UUID{f.pullPublic}, seen, "the listing reaches the visible Job and returns it once")
}

// TestJobsNameTheirRepo checks that Repo-scoped Jobs carry the Repo's ID
// in payload.repo, which the Jobs drawer titles them by, also when the
// handler that queued them put only the Repo UUID in the payload, and that
// a payload's own repo is kept.
func TestJobsNameTheirRepo(t *testing.T) {
	t.Parallel()
	f := newJobAuthzFixture(t)
	e := f.e
	payloadRepo := func(user string, id uuid.UUID) any {
		t.Helper()
		code, body := e.do(user, http.MethodGet, "/jobs/"+id.String(), nil)
		require.Equal(t, http.StatusOK, code, string(body))
		var j gen.Job
		require.NoError(t, json.Unmarshal(body, &j))
		if j.Payload == nil {
			return nil
		}
		return (*j.Payload)["repo"]
	}
	require.Equal(t, "acme/secret", payloadRepo("alice", f.zipSecret))
	require.Equal(t, "alice/public", payloadRepo("alice", f.pullPublic))
	require.Nil(t, payloadRepo("alice", f.instance), "an Instance-wide Job names no Repo")
	require.Nil(t, payloadRepo("root", f.purged), "a purged Repo has no ID to show")
	own := e.enqueue("alice", jobs.NewJob{Kind: jobs.KindPull, Repo: f.public.UUID, Payload: map[string]any{"repo": "upstream/name"}})
	require.Equal(t, "upstream/name", payloadRepo("alice", own))

	code, body := e.do("alice", http.MethodGet, "/jobs?limit=500", nil)
	require.Equal(t, http.StatusOK, code)
	var list gen.JobList
	require.NoError(t, json.Unmarshal(body, &list))
	found := false
	for _, j := range list.Items {
		if j.Id == f.zipSecret {
			found = true
			require.Equal(t, "acme/secret", (*j.Payload)["repo"])
		}
	}
	require.True(t, found)
}
