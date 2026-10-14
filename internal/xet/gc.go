package xet

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/uptrace/bun"

	"github.com/jamesagarside/hangar/internal/blob"
	"github.com/jamesagarside/hangar/internal/db"
	"github.com/jamesagarside/hangar/internal/jobs"
)

// OrphanGrace is how old a stored xorb with no xet_xorbs row must be before
// GC deletes it. A xorb upload stores the object first and records the row
// just after, so a young object without a row is still being uploaded.
const OrphanGrace = time.Hour

// gcBatch is the number of rows GC handles per query.
const gcBatch = 200

// GCResult is what a xet.gc run did.
type GCResult struct {
	// XorbsDeleted counts xorbs past xet.xorb_retention that were removed.
	XorbsDeleted int64 `json:"xorbs_deleted"`
	// BytesFreed is the stored size of those xorbs.
	BytesFreed int64 `json:"bytes_freed"`
	// OrphansDeleted counts stored xorbs that had no row.
	OrphansDeleted int64 `json:"orphans_deleted"`
	// FilesPruned counts xet_files rows whose LFS object no longer exists.
	FilesPruned int64 `json:"files_pruned"`
}

// CollectGarbage deletes xorbs whose last reference is older than
// xet.xorb_retention (03-hf-api.md "Keeping xorbs"), stored xorbs that have
// no row, and xet_files rows whose LFS object GC has removed. report, which
// may be nil, is called after each step.
func (s *Service) CollectGarbage(ctx context.Context, report func(GCResult)) (GCResult, error) {
	var res GCResult
	runStart := s.clock()
	step := func() {
		if report != nil {
			report(res)
		}
	}
	if err := s.expireXorbs(ctx, runStart.Add(-s.retention), &res, step); err != nil {
		return res, err
	}
	if err := s.sweepOrphans(ctx, runStart.Add(-OrphanGrace), &res); err != nil {
		return res, err
	}
	step()
	if err := s.pruneFiles(ctx, runStart, &res); err != nil {
		return res, err
	}
	step()
	s.log.InfoContext(ctx, "Xet garbage collection finished", "xorbs_deleted", res.XorbsDeleted,
		"bytes_freed", res.BytesFreed, "orphans_deleted", res.OrphansDeleted, "files_pruned", res.FilesPruned)
	return res, nil
}

// expireXorbs deletes expired xorbs one at a time. Each deletion runs in a
// transaction that deletes the row only if it is still expired, then the
// object, then commits. A concurrent upload or shard that touches the xorb
// waits for the row lock and then finds no row, so it stores the xorb again
// (an upload) or reports it as unknown (a shard); it never sees a row whose
// object is gone.
func (s *Service) expireXorbs(ctx context.Context, cutoff time.Time, res *GCResult, step func()) error {
	for {
		var rows []db.XetXorb
		if err := s.db.NewSelect().Model(&rows).
			Where("last_referenced_at < ?", cutoff).
			OrderExpr("last_referenced_at ASC").
			Limit(gcBatch).
			Scan(ctx); err != nil {
			return fmt.Errorf("xet: list expired xorbs: %w", err)
		}
		for _, row := range rows {
			if err := ctx.Err(); err != nil {
				return err
			}
			err := s.db.InTx(ctx, func(ctx context.Context, tx bun.Tx) error {
				r, err := tx.NewDelete().Model((*db.XetXorb)(nil)).
					Where("hash = ?", row.Hash).
					Where("last_referenced_at < ?", cutoff).
					Exec(ctx)
				if err != nil {
					return err
				}
				if n, _ := r.RowsAffected(); n != 1 {
					return nil
				}
				if err := s.blobs.Delete(ctx, row.StorageKey); err != nil {
					return err
				}
				res.XorbsDeleted++
				res.BytesFreed += row.Size
				return nil
			})
			if err != nil {
				return fmt.Errorf("xet: delete xorb %s: %w", row.Hash, err)
			}
		}
		step()
		if len(rows) < gcBatch {
			return nil
		}
	}
}

// sweepOrphans deletes stored xorbs that have no row and are older than
// before, such as an object stored by an upload that failed to record it.
func (s *Service) sweepOrphans(ctx context.Context, before time.Time, res *GCResult) error {
	var candidates []blob.Info
	if err := s.blobs.List(ctx, blob.PrefixXorbs, func(i blob.Info) error {
		if i.ModTime.Before(before) {
			candidates = append(candidates, i)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("xet: list stored xorbs: %w", err)
	}
	for i := 0; i < len(candidates); i += touchBatch {
		batch := candidates[i:min(i+touchBatch, len(candidates))]
		names := make([]string, len(batch))
		for j, info := range batch {
			names[j] = strings.TrimPrefix(info.Key, blob.PrefixXorbs)
		}
		var known []string
		if err := s.db.NewSelect().Model((*db.XetXorb)(nil)).Column("hash").
			Where("hash IN (?)", bun.List(names)).Scan(ctx, &known); err != nil {
			return fmt.Errorf("xet: look up xorbs: %w", err)
		}
		have := make(map[string]bool, len(known))
		for _, h := range known {
			have[h] = true
		}
		for j, info := range batch {
			if have[names[j]] {
				continue
			}
			// Re-check just before deleting: an upload in between replaces
			// the object, which makes it young again.
			cur, err := s.blobs.Stat(ctx, info.Key)
			if err != nil || !cur.ModTime.Before(before) {
				continue
			}
			if err := s.blobs.Delete(ctx, info.Key); err != nil {
				return fmt.Errorf("xet: delete orphaned xorb: %w", err)
			}
			res.OrphansDeleted++
		}
	}
	return nil
}

// pruneFiles deletes xet_files rows whose LFS object no longer exists.
// xet_files rows are not references, so LFS GC may remove an object that
// only they point to; the row then means nothing. Rows written after the run
// started are left alone.
func (s *Service) pruneFiles(ctx context.Context, runStart time.Time, res *GCResult) error {
	after := ""
	for {
		var rows []db.XetFile
		if err := s.db.NewSelect().Model(&rows).
			Where("xet_hash > ?", after).
			OrderExpr("xet_hash ASC").
			Limit(gcBatch).
			Scan(ctx); err != nil {
			return fmt.Errorf("xet: list files: %w", err)
		}
		if len(rows) == 0 {
			return nil
		}
		oids := make([]string, len(rows))
		for i, r := range rows {
			oids[i] = r.OID
		}
		present, err := s.lfs.StatMany(ctx, oids)
		if err != nil {
			return fmt.Errorf("xet: look up LFS objects: %w", err)
		}
		for _, r := range rows {
			if _, ok := present[r.OID]; ok {
				continue
			}
			n, err := s.db.NewDelete().Model((*db.XetFile)(nil)).
				Where("xet_hash = ?", r.XetHash).
				Where("updated_at < ?", runStart).
				Exec(ctx)
			if err != nil {
				return fmt.Errorf("xet: prune file: %w", err)
			}
			if c, _ := n.RowsAffected(); c == 1 {
				res.FilesPruned++
			}
		}
		after = rows[len(rows)-1].XetHash
		if len(rows) < gcBatch {
			return nil
		}
	}
}

// GCJobKey is the dedupe and schedule key of the xet.gc Job.
const GCJobKey = "xet"

// HandleGC is the jobs.Handler for xet.gc. It is idempotent.
func (s *Service) HandleGC(ctx context.Context, _ *jobs.Job, progress jobs.Progress) error {
	res, err := s.CollectGarbage(ctx, func(r GCResult) {
		_ = progress.Report(ctx, jobs.ProgressUpdate{Message: fmt.Sprintf(
			"Deleted %d expired xorbs and %d orphaned xorbs", r.XorbsDeleted, r.OrphansDeleted)})
	})
	if err != nil {
		return err
	}
	return progress.SetResult(ctx, res)
}

// RegisterJobs registers the xet.gc handler. One run at a time per worker
// is enough.
func (s *Service) RegisterJobs(reg *jobs.Registry) error {
	return reg.Register(jobs.KindXetGC, s.HandleGC, jobs.HandlerOptions{MaxAttempts: 3, Concurrency: 1})
}

// GCSchedule returns the recurring xet.gc Job, run on cron (gc.schedule).
func GCSchedule(cron string) jobs.Schedule {
	return jobs.Schedule{Kind: jobs.KindXetGC, Key: GCJobKey, Cron: cron, Enabled: true}
}
