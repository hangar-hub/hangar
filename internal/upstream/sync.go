package upstream

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/robfig/cron/v3"

	"github.com/jamesagarside/hangar/internal/auth"
	"github.com/jamesagarside/hangar/internal/jobs"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/repo"
)

// syncPayload is the payload of mirror.sync Jobs and schedules.
type syncPayload struct {
	RepoUUID uuid.UUID `json:"repo_uuid"`
}

// defaultSchedule spreads tracked Mirrors over the hour.
func defaultSchedule(repoUUID uuid.UUID) string {
	h := fnv.New32a()
	_, _ = h.Write(repoUUID[:])
	return fmt.Sprintf(DefaultTrackSchedule, h.Sum32()%60)
}

// ValidateSchedule checks a 5-field cron expression.
func ValidateSchedule(expr string) error {
	if _, err := cron.ParseStandard(expr); err != nil || strings.HasPrefix(strings.TrimSpace(expr), "@") {
		return fmt.Errorf("%w: schedule %q is not a 5-field cron expression", ErrInvalidSelection, expr)
	}
	return nil
}

// scheduleSync creates or removes the Mirror's mirror.sync schedule to
// match its tracking setting.
func (s *Service) scheduleSync(ctx context.Context, r *repo.Repo) error {
	m, err := s.mirrors.get(ctx, r.UUID, r.ID.Type)
	if err != nil {
		return err
	}
	if !m.Track || m.DetachedAt != nil {
		return s.opts.Queue.DeleteSchedule(ctx, jobs.KindMirrorSync, r.UUID.String())
	}
	expr := m.Schedule
	if expr == "" {
		expr = defaultSchedule(r.UUID)
	}
	return s.opts.Queue.SetSchedule(ctx, jobs.Schedule{Kind: jobs.KindMirrorSync, Key: r.UUID.String(), Cron: expr,
		Payload: syncPayload{RepoUUID: r.UUID}, Enabled: true})
}

// runSync is the mirror.sync handler (PULL-9): it queues a Pull of the
// tracked branch with the Mirror's selection. The Pull fetches new
// Commits, applies upstream force-pushes (keeping replaced tips under
// refs/hangar/previous/) and pulls new objects of the selected files.
func (s *Service) runSync(ctx context.Context, j *jobs.Job, _ jobs.Progress) error {
	var p syncPayload
	if err := j.DecodePayload(&p); err != nil {
		return jobs.Permanent(err)
	}
	r, err := s.opts.Repos.GetByUUID(ctx, p.RepoUUID)
	if errors.Is(err, repo.ErrNotFound) || (err == nil && r.DeletedAt != nil) {
		return s.opts.Queue.DeleteSchedule(ctx, jobs.KindMirrorSync, p.RepoUUID.String())
	}
	if err != nil {
		return err
	}
	_, err = s.syncNow(ctx, r)
	if errors.Is(err, ErrNotMirror) || errors.Is(err, errNotTracking) {
		return s.opts.Queue.DeleteSchedule(ctx, jobs.KindMirrorSync, p.RepoUUID.String())
	}
	return err
}

var errNotTracking = errors.New("mirror does not track its upstream")

// syncNow queues a sync Pull of a Mirror.
func (s *Service) syncNow(ctx context.Context, r *repo.Repo) (jobs.JobID, error) {
	m, err := s.mirrors.active(ctx, r.UUID, r.ID.Type)
	if err != nil {
		return uuid.Nil, err
	}
	up, err := s.opts.Registry.Get(ctx, m.UpstreamID)
	if err != nil {
		return uuid.Nil, err
	}
	if !up.Enabled {
		return uuid.Nil, fmt.Errorf("%w: %s", ErrUpstreamDisabled, up.Name)
	}
	p := pullPayload{UpstreamID: up.ID, Upstream: up.Name, UpstreamRepo: m.UpstreamRepo.String(), Type: r.ID.Type, Repo: r.ID.String(),
		Revision: m.TrackRef, Selection: m.Selection, RequestedBy: m.TrackUser, Sync: true}
	return s.enqueuePull(ctx, p, uuid.Nil, jobs.PriorityNormal)
}

// SyncMirror queues a sync of a Mirror now ("Sync now"); the caller needs
// pull_mirror on it.
func (s *Service) SyncMirror(ctx context.Context, p model.Principal, r *repo.Repo) (jobs.JobID, error) {
	if err := s.canPullInto(ctx, p, r); err != nil {
		return uuid.Nil, err
	}
	return s.syncNow(ctx, r)
}

// canPullInto applies StartPull's rule to an existing Mirror.
func (s *Service) canPullInto(ctx context.Context, p model.Principal, r *repo.Repo) error {
	m, up, err := s.Mirror(ctx, r)
	if err != nil {
		return err
	}
	if m.DetachedAt != nil {
		return ErrNotMirror
	}
	return s.authorisePull(ctx, p, up, r.ID, m.UpstreamRepo)
}

// PullMore pulls more files into a Mirror (POST .../mirror/pull).
func (s *Service) PullMore(ctx context.Context, p model.Principal, r *repo.Repo, sel Selection, now bool) (jobs.JobID, error) {
	m, up, err := s.Mirror(ctx, r)
	if err != nil {
		return uuid.Nil, err
	}
	if m.DetachedAt != nil {
		return uuid.Nil, ErrNotMirror
	}
	id, _, err := s.StartPull(ctx, PullRequest{Upstream: up.Name, Repo: m.UpstreamRepo, Revision: m.TrackRef, Selection: sel, Now: now, Actor: p})
	return id, err
}

// MirrorUpdate changes a Mirror's selection, tracking or schedule. Nil
// fields are unchanged.
type MirrorUpdate struct {
	Selection *Selection
	Track     *bool
	TrackRef  *string
	Schedule  *string
}

// UpdateMirror changes a Mirror's settings; the caller needs pull_mirror.
// Turning tracking on makes the caller the user whose upstream token
// syncs use.
func (s *Service) UpdateMirror(ctx context.Context, p model.Principal, r *repo.Repo, u MirrorUpdate) (*Mirror, error) {
	if err := s.canPullInto(ctx, p, r); err != nil {
		return nil, err
	}
	var sel Selection
	if u.Selection != nil {
		var err error
		if sel, err = u.Selection.Normalise(); err != nil {
			return nil, err
		}
	}
	if u.Schedule != nil && *u.Schedule != "" {
		if err := ValidateSchedule(*u.Schedule); err != nil {
			return nil, err
		}
	}
	if u.TrackRef != nil && *u.TrackRef != "" {
		if err := model.ValidateRefName(model.BranchRef(*u.TrackRef)); err != nil {
			return nil, fmt.Errorf("%w: track_ref %q is not a branch name", ErrInvalidSelection, *u.TrackRef)
		}
	}
	m, err := s.mirrors.update(ctx, r.UUID, r.ID.Type, func(m *Mirror) error {
		if m.DetachedAt != nil {
			return ErrNotMirror
		}
		if u.Selection != nil {
			m.Selection = sel
		}
		if u.Track != nil {
			if *u.Track && !m.Track {
				m.TrackUser = p.UserID
			}
			m.Track = *u.Track
		}
		if u.TrackRef != nil {
			m.TrackRef = *u.TrackRef
		}
		if u.Schedule != nil {
			m.Schedule = *u.Schedule
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err := s.scheduleSync(ctx, r); err != nil {
		return nil, err
	}
	return m, nil
}

// Detach turns a Mirror into an ordinary Repo that keeps its history; the
// caller needs admin on the Repo.
func (s *Service) Detach(ctx context.Context, p model.Principal, r *repo.Repo) error {
	if err := s.opts.Authz.Can(ctx, p, auth.ActionAdmin, repoTarget(r)); err != nil {
		return err
	}
	now := s.opts.Now().UTC()
	if _, err := s.mirrors.update(ctx, r.UUID, r.ID.Type, func(m *Mirror) error {
		if m.DetachedAt != nil {
			return ErrNotMirror
		}
		m.DetachedAt, m.Track, m.Status = &now, false, MirrorDetached
		return nil
	}); err != nil {
		return err
	}
	if err := s.opts.Queue.DeleteSchedule(ctx, jobs.KindMirrorSync, r.UUID.String()); err != nil {
		return err
	}
	return s.opts.MirrorState.SetMirrorState(ctx, r.ID, false)
}

// UpdateAvailable reports whether the Upstream's tracked branch has moved
// past the Mirror's last synced Commit. It asks the Upstream, so callers
// use it on single-Repo views only.
func (s *Service) UpdateAvailable(ctx context.Context, p model.Principal, r *repo.Repo) (bool, error) {
	m, up, err := s.Mirror(ctx, r)
	if err != nil || m.DetachedAt != nil || !up.Enabled {
		return false, err
	}
	cred, err := s.credentials(ctx, up, p.UserID)
	if err != nil {
		return false, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	info, err := s.opts.Client.RepoInfo(ctx, *up, cred, m.UpstreamRepo, m.TrackRef)
	if err != nil {
		return false, err
	}
	return !m.LastSyncedCommit.IsZero() && info.SHA != m.LastSyncedCommit, nil
}
