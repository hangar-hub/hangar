package api

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/hangar-hub/hangar/internal/community"
	"github.com/hangar-hub/hangar/internal/index"
	"github.com/hangar-hub/hangar/internal/model"
	"github.com/hangar-hub/hangar/internal/org"
	"github.com/hangar-hub/hangar/internal/repo"
	"github.com/hangar-hub/hangar/internal/scan"
)

// repoPage holds what toRepo reads for a page of Repos, loaded with one
// query per kind of data instead of several queries per Repo. A nil map
// was not loaded, because the service cannot read many Repos at once or
// the read failed, and toRepo reads that data for each Repo instead.
type repoPage struct {
	// heads holds the default branch head of each Repo that has one.
	heads map[uuid.UUID]model.CommitSHA
	// summaries holds the index results of each indexed head.
	summaries map[uuid.UUID]*index.Summary
	// scans holds the scan status of each scanned head.
	scans            map[uuid.UUID]scan.Status
	likes, downloads map[uuid.UUID]int64
}

// loadPage reads the data toRepo needs for repos, and returns a context in
// which permission checks for p read p's Roles once for the page.
func (d *RepoDeps) loadPage(ctx context.Context, p model.Principal, repos []*repo.Repo) (context.Context, *repoPage) {
	pg := &repoPage{}
	if len(repos) == 0 {
		return ctx, pg
	}
	ids := make([]uuid.UUID, 0, len(repos))
	var live []*repo.Repo
	for _, r := range repos {
		ids = append(ids, r.UUID)
		if r.DeletedAt == nil {
			live = append(live, r)
		}
	}
	if rp, ok := d.Orgs.(org.RolePreloader); ok && p.IsUser() && !p.IsAdmin && p.UserID != uuid.Nil {
		if pctx, err := rp.PreloadRoles(ctx, p.UserID, ids); err == nil {
			ctx = pctx
		} else {
			d.Log.WarnContext(ctx, "preload roles for a page of Repos", "error", err.Error())
		}
	}
	if hr, ok := d.Repos.(repo.HeadResolver); ok {
		if heads, err := hr.ResolveHeads(ctx, live); err == nil {
			pg.heads = heads
		}
	}
	if pg.heads != nil {
		if bg, ok := d.Index.(index.BatchGetter); ok && d.Index != nil {
			if sums, err := bg.GetMany(ctx, pg.heads); err == nil {
				pg.summaries = sums
			}
		}
		if sr, ok := d.Scanner.(scan.StatusReader); ok && d.Scanner != nil {
			if st, err := sr.Statuses(ctx, pg.heads); err == nil {
				pg.scans = st
			}
		}
	}
	if c, ok := d.Community.(community.Counter); ok && d.Community != nil {
		if likes, err := c.LikesOf(ctx, ids); err == nil {
			pg.likes = likes
		}
		if dl, err := c.DownloadsOf(ctx, ids, downloadsSince()); err == nil {
			pg.downloads = dl
		}
	}
	return ctx, pg
}

// downloadsSince is the first day a Repo's downloads count covers.
func downloadsSince() time.Time {
	return time.Now().UTC().AddDate(0, 0, -30).Truncate(24 * time.Hour)
}
