package gc

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jamesagarside/hangar/internal/blob"
	"github.com/jamesagarside/hangar/internal/db"
	"github.com/jamesagarside/hangar/internal/jobs"
)

// Schedule keys of the recurring Jobs this package owns.
const (
	ScheduleKeyGC           = "gc"
	ScheduleKeyUploadExpire = "upload.expire"
	// UploadExpireCron runs upload.expire every hour.
	UploadExpireCron = "17 * * * *"
)

// Handler returns the gc Job handler.
func (c *Collector) Handler() jobs.Handler {
	return func(ctx context.Context, j *jobs.Job, p jobs.Progress) error {
		var payload Payload
		if len(j.Payload) > 0 {
			if err := j.DecodePayload(&payload); err != nil {
				return jobs.Permanent(err)
			}
		}
		r, err := c.Run(ctx, payload.Scope, p)
		if err != nil {
			return err
		}
		return p.SetResult(ctx, r)
	}
}

// ExpireReport is the result of an upload.expire run.
type ExpireReport struct {
	UploadsExpired int `json:"uploads_expired"`
	BlobsDeleted   int `json:"blobs_deleted"`
	Errors         int `json:"errors"`
}

// ExpireUploads aborts uploads whose expires_at has passed and deletes
// their rows. A multipart upload is aborted; an object written straight to
// its key by a presigned PUT and never verified is deleted, unless a
// verified object with that key exists.
func (c *Collector) ExpireUploads(ctx context.Context) (ExpireReport, error) {
	var r ExpireReport
	now := c.now()
	for {
		var batch []db.Upload
		if err := c.db.NewSelect().Model(&batch).Where("expires_at < ?", now).OrderExpr("expires_at").Limit(batchSize).Scan(ctx); err != nil {
			return r, err
		}
		if len(batch) == 0 {
			return r, nil
		}
		for _, u := range batch {
			if err := c.expireUpload(ctx, u, &r); err != nil {
				r.Errors++
				c.opts.Logger.WarnContext(ctx, "expire upload", "upload", u.ID, "error", err)
				continue
			}
		}
		if len(batch) < batchSize || r.Errors >= len(batch) {
			return r, nil
		}
	}
}

func (c *Collector) expireUpload(ctx context.Context, u db.Upload, r *ExpireReport) error {
	if u.UploadID != "" {
		if err := c.store.AbortMultipart(ctx, u.StorageKey, u.UploadID); err != nil && !errors.Is(err, blob.ErrNotFound) {
			return err
		}
	} else if u.StorageKey != "" {
		verified, err := c.db.NewSelect().Model((*db.LFSObject)(nil)).
			Where("storage_key = ? AND verified_at IS NOT NULL", u.StorageKey).Exists(ctx)
		if err != nil {
			return err
		}
		if !verified {
			if _, err := c.store.Stat(ctx, u.StorageKey); err == nil {
				if err := c.store.Delete(ctx, u.StorageKey); err != nil {
					return err
				}
				r.BlobsDeleted++
			} else if !errors.Is(err, blob.ErrNotFound) {
				return err
			}
		}
	}
	res, err := c.db.NewDelete().Model((*db.Upload)(nil)).Where("id = ? AND expires_at < ?", u.ID, c.now()).Exec(ctx)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		r.UploadsExpired++
	}
	return nil
}

// ExpireHandler returns the upload.expire Job handler.
func (c *Collector) ExpireHandler() jobs.Handler {
	return func(ctx context.Context, _ *jobs.Job, p jobs.Progress) error {
		r, err := c.ExpireUploads(ctx)
		if err != nil {
			return err
		}
		return p.SetResult(ctx, r)
	}
}

// Register adds the gc and upload.expire handlers to reg and creates their
// schedules: gc on gcCron (gc.schedule) and upload.expire hourly. Running
// it on every start is safe.
func Register(ctx context.Context, reg *jobs.Registry, q jobs.Queue, c *Collector, gcCron string) error {
	if err := reg.Register(jobs.KindGC, c.Handler(), jobs.HandlerOptions{MaxAttempts: 3, Concurrency: 1, Timeout: 6 * time.Hour}); err != nil {
		return err
	}
	if err := reg.Register(jobs.KindUploadExpire, c.ExpireHandler(), jobs.HandlerOptions{MaxAttempts: 3, Concurrency: 1, Timeout: time.Hour}); err != nil {
		return err
	}
	if err := q.SetSchedule(ctx, jobs.Schedule{Kind: jobs.KindGC, Key: ScheduleKeyGC, Cron: gcCron, Payload: Payload{Scope: ScopeAll}, Enabled: true}); err != nil {
		return fmt.Errorf("schedule gc: %w", err)
	}
	if err := q.SetSchedule(ctx, jobs.Schedule{Kind: jobs.KindUploadExpire, Key: ScheduleKeyUploadExpire, Cron: UploadExpireCron, Enabled: true}); err != nil {
		return fmt.Errorf("schedule upload.expire: %w", err)
	}
	return nil
}

// Start enqueues a gc run now and returns its Job ID. A run that is
// already pending or running is joined.
func Start(ctx context.Context, q jobs.Queue, scope string, createdBy jobs.JobID) (jobs.JobID, error) {
	if scope == "" {
		scope = ScopeAll
	}
	// A full run joins the scheduled one if it is pending or running.
	key := "gc:" + scope
	if scope == ScopeAll {
		key = "schedule:" + ScheduleKeyGC
	}
	return q.Enqueue(ctx, jobs.NewJob{Kind: jobs.KindGC, DedupeKey: key, Payload: Payload{Scope: scope}, CreatedBy: createdBy, Priority: jobs.PriorityHigh})
}
