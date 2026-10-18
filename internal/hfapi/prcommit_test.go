package hfapi_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/community"
	"github.com/hangar-hub/hangar/internal/hfapi"
	"github.com/hangar-hub/hangar/internal/hfapi/hfapitest"
	"github.com/hangar-hub/hangar/internal/model"
	"github.com/hangar-hub/hangar/internal/repo"
)

func lfsLine(p, oid string, size int) map[string]any {
	v := map[string]any{"path": p, "algo": "sha256", "oid": oid}
	if size >= 0 {
		v["size"] = size
	}
	return map[string]any{"key": "lfsFile", "value": v}
}

func deleteLine(key, p string) map[string]any {
	return map[string]any{"key": key, "value": map[string]any{"path": p}}
}

// requireNoPullRequests checks that the Repo has no discussions and no
// pull request refs.
func requireNoPullRequests(t *testing.T, e *hfapitest.Env, rp *repo.Repo) {
	t.Helper()
	list, total, err := e.Community.List(context.Background(), rp.UUID, community.ListOptions{})
	require.NoError(t, err)
	require.Zero(t, total)
	require.Empty(t, list)
	refs := decode[map[string]any](t, e.Request(t, http.MethodGet, "/api/models/"+rp.ID.Path()+"/refs?include_prs=1", "alice", ""))
	require.Empty(t, refs["pullRequests"])
	page := decode[map[string]any](t, e.Request(t, http.MethodGet, "/api/models/"+rp.ID.Path()+"/discussions", "alice", ""))
	require.Empty(t, page["discussions"])
}

func TestCreatePRFailedCommitLeavesNoPullRequest(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	weights := "weights"
	rp, base := seed(t, e, "alice", modelID("alice", "probe"), model.VisibilityPublic,
		map[string]string{"a.txt": "a", "dir/b.txt": "b", "w.safetensors": weights})
	unknown := strings.Repeat("a", 64)

	cases := []struct {
		name   string
		lines  []map[string]any
		status int
		code   string
	}{
		// create_commit([CommitOperationDelete("ghost.txt")], create_pr=True)
		{"delete a missing file", []map[string]any{deleteLine("deletedFile", "ghost.txt")}, http.StatusNotFound, "EntryNotFound"},
		{"delete a folder as a file", []map[string]any{deleteLine("deletedFile", "dir")}, http.StatusNotFound, "EntryNotFound"},
		{"delete a file twice", []map[string]any{deleteLine("deletedFile", "a.txt"), deleteLine("deletedFile", "a.txt")}, http.StatusNotFound, "EntryNotFound"},
		{"delete a missing folder", []map[string]any{deleteLine("deletedFolder", "ghost/")}, http.StatusNotFound, "EntryNotFound"},
		{"delete a file as a folder", []map[string]any{deleteLine("deletedFolder", "a.txt/")}, http.StatusNotFound, "EntryNotFound"},
		{"unknown LFS object with a size", []map[string]any{fileLine("ok.txt", "ok"), lfsLine("m.bin", unknown, 3)}, http.StatusBadRequest, ""},
		{"unknown LFS object without a size", []map[string]any{lfsLine("m.bin", unknown, -1)}, http.StatusBadRequest, ""},
		{"LFS object of another size", []map[string]any{lfsLine("m.bin", sha256hex(weights), len(weights)+1)}, http.StatusBadRequest, ""},
	}
	for _, tc := range cases {
		lines := append([]map[string]any{header("Bad PR", string(base))}, tc.lines...)
		r := e.Request(t, http.MethodPost, "/api/models/alice/probe/commit/main?create_pr=1", "bob", ndjson(t, lines...))
		if tc.code != "" {
			requireHubError(t, r, tc.status, tc.code)
		} else {
			require.Equal(t, tc.status, r.StatusCode, tc.name+": "+string(readBody(t, r)))
		}
		requireNoPullRequests(t, e, rp)
	}

	// Never stricter than the Commit: operations that only succeed because
	// of earlier operations in the same commit still open the pull request.
	r := e.Request(t, http.MethodPost, "/api/models/alice/probe/commit/main?create_pr=1", "bob", ndjson(t,
		header("Good PR", string(base)),
		fileLine("new/f.txt", "f"), deleteLine("deletedFolder", "new/"),
		fileLine("x.txt", "x"), deleteLine("deletedFile", "x.txt"), fileLine("x.txt", "y"),
		deleteLine("deletedFile", "a.txt"), deleteLine("deletedFolder", "dir/"),
		lfsLine("copy.safetensors", sha256hex(weights), -1), lfsLine("w2.safetensors", sha256hex(weights), len(weights)),
	))
	require.Equal(t, http.StatusOK, r.StatusCode, string(readBody(t, r)))
	require.Equal(t, e.URL+"/alice/probe/discussions/1", decode[map[string]any](t, r)["pullRequestUrl"])
	require.Equal(t, "y", string(readBody(t, e.Request(t, http.MethodGet, "/alice/probe/resolve/refs%2Fpr%2F1/x.txt", "", ""))))
	requireHubError(t, e.Request(t, http.MethodHead, "/alice/probe/resolve/refs%2Fpr%2F1/a.txt", "", ""), http.StatusNotFound, "EntryNotFound")
	requireHubError(t, e.Request(t, http.MethodHead, "/alice/probe/resolve/refs%2Fpr%2F1/new/f.txt", "", ""), http.StatusNotFound, "EntryNotFound")
}

func TestCreatePRVerifiesPresignedLFSObjects(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	rp, base := seed(t, e, "alice", modelID("alice", "presigned"), model.VisibilityPublic, map[string]string{"a.txt": "a"})
	bob := e.Users["bob"].Principal.UserID
	put := func(content string) string {
		oid := sha256hex(content)
		require.NoError(t, e.RepoFake.LFS.Stage(ctx, oid, []byte(content), bob))
		return oid
	}

	// An object bob staged with the wrong content fails verification.
	bad := sha256hex("expected")
	require.NoError(t, e.RepoFake.LFS.Stage(ctx, bad, []byte("tampered"), bob))
	r := e.Request(t, http.MethodPost, "/api/models/alice/presigned/commit/main?create_pr=1", "bob",
		ndjson(t, header("Bad", string(base)), lfsLine("m.bin", bad, len("tampered"))))
	require.Equal(t, http.StatusBadRequest, r.StatusCode, string(readBody(t, r)))
	requireNoPullRequests(t, e, rp)

	// An object written through a presigned PUT and not verified yet is
	// verified before the pull request opens.
	oid := put("uploaded")
	r = e.Request(t, http.MethodPost, "/api/models/alice/presigned/commit/main?create_pr=1", "bob",
		ndjson(t, header("Upload", string(base)), lfsLine("m.bin", oid, len("uploaded"))))
	require.Equal(t, http.StatusOK, r.StatusCode, string(readBody(t, r)))
	require.Equal(t, "uploaded", string(readBody(t, e.Request(t, http.MethodGet, "/alice/presigned/resolve/refs%2Fpr%2F1/m.bin", "", ""))))
}

// slowLFS holds Verify until release is closed.
type slowLFS struct {
	repo.LFSStore
	release chan struct{}
}

func (s slowLFS) Verify(ctx context.Context, oid string, size int64, userID uuid.UUID) (repo.LFSObject, error) {
	<-s.release
	return s.LFSStore.Verify(ctx, oid, size, userID)
}

func TestCreatePRVerifyWait(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	e := newEnv(t, func(o *hfapitest.Options) {
		o.Settings = func(s *hfapi.Settings) { s.VerifyWait = 50 * time.Millisecond }
		o.Deps = func(d *hfapi.Deps) { d.LFS = slowLFS{LFSStore: d.LFS, release: release} }
	})
	ctx := context.Background()
	rp, base := seed(t, e, "alice", modelID("alice", "slow"), model.VisibilityPublic, map[string]string{"a.txt": "a"})
	content := "slow object"
	oid := sha256hex(content)
	require.NoError(t, e.RepoFake.LFS.Stage(ctx, oid, []byte(content), e.Users["bob"].Principal.UserID))
	body := ndjson(t, header("Upload", string(base)), lfsLine("m.bin", oid, len(content)))

	// The wait runs out: no pull request, and the client is told to retry.
	r := e.Request(t, http.MethodPost, "/api/models/alice/slow/commit/main?create_pr=1", "bob", body)
	require.Equal(t, http.StatusBadRequest, r.StatusCode)
	require.Contains(t, r.Header.Get("X-Error-Message"), "try again later")
	requireNoPullRequests(t, e, rp)

	// The verification carries on after the wait, so a retry succeeds.
	close(release)
	require.Eventually(t, func() bool {
		obj, err := e.LFS.Stat(ctx, oid)
		return err == nil && obj.Verified
	}, 5*time.Second, 10*time.Millisecond)
	r = e.Request(t, http.MethodPost, "/api/models/alice/slow/commit/main?create_pr=1", "bob", body)
	require.Equal(t, http.StatusOK, r.StatusCode, string(readBody(t, r)))
}

// failingCommits fails every Commit to a pull request ref.
type failingCommits struct {
	repo.Service
}

func (f failingCommits) Commit(ctx context.Context, p repo.CommitParams) (*repo.CommitResult, error) {
	if _, ok := model.ParsePRRef(p.Revision); ok {
		return nil, errors.New("storage unavailable")
	}
	return f.Service.Commit(ctx, p)
}

func TestCreatePRDiscardsPullRequestWhenCommitFails(t *testing.T) {
	t.Parallel()
	e := newEnv(t, func(o *hfapitest.Options) {
		o.Deps = func(d *hfapi.Deps) { d.Repos = failingCommits{Service: d.Repos} }
	})
	rp, base := seed(t, e, "alice", modelID("alice", "fails"), model.VisibilityPublic, map[string]string{"a.txt": "a"})
	r := e.Request(t, http.MethodPost, "/api/models/alice/fails/commit/main?create_pr=1", "bob",
		ndjson(t, header("Fix", string(base)), fileLine("a.txt", "b")))
	require.Equal(t, http.StatusInternalServerError, r.StatusCode)
	requireNoPullRequests(t, e, rp)
}

// undiscardable refuses every discard, as the Community service does when
// someone commented on the pull request before the Commit failed.
type undiscardable struct{ community.Service }

func (undiscardable) Discard(context.Context, uuid.UUID, int) error {
	return community.ErrNotDiscardable
}

func TestCreatePRClosesPullRequestThatCannotBeDiscarded(t *testing.T) {
	t.Parallel()
	e := newEnv(t, func(o *hfapitest.Options) {
		o.Deps = func(d *hfapi.Deps) {
			d.Repos = failingCommits{Service: d.Repos}
			d.Community = undiscardable{Service: d.Community}
		}
	})
	rp, base := seed(t, e, "alice", modelID("alice", "kept"), model.VisibilityPublic, map[string]string{"a.txt": "a"})
	r := e.Request(t, http.MethodPost, "/api/models/alice/kept/commit/main?create_pr=1", "bob",
		ndjson(t, header("Fix", string(base)), fileLine("a.txt", "b")))
	require.Equal(t, http.StatusInternalServerError, r.StatusCode)

	list, total, err := e.Community.List(context.Background(), rp.UUID, community.ListOptions{})
	require.NoError(t, err)
	require.Equal(t, 1, total)
	d, err := e.Community.Get(context.Background(), rp.UUID, list[0].Num)
	require.NoError(t, err)
	require.True(t, d.IsPullRequest())
	require.Equal(t, community.StatusClosed, d.Status)
	last := d.Events[len(d.Events)-1]
	require.Equal(t, community.EventStatusChange, last.Type)
	require.Contains(t, last.Body, "the commit that opened this pull request failed")
}

func TestCreatePROnMirror(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	id := modelID("alice", "mirror")
	rp, err := e.Repos.Create(ctx, repo.CreateParams{ID: id, Visibility: model.VisibilityPublic, Mirror: true, Actor: e.Users["alice"].Principal})
	require.NoError(t, err)
	res, err := e.Repos.Commit(ctx, repo.CommitParams{Repo: id, Revision: "main", Summary: "Pull", AllowMirror: true, Actor: e.Users["alice"].Principal,
		Operations: []repo.Operation{{Kind: repo.OpAdd, Path: "a.txt", Content: strings.NewReader("a")}}})
	require.NoError(t, err)
	r := e.Request(t, http.MethodPost, "/api/models/alice/mirror/commit/main?create_pr=1", "bob",
		ndjson(t, header("Fix", string(res.SHA)), fileLine("a.txt", "b")))
	require.Equal(t, http.StatusForbidden, r.StatusCode, string(readBody(t, r)))
	requireNoPullRequests(t, e, rp)
}
