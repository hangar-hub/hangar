package repo_test

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/db"
	"github.com/hangar-hub/hangar/internal/gitstore"
	"github.com/hangar-hub/hangar/internal/jobs"
	"github.com/hangar-hub/hangar/internal/model"
	"github.com/hangar-hub/hangar/internal/repo"
)

func TestZipExportJob(t *testing.T) {
	forEach(t, func(t *testing.T, e *env) {
		require.NoError(t, e.svc.RegisterJobs(e.reg, e.lfs, repo.JobOptions{Blobs: e.blobs,
			ExportURL: func(_ context.Context, key, filename string, _ time.Duration) (string, error) {
				return "https://hangar.example/" + key + "?name=" + filename, nil
			}}))
		require.Error(t, e.svc.RegisterJobs(jobs.NewRegistry(), e.lfs, repo.JobOptions{}), "Blobs is required")
		alice := e.addUser("alice")
		id := model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "zipme"}
		e.create(repo.CreateParams{ID: id, Actor: alice})
		weights := []byte("lfs weights")
		oid := e.putLFS(weights, alice)
		res := e.commit(id, alice, add("config.json", "{}"), add("sub/dir/a.txt", "a"),
			repo.Operation{Kind: repo.OpAddLFS, Path: "sub/w.safetensors", OID: oid, Size: int64(len(weights))})

		jobID, err := e.jobs.Enqueue(e.ctx, jobs.NewJob{Kind: jobs.KindZipExport, Payload: repo.ZipJob{RepoUUID: mustUUID(e, id), Commit: res.SHA}})
		require.NoError(t, err)
		subID, err := e.jobs.Enqueue(e.ctx, jobs.NewJob{Kind: jobs.KindZipExport, Payload: repo.ZipJob{RepoUUID: mustUUID(e, id), Commit: res.SHA, Path: "sub"}})
		require.NoError(t, err)
		badID, err := e.jobs.Enqueue(e.ctx, jobs.NewJob{Kind: jobs.KindZipExport, Payload: repo.ZipJob{RepoUUID: mustUUID(e, id), Commit: res.SHA, Path: "nope"}})
		require.NoError(t, err)
		e.jobs.RunPending(e.ctx)

		check := func(jid uuid.UUID, want map[string]string, filename string) {
			t.Helper()
			j, err := e.jobs.Get(e.ctx, jid)
			require.NoError(t, err)
			require.Equal(t, jobs.StatusSucceeded, j.Status, j.Error)
			var zr repo.ZipResult
			require.NoError(t, json.Unmarshal(j.Result, &zr))
			require.Equal(t, filename, zr.Filename)
			require.Equal(t, repo.ExportKey(jid), zr.Key)
			require.Contains(t, zr.URL, zr.Key)
			b, ok := e.blobs.Bytes(zr.Key)
			require.True(t, ok)
			require.Equal(t, int64(len(b)), zr.Size)
			zrd, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
			require.NoError(t, err)
			got := map[string]string{}
			for _, f := range zrd.File {
				rc, err := f.Open()
				require.NoError(t, err)
				c, _ := io.ReadAll(rc)
				rc.Close()
				got[f.Name] = string(c)
			}
			require.Equal(t, want, got)
			require.Equal(t, len(want), zr.Files)
		}
		short := res.SHA.Short()
		check(jobID, map[string]string{"zipme/.gitattributes": string(repo.DefaultGitAttributes(model.RepoTypeModel)),
			"zipme/config.json": "{}", "zipme/sub/dir/a.txt": "a", "zipme/sub/w.safetensors": string(weights)}, "zipme-"+short+".zip")
		check(subID, map[string]string{"zipme/dir/a.txt": "a", "zipme/w.safetensors": string(weights)}, "zipme-"+short+"-sub.zip")
		j, err := e.jobs.Get(e.ctx, badID)
		require.NoError(t, err)
		require.Equal(t, jobs.StatusFailed, j.Status)
	})
}

func mustUUID(e *env, id model.RepoID) uuid.UUID {
	r, err := e.svc.Get(e.ctx, id)
	require.NoError(e.t, err)
	return r.UUID
}

func TestCompactionJob(t *testing.T) {
	forEach(t, func(t *testing.T, e *env) {
		require.NoError(t, e.svc.RegisterJobs(e.reg, e.lfs, repo.JobOptions{Blobs: e.blobs, PackGracePeriod: time.Nanosecond}))
		alice := e.addUser("alice")
		id := model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "packs"}
		r := e.create(repo.CreateParams{ID: id, Actor: alice})
		for i := range 4 {
			e.commit(id, alice, add(fmt.Sprintf("f%d.txt", i), fmt.Sprint(i)))
		}
		require.NoError(t, e.svc.CreateTag(e.ctx, repo.TagParams{Repo: id, Tag: "v1", Revision: "main", Message: "one", Actor: alice}))
		live := func() []string {
			packs, err := gitstore.LivePacks(e.ctx, e.d, r.UUID)
			require.NoError(t, err)
			out := []string{}
			for _, p := range packs {
				out = append(out, p.PackSHA1)
			}
			sort.Strings(out)
			return out
		}
		require.Len(t, live(), 6)
		// The threshold (3 here) was crossed, so a git.compact Job is queued.
		js, _, err := e.jobs.List(e.ctx, jobs.Filter{Kinds: []jobs.Kind{jobs.KindGitCompact}})
		require.NoError(t, err)
		require.NotEmpty(t, js)
		before := e.export(id)
		e.jobs.RunPending(e.ctx)
		require.Len(t, live(), 1)
		var retired int64
		retired, err = e.d.NewSelect().Model((*db.Pack)(nil)).Where("repo_id = ?", r.UUID).Where("state <> ?", gitstore.PackLive).Count(e.ctx)
		require.NoError(t, err)
		require.Zero(t, retired, "the sweep after compaction deleted the retired packs")
		after := e.export(id)
		gitCmd(t, after, "fsck", "--strict", "--no-dangling")
		require.Equal(t, gitCmd(t, before, "for-each-ref"), gitCmd(t, after, "for-each-ref"))
		require.Equal(t, "3", e.read(id, "", "f3.txt"))

		// A fresh replica with an empty cache reads from the compacted pack.
		gs2, err := gitstore.New(gitstore.Options{DB: e.d, Blobs: e.blobs, CacheDir: t.TempDir()})
		require.NoError(t, err)
		e2, err := repo.NewStore(withOpts(e.opts, func(o *repo.Options) { o.Git = gs2 }))
		require.NoError(t, err)
		_, err = e2.Log(e.ctx, id, "v1", repo.LogOpts{})
		require.NoError(t, err)
		require.Positive(t, gs2.CacheBytes())

		compacted, err := e.svc.Compact(e.ctx, uuid.New())
		require.NoError(t, err)
		require.False(t, compacted, "missing Repos are skipped")
	}, func(o *repo.Options) { o.CompactThreshold = 3 })
}

func TestUploadExpireJobAndSchedules(t *testing.T) {
	forEach(t, func(t *testing.T, e *env) {
		require.NoError(t, e.svc.RegisterJobs(e.reg, e.lfs, repo.JobOptions{Blobs: e.blobs}))
		require.Equal(t, jobs.KindUploadExpire, repo.Schedules()[0].Kind)
		u, err := e.lfs.CreateUpload(e.ctx, repo.UploadParams{OID: sha256Hex([]byte("x")), Size: 1, TTL: time.Nanosecond})
		require.NoError(t, err)
		time.Sleep(time.Millisecond)
		_, err = e.jobs.Enqueue(e.ctx, jobs.NewJob{Kind: jobs.KindUploadExpire})
		require.NoError(t, err)
		e.jobs.RunPending(e.ctx)
		require.ErrorIs(t, e.lfs.AbortUpload(e.ctx, u.ID), repo.ErrNotFound)
	})
}

func TestUploadExpireCanBeLeftToGC(t *testing.T) {
	forEach(t, func(t *testing.T, e *env) {
		reg := jobs.NewRegistry()
		require.NoError(t, e.svc.RegisterJobs(reg, e.lfs, repo.JobOptions{Blobs: e.blobs, UploadsExpiredElsewhere: true}))
		require.NoError(t, reg.Register(jobs.KindUploadExpire, func(context.Context, *jobs.Job, jobs.Progress) error { return nil }, jobs.HandlerOptions{}),
			"upload.expire is free for internal/gc")
		for _, k := range []jobs.Kind{jobs.KindZipExport, jobs.KindRepoPurge, jobs.KindGitCompact} {
			require.Error(t, reg.Register(k, func(context.Context, *jobs.Job, jobs.Progress) error { return nil }, jobs.HandlerOptions{}), k)
		}
	})
}

func TestScanJobsForTagsAndPullRequests(t *testing.T) {
	forEach(t, func(t *testing.T, e *env) {
		alice := e.addUser("alice")
		id := model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "scan-refs"}
		e.create(repo.CreateParams{ID: id, Actor: alice})
		first := e.commit(id, alice, add("a", "a"))
		pr, err := e.svc.Commit(e.ctx, repo.CommitParams{Repo: id, Revision: model.PRRef(1), CreateFrom: first.SHA,
			Summary: "pr", Operations: []repo.Operation{add("b", "b")}, Actor: alice})
		require.NoError(t, err)
		// The annotated tag names a tag object, not a Commit.
		require.NoError(t, e.svc.CreateTag(e.ctx, repo.TagParams{Repo: id, Tag: "v1", Revision: pr.SHA.String(), Message: "Release 1", Actor: alice}))

		commits := func(kind jobs.Kind) map[model.CommitSHA]bool {
			js, _, err := e.jobs.List(e.ctx, jobs.Filter{Kinds: []jobs.Kind{kind}})
			require.NoError(t, err)
			out := map[model.CommitSHA]bool{}
			for _, j := range js {
				var p repo.CommitJob
				require.NoError(t, j.DecodePayload(&p))
				out[p.Commit] = true
			}
			return out
		}
		scans, indexes := commits(jobs.KindScan), commits(jobs.KindIndex)
		require.True(t, scans[pr.SHA], "a pull request's Commit is scanned, so blocking unsafe downloads does not refuse its files for good")
		require.False(t, indexes[pr.SHA], "only branch heads are indexed")
		for c := range scans {
			info, err := e.svc.GetCommit(e.ctx, id, c)
			require.NoError(t, err, "scan Jobs name Commits, never tag objects")
			require.Equal(t, c, info.SHA)
		}

		// A lightweight tag queues a scan of its Commit too. Cancel the
		// pull request's scan first, so the tag's Job is not deduplicated.
		tagged, err := e.svc.Commit(e.ctx, repo.CommitParams{Repo: id, Revision: model.PRRef(2), CreateFrom: first.SHA,
			Summary: "pr 2", Operations: []repo.Operation{add("c", "c")}, Actor: alice})
		require.NoError(t, err)
		active := func() bool {
			js, _, err := e.jobs.List(e.ctx, jobs.Filter{Kinds: []jobs.Kind{jobs.KindScan}})
			require.NoError(t, err)
			found := false
			for _, j := range js {
				var p repo.CommitJob
				require.NoError(t, j.DecodePayload(&p))
				if p.Commit == tagged.SHA && j.Status.Active() {
					found = true
				}
			}
			return found
		}
		js, _, err := e.jobs.List(e.ctx, jobs.Filter{Kinds: []jobs.Kind{jobs.KindScan}})
		require.NoError(t, err)
		for _, j := range js {
			require.NoError(t, e.jobs.Cancel(e.ctx, j.ID))
		}
		require.False(t, active())
		require.NoError(t, e.svc.CreateTag(e.ctx, repo.TagParams{Repo: id, Tag: "v2", Revision: tagged.SHA.String(), Actor: alice}))
		require.True(t, active())
	})
}
