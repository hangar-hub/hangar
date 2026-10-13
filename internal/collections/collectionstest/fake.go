// Package collectionstest provides an in-memory collections.Service. A
// user may change Collections in the Namespace named after them; Instance
// admins may change any.
package collectionstest

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/jamesagarside/hangar/internal/collections"
	"github.com/jamesagarside/hangar/internal/model"
)

// Fake is an in-memory collections.Service.
type Fake struct {
	// CanWrite decides who may change a Namespace's Collections; nil means
	// the user named after the Namespace and Instance admins.
	CanWrite func(actor model.Principal, namespace string) bool

	mu   sync.Mutex
	cols []*collections.Collection
}

var _ collections.Service = (*Fake)(nil)

// NewFake returns an empty Fake.
func NewFake() *Fake { return &Fake{} }

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

// Slugify turns a title into a slug.
func Slugify(title string) string {
	return strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(title), "-"), "-")
}

func (f *Fake) writable(actor model.Principal, ns string) bool {
	if f.CanWrite != nil {
		return f.CanWrite(actor, ns)
	}
	return actor.IsAdmin || (!actor.IsAnonymous() && strings.EqualFold(actor.Username, ns))
}

func visible(viewer model.Principal, c *collections.Collection) bool {
	switch c.Visibility {
	case model.VisibilityPublic:
		return true
	case model.VisibilityInternal:
		return !viewer.IsAnonymous()
	default:
		return viewer.IsAdmin || strings.EqualFold(viewer.Username, c.Namespace)
	}
}

func clone(c *collections.Collection) collections.Collection {
	out := *c
	out.Items = slices.Clone(c.Items)
	return out
}

func (f *Fake) find(viewer model.Principal, ns, slug string) (*collections.Collection, error) {
	for _, c := range f.cols {
		if strings.EqualFold(c.Namespace, ns) && strings.EqualFold(c.Slug, slug) && visible(viewer, c) {
			return c, nil
		}
	}
	return nil, fmt.Errorf("%w: %s/%s", collections.ErrNotFound, ns, slug)
}

func (f *Fake) findWritable(actor model.Principal, ns, slug string) (*collections.Collection, error) {
	c, err := f.find(actor, ns, slug)
	if err != nil {
		return nil, err
	}
	if !f.writable(actor, ns) {
		return nil, collections.ErrForbidden
	}
	return c, nil
}

// List implements collections.Service.
func (f *Fake) List(_ context.Context, viewer model.Principal, opts collections.ListOptions) ([]collections.Collection, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []collections.Collection
	for _, c := range f.cols {
		if !visible(viewer, c) {
			continue
		}
		if opts.Owner != "" && !strings.EqualFold(opts.Owner, c.Namespace) {
			continue
		}
		if opts.Query != "" && !strings.Contains(strings.ToLower(c.Title), strings.ToLower(opts.Query)) {
			continue
		}
		if opts.Item != "" && !slices.ContainsFunc(c.Items, func(i collections.Item) bool { return i.Type+"s/"+i.ItemID == opts.Item }) {
			continue
		}
		out = append(out, clone(c))
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	if opts.Limit > 0 && len(out) > opts.Limit {
		out = out[:opts.Limit]
	}
	return out, nil
}

// Get implements collections.Service.
func (f *Fake) Get(_ context.Context, viewer model.Principal, ns, slug string) (*collections.Collection, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, err := f.find(viewer, ns, slug)
	if err != nil {
		return nil, err
	}
	out := clone(c)
	return &out, nil
}

// Create implements collections.Service.
func (f *Fake) Create(_ context.Context, actor model.Principal, p collections.CreateParams) (*collections.Collection, error) {
	slug := Slugify(p.Title)
	if slug == "" {
		return nil, fmt.Errorf("%w: a title is required", collections.ErrInvalid)
	}
	if !f.writable(actor, p.Namespace) {
		return nil, collections.ErrForbidden
	}
	vis := p.Visibility
	if vis == "" {
		vis = model.VisibilityPublic
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.cols {
		if strings.EqualFold(c.Namespace, p.Namespace) && c.Slug == slug {
			if p.ExistOK {
				out := clone(c)
				return &out, nil
			}
			return nil, collections.ErrExists
		}
	}
	c := &collections.Collection{ID: uuid.New(), Namespace: p.Namespace, Slug: slug, Title: p.Title, Description: p.Description,
		Visibility: vis, Items: []collections.Item{}, UpdatedAt: time.Now().UTC()}
	f.cols = append(f.cols, c)
	out := clone(c)
	return &out, nil
}

// Update implements collections.Service.
func (f *Fake) Update(_ context.Context, actor model.Principal, ns, slug string, u collections.Update) (*collections.Collection, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, err := f.findWritable(actor, ns, slug)
	if err != nil {
		return nil, err
	}
	if u.Visibility != nil && !u.Visibility.Valid() {
		return nil, fmt.Errorf("%w: visibility", collections.ErrInvalid)
	}
	if u.Title != nil {
		c.Title = *u.Title
	}
	if u.Description != nil {
		c.Description = *u.Description
	}
	if u.Visibility != nil {
		c.Visibility = *u.Visibility
	}
	if u.Theme != nil {
		c.Theme = *u.Theme
	}
	c.UpdatedAt = time.Now().UTC()
	out := clone(c)
	return &out, nil
}

// Delete implements collections.Service.
func (f *Fake) Delete(_ context.Context, actor model.Principal, ns, slug string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, err := f.findWritable(actor, ns, slug)
	if err != nil {
		return err
	}
	f.cols = slices.DeleteFunc(f.cols, func(x *collections.Collection) bool { return x == c })
	return nil
}

func renumber(c *collections.Collection) {
	for i := range c.Items {
		c.Items[i].Position = i
	}
	c.UpdatedAt = time.Now().UTC()
}

// AddItem implements collections.Service.
func (f *Fake) AddItem(_ context.Context, actor model.Principal, ns, slug string, p collections.ItemParams) (*collections.Collection, error) {
	if p.Type != collections.ItemModel && p.Type != collections.ItemDataset && p.Type != collections.ItemCollection {
		return nil, fmt.Errorf("%w: item type %q", collections.ErrInvalid, p.Type)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	c, err := f.findWritable(actor, ns, slug)
	if err != nil {
		return nil, err
	}
	for _, it := range c.Items {
		if it.Type == p.Type && it.ItemID == p.ItemID {
			if p.ExistOK {
				out := clone(c)
				return &out, nil
			}
			return nil, collections.ErrExists
		}
	}
	c.Items = append(c.Items, collections.Item{ID: uuid.New(), Type: p.Type, ItemID: p.ItemID, Note: p.Note})
	renumber(c)
	out := clone(c)
	return &out, nil
}

// UpdateItem implements collections.Service.
func (f *Fake) UpdateItem(_ context.Context, actor model.Principal, ns, slug string, itemID uuid.UUID, u collections.ItemUpdate) (*collections.Collection, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, err := f.findWritable(actor, ns, slug)
	if err != nil {
		return nil, err
	}
	i := slices.IndexFunc(c.Items, func(it collections.Item) bool { return it.ID == itemID })
	if i < 0 {
		return nil, fmt.Errorf("%w: item %s", collections.ErrNotFound, itemID)
	}
	if u.Note != nil {
		c.Items[i].Note = *u.Note
	}
	if u.Position != nil {
		it := c.Items[i]
		c.Items = slices.Delete(c.Items, i, i+1)
		pos := max(0, min(*u.Position, len(c.Items)))
		c.Items = slices.Insert(c.Items, pos, it)
	}
	renumber(c)
	out := clone(c)
	return &out, nil
}

// DeleteItem implements collections.Service.
func (f *Fake) DeleteItem(_ context.Context, actor model.Principal, ns, slug string, itemID uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, err := f.findWritable(actor, ns, slug)
	if err != nil {
		return err
	}
	n := len(c.Items)
	c.Items = slices.DeleteFunc(c.Items, func(it collections.Item) bool { return it.ID == itemID })
	if len(c.Items) == n {
		return fmt.Errorf("%w: item %s", collections.ErrNotFound, itemID)
	}
	renumber(c)
	return nil
}
