package hfapi

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/hangar-hub/hangar/internal/community"
	"github.com/hangar-hub/hangar/internal/index"
	"github.com/hangar-hub/hangar/internal/model"
	"github.com/hangar-hub/hangar/internal/repo"
)

// listPage holds what buildInfo reads for a page of /api/models or
// /api/datasets items, loaded with one query per kind of data instead of
// several queries per Repo. A nil map was not loaded, because the service
// cannot read many Repos at once, and buildInfo reads that data for each
// Repo instead.
type listPage struct {
	// heads holds the default branch head of each Repo that has one.
	heads map[uuid.UUID]model.CommitSHA
	// summaries holds the index results of each indexed head.
	summaries map[uuid.UUID]*index.Summary
	// recent and all count downloads in the last 30 days and in all time.
	likes, recent, all map[uuid.UUID]int64
}

// loadListPage reads what the items of a page need, as listRepos and
// buildInfo would read it Repo by Repo.
func (h *Handler) loadListPage(ctx context.Context, repos []*repo.Repo, needSHA bool, o infoOpts) (*listPage, error) {
	pg := &listPage{}
	if len(repos) == 0 {
		return pg, nil
	}
	ids := make([]uuid.UUID, 0, len(repos))
	for _, r := range repos {
		ids = append(ids, r.UUID)
	}
	if hr, ok := h.d.Repos.(repo.HeadResolver); ok && needSHA {
		heads, err := hr.ResolveHeads(ctx, repos)
		if err != nil {
			return nil, err
		}
		pg.heads = heads
		if bg, ok := h.d.Index.(index.BatchGetter); ok {
			if pg.summaries, err = bg.GetMany(ctx, heads); err != nil {
				return nil, err
			}
		}
	}
	c, ok := h.d.Community.(community.Counter)
	if !ok {
		return pg, nil
	}
	var err error
	if o.want("likes", true) {
		if pg.likes, err = c.LikesOf(ctx, ids); err != nil {
			return nil, err
		}
	}
	if o.want("downloads", true) || o.want("downloadsAllTime", false) {
		// As downloads counts them: from the day 30 days ago.
		since := h.now().UTC().AddDate(0, 0, -30).Truncate(24 * time.Hour)
		if pg.recent, err = c.DownloadsOf(ctx, ids, since); err != nil {
			return nil, err
		}
		pg.all = map[uuid.UUID]int64{}
		if o.want("downloadsAllTime", false) {
			if pg.all, err = c.DownloadsOf(ctx, ids, time.Time{}); err != nil {
				return nil, err
			}
		}
	}
	return pg, nil
}
