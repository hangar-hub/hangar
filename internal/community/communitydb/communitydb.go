// Package communitydb is the database-backed community.Service:
// discussions and pull requests with merges through the Repo service,
// likes and daily download counts (REPO-6, REPO-7). It lives apart from
// package community because the Repo service imports community for its
// interface.
package communitydb

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"

	"github.com/hangar-hub/hangar/internal/community"
	"github.com/hangar-hub/hangar/internal/db"
	"github.com/hangar-hub/hangar/internal/model"
	"github.com/hangar-hub/hangar/internal/repo"
	"github.com/hangar-hub/hangar/internal/webhook"
)

// Limits on user text.
const (
	MaxTitle   = 200
	MaxComment = 64 << 10
)

// Options configure the DB service.
type Options struct {
	DB    *db.DB
	Repos repo.Service
	// Webhooks receives discussion.created, discussion.commented,
	// discussion.status_changed and pr.merged; optional.
	Webhooks webhook.Emitter
	Log      *slog.Logger
}

// DB is the database-backed community.Service. Discussion and event authors are
// shown by username, read from the users and namespaces tables.
type DB struct {
	o Options
}

var (
	_ community.Service = (*DB)(nil)
	_ community.Counter = (*DB)(nil)
)

// New returns a DB service.
func New(o Options) *DB {
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	return &DB{o: o}
}

type discussionRow struct {
	db.Discussion `bun:",extend"`
	AuthorName    sql.NullString `bun:"author_name,scanonly"`
}

type eventRow struct {
	db.DiscussionEvent `bun:",extend"`
	AuthorName         sql.NullString `bun:"author_name,scanonly"`
}

// authorJoin reads usernames for author_id; it never writes these tables.
const authorJoin = "LEFT JOIN users AS au ON au.id = %s.author_id LEFT JOIN namespaces AS an ON an.id = au.namespace_id"

func (s *DB) selectDiscussions(dst *[]discussionRow) *bun.SelectQuery {
	return s.o.DB.NewSelect().Model(dst).ModelTableExpr("discussions AS discussion").ColumnExpr("discussion.*").ColumnExpr("an.name AS author_name").
		Join(fmt.Sprintf(authorJoin, "discussion"))
}

func toDiscussion(r *discussionRow) community.Discussion {
	d := community.Discussion{Num: r.Num, Kind: r.Kind, Title: r.Title, Status: r.Status, Author: r.AuthorName.String, CreatedAt: r.CreatedAt.UTC()}
	if r.PRRef != nil {
		d.PRRef = *r.PRRef
	}
	if r.PRBase != nil {
		d.PRBase = *r.PRBase
	}
	return d
}

func toEvent(r *eventRow) community.Event {
	e := community.Event{ID: r.ID, Type: r.Type, Author: r.AuthorName.String, Body: r.Body, Hidden: r.Hidden, CreatedAt: r.CreatedAt.UTC()}
	if len(r.Data) > 0 {
		_ = json.Unmarshal(r.Data, &e.Data)
	}
	if e.Data == nil {
		e.Data = map[string]any{}
	}
	if edited, _ := e.Data["edited"].(bool); edited {
		e.Edited = true
		delete(e.Data, "edited")
	}
	if e.Author == "" {
		if name, _ := e.Data["author"].(string); name != "" {
			e.Author = name
		}
	}
	delete(e.Data, "author")
	return e
}

// List implements community.Service, newest first.
func (s *DB) List(ctx context.Context, repoUUID uuid.UUID, o community.ListOptions) ([]community.Discussion, int, error) {
	var rows []discussionRow
	q := s.selectDiscussions(&rows).Where("discussion.repo_id = ?", repoUUID)
	if o.Type != "" {
		q = q.Where("discussion.kind = ?", o.Type)
	}
	switch o.Status {
	case community.StatusOpen:
		q = q.Where("discussion.status IN (?)", bun.List([]string{community.StatusOpen, community.StatusDraft}))
	case community.StatusClosed:
		q = q.Where("discussion.status IN (?)", bun.List([]string{community.StatusClosed, community.StatusMerged}))
	}
	if o.Author != "" {
		q = q.Where("lower(an.name) = lower(?)", o.Author)
	}
	total, err := q.Count(ctx)
	if err != nil {
		return nil, 0, err
	}
	q = q.Order("discussion.num DESC")
	if o.Limit > 0 {
		q = q.Limit(int64(o.Limit)).Offset(int64(o.Page * o.Limit))
	}
	if err := q.Scan(ctx); err != nil {
		return nil, 0, err
	}
	out := make([]community.Discussion, 0, len(rows))
	for i := range rows {
		out = append(out, toDiscussion(&rows[i]))
	}
	return out, int(total), nil
}

func (s *DB) row(ctx context.Context, idb bun.IDB, repoUUID uuid.UUID, num int) (*discussionRow, error) {
	var rows []discussionRow
	err := idb.NewSelect().Model(&rows).ModelTableExpr("discussions AS discussion").ColumnExpr("discussion.*").ColumnExpr("an.name AS author_name").
		Join(fmt.Sprintf(authorJoin, "discussion")).
		Where("discussion.repo_id = ?", repoUUID).Where("discussion.num = ?", num).Limit(1).Scan(ctx)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("%w: #%d", community.ErrNotFound, num)
	}
	return &rows[0], nil
}

func (s *DB) events(ctx context.Context, discussionID uuid.UUID) ([]community.Event, error) {
	var rows []eventRow
	err := s.o.DB.NewSelect().Model(&rows).ModelTableExpr("discussion_events AS discussion_event").ColumnExpr("discussion_event.*").ColumnExpr("an.name AS author_name").
		Join(fmt.Sprintf(authorJoin, "discussion_event")).
		Where("discussion_event.discussion_id = ?", discussionID).Order("discussion_event.created_at ASC", "discussion_event.id ASC").Scan(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]community.Event, 0, len(rows))
	for i := range rows {
		out = append(out, toEvent(&rows[i]))
	}
	return out, nil
}

// Get implements community.Service.
func (s *DB) Get(ctx context.Context, repoUUID uuid.UUID, num int) (*community.Discussion, error) {
	r, err := s.row(ctx, s.o.DB, repoUUID, num)
	if err != nil {
		return nil, err
	}
	d := toDiscussion(r)
	if d.Events, err = s.events(ctx, r.ID); err != nil {
		return nil, err
	}
	return &d, nil
}

func authorID(p model.Principal) *uuid.UUID {
	if p.UserID == uuid.Nil {
		return nil
	}
	id := p.UserID
	return &id
}

func eventData(data map[string]any, actor model.Principal) db.JSON {
	if data == nil {
		data = map[string]any{}
	}
	if actor.UserID == uuid.Nil && actor.Username != "" {
		data["author"] = actor.Username
	}
	b, _ := json.Marshal(data)
	return b
}

func checkText(title, body string) error {
	if len(title) > MaxTitle {
		return fmt.Errorf("%w: the title is longer than %d characters", community.ErrInvalid, MaxTitle)
	}
	if len(body) > MaxComment {
		return fmt.Errorf("%w: the text is longer than %d bytes", community.ErrInvalid, MaxComment)
	}
	return nil
}

// Create implements community.Service. Numbers are allocated per Repo; a race for
// the same number retries.
func (s *DB) Create(ctx context.Context, p community.CreateParams) (*community.Discussion, error) {
	title := strings.TrimSpace(p.Title)
	if title == "" {
		return nil, fmt.Errorf("%w: a title is required", community.ErrInvalid)
	}
	if p.Author.IsAnonymous() {
		return nil, fmt.Errorf("%w: sign in to open a discussion", community.ErrInvalid)
	}
	if err := checkText(title, p.Description); err != nil {
		return nil, err
	}
	var num int
	for attempt := 0; ; attempt++ {
		err := s.o.DB.InTx(ctx, func(ctx context.Context, tx bun.Tx) error {
			var maxNum sql.NullInt64
			if err := tx.NewSelect().Model((*db.Discussion)(nil)).ColumnExpr("MAX(num)").Where("repo_id = ?", p.RepoUUID).Scan(ctx, &maxNum); err != nil {
				return err
			}
			num = int(maxNum.Int64) + 1
			row := &db.Discussion{ID: db.NewID(), RepoID: p.RepoUUID, Num: num, Kind: community.KindDiscussion, Title: title, Status: community.StatusOpen, AuthorID: authorID(p.Author)}
			if p.PullRequest {
				ref, base := model.PRRef(num), p.Base
				if base == "" {
					base = model.DefaultBranch
				}
				if r, err := s.o.Repos.GetByUUID(ctx, p.RepoUUID); err == nil && p.Base == "" {
					base = r.DefaultBranch
				}
				row.Kind, row.Status, row.PRRef, row.PRBase = community.KindPullRequest, community.StatusDraft, &ref, &base
			}
			if _, err := tx.NewInsert().Model(row).Exec(ctx); err != nil {
				return err
			}
			if p.Description == "" {
				return nil
			}
			ev := &db.DiscussionEvent{ID: db.NewID(), DiscussionID: row.ID, Type: community.EventComment, AuthorID: authorID(p.Author),
				Body: p.Description, Data: eventData(nil, p.Author)}
			_, err := tx.NewInsert().Model(ev).Exec(ctx)
			return err
		})
		if err == nil {
			break
		}
		if !db.IsUniqueViolation(err) || attempt >= 5 {
			return nil, err
		}
	}
	d, err := s.Get(ctx, p.RepoUUID, num)
	if err != nil {
		return nil, err
	}
	s.emit(ctx, webhook.DiscussionCreated, p.Author, p.RepoUUID, map[string]any{"num": num, "title": title, "is_pull_request": p.PullRequest})
	return d, nil
}

func (s *DB) emit(ctx context.Context, name webhook.EventName, actor model.Principal, repoUUID uuid.UUID, data map[string]any) {
	if s.o.Webhooks == nil {
		return
	}
	r, err := s.o.Repos.GetByUUID(ctx, repoUUID)
	if err != nil {
		s.o.Log.WarnContext(ctx, "emit "+string(name), "repo.uuid", repoUUID.String(), "error", err.Error())
		return
	}
	if err := s.o.Webhooks.Emit(ctx, webhook.NewEvent(name, actor, &r.ID, r.UUID, r.NamespaceID, data)); err != nil {
		s.o.Log.WarnContext(ctx, "emit "+string(name), "repo.id", r.ID.String(), "error", err.Error())
	}
}

// addEvent records an event, after change (if any) updates the discussion
// row in the same transaction.
func (s *DB) addEvent(ctx context.Context, repoUUID uuid.UUID, num int, ev *db.DiscussionEvent, change func(ctx context.Context, tx bun.Tx, d *discussionRow) error) (*community.Event, error) {
	err := s.o.DB.InTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		d, err := s.row(ctx, tx, repoUUID, num)
		if err != nil {
			return err
		}
		if change != nil {
			if err := change(ctx, tx, d); err != nil {
				return err
			}
		}
		ev.ID, ev.DiscussionID = db.NewID(), d.ID
		_, err = tx.NewInsert().Model(ev).Exec(ctx)
		return err
	})
	if err != nil {
		return nil, err
	}
	out := toEvent(&eventRow{DiscussionEvent: *ev})
	return &out, nil
}

// Comment implements community.Service.
func (s *DB) Comment(ctx context.Context, repoUUID uuid.UUID, num int, body string, author model.Principal) (*community.Event, error) {
	if strings.TrimSpace(body) == "" {
		return nil, fmt.Errorf("%w: the comment is empty", community.ErrInvalid)
	}
	if author.IsAnonymous() {
		return nil, fmt.Errorf("%w: sign in to comment", community.ErrInvalid)
	}
	if err := checkText("", body); err != nil {
		return nil, err
	}
	e, err := s.addEvent(ctx, repoUUID, num, &db.DiscussionEvent{Type: community.EventComment, AuthorID: authorID(author), Body: body, Data: eventData(nil, author)}, nil)
	if err != nil {
		return nil, err
	}
	e.Author = author.Username
	s.emit(ctx, webhook.DiscussionCommented, author, repoUUID, map[string]any{"num": num, "comment_id": e.ID.String()})
	return e, nil
}

func setStatus(ctx context.Context, tx bun.Tx, d *discussionRow, status string) error {
	d.Status = status
	_, err := tx.NewUpdate().Model(&d.Discussion).Column("status", "updated_at").WherePK().Exec(ctx)
	return err
}

// SetStatus implements community.Service: open or close a discussion or pull
// request. Merged pull requests cannot change status.
func (s *DB) SetStatus(ctx context.Context, repoUUID uuid.UUID, num int, status, comment string, actor model.Principal) (*community.Event, error) {
	if status != community.StatusOpen && status != community.StatusClosed {
		return nil, fmt.Errorf("%w: status must be open or closed", community.ErrInvalid)
	}
	if err := checkText("", comment); err != nil {
		return nil, err
	}
	e, err := s.addEvent(ctx, repoUUID, num, &db.DiscussionEvent{Type: community.EventStatusChange, AuthorID: authorID(actor), Body: comment,
		Data: eventData(map[string]any{"status": status}, actor)}, func(ctx context.Context, tx bun.Tx, d *discussionRow) error {
		if d.Status == community.StatusMerged {
			return community.ErrNotOpen
		}
		return setStatus(ctx, tx, d, status)
	})
	if err != nil {
		return nil, err
	}
	e.Author = actor.Username
	s.emit(ctx, webhook.DiscussionStatusChanged, actor, repoUUID, map[string]any{"num": num, "status": status})
	return e, nil
}

// Rename implements community.Service.
func (s *DB) Rename(ctx context.Context, repoUUID uuid.UUID, num int, title string, actor model.Principal) (*community.Event, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		return nil, fmt.Errorf("%w: a title is required", community.ErrInvalid)
	}
	if err := checkText(title, ""); err != nil {
		return nil, err
	}
	data := map[string]any{"to": title}
	e, err := s.addEvent(ctx, repoUUID, num, &db.DiscussionEvent{Type: community.EventTitleChange, AuthorID: authorID(actor)},
		func(ctx context.Context, tx bun.Tx, d *discussionRow) error {
			data["from"] = d.Title
			d.Title = title
			_, err := tx.NewUpdate().Model(&d.Discussion).Column("title", "updated_at").WherePK().Exec(ctx)
			return err
		})
	if err != nil {
		return nil, err
	}
	raw := eventData(data, actor)
	if _, err := s.o.DB.NewUpdate().Model((*db.DiscussionEvent)(nil)).Set("data = ?", string(raw)).Where("id = ?", e.ID).Exec(ctx); err != nil {
		return nil, err
	}
	e.Data, e.Author = data, actor.Username
	return e, nil
}

func (s *DB) eventRow(ctx context.Context, repoUUID uuid.UUID, num int, eventID uuid.UUID) (*db.DiscussionEvent, error) {
	d, err := s.row(ctx, s.o.DB, repoUUID, num)
	if err != nil {
		return nil, err
	}
	var ev db.DiscussionEvent
	err = s.o.DB.NewSelect().Model(&ev).Where("id = ?", eventID).Where("discussion_id = ?", d.ID).Where("type = ?", community.EventComment).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: comment %s", community.ErrNotFound, eventID)
	}
	return &ev, err
}

func (s *DB) eventByID(ctx context.Context, id uuid.UUID) (*community.Event, error) {
	var rows []eventRow
	err := s.o.DB.NewSelect().Model(&rows).ModelTableExpr("discussion_events AS discussion_event").ColumnExpr("discussion_event.*").ColumnExpr("an.name AS author_name").
		Join(fmt.Sprintf(authorJoin, "discussion_event")).Where("discussion_event.id = ?", id).Scan(ctx)
	if err != nil || len(rows) == 0 {
		return nil, fmt.Errorf("%w: event %s", community.ErrNotFound, id)
	}
	e := toEvent(&rows[0])
	return &e, nil
}

// EditComment implements community.Service.
func (s *DB) EditComment(ctx context.Context, repoUUID uuid.UUID, num int, eventID uuid.UUID, body string, _ model.Principal) (*community.Event, error) {
	if strings.TrimSpace(body) == "" {
		return nil, fmt.Errorf("%w: the comment is empty", community.ErrInvalid)
	}
	if err := checkText("", body); err != nil {
		return nil, err
	}
	ev, err := s.eventRow(ctx, repoUUID, num, eventID)
	if err != nil {
		return nil, err
	}
	if ev.Hidden {
		return nil, fmt.Errorf("%w: the comment is hidden", community.ErrInvalid)
	}
	var data map[string]any
	_ = json.Unmarshal(ev.Data, &data)
	if data == nil {
		data = map[string]any{}
	}
	data["edited"] = true
	raw, _ := json.Marshal(data)
	if _, err := s.o.DB.NewUpdate().Model((*db.DiscussionEvent)(nil)).Set("body = ?", body).Set("data = ?", string(raw)).
		Set("updated_at = ?", db.Now()).Where("id = ?", eventID).Exec(ctx); err != nil {
		return nil, err
	}
	return s.eventByID(ctx, eventID)
}

// HideComment implements community.Service. The text is removed, not kept.
func (s *DB) HideComment(ctx context.Context, repoUUID uuid.UUID, num int, eventID uuid.UUID, _ model.Principal) (*community.Event, error) {
	if _, err := s.eventRow(ctx, repoUUID, num, eventID); err != nil {
		return nil, err
	}
	if _, err := s.o.DB.NewUpdate().Model((*db.DiscussionEvent)(nil)).Set("hidden = ?", true).Set("body = ?", "").
		Set("updated_at = ?", db.Now()).Where("id = ?", eventID).Exec(ctx); err != nil {
		return nil, err
	}
	return s.eventByID(ctx, eventID)
}

// Discard implements community.Service.
func (s *DB) Discard(ctx context.Context, repoUUID uuid.UUID, num int) error {
	d, err := s.Get(ctx, repoUUID, num)
	if err != nil {
		return err
	}
	if !d.IsPullRequest() {
		return community.ErrNotPullRequest
	}
	if len(d.Events) > 1 || (len(d.Events) == 1 && d.Events[0].Type != community.EventComment) {
		return community.ErrNotDiscardable
	}
	if r, err := s.o.Repos.GetByUUID(ctx, repoUUID); err == nil {
		if _, err := s.o.Repos.Resolve(ctx, r.ID, d.PRRef); err == nil {
			return community.ErrNotDiscardable
		}
	}
	_, err = s.o.DB.NewDelete().Model((*db.Discussion)(nil)).Where("repo_id = ?", repoUUID).Where("num = ?", num).Exec(ctx)
	return err
}

// Like implements community.Service. Liking twice is not an error.
func (s *DB) Like(ctx context.Context, userID, repoUUID uuid.UUID) error {
	if userID == uuid.Nil {
		return fmt.Errorf("%w: sign in to like a Repo", community.ErrInvalid)
	}
	_, err := s.o.DB.NewInsert().Model(&db.Like{UserID: userID, RepoID: repoUUID}).On("CONFLICT DO NOTHING").Exec(ctx)
	return err
}

// Unlike implements community.Service.
func (s *DB) Unlike(ctx context.Context, userID, repoUUID uuid.UUID) error {
	_, err := s.o.DB.NewDelete().Model((*db.Like)(nil)).Where("user_id = ?", userID).Where("repo_id = ?", repoUUID).Exec(ctx)
	return err
}

// Likes implements community.Service.
func (s *DB) Likes(ctx context.Context, repoUUID uuid.UUID) (int64, error) {
	return s.o.DB.NewSelect().Model((*db.Like)(nil)).Where("repo_id = ?", repoUUID).Count(ctx)
}

// Liked implements community.Service.
func (s *DB) Liked(ctx context.Context, userID, repoUUID uuid.UUID) (bool, error) {
	return s.o.DB.NewSelect().Model((*db.Like)(nil)).Where("user_id = ?", userID).Where("repo_id = ?", repoUUID).Exists(ctx)
}

// Likers implements community.Service.
func (s *DB) Likers(ctx context.Context, repoUUID uuid.UUID) ([]string, error) {
	var names []string
	err := s.o.DB.NewSelect().TableExpr("likes AS l").ColumnExpr("n.name").
		Join("JOIN users AS u ON u.id = l.user_id").Join("JOIN namespaces AS n ON n.id = u.namespace_id").
		Where("l.repo_id = ?", repoUUID).OrderExpr("lower(n.name) ASC").Scan(ctx, &names)
	if names == nil {
		names = []string{}
	}
	return names, err
}

// LikedBy implements community.Service.
func (s *DB) LikedBy(ctx context.Context, userID uuid.UUID) ([]uuid.UUID, error) {
	var rows []db.Like
	if err := s.o.DB.NewSelect().Model(&rows).Where("user_id = ?", userID).Order("created_at DESC").Scan(ctx); err != nil {
		return nil, err
	}
	out := make([]uuid.UUID, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.RepoID)
	}
	return out, nil
}

func dayOf(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// RecordDownload implements community.Service.
func (s *DB) RecordDownload(ctx context.Context, repoUUID uuid.UUID, at time.Time) error {
	row := &db.DownloadCount{RepoID: repoUUID, Day: dayOf(at), Count: 1}
	_, err := s.o.DB.NewInsert().Model(row).
		On("CONFLICT (repo_id, day) DO UPDATE").Set("count = download_count.count + 1").Set("updated_at = EXCLUDED.updated_at").
		Exec(ctx)
	return err
}

// Downloads implements community.Service.
func (s *DB) Downloads(ctx context.Context, repoUUID uuid.UUID, since time.Time) ([]community.DayCount, error) {
	var rows []db.DownloadCount
	q := s.o.DB.NewSelect().Model(&rows).Where("repo_id = ?", repoUUID).Order("day ASC")
	if !since.IsZero() {
		q = q.Where("day >= ?", dayOf(since))
	}
	if err := q.Scan(ctx); err != nil {
		return nil, err
	}
	out := make([]community.DayCount, 0, len(rows))
	for _, r := range rows {
		out = append(out, community.DayCount{Day: dayOf(r.Day), Count: r.Count})
	}
	return out, nil
}

// countBatch is how many Repos one LikesOf or DownloadsOf query counts.
const countBatch = 500

// LikesOf implements community.Counter with one query per 500 Repos.
func (s *DB) LikesOf(ctx context.Context, repoUUIDs []uuid.UUID) (map[uuid.UUID]int64, error) {
	return s.countOf(ctx, repoUUIDs, func(q *bun.SelectQuery) *bun.SelectQuery {
		return q.Model((*db.Like)(nil)).ColumnExpr("repo_id, COUNT(*) AS n")
	})
}

// DownloadsOf implements community.Counter with one query per 500 Repos.
func (s *DB) DownloadsOf(ctx context.Context, repoUUIDs []uuid.UUID, since time.Time) (map[uuid.UUID]int64, error) {
	return s.countOf(ctx, repoUUIDs, func(q *bun.SelectQuery) *bun.SelectQuery {
		q = q.Model((*db.DownloadCount)(nil)).ColumnExpr("repo_id, CAST(SUM(count) AS BIGINT) AS n")
		if !since.IsZero() {
			q = q.Where("day >= ?", dayOf(since))
		}
		return q
	})
}

// countOf runs a per-Repo count for every Repo, with no row for Repos
// counted zero.
func (s *DB) countOf(ctx context.Context, repoUUIDs []uuid.UUID, count func(*bun.SelectQuery) *bun.SelectQuery) (map[uuid.UUID]int64, error) {
	out := make(map[uuid.UUID]int64, len(repoUUIDs))
	for start := 0; start < len(repoUUIDs); start += countBatch {
		var rows []struct {
			RepoID uuid.UUID `bun:"repo_id"`
			N      int64     `bun:"n"`
		}
		q := count(s.o.DB.NewSelect()).Where("repo_id IN (?)", bun.List(repoUUIDs[start:min(start+countBatch, len(repoUUIDs))])).
			GroupExpr("repo_id")
		if err := q.Scan(ctx, &rows); err != nil {
			return nil, err
		}
		for _, r := range rows {
			out[r.RepoID] = r.N
		}
	}
	return out, nil
}

// fileState is one path in a tree, for diffs.
type fileState struct {
	OID string
	LFS *model.LFSInfo
}

func (s *DB) treeFiles(ctx context.Context, id model.RepoID, commit model.CommitSHA) (map[string]fileState, error) {
	entries, err := s.o.Repos.Tree(ctx, id, commit, "", repo.TreeOpts{Recursive: true})
	if err != nil {
		return nil, err
	}
	out := make(map[string]fileState, len(entries))
	for _, e := range entries {
		if e.Type == model.EntryFile {
			out[e.Path] = fileState{OID: e.OID, LFS: e.LFS}
		}
	}
	return out, nil
}

// changed returns the paths whose content differs between two trees,
// sorted; a path missing from to is deleted.
func changed(from, to map[string]fileState) []string {
	var out []string
	for p, f := range to {
		if g, ok := from[p]; !ok || g.OID != f.OID {
			out = append(out, p)
		}
	}
	for p := range from {
		if _, ok := to[p]; !ok {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// MergeBase returns the newest Commit on the first-parent history of head
// that is also on base's history.
func MergeBase(ctx context.Context, repos repo.Service, id model.RepoID, base, head model.CommitSHA) (model.CommitSHA, error) {
	const window = 10000
	baseLog, err := repos.Log(ctx, id, string(base), repo.LogOpts{Limit: window})
	if err != nil {
		return "", err
	}
	onBase := make(map[model.CommitSHA]bool, len(baseLog))
	for _, c := range baseLog {
		onBase[c.SHA] = true
	}
	headLog, err := repos.Log(ctx, id, string(head), repo.LogOpts{Limit: window})
	if err != nil {
		return "", err
	}
	for _, c := range headLog {
		if onBase[c.SHA] {
			return c.SHA, nil
		}
	}
	return "", fmt.Errorf("%w: the pull request shares no history with its base", community.ErrConflict)
}

// Merge implements community.Service. The pull request's changes since it branched
// from its base are applied to the base branch as one Commit titled
// "<title> (#N)", as the Hub does. If the base changed any of the same
// files since then, Merge returns community.ErrConflict and changes nothing.
func (s *DB) Merge(ctx context.Context, repoUUID uuid.UUID, num int, comment string, actor model.Principal) (*community.Event, error) {
	if err := checkText("", comment); err != nil {
		return nil, err
	}
	d, err := s.Get(ctx, repoUUID, num)
	if err != nil {
		return nil, err
	}
	if !d.IsPullRequest() {
		return nil, community.ErrNotPullRequest
	}
	if d.Status != community.StatusOpen && d.Status != community.StatusDraft {
		return nil, community.ErrNotOpen
	}
	r, err := s.o.Repos.GetByUUID(ctx, repoUUID)
	if err != nil {
		return nil, err
	}
	head, err := s.o.Repos.Resolve(ctx, r.ID, d.PRRef)
	if errors.Is(err, repo.ErrRevisionNotFound) {
		return nil, fmt.Errorf("%w: the pull request has no commits", community.ErrInvalid)
	}
	if err != nil {
		return nil, err
	}
	baseBranch := d.PRBase
	if baseBranch == "" {
		baseBranch = r.DefaultBranch
	}
	baseTip, err := s.o.Repos.Resolve(ctx, r.ID, baseBranch)
	if err != nil {
		return nil, err
	}
	fork, err := MergeBase(ctx, s.o.Repos, r.ID, baseTip, head)
	if err != nil {
		return nil, err
	}
	if fork == head {
		return nil, fmt.Errorf("%w: the pull request has no changes to merge", community.ErrInvalid)
	}
	forkFiles, err := s.treeFiles(ctx, r.ID, fork)
	if err != nil {
		return nil, err
	}
	headFiles, err := s.treeFiles(ctx, r.ID, head)
	if err != nil {
		return nil, err
	}
	prPaths := changed(forkFiles, headFiles)
	if fork != baseTip {
		baseFiles, err := s.treeFiles(ctx, r.ID, baseTip)
		if err != nil {
			return nil, err
		}
		baseChanged := map[string]bool{}
		for _, p := range changed(forkFiles, baseFiles) {
			baseChanged[p] = true
		}
		var conflicts []string
		for _, p := range prPaths {
			if baseChanged[p] && baseFiles[p] != headFiles[p] {
				conflicts = append(conflicts, p)
			}
		}
		if len(conflicts) > 0 {
			return nil, fmt.Errorf("%w: %s", community.ErrConflict, strings.Join(conflicts, ", "))
		}
	}
	var ops []repo.Operation
	for _, p := range prPaths {
		f, ok := headFiles[p]
		switch {
		case !ok:
			ops = append(ops, repo.Operation{Kind: repo.OpDelete, Path: p})
		case f.LFS != nil:
			ops = append(ops, repo.Operation{Kind: repo.OpAddLFS, Path: p, OID: f.LFS.OID, Size: f.LFS.Size})
		default:
			rc, _, err := s.o.Repos.OpenFile(ctx, r.ID, head, p, nil)
			if err != nil {
				return nil, err
			}
			b, err := io.ReadAll(rc)
			_ = rc.Close()
			if err != nil {
				return nil, err
			}
			ops = append(ops, repo.Operation{Kind: repo.OpAdd, Path: p, Content: bytes.NewReader(b)})
		}
	}
	if len(ops) == 0 {
		return nil, fmt.Errorf("%w: the pull request has no changes to merge", community.ErrInvalid)
	}
	res, err := s.o.Repos.Commit(ctx, repo.CommitParams{Repo: r.ID, Revision: baseBranch, ParentCommit: baseTip,
		Summary: d.Title + " (#" + strconv.Itoa(num) + ")", Description: comment, Operations: ops, Actor: actor})
	if err != nil {
		return nil, err
	}
	e, err := s.addEvent(ctx, repoUUID, num, &db.DiscussionEvent{Type: community.EventStatusChange, AuthorID: authorID(actor), Body: comment,
		Data: eventData(map[string]any{"status": community.StatusMerged, "oid": string(res.SHA)}, actor)},
		func(ctx context.Context, tx bun.Tx, row *discussionRow) error {
			if row.Status != community.StatusOpen && row.Status != community.StatusDraft {
				return community.ErrNotOpen
			}
			return setStatus(ctx, tx, row, community.StatusMerged)
		})
	if err != nil {
		return nil, err
	}
	e.Author = actor.Username
	s.emit(ctx, webhook.PRMerged, actor, repoUUID, map[string]any{"num": num, "commit": string(res.SHA), "base": baseBranch})
	return e, nil
}

// Changes summarises the files a pull request changes since it branched
// from its base, for the files-changed view.
func (s *DB) Changes(ctx context.Context, id model.RepoID, d *community.Discussion, defaultBranch string) ([]repo.FileChange, error) {
	return PRChanges(ctx, s.o.Repos, id, d, defaultBranch)
}

// PRChanges summarises the files a pull request changes since it branched
// from its base. A pull request with no commits has no changes.
func PRChanges(ctx context.Context, repos repo.Service, id model.RepoID, d *community.Discussion, defaultBranch string) ([]repo.FileChange, error) {
	if !d.IsPullRequest() {
		return nil, community.ErrNotPullRequest
	}
	head, err := repos.Resolve(ctx, id, d.PRRef)
	if errors.Is(err, repo.ErrRevisionNotFound) {
		return []repo.FileChange{}, nil
	}
	if err != nil {
		return nil, err
	}
	base := d.PRBase
	if base == "" {
		base = defaultBranch
	}
	baseTip, err := repos.Resolve(ctx, id, base)
	if err != nil {
		return nil, err
	}
	fork, err := MergeBase(ctx, repos, id, baseTip, head)
	if err != nil {
		return nil, err
	}
	from, err := treeFiles(ctx, repos, id, fork)
	if err != nil {
		return nil, err
	}
	to, err := treeFiles(ctx, repos, id, head)
	if err != nil {
		return nil, err
	}
	out := []repo.FileChange{}
	for _, p := range changed(from, to) {
		a, inFrom := from[p]
		b, inTo := to[p]
		c := repo.FileChange{Path: p, OldOID: a.OID, NewOID: b.OID, LFS: a.LFS != nil || b.LFS != nil}
		switch {
		case !inFrom:
			c.Status = repo.ChangeAdded
		case !inTo:
			c.Status = repo.ChangeDeleted
		default:
			c.Status = repo.ChangeModified
		}
		if a.LFS != nil {
			c.OldSize = a.LFS.Size
		}
		if b.LFS != nil {
			c.NewSize = b.LFS.Size
		}
		out = append(out, c)
	}
	return out, nil
}

func treeFiles(ctx context.Context, repos repo.Service, id model.RepoID, commit model.CommitSHA) (map[string]fileState, error) {
	return (&DB{o: Options{Repos: repos}}).treeFiles(ctx, id, commit)
}
