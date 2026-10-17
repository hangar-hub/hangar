package api_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/api"
	"github.com/jamesagarside/hangar/internal/api/gen"
	"github.com/jamesagarside/hangar/internal/audit"
	"github.com/jamesagarside/hangar/internal/audit/audittest"
	"github.com/jamesagarside/hangar/internal/auth/authtest"
	"github.com/jamesagarside/hangar/internal/blob"
	"github.com/jamesagarside/hangar/internal/blob/blobtest"
	blobfs "github.com/jamesagarside/hangar/internal/blob/fs"
	"github.com/jamesagarside/hangar/internal/config"
	"github.com/jamesagarside/hangar/internal/db"
	"github.com/jamesagarside/hangar/internal/db/dbtest"
	"github.com/jamesagarside/hangar/internal/gc"
	"github.com/jamesagarside/hangar/internal/httpx"
	"github.com/jamesagarside/hangar/internal/jobs"
	"github.com/jamesagarside/hangar/internal/model"
)

// unimplemented is embedded one level down so the handlers' methods win.
type unimplemented struct{ gen.Unimplemented }

type platformServer struct {
	unimplemented
	*api.Jobs
	*api.AdminStorage
	*api.AdminSystem
}

type platformEnv struct {
	t       *testing.T
	db      *db.DB
	queue   *jobs.DBQueue
	audit   *audittest.Fake
	store   blob.Store
	handler http.Handler
	users   map[string]model.Principal
	authz   *authtest.Authz
	repos   jobRepos
}

// testCheckTimeout is the budget for doctor checks that do real work. The
// production default, 10 seconds, is not enough in this package: its tests
// run in parallel and start git many times, and on a loaded machine
// starting git --version took over 10 seconds while other tests forked.
const testCheckTimeout = time.Minute

func newPlatformEnv(t *testing.T, checks func(d *db.DB, s blob.Store) []api.SystemCheck) *platformEnv {
	t.Helper()
	d := dbtest.SQLite(t)
	store, err := blobfs.New(filepath.Join(t.TempDir(), "blobs"))
	require.NoError(t, err)
	e := &platformEnv{
		t: t, db: d, queue: jobs.NewQueue(d, nil, jobs.QueueOptions{}), audit: audittest.NewFake(), store: store,
		users: map[string]model.Principal{}, authz: authtest.NewAuthz(), repos: jobRepos{},
	}
	for _, name := range []string{"alice", "bob", "root"} {
		e.users[name] = e.newUser(name, name == "root")
	}
	readOnly := e.users["alice"]
	readOnly.Method = model.AuthToken
	readOnly.Token = &model.TokenInfo{ID: uuid.New(), Scope: model.ScopeRead}
	e.users["alice-read"] = readOnly
	rootRead := e.users["root"]
	rootRead.Method = model.AuthToken
	rootRead.Token = &model.TokenInfo{ID: uuid.New(), Scope: model.ScopeRead}
	e.users["root-read"] = rootRead

	if checks == nil {
		checks = func(d *db.DB, s blob.Store) []api.SystemCheck {
			return api.DoctorChecks(d, api.DoctorOptions{Store: s, Downloads: blob.Downloads{Store: s, Mode: blob.DownloadProxy}})
		}
	}
	srv := platformServer{
		Jobs:         &api.Jobs{Queue: e.queue, Authz: e.authz, Repos: e.repos},
		AdminStorage: &api.AdminStorage{Authz: authtest.NewAuthz(), Accounting: gc.NewAccounting(d), Queue: e.queue, Audit: e.audit},
		AdminSystem: &api.AdminSystem{Authz: authtest.NewAuthz(), DB: d, Checks: checks(d, store), CheckTimeout: testCheckTimeout, Info: api.SystemInfo{
			Version: "1.2.3", Commit: "abc123", Role: "all", BlobBackend: "fs", Telemetry: map[string]string{"traces": "none"},
		}},
	}
	h := gen.HandlerWithOptions(srv, gen.ChiServerOptions{BaseURL: "/api/hangar/v1"})
	e.handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := httpx.WithInfo(r.Context(), "req-1")
		if p, ok := e.users[r.Header.Get("X-Test-User")]; ok {
			httpx.SetPrincipal(ctx, p)
		}
		h.ServeHTTP(w, r.WithContext(ctx))
	})
	return e
}

func (e *platformEnv) newUser(name string, admin bool) model.Principal {
	ctx := context.Background()
	ns := &db.Namespace{ID: db.NewID(), Name: name, Kind: "user"}
	_, err := e.db.NewInsert().Model(ns).Exec(ctx)
	require.NoError(e.t, err)
	u := &db.User{ID: db.NewID(), NamespaceID: ns.ID, IsAdmin: admin}
	_, err = e.db.NewInsert().Model(u).Exec(ctx)
	require.NoError(e.t, err)
	return model.Principal{Kind: model.PrincipalUser, UserID: u.ID, NamespaceID: ns.ID, Username: name, IsAdmin: admin, Method: model.AuthSession}
}

func (e *platformEnv) do(user, method, path string, body any) (int, []byte) {
	e.t.Helper()
	var r io.Reader
	if body != nil {
		if s, ok := body.(string); ok {
			r = strings.NewReader(s)
		} else {
			b, err := json.Marshal(body)
			require.NoError(e.t, err)
			r = bytes.NewReader(b)
		}
	}
	req := httptest.NewRequest(method, "/api/hangar/v1"+path, r)
	req.Header.Set("Content-Type", "application/json")
	if user != "" {
		req.Header.Set("X-Test-User", user)
	}
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

func decode[T any](t *testing.T, b []byte) T {
	t.Helper()
	var v T
	require.NoError(t, json.Unmarshal(b, &v), string(b))
	return v
}

func (e *platformEnv) enqueue(user string, nj jobs.NewJob) uuid.UUID {
	if user != "" {
		nj.CreatedBy = e.users[user].UserID
	}
	id, err := e.queue.Enqueue(context.Background(), nj)
	require.NoError(e.t, err)
	return id
}

func TestJobsEndpoints(t *testing.T) {
	t.Parallel()
	e := newPlatformEnv(t, nil)
	ctx := context.Background()
	alicePull := e.enqueue("alice", jobs.NewJob{Kind: jobs.KindPull, Payload: map[string]string{"repo": "a/b"}})
	aliceFile := e.enqueue("alice", jobs.NewJob{Kind: jobs.KindPullFile, ParentID: alicePull})
	bobPull := e.enqueue("bob", jobs.NewJob{Kind: jobs.KindPull})
	system := e.enqueue("", jobs.NewJob{Kind: jobs.KindGC})
	// Progress as a worker reports it.
	_, err := e.db.NewUpdate().Model((*db.Job)(nil)).Set("progress = ?", json.RawMessage(`{"bytes_done":25,"bytes_total":125,"rate":10,"message":"Pulling","files":[{"path":"model.gguf","bytes_done":25,"bytes_total":125,"status":"running"}]}`)).
		Where("id = ?", alicePull).Exec(ctx)
	require.NoError(t, err)

	code, _ := e.do("", http.MethodGet, "/jobs", nil)
	require.Equal(t, http.StatusUnauthorized, code)

	code, body := e.do("alice", http.MethodGet, "/jobs", nil)
	require.Equal(t, http.StatusOK, code)
	list := decode[gen.JobList](t, body)
	require.Len(t, list.Items, 2, "users see their own Jobs")
	require.Equal(t, aliceFile, list.Items[0].Id)

	code, body = e.do("root", http.MethodGet, "/jobs", nil)
	require.Equal(t, http.StatusOK, code)
	require.Len(t, decode[gen.JobList](t, body).Items, 4, "admins see every Job")
	code, body = e.do("root", http.MethodGet, "/jobs?mine=true", nil)
	require.Equal(t, http.StatusOK, code)
	require.Empty(t, decode[gen.JobList](t, body).Items)

	code, body = e.do("root", http.MethodGet, "/jobs?kind=pull&status=pending&limit=1", nil)
	require.Equal(t, http.StatusOK, code)
	page := decode[gen.JobList](t, body)
	require.Len(t, page.Items, 1)
	require.Equal(t, bobPull, page.Items[0].Id)
	require.NotNil(t, page.NextCursor)
	code, body = e.do("root", http.MethodGet, "/jobs?kind=pull&limit=1&cursor="+*page.NextCursor, nil)
	require.Equal(t, http.StatusOK, code)
	page = decode[gen.JobList](t, body)
	require.Equal(t, alicePull, page.Items[0].Id)

	for _, q := range []string{"?kind=bake", "?limit=0", "?limit=501", "?cursor=zzz", "?status=done"} {
		code, _ = e.do("root", http.MethodGet, "/jobs"+q, nil)
		require.Equal(t, http.StatusBadRequest, code, q)
	}

	code, body = e.do("alice", http.MethodGet, "/jobs/"+alicePull.String(), nil)
	require.Equal(t, http.StatusOK, code)
	j := decode[gen.Job](t, body)
	require.Equal(t, gen.JobStatusPending, j.Status)
	require.Equal(t, int64(25), j.Progress.BytesDone)
	require.Equal(t, 10, *j.Progress.EtaSeconds)
	require.Equal(t, "Pulling", *j.Progress.Message)
	require.Equal(t, "model.gguf", (*j.Progress.Files)[0].Path)
	require.Equal(t, "a/b", (*j.Payload)["repo"])
	require.Equal(t, e.users["alice"].UserID.String(), *j.CreatedBy)

	for _, id := range []uuid.UUID{bobPull, system, uuid.New()} {
		code, _ = e.do("alice", http.MethodGet, "/jobs/"+id.String(), nil)
		require.Equal(t, http.StatusNotFound, code, "other users' Jobs and system Jobs are hidden")
	}

	code, _ = e.do("alice-read", http.MethodPost, "/jobs/"+alicePull.String()+"/cancel", nil)
	require.Equal(t, http.StatusForbidden, code, "a read token cannot cancel")
	code, _ = e.do("bob", http.MethodPost, "/jobs/"+alicePull.String()+"/cancel", nil)
	require.Equal(t, http.StatusNotFound, code)
	code, body = e.do("alice", http.MethodPost, "/jobs/"+alicePull.String()+"/cancel", nil)
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, gen.JobStatusCancelled, decode[gen.Job](t, body).Status)
	code, _ = e.do("alice", http.MethodGet, "/jobs/"+aliceFile.String(), nil)
	require.Equal(t, http.StatusOK, code)
	code, _ = e.do("alice", http.MethodPost, "/jobs/"+alicePull.String()+"/cancel", nil)
	require.Equal(t, http.StatusConflict, code)

	code, body = e.do("alice", http.MethodPost, "/jobs/"+alicePull.String()+"/retry", nil)
	require.Equal(t, http.StatusAccepted, code)
	retried := decode[gen.Job](t, body)
	require.NotEqual(t, alicePull, retried.Id)
	require.Equal(t, gen.JobStatusPending, retried.Status)
	code, _ = e.do("alice", http.MethodPost, "/jobs/"+retried.Id.String()+"/retry", nil)
	require.Equal(t, http.StatusConflict, code, "only failed or cancelled Jobs can be retried")
	code, _ = e.do("root", http.MethodPost, "/jobs/"+system.String()+"/cancel", nil)
	require.Equal(t, http.StatusOK, code, "admins can cancel any Job")
}

func TestAdminStorageEndpoints(t *testing.T) {
	t.Parallel()
	e := newPlatformEnv(t, nil)
	ctx := context.Background()
	// A Repo with one LFS object, as the repo package would record it.
	nsID := e.users["alice"].NamespaceID
	repo := &db.Repo{ID: db.NewID(), NamespaceID: nsID, Name: "m", Type: "model", Visibility: "public", DefaultBranch: "main",
		HeadRef: "refs/heads/main", Tags: []string{}, Settings: map[string]any{}, Gating: map[string]any{}}
	_, err := e.db.NewInsert().Model(repo).Exec(ctx)
	require.NoError(t, err)
	oid := strings.Repeat("ab", 32)
	_, err = e.db.NewInsert().Model(&db.LFSObject{OID: oid, Size: 4096, StorageKey: model.LFSObjectKey(oid), LastReferencedAt: db.Now()}).Exec(ctx)
	require.NoError(t, err)
	_, err = e.db.NewInsert().Model(&db.RepoLFS{RepoID: repo.ID, OID: oid, Size: 4096, Present: true}).Exec(ctx)
	require.NoError(t, err)

	code, _ := e.do("", http.MethodGet, "/admin/storage", nil)
	require.Equal(t, http.StatusUnauthorized, code)
	code, _ = e.do("alice", http.MethodGet, "/admin/storage", nil)
	require.Equal(t, http.StatusForbidden, code)
	code, _ = e.do("root-read", http.MethodGet, "/admin/storage", nil)
	require.Equal(t, http.StatusForbidden, code, "admin endpoints need an admin-scope token")

	code, body := e.do("root", http.MethodGet, "/admin/storage", nil)
	require.Equal(t, http.StatusOK, code)
	u := decode[gen.StorageUsage](t, body)
	require.Equal(t, int64(4096), u.TotalLfsBytes)
	require.Len(t, u.Namespaces, 1)
	require.Equal(t, "alice", u.Namespaces[0].Namespace)
	require.Equal(t, 1, u.Namespaces[0].RepoCount)
	require.Nil(t, u.Namespaces[0].QuotaBytes)
	require.Nil(t, u.LastGcAt)

	code, body = e.do("root", http.MethodPut, "/admin/quotas/ALICE", gen.Quota{BytesLimit: 1 << 30})
	require.Equal(t, http.StatusOK, code, string(body))
	require.Equal(t, int64(1<<30), decode[gen.Quota](t, body).BytesLimit)
	require.Equal(t, []audit.Action{audit.QuotaChanged}, e.audit.Actions())
	ev := e.audit.Events()[0]
	require.Equal(t, "alice", ev.TargetID)
	require.Equal(t, int64(1<<30), ev.Data["bytes_limit"])
	code, body = e.do("root", http.MethodGet, "/admin/storage", nil)
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, int64(1<<30), *decode[gen.StorageUsage](t, body).Namespaces[0].QuotaBytes)

	code, _ = e.do("root", http.MethodPut, "/admin/quotas/nobody", gen.Quota{BytesLimit: 1})
	require.Equal(t, http.StatusNotFound, code)
	code, _ = e.do("root", http.MethodPut, "/admin/quotas/alice", gen.Quota{BytesLimit: -1})
	require.Equal(t, http.StatusBadRequest, code)
	code, _ = e.do("root", http.MethodPut, "/admin/quotas/alice", `{"limit": 5}`)
	require.Equal(t, http.StatusBadRequest, code)
	code, _ = e.do("alice", http.MethodPut, "/admin/quotas/alice", gen.Quota{BytesLimit: 0})
	require.Equal(t, http.StatusForbidden, code)
	code, _ = e.do("root", http.MethodPut, "/admin/quotas/alice", gen.Quota{BytesLimit: 0})
	require.Equal(t, http.StatusOK, code)
	q, err := gc.NewAccounting(e.db).Quota(ctx, nsID)
	require.NoError(t, err)
	require.Zero(t, q, "0 removes the quota")

	code, body = e.do("root", http.MethodPost, "/admin/gc", nil)
	require.Equal(t, http.StatusAccepted, code)
	first := decode[gen.Job](t, body)
	require.Equal(t, "gc", first.Kind)
	code, body = e.do("root", http.MethodPost, "/admin/gc", nil)
	require.Equal(t, http.StatusAccepted, code)
	require.Equal(t, first.Id, decode[gen.Job](t, body).Id, "a pending run is joined")
	code, _ = e.do("alice", http.MethodPost, "/admin/gc", nil)
	require.Equal(t, http.StatusForbidden, code)

	// Run it, then the usage report says when GC last ran.
	reg := jobs.NewRegistry()
	c := gc.New(e.db, e.store, gc.Options{})
	require.NoError(t, reg.Register(jobs.KindGC, c.Handler(), jobs.HandlerOptions{}))
	w := jobs.NewWorker(e.db, reg, jobs.WorkerOptions{})
	w.Tick(ctx)
	w.Wait()
	code, body = e.do("root", http.MethodGet, "/admin/storage", nil)
	require.Equal(t, http.StatusOK, code)
	require.NotNil(t, decode[gen.StorageUsage](t, body).LastGcAt)
}

func TestAdminSystem(t *testing.T) {
	t.Parallel()
	e := newPlatformEnv(t, nil)
	code, _ := e.do("alice", http.MethodGet, "/admin/system", nil)
	require.Equal(t, http.StatusForbidden, code)
	code, body := e.do("root", http.MethodGet, "/admin/system", nil)
	require.Equal(t, http.StatusOK, code)
	s := decode[gen.SystemInfo](t, body)
	require.Equal(t, "1.2.3", s.Version)
	require.Equal(t, "abc123", *s.Commit)
	require.Equal(t, "all", s.Role)
	require.Equal(t, "sqlite", s.Database.Driver)
	require.NotEmpty(t, s.Database.SchemaVersion)
	require.Equal(t, "fs", s.BlobStore.Backend)
	require.Nil(t, s.BlobStore.DownloadMode, "the filesystem has no download mode")
	require.Nil(t, s.BlobStore.UploadMode, "the filesystem has no upload mode")
	require.Equal(t, "none", s.Telemetry["traces"])
	names := map[string]gen.CheckResult{}
	for _, c := range s.Checks {
		names[*c.Name] = c
		require.True(t, c.Ok, "%s: %s", *c.Name, c.Message)
		require.NotNil(t, c.LatencyMs)
	}
	for _, n := range []string{"database", "schema", "jobs", "clock", "blob_store", "presigned_downloads", "git"} {
		require.Contains(t, names, n)
	}
	require.Equal(t, "Downloads stream through Hangar (download_mode is proxy).", names["presigned_downloads"].Message)
	require.Contains(t, names["git"].Message, "is installed")
	require.Equal(t, "This host's clock agrees with the Blob store to within 10s.", names["clock"].Message)
}

func TestDoctorClockCheck(t *testing.T) {
	t.Parallel()
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		ctx := context.Background()
		run := func(opts api.DoctorOptions) gen.CheckResult {
			t.Helper()
			opts.GitBinary = "-"
			for _, r := range api.RunChecks(ctx, api.DoctorChecks(d, opts), testCheckTimeout) {
				if *r.Name == "clock" {
					return r
				}
			}
			t.Fatal("no clock check")
			return gen.CheckResult{}
		}
		store := blobtest.NewFake()
		r := run(api.DoctorOptions{Store: store})
		require.True(t, r.Ok, r.Message)
		require.Empty(t, store.Keys(), "the check removes its object")
		if d.IsSQLite() {
			require.Equal(t, "This host's clock agrees with the Blob store to within 10s.", r.Message)
			r = run(api.DoctorOptions{})
			require.True(t, r.Ok, r.Message)
			require.Equal(t, "SQLite uses this host's clock, and there is no Blob store to compare with.", r.Message)
		} else {
			require.Equal(t, "This host's clock agrees with the database and the Blob store to within 10s.", r.Message)
		}

		// This host's clock is two minutes fast. The check reads the other
		// clock some time after it reads this host's, so it reports 2
		// minutes less that delay, rounded to a second: 2m0s unless the
		// delay passes half a second, as it can on a loaded machine.
		fast := func() time.Time { return time.Now().Add(2 * time.Minute) }
		start := time.Now()
		r = run(api.DoctorOptions{Store: store, Now: fast})
		least := (2*time.Minute - time.Since(start)).Round(time.Second)
		require.False(t, r.Ok)
		requireBehind(t, r.Message, "the Blob store's clock is ", least, 2*time.Minute)
		require.Contains(t, r.Message, "behind this host's clock")
		require.Contains(t, r.Message, "run NTP on every host")
		if !d.IsSQLite() {
			requireBehind(t, r.Message, "the database's clock is ", least, 2*time.Minute)
		}
		slow := func() time.Time { return time.Now().Add(-time.Minute) }
		r = run(api.DoctorOptions{Store: store, Now: slow, MaxClockSkew: 30 * time.Second})
		require.False(t, r.Ok)
		require.Contains(t, r.Message, "ahead of this host's clock")
		r = run(api.DoctorOptions{Store: store, Now: slow, MaxClockSkew: 2 * time.Minute})
		require.True(t, r.Ok, r.Message)
	})
}

// requireBehind checks that msg says, after prefix, that a clock is
// between least and most behind.
func requireBehind(t *testing.T, msg, prefix string, least, most time.Duration) {
	t.Helper()
	m := regexp.MustCompile(regexp.QuoteMeta(prefix) + `(\S+) behind`).FindStringSubmatch(msg)
	require.NotNil(t, m, msg)
	got, err := time.ParseDuration(m[1])
	require.NoError(t, err, msg)
	require.GreaterOrEqual(t, got, least, msg)
	require.LessOrEqual(t, got, most, msg)
}

func TestDoctorConfigCheck(t *testing.T) {
	t.Parallel()
	d := dbtest.SQLite(t)
	cfg := config.Default()
	results := api.RunChecks(context.Background(), api.DoctorChecks(d, api.DoctorOptions{Config: &cfg, GitBinary: "-"}), testCheckTimeout)
	require.Equal(t, "config", *results[0].Name, "the config check runs first")
	require.False(t, results[0].Ok)
	require.Contains(t, results[0].Message, "server.public_url is required")

	cfg.Server.PublicURL = "https://hangar.example.com"
	cfg.Server.DataDir = t.TempDir()
	cfg.Security.InstanceKey = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	results = api.RunChecks(context.Background(), api.DoctorChecks(d, api.DoctorOptions{Config: &cfg, GitBinary: "-"}), testCheckTimeout)
	require.True(t, results[0].Ok, results[0].Message)
	require.Equal(t, "The configuration is valid.", results[0].Message)
}

func TestDoctorChecksReportProblems(t *testing.T) {
	t.Parallel()
	d := dbtest.SQLite(t)
	ctx := context.Background()
	// An overdue Job and no worker.
	_, err := jobs.NewQueue(d, nil, jobs.QueueOptions{Now: func() time.Time { return time.Now().Add(-time.Hour) }}).Enqueue(ctx, jobs.NewJob{Kind: jobs.KindIndex})
	require.NoError(t, err)
	fake := blobtest.NewFake()
	fake.PresignBaseURL = "http://127.0.0.1:1/bucket"
	checks := api.DoctorChecks(d, api.DoctorOptions{Store: fake, Downloads: blob.Downloads{Mode: blob.DownloadRedirect}, GitBinary: filepath.Join(t.TempDir(), "no-git")})
	results := api.RunChecks(ctx, checks, testCheckTimeout)
	// A check that runs past the budget, and one that panics. They run on
	// their own so the budget can be short without cutting off the real
	// checks above.
	results = append(results, api.RunChecks(ctx, []api.SystemCheck{
		{Name: "slow", Run: func(ctx context.Context) (string, error) { <-ctx.Done(); return "", ctx.Err() }},
		{Name: "broken", Run: func(context.Context) (string, error) { panic("oops") }},
	}, 200*time.Millisecond)...)
	byName := map[string]gen.CheckResult{}
	for _, r := range results {
		byName[*r.Name] = r
	}
	require.True(t, byName["database"].Ok)
	require.False(t, byName["jobs"].Ok)
	require.Contains(t, byName["jobs"].Message, "Check that a worker process")
	require.True(t, byName["blob_store"].Ok)
	require.False(t, byName["presigned_downloads"].Ok)
	require.NotContains(t, byName["presigned_downloads"].Message, "X-Expires", "presigned URLs never appear in messages")
	require.NotContains(t, byName["presigned_downloads"].Message, "127.0.0.1:1/bucket")
	require.False(t, byName["git"].Ok)
	require.Contains(t, byName["git"].Message, "Install git 2.39 or later")
	require.False(t, byName["slow"].Ok)
	require.Equal(t, "The check did not finish within 200ms.", byName["slow"].Message)
	require.False(t, byName["broken"].Ok)
	require.Contains(t, byName["broken"].Message, "oops")
}

func TestPresignCheckAgainstMinIO(t *testing.T) {
	t.Parallel()
	if !blobtest.S3Enabled() {
		t.Logf("S3 tests not registered: set %s=1 (needs Docker)", blobtest.EnvMinIO)
		return
	}
	d := dbtest.SQLite(t)
	s := blobtest.S3(t)
	checks := api.DoctorChecks(d, api.DoctorOptions{Store: s, Downloads: blob.Downloads{Mode: blob.DownloadRedirect}, GitBinary: "-"})
	for _, r := range api.RunChecks(context.Background(), checks, testCheckTimeout) {
		require.True(t, r.Ok, "%s: %s", *r.Name, r.Message)
		if *r.Name == "presigned_downloads" {
			require.Equal(t, "Presigned download URLs work from this server.", r.Message)
		}
	}
}

// TestAdminSystemReportsS3TransferModes checks that an S3 Instance reports
// both storage.s3.download_mode and storage.s3.upload_mode, so an admin can
// see from the System page whether clients reach the bucket directly.
func TestAdminSystemReportsS3TransferModes(t *testing.T) {
	t.Parallel()
	e := newPlatformEnv(t, func(*db.DB, blob.Store) []api.SystemCheck { return nil })
	h := &api.AdminSystem{Authz: authtest.NewAuthz(), DB: e.db, Info: api.SystemInfo{
		Version: "1.2.3", Role: "web", BlobBackend: "s3", DownloadMode: "redirect", UploadMode: "proxy",
	}}
	req := httptest.NewRequest(http.MethodGet, "/api/hangar/v1/admin/system", nil)
	ctx := httpx.WithInfo(req.Context(), "req-1")
	httpx.SetPrincipal(ctx, e.users["root"])
	rec := httptest.NewRecorder()
	h.AdminGetSystem(rec, req.WithContext(ctx))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	s := decode[gen.SystemInfo](t, rec.Body.Bytes())
	require.Equal(t, "s3", s.BlobStore.Backend)
	require.NotNil(t, s.BlobStore.DownloadMode)
	require.Equal(t, "redirect", *s.BlobStore.DownloadMode)
	require.NotNil(t, s.BlobStore.UploadMode)
	require.Equal(t, "proxy", *s.BlobStore.UploadMode)
}
