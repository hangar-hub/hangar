package gc_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/hangar-hub/hangar/internal/blob"
	blobfs "github.com/hangar-hub/hangar/internal/blob/fs"
	"github.com/hangar-hub/hangar/internal/db"
	"github.com/hangar-hub/hangar/internal/db/dbtest"
	"github.com/hangar-hub/hangar/internal/gc"
	"github.com/hangar-hub/hangar/internal/jobs"
	"github.com/hangar-hub/hangar/internal/jobs/jobstest"
	"github.com/hangar-hub/hangar/internal/model"
	"github.com/hangar-hub/hangar/internal/telemetry"
)

// fixture is a database and filesystem Blob store with helpers to insert
// rows the way the owning packages would, and to age blobs.
type fixture struct {
	t     *testing.T
	db    *db.DB
	store *blobfs.Store
	now   time.Time
}

func newFixture(t *testing.T, d *db.DB) *fixture {
	t.Helper()
	s, err := blobfs.New(filepath.Join(t.TempDir(), "blobs"))
	require.NoError(t, err)
	return &fixture{t: t, db: d, store: s, now: time.Now().UTC().Truncate(time.Microsecond)}
}

func (f *fixture) collector() *gc.Collector {
	return gc.New(f.db, f.store, gc.Options{Now: func() time.Time { return f.now }})
}

func (f *fixture) namespace(name string) uuid.UUID {
	ns := &db.Namespace{ID: db.NewID(), Name: name, Kind: "org"}
	_, err := f.db.NewInsert().Model(ns).Exec(context.Background())
	require.NoError(f.t, err)
	return ns.ID
}

func (f *fixture) repo(ns uuid.UUID, name string, deleted bool) uuid.UUID {
	r := &db.Repo{ID: db.NewID(), NamespaceID: ns, Name: name, Type: "model", Visibility: "public",
		DefaultBranch: "main", HeadRef: "refs/heads/main", Tags: []string{}, Settings: map[string]any{}, Gating: map[string]any{}}
	if deleted {
		at := f.now.Add(-time.Hour)
		r.DeletedAt = &at
	}
	_, err := f.db.NewInsert().Model(r).Exec(context.Background())
	require.NoError(f.t, err)
	return r.ID
}

// object stores an LFS object and its row, last referenced at refAt.
func (f *fixture) object(content string, refAt time.Time) (oid, key string) {
	sum := sha256.Sum256([]byte(content))
	oid = hex.EncodeToString(sum[:])
	key = model.LFSObjectKey(oid)
	require.NoError(f.t, f.store.Put(context.Background(), key, strings.NewReader(content), int64(len(content))))
	verified := refAt
	_, err := f.db.NewInsert().Model(&db.LFSObject{OID: oid, Size: int64(len(content)), StorageKey: key, VerifiedAt: &verified, LastReferencedAt: refAt}).Exec(context.Background())
	require.NoError(f.t, err)
	f.age(key, refAt)
	return oid, key
}

func (f *fixture) reference(repo uuid.UUID, oid string, size int64, present bool) {
	_, err := f.db.NewInsert().Model(&db.RepoLFS{RepoID: repo, OID: oid, Size: size, Present: present}).Exec(context.Background())
	require.NoError(f.t, err)
}

func (f *fixture) upload(oid, key, uploadID string, expires time.Time) uuid.UUID {
	u := &db.Upload{ID: db.NewID(), OID: oid, Size: 1, Kind: "lfs", UploadID: uploadID, StorageKey: key, Parts: []db.UploadPart{}, ExpiresAt: expires}
	_, err := f.db.NewInsert().Model(u).Exec(context.Background())
	require.NoError(f.t, err)
	return u.ID
}

func (f *fixture) pack(repo uuid.UUID, sha, state string, at time.Time) {
	p := &db.Pack{RepoID: repo, PackSHA1: sha, Size: 100, ObjectCount: 3, State: state}
	if state == "retired" || state == "orphaned" {
		p.RetiredAt = &at
	}
	p.CreatedAt, p.UpdatedAt = at, at
	_, err := f.db.NewInsert().Model(p).Exec(context.Background())
	require.NoError(f.t, err)
	for _, ext := range []string{"pack", "idx", "rev"} {
		f.blob(gc.PackPrefix(repo, sha)+ext, at)
	}
}

// blob stores a blob and sets its modification time.
func (f *fixture) blob(key string, at time.Time) {
	require.NoError(f.t, f.store.Put(context.Background(), key, strings.NewReader(key), int64(len(key))))
	f.age(key, at)
}

// rawBlob writes a blob's file directly, without the store's fsync, for
// tests that need thousands of them.
func (f *fixture) rawBlob(key string, at time.Time) {
	p := filepath.Join(f.store.Root(), filepath.FromSlash(key))
	require.NoError(f.t, os.MkdirAll(filepath.Dir(p), 0o750))
	require.NoError(f.t, os.WriteFile(p, []byte(key), 0o600))
	require.NoError(f.t, os.Chtimes(p, at, at))
}

func (f *fixture) age(key string, at time.Time) {
	p := filepath.Join(f.store.Root(), filepath.FromSlash(key))
	require.NoError(f.t, os.Chtimes(p, at, at))
}

func (f *fixture) exists(key string) bool {
	_, err := f.store.Stat(context.Background(), key)
	if err == nil {
		return true
	}
	require.ErrorIs(f.t, err, blob.ErrNotFound)
	return false
}

func (f *fixture) hasRow(oid string) bool {
	ok, err := f.db.NewSelect().Model((*db.LFSObject)(nil)).Where("oid = ?", oid).Exists(context.Background())
	require.NoError(f.t, err)
	return ok
}

func TestGCNeverDeletesReferencedLFSObjects(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		ctx := context.Background()
		f := newFixture(t, d)
		old := f.now.Add(-30 * 24 * time.Hour)
		ns := f.namespace("ml")
		repo := f.repo(ns, "bert", false)
		mirror := f.repo(ns, "partial", false)
		deleted := f.repo(ns, "gone", true)

		committed, committedKey := f.object("in a commit", old)
		f.reference(repo, committed, 11, true)
		notPresent, notPresentKey := f.object("partial mirror gap", old)
		f.reference(mirror, notPresent, 18, false)
		inDeleted, inDeletedKey := f.object("in a deleted repo", old)
		f.reference(deleted, inDeleted, 17, true)
		uploading, uploadingKey := f.object("being uploaded again", old)
		f.upload(uploading, uploadingKey, "", f.now.Add(time.Hour))
		recent, recentKey := f.object("unreferenced but recent", f.now.Add(-time.Hour))
		garbage, garbageKey := f.object("unreferenced and old", old)

		// Blobs without rows: an orphan, a recent orphan, and the target
		// of a presigned PUT that has not been verified yet.
		orphanKey := model.LFSObjectKey(strings.Repeat("0a", 32))
		f.blob(orphanKey, old)
		freshOrphanKey := model.LFSObjectKey(strings.Repeat("0b", 32))
		f.blob(freshOrphanKey, f.now.Add(-time.Minute))
		presignedOID := strings.Repeat("0c", 32)
		presignedKey := model.LFSObjectKey(presignedOID)
		f.blob(presignedKey, old)
		f.upload(presignedOID, presignedKey, "", f.now.Add(time.Hour))

		r, err := f.collector().Run(ctx, gc.ScopeAll, nil)
		require.NoError(t, err)
		require.Equal(t, 1, r.LFSObjectsDeleted)
		require.Equal(t, int64(len("unreferenced and old")), r.LFSBytesFreed)
		require.Equal(t, 1, r.OrphanLFSBlobsDeleted)
		require.Zero(t, r.Errors)

		require.False(t, f.hasRow(garbage))
		require.False(t, f.exists(garbageKey))
		require.False(t, f.exists(orphanKey))
		for oid, key := range map[string]string{committed: committedKey, notPresent: notPresentKey, inDeleted: inDeletedKey, uploading: uploadingKey, recent: recentKey} {
			require.True(t, f.hasRow(oid), "row of %s kept", key)
			require.True(t, f.exists(key), "blob %s kept", key)
		}
		require.True(t, f.exists(freshOrphanKey), "orphans within the grace period are kept")
		require.True(t, f.exists(presignedKey), "the target of an upload in progress is kept")

		// A second run finds nothing more.
		r, err = f.collector().Run(ctx, gc.ScopeAll, nil)
		require.NoError(t, err)
		require.Zero(t, r.LFSObjectsDeleted+r.OrphanLFSBlobsDeleted)
	})
}

// TestGCRacesWithReferencingWriters runs GC while writers reference old,
// unreferenced objects with the referencing protocol in the package doc.
// Whatever the interleaving, every repo_lfs row must point at an object
// whose row and blob both exist.
func TestGCRacesWithReferencingWriters(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		ctx := context.Background()
		f := newFixture(t, d)
		old := f.now.Add(-30 * 24 * time.Hour)
		repo := f.repo(f.namespace("race"), "r", false)
		var oids []string
		for i := range 60 {
			oid, _ := f.object(fmt.Sprintf("object %d", i), old)
			oids = append(oids, oid)
		}
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, err := f.collector().Run(ctx, gc.ScopeLFS, nil)
			require.NoError(t, err)
		}()
		go func() {
			defer wg.Done()
			for i, oid := range oids {
				if i%2 == 1 {
					continue
				}
				err := d.InTx(ctx, func(ctx context.Context, tx bun.Tx) error {
					res, err := tx.NewUpdate().Model((*db.LFSObject)(nil)).Set("last_referenced_at = ?", db.Now()).Where("oid = ?", oid).Exec(ctx)
					if err != nil {
						return err
					}
					if n, _ := res.RowsAffected(); n != 1 {
						return nil // GC got there first: the writer would store the object again.
					}
					_, err = tx.NewInsert().Model(&db.RepoLFS{RepoID: repo, OID: oid, Size: 1, Present: true}).Exec(ctx)
					return err
				})
				require.NoError(t, err)
			}
		}()
		wg.Wait()
		var refs []string
		require.NoError(t, d.NewSelect().Model((*db.RepoLFS)(nil)).Column("oid").Scan(ctx, &refs))
		require.NotEmpty(t, oids)
		for _, oid := range refs {
			require.True(t, f.hasRow(oid), "referenced object %s has a row", oid)
			require.True(t, f.exists(model.LFSObjectKey(oid)), "referenced object %s has a blob", oid)
		}
		for i, oid := range oids {
			if i%2 == 1 {
				require.False(t, f.hasRow(oid), "never-referenced object %d is collected", i)
			}
		}
	})
}

func TestGCPacksAndGitData(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		ctx := context.Background()
		f := newFixture(t, d)
		old := f.now.Add(-30 * 24 * time.Hour)
		repo := f.repo(f.namespace("git"), "r", false)
		sha := func(c byte) string { return strings.Repeat(string(c), 40) }
		f.pack(repo, sha('a'), "live", old)
		f.pack(repo, sha('b'), "retired", f.now.Add(-2*time.Hour))
		f.pack(repo, sha('c'), "retired", f.now.Add(-10*time.Minute))
		f.pack(repo, sha('d'), "orphaned", f.now.Add(-3*time.Hour))
		f.pack(repo, sha('e'), "pending", f.now.Add(-5*time.Hour))
		f.pack(repo, sha('f'), "pending", f.now.Add(-5*time.Minute))
		// A pack an earlier run claimed and did not finish deleting, and one
		// a run is deleting now.
		f.pack(repo, sha('g'), "deleting", f.now.Add(-3*time.Hour))
		f.pack(repo, sha('h'), "deleting", f.now.Add(-5*time.Minute))
		// A pack file with no row (the push died before inserting it), and
		// the git data of a purged Repo.
		f.blob(gc.PackPrefix(repo, sha('9'))+"pack", old)
		purged := uuid.New()
		f.blob("git/"+purged.String()+"/pack/pack-"+sha('8')+".pack", old)
		f.blob("git/"+purged.String()+"/pack/pack-"+sha('7')+".pack", f.now.Add(-time.Hour))

		r, err := f.collector().Run(ctx, gc.ScopeGit, nil)
		require.NoError(t, err)
		require.Equal(t, 4, r.PacksDeleted, "retired after the grace period, orphaned, dead pending, and left deleting")
		require.Equal(t, int64(400), r.PackBytesFreed)
		require.Equal(t, 2, r.OrphanGitBlobsDeleted)
		var left []string
		require.NoError(t, d.NewSelect().Model((*db.Pack)(nil)).Column("pack_sha1").OrderExpr("pack_sha1").Scan(ctx, &left))
		require.Equal(t, []string{sha('a'), sha('c'), sha('f'), sha('h')}, left)
		for _, c := range []byte{'a', 'c', 'f', 'h'} {
			require.True(t, f.exists(gc.PackPrefix(repo, sha(c))+"idx"))
		}
		for _, c := range []byte{'b', 'd', 'e', 'g', '9'} {
			require.False(t, f.exists(gc.PackPrefix(repo, sha(c))+"pack"))
		}
		require.False(t, f.exists("git/"+purged.String()+"/pack/pack-"+sha('8')+".pack"))
		require.True(t, f.exists("git/"+purged.String()+"/pack/pack-"+sha('7')+".pack"), "recent data is kept even without a Repo")
	})
}

// TestGCOrphanSweepsReachEveryKey checks that the orphan sweeps walk the
// whole of lfs/ and git/ in one run: referenced objects across several
// listing batches never stop a run before the orphans whose keys sort last,
// such as the git data of the newest purged Repo (UUIDv7 IDs sort by age).
func TestGCOrphanSweepsReachEveryKey(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		ctx := context.Background()
		f := newFixture(t, d)
		old := f.now.Add(-30 * 24 * time.Hour)
		const referenced = 1600 // more than three listing batches
		rows := make([]db.LFSObject, 0, referenced)
		for i := range referenced {
			sum := sha256.Sum256(fmt.Appendf(nil, "object %d", i))
			oid := hex.EncodeToString(sum[:])
			if strings.HasPrefix(oid, "ff") {
				continue // Keep the orphan below last in key order.
			}
			key := model.LFSObjectKey(oid)
			f.rawBlob(key, old)
			rows = append(rows, db.LFSObject{OID: oid, Size: int64(len(key)), StorageKey: key, LastReferencedAt: f.now})
		}
		for start := 0; start < len(rows); start += 200 {
			chunk := rows[start:min(start+200, len(rows))]
			_, err := d.NewInsert().Model(&chunk).Exec(ctx)
			require.NoError(t, err)
		}
		orphanFirst := model.LFSObjectKey(strings.Repeat("0", 64))
		orphanLast := model.LFSObjectKey(strings.Repeat("f", 64))
		f.blob(orphanFirst, old)
		f.blob(orphanLast, old)

		ns := f.namespace("scale")
		var live []uuid.UUID
		for i := range 4 {
			id := f.repo(ns, fmt.Sprint("r", i), false)
			live = append(live, id)
			for j := range 400 {
				f.rawBlob(fmt.Sprintf("git/%s/objects/info/f%03d", id, j), old)
			}
		}
		purged := db.NewID() // Newer than every live Repo, so its keys sort last.
		purgedKey := "git/" + purged.String() + "/pack/pack-" + strings.Repeat("e", 40) + ".pack"
		f.blob(purgedKey, old)

		r, err := f.collector().Run(ctx, gc.ScopeAll, nil)
		require.NoError(t, err)
		require.Equal(t, 2, r.OrphanLFSBlobsDeleted)
		require.Equal(t, 1, r.OrphanGitBlobsDeleted)
		require.Zero(t, r.LFSObjectsDeleted, "every row is recently referenced")
		require.Zero(t, r.Errors)
		require.False(t, f.exists(orphanFirst))
		require.False(t, f.exists(orphanLast), "an orphan after %d referenced objects is reached", len(rows))
		require.False(t, f.exists(purgedKey), "git data of the newest purged Repo is reached")
		for _, o := range rows {
			require.True(t, f.exists(o.StorageKey))
		}
		for _, id := range live {
			require.True(t, f.exists(fmt.Sprintf("git/%s/objects/info/f399", id)))
		}
	})
}

func TestGCStagingAndAbandonedUploads(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		ctx := context.Background()
		f := newFixture(t, d)
		f.blob("tmp/old", f.now.Add(-25*time.Hour))
		f.blob("tmp/new", f.now.Add(-time.Hour))
		f.blob("exports/old.zip", f.now.Add(-48*time.Hour))
		f.blob("avatars/keep", f.now.Add(-1000*time.Hour))

		abandoned, err := f.store.CreateMultipart(ctx, "lfs/sha256/ab/an/abandoned")
		require.NoError(t, err)
		tracked, err := f.store.CreateMultipart(ctx, "lfs/sha256/tr/ac/tracked")
		require.NoError(t, err)
		f.upload("tracked", "lfs/sha256/tr/ac/tracked", tracked, f.now.Add(time.Hour))
		for _, id := range []string{abandoned, tracked} {
			f.age("uploads/"+id+"/target", f.now.Add(-72*time.Hour))
		}
		// Partial writes a killed process left in the filesystem backend's
		// staging directory, which List never shows.
		staging := filepath.Join(f.store.Root(), ".staging")
		for name, at := range map[string]time.Time{"put-crashed": f.now.Add(-30 * time.Hour), "put-live": f.now.Add(-time.Minute)} {
			p := filepath.Join(staging, name)
			require.NoError(t, os.WriteFile(p, []byte("partial"), 0o600))
			require.NoError(t, os.Chtimes(p, at, at))
		}
		r, err := f.collector().Run(ctx, gc.ScopeAll, nil)
		require.NoError(t, err)
		require.Equal(t, 3, r.StagingBlobsDeleted)
		require.Equal(t, 1, r.UploadsAborted)
		_, err = os.Stat(filepath.Join(staging, "put-crashed"))
		require.ErrorIs(t, err, os.ErrNotExist, "a partial write older than the staging age is swept")
		_, err = os.Stat(filepath.Join(staging, "put-live"))
		require.NoError(t, err, "a write in progress is kept")
		require.False(t, f.exists("tmp/old"))
		require.True(t, f.exists("tmp/new"))
		require.False(t, f.exists("exports/old.zip"))
		require.True(t, f.exists("avatars/keep"))
		_, err = f.store.ListParts(ctx, "lfs/sha256/ab/an/abandoned", abandoned)
		require.ErrorIs(t, err, blob.ErrNotFound)
		_, err = f.store.ListParts(ctx, "lfs/sha256/tr/ac/tracked", tracked)
		require.NoError(t, err, "an upload with a row is left to upload.expire")
	})
}

func TestExpireUploads(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		ctx := context.Background()
		f := newFixture(t, d)
		past, future := f.now.Add(-time.Minute), f.now.Add(time.Hour)

		mpKey := "lfs/sha256/mp/expired"
		mpID, err := f.store.CreateMultipart(ctx, mpKey)
		require.NoError(t, err)
		mpRow := f.upload("mp", mpKey, mpID, past)

		unverifiedKey := model.LFSObjectKey(strings.Repeat("1a", 32))
		f.blob(unverifiedKey, past)
		putRow := f.upload(strings.Repeat("1a", 32), unverifiedKey, "", past)

		verifiedOID, verifiedKey := f.object("already verified", past)
		dupRow := f.upload(verifiedOID, verifiedKey, "", past)

		liveRow := f.upload("live", "lfs/sha256/li/ve/live", "", future)
		goneRow := f.upload("gone", "lfs/sha256/go/ne/gone", "not-a-real-upload", past)

		r, err := f.collector().ExpireUploads(ctx)
		require.NoError(t, err)
		require.Equal(t, 4, r.UploadsExpired)
		require.Equal(t, 1, r.BlobsDeleted)
		require.Zero(t, r.Errors)
		_, err = f.store.ListParts(ctx, mpKey, mpID)
		require.ErrorIs(t, err, blob.ErrNotFound)
		require.False(t, f.exists(unverifiedKey))
		require.True(t, f.exists(verifiedKey), "a verified object with the same key is kept")
		for id, want := range map[uuid.UUID]bool{mpRow: false, putRow: false, dupRow: false, goneRow: false, liveRow: true} {
			ok, err := d.NewSelect().Model((*db.Upload)(nil)).Where("id = ?", id).Exists(ctx)
			require.NoError(t, err)
			require.Equal(t, want, ok)
		}
	})
}

// TestGCKeepsStagedObjectsOfLiveUploads: a multipart upload's assembled
// object and a presigned PUT's staged object live under tmp/uploads/ for
// as long as their upload row does, which can be longer than the staging
// age. The staging sweep leaves them to upload.expire, and still removes
// old staging that no upload names.
func TestGCKeepsStagedObjectsOfLiveUploads(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		ctx := context.Background()
		f := newFixture(t, d)
		old := f.now.Add(-48 * time.Hour)
		assembled := "tmp/uploads/" + db.NewID().String()
		staged := "tmp/uploads/" + db.NewID().String()
		leftover := "tmp/uploads/" + db.NewID().String()
		for _, k := range []string{assembled, staged, leftover} {
			f.blob(k, old)
		}
		f.upload(strings.Repeat("a", 64), assembled, "multipart-1", f.now.Add(time.Hour))
		f.upload(strings.Repeat("b", 64), staged, "", f.now.Add(time.Hour))

		r, err := f.collector().Run(ctx, gc.ScopeAll, nil)
		require.NoError(t, err)
		require.Zero(t, r.Errors)
		require.Equal(t, 1, r.StagingBlobsDeleted)
		require.True(t, f.exists(assembled), "an assembled multipart upload a live upload still completes is kept")
		require.True(t, f.exists(staged), "a presigned PUT's staged object is kept until it is verified")
		require.False(t, f.exists(leftover), "staging that no upload names is swept")
	})
}

func TestGCScopeAndErrors(t *testing.T) {
	d := dbtest.SQLite(t)
	ctx := context.Background()
	f := newFixture(t, d)
	old := f.now.Add(-30 * 24 * time.Hour)
	garbage, _ := f.object("garbage", old)
	_, err := f.collector().Run(ctx, gc.ScopeGit, nil)
	require.NoError(t, err)
	require.True(t, f.hasRow(garbage), "a git run leaves LFS objects alone")
	_, err = f.collector().Run(ctx, "everything", nil)
	require.True(t, jobs.IsPermanent(err))
}

func TestGCJobsRegistrationAndMetrics(t *testing.T) {
	d := dbtest.SQLite(t)
	ctx := context.Background()
	f := newFixture(t, d)
	old := f.now.Add(-30 * 24 * time.Hour)
	f.object("garbage", old)
	f.object("kept", f.now)

	reader := sdkmetric.NewManualReader()
	inst, err := telemetry.NewInstruments(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("test"))
	require.NoError(t, err)
	c := gc.New(d, f.store, gc.Options{Now: func() time.Time { return f.now }, Instrument: inst})

	reg := jobs.NewRegistry()
	q := jobstest.NewFake(reg)
	require.NoError(t, gc.Register(ctx, reg, q, c, "0 3 * * *"))
	require.Error(t, gc.Register(ctx, reg, q, c, "0 3 * * *"), "handlers register once")
	sch := q.Schedules()
	require.Len(t, sch, 2)
	require.Equal(t, jobs.KindGC, sch[0].Kind)
	require.Equal(t, "0 3 * * *", sch[0].Cron)
	require.Equal(t, jobs.KindUploadExpire, sch[1].Kind)

	user := uuid.New()
	id, err := gc.Start(ctx, q, "", user)
	require.NoError(t, err)
	again, err := gc.Start(ctx, q, gc.ScopeAll, user)
	require.NoError(t, err)
	require.Equal(t, id, again, "a second request joins the pending run")
	expire, err := q.Enqueue(ctx, jobs.NewJob{Kind: jobs.KindUploadExpire})
	require.NoError(t, err)
	q.RunPending(ctx)
	j, err := q.Get(ctx, id)
	require.NoError(t, err)
	require.Equal(t, jobs.StatusSucceeded, j.Status)
	require.Contains(t, string(j.Result), `"lfs_objects_deleted":1`)
	require.Contains(t, string(j.Result), `"lfs_bytes":4`)
	ej, err := q.Get(ctx, expire)
	require.NoError(t, err)
	require.Equal(t, jobs.StatusSucceeded, ej.Status)

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(ctx, &rm))
	got := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == "hangar.storage.bytes" {
				for _, dp := range m.Data.(metricdata.Gauge[int64]).DataPoints {
					k, _ := dp.Attributes.Value(telemetry.MetricKind)
					got[k.AsString()] = dp.Value
				}
			}
		}
	}
	require.Equal(t, map[string]int64{"lfs": 4, "git": 0}, got)

	bad, err := q.Enqueue(ctx, jobs.NewJob{Kind: jobs.KindGC, Payload: map[string]string{"scope": "nonsense"}})
	require.NoError(t, err)
	q.RunPending(ctx)
	bj, err := q.Get(ctx, bad)
	require.NoError(t, err)
	require.Equal(t, jobs.StatusFailed, bj.Status)
}
