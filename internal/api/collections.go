package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/jamesagarside/hangar/internal/api/gen"
	"github.com/jamesagarside/hangar/internal/collections"
	"github.com/jamesagarside/hangar/internal/httpx"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/repo"
)

// CollectionsAPI serves Collections (BROWSE-5).
type CollectionsAPI struct{ d MetaDeps }

// NewCollectionsAPI returns the Collection handlers. Collections is
// required.
func NewCollectionsAPI(d MetaDeps) (*CollectionsAPI, error) {
	if err := d.init(); err != nil {
		return nil, err
	}
	if d.Collections == nil {
		return nil, errors.New("api: Collections is required")
	}
	return &CollectionsAPI{d: d}, nil
}

// collRepo is the short Repo form shown with Collection items.
func (a *CollectionsAPI) collRepo(ctx context.Context, it collections.Item) *gen.Repo {
	t := model.RepoTypeModel
	if it.Type == collections.ItemDataset {
		t = model.RepoTypeDataset
	}
	id, err := model.ParseRepoID(t, it.ItemID)
	if err != nil {
		return nil
	}
	r, err := a.d.Repos.Get(ctx, id)
	if err != nil {
		return nil
	}
	return collRepoOf(r)
}

func collRepoOf(r *repo.Repo) *gen.Repo {
	out := &gen.Repo{Id: r.ID.String(), Type: gen.RepoTypeSingular(r.ID.Type), Namespace: r.ID.Namespace, Name: r.ID.Name,
		Visibility: gen.Visibility(r.Visibility), DefaultBranch: r.DefaultBranch, Mirror: r.Mirror, Gated: gen.RepoGated(r.Gating.Mode),
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt, SizeBytes: r.SizeBytes, LfsBytes: r.LFSBytes}
	u := r.UUID
	out.Uuid = &u
	if r.Description != "" {
		d := r.Description
		out.Description = &d
	}
	if !r.LastModified.IsZero() {
		lm := r.LastModified
		out.LastModified = &lm
	}
	return out
}

func (a *CollectionsAPI) collOut(ctx context.Context, c *collections.Collection) gen.Collection {
	out := gen.Collection{Namespace: c.Namespace, Slug: c.Slug, Title: c.Title, Visibility: gen.Visibility(c.Visibility),
		UpdatedAt: c.UpdatedAt, Items: make([]gen.CollectionItem, 0, len(c.Items))}
	if c.Description != "" {
		d := c.Description
		out.Description = &d
	}
	if c.Theme != "" {
		t := c.Theme
		out.Theme = &t
	}
	for _, it := range c.Items {
		item := gen.CollectionItem{Id: it.ID, ItemId: it.ItemID, Position: it.Position, Type: gen.CollectionItemType(it.Type)}
		if it.Note != "" {
			n := it.Note
			item.Note = &n
		}
		if it.Type == collections.ItemModel || it.Type == collections.ItemDataset {
			item.Repo = a.collRepo(ctx, it)
		}
		out.Items = append(out.Items, item)
	}
	return out
}

// ListCollections implements gen.ServerInterface: the Collections the
// caller can see, most recently updated first.
func (a *CollectionsAPI) ListCollections(w http.ResponseWriter, r *http.Request, params gen.ListCollectionsParams) {
	limit, ok := metaLimit(params.Limit, 50)
	if !ok {
		metaBadRequest(w, r, "limit", "limit must be between 1 and 500.")
		return
	}
	o := collections.ListOptions{Limit: limit}
	if params.Owner != nil {
		o.Owner = *params.Owner
	}
	if params.Item != nil {
		o.Item = *params.Item
	}
	if params.Q != nil {
		o.Query = *params.Q
	}
	list, err := a.d.Collections.List(r.Context(), httpx.Principal(r.Context()), o)
	if err != nil {
		a.d.metaProblem(w, r, err)
		return
	}
	out := gen.CollectionList{Items: make([]gen.Collection, 0, len(list))}
	for i := range list {
		out.Items = append(out.Items, a.collOut(r.Context(), &list[i]))
	}
	httpx.WriteJSON(w, r, http.StatusOK, out)
}

func collSignedIn(w http.ResponseWriter, r *http.Request) (model.Principal, bool) {
	p := httpx.Principal(r.Context())
	if p.IsAnonymous() {
		httpx.WriteProblem(w, r, &httpx.Problem{Status: http.StatusUnauthorized, Detail: "Sign in to change Collections."})
		return p, false
	}
	return p, true
}

// CreateCollection implements gen.ServerInterface.
func (a *CollectionsAPI) CreateCollection(w http.ResponseWriter, r *http.Request) {
	p, ok := collSignedIn(w, r)
	if !ok {
		return
	}
	var body gen.CollectionCreate
	if !metaDecode(w, r, &body) {
		return
	}
	cp := collections.CreateParams{Namespace: body.Namespace, Title: body.Title}
	if body.Description != nil {
		cp.Description = *body.Description
	}
	if body.Visibility != nil {
		cp.Visibility = model.Visibility(*body.Visibility)
	}
	c, err := a.d.Collections.Create(r.Context(), p, cp)
	if err != nil {
		a.d.metaProblem(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusCreated, a.collOut(r.Context(), c))
}

// GetCollection implements gen.ServerInterface.
func (a *CollectionsAPI) GetCollection(w http.ResponseWriter, r *http.Request, ns gen.Ns, slug gen.Slug) {
	c, err := a.d.Collections.Get(r.Context(), httpx.Principal(r.Context()), ns, slug)
	if err != nil {
		a.d.metaProblem(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, a.collOut(r.Context(), c))
}

// UpdateCollection implements gen.ServerInterface.
func (a *CollectionsAPI) UpdateCollection(w http.ResponseWriter, r *http.Request, ns gen.Ns, slug gen.Slug) {
	p, ok := collSignedIn(w, r)
	if !ok {
		return
	}
	var body gen.CollectionUpdate
	if !metaDecode(w, r, &body) {
		return
	}
	u := collections.Update{Title: body.Title, Description: body.Description, Theme: body.Theme}
	if body.Visibility != nil {
		v := model.Visibility(*body.Visibility)
		u.Visibility = &v
	}
	c, err := a.d.Collections.Update(r.Context(), p, ns, slug, u)
	if err != nil {
		a.d.metaProblem(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, a.collOut(r.Context(), c))
}

// DeleteCollection implements gen.ServerInterface.
func (a *CollectionsAPI) DeleteCollection(w http.ResponseWriter, r *http.Request, ns gen.Ns, slug gen.Slug) {
	p, ok := collSignedIn(w, r)
	if !ok {
		return
	}
	if err := a.d.Collections.Delete(r.Context(), p, ns, slug); err != nil {
		a.d.metaProblem(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// AddCollectionItem implements gen.ServerInterface.
func (a *CollectionsAPI) AddCollectionItem(w http.ResponseWriter, r *http.Request, ns gen.Ns, slug gen.Slug) {
	p, ok := collSignedIn(w, r)
	if !ok {
		return
	}
	var body gen.CollectionItemCreate
	if !metaDecode(w, r, &body) {
		return
	}
	ip := collections.ItemParams{Type: string(body.Type), ItemID: body.ItemId}
	if body.Note != nil {
		ip.Note = *body.Note
	}
	c, err := a.d.Collections.AddItem(r.Context(), p, ns, slug, ip)
	if err != nil {
		a.d.metaProblem(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, a.collOut(r.Context(), c))
}

// UpdateCollectionItem implements gen.ServerInterface.
func (a *CollectionsAPI) UpdateCollectionItem(w http.ResponseWriter, r *http.Request, ns gen.Ns, slug gen.Slug, id gen.ID) {
	p, ok := collSignedIn(w, r)
	if !ok {
		return
	}
	var body gen.CollectionItemUpdate
	if !metaDecode(w, r, &body) {
		return
	}
	if body.Position != nil && *body.Position < 0 {
		metaBadRequest(w, r, "position", "position must be 0 or more.")
		return
	}
	c, err := a.d.Collections.UpdateItem(r.Context(), p, ns, slug, id, collections.ItemUpdate{Note: body.Note, Position: body.Position})
	if err != nil {
		a.d.metaProblem(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, a.collOut(r.Context(), c))
}

// DeleteCollectionItem implements gen.ServerInterface.
func (a *CollectionsAPI) DeleteCollectionItem(w http.ResponseWriter, r *http.Request, ns gen.Ns, slug gen.Slug, id gen.ID) {
	p, ok := collSignedIn(w, r)
	if !ok {
		return
	}
	if err := a.d.Collections.DeleteItem(r.Context(), p, ns, slug, id); err != nil {
		a.d.metaProblem(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
