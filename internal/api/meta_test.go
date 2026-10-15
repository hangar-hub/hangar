package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/api"
	"github.com/jamesagarside/hangar/internal/api/gen"
	"github.com/jamesagarside/hangar/internal/audit"
	"github.com/jamesagarside/hangar/internal/audit/audittest"
	"github.com/jamesagarside/hangar/internal/auth/authtest"
	"github.com/jamesagarside/hangar/internal/collections"
	"github.com/jamesagarside/hangar/internal/community/communitydb"
	"github.com/jamesagarside/hangar/internal/db"
	"github.com/jamesagarside/hangar/internal/db/dbtest"
	"github.com/jamesagarside/hangar/internal/httpx"
	"github.com/jamesagarside/hangar/internal/index/indexdb"
	"github.com/jamesagarside/hangar/internal/index/indextest"
	"github.com/jamesagarside/hangar/internal/index/safetensors"
	"github.com/jamesagarside/hangar/internal/jobs"
	"github.com/jamesagarside/hangar/internal/jobs/jobstest"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/org/orgtest"
	"github.com/jamesagarside/hangar/internal/repo"
	"github.com/jamesagarside/hangar/internal/repo/repotest"
	"github.com/jamesagarside/hangar/internal/scan"
	"github.com/jamesagarside/hangar/internal/secrets"
	"github.com/jamesagarside/hangar/internal/webhook"
)

// metaServer composes the W7 handlers; gen.Unimplemented sits one level
// deeper so the real methods win.
type metaServer struct {
	*api.MetadataAPI
	*api.CommunityAPI
	*api.CollectionsAPI
	*api.WebhooksAPI
	metaFallback
}

type metaFallback struct{ gen.Unimplemented }

type metaEnv struct {
	t          *testing.T
	srv        *httptest.Server
	db         *db.DB
	repos      *repotest.Fake
	authz      *authtest.Authz
	index      *indexdb.Service
	queue      *jobstest.Fake
	audit      *audittest.Fake
	receiver   *httptest.Server
	got        chan *http.Request
	alice, bob model.Principal
	model      *repo.Repo
	sha        model.CommitSHA
}

func newMetaEnv(t *testing.T) *metaEnv {
	t.Helper()
	return newMetaEnvWith(t, nil)
}

// newMetaEnvWith is newMetaEnv with a hook that changes the dependencies.
func newMetaEnvWith(t *testing.T, with func(*api.MetaDeps)) *metaEnv {
	t.Helper()
	ctx := context.Background()
	d := dbtest.SQLite(t)
	e := &metaEnv{t: t, db: d, repos: repotest.NewFake(t), authz: authtest.NewAuthz(), audit: audittest.NewFake(), got: make(chan *http.Request, 16)}
	orgs := orgtest.NewFake()
	var err error
	e.alice, err = orgs.AddUser("alice")
	require.NoError(t, err)
	e.bob, err = orgs.AddUser("bob")
	require.NoError(t, err)
	indextest.InsertUser(t, d, e.alice)
	indextest.InsertUser(t, d, e.bob)
	authn := authtest.NewAuthenticator()
	authn.Add("alice-token", e.alice)
	authn.Add("bob-token", e.bob)

	e.model, err = e.repos.Create(ctx, repo.CreateParams{ID: model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "tiny"}, Actor: e.alice})
	require.NoError(t, err)
	indextest.InsertRepoRow(t, d, e.model)
	e.sha = indextest.Commit(t, e.repos, e.model.ID, map[string][]byte{
		"model.safetensors": indextest.Safetensors(t, nil, safetensors.Tensor{Name: "w", DType: "F16", Shape: []int64{4, 4}}),
		"tiny-Q4_0.gguf":    indextest.LlamaGGUF(t, 2, "Q4_0"),
		"tiny-Q8_0.gguf":    indextest.LlamaGGUF(t, 7, "Q8_0"),
		"notes.txt":         []byte("hello"),
	})
	q8, err := e.repos.Entry(ctx, e.model.ID, e.sha, "tiny-Q8_0.gguf")
	require.NoError(t, err)
	require.NoError(t, e.repos.SetLFSPresence(ctx, e.model.ID, []repo.LFSRef{{OID: q8.LFS.OID, Size: q8.Size, Present: false}}))

	sealer, err := secrets.New([]byte("0123456789abcdef0123456789abcdef"))
	require.NoError(t, err)
	reg := jobs.NewRegistry()
	e.queue = jobstest.NewFake(reg)
	hooks := webhook.New(webhook.Options{DB: d, Queue: e.queue, Sealer: sealer})
	require.NoError(t, hooks.Register(reg))
	e.index = indexdb.New(indexdb.Options{DB: d, Repos: e.repos})
	deps := api.MetaDeps{
		Repos: e.repos, Authz: e.authz, Orgs: orgs, Index: e.index, Scanner: scan.New(scan.Options{DB: d, Repos: e.repos}),
		Community:   communitydb.New(communitydb.Options{DB: d, Repos: e.repos, Webhooks: hooks}),
		Collections: collections.New(collections.Options{DB: d, Orgs: orgs, Repos: e.repos, Authz: e.authz}),
		Webhooks:    hooks, Audit: e.audit,
	}
	if with != nil {
		with(&deps)
	}
	s := metaServer{}
	s.MetadataAPI, err = api.NewMetadataAPI(deps)
	require.NoError(t, err)
	s.CommunityAPI, err = api.NewCommunityAPI(deps)
	require.NoError(t, err)
	s.CollectionsAPI, err = api.NewCollectionsAPI(deps)
	require.NoError(t, err)
	s.WebhooksAPI, err = api.NewWebhooksAPI(deps)
	require.NoError(t, err)
	native := gen.HandlerWithOptions(s, gen.ChiServerOptions{BaseURL: "/api/hangar/v1",
		ErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			httpx.WriteProblem(w, r, &httpx.Problem{Status: http.StatusBadRequest, Detail: err.Error()})
		}})
	h := httpx.Dispatch(httpx.Protocols{Native: native}, "http://hangar.test")
	e.srv = httptest.NewServer(httpx.RequestIDs(httpx.Authenticate(authn, nil)(h)))
	t.Cleanup(e.srv.Close)
	e.receiver = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		e.got <- r
	}))
	t.Cleanup(e.receiver.Close)
	return e
}

func (e *metaEnv) do(method, path, token string, body any) (int, []byte) {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		require.NoError(e.t, err)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, e.srv.URL+"/api/hangar/v1"+path, rd)
	require.NoError(e.t, err)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(e.t, err)
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	require.NoError(e.t, err)
	return resp.StatusCode, b
}

func (e *metaEnv) json(method, path, token string, body any, want int, out any) {
	e.t.Helper()
	status, b := e.do(method, path, token, body)
	require.Equal(e.t, want, status, "%s %s: %s", method, path, b)
	if out != nil {
		require.NoError(e.t, json.Unmarshal(b, out), string(b))
	}
}

func TestDatasetMetadataListsTheModelsTheCallerCanRead(t *testing.T) {
	var asked []model.Principal
	fail := false
	e := newMetaEnvWith(t, func(d *api.MetaDeps) {
		d.DatasetModels = func(_ context.Context, p model.Principal, r *repo.Repo) ([]model.RepoID, error) {
			if fail {
				return nil, errors.New("dependencies unavailable")
			}
			asked = append(asked, p)
			require.Equal(t, "alice/reviews", r.ID.String())
			if p.Username != "bob" {
				return nil, nil
			}
			return []model.RepoID{{Type: model.RepoTypeModel, Namespace: "bob", Name: "classifier"}}, nil
		}
	})
	ctx := context.Background()
	ds, err := e.repos.Create(ctx, repo.CreateParams{ID: model.RepoID{Type: model.RepoTypeDataset, Namespace: "alice", Name: "reviews"}, Actor: e.alice})
	require.NoError(t, err)
	indextest.InsertRepoRow(t, e.db, ds)
	sha := indextest.Commit(t, e.repos, ds.ID, map[string][]byte{"data/train.csv": []byte("text\nhello\n")})
	_, err = e.index.Index(ctx, ds.UUID, ds.ID, sha)
	require.NoError(t, err)
	_, err = e.index.Index(ctx, e.model.UUID, e.model.ID, e.sha)
	require.NoError(t, err)

	var md gen.RepoMetadata
	e.json("GET", "/repos/datasets/alice/reviews/metadata", "bob-token", nil, http.StatusOK, &md)
	dataset := (*md.Summary)["dataset"].(map[string]any)
	require.Equal(t, []any{"bob/classifier"}, dataset["models"])
	e.json("GET", "/repos/datasets/alice/reviews/metadata", "alice-token", nil, http.StatusOK, &md)
	require.NotContains(t, (*md.Summary)["dataset"].(map[string]any), "models", "each caller gets the models they can read")
	require.Len(t, asked, 2)
	require.Equal(t, "bob", asked[0].Username)

	stored, err := e.index.Get(ctx, ds.UUID, sha)
	require.NoError(t, err)
	require.Empty(t, stored.Dataset.TrainedBy, "the list is never stored in the index")

	e.json("GET", "/repos/models/alice/tiny/metadata", "bob-token", nil, http.StatusOK, &md)
	require.Len(t, asked, 2, "model metadata does not look for models")

	fail = true
	status, _ := e.do("GET", "/repos/datasets/alice/reviews/metadata", "bob-token", nil)
	require.Equal(t, http.StatusInternalServerError, status)
}

func TestMetadataEndpoints(t *testing.T) {
	e := newMetaEnv(t)
	base := "/repos/models/alice/tiny"
	var md gen.RepoMetadata
	e.json("GET", base+"/metadata", "bob-token", nil, http.StatusOK, &md)
	require.Equal(t, gen.RepoMetadataStatusPending, md.Status)
	require.Equal(t, string(e.sha), md.Commit)

	_, err := e.index.Index(context.Background(), e.model.UUID, e.model.ID, e.sha)
	require.NoError(t, err)
	e.json("GET", base+"/metadata?rev=main", "bob-token", nil, http.StatusOK, &md)
	require.Equal(t, gen.RepoMetadataStatusReady, md.Status)
	require.NotNil(t, md.IndexedAt)
	require.Equal(t, []any{"gguf", "safetensors"}, (*md.Summary)["formats"])

	status, _ := e.do("GET", base+"/metadata?rev=nope", "bob-token", nil)
	require.Equal(t, http.StatusNotFound, status)
	status, _ = e.do("GET", "/repos/models/alice/missing/metadata", "", nil)
	require.Equal(t, http.StatusUnauthorized, status, "anonymous callers cannot tell missing from private")
	status, _ = e.do("GET", "/repos/models/alice/missing/metadata", "bob-token", nil)
	require.Equal(t, http.StatusNotFound, status)

	var header gen.FileHeader
	e.json("GET", base+"/files/main/model.safetensors/header", "bob-token", nil, http.StatusOK, &header)
	require.Equal(t, gen.Safetensors, header.Format)
	require.Contains(t, header.Header, "w")
	e.json("GET", base+"/files/main/tiny-Q4_0.gguf/header", "bob-token", nil, http.StatusOK, &header)
	require.Equal(t, gen.Gguf, header.Format)
	require.EqualValues(t, 3, header.Header["version"])
	status, _ = e.do("GET", base+"/files/main/notes.txt/header", "bob-token", nil)
	require.Equal(t, http.StatusNotFound, status)
	status, b := e.do("GET", base+"/files/main/tiny-Q8_0.gguf/header", "bob-token", nil)
	require.Equal(t, http.StatusNotFound, status)
	require.Contains(t, string(b), "not in the arsenal")

	var quants gen.QuantList
	e.json("GET", base+"/quants", "bob-token", nil, http.StatusOK, &quants)
	require.Len(t, quants.Items, 2)
	require.True(t, quants.Items[0].Present)
	require.False(t, quants.Items[1].Present, "Q8_0 is not in the arsenal")

	var like gen.LikeStatus
	status, _ = e.do("POST", base+"/like", "", nil)
	require.Equal(t, http.StatusUnauthorized, status)
	e.json("POST", base+"/like", "bob-token", nil, http.StatusOK, &like)
	require.Equal(t, gen.LikeStatus{Liked: true, Likes: 1}, like)
	e.json("DELETE", base+"/like", "bob-token", nil, http.StatusOK, &like)
	require.Equal(t, gen.LikeStatus{Liked: false, Likes: 0}, like)

	require.NoError(t, communitydb.New(communitydb.Options{DB: e.db, Repos: e.repos}).RecordDownload(context.Background(), e.model.UUID, time.Now()))
	var stats gen.RepoStats
	e.json("GET", base+"/stats?days=7", "bob-token", nil, http.StatusOK, &stats)
	require.Len(t, stats.Days, 7)
	require.Equal(t, int64(1), stats.Total)
	require.Equal(t, int64(1), stats.Days[6].Count)
	status, _ = e.do("GET", base+"/stats?days=0", "bob-token", nil)
	require.Equal(t, http.StatusBadRequest, status)
}

func TestDiscussionEndpoints(t *testing.T) {
	e := newMetaEnv(t)
	base := "/repos/models/alice/tiny/discussions"
	status, _ := e.do("POST", base, "", gen.DiscussionCreate{Title: "x"})
	require.Equal(t, http.StatusUnauthorized, status)

	var d gen.Discussion
	desc := "Why?"
	e.json("POST", base, "bob-token", gen.DiscussionCreate{Title: "Question", Description: &desc}, http.StatusCreated, &d)
	require.Equal(t, 1, d.Num)
	require.Equal(t, "bob", d.Author)
	yes, mainBranch := true, "main"
	var pr gen.Discussion
	e.json("POST", base, "bob-token", gen.DiscussionCreate{Title: "Change", PullRequest: &yes, Base: &mainBranch}, http.StatusCreated, &pr)
	require.Equal(t, gen.DiscussionKindPullRequest, pr.Kind)
	require.Equal(t, "refs/pr/2", *pr.PrRef)
	status, _ = e.do("POST", base, "bob-token", gen.DiscussionCreate{Title: "x", Base: &mainBranch})
	require.Equal(t, http.StatusBadRequest, status, "base needs a pull request")
	nope := "nope"
	status, _ = e.do("POST", base, "bob-token", gen.DiscussionCreate{Title: "x", PullRequest: &yes, Base: &nope})
	require.Equal(t, http.StatusNotFound, status)

	var list gen.DiscussionList
	e.json("GET", base+"?limit=1", "bob-token", nil, http.StatusOK, &list)
	require.Equal(t, 2, list.Total)
	require.NotNil(t, list.NextCursor)
	cursor := *list.NextCursor
	list = gen.DiscussionList{}
	e.json("GET", base+"?limit=1&cursor="+cursor, "bob-token", nil, http.StatusOK, &list)
	require.Equal(t, 1, list.Items[0].Num)
	require.Nil(t, list.NextCursor)
	e.json("GET", base+"?kind=pull_request&status=open&author=bob", "bob-token", nil, http.StatusOK, &list)
	require.Len(t, list.Items, 1)
	status, _ = e.do("GET", base+"?cursor=x", "bob-token", nil)
	require.Equal(t, http.StatusBadRequest, status)
	status, _ = e.do("GET", base+"?limit=501", "bob-token", nil)
	require.Equal(t, http.StatusBadRequest, status)

	var ev gen.DiscussionEvent
	e.json("POST", base+"/1/comments", "alice-token", gen.CommentCreate{Body: "Because."}, http.StatusCreated, &ev)
	require.Equal(t, "alice", ev.Author)
	status, _ = e.do("POST", base+"/1/comments", "alice-token", gen.CommentCreate{Body: " "})
	require.Equal(t, http.StatusBadRequest, status)

	// Only the author or a writer may change a discussion.
	closed := gen.DiscussionUpdateStatusClosed
	title := "Renamed"
	e.json("PATCH", base+"/1", "bob-token", gen.DiscussionUpdate{Title: &title, Status: &closed}, http.StatusOK, &d)
	require.Equal(t, "Renamed", d.Title)
	require.Equal(t, gen.DiscussionStatusClosed, d.Status)
	status, _ = e.do("PATCH", base+"/1", "bob-token", gen.DiscussionUpdate{})
	require.Equal(t, http.StatusBadRequest, status)
	open := gen.DiscussionUpdateStatusOpen
	e.json("PATCH", base+"/1", "alice-token", gen.DiscussionUpdate{Status: &open}, http.StatusOK, &d)
	require.Equal(t, gen.DiscussionStatusOpen, d.Status)
	e.json("GET", base+"/1", "bob-token", nil, http.StatusOK, &d)
	require.Len(t, *d.Events, 5)
	status, _ = e.do("GET", base+"/9", "bob-token", nil)
	require.Equal(t, http.StatusNotFound, status)

	// Commit to the pull request, then merge it as the Repo owner.
	ctx := context.Background()
	tip, err := e.repos.Resolve(ctx, e.model.ID, "main")
	require.NoError(t, err)
	_, err = e.repos.Commit(ctx, repo.CommitParams{Repo: e.model.ID, Revision: "refs/pr/2", CreateFrom: tip, Summary: "edit", Actor: e.bob,
		Operations: []repo.Operation{{Kind: repo.OpAdd, Path: "notes.txt", Content: bytes.NewReader([]byte("changed"))}}})
	require.NoError(t, err)
	e.json("GET", base+"/2", "bob-token", nil, http.StatusOK, &pr)
	require.Len(t, *pr.Changes, 1)
	require.Equal(t, "notes.txt", (*pr.Changes)[0].Path)
	status, _ = e.do("POST", base+"/2/merge", "bob-token", nil)
	require.Equal(t, http.StatusForbidden, status, "merging needs write access")
	comment := "Thanks"
	e.json("POST", base+"/2/merge", "alice-token", gen.MergeRequest{Comment: &comment}, http.StatusOK, &pr)
	require.Equal(t, gen.DiscussionStatusMerged, pr.Status)
	status, _ = e.do("POST", base+"/2/merge", "alice-token", nil)
	require.Equal(t, http.StatusUnprocessableEntity, status)
	status, _ = e.do("POST", base+"/1/merge", "alice-token", nil)
	require.Equal(t, http.StatusUnprocessableEntity, status)
	status, _ = e.do("PATCH", base+"/2", "alice-token", gen.DiscussionUpdate{Status: &closed})
	require.Equal(t, http.StatusUnprocessableEntity, status)
}

func TestCollectionEndpoints(t *testing.T) {
	e := newMetaEnv(t)
	status, _ := e.do("POST", "/collections", "", gen.CollectionCreate{Namespace: "alice", Title: "x"})
	require.Equal(t, http.StatusUnauthorized, status)
	var c gen.Collection
	desc := "Favourites"
	e.json("POST", "/collections", "alice-token", gen.CollectionCreate{Namespace: "alice", Title: "Tiny models", Description: &desc}, http.StatusCreated, &c)
	require.Equal(t, "tiny-models", c.Slug)
	status, _ = e.do("POST", "/collections", "alice-token", gen.CollectionCreate{Namespace: "alice", Title: "Tiny models"})
	require.Equal(t, http.StatusConflict, status)
	status, _ = e.do("POST", "/collections", "bob-token", gen.CollectionCreate{Namespace: "alice", Title: "Other"})
	require.Equal(t, http.StatusForbidden, status)

	note := "Start here"
	e.json("POST", "/collections/alice/tiny-models/items", "alice-token", gen.CollectionItemCreate{Type: "model", ItemId: "alice/tiny", Note: &note}, http.StatusOK, &c)
	require.Len(t, c.Items, 1)
	require.NotNil(t, c.Items[0].Repo)
	require.Equal(t, "alice/tiny", c.Items[0].Repo.Id)
	status, _ = e.do("POST", "/collections/alice/tiny-models/items", "alice-token", gen.CollectionItemCreate{Type: "model", ItemId: "alice/none"})
	require.Equal(t, http.StatusBadRequest, status)
	pos := 0
	e.json("PATCH", "/collections/alice/tiny-models/items/"+c.Items[0].Id.String(), "alice-token", gen.CollectionItemUpdate{Position: &pos}, http.StatusOK, &c)
	neg := -1
	status, _ = e.do("PATCH", "/collections/alice/tiny-models/items/"+c.Items[0].Id.String(), "alice-token", gen.CollectionItemUpdate{Position: &neg})
	require.Equal(t, http.StatusBadRequest, status)

	var list gen.CollectionList
	e.json("GET", "/collections?owner=alice&q=tiny&item=models/alice/tiny&limit=10", "", nil, http.StatusOK, &list)
	require.Len(t, list.Items, 1)
	status, _ = e.do("GET", "/collections?limit=0", "", nil)
	require.Equal(t, http.StatusBadRequest, status)
	e.json("GET", "/collections/alice/tiny-models", "bob-token", nil, http.StatusOK, &c)
	require.Equal(t, "Favourites", *c.Description)

	private := gen.Private
	theme := "green"
	e.json("PATCH", "/collections/alice/tiny-models", "alice-token", gen.CollectionUpdate{Visibility: &private, Theme: &theme}, http.StatusOK, &c)
	require.Equal(t, gen.Private, c.Visibility)
	status, _ = e.do("GET", "/collections/alice/tiny-models", "bob-token", nil)
	require.Equal(t, http.StatusNotFound, status)

	status, _ = e.do("DELETE", "/collections/alice/tiny-models/items/"+c.Items[0].Id.String(), "alice-token", nil)
	require.Equal(t, http.StatusNoContent, status)
	status, _ = e.do("DELETE", "/collections/alice/tiny-models", "alice-token", nil)
	require.Equal(t, http.StatusNoContent, status)
	status, _ = e.do("DELETE", "/collections/alice/tiny-models", "alice-token", nil)
	require.Equal(t, http.StatusNotFound, status)
}

func TestWebhookEndpoints(t *testing.T) {
	e := newMetaEnv(t)
	status, _ := e.do("GET", "/webhooks", "", nil)
	require.Equal(t, http.StatusUnauthorized, status)
	ns, repoID := "alice", "models/alice/tiny"
	status, _ = e.do("POST", "/webhooks", "alice-token", gen.WebhookCreate{Namespace: &ns, Repo: &repoID, Url: e.receiver.URL, Secret: "secret-value", Events: []string{"repo.updated"}})
	require.Equal(t, http.StatusBadRequest, status)
	status, _ = e.do("POST", "/webhooks", "bob-token", gen.WebhookCreate{Namespace: &ns, Url: e.receiver.URL, Secret: "secret-value", Events: []string{"repo.updated"}})
	require.Equal(t, http.StatusForbidden, status, "Bob does not administer alice")
	status, _ = e.do("POST", "/webhooks", "alice-token", gen.WebhookCreate{Namespace: &ns, Url: "nope", Secret: "secret-value", Events: []string{"repo.updated"}})
	require.Equal(t, http.StatusBadRequest, status)
	badRepo := "spaces/alice/tiny"
	status, _ = e.do("POST", "/webhooks", "alice-token", gen.WebhookCreate{Repo: &badRepo, Url: e.receiver.URL, Secret: "secret-value", Events: []string{"repo.updated"}})
	require.Equal(t, http.StatusBadRequest, status)

	var nsHook, repoHook gen.Webhook
	e.json("POST", "/webhooks", "alice-token", gen.WebhookCreate{Namespace: &ns, Url: e.receiver.URL + "/ns", Secret: "secret-value", Events: []string{"discussion.created"}}, http.StatusCreated, &nsHook)
	require.Equal(t, "alice", *nsHook.Namespace)
	require.True(t, nsHook.Enabled)
	off := false
	e.json("POST", "/webhooks", "alice-token", gen.WebhookCreate{Repo: &repoID, Url: e.receiver.URL + "/repo", Secret: "secret-value", Events: []string{"pr.merged"}, Enabled: &off}, http.StatusCreated, &repoHook)
	require.Equal(t, "models/alice/tiny", *repoHook.Repo)
	require.Len(t, e.audit.Events(), 2)
	require.NotContains(t, e.audit.Events()[0].Data["url_host"], "/ns")

	var list gen.WebhookList
	e.json("GET", "/webhooks", "alice-token", nil, http.StatusOK, &list)
	require.Len(t, list.Items, 2)
	e.json("GET", "/webhooks?ns=alice", "alice-token", nil, http.StatusOK, &list)
	require.Len(t, list.Items, 1)
	e.json("GET", "/webhooks?repo=models/alice/tiny", "alice-token", nil, http.StatusOK, &list)
	require.Len(t, list.Items, 1)
	e.json("GET", "/webhooks", "bob-token", nil, http.StatusOK, &list)
	require.Empty(t, list.Items)
	status, _ = e.do("GET", "/webhooks?ns=alice", "bob-token", nil)
	require.Equal(t, http.StatusForbidden, status)
	status, _ = e.do("GET", "/webhooks?repo=bad", "bob-token", nil)
	require.Equal(t, http.StatusBadRequest, status)
	status, _ = e.do("GET", "/webhooks/"+nsHook.Id.String(), "bob-token", nil)
	require.Equal(t, http.StatusNotFound, status, "other people's webhooks are not found")

	// An event in alice's Namespace is delivered.
	desc := "hello"
	status, _ = e.do("POST", "/repos/models/alice/tiny/discussions", "bob-token", gen.DiscussionCreate{Title: "Ping", Description: &desc})
	require.Equal(t, http.StatusCreated, status)
	e.queue.RunPending(context.Background())
	got := <-e.got
	require.Equal(t, "/ns", got.URL.Path)
	require.Equal(t, "discussion.created", got.Header.Get(webhook.HeaderEvent))

	var g gen.Webhook
	e.json("GET", "/webhooks/"+nsHook.Id.String(), "alice-token", nil, http.StatusOK, &g)
	require.NotNil(t, g.LastDelivery)
	require.Equal(t, 200, *g.LastDelivery.StatusCode)

	var deliveries gen.WebhookDeliveryList
	e.json("GET", "/webhooks/"+nsHook.Id.String()+"/deliveries?limit=1", "alice-token", nil, http.StatusOK, &deliveries)
	require.Len(t, deliveries.Items, 1)
	require.Contains(t, *deliveries.Items[0].Payload, "discussion.created")
	var del gen.WebhookDelivery
	e.json("POST", "/webhooks/"+nsHook.Id.String()+"/deliveries/"+deliveries.Items[0].Id.String()+"/redeliver", "alice-token", nil, http.StatusAccepted, &del)
	require.Equal(t, 0, del.Attempt)
	e.json("POST", "/webhooks/"+nsHook.Id.String()+"/test", "alice-token", nil, http.StatusAccepted, &del)
	require.Equal(t, "ping", del.Event)
	e.queue.RunPending(context.Background())
	require.Equal(t, "discussion.created", (<-e.got).Header.Get(webhook.HeaderEvent))
	require.Equal(t, "ping", (<-e.got).Header.Get(webhook.HeaderEvent))
	status, _ = e.do("GET", "/webhooks/"+nsHook.Id.String()+"/deliveries?limit=900", "alice-token", nil)
	require.Equal(t, http.StatusBadRequest, status)

	on := true
	events := []string{"pr.merged", "repo.deleted"}
	e.json("PATCH", "/webhooks/"+repoHook.Id.String(), "alice-token", gen.WebhookUpdate{Enabled: &on, Events: &events}, http.StatusOK, &g)
	require.True(t, g.Enabled)
	require.Equal(t, events, g.Events)
	status, _ = e.do("PATCH", "/webhooks/"+repoHook.Id.String(), "bob-token", gen.WebhookUpdate{Enabled: &on})
	require.Equal(t, http.StatusNotFound, status)
	status, _ = e.do("DELETE", "/webhooks/"+repoHook.Id.String(), "alice-token", nil)
	require.Equal(t, http.StatusNoContent, status)
	status, _ = e.do("DELETE", "/webhooks/"+repoHook.Id.String(), "alice-token", nil)
	require.Equal(t, http.StatusNotFound, status)

	// Every change is audited as webhook.changed (EVT-2), with the URL's
	// host only, and refused changes are not.
	receiver, err := url.Parse(e.receiver.URL)
	require.NoError(t, err)
	want := []struct{ id, change string }{
		{nsHook.Id.String(), "created"}, {repoHook.Id.String(), "created"},
		{repoHook.Id.String(), "updated"}, {repoHook.Id.String(), "deleted"},
	}
	audited := e.audit.Events()
	require.Len(t, audited, len(want))
	for i, w := range want {
		ev := audited[i]
		require.Equal(t, audit.WebhookChanged, ev.Action, "event %d", i)
		require.Equal(t, "webhook.changed", string(ev.Action))
		require.Equal(t, "alice", ev.Actor.Username, "event %d", i)
		require.Equal(t, "webhook", ev.TargetType)
		require.Equal(t, w.id, ev.TargetID, "event %d", i)
		require.Equal(t, map[string]any{"change": w.change, "url_host": receiver.Host}, ev.Data, "event %d", i)
	}
}
