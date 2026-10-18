package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/auth"
	"github.com/hangar-hub/hangar/internal/blob"
	"github.com/hangar-hub/hangar/internal/blob/blobtest"
	"github.com/hangar-hub/hangar/internal/config"
	"github.com/hangar-hub/hangar/internal/datasets"
	"github.com/hangar-hub/hangar/internal/hfapi"
	"github.com/hangar-hub/hangar/internal/index"
	"github.com/hangar-hub/hangar/internal/jobs"
	"github.com/hangar-hub/hangar/internal/model"
	"github.com/hangar-hub/hangar/internal/repo"
	"github.com/hangar-hub/hangar/internal/telemetry"
	"github.com/hangar-hub/hangar/internal/upstream"
)

// newTestInstance builds a full Instance over a migrated SQLite database
// and the configured Blob store, as serve does, without serving HTTP.
// mutate, when not nil, changes the configuration first.
func newTestInstance(t *testing.T, mutate func(*config.Config)) *instance {
	t.Helper()
	cfg := testConfig(t)
	if mutate != nil {
		mutate(cfg)
	}
	ctx := context.Background()
	_, err := config.EnsureSecrets(cfg)
	require.NoError(t, err)
	tel, err := telemetry.Setup(ctx, telemetry.Options{Config: cfg.Telemetry, Version: version, Role: cfg.Server.Role,
		Getenv: func(string) string { return "" }, Stdout: io.Discard})
	require.NoError(t, err)
	t.Cleanup(func() { _ = tel.Shutdown(context.Background()) })
	d, err := openDatabase(ctx, cfg, false)
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	require.NoError(t, d.Migrate(ctx))
	blobs, err := openBlobStore(ctx, cfg, tel)
	require.NoError(t, err)
	in, err := newInstance(ctx, cfg, tel, d, blobs)
	require.NoError(t, err)
	t.Cleanup(func() { in.close(ctx) })
	return in
}

// testUser creates a user and returns its Principal.
func testUser(t *testing.T, in *instance, name string) model.Principal {
	t.Helper()
	u, err := in.auth.Users.Create(context.Background(), auth.NewUser{Username: name, Password: "a long enough password 1"}, model.System())
	require.NoError(t, err)
	return u.Principal(model.AuthSession)
}

func TestDisabledViewerAnswersNotFound(t *testing.T) {
	ctx := context.Background()
	var v disabledViewer
	var p model.Principal
	id := model.RepoID{Type: model.RepoTypeDataset, Namespace: "a", Name: "b"}
	calls := map[string]func() error{
		"IsValid":    func() error { _, err := v.IsValid(ctx, p, id); return err },
		"Splits":     func() error { _, err := v.Splits(ctx, p, id); return err },
		"FirstRows":  func() error { _, err := v.FirstRows(ctx, p, id, "default", "train"); return err },
		"Rows":       func() error { _, err := v.Rows(ctx, p, datasets.RowsRequest{}); return err },
		"Search":     func() error { _, err := v.Search(ctx, p, datasets.SearchRequest{}); return err },
		"Filter":     func() error { _, err := v.Filter(ctx, p, datasets.FilterRequest{}); return err },
		"Parquet":    func() error { _, err := v.Parquet(ctx, p, id, "default"); return err },
		"Info":       func() error { _, err := v.Info(ctx, p, id, "default"); return err },
		"Size":       func() error { _, err := v.Size(ctx, p, id, "default"); return err },
		"Statistics": func() error { _, err := v.Statistics(ctx, p, id, "default", "train"); return err },
		"Croissant":  func() error { _, err := v.Croissant(ctx, p, id); return err },
	}
	for name, call := range calls {
		require.ErrorIs(t, call(), datasets.ErrNotFound, name)
	}

	// With viewer.enabled false the Instance builds no viewer or Parquet
	// pipeline, and registers no dataset Job handlers.
	in := newTestInstance(t, func(c *config.Config) { c.Viewer.Enabled = false })
	require.Nil(t, in.viewer)
	require.Nil(t, in.pipeline)
}

func TestHubPullErrorMapsUpstreamErrors(t *testing.T) {
	require.NoError(t, hubPullError(nil))
	require.ErrorIs(t, hubPullError(upstream.ErrPullPending), hfapi.ErrPullPending)
	var pe *repo.PolicyError
	require.ErrorAs(t, hubPullError(upstream.QuotaExceeded(errors.New("the Namespace is over its quota"))), &pe)
	require.Contains(t, pe.Message, "quota")
	other := errors.New("upstream answered 500")
	require.Same(t, other, hubPullError(other))
}

func TestQuotaAdapters(t *testing.T) {
	in := newTestInstance(t, nil)
	ctx := context.Background()
	p := testUser(t, in, "alice")
	ns, _, err := in.accounting.NamespaceByName(ctx, "alice")
	require.NoError(t, err)
	require.NoError(t, repoQuota{in.accounting}.CheckQuota(ctx, ns, 10), "no quota set")
	require.NoError(t, pullQuota{in.accounting}.CheckQuota(ctx, ns, 10))

	require.NoError(t, in.accounting.SetQuota(ctx, ns, 5))
	var pe *repo.PolicyError
	require.ErrorAs(t, repoQuota{in.accounting}.CheckQuota(ctx, ns, 10), &pe, "Commits and pushes refuse with a policy error")
	err = pullQuota{in.accounting}.CheckQuota(ctx, ns, 10)
	require.ErrorIs(t, err, upstream.ErrQuotaExceeded, "Pulls tell a refusal from a failed check")
	require.NoError(t, pullQuota{in.accounting}.CheckQuota(ctx, ns, 5), "within the quota")
	_ = p
}

func TestHubPullThroughAdapter(t *testing.T) {
	in := newTestInstance(t, nil)
	ctx := context.Background()
	p := testUser(t, in, "alice")
	pt := hubPullThrough{in.pulls.PullThrough()}
	_, err := pt.Repo(ctx, p, model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "missing"})
	require.ErrorIs(t, err, repo.ErrNotFound, "no Upstream serves a Namespace that is not an Upstream's")

	id := model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "m"}
	r, err := in.repos.Create(ctx, repo.CreateParams{ID: id, Actor: p, InitialReadme: true, Visibility: model.VisibilityPublic})
	require.NoError(t, err)
	_, err = pt.File(ctx, p, r, "", model.Entry{Path: "README.md"})
	require.ErrorIs(t, err, repo.ErrNotPresent, "a file outside LFS is never pulled through")
	_, err = pt.File(ctx, p, r, "", model.Entry{Path: "w.bin", LFS: &model.LFSInfo{OID: strings.Repeat("a", 64), Size: 1}})
	require.ErrorIs(t, err, repo.ErrNotPresent, "a Repo that is not a Mirror has nothing to pull through")

	sum, err := in.mirrorStatus(ctx, r)
	require.NoError(t, err)
	require.Nil(t, sum, "a Repo that is not a Mirror has no mirror_status")
}

func TestPullSettingsFollowInstanceSettings(t *testing.T) {
	in := newTestInstance(t, nil)
	ctx := context.Background()
	get := pullSettings(in.auth)
	s, err := get(ctx)
	require.NoError(t, err)
	require.False(t, s.PullThroughEnabled, "Pull-through is off by default")
	enabled, allow, role := true, []string{"huggingface/*"}, "user"
	_, err = in.auth.Settings.Update(ctx, auth.SettingsPatch{PullThroughEnabled: &enabled, PullThroughAllow: &allow, PullThroughMinRole: &role},
		model.System())
	require.NoError(t, err)
	s, err = get(ctx)
	require.NoError(t, err)
	require.True(t, s.PullThroughEnabled)
	require.Equal(t, allow, s.PullThroughAllow)
	require.Equal(t, role, s.PullThroughMinRole)
}

// csvDataset creates a public dataset with one CSV file and returns it.
func csvDataset(t *testing.T, in *instance, p model.Principal, name string) *repo.Repo {
	t.Helper()
	ctx := context.Background()
	id := model.RepoID{Type: model.RepoTypeDataset, Namespace: p.Username, Name: name}
	_, err := in.repos.Create(ctx, repo.CreateParams{ID: id, Actor: p, InitialReadme: true, Visibility: model.VisibilityPublic})
	require.NoError(t, err)
	_, err = in.repos.Commit(ctx, repo.CommitParams{Repo: id, Revision: "main", Summary: "Add data", Actor: p,
		Operations: []repo.Operation{{Kind: repo.OpAdd, Path: "data/train.csv", Content: strings.NewReader("a,b\n1,2\n")}}})
	require.NoError(t, err)
	r, err := in.repos.Get(ctx, id)
	require.NoError(t, err)
	return r
}

// TestDatasetWiring checks the adapters between the index (W7) and the
// dataset services (W10): Config and Split counts for the index, and the
// Parquet export queued after each index Job.
func TestDatasetWiring(t *testing.T) {
	in := newTestInstance(t, nil)
	ctx := context.Background()
	p := testUser(t, in, "alice")
	r := csvDataset(t, in, p, "d")
	head, err := in.repos.Resolve(ctx, r.ID, "")
	require.NoError(t, err)

	names, splits, err := datasetLayout(in.layout)(ctx, r.ID, head)
	require.NoError(t, err)
	require.Equal(t, []string{"default"}, names)
	require.Equal(t, map[string]int{"default": 1}, splits)
	_, _, err = datasetLayout(in.layout)(ctx, model.RepoID{Type: model.RepoTypeDataset, Namespace: "alice", Name: "missing"}, head)
	require.ErrorIs(t, err, datasets.ErrNotFound)

	converts := func() []*jobs.Job {
		js, _, err := in.queue.List(ctx, jobs.Filter{Kinds: []jobs.Kind{jobs.KindDatasetConvert}})
		require.NoError(t, err)
		return js
	}

	// An older Commit, a deleted Repo and a model export nothing.
	require.NoError(t, in.afterIndex(ctx, index.CommitJob{RepoUUID: r.UUID, Repo: r.ID.String(), Type: r.ID.Type, Commit: model.CommitSHA(strings.Repeat("0", 40))}, nil))
	require.NoError(t, in.afterIndex(ctx, index.CommitJob{RepoUUID: r.UUID, Repo: "alice/gone", Type: model.RepoTypeDataset, Commit: head}, nil))
	m, err := in.repos.Create(ctx, repo.CreateParams{ID: model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "m"}, Actor: p,
		InitialReadme: true, Visibility: model.VisibilityPublic})
	require.NoError(t, err)
	mHead, err := in.repos.Resolve(ctx, m.ID, "")
	require.NoError(t, err)
	require.NoError(t, in.afterIndex(ctx, index.CommitJob{RepoUUID: m.UUID, Repo: m.ID.String(), Type: m.ID.Type, Commit: mHead}, nil))
	require.Empty(t, converts())

	// A Repo ID that does not parse is a permanent failure, not a retry.
	err = in.afterIndex(ctx, index.CommitJob{RepoUUID: r.UUID, Repo: "not a repo id", Type: model.RepoTypeDataset, Commit: head}, nil)
	require.Error(t, err)
	require.True(t, jobs.IsPermanent(err), err.Error())

	require.NoError(t, in.afterIndex(ctx, index.CommitJob{RepoUUID: r.UUID, Repo: r.ID.String(), Type: r.ID.Type, Commit: head}, nil))
	js := converts()
	require.Len(t, js, 1, "the head of a dataset's default branch is exported to Parquet")
	require.Equal(t, r.UUID, js[0].Repo)

	// Before the export the index has no rows; the export queues another
	// index Job, which records them and the size category (DATA-9).
	splitsOf := datasetSplits(in.datasets)
	before, err := splitsOf(ctx, r.UUID, head)
	require.NoError(t, err)
	require.Empty(t, before)
	pendingIndex := func() []*jobs.Job {
		list, _, err := in.queue.List(ctx, jobs.Filter{Kinds: []jobs.Kind{jobs.KindIndex}, Statuses: []jobs.Status{jobs.StatusPending}})
		require.NoError(t, err)
		return list
	}
	// The Commits' own index Jobs have not run (there is no worker); clear
	// them, as a finished index would.
	for _, j := range pendingIndex() {
		require.NoError(t, in.queue.Cancel(ctx, j.ID))
	}
	require.NoError(t, in.pipeline.HandleConvert(ctx, js[0], nil))
	again := pendingIndex()
	require.Len(t, again, 1, "the export queues the index again")
	require.Equal(t, r.UUID, again[0].Repo)
	after, err := splitsOf(ctx, r.UUID, head)
	require.NoError(t, err)
	require.Len(t, after, 1)
	require.True(t, after[0].Converted)
	require.Equal(t, int64(1), after[0].Rows)
	sum, err := in.indexer.Index(ctx, r.UUID, r.ID, head)
	require.NoError(t, err)
	require.Equal(t, int64(1), sum.Dataset.Rows)
	require.Equal(t, "n<1K", sum.Dataset.SizeCategory)

	// With the viewer off there is no pipeline to queue for.
	in.pipeline = nil
	require.NoError(t, in.afterIndex(ctx, index.CommitJob{RepoUUID: r.UUID, Repo: r.ID.String(), Type: r.ID.Type, Commit: head}, nil))
}

// TestAfterIndexOfADeletedRepo checks that an index Job that finishes after
// its Repo is deleted does not warn that the Repo's Dependencies could not
// be resolved.
func TestAfterIndexOfADeletedRepo(t *testing.T) {
	in := newTestInstance(t, nil)
	ctx := context.Background()
	p := testUser(t, in, "alice")
	m, err := in.repos.Create(ctx, repo.CreateParams{ID: model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "gone"}, Actor: p,
		InitialReadme: true, Visibility: model.VisibilityPublic})
	require.NoError(t, err)
	head, err := in.repos.Resolve(ctx, m.ID, "")
	require.NoError(t, err)
	require.NoError(t, in.repos.Delete(ctx, m.ID, p))

	var logs bytes.Buffer
	in.log = slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	require.NoError(t, in.afterIndex(ctx, index.CommitJob{RepoUUID: m.UUID, Repo: m.ID.String(), Type: m.ID.Type, Commit: head}, nil))
	require.NotContains(t, logs.String(), `"level":"WARN"`)
	require.Contains(t, logs.String(), "resolve dependencies: repo deleted")
}

// presigningStore is a Blob store that presigns uploads to a fixed URL.
type presigningStore struct {
	blob.Store
	url string
}

func (s presigningStore) PresignPut(context.Context, string, int64, []byte, time.Duration) (blob.PresignedRequest, error) {
	return blob.PresignedRequest{URL: s.url}, nil
}

func TestPresignOrigins(t *testing.T) {
	ctx := context.Background()
	in := &instance{blobs: newEmptyBlobs()}
	require.Nil(t, in.presignOrigins(ctx), "a store that cannot presign adds no origin")

	fake := blobtest.NewFake()
	fake.PresignBaseURL = "https://bucket.example.com:9000/prefix"
	in.blobs = fake
	require.Equal(t, []string{"https://bucket.example.com:9000"}, in.presignOrigins(ctx))

	for _, bad := range []string{"not a url", "/relative/path", "%zz"} {
		in.blobs = presigningStore{Store: newEmptyBlobs(), url: bad}
		require.Nil(t, in.presignOrigins(ctx), bad)
	}
}

func TestRefreshStaleLogsFailures(t *testing.T) {
	in := newTestInstance(t, nil)
	var logs strings.Builder
	in.log = slog.New(slog.NewTextHandler(&logs, nil))
	in.refreshStale(context.Background())
	require.Empty(t, logs.String(), "nothing is stale on a new Instance")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	in.refreshStale(ctx)
	require.Contains(t, logs.String(), "reindex stale results")
	require.Contains(t, logs.String(), "rescan stale results")
}

func TestBlobBackendAndDownloadMode(t *testing.T) {
	cfg := &config.Config{}
	require.Equal(t, config.BackendFS, blobBackend(cfg), "an empty backend is the filesystem")
	require.Empty(t, downloadMode(cfg), "the filesystem always streams")
	cfg.Storage.Backend = config.BackendS3
	cfg.Storage.S3.DownloadMode = config.DownloadProxy
	require.Equal(t, config.BackendS3, blobBackend(cfg))
	require.Equal(t, config.DownloadProxy, downloadMode(cfg))
}

func TestUploadModeIsReportedOnlyForS3(t *testing.T) {
	cfg := &config.Config{}
	cfg.Storage.S3.UploadMode = config.UploadProxy
	require.Empty(t, uploadMode(cfg), "the filesystem always takes uploads through Hangar")
	cfg.Storage.Backend = config.BackendS3
	require.Equal(t, config.UploadProxy, uploadMode(cfg))
	cfg.Storage.S3.UploadMode = config.UploadDirect
	require.Equal(t, config.UploadDirect, uploadMode(cfg))
}

// TestS3ProxyModesKeepTransfersOnHangar checks the wiring of
// storage.s3.upload_mode and download_mode proxy against MinIO: every
// git-lfs action, basic and multipart, points at Hangar and never at the
// bucket endpoint, which clients of the Compose examples cannot resolve,
// and an upload through Hangar lands in the bucket.
func TestS3ProxyModesKeepTransfersOnHangar(t *testing.T) {
	blobtest.RequireS3(t)
	s3srv := blobtest.S3TestServer(t)
	_ = blobtest.S3(t) // creates the bucket
	srv := httptest.NewUnstartedServer(http.NotFoundHandler())
	pub := "http://" + srv.Listener.Addr().String()
	in := newTestInstance(t, func(c *config.Config) {
		c.Server.PublicURL = pub
		c.Storage.Backend = config.BackendS3
		c.Storage.S3 = config.S3Config{Endpoint: s3srv.Endpoint, Region: s3srv.Region, Bucket: s3srv.Bucket,
			Prefix: "cmd-" + strings.ReplaceAll(uuid.NewString(), "-", ""), AccessKeyID: s3srv.AccessKey, SecretAccessKey: s3srv.SecretKey,
			UsePathStyle: true, DownloadMode: config.DownloadProxy, UploadMode: config.UploadProxy, PresignTTL: time.Hour}
		c.LFS.MultipartThreshold = config.MiB
	})
	ctx := context.Background()
	h, err := newHandler(ctx, in, newHealth(in.db, in.blobs), false)
	require.NoError(t, err)
	srv.Config.Handler = h
	srv.Start()
	t.Cleanup(srv.Close)
	require.Nil(t, in.presignOrigins(ctx), "the UI's Content Security Policy allows no bucket origin")

	p := testUser(t, in, "alice")
	_, token, err := in.auth.TokenStore.Create(ctx, auth.NewToken{UserID: p.UserID, Name: "push", Scope: model.ScopeWrite})
	require.NoError(t, err)
	_, err = in.repos.Create(ctx, repo.CreateParams{ID: model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "m"}, Actor: p,
		Visibility: model.VisibilityPrivate})
	require.NoError(t, err)

	type lfsAction struct {
		Href   string            `json:"href"`
		Header map[string]string `json:"header"`
	}
	batch := func(operation string, transfers []string, oid string, size int64) lfsAction {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"operation": operation, "transfers": transfers,
			"objects": []map[string]any{{"oid": oid, "size": size}}})
		req, _ := http.NewRequest(http.MethodPost, pub+"/alice/m.git/info/lfs/objects/batch", bytes.NewReader(body))
		req.Header.Set("Accept", "application/vnd.git-lfs+json")
		req.Header.Set("Content-Type", "application/vnd.git-lfs+json")
		req.SetBasicAuth("alice", token)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
		var out struct {
			Objects []struct {
				Actions map[string]lfsAction `json:"actions"`
			} `json:"objects"`
		}
		require.NoError(t, json.Unmarshal(raw, &out), string(raw))
		require.Len(t, out.Objects, 1, string(raw))
		a, ok := out.Objects[0].Actions[operation]
		require.True(t, ok, string(raw))
		require.True(t, strings.HasPrefix(a.Href, pub+"/"), "the %s action is served by Hangar, not %s: %s", operation, s3srv.Endpoint, a.Href)
		for k, v := range a.Header {
			require.NotContains(t, v, s3srv.Endpoint, k)
		}
		return a
	}

	// A multipart upload's part URLs are Hangar's too.
	big := batch("upload", []string{"multipart", "basic"}, strings.Repeat("b", 64), 6<<20)
	require.NotEmpty(t, big.Header["00001"])

	// A basic upload streams through Hangar into the bucket.
	content := []byte("weights that clients upload through Hangar")
	sum := sha256.Sum256(content)
	oid := hex.EncodeToString(sum[:])
	up := batch("upload", []string{"basic"}, oid, int64(len(content)))
	req, _ := http.NewRequest(http.MethodPut, up.Href, bytes.NewReader(content))
	for k, v := range up.Header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	info, err := in.blobs.Stat(ctx, model.LFSObjectKey(oid))
	require.NoError(t, err)
	require.Equal(t, int64(len(content)), info.Size)

	// Downloads stream through Hangar as well.
	_, err = in.repos.Commit(ctx, repo.CommitParams{Repo: model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "m"}, Revision: "main",
		Summary: "Add weights", Actor: p, Operations: []repo.Operation{{Kind: repo.OpAddLFS, Path: "w.bin", OID: oid, Size: int64(len(content))}}})
	require.NoError(t, err)
	down := batch("download", []string{"basic"}, oid, int64(len(content)))
	resp, err = http.Get(down.Href)
	require.NoError(t, err)
	raw, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, content, raw)
}
