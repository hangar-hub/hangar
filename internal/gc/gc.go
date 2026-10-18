// Package gc is garbage collection and storage accounting (OPS-4): the gc
// Job that deletes unreferenced LFS objects, retired and orphaned git packs
// and stale staging data; the upload.expire Job that aborts expired
// uploads; storage usage by Namespace; and per-Namespace quotas with an
// enforcement helper for pushes, Commits and uploads.
//
// GC never deletes an LFS object that a Repo references (a repo_lfs row,
// including Partial mirror rows that are not present) or that an upload in
// progress references (an uploads row), and never deletes anything newer
// than its grace period.
//
// # Referencing protocol
//
// Writers that make a Repo reference an existing LFS object must, in the
// transaction that inserts the repo_lfs row, update that object's
// lfs_objects.last_referenced_at and check the update touched one row. GC
// deletes an lfs_objects row only with a conditional DELETE that re-checks
// every condition, so either GC removes the row first and the writer sees
// the object is missing (and stores it again), or the writer's row exists
// and GC keeps the object. The blob is deleted only after its row is gone.
package gc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/hangar-hub/hangar/internal/blob"
	"github.com/hangar-hub/hangar/internal/db"
	"github.com/hangar-hub/hangar/internal/jobs"
	"github.com/hangar-hub/hangar/internal/telemetry"
)

// Scopes of a gc run.
const (
	ScopeAll = "all"
	ScopeLFS = "lfs"
	ScopeGit = "git"
)

// Payload is the gc Job's payload.
type Payload struct {
	// Scope is all (the default), lfs or git (`hangar admin gc --git`).
	Scope string `json:"scope,omitempty"`
}

// Options configure a Collector. Zero values take the defaults.
type Options struct {
	// GracePeriod is how long an unreferenced LFS object, or an orphaned
	// blob, is kept (gc.grace_period). Default 168 hours.
	GracePeriod time.Duration
	// PackGracePeriod is how long retired and orphaned packs are kept
	// (git.pack_grace_period). Default one hour.
	PackGracePeriod time.Duration
	// PendingPackAge is when a pack still pending belongs to a push that
	// died. Default git.timeout plus the pack grace period: two hours.
	PendingPackAge time.Duration
	// StagingAge is how long tmp/ and exports/ objects are kept. Default 24 hours.
	StagingAge time.Duration
	// AbandonedUploadAge is when an incomplete multipart upload with no
	// upload row is aborted. Default 48 hours.
	AbandonedUploadAge time.Duration
	// Now is the clock. Nil means time.Now.
	Now        func() time.Time
	Logger     *slog.Logger
	Instrument *telemetry.Instruments
}

func (o *Options) defaults() {
	if o.GracePeriod <= 0 {
		o.GracePeriod = 168 * time.Hour
	}
	if o.PackGracePeriod <= 0 {
		o.PackGracePeriod = time.Hour
	}
	if o.PendingPackAge <= 0 {
		o.PendingPackAge = time.Hour + o.PackGracePeriod
	}
	if o.StagingAge <= 0 {
		o.StagingAge = 24 * time.Hour
	}
	if o.AbandonedUploadAge <= 0 {
		o.AbandonedUploadAge = 48 * time.Hour
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
}

// Collector runs garbage collection.
type Collector struct {
	db    *db.DB
	store blob.Store
	opts  Options
}

// New returns a Collector over the database and Blob store.
func New(d *db.DB, s blob.Store, opts Options) *Collector {
	opts.defaults()
	return &Collector{db: d, store: s, opts: opts}
}

// Report is the result of a gc run, stored as the Job's result.
type Report struct {
	Scope                 string `json:"scope"`
	LFSObjectsDeleted     int    `json:"lfs_objects_deleted"`
	LFSBytesFreed         int64  `json:"lfs_bytes_freed"`
	OrphanLFSBlobsDeleted int    `json:"orphan_lfs_blobs_deleted"`
	PacksDeleted          int    `json:"packs_deleted"`
	PackBytesFreed        int64  `json:"pack_bytes_freed"`
	OrphanGitBlobsDeleted int    `json:"orphan_git_blobs_deleted"`
	StagingBlobsDeleted   int    `json:"staging_blobs_deleted"`
	UploadsAborted        int    `json:"uploads_aborted"`
	Errors                int    `json:"errors"`
	LFSBytes              int64  `json:"lfs_bytes"`
	GitBytes              int64  `json:"git_bytes"`
}

// batchSize bounds every query and every in-memory list of a run.
const batchSize = 500

func (c *Collector) now() time.Time { return c.opts.Now().UTC().Truncate(time.Microsecond) }

// Run collects garbage in scope and reports progress through p, which may
// be nil.
func (c *Collector) Run(ctx context.Context, scope string, p jobs.Progress) (Report, error) {
	if scope == "" {
		scope = ScopeAll
	}
	if scope != ScopeAll && scope != ScopeLFS && scope != ScopeGit {
		return Report{}, jobs.Permanent(fmt.Errorf("unknown gc scope %q: use all, lfs or git", scope))
	}
	r := Report{Scope: scope}
	steps := []struct {
		name  string
		scope string
		run   func(context.Context, *Report) error
	}{
		{"Deleting unreferenced LFS objects", ScopeLFS, c.collectLFSObjects},
		{"Deleting orphaned LFS blobs", ScopeLFS, c.collectOrphanLFSBlobs},
		{"Deleting retired git packs", ScopeGit, c.collectPacks},
		{"Deleting orphaned git data", ScopeGit, c.collectOrphanGitBlobs},
		{"Deleting stale staging data", ScopeAll, c.collectStaging},
		{"Aborting abandoned uploads", ScopeAll, c.collectAbandonedUploads},
	}
	for i, st := range steps {
		if scope != ScopeAll && st.scope != scope {
			continue
		}
		if p != nil {
			_ = p.Report(ctx, jobs.ProgressUpdate{FilesDone: i, FilesTotal: len(steps), Message: st.name})
		}
		if err := st.run(ctx, &r); err != nil {
			return r, fmt.Errorf("%s: %w", strings.ToLower(st.name), err)
		}
	}
	totals, err := c.totals(ctx)
	if err != nil {
		return r, err
	}
	r.LFSBytes, r.GitBytes = totals.lfs, totals.git
	if inst := c.opts.Instrument; inst != nil {
		inst.StorageBytes.Record(ctx, totals.lfs, metric.WithAttributes(attribute.String(telemetry.MetricKind, "lfs")))
		inst.StorageBytes.Record(ctx, totals.git, metric.WithAttributes(attribute.String(telemetry.MetricKind, "git")))
	}
	if p != nil {
		_ = p.Report(ctx, jobs.ProgressUpdate{FilesDone: len(steps), FilesTotal: len(steps), Message: "Done"})
	}
	c.opts.Logger.InfoContext(ctx, "garbage collection finished", "scope", scope,
		"lfs_objects_deleted", r.LFSObjectsDeleted, "lfs_bytes_freed", r.LFSBytesFreed,
		"orphan_lfs_blobs_deleted", r.OrphanLFSBlobsDeleted, "packs_deleted", r.PacksDeleted,
		"pack_bytes_freed", r.PackBytesFreed, "orphan_git_blobs_deleted", r.OrphanGitBlobsDeleted,
		"staging_blobs_deleted", r.StagingBlobsDeleted, "uploads_aborted", r.UploadsAborted, "errors", r.Errors)
	return r, nil
}

// unreferenced is the condition shared by the candidate query and the
// conditional delete: no Repo and no upload refers to the object, and it
// has not been referenced within the grace period.
const unreferenced = `?TableAlias.last_referenced_at < ?
	AND NOT EXISTS (SELECT 1 FROM repo_lfs r WHERE r.oid = ?TableAlias.oid)
	AND NOT EXISTS (SELECT 1 FROM uploads u WHERE u.oid = ?TableAlias.oid)`

// collectLFSObjects deletes LFS objects that no Repo or upload references.
func (c *Collector) collectLFSObjects(ctx context.Context, r *Report) error {
	cutoff := c.now().Add(-c.opts.GracePeriod)
	after := ""
	for {
		var batch []db.LFSObject
		if err := c.db.NewSelect().Model(&batch).Column("oid", "size", "storage_key").
			Where("oid > ?", after).Where(unreferenced, cutoff).
			OrderExpr("oid").Limit(batchSize).Scan(ctx); err != nil {
			return err
		}
		if len(batch) == 0 {
			return nil
		}
		for _, o := range batch {
			after = o.OID
			res, err := c.db.NewDelete().Model((*db.LFSObject)(nil)).Where("oid = ?", o.OID).Where(unreferenced, cutoff).Exec(ctx)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n != 1 {
				continue // Referenced again since the query.
			}
			r.LFSObjectsDeleted++
			r.LFSBytesFreed += o.Size
			if err := c.store.Delete(ctx, o.StorageKey); err != nil {
				// The row is gone; the orphan sweep removes the blob later.
				r.Errors++
				c.opts.Logger.WarnContext(ctx, "delete LFS object blob", "oid", o.OID, "error", err)
			}
		}
		if len(batch) < batchSize {
			return nil
		}
	}
}

// sweepOld lists the objects under prefix older than cutoff in batches,
// asks orphans which keys of each batch to delete, and deletes them before
// the listing goes on. Every run walks the whole prefix and holds at most
// one batch in memory, so no number of referenced objects that sort first
// can stop a run from reaching an orphan. Deleting keys the listing has
// already returned is safe on both backends: the filesystem walk reads each
// directory before visiting it and skips entries that disappear, and an S3
// listing continues after the last key it returned. It returns how many
// keys it deleted and how many deletes failed.
func (c *Collector) sweepOld(ctx context.Context, prefix string, cutoff time.Time, orphans func([]blob.Info) ([]string, error)) (deleted, failed int, err error) {
	var batch []blob.Info
	flush := func() error {
		doomed, err := orphans(batch)
		if err != nil {
			return err
		}
		batch = batch[:0]
		for _, k := range doomed {
			if err := c.store.Delete(ctx, k); err != nil {
				failed++
				c.opts.Logger.WarnContext(ctx, "delete orphaned blob", "prefix", prefix, "error", err)
				continue
			}
			deleted++
		}
		return nil
	}
	err = c.store.List(ctx, prefix, func(i blob.Info) error {
		if !i.ModTime.Before(cutoff) {
			return nil
		}
		batch = append(batch, i)
		if len(batch) == batchSize {
			return flush()
		}
		return nil
	})
	if err == nil && len(batch) > 0 {
		err = flush()
	}
	return deleted, failed, err
}

// collectOrphanLFSBlobs deletes blobs under lfs/ that have no lfs_objects
// row and no upload, such as leftovers of failed deletes.
func (c *Collector) collectOrphanLFSBlobs(ctx context.Context, r *Report) error {
	cutoff := c.now().Add(-c.opts.GracePeriod)
	deleted, failed, err := c.sweepOld(ctx, blob.PrefixLFS, cutoff, func(batch []blob.Info) ([]string, error) {
		keys := make([]string, len(batch))
		oids := make([]string, len(batch))
		for i, info := range batch {
			keys[i] = info.Key
			oids[i] = path.Base(info.Key)
		}
		var objs []db.LFSObject
		if err := c.db.NewSelect().Model(&objs).Column("oid", "storage_key").
			Where("storage_key IN (?) OR oid IN (?)", bun.List(keys), bun.List(oids)).Scan(ctx); err != nil {
			return nil, err
		}
		var ups []db.Upload
		if err := c.db.NewSelect().Model(&ups).Column("oid", "storage_key").
			Where("storage_key IN (?) OR oid IN (?)", bun.List(keys), bun.List(oids)).Scan(ctx); err != nil {
			return nil, err
		}
		keep := map[string]bool{}
		for _, o := range objs {
			keep[o.OID], keep[o.StorageKey] = true, true
		}
		for _, u := range ups {
			keep[u.OID], keep[u.StorageKey] = true, true
		}
		var doomed []string
		for i, k := range keys {
			if !keep[k] && !keep[oids[i]] {
				doomed = append(doomed, k)
			}
		}
		return doomed, nil
	})
	r.OrphanLFSBlobsDeleted += deleted
	r.Errors += failed
	return err
}

func set(lists ...[]string) map[string]bool {
	m := map[string]bool{}
	for _, l := range lists {
		for _, v := range l {
			m[v] = true
		}
	}
	return m
}

// PackPrefix returns the key prefix of a pack's files
// (git/<repo-uuid>/pack/pack-<sha1>.*, docs/spec/04-git.md).
func PackPrefix(repoID uuid.UUID, packSHA1 string) string {
	return blob.PrefixGit + repoID.String() + "/pack/pack-" + packSHA1 + "."
}

// collectPacks deletes retired and orphaned packs after the pack grace
// period, and pending packs of pushes that died.
func (c *Collector) collectPacks(ctx context.Context, r *Report) error {
	now := c.now()
	retiredCutoff := now.Add(-c.opts.PackGracePeriod)
	pendingCutoff := now.Add(-c.opts.PendingPackAge)
	const cond = `((state IN ('retired', 'orphaned') AND COALESCE(retired_at, updated_at) < ?) OR (state = 'pending' AND created_at < ?))`
	for {
		var batch []db.Pack
		if err := c.db.NewSelect().Model(&batch).Column("repo_id", "pack_sha1", "size", "state").
			Where(cond, retiredCutoff, pendingCutoff).Limit(batchSize).Scan(ctx); err != nil {
			return err
		}
		if len(batch) == 0 {
			return nil
		}
		progressed := false
		for _, p := range batch {
			res, err := c.db.NewDelete().Model((*db.Pack)(nil)).
				Where("repo_id = ? AND pack_sha1 = ? AND state = ?", p.RepoID, p.PackSHA1, p.State).
				Where(cond, retiredCutoff, pendingCutoff).Exec(ctx)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n != 1 {
				continue
			}
			progressed = true
			r.PacksDeleted++
			r.PackBytesFreed += p.Size
			if err := c.deletePrefix(ctx, PackPrefix(p.RepoID, p.PackSHA1)); err != nil {
				r.Errors++
				c.opts.Logger.WarnContext(ctx, "delete pack files", "repo_id", p.RepoID, "pack", p.PackSHA1, "error", err)
			}
		}
		if !progressed || len(batch) < batchSize {
			return nil
		}
	}
}

func (c *Collector) deletePrefix(ctx context.Context, prefix string) error {
	var keys []string
	if err := c.store.List(ctx, prefix, func(i blob.Info) error { keys = append(keys, i.Key); return nil }); err != nil {
		return err
	}
	var errs []error
	for _, k := range keys {
		if err := c.store.Delete(ctx, k); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// collectOrphanGitBlobs deletes git data of Repos that no longer exist,
// and pack files with no packs row, once they are older than the grace
// period.
func (c *Collector) collectOrphanGitBlobs(ctx context.Context, r *Report) error {
	cutoff := c.now().Add(-c.opts.GracePeriod)
	deleted, failed, err := c.sweepOld(ctx, blob.PrefixGit, cutoff, func(batch []blob.Info) ([]string, error) {
		repoIDs := map[uuid.UUID]bool{}
		type packRef struct {
			repo uuid.UUID
			sha  string
		}
		refs := make([]*packRef, len(batch))
		for i, info := range batch {
			parts := strings.Split(strings.TrimPrefix(info.Key, blob.PrefixGit), "/")
			id, err := uuid.Parse(parts[0])
			if err != nil {
				continue // Not a Repo's git data; leave it alone.
			}
			repoIDs[id] = true
			if len(parts) == 3 && parts[1] == "pack" && strings.HasPrefix(parts[2], "pack-") {
				sha, _, _ := strings.Cut(strings.TrimPrefix(parts[2], "pack-"), ".")
				refs[i] = &packRef{repo: id, sha: sha}
			}
		}
		ids := make([]uuid.UUID, 0, len(repoIDs))
		for id := range repoIDs {
			ids = append(ids, id)
		}
		existing := map[uuid.UUID]bool{}
		if len(ids) > 0 {
			var found []uuid.UUID
			if err := c.db.NewSelect().Model((*db.Repo)(nil)).Column("id").Where("id IN (?)", bun.List(ids)).Scan(ctx, &found); err != nil {
				return nil, err
			}
			for _, id := range found {
				existing[id] = true
			}
		}
		var packs []db.Pack
		if len(ids) > 0 {
			if err := c.db.NewSelect().Model(&packs).Column("repo_id", "pack_sha1").Where("repo_id IN (?)", bun.List(ids)).Scan(ctx); err != nil {
				return nil, err
			}
		}
		known := map[string]bool{}
		var doomed []string
		for _, p := range packs {
			known[p.RepoID.String()+"/"+p.PackSHA1] = true
		}
		for i, info := range batch {
			parts := strings.SplitN(strings.TrimPrefix(info.Key, blob.PrefixGit), "/", 2)
			id, err := uuid.Parse(parts[0])
			if err != nil {
				continue
			}
			switch {
			case !existing[id]:
				doomed = append(doomed, info.Key)
			case refs[i] != nil && !known[refs[i].repo.String()+"/"+refs[i].sha]:
				doomed = append(doomed, info.Key)
			}
		}
		return doomed, nil
	})
	r.OrphanGitBlobsDeleted += deleted
	r.Errors += failed
	return err
}

// collectStaging deletes streaming staging (tmp/) and generated zips
// (exports/) older than the staging age, and staging files outside the key
// space that a crash or a killed process left behind, such as the
// filesystem backend's partial writes.
func (c *Collector) collectStaging(ctx context.Context, r *Report) error {
	cutoff := c.now().Add(-c.opts.StagingAge)
	all := func(batch []blob.Info) ([]string, error) {
		keys := make([]string, len(batch))
		for i, info := range batch {
			keys[i] = info.Key
		}
		return keys, nil
	}
	for _, prefix := range []string{blob.PrefixTmp, blob.PrefixExports} {
		deleted, failed, err := c.sweepOld(ctx, prefix, cutoff, all)
		r.StagingBlobsDeleted += deleted
		r.Errors += failed
		if err != nil {
			return err
		}
	}
	if sw, ok := c.store.(blob.StagingSweeper); ok {
		n, err := sw.SweepStaging(ctx, cutoff)
		r.StagingBlobsDeleted += n
		if err != nil && !errors.Is(err, blob.ErrNotSupported) {
			return err
		}
	}
	return nil
}

// collectAbandonedUploads aborts incomplete multipart uploads that no
// upload row refers to, once they are older than AbandonedUploadAge. It
// checks and aborts them in batches while the listing goes on, so memory
// stays bounded however many there are. Stores that cannot list uploads
// rely on upload.expire and their own expiry.
func (c *Collector) collectAbandonedUploads(ctx context.Context, r *Report) error {
	ul, ok := c.store.(blob.UploadLister)
	if !ok {
		return nil
	}
	cutoff := c.now().Add(-c.opts.AbandonedUploadAge)
	var batch []blob.IncompleteUpload
	flush := func() error {
		ids := make([]string, len(batch))
		for i, u := range batch {
			ids[i] = u.UploadID
		}
		var live []string
		if err := c.db.NewSelect().Model((*db.Upload)(nil)).Column("upload_id").Where("upload_id IN (?)", bun.List(ids)).Scan(ctx, &live); err != nil {
			return err
		}
		keep := set(live)
		for _, u := range batch {
			if keep[u.UploadID] {
				continue
			}
			if err := c.store.AbortMultipart(ctx, u.Key, u.UploadID); err != nil && !errors.Is(err, blob.ErrNotFound) {
				r.Errors++
				continue
			}
			r.UploadsAborted++
		}
		batch = batch[:0]
		return nil
	}
	err := ul.ListUploads(ctx, func(u blob.IncompleteUpload) error {
		if !u.Initiated.Before(cutoff) {
			return nil
		}
		batch = append(batch, u)
		if len(batch) == batchSize {
			return flush()
		}
		return nil
	})
	if errors.Is(err, blob.ErrNotSupported) {
		return nil
	}
	if err == nil && len(batch) > 0 {
		err = flush()
	}
	return err
}

type totals struct{ lfs, git int64 }

func (c *Collector) totals(ctx context.Context) (totals, error) {
	var t totals
	if err := c.db.NewSelect().Model((*db.LFSObject)(nil)).ColumnExpr("COALESCE(SUM(size), 0)").Scan(ctx, &t.lfs); err != nil {
		return t, err
	}
	if err := c.db.NewSelect().Model((*db.Pack)(nil)).ColumnExpr("COALESCE(SUM(size), 0)").Scan(ctx, &t.git); err != nil {
		return t, err
	}
	return t, nil
}
