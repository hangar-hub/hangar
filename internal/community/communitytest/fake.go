// Package communitytest provides an in-memory community.Service. Merge
// only changes the status; it does not move refs.
package communitytest

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/jamesagarside/hangar/internal/community"
	"github.com/jamesagarside/hangar/internal/model"
)

// Fake is an in-memory community.Service.
type Fake struct {
	mu          sync.Mutex
	discussions map[uuid.UUID][]*community.Discussion
	likes       map[uuid.UUID]map[uuid.UUID]bool
	downloads   map[uuid.UUID]map[time.Time]int64
	now         func() time.Time
}

var _ community.Service = (*Fake)(nil)

// NewFake returns an empty Fake.
func NewFake() *Fake {
	return &Fake{discussions: map[uuid.UUID][]*community.Discussion{}, likes: map[uuid.UUID]map[uuid.UUID]bool{},
		downloads: map[uuid.UUID]map[time.Time]int64{}, now: func() time.Time { return time.Now().UTC() }}
}

func clone(d *community.Discussion) community.Discussion {
	out := *d
	out.Events = append([]community.Event(nil), d.Events...)
	return out
}

func (f *Fake) find(repoUUID uuid.UUID, num int) (*community.Discussion, error) {
	for _, d := range f.discussions[repoUUID] {
		if d.Num == num {
			return d, nil
		}
	}
	return nil, fmt.Errorf("%w: #%d", community.ErrNotFound, num)
}

// List implements community.Service, newest first.
func (f *Fake) List(_ context.Context, repoUUID uuid.UUID, opts community.ListOptions) ([]community.Discussion, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []community.Discussion
	for _, d := range f.discussions[repoUUID] {
		if opts.Type != "" && d.Kind != opts.Type {
			continue
		}
		if opts.Status == community.StatusOpen && d.Status != community.StatusOpen && d.Status != community.StatusDraft {
			continue
		}
		if opts.Status == community.StatusClosed && d.Status != community.StatusClosed && d.Status != community.StatusMerged {
			continue
		}
		if opts.Author != "" && !strings.EqualFold(d.Author, opts.Author) {
			continue
		}
		out = append(out, clone(d))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Num > out[j].Num })
	total := len(out)
	if opts.Limit > 0 {
		start := min(opts.Page*opts.Limit, len(out))
		out = out[start:min(start+opts.Limit, len(out))]
	}
	return out, total, nil
}

// Get implements community.Service.
func (f *Fake) Get(_ context.Context, repoUUID uuid.UUID, num int) (*community.Discussion, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, err := f.find(repoUUID, num)
	if err != nil {
		return nil, err
	}
	out := clone(d)
	return &out, nil
}

// Create implements community.Service.
func (f *Fake) Create(_ context.Context, p community.CreateParams) (*community.Discussion, error) {
	if strings.TrimSpace(p.Title) == "" {
		return nil, fmt.Errorf("%w: a title is required", community.ErrInvalid)
	}
	if p.Author.IsAnonymous() {
		return nil, fmt.Errorf("%w: sign in to open a discussion", community.ErrInvalid)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	d := &community.Discussion{Num: len(f.discussions[p.RepoUUID]) + 1, Kind: community.KindDiscussion, Title: p.Title,
		Status: community.StatusOpen, Author: p.Author.Username, CreatedAt: f.now()}
	if p.PullRequest {
		d.Kind = community.KindPullRequest
		d.Status = community.StatusDraft
		d.PRRef = model.PRRef(d.Num)
		d.PRBase = p.Base
		if d.PRBase == "" {
			d.PRBase = model.DefaultBranch
		}
	}
	if p.Description != "" {
		d.Events = append(d.Events, community.Event{ID: uuid.New(), Type: community.EventComment, Author: p.Author.Username, Body: p.Description, CreatedAt: d.CreatedAt})
	}
	f.discussions[p.RepoUUID] = append(f.discussions[p.RepoUUID], d)
	out := clone(d)
	return &out, nil
}

func (f *Fake) add(repoUUID uuid.UUID, num int, e community.Event, change func(*community.Discussion) error) (*community.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, err := f.find(repoUUID, num)
	if err != nil {
		return nil, err
	}
	if change != nil {
		if err := change(d); err != nil {
			return nil, err
		}
	}
	e.ID, e.CreatedAt = uuid.New(), f.now()
	d.Events = append(d.Events, e)
	return &e, nil
}

// Comment implements community.Service.
func (f *Fake) Comment(_ context.Context, repoUUID uuid.UUID, num int, body string, author model.Principal) (*community.Event, error) {
	if strings.TrimSpace(body) == "" {
		return nil, fmt.Errorf("%w: the comment is empty", community.ErrInvalid)
	}
	return f.add(repoUUID, num, community.Event{Type: community.EventComment, Author: author.Username, Body: body}, nil)
}

// SetStatus implements community.Service.
func (f *Fake) SetStatus(_ context.Context, repoUUID uuid.UUID, num int, status, comment string, actor model.Principal) (*community.Event, error) {
	if status != community.StatusOpen && status != community.StatusClosed {
		return nil, fmt.Errorf("%w: status must be open or closed", community.ErrInvalid)
	}
	return f.add(repoUUID, num, community.Event{Type: community.EventStatusChange, Author: actor.Username, Body: comment, Data: map[string]any{"status": status}},
		func(d *community.Discussion) error {
			if d.Status == community.StatusMerged {
				return community.ErrNotOpen
			}
			d.Status = status
			return nil
		})
}

// Rename implements community.Service.
func (f *Fake) Rename(_ context.Context, repoUUID uuid.UUID, num int, title string, actor model.Principal) (*community.Event, error) {
	if strings.TrimSpace(title) == "" {
		return nil, fmt.Errorf("%w: a title is required", community.ErrInvalid)
	}
	return f.add(repoUUID, num, community.Event{Type: community.EventTitleChange, Author: actor.Username, Data: map[string]any{"to": title}},
		func(d *community.Discussion) error { d.Title = title; return nil })
}

// Merge implements community.Service.
func (f *Fake) Merge(_ context.Context, repoUUID uuid.UUID, num int, comment string, actor model.Principal) (*community.Event, error) {
	return f.add(repoUUID, num, community.Event{Type: community.EventStatusChange, Author: actor.Username, Body: comment, Data: map[string]any{"status": community.StatusMerged}},
		func(d *community.Discussion) error {
			if !d.IsPullRequest() {
				return community.ErrNotPullRequest
			}
			if d.Status != community.StatusOpen && d.Status != community.StatusDraft {
				return community.ErrNotOpen
			}
			d.Status = community.StatusMerged
			return nil
		})
}

// Like implements community.Service.
func (f *Fake) Like(_ context.Context, userID, repoUUID uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.likes[repoUUID] == nil {
		f.likes[repoUUID] = map[uuid.UUID]bool{}
	}
	f.likes[repoUUID][userID] = true
	return nil
}

// Unlike implements community.Service.
func (f *Fake) Unlike(_ context.Context, userID, repoUUID uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.likes[repoUUID], userID)
	return nil
}

// Likes implements community.Service.
func (f *Fake) Likes(_ context.Context, repoUUID uuid.UUID) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return int64(len(f.likes[repoUUID])), nil
}

func day(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// RecordDownload implements community.Service.
func (f *Fake) RecordDownload(_ context.Context, repoUUID uuid.UUID, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.downloads[repoUUID] == nil {
		f.downloads[repoUUID] = map[time.Time]int64{}
	}
	f.downloads[repoUUID][day(at)]++
	return nil
}

// Downloads implements community.Service.
func (f *Fake) Downloads(_ context.Context, repoUUID uuid.UUID, since time.Time) ([]community.DayCount, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []community.DayCount
	for d, n := range f.downloads[repoUUID] {
		if !d.Before(day(since)) {
			out = append(out, community.DayCount{Day: d, Count: n})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Day.Before(out[j].Day) })
	return out, nil
}

func (f *Fake) editEvent(repoUUID uuid.UUID, num int, eventID uuid.UUID, change func(*community.Event)) (*community.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, err := f.find(repoUUID, num)
	if err != nil {
		return nil, err
	}
	for i := range d.Events {
		if d.Events[i].ID == eventID && d.Events[i].Type == community.EventComment {
			change(&d.Events[i])
			e := d.Events[i]
			return &e, nil
		}
	}
	return nil, fmt.Errorf("%w: comment %s", community.ErrNotFound, eventID)
}

// EditComment implements community.Service.
func (f *Fake) EditComment(_ context.Context, repoUUID uuid.UUID, num int, eventID uuid.UUID, body string, _ model.Principal) (*community.Event, error) {
	if strings.TrimSpace(body) == "" {
		return nil, fmt.Errorf("%w: the comment is empty", community.ErrInvalid)
	}
	return f.editEvent(repoUUID, num, eventID, func(e *community.Event) { e.Body, e.Edited = body, true })
}

// HideComment implements community.Service.
func (f *Fake) HideComment(_ context.Context, repoUUID uuid.UUID, num int, eventID uuid.UUID, _ model.Principal) (*community.Event, error) {
	return f.editEvent(repoUUID, num, eventID, func(e *community.Event) { e.Body, e.Hidden = "", true })
}

// Discard implements community.Service. The fake has no refs, so a pull
// request is discardable while it has at most its description event.
func (f *Fake) Discard(_ context.Context, repoUUID uuid.UUID, num int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, err := f.find(repoUUID, num)
	if err != nil {
		return err
	}
	if !d.IsPullRequest() {
		return community.ErrNotPullRequest
	}
	if len(d.Events) > 1 {
		return community.ErrNotDiscardable
	}
	list := f.discussions[repoUUID]
	for i, x := range list {
		if x == d {
			f.discussions[repoUUID] = append(list[:i:i], list[i+1:]...)
		}
	}
	return nil
}

// Liked implements community.Service.
func (f *Fake) Liked(_ context.Context, userID, repoUUID uuid.UUID) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.likes[repoUUID][userID], nil
}

// Likers implements community.Service. The fake knows user IDs, not
// names, so it returns the IDs as strings in order.
func (f *Fake) Likers(_ context.Context, repoUUID uuid.UUID) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []string{}
	for u := range f.likes[repoUUID] {
		out = append(out, u.String())
	}
	sort.Strings(out)
	return out, nil
}

// LikedBy implements community.Service, in Repo UUID order.
func (f *Fake) LikedBy(_ context.Context, userID uuid.UUID) ([]uuid.UUID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []uuid.UUID{}
	for r, users := range f.likes {
		if users[userID] {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out, nil
}
