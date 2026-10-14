package repo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/uptrace/bun"

	"github.com/jamesagarside/hangar/internal/audit"
	"github.com/jamesagarside/hangar/internal/db"
	"github.com/jamesagarside/hangar/internal/gitstore"
	"github.com/jamesagarside/hangar/internal/jobs"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/org"
	"github.com/jamesagarside/hangar/internal/webhook"
)

func (s *Store) namespace(ctx context.Context, name string) (*org.Namespace, error) {
	ns, err := s.ns.Get(ctx, name)
	if errors.Is(err, org.ErrNotFound) {
		return nil, fmt.Errorf("%w: namespace %q does not exist", ErrInvalid, name)
	}
	if err != nil {
		return nil, err
	}
	return ns, nil
}

// readme is the starter Model card the UI asks for.
func readme(p CreateParams) string {
	licence := p.Licence
	if licence == "" {
		licence = "other"
	}
	return "---\nlicense: " + licence + "\n---\n\n# " + p.ID.Name + "\n\n" + p.Description + "\n"
}

// Create implements Service. The ID keeps the case it was created with;
// lookups ignore case.
func (s *Store) Create(ctx context.Context, p CreateParams) (*Repo, error) {
	if err := p.ID.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	vis := p.Visibility
	if vis == "" {
		vis = model.VisibilityPublic
	}
	if !vis.Valid() {
		return nil, fmt.Errorf("%w: visibility %q", ErrInvalid, vis)
	}
	ns, err := s.namespace(ctx, p.ID.Namespace)
	if err != nil {
		return nil, err
	}
	settings := map[string]any{}
	if p.Licence != "" {
		settings[settingLicence] = p.Licence
	}
	if p.Mirror {
		settings[settingMirror] = true
	}
	now := s.now()
	row := &db.Repo{
		ID: db.NewID(), NamespaceID: ns.ID, Name: p.ID.Name, Type: string(p.ID.Type), Visibility: string(vis),
		DefaultBranch: model.DefaultBranch, HeadRef: model.DefaultHeadRef, Description: p.Description,
		Tags: []string{}, Settings: settings, Gating: map[string]any{}, CreatedBy: userRef(p.Actor),
		LastModifiedAt: &now, Timestamps: db.Timestamps{CreatedAt: now, UpdatedAt: now},
	}
	err = s.db.InTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.NewInsert().Model(row).Exec(ctx); err != nil {
			return err
		}
		return deleteRedirect(ctx, tx, p.ID.Type, ns.Name, p.ID.Name)
	})
	if db.IsUniqueViolation(err) {
		return nil, fmt.Errorf("%w: %s", ErrExists, p.ID.Path())
	}
	if err != nil {
		return nil, fmt.Errorf("repo: create %s: %w", p.ID.Path(), err)
	}
	full, err := s.byUUID(ctx, s.db, row.ID)
	if err != nil {
		return nil, err
	}
	if !p.Mirror {
		ops := []Operation{{Kind: OpAdd, Path: ".gitattributes", Content: bytes.NewReader(DefaultGitAttributes(p.ID.Type))}}
		if p.InitialReadme {
			ops = append(ops, Operation{Kind: OpAdd, Path: "README.md", Content: strings.NewReader(readme(p))})
		}
		if _, err := s.commit(ctx, full, CommitParams{Repo: p.ID, Revision: model.DefaultBranch, Summary: "initial commit",
			Operations: ops, Actor: p.Actor, AllowMirror: true, Time: now}); err != nil {
			// Do not leave a Repo without its first Commit.
			_, _ = s.db.NewDelete().Model((*db.Repo)(nil)).Where("id = ?", row.ID).Exec(context.WithoutCancel(ctx))
			_ = s.git.DeleteRepo(context.WithoutCancel(ctx), row.ID)
			return nil, err
		}
		if full, err = s.byUUID(ctx, s.db, row.ID); err != nil {
			return nil, err
		}
	}
	r := toRepo(full)
	s.record(ctx, audit.RepoCreated, p.Actor, r, map[string]any{"visibility": string(r.Visibility)})
	s.emit(ctx, webhook.RepoCreated, p.Actor, r, map[string]any{"visibility": string(r.Visibility)})
	return r, nil
}

func deleteRedirect(ctx context.Context, tx bun.IDB, t model.RepoType, ns, name string) error {
	_, err := tx.NewDelete().Model((*db.RepoRedirect)(nil)).Where("type = ?", string(t)).
		Where("lower(old_namespace) = lower(?)", ns).Where("lower(old_name) = lower(?)", name).Exec(ctx)
	return err
}

// move changes a Repo's Namespace or name and keeps a redirect from the old
// ID (REPO-4).
func (s *Store) move(ctx context.Context, row *repoRow, ns *org.Namespace, name string) (*Repo, error) {
	old := toRepo(row)
	if ns.ID == row.NamespaceID && name == row.Name {
		return old, nil
	}
	now := s.now()
	err := s.db.InTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.NewUpdate().Model((*db.Repo)(nil)).Set("namespace_id = ?", ns.ID).Set("name = ?", name).
			Set("updated_at = ?", now).Where("id = ?", row.ID).Exec(ctx); err != nil {
			return err
		}
		if err := deleteRedirect(ctx, tx, old.ID.Type, ns.Name, name); err != nil {
			return err
		}
		if strings.EqualFold(old.ID.Namespace, ns.Name) && strings.EqualFold(old.ID.Name, name) {
			// A change of case only: the old ID still finds the Repo.
			return nil
		}
		if err := deleteRedirect(ctx, tx, old.ID.Type, old.ID.Namespace, old.ID.Name); err != nil {
			return err
		}
		_, err := tx.NewInsert().Model(&db.RepoRedirect{ID: db.NewID(), OldNamespace: old.ID.Namespace, OldName: old.ID.Name,
			Type: string(old.ID.Type), RepoID: row.ID, Timestamps: db.Timestamps{CreatedAt: now, UpdatedAt: now}}).Exec(ctx)
		return err
	})
	if db.IsUniqueViolation(err) {
		return nil, fmt.Errorf("%w: %s/%s", ErrExists, ns.Name, name)
	}
	if err != nil {
		return nil, fmt.Errorf("repo: move %s: %w", old.ID.Path(), err)
	}
	moved, err := s.byUUID(ctx, s.db, row.ID)
	if err != nil {
		return nil, err
	}
	return toRepo(moved), nil
}

// Rename implements Service.
func (s *Store) Rename(ctx context.Context, id model.RepoID, newName string, actor model.Principal) (*Repo, error) {
	if err := model.ValidateRepoName(newName); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	row, err := s.lookup(ctx, s.db, id)
	if err != nil {
		return nil, err
	}
	ns, err := s.ns.GetByID(ctx, row.NamespaceID)
	if err != nil {
		return nil, err
	}
	old := toRepo(row)
	r, err := s.move(ctx, row, ns, newName)
	if err != nil {
		return nil, err
	}
	data := map[string]any{"from": old.ID.String(), "to": r.ID.String()}
	s.record(ctx, audit.RepoRenamed, actor, r, data)
	s.emit(ctx, webhook.RepoRenamed, actor, r, data)
	return r, nil
}

// Transfer implements Service.
func (s *Store) Transfer(ctx context.Context, id model.RepoID, newNamespace string, actor model.Principal) (*Repo, error) {
	row, err := s.lookup(ctx, s.db, id)
	if err != nil {
		return nil, err
	}
	ns, err := s.namespace(ctx, newNamespace)
	if err != nil {
		return nil, err
	}
	old := toRepo(row)
	r, err := s.move(ctx, row, ns, row.Name)
	if err != nil {
		return nil, err
	}
	data := map[string]any{"from": old.ID.String(), "to": r.ID.String()}
	s.record(ctx, audit.RepoTransferred, actor, r, data)
	s.emit(ctx, webhook.RepoRenamed, actor, r, data)
	return r, nil
}

// Delete implements Service. The Repo is hidden at once and purged by a
// repo.purge Job when the restore window ends (REPO-5).
func (s *Store) Delete(ctx context.Context, id model.RepoID, actor model.Principal) error {
	row, err := s.lookup(ctx, s.db, id)
	if err != nil {
		return err
	}
	now := s.now()
	if _, err := s.db.NewUpdate().Model((*db.Repo)(nil)).Set("deleted_at = ?", now).Set("updated_at = ?", now).
		Where("id = ?", row.ID).Where("deleted_at IS NULL").Exec(ctx); err != nil {
		return fmt.Errorf("repo: delete %s: %w", id.Path(), err)
	}
	r := toRepo(row)
	r.DeletedAt = &now
	s.enqueue(ctx, jobs.NewJob{Kind: jobs.KindRepoPurge, DedupeKey: purgeDedupe(row.ID, now), Payload: PurgeJob{RepoUUID: row.ID, DeletedAt: now},
		RunAfter: now.Add(s.opts.RestoreWindow), Priority: jobs.PriorityLow, Repo: row.ID})
	s.record(ctx, audit.RepoDeleted, actor, r, nil)
	s.emit(ctx, webhook.RepoDeleted, actor, r, nil)
	return nil
}

// Restore implements Service. It restores the most recently deleted Repo
// with the ID, if it was deleted within the restore window.
func (s *Store) Restore(ctx context.Context, id model.RepoID, actor model.Principal) (*Repo, error) {
	row := new(repoRow)
	err := s.selectRepo(s.db).Where("lower(ns.name) = lower(?)", id.Namespace).Where("repo.type = ?", string(id.Type)).
		Where("lower(repo.name) = lower(?)", id.Name).Where("repo.deleted_at IS NOT NULL").
		OrderExpr("repo.deleted_at DESC").Limit(1).Scan(ctx, row)
	if db.IsNotFound(err) {
		return nil, fmt.Errorf("%w: no deleted repo %s", ErrNotFound, id.Path())
	}
	if err != nil {
		return nil, fmt.Errorf("repo: restore %s: %w", id.Path(), err)
	}
	if s.now().Sub(*row.DeletedAt) > s.opts.RestoreWindow {
		return nil, fmt.Errorf("%w: %s was deleted more than %s ago and can no longer be restored", ErrNotFound, id.Path(), s.opts.RestoreWindow)
	}
	_, err = s.db.NewUpdate().Model((*db.Repo)(nil)).Set("deleted_at = NULL").Set("updated_at = ?", s.now()).
		Where("id = ?", row.ID).Exec(ctx)
	if db.IsUniqueViolation(err) {
		return nil, fmt.Errorf("%w: %s was created again after it was deleted", ErrExists, id.Path())
	}
	if err != nil {
		return nil, fmt.Errorf("repo: restore %s: %w", id.Path(), err)
	}
	restored, err := s.byUUID(ctx, s.db, row.ID)
	if err != nil {
		return nil, err
	}
	r := toRepo(restored)
	s.record(ctx, audit.RepoRestored, actor, r, nil)
	s.emit(ctx, webhook.RepoRestored, actor, r, nil)
	return r, nil
}

// Purge implements Service: it deletes the Repo's git data and rows. LFS
// objects stay until garbage collection finds them unreferenced; their
// grace period starts now.
func (s *Store) Purge(ctx context.Context, repoUUID uuid.UUID) error {
	row, err := s.byUUID(ctx, s.db, repoUUID)
	if err != nil {
		return err
	}
	if row.DeletedAt == nil {
		return fmt.Errorf("%w: delete the repo before purging it", ErrInvalid)
	}
	var oids []string
	if err := s.db.NewSelect().Model((*db.RepoLFS)(nil)).Column("oid").Where("repo_id = ?", repoUUID).Scan(ctx, &oids); err != nil {
		return err
	}
	if err := Touch(ctx, s.db, oids, s.now()); err != nil {
		return err
	}
	if err := s.git.DeleteRepo(ctx, repoUUID); err != nil {
		return fmt.Errorf("repo: delete git data: %w", err)
	}
	if _, err := s.db.NewDelete().Model((*db.Repo)(nil)).Where("id = ?", repoUUID).Exec(ctx); err != nil {
		return fmt.Errorf("repo: purge: %w", err)
	}
	s.record(ctx, audit.RepoPurged, model.System(), toRepo(row), nil)
	return nil
}

// UpdateSettings implements Service.
func (s *Store) UpdateSettings(ctx context.Context, id model.RepoID, u SettingsUpdate, actor model.Principal) (*Repo, error) {
	row, err := s.lookup(ctx, s.db, id)
	if err != nil {
		return nil, err
	}
	before := toRepo(row)
	q := s.db.NewUpdate().Model((*db.Repo)(nil)).Where("id = ?", row.ID).Set("updated_at = ?", s.now())
	if u.Visibility != nil {
		if !u.Visibility.Valid() {
			return nil, fmt.Errorf("%w: visibility %q", ErrInvalid, *u.Visibility)
		}
		q = q.Set("visibility = ?", string(*u.Visibility))
	}
	if u.DefaultBranch != nil {
		ref := model.RefHeads + *u.DefaultBranch
		if _, ok, err := gitstore.LookupRef(ctx, s.db, row.ID, ref); err != nil {
			return nil, err
		} else if !ok {
			return nil, fmt.Errorf("%w: branch %s", ErrRevisionNotFound, *u.DefaultBranch)
		}
		q = q.Set("default_branch = ?", *u.DefaultBranch).Set("head_ref = ?", ref)
	}
	if u.Description != nil {
		q = q.Set("description = ?", *u.Description)
	}
	if u.Tags != nil {
		tags := slices.Clone(*u.Tags)
		if tags == nil {
			tags = []string{}
		}
		b, _ := json.Marshal(tags)
		q = q.Set("tags = ?", string(b))
	}
	settings := maps.Clone(row.Settings)
	if settings == nil {
		settings = map[string]any{}
	}
	if u.Licence != nil {
		if *u.Licence == "" {
			delete(settings, settingLicence)
		} else {
			settings[settingLicence] = *u.Licence
		}
		b, _ := json.Marshal(settings)
		q = q.Set("settings = ?", string(b))
	}
	if u.Gating != nil {
		if m := u.Gating.Mode; m != "" && m != "auto" && m != "manual" {
			return nil, fmt.Errorf("%w: gating mode %q", ErrInvalid, m)
		}
		b, _ := json.Marshal(u.Gating)
		q = q.Set("gating = ?", string(b))
	}
	if _, err := q.Exec(ctx); err != nil {
		return nil, fmt.Errorf("repo: update settings: %w", err)
	}
	updated, err := s.byUUID(ctx, s.db, row.ID)
	if err != nil {
		return nil, err
	}
	r := toRepo(updated)
	if r.Visibility != before.Visibility {
		data := map[string]any{"from": string(before.Visibility), "to": string(r.Visibility)}
		s.record(ctx, audit.RepoVisibilityChanged, actor, r, data)
		s.emit(ctx, webhook.RepoVisibilityChanged, actor, r, data)
	}
	return r, nil
}

// SetMirrorState implements Service.
func (s *Store) SetMirrorState(ctx context.Context, id model.RepoID, mirror bool) error {
	row, err := s.lookup(ctx, s.db, id)
	if err != nil {
		return err
	}
	settings := maps.Clone(row.Settings)
	if settings == nil {
		settings = map[string]any{}
	}
	if mirror {
		settings[settingMirror] = true
	} else {
		delete(settings, settingMirror)
	}
	b, _ := json.Marshal(settings)
	_, err = s.db.NewUpdate().Model((*db.Repo)(nil)).Set("settings = ?", string(b)).Set("updated_at = ?", s.now()).
		Where("id = ?", row.ID).Exec(ctx)
	return err
}

// SetLFSPresence implements Service.
func (s *Store) SetLFSPresence(ctx context.Context, id model.RepoID, refs []LFSRef) error {
	row, err := s.lookup(ctx, s.db, id)
	if err != nil {
		return err
	}
	for _, l := range refs {
		if !model.IsSHA256(l.OID) || l.Size < 0 {
			return fmt.Errorf("%w: oid %q", ErrInvalid, l.OID)
		}
	}
	return s.db.InTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		if err := upsertLFS(ctx, tx, row.ID, refs, true); err != nil {
			return err
		}
		return s.touchSizes(ctx, tx, row.ID)
	})
}

func (s *Store) touchSizes(ctx context.Context, tx bun.IDB, repoID uuid.UUID) error {
	_, err := tx.NewUpdate().Model((*db.Repo)(nil)).
		Set("lfs_bytes = (SELECT COALESCE(SUM(size), 0) FROM repo_lfs WHERE repo_id = ? AND present = ?)", repoID, true).
		Set("size_bytes = (SELECT COALESCE(SUM(size), 0) FROM packs WHERE repo_id = ? AND state = ?) + (SELECT COALESCE(SUM(size), 0) FROM repo_lfs WHERE repo_id = ? AND present = ?)",
			repoID, gitstore.PackLive, repoID, true).
		Where("id = ?", repoID).Exec(ctx)
	return err
}
