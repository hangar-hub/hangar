package collections

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/uptrace/bun"

	"github.com/jamesagarside/hangar/internal/auth"
	"github.com/jamesagarside/hangar/internal/db"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/org"
	"github.com/jamesagarside/hangar/internal/repo"
)

// Limits.
const (
	MaxTitle       = 60
	MaxDescription = 150
	MaxNote        = 500
	MaxItems       = 1000
	maxSlug        = 80
)

// Options configure the DB service.
type Options struct {
	DB    *db.DB
	Orgs  org.Service
	Repos repo.Service
	Authz auth.Authz
}

// DB is the database-backed Service. Members of a Namespace with the write
// Role, and Instance admins, may change its Collections. Items a viewer
// cannot read are left out of what they see.
type DB struct {
	o Options
}

var _ Service = (*DB)(nil)

// New returns a DB service.
func New(o Options) *DB { return &DB{o: o} }

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

// Slugify turns a title into a slug: lower-case letters and digits joined
// by hyphens.
func Slugify(title string) string {
	s := strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(title), "-"), "-")
	if len(s) > maxSlug {
		s = strings.TrimRight(s[:maxSlug], "-")
	}
	return s
}

func (s *DB) namespace(ctx context.Context, name string) (*org.Namespace, error) {
	ns, err := s.o.Orgs.Get(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("%w: namespace %s", ErrNotFound, name)
	}
	return ns, nil
}

// viewerCache remembers Namespace roles while one call checks many
// Collections.
type viewerCache struct {
	s      *DB
	viewer model.Principal
	roles  map[uuid.UUID]bool
	names  map[uuid.UUID]string
	// public is whether the viewer may see public Collections, asked once.
	public *bool
}

func (s *DB) cache(viewer model.Principal) *viewerCache {
	return &viewerCache{s: s, viewer: viewer, roles: map[uuid.UUID]bool{}, names: map[uuid.UUID]string{}}
}

func (c *viewerCache) member(ctx context.Context, nsID uuid.UUID) bool {
	if c.viewer.IsAdmin {
		return true
	}
	if c.viewer.IsAnonymous() {
		return false
	}
	if v, ok := c.roles[nsID]; ok {
		return v
	}
	role, ok, err := c.s.o.Orgs.RoleIn(ctx, nsID, c.viewer.UserID)
	member := err == nil && ok && role.Includes(model.RoleRead)
	c.roles[nsID] = member
	return member
}

func (c *viewerCache) visible(ctx context.Context, row *db.Collection) bool {
	switch model.Visibility(row.Visibility) {
	case model.VisibilityPublic:
		return c.seesPublic(ctx, row.NamespaceID)
	case model.VisibilityInternal:
		return !c.viewer.IsAnonymous()
	default:
		return c.member(ctx, row.NamespaceID)
	}
}

// seesPublic reports whether the viewer may see public Collections:
// anyone while security.anonymous_read is on, only signed-in viewers
// while it is off, as for the Namespaces that own them (auth.Authorizer).
func (c *viewerCache) seesPublic(ctx context.Context, nsID uuid.UUID) bool {
	if !c.viewer.IsAnonymous() {
		return true
	}
	if c.public == nil {
		ok := c.s.o.Authz.Can(ctx, c.viewer, auth.ActionRead, auth.Target{Namespace: &auth.NamespaceTarget{ID: nsID}}) == nil
		c.public = &ok
	}
	return *c.public
}

func (c *viewerCache) nsName(ctx context.Context, id uuid.UUID) string {
	if n, ok := c.names[id]; ok {
		return n
	}
	ns, err := c.s.o.Orgs.GetByID(ctx, id)
	name := ""
	if err == nil {
		name = ns.Name
	}
	c.names[id] = name
	return name
}

func repoTarget(r *repo.Repo) *auth.RepoTarget {
	return &auth.RepoTarget{ID: r.ID, UUID: r.UUID, NamespaceID: r.NamespaceID, Visibility: r.Visibility, Gated: r.Gating.Enabled(), Mirror: r.Mirror}
}

// itemVisible reports whether the viewer may see an item: a Repo they can
// read, or a Collection they can see.
func (c *viewerCache) itemVisible(ctx context.Context, it *db.CollectionItem) bool {
	switch it.ItemType {
	case ItemModel, ItemDataset:
		t := model.RepoTypeModel
		if it.ItemType == ItemDataset {
			t = model.RepoTypeDataset
		}
		id, err := model.ParseRepoID(t, it.ItemID)
		if err != nil {
			return false
		}
		r, err := c.s.o.Repos.Get(ctx, id)
		if err != nil || r.DeletedAt != nil {
			return false
		}
		return c.s.o.Authz.Can(ctx, c.viewer, auth.ActionRead, auth.Target{Repo: repoTarget(r)}) == nil
	case ItemCollection:
		ns, slug, ok := strings.Cut(it.ItemID, "/")
		if !ok {
			return false
		}
		row, err := c.s.row(ctx, ns, slug)
		return err == nil && c.visible(ctx, row)
	}
	return false
}

func (s *DB) row(ctx context.Context, nsName, slug string) (*db.Collection, error) {
	ns, err := s.namespace(ctx, nsName)
	if err != nil {
		return nil, err
	}
	var row db.Collection
	err = s.o.DB.NewSelect().Model(&row).Where("namespace_id = ?", ns.ID).Where("lower(slug) = lower(?)", slug).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s/%s", ErrNotFound, nsName, slug)
	}
	return &row, err
}

func (s *DB) items(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID][]db.CollectionItem, error) {
	out := map[uuid.UUID][]db.CollectionItem{}
	if len(ids) == 0 {
		return out, nil
	}
	var rows []db.CollectionItem
	if err := s.o.DB.NewSelect().Model(&rows).Where("collection_id IN (?)", bun.List(ids)).Order("collection_id", "position ASC", "id ASC").Scan(ctx); err != nil {
		return nil, err
	}
	for _, r := range rows {
		out[r.CollectionID] = append(out[r.CollectionID], r)
	}
	return out, nil
}

func (c *viewerCache) build(ctx context.Context, row *db.Collection, items []db.CollectionItem) Collection {
	out := Collection{ID: row.ID, Namespace: c.nsName(ctx, row.NamespaceID), Slug: row.Slug, Title: row.Title, Description: row.Description,
		Visibility: model.Visibility(row.Visibility), Theme: row.Theme, Items: []Item{}, UpdatedAt: row.UpdatedAt.UTC()}
	for i := range items {
		it := &items[i]
		if !c.itemVisible(ctx, it) {
			continue
		}
		out.Items = append(out.Items, Item{ID: it.ID, Type: it.ItemType, ItemID: it.ItemID, Position: it.Position, Note: it.Note})
	}
	return out
}

// List implements Service, most recently updated first.
func (s *DB) List(ctx context.Context, viewer model.Principal, o ListOptions) ([]Collection, error) {
	var rows []db.Collection
	q := s.o.DB.NewSelect().Model(&rows).Order("updated_at DESC", "id DESC")
	if o.Owner != "" {
		ns, err := s.o.Orgs.Get(ctx, o.Owner)
		if errors.Is(err, org.ErrNotFound) {
			return []Collection{}, nil
		}
		if err != nil {
			return nil, err
		}
		q = q.Where("namespace_id = ?", ns.ID)
	}
	if o.Query != "" {
		q = q.Where("lower(title) LIKE ? ESCAPE '\\'", "%"+strings.ToLower(escapeLike(o.Query))+"%")
	}
	itemType, itemID := "", ""
	if o.Item != "" {
		plural, rest, ok := strings.Cut(o.Item, "/")
		if !ok || !strings.HasSuffix(plural, "s") {
			return nil, fmt.Errorf("%w: item must look like models/namespace/name", ErrInvalid)
		}
		itemType, itemID = strings.TrimSuffix(plural, "s"), rest
		q = q.Where("id IN (SELECT collection_id FROM collection_items WHERE item_type = ? AND lower(item_id) = lower(?))", itemType, itemID)
	}
	if err := q.Scan(ctx); err != nil {
		return nil, err
	}
	c := s.cache(viewer)
	var visible []db.Collection
	for i := range rows {
		if c.visible(ctx, &rows[i]) {
			visible = append(visible, rows[i])
		}
		if o.Limit > 0 && len(visible) >= o.Limit {
			break
		}
	}
	ids := make([]uuid.UUID, 0, len(visible))
	for _, r := range visible {
		ids = append(ids, r.ID)
	}
	items, err := s.items(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make([]Collection, 0, len(visible))
	for i := range visible {
		out = append(out, c.build(ctx, &visible[i], items[visible[i].ID]))
	}
	return out, nil
}

func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// Get implements Service.
func (s *DB) Get(ctx context.Context, viewer model.Principal, ns, slug string) (*Collection, error) {
	row, err := s.row(ctx, ns, slug)
	if err != nil {
		return nil, err
	}
	c := s.cache(viewer)
	if !c.visible(ctx, row) {
		return nil, fmt.Errorf("%w: %s/%s", ErrNotFound, ns, slug)
	}
	return s.load(ctx, c, row)
}

func (s *DB) load(ctx context.Context, c *viewerCache, row *db.Collection) (*Collection, error) {
	items, err := s.items(ctx, []uuid.UUID{row.ID})
	if err != nil {
		return nil, err
	}
	out := c.build(ctx, row, items[row.ID])
	return &out, nil
}

func (s *DB) canWrite(ctx context.Context, actor model.Principal, ns *org.Namespace) error {
	err := s.o.Authz.Can(ctx, actor, auth.ActionWrite, auth.Target{Namespace: &auth.NamespaceTarget{ID: ns.ID, Name: ns.Name, Kind: ns.Kind}})
	if err != nil {
		return fmt.Errorf("%w: %w", ErrForbidden, err)
	}
	return nil
}

// writable returns a Collection the actor may see and change.
func (s *DB) writable(ctx context.Context, actor model.Principal, nsName, slug string) (*db.Collection, *viewerCache, error) {
	ns, err := s.namespace(ctx, nsName)
	if err != nil {
		return nil, nil, err
	}
	row, err := s.row(ctx, nsName, slug)
	if err != nil {
		return nil, nil, err
	}
	c := s.cache(actor)
	if !c.visible(ctx, row) {
		return nil, nil, fmt.Errorf("%w: %s/%s", ErrNotFound, nsName, slug)
	}
	if err := s.canWrite(ctx, actor, ns); err != nil {
		return nil, nil, err
	}
	return row, c, nil
}

func checkText(title, description string) error {
	if utf8.RuneCountInString(title) > MaxTitle {
		return fmt.Errorf("%w: the title is longer than %d characters", ErrInvalid, MaxTitle)
	}
	if utf8.RuneCountInString(description) > MaxDescription {
		return fmt.Errorf("%w: the description is longer than %d characters", ErrInvalid, MaxDescription)
	}
	return nil
}

// Create implements Service.
func (s *DB) Create(ctx context.Context, actor model.Principal, p CreateParams) (*Collection, error) {
	title := strings.TrimSpace(p.Title)
	slug := Slugify(title)
	if slug == "" {
		return nil, fmt.Errorf("%w: the title needs a letter or digit", ErrInvalid)
	}
	if err := checkText(title, p.Description); err != nil {
		return nil, err
	}
	vis := p.Visibility
	if vis == "" {
		vis = model.VisibilityPublic
	}
	if !vis.Valid() {
		return nil, fmt.Errorf("%w: visibility %q", ErrInvalid, vis)
	}
	ns, err := s.namespace(ctx, p.Namespace)
	if err != nil {
		return nil, err
	}
	if err := s.canWrite(ctx, actor, ns); err != nil {
		return nil, err
	}
	row := &db.Collection{ID: db.NewID(), NamespaceID: ns.ID, Slug: slug, Title: title, Description: p.Description, Visibility: string(vis)}
	if _, err := s.o.DB.NewInsert().Model(row).Exec(ctx); err != nil {
		if !db.IsUniqueViolation(err) {
			return nil, err
		}
		if !p.ExistOK {
			return nil, fmt.Errorf("%w: %s/%s", ErrExists, ns.Name, slug)
		}
		return s.Get(ctx, actor, ns.Name, slug)
	}
	return s.load(ctx, s.cache(actor), row)
}

// Update implements Service.
func (s *DB) Update(ctx context.Context, actor model.Principal, ns, slug string, u Update) (*Collection, error) {
	row, c, err := s.writable(ctx, actor, ns, slug)
	if err != nil {
		return nil, err
	}
	if u.Title != nil {
		t := strings.TrimSpace(*u.Title)
		if t == "" {
			return nil, fmt.Errorf("%w: a title is required", ErrInvalid)
		}
		row.Title = t
	}
	if u.Description != nil {
		row.Description = *u.Description
	}
	if err := checkText(row.Title, row.Description); err != nil {
		return nil, err
	}
	if u.Visibility != nil {
		if !u.Visibility.Valid() {
			return nil, fmt.Errorf("%w: visibility %q", ErrInvalid, *u.Visibility)
		}
		row.Visibility = string(*u.Visibility)
	}
	if u.Theme != nil {
		if len(*u.Theme) > 32 {
			return nil, fmt.Errorf("%w: the theme is too long", ErrInvalid)
		}
		row.Theme = *u.Theme
	}
	if _, err := s.o.DB.NewUpdate().Model(row).Column("title", "description", "visibility", "theme", "updated_at").WherePK().Exec(ctx); err != nil {
		return nil, err
	}
	return s.load(ctx, c, row)
}

// Delete implements Service.
func (s *DB) Delete(ctx context.Context, actor model.Principal, ns, slug string) error {
	row, _, err := s.writable(ctx, actor, ns, slug)
	if err != nil {
		return err
	}
	_, err = s.o.DB.NewDelete().Model(row).WherePK().Exec(ctx)
	return err
}

// touch moves updated_at forward after an item change.
func (s *DB) touch(ctx context.Context, idb bun.IDB, row *db.Collection) error {
	_, err := idb.NewUpdate().Model(row).Column("updated_at").WherePK().Exec(ctx)
	return err
}

func (s *DB) checkItem(ctx context.Context, actor model.Principal, p ItemParams) (string, error) {
	switch p.Type {
	case ItemModel, ItemDataset:
		t := model.RepoTypeModel
		if p.Type == ItemDataset {
			t = model.RepoTypeDataset
		}
		id, err := model.ParseRepoID(t, p.ItemID)
		if err != nil {
			return "", fmt.Errorf("%w: %w", ErrInvalid, err)
		}
		r, err := s.o.Repos.Get(ctx, id)
		if err != nil || r.DeletedAt != nil ||
			s.o.Authz.Can(ctx, actor, auth.ActionRead, auth.Target{Repo: repoTarget(r)}) != nil {
			return "", fmt.Errorf("%w: %s %s does not exist", ErrInvalid, p.Type, p.ItemID)
		}
		return r.ID.String(), nil
	case ItemCollection:
		ns, slug, ok := strings.Cut(p.ItemID, "/")
		if !ok {
			return "", fmt.Errorf("%w: a Collection item must look like namespace/slug", ErrInvalid)
		}
		got, err := s.Get(ctx, actor, ns, slug)
		if err != nil {
			return "", fmt.Errorf("%w: collection %s does not exist", ErrInvalid, p.ItemID)
		}
		return got.Namespace + "/" + got.Slug, nil
	}
	return "", fmt.Errorf("%w: item type %q must be model, dataset or collection", ErrInvalid, p.Type)
}

// AddItem implements Service: the item goes to the end. Adding an item
// already present is ErrExists, unless ExistOK.
func (s *DB) AddItem(ctx context.Context, actor model.Principal, ns, slug string, p ItemParams) (*Collection, error) {
	if utf8.RuneCountInString(p.Note) > MaxNote {
		return nil, fmt.Errorf("%w: the note is longer than %d characters", ErrInvalid, MaxNote)
	}
	row, c, err := s.writable(ctx, actor, ns, slug)
	if err != nil {
		return nil, err
	}
	itemID, err := s.checkItem(ctx, actor, p)
	if err != nil {
		return nil, err
	}
	if p.Type == ItemCollection && strings.EqualFold(itemID, c.nsName(ctx, row.NamespaceID)+"/"+row.Slug) {
		return nil, fmt.Errorf("%w: a Collection cannot contain itself", ErrInvalid)
	}
	err = s.o.DB.InTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		var existing []db.CollectionItem
		if err := tx.NewSelect().Model(&existing).Where("collection_id = ?", row.ID).Order("position ASC").Scan(ctx); err != nil {
			return err
		}
		for _, it := range existing {
			if it.ItemType == p.Type && strings.EqualFold(it.ItemID, itemID) {
				if p.ExistOK {
					return nil
				}
				return fmt.Errorf("%w: %s %s is already in the Collection", ErrExists, p.Type, itemID)
			}
		}
		if len(existing) >= MaxItems {
			return fmt.Errorf("%w: a Collection holds at most %d items", ErrInvalid, MaxItems)
		}
		it := &db.CollectionItem{ID: db.NewID(), CollectionID: row.ID, ItemType: p.Type, ItemID: itemID, Position: len(existing), Note: p.Note}
		if _, err := tx.NewInsert().Model(it).Exec(ctx); err != nil {
			return err
		}
		return s.touch(ctx, tx, row)
	})
	if err != nil {
		return nil, err
	}
	return s.load(ctx, c, row)
}

// UpdateItem implements Service. A new position moves the item and
// renumbers the rest.
func (s *DB) UpdateItem(ctx context.Context, actor model.Principal, ns, slug string, itemID uuid.UUID, u ItemUpdate) (*Collection, error) {
	if u.Note != nil && utf8.RuneCountInString(*u.Note) > MaxNote {
		return nil, fmt.Errorf("%w: the note is longer than %d characters", ErrInvalid, MaxNote)
	}
	row, c, err := s.writable(ctx, actor, ns, slug)
	if err != nil {
		return nil, err
	}
	err = s.o.DB.InTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		var items []db.CollectionItem
		if err := tx.NewSelect().Model(&items).Where("collection_id = ?", row.ID).Order("position ASC", "id ASC").Scan(ctx); err != nil {
			return err
		}
		i := -1
		for k := range items {
			if items[k].ID == itemID {
				i = k
			}
		}
		if i < 0 {
			return fmt.Errorf("%w: item %s", ErrNotFound, itemID)
		}
		if u.Note != nil {
			items[i].Note = *u.Note
		}
		if u.Position != nil {
			moved := items[i]
			items = append(items[:i], items[i+1:]...)
			pos := max(0, min(*u.Position, len(items)))
			items = append(items[:pos], append([]db.CollectionItem{moved}, items[pos:]...)...)
		}
		for k := range items {
			items[k].Position = k
			if _, err := tx.NewUpdate().Model(&items[k]).Column("position", "note", "updated_at").WherePK().Exec(ctx); err != nil {
				return err
			}
		}
		return s.touch(ctx, tx, row)
	})
	if err != nil {
		return nil, err
	}
	return s.load(ctx, c, row)
}

// DeleteItem implements Service.
func (s *DB) DeleteItem(ctx context.Context, actor model.Principal, ns, slug string, itemID uuid.UUID) error {
	row, _, err := s.writable(ctx, actor, ns, slug)
	if err != nil {
		return err
	}
	return s.o.DB.InTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		res, err := tx.NewDelete().Model((*db.CollectionItem)(nil)).Where("id = ?", itemID).Where("collection_id = ?", row.ID).Exec(ctx)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("%w: item %s", ErrNotFound, itemID)
		}
		var items []db.CollectionItem
		if err := tx.NewSelect().Model(&items).Where("collection_id = ?", row.ID).Order("position ASC", "id ASC").Scan(ctx); err != nil {
			return err
		}
		for k := range items {
			if items[k].Position != k {
				items[k].Position = k
				if _, err := tx.NewUpdate().Model(&items[k]).Column("position", "updated_at").WherePK().Exec(ctx); err != nil {
					return err
				}
			}
		}
		return s.touch(ctx, tx, row)
	})
}
