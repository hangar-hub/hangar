package api_test

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/api"
	"github.com/jamesagarside/hangar/internal/api/gen"
	"github.com/jamesagarside/hangar/internal/auth/authtest"
	"github.com/jamesagarside/hangar/internal/collections"
	"github.com/jamesagarside/hangar/internal/collections/collectionstest"
	"github.com/jamesagarside/hangar/internal/community/communitytest"
	"github.com/jamesagarside/hangar/internal/db"
	"github.com/jamesagarside/hangar/internal/db/dbtest"
	"github.com/jamesagarside/hangar/internal/httpx"
	"github.com/jamesagarside/hangar/internal/index"
	"github.com/jamesagarside/hangar/internal/index/indextest"
	"github.com/jamesagarside/hangar/internal/jobs"
	"github.com/jamesagarside/hangar/internal/jobs/jobstest"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/org"
	"github.com/jamesagarside/hangar/internal/repo"
	"github.com/jamesagarside/hangar/internal/repo/repotest"
	"github.com/jamesagarside/hangar/internal/scan/scantest"
)

// orgs is the part of org.Service the handlers use, over the database.
type orgs struct {
	org.Service
	*repotest.DBNamespaces
}

func (o orgs) Get(ctx context.Context, name string) (*org.Namespace, error) {
	return o.DBNamespaces.Get(ctx, name)
}

func (o orgs) GetByID(ctx context.Context, id uuid.UUID) (*org.Namespace, error) {
	return o.DBNamespaces.GetByID(ctx, id)
}

func (o orgs) MembershipsOf(ctx context.Context, userID uuid.UUID) ([]org.Membership, error) {
	return o.DBNamespaces.MembershipsOf(ctx, userID)
}

func (o orgs) Members(ctx context.Context, name string) ([]org.Member, error) {
	ns, err := o.DBNamespaces.Get(ctx, name)
	if err != nil {
		return nil, err
	}
	var ms []db.Membership
	if err := o.DB.NewSelect().Model(&ms).Where("org_namespace_id = ?", ns.ID).Scan(ctx); err != nil {
		return nil, err
	}
	out := []org.Member{}
	for _, m := range ms {
		out = append(out, org.Member{UserID: m.UserID, Role: model.Role(m.Role)})
	}
	return out, nil
}

// SearchNamespaces implements api.NamespaceSearcher over the database.
func (o orgs) SearchNamespaces(ctx context.Context, q string, limit int) ([]org.Namespace, error) {
	var rows []db.Namespace
	if err := o.DB.NewSelect().Model(&rows).Where("instr(lower(name), lower(?)) > 0", q).Order("name").Limit(int64(limit)).Scan(ctx); err != nil {
		return nil, err
	}
	out := []org.Namespace{}
	for _, r := range rows {
		out = append(out, org.Namespace{ID: r.ID, Name: r.Name, Kind: r.Kind, DisplayName: r.DisplayName})
	}
	return out, nil
}

// reposServer wires the W2 handlers into the generated router, with
// gen.Unimplemented one level deeper so the handlers win.
type reposServer struct {
	*api.ReposAPI
	*api.UploadsAPI
	*api.SearchAPI
	*api.NamespacesAPI
	reposUnimplemented
}

type reposUnimplemented struct{ gen.Unimplemented }

type fixture struct {
	t      *testing.T
	ctx    context.Context
	real   *repotest.Real
	authn  *authtest.Authenticator
	authz  *authtest.Authz
	jobs   *jobstest.Fake
	colls  *collectionstest.Fake
	index  *indextest.Fake
	scans  *scantest.Fake
	srv    *httptest.Server
	signer *httpx.Signer
	users  map[string]model.Principal
}

func newFixture(t *testing.T, tweaks ...func(*api.RepoDeps)) *fixture {
	t.Helper()
	d := dbtest.SQLite(t)
	real := repotest.NewReal(t, d)
	f := &fixture{t: t, ctx: context.Background(), real: real, authn: authtest.NewAuthenticator(), authz: authtest.NewAuthz(),
		colls: collectionstest.NewFake(), index: indextest.NewFake(), scans: scantest.NewFake(real), users: map[string]model.Principal{}}
	reg := jobs.NewRegistry()
	f.jobs = jobstest.NewFake(reg)
	var err error
	f.signer, err = httpx.NewSigner(bytes.Repeat([]byte("k"), 32))
	require.NoError(t, err)
	mux := http.NewServeMux()
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	require.NoError(t, real.RegisterJobs(reg, real.LFS, repo.JobOptions{Blobs: real.Blobs, ExportURL: api.ExportURL(f.srv.URL, real.Blobs, f.signer)}))
	o := orgs{DBNamespaces: real.Namespaces}
	deps := api.RepoDeps{Repos: real, LFS: real.LFS, Authz: f.authz, Orgs: o, Jobs: f.jobs, Collections: f.colls, NamespaceSearch: o,
		Community: communitytest.NewFake(), Index: f.index, Scanner: f.scans, PublicURL: f.srv.URL + "/"}
	for _, tw := range tweaks {
		tw(&deps)
	}
	var s reposServer
	s.ReposAPI, err = api.NewReposAPI(deps)
	require.NoError(t, err)
	s.UploadsAPI, err = api.NewUploadsAPI(deps)
	require.NoError(t, err)
	s.SearchAPI, err = api.NewSearchAPI(deps)
	require.NoError(t, err)
	s.NamespacesAPI, err = api.NewNamespacesAPI(deps)
	require.NoError(t, err)
	native := gen.HandlerWithOptions(&s, gen.ChiServerOptions{BaseURL: "/api/hangar/v1",
		ErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			httpx.WriteProblem(w, r, &httpx.Problem{Status: http.StatusBadRequest, Detail: err.Error()})
		}})
	mux.Handle(api.ExportPath, httpx.RequestIDs(api.ExportHandler(real.Blobs, f.signer, nil)))
	mux.Handle("/", httpx.RequestIDs(httpx.Authenticate(f.authn, nil)(native)))
	return f
}

func (f *fixture) user(name string) model.Principal {
	f.t.Helper()
	p := f.real.AddUser(f.t, name)
	f.authn.Add("tok-"+name, p)
	f.users[name] = p
	return p
}

type resp struct {
	status int
	header http.Header
	body   []byte
}

func (r resp) json(t *testing.T, v any) {
	t.Helper()
	require.NoError(t, json.Unmarshal(r.body, v), string(r.body))
}

func (f *fixture) do(method, as, path string, body any) resp {
	f.t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case []byte:
		rd = bytes.NewReader(b)
	case string:
		rd = strings.NewReader(b)
	default:
		js, err := json.Marshal(b)
		require.NoError(f.t, err)
		rd = bytes.NewReader(js)
	}
	u := path
	if !strings.HasPrefix(path, "http") {
		u = f.srv.URL + "/api/hangar/v1" + path
	}
	req, err := http.NewRequestWithContext(f.ctx, method, u, rd)
	require.NoError(f.t, err)
	if as != "" {
		req.Header.Set("Authorization", "Bearer tok-"+as)
	}
	res, err := http.DefaultClient.Do(req)
	require.NoError(f.t, err)
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	require.NoError(f.t, err)
	return resp{status: res.StatusCode, header: res.Header, body: b}
}

func (f *fixture) expect(status int, method, as, path string, body any) resp {
	f.t.Helper()
	r := f.do(method, as, path, body)
	require.Equal(f.t, status, r.status, "%s %s: %s", method, path, r.body)
	return r
}

func (f *fixture) createRepo(as string, body gen.RepoCreate) gen.Repo {
	f.t.Helper()
	var out gen.Repo
	f.expect(http.StatusCreated, http.MethodPost, as, "/repos", body).json(f.t, &out)
	return out
}

func TestRepoLifecycleEndpoints(t *testing.T) {
	f := newFixture(t)
	f.user("alice")
	bob := f.user("bob")
	f.user("carol")
	labID := f.real.AddOrg(t, "lab", map[uuid.UUID]model.Role{bob.UserID: model.RoleWrite})
	f.authz.SetRole(bob.UserID, labID, model.RoleWrite)

	f.expect(http.StatusUnauthorized, http.MethodPost, "", "/repos", gen.RepoCreate{Type: "model", Namespace: "alice", Name: "x"})
	f.expect(http.StatusForbidden, http.MethodPost, "bob", "/repos", gen.RepoCreate{Type: "model", Namespace: "alice", Name: "x"})
	f.expect(http.StatusNotFound, http.MethodPost, "bob", "/repos", gen.RepoCreate{Type: "model", Namespace: "nobody", Name: "x"})
	f.expect(http.StatusBadRequest, http.MethodPost, "bob", "/repos", `{"type":"model","namespace":"lab","name":"x","extra":1}`)
	f.expect(http.StatusBadRequest, http.MethodPost, "bob", "/repos", gen.RepoCreate{Type: "space", Namespace: "lab", Name: "x"})
	f.expect(http.StatusBadRequest, http.MethodPost, "bob", "/repos", gen.RepoCreate{Type: "model", Namespace: "lab", Name: "bad--name"})

	priv := gen.Visibility("private")
	readme := true
	desc, lic := "Tiny", "mit"
	created := f.createRepo("alice", gen.RepoCreate{Type: "model", Namespace: "alice", Name: "tiny", Visibility: &priv, Readme: &readme, Description: &desc, Licence: &lic})
	require.Equal(t, "alice/tiny", created.Id)
	require.Equal(t, gen.Visibility("private"), created.Visibility)
	require.NotNil(t, created.Sha)
	require.True(t, created.Permissions.Admin)
	require.Equal(t, "mit", *created.Licence)
	f.expect(http.StatusConflict, http.MethodPost, "alice", "/repos", gen.RepoCreate{Type: "model", Namespace: "alice", Name: "TINY"})
	f.createRepo("bob", gen.RepoCreate{Type: "dataset", Namespace: "lab", Name: "data"})

	// Private Repos are invisible to others: 404, and 401 when anonymous.
	f.expect(http.StatusOK, http.MethodGet, "alice", "/repos/models/alice/tiny", nil)
	f.expect(http.StatusNotFound, http.MethodGet, "bob", "/repos/models/alice/tiny", nil)
	f.expect(http.StatusUnauthorized, http.MethodGet, "", "/repos/models/alice/tiny", nil)
	f.expect(http.StatusUnauthorized, http.MethodGet, "", "/repos/models/alice/none", nil)
	f.expect(http.StatusNotFound, http.MethodGet, "bob", "/repos/models/alice/none", nil)
	f.expect(http.StatusBadRequest, http.MethodGet, "bob", "/repos/spaces/alice/tiny", nil)

	// Settings need admin.
	pub := gen.Visibility("public")
	f.expect(http.StatusForbidden, http.MethodPatch, "bob", "/repos/datasets/lab/data", gen.RepoUpdate{Visibility: &pub})
	f.authz.SetRole(bob.UserID, labID, model.RoleAdmin)
	tags := []string{"csv"}
	mode := gen.GatingMode("manual")
	var updated gen.Repo
	f.expect(http.StatusOK, http.MethodPatch, "alice", "/repos/models/alice/tiny", gen.RepoUpdate{Visibility: &pub, Tags: &tags,
		Gating: &gen.Gating{Mode: &mode, Fields: &[]struct {
			Name     string               `json:"name"`
			Required *bool                `json:"required,omitempty"`
			Type     gen.GatingFieldsType `json:"type"`
		}{{Name: "why", Type: "text", Required: &readme}}}}).json(t, &updated)
	require.Equal(t, gen.Visibility("public"), updated.Visibility)
	require.Equal(t, []string{"csv"}, *updated.Tags)
	require.Equal(t, gen.RepoGated("manual"), updated.Gated)
	branch := "nope"
	f.expect(http.StatusNotFound, http.MethodPatch, "alice", "/repos/models/alice/tiny", gen.RepoUpdate{DefaultBranch: &branch})
	f.expect(http.StatusOK, http.MethodGet, "bob", "/repos/models/alice/tiny", nil)

	// Rename and transfer keep the old ID working.
	var renamed gen.Repo
	f.expect(http.StatusOK, http.MethodPost, "alice", "/repos/models/alice/tiny/rename", gen.RenameRequest{Name: "small"}).json(t, &renamed)
	require.Equal(t, "alice/small", renamed.Id)
	var got gen.Repo
	f.expect(http.StatusOK, http.MethodGet, "alice", "/repos/models/alice/tiny", nil).json(t, &got)
	require.Equal(t, "alice/small", got.Id)
	f.expect(http.StatusForbidden, http.MethodPost, "alice", "/repos/models/alice/small/transfer", gen.TransferRequest{Namespace: "lab"})
	f.expect(http.StatusBadRequest, http.MethodPost, "alice", "/repos/models/alice/small/transfer", gen.TransferRequest{Namespace: "nobody"})
	f.authz.SetRole(f.users["alice"].UserID, labID, model.RoleWrite)
	var moved gen.Repo
	f.expect(http.StatusOK, http.MethodPost, "alice", "/repos/models/alice/small/transfer", gen.TransferRequest{Namespace: "lab"}).json(t, &moved)
	require.Equal(t, "lab/small", moved.Id)

	// Delete and restore.
	f.expect(http.StatusForbidden, http.MethodDelete, "carol", "/repos/models/lab/small", nil)
	f.expect(http.StatusNoContent, http.MethodDelete, "bob", "/repos/models/lab/small", nil)
	f.expect(http.StatusNotFound, http.MethodGet, "bob", "/repos/models/lab/small", nil)
	f.expect(http.StatusNotFound, http.MethodPost, "carol", "/repos/models/lab/small/restore", nil)
	f.expect(http.StatusNotFound, http.MethodPost, "bob", "/repos/models/nowhere/small/restore", nil)
	var restored gen.Repo
	f.expect(http.StatusOK, http.MethodPost, "bob", "/repos/models/lab/small/restore", nil).json(t, &restored)
	require.Nil(t, restored.DeletedAt)
	f.expect(http.StatusNotFound, http.MethodPost, "bob", "/repos/models/lab/small/restore", nil)
}

func TestListAndSearchEndpoints(t *testing.T) {
	f := newFixture(t)
	f.user("alice")
	bob := f.user("bob")
	pub := gen.Visibility("public")
	priv := gen.Visibility("private")
	a := f.createRepo("alice", gen.RepoCreate{Type: "model", Namespace: "alice", Name: "llama-tiny", Visibility: &pub})
	f.createRepo("alice", gen.RepoCreate{Type: "model", Namespace: "alice", Name: "secret-llama", Visibility: &priv})
	f.createRepo("alice", gen.RepoCreate{Type: "dataset", Namespace: "alice", Name: "llama-data", Visibility: &pub})
	f.index.SetSummary(*a.Uuid, model.CommitSHA(*a.Sha), &index.Summary{Formats: []string{"gguf"}, Parameters: &index.Parameters{Total: 7}})
	_, err := f.index.Index(f.ctx, *a.Uuid, model.RepoID{}, model.CommitSHA(*a.Sha))
	require.NoError(t, err)
	_, err = f.scans.ScanCommit(f.ctx, *a.Uuid, model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "llama-tiny"}, model.CommitSHA(*a.Sha))
	require.NoError(t, err)

	var list gen.RepoList
	f.expect(http.StatusOK, http.MethodGet, "bob", "/repos?limit=1&sort=name", nil).json(t, &list)
	require.Len(t, list.Items, 1)
	require.Equal(t, "alice/llama-data", list.Items[0].Id)
	require.NotNil(t, list.NextCursor)
	next := *list.NextCursor
	list = gen.RepoList{}
	f.expect(http.StatusOK, http.MethodGet, "bob", "/repos?limit=1&sort=name&cursor="+next, nil).json(t, &list)
	require.Equal(t, "alice/llama-tiny", list.Items[0].Id)
	require.Equal(t, []string{"gguf"}, *list.Items[0].Summary.Formats)
	require.Equal(t, gen.RepoSummaryScanStatus("safe"), *list.Items[0].Summary.ScanStatus)
	f.expect(http.StatusOK, http.MethodGet, "bob", "/repos?type=datasets&q=LLAMA&ns=alice&mirror=false", nil).json(t, &list)
	require.Len(t, list.Items, 1)
	f.expect(http.StatusOK, http.MethodGet, "alice", "/repos", nil).json(t, &list)
	require.Len(t, list.Items, 3)
	f.expect(http.StatusBadRequest, http.MethodGet, "bob", "/repos?limit=0", nil)
	f.expect(http.StatusBadRequest, http.MethodGet, "bob", "/repos?limit=501", nil)
	f.expect(http.StatusBadRequest, http.MethodGet, "bob", "/repos?params_min=-1", nil)
	for _, p := range []string{"rows_min", "rows_max"} {
		var problem map[string]any
		f.expect(http.StatusBadRequest, http.MethodGet, "bob", "/repos?"+p+"=-1", nil).json(t, &problem)
		require.Contains(t, fmt.Sprint(problem), p+" cannot be negative")
	}
	f.expect(http.StatusBadRequest, http.MethodGet, "bob", "/repos?rows_min=ten", nil)
	f.expect(http.StatusBadRequest, http.MethodGet, "bob", "/repos?cursor=x", nil)
	f.expect(http.StatusBadRequest, http.MethodGet, "bob", "/repos?format=gguf", nil)
	f.expect(http.StatusBadRequest, http.MethodGet, "bob", "/repos?type=spaces", nil)

	_, err = f.colls.Create(f.ctx, f.users["alice"], collections.CreateParams{Namespace: "alice", Title: "Llama picks", Visibility: model.VisibilityPublic})
	require.NoError(t, err)
	var sr gen.SearchResults
	f.expect(http.StatusOK, http.MethodGet, "bob", "/search?q=llama", nil).json(t, &sr)
	require.Len(t, sr.Repos, 2, "the private Repo is left out")
	require.Len(t, sr.Collections, 1)
	require.Empty(t, sr.Namespaces)
	f.expect(http.StatusOK, http.MethodGet, "bob", "/search?q=ali&types=namespaces", nil).json(t, &sr)
	require.Empty(t, sr.Repos)
	require.Equal(t, "alice", sr.Namespaces[0].Name)
	f.expect(http.StatusBadRequest, http.MethodGet, "bob", "/search?q=%20", nil)
	f.expect(http.StatusBadRequest, http.MethodGet, "bob", "/search?q=x&types=users", nil)
	f.expect(http.StatusBadRequest, http.MethodGet, "bob", "/search?q=x&limit=0", nil)

	labID := f.real.AddOrg(t, "lab", map[uuid.UUID]model.Role{bob.UserID: model.RoleRead})
	require.NotZero(t, labID)
	var ns gen.Namespace
	f.expect(http.StatusOK, http.MethodGet, "bob", "/namespaces/alice", nil).json(t, &ns)
	require.Equal(t, 2, *ns.RepoCount, "bob sees alice's public Repos")
	require.Nil(t, ns.MemberCount)
	f.expect(http.StatusOK, http.MethodGet, "alice", "/namespaces/alice", nil).json(t, &ns)
	require.Equal(t, 3, *ns.RepoCount)
	f.expect(http.StatusOK, http.MethodGet, "", "/namespaces/LAB", nil).json(t, &ns)
	require.Equal(t, gen.NamespaceKind("org"), ns.Kind)
	require.Equal(t, 1, *ns.MemberCount)
	f.expect(http.StatusNotFound, http.MethodGet, "", "/namespaces/nobody", nil)
}

func sum256(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func TestUploadCommitAndBrowseEndpoints(t *testing.T) {
	f := newFixture(t)
	f.user("alice")
	f.user("bob")
	f.createRepo("alice", gen.RepoCreate{Type: "model", Namespace: "alice", Name: "up"})
	f.createRepo("alice", gen.RepoCreate{Type: "model", Namespace: "alice", Name: "other"})
	base := "/repos/models/alice/up"

	weights := bytes.Repeat([]byte("w"), 100)
	stored := []byte("already stored")
	_, err := f.real.LFS.Ingest(f.ctx, sum256(stored), int64(len(stored)), bytes.NewReader(stored))
	require.NoError(t, err)
	plan := gen.UploadPlanRequest{Files: []gen.UploadFile{
		{Path: "config.json", Size: 2, Sha256: sum256([]byte("{}"))},
		{Path: "model.safetensors", Size: int64(len(weights)), Sha256: sum256(weights)},
		{Path: "old.bin", Size: int64(len(stored)), Sha256: sum256(stored)},
	}}
	f.expect(http.StatusForbidden, http.MethodPost, "bob", base+"/uploads", plan)
	var p gen.UploadPlan
	f.expect(http.StatusOK, http.MethodPost, "alice", base+"/uploads", plan).json(t, &p)
	require.Equal(t, gen.UploadPlanItemAction("inline"), p.Files[0].Action)
	require.Equal(t, gen.UploadPlanItemAction("upload"), p.Files[1].Action)
	require.Equal(t, gen.UploadPlanItemAction("exists"), p.Files[2].Action)
	up := p.Files[1]
	require.False(t, *up.Presigned)
	require.Len(t, *up.PartUrls, 1)
	partURL := (*up.PartUrls)[0]
	require.True(t, strings.HasPrefix(partURL, f.srv.URL+"/api/hangar/v1/repos/models/alice/up/uploads/"))

	for _, bad := range []gen.UploadPlanRequest{
		{Files: []gen.UploadFile{{Path: "../x", Size: 1, Sha256: sum256(nil)}}},
		{Files: []gen.UploadFile{{Path: "x", Size: 1, Sha256: "nope"}}},
		{Files: make([]gen.UploadFile, 1001)},
	} {
		f.expect(http.StatusBadRequest, http.MethodPost, "alice", base+"/uploads", bad)
	}

	uploadPath := base + "/uploads/" + up.UploadId.String()
	f.expect(http.StatusNotFound, http.MethodGet, "alice", "/repos/models/alice/other/uploads/"+up.UploadId.String(), nil)
	f.expect(http.StatusNotFound, http.MethodGet, "alice", base+"/uploads/"+uuid.NewString(), nil)
	r := f.expect(http.StatusOK, http.MethodPut, "alice", partURL, weights)
	var part gen.UploadedPart
	r.json(t, &part)
	require.Equal(t, `"`+strings.Trim(part.Etag, `"`)+`"`, r.header.Get("ETag"))
	var status gen.UploadStatus
	f.expect(http.StatusOK, http.MethodGet, "alice", uploadPath, nil).json(t, &status)
	require.Equal(t, gen.UploadStatusState("uploading"), status.State)
	require.Len(t, status.Parts, 1)
	// Planning again, as the UI does after a reload, continues this upload
	// with its part instead of starting another (requests W8 item 9).
	var again gen.UploadPlan
	f.expect(http.StatusOK, http.MethodPost, "alice", base+"/uploads", plan).json(t, &again)
	require.Equal(t, *up.UploadId, *again.Files[1].UploadId)
	f.expect(http.StatusBadRequest, http.MethodPut, "alice", uploadPath+"/parts/1", []byte("short"))
	f.expect(http.StatusOK, http.MethodPost, "alice", uploadPath+"/complete", gen.UploadComplete{Parts: []gen.UploadedPart{part}}).json(t, &status)
	require.Equal(t, gen.UploadStatusState("verified"), status.State)

	commit := gen.CommitRequest{Summary: "Add model", Operations: []gen.CommitOperation{
		{Op: "add", Path: "config.json", ContentBase64: ptr(base64.StdEncoding.EncodeToString([]byte("{}")))},
		{Op: "add_lfs", Path: "model.safetensors", Sha256: ptr(sum256(weights)), Size: i64(int64(len(weights)))},
		{Op: "add_lfs", Path: "old.bin", Sha256: ptr(sum256(stored)), Size: i64(int64(len(stored)))},
		{Op: "add", Path: "dir/sub/notes.md", ContentBase64: ptr(base64.StdEncoding.EncodeToString([]byte("notes")))},
	}}
	f.expect(http.StatusForbidden, http.MethodPost, "bob", base+"/commits", commit)
	var cr gen.CommitResponse
	f.expect(http.StatusCreated, http.MethodPost, "alice", base+"/commits", commit).json(t, &cr)
	require.Equal(t, "refs/heads/main", cr.Ref)
	var noop gen.CommitResponse
	f.expect(http.StatusCreated, http.MethodPost, "alice", base+"/commits", gen.CommitRequest{Summary: "again", Operations: commit.Operations[:1]}).json(t, &noop)
	require.True(t, *noop.NoOp)
	require.Equal(t, cr.Sha, noop.Sha)
	f.expect(http.StatusPreconditionFailed, http.MethodPost, "alice", base+"/commits", gen.CommitRequest{Summary: "s", ParentCommit: ptr(strings.Repeat("0", 40)),
		Operations: []gen.CommitOperation{{Op: "add", Path: "x", ContentBase64: ptr("eA==")}}})
	f.expect(http.StatusBadRequest, http.MethodPost, "alice", base+"/commits", gen.CommitRequest{Summary: "s", Operations: []gen.CommitOperation{{Op: "add", Path: "x", ContentBase64: ptr("!!")}}})
	f.expect(http.StatusBadRequest, http.MethodPost, "alice", base+"/commits", gen.CommitRequest{Summary: "s", Operations: []gen.CommitOperation{{Op: "rename", Path: "x"}}})
	f.expect(http.StatusUnprocessableEntity, http.MethodPost, "alice", base+"/commits", gen.CommitRequest{Summary: "s",
		Operations: []gen.CommitOperation{{Op: "add_lfs", Path: "x.bin", Sha256: ptr(sum256([]byte("missing"))), Size: i64(7)}}})

	// A pull request from a reader.
	f.authz.AnonymousRead = true
	var prc gen.CommitResponse
	f.expect(http.StatusCreated, http.MethodPost, "bob", base+"/commits", gen.CommitRequest{Summary: "Suggest", CreatePr: ptrBool(true),
		Operations: []gen.CommitOperation{{Op: "copy", Path: "copy.json", SrcPath: ptr("config.json")}}}).json(t, &prc)
	require.Equal(t, "refs/pr/1", prc.Ref)
	require.Equal(t, 1, *prc.PullRequest)

	// Browse what was committed.
	var refs gen.Refs
	f.expect(http.StatusOK, http.MethodGet, "bob", base+"/refs", nil).json(t, &refs)
	require.Equal(t, "main", refs.Branches[0].Name)
	require.Equal(t, cr.Sha, refs.Branches[0].TargetCommit)
	require.Equal(t, "refs/pr/1", refs.PullRequests[0].Ref)

	var tree gen.TreeListing
	f.expect(http.StatusOK, http.MethodGet, "bob", base+"/tree/main", nil).json(t, &tree)
	require.Equal(t, cr.Sha, tree.Commit)
	names := []string{}
	for _, e := range tree.Items {
		names = append(names, e.Path)
		require.NotNil(t, e.LastCommit)
	}
	require.Equal(t, []string{"dir", ".gitattributes", "config.json", "model.safetensors", "old.bin"}, names)
	require.Equal(t, int64(len(weights)), tree.Items[3].Size)
	require.Equal(t, sum256(weights), tree.Items[3].Lfs.Oid)
	require.True(t, tree.Items[3].Present)
	f.expect(http.StatusOK, http.MethodGet, "bob", base+"/tree/main?limit=2", nil).json(t, &tree)
	require.Len(t, tree.Items, 2)
	f.expect(http.StatusOK, http.MethodGet, "bob", base+"/tree/main?limit=2&cursor="+url.QueryEscape(*tree.NextCursor), nil).json(t, &tree)
	require.Equal(t, "config.json", tree.Items[0].Path)
	f.expect(http.StatusOK, http.MethodGet, "bob", base+"/tree/refs%2Fpr%2F1/dir%2Fsub?recursive=true", nil).json(t, &tree)
	require.Equal(t, "dir/sub/notes.md", tree.Items[0].Path)
	require.Equal(t, prc.Sha, tree.Commit)
	f.expect(http.StatusOK, http.MethodGet, "bob", base+"/tree/main/dir%2Fsub%2Fnotes.md", nil).json(t, &tree)
	require.Len(t, tree.Items, 1)
	require.Equal(t, gen.TreeEntryType("file"), tree.Items[0].Type)
	f.expect(http.StatusNotFound, http.MethodGet, "bob", base+"/tree/main/missing", nil)
	f.expect(http.StatusNotFound, http.MethodGet, "bob", base+"/tree/nope", nil)
	f.expect(http.StatusBadRequest, http.MethodGet, "bob", base+"/tree/main?limit=900", nil)

	var commits gen.CommitList
	f.expect(http.StatusOK, http.MethodGet, "bob", base+"/commits?limit=1", nil).json(t, &commits)
	require.Equal(t, cr.Sha, commits.Items[0].Sha)
	require.Equal(t, "Add model", commits.Items[0].Title)
	var last gen.CommitList
	f.expect(http.StatusOK, http.MethodGet, "bob", base+"/commits?limit=1&cursor="+*commits.NextCursor, nil).json(t, &last)
	require.Equal(t, "initial commit", last.Items[0].Title)
	require.Nil(t, last.NextCursor)
	f.expect(http.StatusOK, http.MethodGet, "bob", base+"/commits?rev=refs%2Fpr%2F1&path=copy.json", nil).json(t, &commits)
	require.Len(t, commits.Items, 1)
	f.expect(http.StatusBadRequest, http.MethodGet, "bob", base+"/commits?cursor=-1", nil)

	var detail gen.CommitDetail
	f.expect(http.StatusOK, http.MethodGet, "bob", base+"/commits/"+cr.Sha, nil).json(t, &detail)
	require.Len(t, detail.Changes, 4)
	var fd gen.FileDiff
	f.expect(http.StatusOK, http.MethodGet, "bob", base+"/commits/"+cr.Sha+"/diff?path=dir%2Fsub%2Fnotes.md", nil).json(t, &fd)
	require.Contains(t, *fd.Patch, "+notes")
	var lfsDiff gen.FileDiff
	f.expect(http.StatusOK, http.MethodGet, "bob", base+"/commits/"+cr.Sha+"/diff?path=model.safetensors", nil).json(t, &lfsDiff)
	require.True(t, lfsDiff.Lfs)
	require.Nil(t, lfsDiff.Patch)
	f.expect(http.StatusNotFound, http.MethodGet, "bob", base+"/commits/"+strings.Repeat("a", 40), nil)

	// A zip of the Revision, downloaded from its signed URL.
	var job gen.Job
	f.expect(http.StatusAccepted, http.MethodPost, "bob", base+"/zip?path=dir", nil).json(t, &job)
	require.Equal(t, "zip.export", job.Kind)
	f.expect(http.StatusBadRequest, http.MethodPost, "bob", base+"/zip?path=config.json", nil)
	f.expect(http.StatusNotFound, http.MethodPost, "bob", base+"/zip?rev=nope", nil)
	f.jobs.RunPending(f.ctx)
	j, err := f.jobs.Get(f.ctx, job.Id)
	require.NoError(t, err)
	require.Equal(t, jobs.StatusSucceeded, j.Status, j.Error)
	var zr repo.ZipResult
	require.NoError(t, json.Unmarshal(j.Result, &zr))
	res := f.expect(http.StatusOK, http.MethodGet, "", zr.URL, nil)
	require.Equal(t, "application/zip", res.header.Get("Content-Type"))
	require.Contains(t, res.header.Get("Content-Disposition"), zr.Filename)
	zrd, err := zip.NewReader(bytes.NewReader(res.body), int64(len(res.body)))
	require.NoError(t, err)
	require.Equal(t, "up/sub/notes.md", zrd.File[0].Name)
	f.expect(http.StatusForbidden, http.MethodGet, "", strings.Replace(zr.URL, "sig=", "sig=x", 1), nil)
	f.expect(http.StatusMethodNotAllowed, http.MethodPost, "", zr.URL, nil)
	require.NoError(t, f.real.Blobs.Delete(f.ctx, zr.Key))
	f.expect(http.StatusNotFound, http.MethodGet, "", zr.URL, nil)
	exp := api.ExportURL(f.srv.URL, f.real.Blobs, f.signer)
	u, err := exp(f.ctx, "exports/x.zip", "x.zip", time.Minute)
	require.NoError(t, err)
	require.Contains(t, u, api.ExportPath+"x.zip")
}

func ptr(s string) *string { return &s }
func i64(n int64) *int64   { return &n }
func ptrBool(b bool) *bool { return &b }

func TestRepoMirrorStatus(t *testing.T) {
	var asked []string
	f := newFixture(t, func(d *api.RepoDeps) {
		d.MirrorStatus = func(_ context.Context, r *repo.Repo) (*gen.MirrorSummary, error) {
			asked = append(asked, r.ID.String())
			return &gen.MirrorSummary{Upstream: "huggingface", UpstreamRepo: r.ID.String(), Status: "ok"}, nil
		}
	})
	f.user("alice")
	f.createRepo("alice", gen.RepoCreate{Type: "model", Namespace: "alice", Name: "local"})
	mirrored := f.createRepo("alice", gen.RepoCreate{Type: "model", Namespace: "alice", Name: "mirrored"})
	require.NoError(t, f.real.SetMirrorState(f.ctx, model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "mirrored"}, true))

	var got gen.Repo
	f.expect(http.StatusOK, http.MethodGet, "alice", "/repos/models/alice/local", nil).json(t, &got)
	require.Nil(t, got.MirrorStatus, "an ordinary Repo has no mirror_status")
	require.Empty(t, asked)
	f.expect(http.StatusOK, http.MethodGet, "alice", "/repos/models/"+mirrored.Id, nil).json(t, &got)
	require.NotNil(t, got.MirrorStatus)
	require.Equal(t, "huggingface", got.MirrorStatus.Upstream)
	require.Equal(t, []string{"alice/mirrored"}, asked)
}

// TestClientClosedRequestIsNotAServerError checks that a request the
// client closed, whose git or database work then stops with
// context.Canceled, is answered with 499 and not logged as an error. Under
// load, e2e clients gave up on slow requests and the server logged each one
// as a failed request with status 500.
func TestClientClosedRequestIsNotAServerError(t *testing.T) {
	var logs bytes.Buffer
	var deps api.RepoDeps
	f := newFixture(t, func(d *api.RepoDeps) {
		d.Log = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
		deps = *d
	})
	alice := f.user("alice")
	f.createRepo("alice", gen.RepoCreate{Type: "model", Namespace: "alice", Name: "tiny"})
	h, err := api.NewReposAPI(deps)
	require.NoError(t, err)
	asAlice := func(id string) context.Context {
		ctx := httpx.WithInfo(context.Background(), id)
		httpx.SetPrincipal(ctx, alice)
		return ctx
	}

	ctx, cancel := context.WithCancel(asAlice("req-1"))
	cancel()
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/hangar/v1/repos/model/alice/tiny", nil)
	rec := httptest.NewRecorder()
	h.GetRepo(rec, req, gen.Models, "alice", "tiny")
	require.Equal(t, 499, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "The client closed the request.")
	require.NotContains(t, logs.String(), "level=ERROR")

	// A failure while the client is still there is still a server error.
	req = httptest.NewRequestWithContext(asAlice("req-2"), http.MethodGet, "/", nil)
	rec = httptest.NewRecorder()
	h.GetRepo(rec, req, gen.Models, "alice", "tiny")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}
