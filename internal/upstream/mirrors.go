package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"

	"github.com/jamesagarside/hangar/internal/db"
	"github.com/jamesagarside/hangar/internal/model"
)

// ErrNotMirror means the Repo is not a Mirror (or was detached).
var ErrNotMirror = errors.New("repo is not a mirror")

// Mirror statuses.
const (
	MirrorPending  = "pending"
	MirrorSyncing  = "syncing"
	MirrorOK       = "ok"
	MirrorPartial  = "partial"
	MirrorFailed   = "failed"
	MirrorDetached = "detached"
)

// DefaultTrackSchedule is how often a tracked Mirror syncs: every 6 hours,
// at a minute picked per Mirror for jitter.
const DefaultTrackSchedule = "%d */6 * * *"

// Mirror is a Mirror's settings and state (the mirrors table).
type Mirror struct {
	RepoUUID     uuid.UUID
	UpstreamID   uuid.UUID
	UpstreamRepo model.RepoID
	// Selection is the union of every Pull into the Mirror.
	Selection Selection
	Track     bool
	// TrackRef is the tracked branch; "" means the default branch.
	TrackRef string
	// Schedule is a 5-field cron expression; "" means the default.
	Schedule         string
	LastSyncedCommit model.CommitSHA
	LastSyncedAt     *time.Time
	Status           string
	LastError        string
	DetachedAt       *time.Time
	// TrackUser is the user whose upstream token tracking syncs use.
	TrackUser uuid.UUID
}

// mirrorSelection is the JSON in mirrors.selection.
type mirrorSelection struct {
	Selection
	TrackUser uuid.UUID `json:"track_user,omitempty"`
}

func toMirror(row *db.Mirror, t model.RepoType) (*Mirror, error) {
	b, err := json.Marshal(row.Selection)
	if err != nil {
		return nil, err
	}
	var ms mirrorSelection
	if err := json.Unmarshal(b, &ms); err != nil {
		return nil, fmt.Errorf("decode mirror selection: %w", err)
	}
	up, err := model.ParseRepoID(t, row.UpstreamRepo)
	if err != nil {
		return nil, fmt.Errorf("mirror has an invalid upstream repo: %w", err)
	}
	m := &Mirror{RepoUUID: row.RepoID, UpstreamID: row.UpstreamID, UpstreamRepo: up, Selection: ms.Selection, Track: row.Track,
		TrackRef: row.TrackRef, Schedule: row.Schedule, LastSyncedAt: row.LastSyncedAt, Status: row.Status, LastError: row.LastError,
		DetachedAt: row.DetachedAt, TrackUser: ms.TrackUser}
	if row.LastSyncedCommit != nil {
		m.LastSyncedCommit = model.CommitSHA(*row.LastSyncedCommit)
	}
	return m, nil
}

func fromMirror(m *Mirror) (*db.Mirror, error) {
	b, err := json.Marshal(mirrorSelection{Selection: m.Selection, TrackUser: m.TrackUser})
	if err != nil {
		return nil, err
	}
	var sel map[string]any
	if err := json.Unmarshal(b, &sel); err != nil {
		return nil, err
	}
	row := &db.Mirror{RepoID: m.RepoUUID, UpstreamID: m.UpstreamID, UpstreamRepo: m.UpstreamRepo.String(),
		Include: nonNil(m.Selection.Include), Exclude: nonNil(m.Selection.Exclude), Selection: sel, Track: m.Track, TrackRef: m.TrackRef,
		Schedule: m.Schedule, LastSyncedAt: m.LastSyncedAt, Status: m.Status, LastError: m.LastError, DetachedAt: m.DetachedAt}
	if !m.LastSyncedCommit.IsZero() {
		c := string(m.LastSyncedCommit)
		row.LastSyncedCommit = &c
	}
	return row, nil
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// mirrorStore reads and writes the mirrors table.
type mirrorStore struct{ db *db.DB }

// get returns the Mirror row of a Repo, including detached ones.
func (s mirrorStore) get(ctx context.Context, repoUUID uuid.UUID, t model.RepoType) (*Mirror, error) {
	var row db.Mirror
	if err := s.db.NewSelect().Model(&row).Where("repo_id = ?", repoUUID).Scan(ctx); err != nil {
		if db.IsNotFound(err) {
			return nil, ErrNotMirror
		}
		return nil, err
	}
	return toMirror(&row, t)
}

// active returns the Mirror row of a Repo unless it is detached.
func (s mirrorStore) active(ctx context.Context, repoUUID uuid.UUID, t model.RepoType) (*Mirror, error) {
	m, err := s.get(ctx, repoUUID, t)
	if err != nil {
		return nil, err
	}
	if m.DetachedAt != nil {
		return nil, ErrNotMirror
	}
	return m, nil
}

// put inserts or replaces a Mirror row.
func (s mirrorStore) put(ctx context.Context, m *Mirror) error {
	row, err := fromMirror(m)
	if err != nil {
		return err
	}
	now := db.Now()
	row.CreatedAt, row.UpdatedAt = now, now
	_, err = s.db.NewInsert().Model(row).
		On("CONFLICT (repo_id) DO UPDATE").
		Set("upstream_id = EXCLUDED.upstream_id").Set("upstream_repo = EXCLUDED.upstream_repo").
		Set("include = EXCLUDED.include").Set("exclude = EXCLUDED.exclude").Set("selection = EXCLUDED.selection").
		Set("track = EXCLUDED.track").Set("track_ref = EXCLUDED.track_ref").Set("schedule = EXCLUDED.schedule").
		Set("last_synced_commit = EXCLUDED.last_synced_commit").Set("last_synced_at = EXCLUDED.last_synced_at").
		Set("status = EXCLUDED.status").Set("last_error = EXCLUDED.last_error").Set("detached_at = EXCLUDED.detached_at").
		Set("updated_at = EXCLUDED.updated_at").
		Exec(ctx)
	return err
}

// update loads a Mirror, applies fn and saves it in one transaction.
func (s mirrorStore) update(ctx context.Context, repoUUID uuid.UUID, t model.RepoType, fn func(m *Mirror) error) (*Mirror, error) {
	var out *Mirror
	err := s.db.InTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		var row db.Mirror
		if err := forUpdate(s.db, tx.NewSelect().Model(&row).Where("repo_id = ?", repoUUID)).Scan(ctx); err != nil {
			if db.IsNotFound(err) {
				return ErrNotMirror
			}
			return err
		}
		m, err := toMirror(&row, t)
		if err != nil {
			return err
		}
		if err := fn(m); err != nil {
			return err
		}
		nr, err := fromMirror(m)
		if err != nil {
			return err
		}
		nr.CreatedAt, nr.UpdatedAt = row.CreatedAt, db.Now()
		if _, err := tx.NewUpdate().Model(nr).WherePK().Exec(ctx); err != nil {
			return err
		}
		out = m
		return nil
	})
	return out, err
}

// byUpstream returns the Mirrors (including detached ones) of an upstream
// Repo, to stop two Upstreams providing the same ID.
func (s mirrorStore) byUpstreamRepo(ctx context.Context, upstreamRepo model.RepoID) ([]db.Mirror, error) {
	var rows []db.Mirror
	err := s.db.NewSelect().Model(&rows).Where("lower(upstream_repo) = lower(?)", upstreamRepo.String()).Scan(ctx)
	return rows, err
}
